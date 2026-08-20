package graph

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// Topology diff (A2): "what changed" over a window — new and silent services
// and edges, error-rate jumps, and deploys. An agent can answer "what changed
// in the last hour?" from this alone, without touching the logs.

// jumpThreshold — the minimum error-rate increase that counts as a jump.
const jumpThreshold = 0.05

// jumpMinCount — the noise floor: edges with fewer observations in the
// current window are not judged.
const jumpMinCount = 5

// DiffNode — a service that appeared or went silent.
type DiffNode struct {
	App       string    `json:"app"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// DiffEdge — a link that appeared or went silent.
type DiffEdge struct {
	Src       string    `json:"src"`
	Dst       string    `json:"dst"`
	Origin    string    `json:"origin"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// ErrorJump — an edge whose error rate rose markedly versus the previous window.
type ErrorJump struct {
	Src           string  `json:"src"`
	Dst           string  `json:"dst"`
	PrevErrorRate float64 `json:"prev_error_rate"`
	CurErrorRate  float64 `json:"cur_error_rate"`
	CurCount      uint64  `json:"cur_count"`
}

// Diff — everything that changed between [from, to] and the window before it.
type Diff struct {
	From           time.Time  `json:"from"`
	To             time.Time  `json:"to"`
	NewServices    []DiffNode `json:"new_services"`
	SilentServices []DiffNode `json:"silent_services"` // logged before, silent in the window
	NewEdges       []DiffEdge `json:"new_edges"`
	SilentEdges    []DiffEdge `json:"silent_edges"`
	ErrorJumps     []ErrorJump `json:"error_jumps"`
	Deploys        []Deploy   `json:"deploys"`
}

// Diff computes the change report for the trailing window. The comparison
// baseline is the window immediately before it.
func (m *Manager) Diff(ctx context.Context, tenantID string, window time.Duration) (Diff, error) {
	to := time.Now()
	from := to.Add(-window)
	prevFrom := from.Add(-window)

	d := Diff{
		From:           from,
		To:             to,
		NewServices:    []DiffNode{},
		SilentServices: []DiffNode{},
		NewEdges:       []DiffEdge{},
		SilentEdges:    []DiffEdge{},
		ErrorJumps:     []ErrorJump{},
		Deploys:        []Deploy{},
	}

	topo, err := m.store.Topology(ctx, tenantID)
	if err != nil {
		return Diff{}, err
	}

	for _, n := range topo.Nodes {
		dn := DiffNode{App: n.App, FirstSeen: n.FirstSeen, LastSeen: n.LastSeen}
		switch {
		case !n.FirstSeen.Before(from):
			d.NewServices = append(d.NewServices, dn)
		case n.LastSeen.Before(from) && !n.LastSeen.Before(prevFrom):
			// Active in the previous window, silent in this one.
			d.SilentServices = append(d.SilentServices, dn)
		}
	}
	for _, e := range topo.Edges {
		de := DiffEdge{Src: e.Src, Dst: e.Dst, Origin: e.Origin, FirstSeen: e.FirstSeen, LastSeen: e.LastSeen}
		switch {
		case !e.FirstSeen.Before(from):
			d.NewEdges = append(d.NewEdges, de)
		case e.LastSeen.Before(from) && !e.LastSeen.Before(prevFrom):
			d.SilentEdges = append(d.SilentEdges, de)
		}
	}

	// Error-rate jumps: current window vs the one before. Metrics being
	// unavailable degrades the report instead of failing it.
	cur, err := m.metrics.EdgeRatesRange(ctx, tenantID, from, to)
	if err != nil {
		slog.Warn("graph diff: current edge rates unavailable", "err", err)
		cur = map[EdgeKey]Rates{}
	}
	prev, err := m.metrics.EdgeRatesRange(ctx, tenantID, prevFrom, from)
	if err != nil {
		slog.Warn("graph diff: previous edge rates unavailable", "err", err)
		prev = map[EdgeKey]Rates{}
	}
	for k, c := range cur {
		if c.Count < jumpMinCount {
			continue
		}
		curRate := float64(c.Errors) / float64(c.Count)
		prevRate := 0.0
		if p, ok := prev[k]; ok && p.Count > 0 {
			prevRate = float64(p.Errors) / float64(p.Count)
		}
		if curRate-prevRate >= jumpThreshold {
			d.ErrorJumps = append(d.ErrorJumps, ErrorJump{
				Src: k.Src, Dst: k.Dst,
				PrevErrorRate: prevRate, CurErrorRate: curRate, CurCount: c.Count,
			})
		}
	}
	sort.Slice(d.ErrorJumps, func(i, j int) bool {
		return d.ErrorJumps[i].CurErrorRate-d.ErrorJumps[i].PrevErrorRate >
			d.ErrorJumps[j].CurErrorRate-d.ErrorJumps[j].PrevErrorRate
	})

	deploys, err := m.store.Deploys(ctx, tenantID, "", from, 100)
	if err != nil {
		slog.Warn("graph diff: deploys unavailable", "err", err)
	} else {
		d.Deploys = append(d.Deploys, deploys...)
	}

	return d, nil
}
