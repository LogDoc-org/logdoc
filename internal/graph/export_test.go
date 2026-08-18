package graph

import (
	"strings"
	"testing"
	"time"
)

func demoTopology() Topology {
	ts := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	return Topology{
		Nodes: []Node{
			{App: "api", FirstSeen: ts, LastSeen: ts, Count: 100, Errors: 2},
			{App: "billing", FirstSeen: ts, LastSeen: ts, Count: 50},
			{App: "notifier", FirstSeen: ts, LastSeen: ts, Count: 10},
		},
		Edges: []Edge{
			{Src: "api", Dst: "billing", Origin: "trace", Count: 40, RPS: 1.5, ErrorRate: 0.05},
			{Src: "billing", Dst: "notifier", Origin: "inferred", Count: 8},
		},
	}
}

func TestMermaid(t *testing.T) {
	out := Mermaid(demoTopology())
	if !strings.HasPrefix(out, "flowchart LR\n") {
		t.Fatalf("missing header:\n%s", out)
	}
	for _, want := range []string{
		`n0["api"]`,
		`n1["billing"]`,
		`n2["notifier"]`,
		`n0 -->|1.5 rps, 5.0% err| n1`,
		`n1 --> n2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestMermaidEscapesUnsafeNames(t *testing.T) {
	topo := Topology{
		Nodes: []Node{{App: `svc"x|y`}, {App: "plain"}},
		Edges: []Edge{{Src: `svc"x|y`, Dst: "plain"}},
	}
	out := Mermaid(topo)
	if strings.Contains(out, `svc"x`) || strings.Contains(out, "x|y") {
		t.Errorf("unescaped name leaked into diagram:\n%s", out)
	}
	if !strings.Contains(out, "svc#quot;x#124;y") {
		t.Errorf("expected escaped label:\n%s", out)
	}
	if !strings.Contains(out, "n0 --> n1") {
		t.Errorf("edge must use safe ids:\n%s", out)
	}
}

func TestMermaidSkipsEdgesWithUnknownNodes(t *testing.T) {
	topo := Topology{
		Nodes: []Node{{App: "api"}},
		Edges: []Edge{{Src: "api", Dst: "ghost"}},
	}
	out := Mermaid(topo)
	if strings.Contains(out, "ghost") || strings.Contains(out, "-->") {
		t.Errorf("edge to unknown node must be skipped:\n%s", out)
	}
}

func TestMarkdown(t *testing.T) {
	out := Markdown(demoTopology())
	for _, want := range []string{
		"# Architecture",
		"```mermaid",
		"flowchart LR",
		"## Services",
		"| api | 100 | 2 | 2026-08-17 12:00:00 UTC |",
		"## Links",
		"| api | billing | trace | 1.50 rps | 5.00% |",
		"| billing | notifier | inferred | — | 0.00% |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
