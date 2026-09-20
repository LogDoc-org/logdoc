package graph

import (
	"strings"
	"testing"
)

func TestValidateDeclared(t *testing.T) {
	ok := DeclaredGraph{
		Nodes: []DeclaredNode{{App: "nginx"}, {App: "engine", Description: "the app"}},
		Edges: []DeclaredEdge{
			{Src: "nginx", Dst: "engine", Transport: "http", Evidence: "nginx.conf:69"},
			{Src: "engine", Dst: "postgres", Transport: "sql"},
		},
	}
	if err := ValidateDeclared(ok); err != nil {
		t.Fatalf("valid graph rejected: %v", err)
	}

	bad := []DeclaredGraph{
		{Nodes: []DeclaredNode{{App: ""}}},
		{Nodes: []DeclaredNode{{App: "a"}, {App: "a"}}},
		{Edges: []DeclaredEdge{{Src: "a", Dst: ""}}},
		{Edges: []DeclaredEdge{{Src: "a", Dst: "a"}}},
		{Edges: []DeclaredEdge{{Src: "a", Dst: "b"}, {Src: "a", Dst: "b"}}},
		{Edges: []DeclaredEdge{{Src: "a", Dst: "b", Evidence: strings.Repeat("x", 501)}}},
	}
	for i, g := range bad {
		if err := ValidateDeclared(g); err == nil {
			t.Errorf("case %d must be rejected: %+v", i, g)
		}
	}
}

func TestMergeDeclared(t *testing.T) {
	topo := Topology{
		Nodes: []Node{{App: "nginx", Count: 100}, {App: "engine", Count: 50}},
		Edges: []Edge{{Src: "nginx", Dst: "engine", Origin: "inferred", Count: 40}},
	}
	decl := DeclaredGraph{
		Nodes: []DeclaredNode{{App: "engine"}, {App: "postgres"}},
		Edges: []DeclaredEdge{
			{Src: "nginx", Dst: "engine", Transport: "http", Evidence: "nginx.conf:69"},
			{Src: "engine", Dst: "postgres", Transport: "sql", Evidence: "database.js:17"},
			// Edge referencing a service listed nowhere as a node.
			{Src: "engine", Dst: "smtp", Transport: "smtp"},
		},
	}

	got := mergeDeclared(topo, decl)

	// Observed edge confirmed by the declaration keeps its origin.
	e0 := got.Edges[0]
	if !e0.Declared || e0.Transport != "http" || e0.Evidence != "nginx.conf:69" || e0.Origin != "inferred" || e0.Count != 40 {
		t.Errorf("confirmed edge: %+v", e0)
	}
	// Declared-only edge appears with zero counters and origin "declared".
	var sql *Edge
	for i := range got.Edges {
		if got.Edges[i].Dst == "postgres" {
			sql = &got.Edges[i]
		}
	}
	if sql == nil || sql.Origin != "declared" || !sql.Declared || sql.Count != 0 || sql.Transport != "sql" {
		t.Fatalf("declared-only edge: %+v", sql)
	}
	// Declared-only nodes appear, including ones referenced only by edges.
	apps := map[string]Node{}
	for _, n := range got.Nodes {
		apps[n.App] = n
	}
	if !apps["postgres"].DeclaredOnly || !apps["smtp"].DeclaredOnly {
		t.Errorf("declared-only nodes: %+v", got.Nodes)
	}
	if apps["engine"].DeclaredOnly || apps["engine"].Count != 50 {
		t.Errorf("observed node must stay observed: %+v", apps["engine"])
	}
	if len(got.Nodes) != 4 {
		t.Errorf("want 4 nodes, got %+v", got.Nodes)
	}
}
