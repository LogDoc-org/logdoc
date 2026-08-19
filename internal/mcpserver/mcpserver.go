// Package mcpserver — the agent interface: an embedded MCP server with three
// tools (query_logs, get_topology, get_service_card) over Streamable HTTP.
// Agents get the same view of the system as the UI: logs, the architecture
// map, and per-service summaries.
package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LogDoc-org/logdoc/internal/graph"
	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/query"
)

type Server struct {
	backend query.Backend
	stats   query.StatsSink
	manager *graph.Manager
	mcp     *sdk.Server
}

// New builds the MCP server and registers the tools.
func New(backend query.Backend, stats query.StatsSink, manager *graph.Manager, version string) *Server {
	s := &Server{backend: backend, stats: stats, manager: manager}
	s.mcp = sdk.NewServer(&sdk.Implementation{Name: "logdoc", Version: version}, nil)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "query_logs",
		Description: "Search log entries. Filter by application, level, substring and " +
			"structured fields over a trailing time window. Returns the newest entries first.",
	}, s.queryLogs)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "get_topology",
		Description: "The architecture map built from logs: services (nodes) and directed " +
			"call edges with rates and error rates over a trailing window. " +
			"Also returns the map as a Mermaid diagram.",
	}, s.getTopology)

	sdk.AddTool(s.mcp, &sdk.Tool{
		Name: "get_service_card",
		Description: "Everything about one service: entry/error counts, inbound and outbound " +
			"edges with rates, and its most recent error log entries.",
	}, s.getServiceCard)

	return s
}

// Handler returns the Streamable HTTP handler (mount on /mcp).
func (s *Server) Handler() http.Handler {
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s.mcp }, nil)
}

// MCP returns the underlying SDK server (used by tests).
func (s *Server) MCP() *sdk.Server { return s.mcp }

func parseWindow(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > 24*time.Hour {
		return 0, fmt.Errorf("invalid window %q (want a duration between 1s and 24h, e.g. 15m)", s)
	}
	return d, nil
}

// --- query_logs ---

type queryLogsArgs struct {
	App    string            `json:"app,omitempty" jsonschema:"application name(s), comma-separated; empty = all"`
	Lvl    string            `json:"lvl,omitempty" jsonschema:"level name(s), comma-separated: DEBUG INFO LOG WARN ERROR SEVERE PANIC"`
	Q      string            `json:"q,omitempty" jsonschema:"case-insensitive substring to search in the message"`
	Fields map[string]string `json:"fields,omitempty" jsonschema:"exact-match filters on structured fields, e.g. {\"trace_id\":\"abc\"}"`
	Window string            `json:"window,omitempty" jsonschema:"trailing time window like 15m or 2h (default 1h, max 24h)"`
	Limit  int               `json:"limit,omitempty" jsonschema:"maximum entries to return (default 100)"`
}

type queryLogsResult struct {
	Entries []query.EntryDTO `json:"entries"`
	Count   int              `json:"count"`
}

func (s *Server) queryLogs(ctx context.Context, _ *sdk.CallToolRequest, args queryLogsArgs) (*sdk.CallToolResult, queryLogsResult, error) {
	values := url.Values{}
	// ParsePlan splits comma lists for lvl but expects repeated app params.
	for _, a := range splitComma(args.App) {
		values.Add("app", a)
	}
	if args.Lvl != "" {
		values.Set("lvl", args.Lvl)
	}
	if args.Q != "" {
		values.Set("q", args.Q)
	}
	for k, v := range args.Fields {
		values.Set("field."+k, v)
	}
	if args.Limit > 0 {
		values.Set("limit", strconv.Itoa(args.Limit))
	}
	plan, err := query.ParsePlan(values)
	if err != nil {
		return nil, queryLogsResult{}, err
	}
	window, err := parseWindow(args.Window, time.Hour)
	if err != nil {
		return nil, queryLogsResult{}, err
	}
	from := time.Now().Add(-window)
	plan.From = &from

	start := time.Now()
	entries, err := s.backend.Query(ctx, plan)
	if err != nil {
		return nil, queryLogsResult{}, fmt.Errorf("query failed: %w", err)
	}
	if s.stats != nil {
		go s.stats.RecordQueryStats(context.WithoutCancel(ctx), query.Stats{
			Ts:         start,
			TenantID:   plan.TenantID,
			PlanJSON:   plan.JSON(),
			DurationMs: time.Since(start).Milliseconds(),
			Rows:       len(entries),
		})
	}

	res := queryLogsResult{Entries: make([]query.EntryDTO, len(entries)), Count: len(entries)}
	for i, e := range entries {
		res.Entries[i] = query.ToDTO(e)
	}
	return nil, res, nil
}

