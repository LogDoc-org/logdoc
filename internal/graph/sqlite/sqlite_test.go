package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/graph"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpsertMergesCountersAndOrigin(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1000)
	t1 := time.UnixMilli(2000)

	// First flush: edge discovered via trace.
	err := s.UpsertGraph(ctx,
		[]graph.NodeAgg{{TenantID: "default", App: "api", Count: 10, Errors: 1, LastSeen: t0}},
		[]graph.EdgeAgg{{
			Key:    graph.EdgeKey{TenantID: "default", Src: "api", Dst: "billing"},
			Origin: graph.OriginTrace, Count: 5, Errors: 1, LastSeen: t0,
		}})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Second flush: same edge, now inferred; counters must accumulate.
	err = s.UpsertGraph(ctx,
		[]graph.NodeAgg{{TenantID: "default", App: "api", Count: 3, Errors: 0, LastSeen: t1}},
		[]graph.EdgeAgg{{
			Key:    graph.EdgeKey{TenantID: "default", Src: "api", Dst: "billing"},
			Origin: graph.OriginInferred, Count: 2, Errors: 0, LastSeen: t1,
		}})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	topo, err := s.Topology(ctx, "default")
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	if len(topo.Nodes) != 1 || len(topo.Edges) != 1 {
		t.Fatalf("want 1 node / 1 edge, got %d / %d", len(topo.Nodes), len(topo.Edges))
	}

	n := topo.Nodes[0]
	if n.App != "api" || n.Count != 13 || n.Errors != 1 {
		t.Errorf("node merge: %+v", n)
	}
	if !n.FirstSeen.Equal(t0) || !n.LastSeen.Equal(t1) {
		t.Errorf("node times: first=%v last=%v", n.FirstSeen, n.LastSeen)
	}

	e := topo.Edges[0]
	if e.Src != "api" || e.Dst != "billing" || e.Count != 7 || e.Errors != 1 {
		t.Errorf("edge merge: %+v", e)
	}
	if e.Origin != "both" {
		t.Errorf("origin: want both, got %q", e.Origin)
	}
	if !e.FirstSeen.Equal(t0) || !e.LastSeen.Equal(t1) {
		t.Errorf("edge times: first=%v last=%v", e.FirstSeen, e.LastSeen)
	}
}

func TestLastSeenNeverGoesBackwards(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	late := time.UnixMilli(5000)
	early := time.UnixMilli(3000)

	for _, ts := range []time.Time{late, early} {
		err := s.UpsertGraph(ctx,
			[]graph.NodeAgg{{TenantID: "default", App: "api", Count: 1, LastSeen: ts}}, nil)
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	topo, err := s.Topology(ctx, "default")
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	if got := topo.Nodes[0].LastSeen; !got.Equal(late) {
		t.Errorf("last_seen went backwards: %v", got)
	}
}

func TestTopologyIsTenantScoped(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()

	err := s.UpsertGraph(ctx,
		[]graph.NodeAgg{
			{TenantID: "default", App: "api", Count: 1, LastSeen: now},
			{TenantID: "other", App: "ghost", Count: 1, LastSeen: now},
		},
		[]graph.EdgeAgg{{
			Key:    graph.EdgeKey{TenantID: "other", Src: "ghost", Dst: "ghost2"},
			Origin: graph.OriginTrace, Count: 1, LastSeen: now,
		}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	topo, err := s.Topology(ctx, "default")
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	if len(topo.Nodes) != 1 || topo.Nodes[0].App != "api" {
		t.Errorf("tenant leak in nodes: %+v", topo.Nodes)
	}
	if len(topo.Edges) != 0 {
		t.Errorf("tenant leak in edges: %+v", topo.Edges)
	}
}

func TestEmptyTopology(t *testing.T) {
	s := openTest(t)
	topo, err := s.Topology(context.Background(), "default")
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	if len(topo.Nodes) != 0 || len(topo.Edges) != 0 {
		t.Errorf("want empty topology, got %+v", topo)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/graph.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now()
	err = s.UpsertGraph(context.Background(),
		[]graph.NodeAgg{{TenantID: "default", App: "api", Count: 4, LastSeen: now}}, nil)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	topo, err := s2.Topology(context.Background(), "default")
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	if len(topo.Nodes) != 1 || topo.Nodes[0].Count != 4 {
		t.Errorf("state lost across reopen: %+v", topo.Nodes)
	}
}

func TestDeploysInsertAndDedup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	t0 := time.UnixMilli(1000)
	t1 := time.UnixMilli(2000)
	t2 := time.UnixMilli(3000)

	err := s.InsertDeploys(ctx, "default", []graph.Deploy{
		{App: "billing", Version: "2.3.0", Ts: t0},
		{App: "billing", Version: "2.3.1", Ts: t1},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Restart-safe dedup: re-detecting the current version is a no-op.
	err = s.InsertDeploys(ctx, "default", []graph.Deploy{
		{App: "billing", Version: "2.3.1", Ts: t2},
	})
	if err != nil {
		t.Fatalf("dedup insert: %v", err)
	}

	got, err := s.Deploys(ctx, "default", "billing", time.UnixMilli(0), 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("deploys: got %d want 2 (%+v)", len(got), got)
	}
	// Newest first.
	if got[0].Version != "2.3.1" || got[1].Version != "2.3.0" {
		t.Fatalf("order: %+v", got)
	}
	if !got[0].Ts.Equal(t1) {
		t.Fatalf("ts: %v", got[0].Ts)
	}

	// A rollback to an older version is a new marker again.
	err = s.InsertDeploys(ctx, "default", []graph.Deploy{
		{App: "billing", Version: "2.3.0", Ts: t2},
	})
	if err != nil {
		t.Fatalf("rollback insert: %v", err)
	}
	got, _ = s.Deploys(ctx, "default", "billing", time.UnixMilli(0), 10)
	if len(got) != 3 || got[0].Version != "2.3.0" {
		t.Fatalf("rollback: %+v", got)
	}
}

func TestDeploysFilters(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	err := s.InsertDeploys(ctx, "default", []graph.Deploy{
		{App: "api", Version: "1.0.0", Ts: time.UnixMilli(1000)},
		{App: "web", Version: "4.2.0", Ts: time.UnixMilli(5000)},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// since filter
	got, err := s.Deploys(ctx, "default", "", time.UnixMilli(2000), 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].App != "web" {
		t.Fatalf("since filter: %+v", got)
	}

	// tenant isolation
	got, _ = s.Deploys(ctx, "other", "", time.UnixMilli(0), 10)
	if len(got) != 0 {
		t.Fatalf("tenant leak: %+v", got)
	}
}
