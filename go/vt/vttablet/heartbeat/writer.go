/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package heartbeat

import (
	"fmt"
	"sync"
	"time"

	"vitess.io/vitess/go/vt/vterrors"

	"golang.org/x/net/context"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/timer"
	"vitess.io/vitess/go/vt/dbconfigs"
	"vitess.io/vitess/go/vt/dbconnpool"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/connpool"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"

	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

const (
	sqlCreateSidecarDB      = "create database if not exists %s"
	sqlCreateHeartbeatTable = `CREATE TABLE IF NOT EXISTS %s.heartbeat (
  keyspaceShard VARBINARY(256) NOT NULL PRIMARY KEY,
  tabletUid INT UNSIGNED NOT NULL,
  ts BIGINT UNSIGNED NOT NULL
        ) engine=InnoDB`
	sqlInsertInitialRow = "INSERT INTO %s.heartbeat (ts, tabletUid, keyspaceShard) VALUES (%a, %a, %a) ON DUPLICATE KEY UPDATE ts=VALUES(ts)"
	sqlUpdateHeartbeat  = "UPDATE %s.heartbeat SET ts=%a, tabletUid=%a WHERE keyspaceShard=%a"
)

// Writer runs on master tablets and writes heartbeats to the _vt.heartbeat
// table at a regular interval, defined by heartbeat_interval.
type Writer struct {
	env tabletenv.Env

	enabled       bool
	interval      time.Duration
	tabletAlias   topodatapb.TabletAlias
	keyspaceShard string
	now           func() time.Time
	errorLog      *logutil.ThrottledLogger

	mu     sync.Mutex
	isOpen bool
	pool   *connpool.Pool
	ticks  *timer.Timer
}

// NewWriter creates a new Writer.
func NewWriter(env tabletenv.Env, alias topodatapb.TabletAlias) *Writer {
	config := env.Config()
	if config.HeartbeatIntervalSeconds == 0 {
		return &Writer{}
	}
	heartbeatInterval := time.Duration(config.HeartbeatIntervalSeconds * 1e9)
	return &Writer{
		env:         env,
		enabled:     true,
		tabletAlias: alias,
		now:         time.Now,
		interval:    heartbeatInterval,
		ticks:       timer.NewTimer(heartbeatInterval),
		errorLog:    logutil.NewThrottledLogger("HeartbeatWriter", 60*time.Second),
		pool: connpool.NewPool(env, "HeartbeatWritePool", tabletenv.ConnPoolConfig{
			Size:               1,
			IdleTimeoutSeconds: env.Config().OltpReadPool.IdleTimeoutSeconds,
		}),
	}
}

// Init runs at tablet startup and last minute initialization of db settings, and
// creates the necessary tables for heartbeat.
func (w *Writer) Init(target querypb.Target) error {
	if !w.enabled {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	log.Info("Initializing heartbeat table.")
	w.keyspaceShard = fmt.Sprintf("%s:%s", target.Keyspace, target.Shard)

	if target.TabletType == topodatapb.TabletType_MASTER {
		err := w.initializeTables(w.env.Config().DB.AppWithDB())
		if err != nil {
			w.recordError(err)
			return err
		}
	}
	return nil
}

// Open sets up the Writer's db connection and launches the ticker
// responsible for periodically writing to the heartbeat table.
// Open may be called multiple times, as long as it was closed since
// last invocation.
func (w *Writer) Open() {
	if !w.enabled {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.isOpen {
		return
	}
	log.Info("Beginning heartbeat writes")
	w.pool.Open(w.env.Config().DB.AppWithDB(), w.env.Config().DB.DbaWithDB(), w.env.Config().DB.AppDebugWithDB())
	w.ticks.Start(func() { w.writeHeartbeat() })
	w.isOpen = true
}

// Close closes the Writer's db connection and stops the periodic ticker. A writer
// object can be re-opened after closing.
func (w *Writer) Close() {
	if !w.enabled {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.isOpen {
		return
	}
	w.ticks.Stop()
	w.pool.Close()
	log.Info("Stopped heartbeat writes.")
	w.isOpen = false
}

// initializeTables attempts to create the heartbeat tables and record an
// initial row. The row is created only on master and is replicated to all
// other servers.
func (w *Writer) initializeTables(cp dbconfigs.Connector) error {
	conn, err := dbconnpool.NewDBConnection(context.TODO(), cp)
	if err != nil {
		return vterrors.Wrap(err, "Failed to create connection for heartbeat")
	}
	defer conn.Close()
	statements := []string{
		fmt.Sprintf(sqlCreateSidecarDB, "_vt"),
		fmt.Sprintf(sqlCreateHeartbeatTable, "_vt"),
	}
	for _, s := range statements {
		if _, err := conn.ExecuteFetch(s, 0, false); err != nil {
			return vterrors.Wrap(err, "Failed to execute heartbeat init query")
		}
	}
	insert, err := w.bindHeartbeatVars(sqlInsertInitialRow)
	if err != nil {
		return vterrors.Wrap(err, "Failed to bindHeartbeatVars initial heartbeat insert")
	}
	_, err = conn.ExecuteFetch(insert, 0, false)
	if err != nil {
		return vterrors.Wrap(err, "Failed to execute initial heartbeat insert")
	}
	writes.Add(1)
	return nil
}

// bindHeartbeatVars takes a heartbeat write (insert or update) and
// adds the necessary fields to the query as bind vars. This is done
// to protect ourselves against a badly formed keyspace or shard name.
func (w *Writer) bindHeartbeatVars(query string) (string, error) {
	bindVars := map[string]*querypb.BindVariable{
		"ks":  sqltypes.StringBindVariable(w.keyspaceShard),
		"ts":  sqltypes.Int64BindVariable(w.now().UnixNano()),
		"uid": sqltypes.Int64BindVariable(int64(w.tabletAlias.Uid)),
	}
	parsed := sqlparser.BuildParsedQuery(query, "_vt", ":ts", ":uid", ":ks")
	bound, err := parsed.GenerateQuery(bindVars, nil)
	if err != nil {
		return "", err
	}
	return bound, nil
}

// writeHeartbeat updates the heartbeat row for this tablet with the current time in nanoseconds.
func (w *Writer) writeHeartbeat() {
	defer w.env.LogError()
	ctx, cancel := context.WithDeadline(context.Background(), w.now().Add(w.interval))
	defer cancel()
	update, err := w.bindHeartbeatVars(sqlUpdateHeartbeat)
	if err != nil {
		w.recordError(err)
		return
	}
	err = w.exec(ctx, update)
	if err != nil {
		w.recordError(err)
		return
	}
	writes.Add(1)
}

func (w *Writer) exec(ctx context.Context, query string) error {
	conn, err := w.pool.Get(ctx)
	if err != nil {
		return err
	}
	defer conn.Recycle()
	_, err = conn.Exec(ctx, query, 0, false)
	if err != nil {
		return err
	}
	return nil
}

func (w *Writer) recordError(err error) {
	w.errorLog.Errorf("%v", err)
	writeErrors.Add(1)
}