func splitComma(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// --- get_topology ---

type getTopologyArgs struct {
	Window string `json:"window,omitempty" jsonschema:"trailing window for edge rates, like 5m or 1h (default 5m, max 24h)"`
}

type getTopologyResult struct {
	Nodes   []graph.Node `json:"nodes"`
	Edges   []graph.Edge `json:"edges"`
	Mermaid string       `json:"mermaid"`
}

func (s *Server) getTopology(ctx context.Context, _ *sdk.CallToolRequest, args getTopologyArgs) (*sdk.CallToolResult, getTopologyResult, error) {
	window, err := parseWindow(args.Window, 5*time.Minute)
	if err != nil {
		return nil, getTopologyResult{}, err
	}
	topo, err := s.manager.Topology(ctx, model.DefaultTenant, window)
	if err != nil {
		return nil, getTopologyResult{}, fmt.Errorf("topology unavailable: %w", err)
	}
	if topo.Nodes == nil {
		topo.Nodes = []graph.Node{}
	}
	if topo.Edges == nil {
		topo.Edges = []graph.Edge{}
	}
	return nil, getTopologyResult{Nodes: topo.Nodes, Edges: topo.Edges, Mermaid: graph.Mermaid(topo)}, nil
}

// --- get_service_card ---

type getServiceCardArgs struct {
	App    string `json:"app" jsonschema:"the service (application) name"`
	Window string `json:"window,omitempty" jsonschema:"trailing window for rates and recent errors, like 15m (default 15m, max 24h)"`
}

type edgeInfo struct {
	Service   string  `json:"service"`
	Origin    string  `json:"origin"`
	RPS       float64 `json:"rps"`
	ErrorRate float64 `json:"error_rate"`
	Count     uint64  `json:"count"`
}

type getServiceCardResult struct {
	App          string           `json:"app"`
	FirstSeen    time.Time        `json:"first_seen"`
	LastSeen     time.Time        `json:"last_seen"`
	Count        uint64           `json:"count"`
	Errors       uint64           `json:"errors"`
	Inbound      []edgeInfo       `json:"inbound"`  // services calling this one
	Outbound     []edgeInfo       `json:"outbound"` // services this one calls
	RecentErrors []query.EntryDTO `json:"recent_errors"`
}

func (s *Server) getServiceCard(ctx context.Context, _ *sdk.CallToolRequest, args getServiceCardArgs) (*sdk.CallToolResult, getServiceCardResult, error) {
	if args.App == "" {
		return nil, getServiceCardResult{}, fmt.Errorf("app is required")
	}
	window, err := parseWindow(args.Window, 15*time.Minute)
	if err != nil {
		return nil, getServiceCardResult{}, err
	}

	topo, err := s.manager.Topology(ctx, model.DefaultTenant, window)
	if err != nil {
		return nil, getServiceCardResult{}, fmt.Errorf("topology unavailable: %w", err)
	}

	res := getServiceCardResult{App: args.App, Inbound: []edgeInfo{}, Outbound: []edgeInfo{}, RecentErrors: []query.EntryDTO{}}
	found := false
	for _, n := range topo.Nodes {
		if n.App == args.App {
			res.FirstSeen, res.LastSeen = n.FirstSeen, n.LastSeen
			res.Count, res.Errors = n.Count, n.Errors
			found = true
			break
		}
	}
	if !found {
		return nil, getServiceCardResult{}, fmt.Errorf("unknown service %q (see get_topology for the list)", args.App)
	}
	for _, e := range topo.Edges {
		switch args.App {
		case e.Dst:
			res.Inbound = append(res.Inbound, edgeInfo{Service: e.Src, Origin: e.Origin, RPS: e.RPS, ErrorRate: e.ErrorRate, Count: e.Count})
		case e.Src:
			res.Outbound = append(res.Outbound, edgeInfo{Service: e.Dst, Origin: e.Origin, RPS: e.RPS, ErrorRate: e.ErrorRate, Count: e.Count})
		}
	}

	from := time.Now().Add(-window)
	entries, err := s.backend.Query(ctx, query.Plan{
		TenantID: model.DefaultTenant,
		Apps:     []string{args.App},
		Levels:   []model.Level{model.LevelError, model.LevelSevere, model.LevelPanic},
		From:     &from,
		Limit:    20,
	})
	if err != nil {
		return nil, getServiceCardResult{}, fmt.Errorf("recent errors query failed: %w", err)
	}
	for _, e := range entries {
		res.RecentErrors = append(res.RecentErrors, query.ToDTO(e))
	}
	return nil, res, nil
}
