package graph

import (
	"context"
	"log/slog"
	"time"
)

// Manager glues the extractor to persistence: flushed aggregates go into the
// Store (current state, SQLite) and the MetricsBackend (time series, ClickHouse);
// topology reads merge both.
type Manager struct {
	store   Store
	metrics MetricsBackend
}

func NewManager(store Store, metrics MetricsBackend) *Manager {
	return &Manager{store: store, metrics: metrics}
}

// ApplyGraph implements Sink for the extractor. It is called from the
// extractor's flush goroutine, so blocking I/O is fine here.
func (m *Manager) ApplyGraph(nodes []NodeAgg, edges []EdgeAgg) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.store.UpsertGraph(ctx, nodes, edges); err != nil {
		slog.Warn("graph: state upsert failed", "err", err)
	}
	if err := m.metrics.InsertEdgeMetrics(ctx, time.Now(), edges); err != nil {
		slog.Warn("graph: edge metrics insert failed", "err", err)
	}
}

// Topology returns the current graph with windowed rates merged into edges.
// A metrics failure degrades to rates=0 instead of failing the map.
func (m *Manager) Topology(ctx context.Context, tenantID string, window time.Duration) (Topology, error) {
	topo, err := m.store.Topology(ctx, tenantID)
	if err != nil {
		return Topology{}, err
	}
	rates, err := m.metrics.EdgeRates(ctx, tenantID, window)
	if err != nil {
		slog.Warn("graph: edge rates unavailable", "err", err)
		return topo, nil
	}
	secs := window.Seconds()
	for i := range topo.Edges {
		e := &topo.Edges[i]
		r, ok := rates[EdgeKey{TenantID: tenantID, Src: e.Src, Dst: e.Dst}]
		if !ok || secs <= 0 {
			continue
		}
		e.RPS = float64(r.Count) / secs
		if r.Count > 0 {
			e.ErrorRate = float64(r.Errors) / float64(r.Count)
		}
	}
	return topo, nil
}
