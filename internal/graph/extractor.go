package graph

import (
	"sync"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Field names that group entries of different services into one interaction.
var (
	// traceFields carry a distributed trace id → OriginTrace.
	traceFields = []string{"trace_id", "traceid", "trace-id"}
	// corrFields carry an application-level correlation id → OriginInferred.
	corrFields = []string{"correlation_id", "corr_id", "request_id", "x_request_id", "x-request-id"}
	// peerFields directly name the callee service → OriginInferred.
	peerFields = []string{"peer.service", "peer_service", "target", "upstream", "callee"}
)

// ExtractorOptions tune the in-memory aggregation.
type ExtractorOptions struct {
	FlushInterval time.Duration // aggregate flush period (default 10s)
	GroupTTL      time.Duration // how long a trace/corr group lives (default 5m)
	MaxGroups     int           // group cap to bound memory (default 100k)
}

// Extractor consumes the entry stream (non-blocking, fits the ingest fanout)
// and periodically flushes node/edge aggregates into a Sink.
//
// Direction heuristic: within one trace/correlation group services are linked
// in the order their entries arrive — for a chain A→B→C sharing one trace id
// this yields the edges A→B and B→C. It is an approximation (documented as
// such); explicit peer fields give exact direction.
type Extractor struct {
	sink Sink
	opts ExtractorOptions

	mu      sync.Mutex
	nodes   map[nodeKey]*nodeState
	edges   map[EdgeKey]*edgeState
	groups  map[groupKey]*groupState
	dropped uint64 // groups not created because of MaxGroups

	stop chan struct{}
	done chan struct{}
}

type nodeKey struct {
	tenantID string
	app      string
}

type nodeState struct {
	count    uint64
	errors   uint64
	lastSeen time.Time
}

type edgeState struct {
	origin   Origin
	count    uint64
	errors   uint64
	lastSeen time.Time
}

type groupKind uint8

const (
	groupTrace groupKind = iota
	groupCorr
)

type groupKey struct {
	tenantID string
	kind     groupKind
	id       string
}

type groupState struct {
	lastApp  string
	lastSeen time.Time
}

// NewExtractor creates the extractor and starts its flush loop.
func NewExtractor(sink Sink, opts ExtractorOptions) *Extractor {
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 10 * time.Second
	}
	if opts.GroupTTL <= 0 {
		opts.GroupTTL = 5 * time.Minute
	}
	if opts.MaxGroups <= 0 {
		opts.MaxGroups = 100_000
	}
	x := &Extractor{
		sink:   sink,
		opts:   opts,
		nodes:  make(map[nodeKey]*nodeState),
		edges:  make(map[EdgeKey]*edgeState),
		groups: make(map[groupKey]*groupState),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go x.loop()
	return x
}

// Append consumes one entry. It only takes a mutex and touches maps —
// fast enough for the ingest path and never blocks on I/O.
func (x *Extractor) Append(e model.Entry) {
	if e.App == "" {
		return
	}
	isErr := e.Lvl >= model.LevelError
	now := e.Ts
	if now.IsZero() {
		now = time.Now()
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	// Node: every service that logs is on the map, even without edges.
	nk := nodeKey{e.TenantID, e.App}
	n := x.nodes[nk]
	if n == nil {
		n = &nodeState{}
		x.nodes[nk] = n
	}
	n.count++
	if isErr {
		n.errors++
	}
	if now.After(n.lastSeen) {
		n.lastSeen = now
	}

	// Signal 1: shared trace id.
	if id := firstField(e.Fields, traceFields); id != "" {
		x.linkGroup(groupKey{e.TenantID, groupTrace, id}, e.App, OriginTrace, isErr, now)
	}
	// Signal 2a: shared correlation id.
	if id := firstField(e.Fields, corrFields); id != "" {
		x.linkGroup(groupKey{e.TenantID, groupCorr, id}, e.App, OriginInferred, isErr, now)
	}
	// Signal 2b: an explicit peer field naming the callee.
	if peer := firstField(e.Fields, peerFields); peer != "" && peer != e.App {
		x.bumpEdge(EdgeKey{e.TenantID, e.App, peer}, OriginInferred, isErr, now)
	}
}

func (x *Extractor) linkGroup(gk groupKey, app string, origin Origin, isErr bool, now time.Time) {
	g := x.groups[gk]
	if g == nil {
		if len(x.groups) >= x.opts.MaxGroups {
			x.dropped++
			return
		}
		g = &groupState{}
		x.groups[gk] = g
	}
	if g.lastApp != "" && g.lastApp != app {
		x.bumpEdge(EdgeKey{gk.tenantID, g.lastApp, app}, origin, isErr, now)
	}
	g.lastApp = app
	g.lastSeen = now
}

func (x *Extractor) bumpEdge(k EdgeKey, origin Origin, isErr bool, now time.Time) {
	s := x.edges[k]
	if s == nil {
		s = &edgeState{}
		x.edges[k] = s
	}
	s.origin |= origin
	s.count++
	if isErr {
		s.errors++
	}
	if now.After(s.lastSeen) {
		s.lastSeen = now
	}
}

// Dropped reports how many groups were rejected by the MaxGroups cap.
func (x *Extractor) Dropped() uint64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.dropped
}

// Close flushes the remaining aggregates and stops the loop.
func (x *Extractor) Close() {
	close(x.stop)
	<-x.done
}

func (x *Extractor) loop() {
	defer close(x.done)
	ticker := time.NewTicker(x.opts.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			x.flush()
			x.evictGroups()
		case <-x.stop:
			x.flush()
			return
		}
	}
}

// flush hands the accumulated aggregates to the sink and resets counters.
func (x *Extractor) flush() {
	x.mu.Lock()
	var nodes []NodeAgg
	for k, s := range x.nodes {
		nodes = append(nodes, NodeAgg{
			TenantID: k.tenantID, App: k.app,
			Count: s.count, Errors: s.errors, LastSeen: s.lastSeen,
		})
	}
	var edges []EdgeAgg
	for k, s := range x.edges {
		edges = append(edges, EdgeAgg{
			Key: k, Origin: s.origin,
			Count: s.count, Errors: s.errors, LastSeen: s.lastSeen,
		})
	}
	x.nodes = make(map[nodeKey]*nodeState)
	x.edges = make(map[EdgeKey]*edgeState)
	x.mu.Unlock()

	if len(nodes) == 0 && len(edges) == 0 {
		return
	}
	x.sink.ApplyGraph(nodes, edges)
}

func (x *Extractor) evictGroups() {
	cutoff := time.Now().Add(-x.opts.GroupTTL)
	x.mu.Lock()
	for k, g := range x.groups {
		if g.lastSeen.Before(cutoff) {
			delete(x.groups, k)
		}
	}
	x.mu.Unlock()
}

func firstField(fields map[string]string, names []string) string {
	if fields == nil {
		return ""
	}
	for _, n := range names {
		if v := fields[n]; v != "" {
			return v
		}
	}
	return ""
}
