// Package graph — the Architecture Graph: service nodes and edges extracted
// from runtime log signals (trace ids, correlation ids, peer fields).
// This is the S2 differentiator: the map appears from logs alone,
// traces only refine it (06-doc-layer.md).
package graph

import "time"

// Origin marks how an edge was discovered. A bitmask: the same edge can be
// confirmed by traces and inferred heuristics at once.
type Origin uint8

const (
	// OriginTrace — the edge was observed via shared trace_id (best signal).
	OriginTrace Origin = 1 << iota
	// OriginInferred — the edge was inferred from log fields
	// (correlation ids, peer/target fields).
	OriginInferred
)

func (o Origin) String() string {
	switch {
	case o&OriginTrace != 0 && o&OriginInferred != 0:
		return "both"
	case o&OriginTrace != 0:
		return "trace"
	case o&OriginInferred != 0:
		return "inferred"
	default:
		return "unknown"
	}
}

// EdgeKey identifies a directed edge between two services of one tenant.
type EdgeKey struct {
	TenantID string
	Src      string // caller app
	Dst      string // callee app
}

// EdgeAgg — aggregated edge observations over one flush interval.
type EdgeAgg struct {
	Key      EdgeKey
	Origin   Origin
	Count    uint64 // observed interactions in the interval
	Errors   uint64 // interactions where the entry level was ERROR+
	LastSeen time.Time
}

// NodeAgg — aggregated per-service observations over one flush interval.
type NodeAgg struct {
	TenantID string
	App      string
	Count    uint64 // log entries in the interval
	Errors   uint64 // ERROR+ entries in the interval
	LastSeen time.Time
}

// Sink receives periodic graph aggregates (implemented by graph storage).
type Sink interface {
	ApplyGraph(nodes []NodeAgg, edges []EdgeAgg)
}
