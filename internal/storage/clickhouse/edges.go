package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/LogDoc-org/logdoc/internal/graph"
)

// InsertEdgeMetrics appends one flush interval of edge observations to the
// edge_metrics time series (implements graph.MetricsBackend).
func (s *Store) InsertEdgeMetrics(ctx context.Context, ts time.Time, edges []graph.EdgeAgg) error {
	if len(edges) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx,
		fmt.Sprintf(`INSERT INTO %s.edge_metrics (ts, tenant_id, src, dst, count, errors)`, s.db))
	if err != nil {
		return err
	}
	for _, e := range edges {
		if err := batch.Append(ts, e.Key.TenantID, e.Key.Src, e.Key.Dst, e.Count, e.Errors); err != nil {
			return err
		}
	}
	return batch.Send()
}

// EdgeRates sums edge observations over the trailing window
// (implements graph.MetricsBackend).
func (s *Store) EdgeRates(ctx context.Context, tenantID string, window time.Duration) (map[graph.EdgeKey]graph.Rates, error) {
	now := time.Now()
	return s.EdgeRatesRange(ctx, tenantID, now.Add(-window), now)
}

// EdgeRatesRange sums edge observations over [from, to)
// (implements graph.MetricsBackend).
func (s *Store) EdgeRatesRange(ctx context.Context, tenantID string, from, to time.Time) (map[graph.EdgeKey]graph.Rates, error) {
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`
SELECT src, dst, sum(count), sum(errors)
FROM %s.edge_metrics
WHERE tenant_id = ? AND ts >= ? AND ts < ?
GROUP BY src, dst`, s.db), tenantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("edge rates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[graph.EdgeKey]graph.Rates)
	for rows.Next() {
		var src, dst string
		var r graph.Rates
		if err := rows.Scan(&src, &dst, &r.Count, &r.Errors); err != nil {
			return nil, err
		}
		out[graph.EdgeKey{TenantID: tenantID, Src: src, Dst: dst}] = r
	}
	return out, rows.Err()
}
