package graph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// memCatalogStore fakes CatalogStore with a map.
type memCatalogStore struct {
	metas map[string]ServiceMeta
}

func newMemCatalogStore() *memCatalogStore {
	return &memCatalogStore{metas: map[string]ServiceMeta{}}
}

func (s *memCatalogStore) UpsertMeta(_ context.Context, _ string, m ServiceMeta) error {
	s.metas[m.App] = m
	return nil
}

func (s *memCatalogStore) DeleteMeta(_ context.Context, _ string, app string) error {
	delete(s.metas, app)
	return nil
}

func (s *memCatalogStore) CatalogMeta(context.Context, string) ([]ServiceMeta, error) {
	out := make([]ServiceMeta, 0, len(s.metas))
	for _, m := range s.metas {
		out = append(out, m)
	}
	return out, nil
}

func TestCatalogConfigWinsOverStored(t *testing.T) {
	store := newMemCatalogStore()
	store.metas["billing"] = ServiceMeta{App: "billing", Owner: "stale-team"}
	store.metas["web"] = ServiceMeta{App: "web", Owner: "team-front"}

	c, err := NewCatalog(store, []ServiceMeta{{App: "billing", Owner: "team-payments"}})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}

	list, err := c.List(context.Background(), "default")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 entries, got %+v", list)
	}
	// Sorted by app: billing (config) then web (api).
	if list[0].Owner != "team-payments" || list[0].Source != "config" {
		t.Errorf("config entry must shadow the stored one: %+v", list[0])
	}
	if list[1].App != "web" || list[1].Source != "api" {
		t.Errorf("stored entry: %+v", list[1])
	}

	m, err := c.Get(context.Background(), "default", "billing")
	if err != nil || m.Owner != "team-payments" {
		t.Errorf("get billing: %+v, %v", m, err)
	}
	if m, _ := c.Get(context.Background(), "default", "ghost"); m.Source != "" || m.App != "" {
		t.Errorf("ghost must be a zero meta: %+v", m)
	}
}

func TestCatalogConfigEntriesReadOnly(t *testing.T) {
	c, err := NewCatalog(newMemCatalogStore(), []ServiceMeta{{App: "billing"}})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if err := c.Put(context.Background(), "default", ServiceMeta{App: "billing"}); err != ErrConfigDefined {
		t.Errorf("put on a config app: want ErrConfigDefined, got %v", err)
	}
	if err := c.Delete(context.Background(), "default", "billing"); err != ErrConfigDefined {
		t.Errorf("delete on a config app: want ErrConfigDefined, got %v", err)
	}
}

func TestCatalogConfigValidation(t *testing.T) {
	if _, err := NewCatalog(newMemCatalogStore(), []ServiceMeta{{App: ""}}); err == nil {
		t.Error("empty app in config must be rejected")
	}
	if _, err := NewCatalog(newMemCatalogStore(),
		[]ServiceMeta{{App: "a"}, {App: "a"}}); err == nil {
		t.Error("duplicate app in config must be rejected")
	}
}

func TestValidateMeta(t *testing.T) {
	bad := []ServiceMeta{
		{App: "  "},
		{App: "a", Links: map[string]string{"repo": "not-a-url"}},
		{App: "a", Links: map[string]string{"repo": "ftp://example.com/x"}},
		{App: "a", Links: map[string]string{"": "https://example.com"}},
		{App: "a", Tags: []string{""}},
		{App: "a", Owner: strings.Repeat("x", 201)},
		{App: "a", Description: strings.Repeat("x", 2001)},
	}
	for i, m := range bad {
		if err := ValidateMeta(m); err == nil {
			t.Errorf("case %d: %+v must be rejected", i, m)
		}
	}
	ok := ServiceMeta{
		App: "billing", Owner: "team-payments", Description: "charges cards",
		Links: map[string]string{"repo": "https://github.com/x/y", "runbook": "http://wiki/rb"},
		Tags:  []string{"critical", "pci"},
	}
	if err := ValidateMeta(ok); err != nil {
		t.Errorf("valid meta rejected: %v", err)
	}
}

func TestCatalogHTTP(t *testing.T) {
	c, err := NewCatalog(newMemCatalogStore(), []ServiceMeta{{App: "cfg-app", Owner: "cfg-team"}})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/catalog", NewCatalogListHandler(c))
	for _, m := range []string{"PUT", "DELETE"} {
		mux.Handle(m+" /api/v1/catalog/{app}", NewCatalogEditHandler(c))
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	do := func(method, path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}

	// Create, then list.
	res := do("PUT", "/api/v1/catalog/billing",
		`{"owner":"team-payments","links":{"repo":"https://github.com/x/y"},"tags":["critical"]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", res.StatusCode)
	}
	res = do("GET", "/api/v1/catalog", "")
	var list struct {
		Services []ServiceMeta `json:"services"`
	}
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(list.Services) != 2 || list.Services[0].App != "billing" || list.Services[1].Source != "config" {
		t.Fatalf("list: %+v", list.Services)
	}
	if list.Services[0].Owner != "team-payments" || list.Services[0].Source != "api" {
		t.Errorf("billing entry: %+v", list.Services[0])
	}

	// Validation error → 400; config-owned → 409.
	if res = do("PUT", "/api/v1/catalog/x", `{"links":{"r":"nope"}}`); res.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid link: want 400, got %d", res.StatusCode)
	}
	if res = do("PUT", "/api/v1/catalog/cfg-app", `{}`); res.StatusCode != http.StatusConflict {
		t.Errorf("config app put: want 409, got %d", res.StatusCode)
	}
	if res = do("DELETE", "/api/v1/catalog/cfg-app", ""); res.StatusCode != http.StatusConflict {
		t.Errorf("config app delete: want 409, got %d", res.StatusCode)
	}

	// Delete the runtime entry.
	if res = do("DELETE", "/api/v1/catalog/billing", ""); res.StatusCode != http.StatusOK {
		t.Errorf("delete: %d", res.StatusCode)
	}
	res = do("GET", "/api/v1/catalog", "")
	list.Services = nil
	_ = json.NewDecoder(res.Body).Decode(&list)
	if len(list.Services) != 1 || list.Services[0].App != "cfg-app" {
		t.Errorf("after delete: %+v", list.Services)
	}
}
