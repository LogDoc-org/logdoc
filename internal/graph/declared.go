package graph

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The declared graph is the architecture as the code (or an agent reading
// the code) promises it: services and links with a transport and a
// file:line evidence reference. It complements the observed graph — the
// map appears complete before the first log line arrives, including paths
// that carry traffic once a month. Where declared and observed disagree,
// that is drift, and drift is exactly what stale architecture docs never
// show.

// DeclaredNode — a service the code declares.
type DeclaredNode struct {
	App         string `json:"app" yaml:"app"`
	Description string `json:"description,omitempty" yaml:"description"`
	// Group — domain/namespace/team the service belongs to; the UI
	// clusters large maps by it.
	Group string `json:"group,omitempty" yaml:"group"`
}

// DeclaredEdge — a directed link the code declares.
type DeclaredEdge struct {
	Src string `json:"src" yaml:"src"`
	Dst string `json:"dst" yaml:"dst"`
	// Transport — how the services talk: http, grpc, sql, s3, smtp, amqp...
	Transport string `json:"transport,omitempty" yaml:"transport"`
	// Evidence — where the code promises this link, e.g.
	// "src/config/database.js:17" or "nginx.conf: upstream otts_engine".
	Evidence string `json:"evidence,omitempty" yaml:"evidence"`
}

// DeclaredGraph — the full declared architecture of one tenant. A new
// declaration replaces the previous one: the source of truth is whoever
// analyzed the code last, not an accumulation of stale claims.
type DeclaredGraph struct {
	Nodes []DeclaredNode `json:"nodes"`
	Edges []DeclaredEdge `json:"edges"`
}

// DeclaredStore persists the declared graph.
type DeclaredStore interface {
	// ReplaceDeclared atomically replaces the tenant's declared graph.
	ReplaceDeclared(ctx context.Context, tenantID string, g DeclaredGraph) error
	DeclaredGraph(ctx context.Context, tenantID string) (DeclaredGraph, error)
}

// Sized for real platforms: a single enterprise ad platform declared ~700
// nodes on first contact with the feature.
const (
	maxDeclaredNodes = 2000
	maxDeclaredEdges = 10000
)

// ValidateDeclared bounds the declared graph: it ends up rendered and
// exported verbatim, and a replace is all-or-nothing.
func ValidateDeclared(g DeclaredGraph) error {
	if len(g.Nodes) > maxDeclaredNodes {
		return fmt.Errorf("too many nodes (max %d)", maxDeclaredNodes)
	}
	if len(g.Edges) > maxDeclaredEdges {
		return fmt.Errorf("too many edges (max %d)", maxDeclaredEdges)
	}
	apps := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if strings.TrimSpace(n.App) == "" {
			return errors.New("node: app is required")
		}
		if len(n.App) > 200 {
			return fmt.Errorf("node %q: app is too long (max 200)", n.App)
		}
		if len(n.Description) > 2000 {
			return fmt.Errorf("node %q: description is too long (max 2000)", n.App)
		}
		if len(n.Group) > 100 {
			return fmt.Errorf("node %q: group is too long (max 100)", n.App)
		}
		if apps[n.App] {
			return fmt.Errorf("node %q: duplicate", n.App)
		}
		apps[n.App] = true
	}
	seen := make(map[EdgeKey]bool, len(g.Edges))
	for _, e := range g.Edges {
		if strings.TrimSpace(e.Src) == "" || strings.TrimSpace(e.Dst) == "" {
			return errors.New("edge: src and dst are required")
		}
		if len(e.Src) > 200 || len(e.Dst) > 200 {
			return errors.New("edge: src/dst is too long (max 200)")
		}
		if e.Src == e.Dst {
			return fmt.Errorf("edge %s→%s: self-loops are not allowed", e.Src, e.Dst)
		}
		if len(e.Transport) > 50 {
			return fmt.Errorf("edge %s→%s: transport is too long (max 50)", e.Src, e.Dst)
		}
		if len(e.Evidence) > 500 {
			return fmt.Errorf("edge %s→%s: evidence is too long (max 500)", e.Src, e.Dst)
		}
		k := EdgeKey{Src: e.Src, Dst: e.Dst}
		if seen[k] {
			return fmt.Errorf("edge %s→%s: duplicate", e.Src, e.Dst)
		}
		seen[k] = true
	}
	return nil
}

// mergeDeclared folds the declared graph into an observed topology:
// observed nodes/edges get Declared=true (+transport/evidence) when the
// code promises them; declared-only nodes/edges are appended with zero
// counters and origin "declared".
func mergeDeclared(topo Topology, decl DeclaredGraph) Topology {
	nodeIdx := make(map[string]int, len(topo.Nodes))
	for i, n := range topo.Nodes {
		nodeIdx[n.App] = i
	}
	for _, dn := range decl.Nodes {
		if i, ok := nodeIdx[dn.App]; ok {
			topo.Nodes[i].Group = dn.Group
			topo.Nodes[i].Description = dn.Description
			continue
		}
		nodeIdx[dn.App] = len(topo.Nodes)
		topo.Nodes = append(topo.Nodes, Node{
			App: dn.App, DeclaredOnly: true, Group: dn.Group, Description: dn.Description,
		})
	}

	edgeIdx := make(map[EdgeKey]int, len(topo.Edges))
	for i, e := range topo.Edges {
		edgeIdx[EdgeKey{Src: e.Src, Dst: e.Dst}] = i
	}
	for _, de := range decl.Edges {
		if i, ok := edgeIdx[EdgeKey{Src: de.Src, Dst: de.Dst}]; ok {
			topo.Edges[i].Declared = true
			topo.Edges[i].Transport = de.Transport
			topo.Edges[i].Evidence = de.Evidence
			continue
		}
		topo.Edges = append(topo.Edges, Edge{
			Src:       de.Src,
			Dst:       de.Dst,
			Origin:    "declared",
			Declared:  true,
			Transport: de.Transport,
			Evidence:  de.Evidence,
		})
		// A declared edge may reference services nobody listed as nodes.
		for _, app := range []string{de.Src, de.Dst} {
			if _, ok := nodeIdx[app]; !ok {
				nodeIdx[app] = len(topo.Nodes)
				topo.Nodes = append(topo.Nodes, Node{App: app, DeclaredOnly: true})
			}
		}
	}
	return topo
}
