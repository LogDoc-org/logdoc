package graph

import (
	"context"
	"time"
)

// Node — the current state of a service on the map.
type Node struct {
	App       string    `json:"app"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Count     uint64    `json:"count"`
	Errors    uint64    `json:"errors"`
}

// Edge — the current state of a directed service link.
type Edge struct {
	Src       string    `json:"src"`
	Dst       string    `json:"dst"`
	Origin    string    `json:"origin"` // trace | inferred | both
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Count     uint64    `json:"count"`
	Errors    uint64    `json:"errors"`
	// Windowed rates from edge metrics (zero when the metrics
	// backend is unavailable).
	RPS       float64 `json:"rps"`
	ErrorRate float64 `json:"error_rate"`
}

// Topology — the full graph of one tenant.
type Topology struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Store persists the current graph state (SQLite in single-node mode).
type Store interface {
	// UpsertGraph merges flushed aggregates into the persistent state:
	// counters add up, origin bits OR together, last_seen advances.
	UpsertGraph(ctx context.Context, nodes []NodeAgg, edges []EdgeAgg) error
	Topology(ctx context.Context, tenantID string) (Topology, error)
	DeployStore
	Close() error
}

// MetricsBackend stores the edge metrics time series (ClickHouse).
type MetricsBackend interface {
	InsertEdgeMetrics(ctx context.Context, ts time.Time, edges []EdgeAgg) error
	// EdgeRates returns per-edge count/error sums over the trailing window.
	EdgeRates(ctx context.Context, tenantID string, window time.Duration) (map[EdgeKey]Rates, error)
	// EdgeRatesRange — the same sums over an explicit [from, to) range
	// (used by the topology diff to compare adjacent windows).
	EdgeRatesRange(ctx context.Context, tenantID string, from, to time.Time) (map[EdgeKey]Rates, error)
}

// Rates — windowed sums for one edge.
type Rates struct {
	Count  uint64
	Errors uint64
}
