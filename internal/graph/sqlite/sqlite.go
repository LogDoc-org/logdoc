// Package sqlite — persistent Architecture Graph state on embedded SQLite
// (modernc.org/sqlite: pure Go, keeps CGO_ENABLED=0).
// Node/edge current state lives here; edge metric time series live in
// ClickHouse (graph.MetricsBackend).
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // database/sql driver

	"github.com/LogDoc-org/logdoc/internal/graph"
)

type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the graph database at path.
// Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	// busy_timeout: the extractor flush and HTTP reads may race; wait, not fail.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("sqlite open %s: %w", path, err)
	}
	// modernc.org/sqlite is safe for one writer; serialize access.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS nodes (
	tenant_id    TEXT    NOT NULL,
	app          TEXT    NOT NULL,
	first_seen   INTEGER NOT NULL, -- unix milliseconds
	last_seen    INTEGER NOT NULL,
	total_count  INTEGER NOT NULL DEFAULT 0,
	total_errors INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (tenant_id, app)
);
CREATE TABLE IF NOT EXISTS edges (
	tenant_id    TEXT    NOT NULL,
	src          TEXT    NOT NULL,
	dst          TEXT    NOT NULL,
	origin       INTEGER NOT NULL DEFAULT 0, -- graph.Origin bitmask
	first_seen   INTEGER NOT NULL,
	last_seen    INTEGER NOT NULL,
	total_count  INTEGER NOT NULL DEFAULT 0,
	total_errors INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (tenant_id, src, dst)
);
CREATE TABLE IF NOT EXISTS deploys (
	tenant_id TEXT    NOT NULL,
	app       TEXT    NOT NULL,
	version   TEXT    NOT NULL,
	ts        INTEGER NOT NULL -- unix milliseconds
);
CREATE INDEX IF NOT EXISTS deploys_app_ts ON deploys (tenant_id, app, ts DESC);`)
	if err != nil {
		return fmt.Errorf("sqlite migrate: %w", err)
	}
	return nil
}

func (s *Store) UpsertGraph(ctx context.Context, nodes []graph.NodeAgg, edges []graph.EdgeAgg) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("graph upsert begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nodeStmt, err := tx.PrepareContext(ctx, `
INSERT INTO nodes (tenant_id, app, first_seen, last_seen, total_count, total_errors)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, app) DO UPDATE SET
	last_seen    = MAX(last_seen, excluded.last_seen),
	total_count  = total_count + excluded.total_count,
	total_errors = total_errors + excluded.total_errors`)
	if err != nil {
		return err
	}
	defer func() { _ = nodeStmt.Close() }()
	for _, n := range nodes {
		ms := n.LastSeen.UnixMilli()
		if _, err := nodeStmt.ExecContext(ctx, n.TenantID, n.App, ms, ms, n.Count, n.Errors); err != nil {
			return fmt.Errorf("node upsert %s: %w", n.App, err)
		}
	}

	edgeStmt, err := tx.PrepareContext(ctx, `
INSERT INTO edges (tenant_id, src, dst, origin, first_seen, last_seen, total_count, total_errors)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, src, dst) DO UPDATE SET
	origin       = origin | excluded.origin,
	last_seen    = MAX(last_seen, excluded.last_seen),
	total_count  = total_count + excluded.total_count,
	total_errors = total_errors + excluded.total_errors`)
	if err != nil {
		return err
	}
	defer func() { _ = edgeStmt.Close() }()
	for _, e := range edges {
		ms := e.LastSeen.UnixMilli()
		if _, err := edgeStmt.ExecContext(ctx, e.Key.TenantID, e.Key.Src, e.Key.Dst,
			uint8(e.Origin), ms, ms, e.Count, e.Errors); err != nil {
			return fmt.Errorf("edge upsert %s→%s: %w", e.Key.Src, e.Key.Dst, err)
		}
	}
	return tx.Commit()
}

func (s *Store) Topology(ctx context.Context, tenantID string) (graph.Topology, error) {
	var topo graph.Topology

	rows, err := s.db.QueryContext(ctx,
		`SELECT app, first_seen, last_seen, total_count, total_errors
		 FROM nodes WHERE tenant_id = ? ORDER BY app`, tenantID)
	if err != nil {
		return topo, fmt.Errorf("topology nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n graph.Node
		var first, last int64
		if err := rows.Scan(&n.App, &first, &last, &n.Count, &n.Errors); err != nil {
			return topo, err
		}
		n.FirstSeen = time.UnixMilli(first)
		n.LastSeen = time.UnixMilli(last)
		topo.Nodes = append(topo.Nodes, n)
	}
	if err := rows.Err(); err != nil {
		return topo, err
	}

	erows, err := s.db.QueryContext(ctx,
		`SELECT src, dst, origin, first_seen, last_seen, total_count, total_errors
		 FROM edges WHERE tenant_id = ? ORDER BY src, dst`, tenantID)
	if err != nil {
		return topo, fmt.Errorf("topology edges: %w", err)
	}
	defer func() { _ = erows.Close() }()
	for erows.Next() {
		var e graph.Edge
		var origin uint8
		var first, last int64
		if err := erows.Scan(&e.Src, &e.Dst, &origin, &first, &last, &e.Count, &e.Errors); err != nil {
			return topo, err
		}
		e.Origin = graph.Origin(origin).String()
		e.FirstSeen = time.UnixMilli(first)
		e.LastSeen = time.UnixMilli(last)
		topo.Edges = append(topo.Edges, e)
	}
	return topo, erows.Err()
}

// InsertDeploys appends deploy markers. A marker whose version equals the
// latest stored version of the same app is skipped — this makes detection
// restart-safe (the in-memory last-version map starts empty on boot).
func (s *Store) InsertDeploys(ctx context.Context, tenantID string, deploys []graph.Deploy) error {
	if len(deploys) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("deploys insert begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, d := range deploys {
		var latest sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT version FROM deploys WHERE tenant_id = ? AND app = ?
			 ORDER BY ts DESC LIMIT 1`, tenantID, d.App).Scan(&latest)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("deploys latest %s: %w", d.App, err)
		}
		if latest.Valid && latest.String == d.Version {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO deploys (tenant_id, app, version, ts) VALUES (?, ?, ?, ?)`,
			tenantID, d.App, d.Version, d.Ts.UnixMilli()); err != nil {
			return fmt.Errorf("deploys insert %s: %w", d.App, err)
		}
	}
	return tx.Commit()
}

// Deploys returns markers newest-first; empty app = all apps of the tenant.
func (s *Store) Deploys(ctx context.Context, tenantID, app string, since time.Time, limit int) ([]graph.Deploy, error) {
	q := `SELECT app, version, ts FROM deploys WHERE tenant_id = ? AND ts >= ?`
	args := []any{tenantID, since.UnixMilli()}
	if app != "" {
		q += ` AND app = ?`
		args = append(args, app)
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("deploys query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []graph.Deploy
	for rows.Next() {
		var d graph.Deploy
		var ms int64
		if err := rows.Scan(&d.App, &d.Version, &ms); err != nil {
			return nil, err
		}
		d.Ts = time.UnixMilli(ms)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) Close() error {
	return s.db.Close()
}
