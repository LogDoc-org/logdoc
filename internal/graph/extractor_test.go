package graph

import (
	"sync"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

type captureSink struct {
	mu    sync.Mutex
	nodes []NodeAgg
	edges []EdgeAgg
}

func (c *captureSink) ApplyGraph(nodes []NodeAgg, edges []EdgeAgg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes = append(c.nodes, nodes...)
	c.edges = append(c.edges, edges...)
}

func (c *captureSink) edge(src, dst string) (EdgeAgg, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.edges {
		if e.Key.Src == src && e.Key.Dst == dst {
			return e, true
		}
	}
	return EdgeAgg{}, false
}

func entry(app string, lvl model.Level, ts time.Time, fields map[string]string) model.Entry {
	return model.Entry{TenantID: model.DefaultTenant, App: app, Lvl: lvl, Ts: ts, Msg: "m", Fields: fields}
}

func newTestExtractor(sink Sink) *Extractor {
	// A long flush interval: tests trigger flushes explicitly via Close.
	return NewExtractor(sink, ExtractorOptions{FlushInterval: time.Hour})
}

func TestTraceChainProducesDirectedEdges(t *testing.T) {
	sink := &captureSink{}
	x := newTestExtractor(sink)
	ts := time.Now()

	// Chain gateway → payment → db-proxy sharing one trace id.
	x.Append(entry("gateway", model.LevelInfo, ts, map[string]string{"trace_id": "t1"}))
	x.Append(entry("payment", model.LevelInfo, ts.Add(time.Millisecond), map[string]string{"trace_id": "t1"}))
	x.Append(entry("db-proxy", model.LevelError, ts.Add(2*time.Millisecond), map[string]string{"trace_id": "t1"}))
	x.Close()

	e1, ok := sink.edge("gateway", "payment")
	if !ok {
		t.Fatalf("no gateway→payment edge: %+v", sink.edges)
	}
	if e1.Origin != OriginTrace || e1.Count != 1 || e1.Errors != 0 {
		t.Fatalf("gateway→payment: %+v", e1)
	}
	e2, ok := sink.edge("payment", "db-proxy")
	if !ok {
		t.Fatal("no payment→db-proxy edge")
	}
	if e2.Errors != 1 {
		t.Fatalf("error entry must count on the edge: %+v", e2)
	}
	if _, ok := sink.edge("gateway", "db-proxy"); ok {
		t.Fatal("chain must not produce a transitive gateway→db-proxy edge")
	}
}

func TestCorrelationIDInferredEdge(t *testing.T) {
	sink := &captureSink{}
	x := newTestExtractor(sink)
	ts := time.Now()

	x.Append(entry("frontend", model.LevelInfo, ts, map[string]string{"request_id": "r-42"}))
	x.Append(entry("backend", model.LevelInfo, ts.Add(time.Millisecond), map[string]string{"request_id": "r-42"}))
	x.Close()

	e, ok := sink.edge("frontend", "backend")
	if !ok {
		t.Fatalf("no inferred edge: %+v", sink.edges)
	}
	if e.Origin != OriginInferred {
		t.Fatalf("origin = %v, want inferred", e.Origin)
	}
}

func TestPeerFieldEdge(t *testing.T) {
	sink := &captureSink{}
	x := newTestExtractor(sink)

	x.Append(entry("api", model.LevelInfo, time.Now(), map[string]string{"target": "billing"}))
	x.Close()

	e, ok := sink.edge("api", "billing")
	if !ok {
		t.Fatalf("no peer edge: %+v", sink.edges)
	}
	if e.Origin != OriginInferred || e.Count != 1 {
		t.Fatalf("peer edge: %+v", e)
	}
}

func TestTracePlusInferredBecomesBoth(t *testing.T) {
	sink := &captureSink{}
	x := newTestExtractor(sink)
	ts := time.Now()

	x.Append(entry("a", model.LevelInfo, ts, map[string]string{"trace_id": "t"}))
	x.Append(entry("b", model.LevelInfo, ts.Add(time.Millisecond), map[string]string{"trace_id": "t"}))
	x.Append(entry("a", model.LevelInfo, ts.Add(2*time.Millisecond), map[string]string{"target": "b"}))
	x.Close()

	e, ok := sink.edge("a", "b")
	if !ok {
		t.Fatal("no edge")
	}
	if e.Origin.String() != "both" {
		t.Fatalf("origin = %v, want both", e.Origin)
	}
	if e.Count != 2 {
		t.Fatalf("count = %d, want 2", e.Count)
	}
}

func TestNodesAppearWithoutEdges(t *testing.T) {
	sink := &captureSink{}
	x := newTestExtractor(sink)

	x.Append(entry("loner", model.LevelError, time.Now(), nil))
	x.Close()

	if len(sink.edges) != 0 {
		t.Fatalf("unexpected edges: %+v", sink.edges)
	}
	if len(sink.nodes) != 1 || sink.nodes[0].App != "loner" || sink.nodes[0].Errors != 1 {
		t.Fatalf("nodes: %+v", sink.nodes)
	}
}

func TestSameAppRepeatsDoNotSelfLink(t *testing.T) {
	sink := &captureSink{}
	x := newTestExtractor(sink)
	ts := time.Now()

	for i := 0; i < 3; i++ {
		x.Append(entry("solo", model.LevelInfo, ts.Add(time.Duration(i)*time.Millisecond),
			map[string]string{"trace_id": "t", "target": "solo"}))
	}
	x.Close()

	if len(sink.edges) != 0 {
		t.Fatalf("self-edges must not exist: %+v", sink.edges)
	}
}

func TestMaxGroupsCap(t *testing.T) {
	sink := &captureSink{}
	x := NewExtractor(sink, ExtractorOptions{FlushInterval: time.Hour, MaxGroups: 2})
	ts := time.Now()

	for _, id := range []string{"t1", "t2", "t3"} {
		x.Append(entry("a", model.LevelInfo, ts, map[string]string{"trace_id": id}))
	}
	if got := x.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	x.Close()
}

func TestFlushResetsCounters(t *testing.T) {
	sink := &captureSink{}
	x := NewExtractor(sink, ExtractorOptions{FlushInterval: 30 * time.Millisecond})
	defer x.Close()
	ts := time.Now()

	x.Append(entry("a", model.LevelInfo, ts, map[string]string{"target": "b"}))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := sink.edge("a", "b"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("edge was not flushed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The second interaction lands in a fresh aggregate, not on top of the old one.
	x.Append(entry("a", model.LevelInfo, ts.Add(time.Second), map[string]string{"target": "b"}))
	deadline = time.Now().Add(2 * time.Second)
	for {
		sink.mu.Lock()
		n := len(sink.edges)
		sink.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second flush did not arrive")
		}
		time.Sleep(5 * time.Millisecond)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.edges[1].Count != 1 {
		t.Fatalf("second aggregate must start from zero: %+v", sink.edges[1])
	}
}
