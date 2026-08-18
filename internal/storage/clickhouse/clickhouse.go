// Package clickhouse — the storage.Store implementation on top of ClickHouse.
package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/LogDoc-org/logdoc/internal/model"
)

type Options struct {
	Addr     string
	Database string
	Username string
	Password string
	TTLDays  int // log retention period; 0 → 30
}

type Store struct {
	conn driver.Conn
	db   string
}

// Open connects to ClickHouse (waiting up to 30 seconds for readiness —
// convenient when starting together with compose) and runs the schema migration.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.TTLDays <= 0 {
		opts.TTLDays = 30
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{opts.Addr},
		Auth: clickhouse.Auth{
			Database: "default", // the database is created by the migration
			Username: opts.Username,
			Password: opts.Password,
		},
		DialTimeout: 5 * time.Second,
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse open: %w", err)
	}

	if err := pingWithRetry(ctx, conn, 30*time.Second); err != nil {
		return nil, fmt.Errorf("clickhouse ping %s: %w", opts.Addr, err)
	}

	s := &Store{conn: conn, db: opts.Database}
	if err := s.migrate(ctx, opts.TTLDays); err != nil {
		return nil, fmt.Errorf("clickhouse migrate: %w", err)
	}
	return s, nil
}

func pingWithRetry(ctx context.Context, conn driver.Conn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = conn.Ping(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return err
}

func (s *Store) migrate(ctx context.Context, ttlDays int) error {
	stmts := []string{
		fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s`, s.db),
		fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.logs (
    tenant_id   LowCardinality(String),
    app         LowCardinality(String),
    src         LowCardinality(String),
    lvl         UInt8,
    pid         String,
    ts          DateTime64(3),
    msg         String,
    fields      Map(String, String),
    ingested_at DateTime64(3) DEFAULT now64(3)
) ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (tenant_id, app, ts)
TTL toDateTime(ts) + INTERVAL %d DAY
SETTINGS index_granularity = 8192`, s.db, ttlDays),
		fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.query_telemetry (
    ts          DateTime64(3),
    tenant_id   LowCardinality(String),
    plan        String,
    duration_ms UInt32,
    rows        UInt32
) ENGINE = MergeTree
ORDER BY ts
TTL toDateTime(ts) + INTERVAL 90 DAY`, s.db),
		fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.edge_metrics (
    ts        DateTime64(3),
    tenant_id LowCardinality(String),
    src       LowCardinality(String),
    dst       LowCardinality(String),
    count     UInt64,
    errors    UInt64
) ENGINE = MergeTree
PARTITION BY toDate(ts)
ORDER BY (tenant_id, src, dst, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY`, s.db),
	}
	for _, q := range stmts {
		if err := s.conn.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) InsertBatch(ctx context.Context, entries []model.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx,
		fmt.Sprintf(`INSERT INTO %s.logs (tenant_id, app, src, lvl, pid, ts, msg, fields)`, s.db))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := batch.Append(e.TenantID, e.App, e.Src, uint8(e.Lvl), e.PID, e.Ts, e.Msg, e.Fields); err != nil {
			return err
		}
	}
	return batch.Send()
}

func (s *Store) Close() error {
	return s.conn.Close()
}
