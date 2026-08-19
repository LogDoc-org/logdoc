package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/LogDoc-org/logdoc/internal/graph"
	graphsqlite "github.com/LogDoc-org/logdoc/internal/graph/sqlite"
	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/query"
)

type fakeBackend struct {
	entries  []model.Entry
	lastPlan query.Plan
}

func (f *fakeBackend) Query(_ context.Context, p query.Plan) ([]model.Entry, error) {
	f.lastPlan = p
	var out []model.Entry
	for _, e := range f.entries {
		if p.Matches(e) {
			out = append(out, e)
		}
	}
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

type fakeMetrics struct{}

func (fakeMetrics) InsertEdgeMetrics(context.Context, time.Time, []graph.EdgeAgg) error {
	return nil
}

func (fakeMetrics) EdgeRates(context.Context, string, time.Duration) (map[graph.EdgeKey]graph.Rates, error) {
	return map[graph.EdgeKey]graph.Rates{}, nil
}

func newSession(t *testing.T, backend *fakeBackend) *sdk.ClientSession {
	t.Helper()

	store, err := graphsqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("graph store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	err = store.UpsertGraph(context.Background(),
		[]graph.NodeAgg{
			{TenantID: model.DefaultTenant, App: "api", Count: 100, Errors: 1, LastSeen: now},
			{TenantID: model.DefaultTenant, App: "billing", Count: 50, Errors: 10, LastSeen: now},
		},
		[]graph.EdgeAgg{{
			Key:    graph.EdgeKey{TenantID: model.DefaultTenant, Src: "api", Dst: "billing"},
			Origin: graph.OriginTrace, Count: 40, Errors: 10, LastSeen: now,
		}})
	if err != nil {
		t.Fatalf("seed graph: %v", err)
	}

	srv := New(backend, nil, graph.NewManager(store, fakeMetrics{}), "test")

	st, ct := sdk.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func structured(t *testing.T, res *sdk.CallToolResult, into any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured: %v", err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
}

func TestToolsAreListed(t *testing.T) {
	session := newSession(t, &fakeBackend{})
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, want := range []string{"query_logs", "get_topology", "get_service_card"} {
		if !got[want] {
			t.Errorf("missing tool %q in %v", want, got)
		}
	}
}

func TestQueryLogs(t *testing.T) {
	backend := &fakeBackend{entries: []model.Entry{
		{TenantID: model.DefaultTenant, App: "billing", Lvl: model.LevelError, Msg: "charge declined", Ts: time.Now()},
		{TenantID: model.DefaultTenant, App: "api", Lvl: model.LevelInfo, Msg: "create order", Ts: time.Now()},
	}}
	session := newSession(t, backend)

	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "query_logs",
		Arguments: map[string]any{"app": "billing", "lvl": "ERROR", "window": "10m", "limit": 5},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out queryLogsResult
	structured(t, res, &out)
	if out.Count != 1 || out.Entries[0].Msg != "charge declined" {
		t.Fatalf("unexpected result: %+v", out)
	}
	if backend.lastPlan.From == nil || time.Since(*backend.lastPlan.From) > 11*time.Minute {
		t.Errorf("window not applied to the plan: %+v", backend.lastPlan)
	}
	if backend.lastPlan.Limit != 5 {
		t.Errorf("limit not applied: %d", backend.lastPlan.Limit)
	}
}

func TestQueryLogsRejectsBadWindow(t *testing.T) {
	session := newSession(t, &fakeBackend{})
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "query_logs",
		Arguments: map[string]any{"window": "48h"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error for a 48h window")
	}
}

func TestGetTopology(t *testing.T) {
	session := newSession(t, &fakeBackend{})
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_topology",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out getTopologyResult
	structured(t, res, &out)
	if len(out.Nodes) != 2 || len(out.Edges) != 1 {
		t.Fatalf("nodes/edges: %+v", out)
	}
	if out.Edges[0].Src != "api" || out.Edges[0].Dst != "billing" {
		t.Errorf("edge: %+v", out.Edges[0])
	}
	if !strings.Contains(out.Mermaid, "flowchart LR") {
		t.Errorf("mermaid missing: %q", out.Mermaid)
	}
}

func TestGetServiceCard(t *testing.T) {
	backend := &fakeBackend{entries: []model.Entry{
		{TenantID: model.DefaultTenant, App: "billing", Lvl: model.LevelError, Msg: "charge declined", Ts: time.Now()},
		{TenantID: model.DefaultTenant, App: "billing", Lvl: model.LevelInfo, Msg: "charge ok", Ts: time.Now()},
	}}
	session := newSession(t, backend)

	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_service_card",
		Arguments: map[string]any{"app": "billing"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out getServiceCardResult
	structured(t, res, &out)
	if out.Count != 50 || out.Errors != 10 {
		t.Errorf("counters: %+v", out)
	}
	if len(out.Inbound) != 1 || out.Inbound[0].Service != "api" {
		t.Errorf("inbound: %+v", out.Inbound)
	}
	if len(out.Outbound) != 0 {
		t.Errorf("outbound: %+v", out.Outbound)
	}
	if len(out.RecentErrors) != 1 || out.RecentErrors[0].Msg != "charge declined" {
		t.Errorf("recent errors: %+v", out.RecentErrors)
	}
}

func TestGetServiceCardUnknownService(t *testing.T) {
	session := newSession(t, &fakeBackend{})
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_service_card",
		Arguments: map[string]any{"app": "ghost"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error for an unknown service")
	}
}
