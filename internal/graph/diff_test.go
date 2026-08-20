package graph

import (
	"context"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// diffStore fakes graph.Store with fixed topology and deploys.
type diffStore struct {
	topo    Topology
	deploys []Deploy
}

func (s *diffStore) UpsertGraph(context.Context, []NodeAgg, []EdgeAgg) error { return nil }
func (s *diffStore) Topology(context.Context, string) (Topology, error)     { return s.topo, nil }
func (s *diffStore) InsertDeploys(context.Context, string, []Deploy) error  { return nil }
func (s *diffStore) Close() error                                           { return nil }

func (s *diffStore) Deploys(_ context.Context, _, app string, since time.Time, _ int) ([]Deploy, error) {
	var out []Deploy
	for _, d := range s.deploys {
		if (app == "" || d.App == app) && !d.Ts.Before(since) {
			out = append(out, d)
		}
	}
	return out, nil
}

// diffMetrics fakes MetricsBackend with per-range rates keyed by range start.
type diffMetrics struct {
	cur, prev map[EdgeKey]Rates
	pivot     time.Time // ranges starting before pivot get prev
}

func (m *diffMetrics) InsertEdgeMetrics(context.Context, time.Time, []EdgeAgg) error { return nil }
func (m *diffMetrics) EdgeRates(context.Context, string, time.Duration) (map[EdgeKey]Rates, error) {
	return m.cur, nil
}

func (m *diffMetrics) EdgeRatesRange(_ context.Context, _ string, from, _ time.Time) (map[EdgeKey]Rates, error) {
	if from.Before(m.pivot) {
		return m.prev, nil
	}
	return m.cur, nil
}

func TestDiff(t *testing.T) {
	now := time.Now()
	in := now.Add(-30 * time.Minute)       // inside the 1h window
	before := now.Add(-90 * time.Minute)   // inside the previous window
	ancient := now.Add(-10 * time.Hour)    // older than both

	key := func(src, dst string) EdgeKey {
		return EdgeKey{TenantID: model.DefaultTenant, Src: src, Dst: dst}
	}

	store := &diffStore{
		topo: Topology{
			Nodes: []Node{
				{App: "fresh", FirstSeen: in, LastSeen: in},           // new
				{App: "gone", FirstSeen: ancient, LastSeen: before},   // went silent
				{App: "steady", FirstSeen: ancient, LastSeen: in},     // unchanged
				{App: "long-dead", FirstSeen: ancient, LastSeen: ancient}, // silent long ago — not reported
			},
			Edges: []Edge{
				{Src: "steady", Dst: "fresh", Origin: "inferred", FirstSeen: in, LastSeen: in},   // new
				{Src: "steady", Dst: "gone", Origin: "trace", FirstSeen: ancient, LastSeen: before}, // silent
				{Src: "steady", Dst: "steady2", Origin: "trace", FirstSeen: ancient, LastSeen: in},
			},
		},
		deploys: []Deploy{
			{App: "fresh", Version: "1.0.0", Ts: in},
			{App: "steady", Version: "0.9.0", Ts: ancient}, // outside the window
		},
	}
	metrics := &diffMetrics{
		pivot: now.Add(-time.Hour),
		cur: map[EdgeKey]Rates{
			key("steady", "steady2"): {Count: 100, Errors: 30}, // 30% now
			key("steady", "fresh"):   {Count: 3, Errors: 3},    // under the noise floor
		},
		prev: map[EdgeKey]Rates{
			key("steady", "steady2"): {Count: 100, Errors: 2}, // 2% before
		},
	}

	d, err := NewManager(store, metrics).Diff(context.Background(), model.DefaultTenant, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if len(d.NewServices) != 1 || d.NewServices[0].App != "fresh" {
		t.Errorf("new services: %+v", d.NewServices)
	}
	if len(d.SilentServices) != 1 || d.SilentServices[0].App != "gone" {
		t.Errorf("silent services: %+v", d.SilentServices)
	}
	if len(d.NewEdges) != 1 || d.NewEdges[0].Dst != "fresh" {
		t.Errorf("new edges: %+v", d.NewEdges)
	}
	if len(d.SilentEdges) != 1 || d.SilentEdges[0].Dst != "gone" {
		t.Errorf("silent edges: %+v", d.SilentEdges)
	}
	if len(d.ErrorJumps) != 1 {
		t.Fatalf("error jumps: %+v", d.ErrorJumps)
	}
	j := d.ErrorJumps[0]
	if j.Src != "steady" || j.Dst != "steady2" || j.CurErrorRate < 0.29 || j.PrevErrorRate > 0.03 {
		t.Errorf("jump: %+v", j)
	}
	if len(d.Deploys) != 1 || d.Deploys[0].Version != "1.0.0" {
		t.Errorf("deploys: %+v", d.Deploys)
	}
}
