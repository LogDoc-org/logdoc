package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// parseWindow reads ?window= (default 5m, bounds 1s..24h).
// Returns ok=false after writing the error response.
func parseWindow(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	window := 5 * time.Minute
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 24*time.Hour {
			http.Error(w, `{"error":"invalid window (want 1s..24h)"}`, http.StatusBadRequest)
			return 0, false
		}
		window = d
	}
	return window, true
}

// NewHTTPHandler — GET /api/v1/topology?window=5m
// Returns the tenant graph with windowed edge rates.
func NewHTTPHandler(m *Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		window, ok := parseWindow(w, r)
		if !ok {
			return
		}

		topo, err := m.Topology(r.Context(), model.DefaultTenant, window)
		if err != nil {
			http.Error(w, `{"error":"topology unavailable"}`, http.StatusInternalServerError)
			return
		}
		if topo.Nodes == nil {
			topo.Nodes = []Node{}
		}
		if topo.Edges == nil {
			topo.Edges = []Edge{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(topo)
	})
}

// NewDiffHandler — GET /api/v1/topology/diff?window=1h
// "What changed": new/silent services and edges, error-rate jumps, deploys.
func NewDiffHandler(m *Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		window := time.Hour
		if v := r.URL.Query().Get("window"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 || d > 24*time.Hour {
				http.Error(w, `{"error":"invalid window (want 1s..24h)"}`, http.StatusBadRequest)
				return
			}
			window = d
		}
		diff, err := m.Diff(r.Context(), model.DefaultTenant, window)
		if err != nil {
			http.Error(w, `{"error":"diff unavailable"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diff)
	})
}

// NewDeploysHandler — GET /api/v1/deploys?app=billing&window=24h&limit=20
// Deploy markers detected from logs, newest first; empty app = all services.
func NewDeploysHandler(m *Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		window := 24 * time.Hour
		if v := r.URL.Query().Get("window"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 || d > 30*24*time.Hour {
				http.Error(w, `{"error":"invalid window (want 1s..720h)"}`, http.StatusBadRequest)
				return
			}
			window = d
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n > 1000 {
				http.Error(w, `{"error":"invalid limit (want 1..1000)"}`, http.StatusBadRequest)
				return
			}
			limit = n
		}

		deploys, err := m.Deploys(r.Context(), model.DefaultTenant,
			r.URL.Query().Get("app"), time.Now().Add(-window), limit)
		if err != nil {
			http.Error(w, `{"error":"deploys unavailable"}`, http.StatusInternalServerError)
			return
		}
		if deploys == nil {
			deploys = []Deploy{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"deploys": deploys})
	})
}

// NewDeclaredHandler — GET/PUT /api/v1/topology/declared
// GET returns the declared graph; PUT replaces it (typically posted by an
// agent that analyzed the repository: nodes, edges, transports, evidence).
func NewDeclaredHandler(m *Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			g, err := m.Declared(r.Context(), model.DefaultTenant)
			if err != nil {
				http.Error(w, `{"error":"declared graph unavailable"}`, http.StatusInternalServerError)
				return
			}
			if g.Nodes == nil {
				g.Nodes = []DeclaredNode{}
			}
			if g.Edges == nil {
				g.Edges = []DeclaredEdge{}
			}
			_ = json.NewEncoder(w).Encode(g)
			return
		}

		var g DeclaredGraph
		// 16MB: a few thousand nodes with rich descriptions and per-edge
		// business evidence outgrow 1MB easily.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&g); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
		if err := m.DeclareTopology(r.Context(), model.DefaultTenant, g); err != nil {
			b, _ := json.Marshal(map[string]string{"error": err.Error()})
			http.Error(w, string(b), http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":"ok","nodes":%d,"edges":%d}`, len(g.Nodes), len(g.Edges))
	})
}

// NewCatalogListHandler — GET /api/v1/catalog
// Every catalog entry of the tenant: config-declared and runtime-edited.
func NewCatalogListHandler(c *Catalog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		services, err := c.List(r.Context(), model.DefaultTenant)
		if err != nil {
			http.Error(w, `{"error":"catalog unavailable"}`, http.StatusInternalServerError)
			return
		}
		if services == nil {
			services = []ServiceMeta{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"services": services})
	})
}

// NewCatalogEditHandler — PUT/DELETE /api/v1/catalog/{app}
// Creates, replaces or removes one runtime entry; config-declared entries
// are read-only here (edit logdoc.yml instead).
func NewCatalogEditHandler(c *Catalog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app := r.PathValue("app")
		w.Header().Set("Content-Type", "application/json")

		var err error
		switch r.Method {
		case http.MethodDelete:
			err = c.Delete(r.Context(), model.DefaultTenant, app)
		default: // PUT
			var m ServiceMeta
			if derr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&m); derr != nil {
				http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
				return
			}
			m.App = app // the path is authoritative
			err = c.Put(r.Context(), model.DefaultTenant, m)
		}

		switch {
		case err == nil:
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case errors.Is(err, ErrConfigDefined):
			http.Error(w, `{"error":"defined in the config file (read-only via API)"}`, http.StatusConflict)
		default:
			b, _ := json.Marshal(map[string]string{"error": err.Error()})
			http.Error(w, string(b), http.StatusBadRequest)
		}
	})
}

// NewExportHandler — GET /api/v1/topology/export?format=mermaid|markdown|backstage&window=5m
// Renders the current architecture map as text: diagrams for docs, or
// Backstage catalog entities (point Backstage's URL reader at this endpoint
// — with ?api_key=<token> when auth is on — and its catalog keeps itself
// fresh from logs).
func NewExportHandler(m *Manager, c *Catalog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		format := r.URL.Query().Get("format")
		if format == "" {
			format = "mermaid"
		}
		if format != "mermaid" && format != "markdown" && format != "backstage" {
			http.Error(w, `{"error":"invalid format (want mermaid, markdown or backstage)"}`, http.StatusBadRequest)
			return
		}
		window, ok := parseWindow(w, r)
		if !ok {
			return
		}

		topo, err := m.Topology(r.Context(), model.DefaultTenant, window)
		if err != nil {
			http.Error(w, `{"error":"topology unavailable"}`, http.StatusInternalServerError)
			return
		}

		var out string
		switch format {
		case "markdown":
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			out = Markdown(topo)
		case "backstage":
			metas, err := c.List(r.Context(), model.DefaultTenant)
			if err != nil {
				http.Error(w, `{"error":"catalog unavailable"}`, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
			out = Backstage(topo, metas)
		default:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			out = Mermaid(topo)
		}
		_, _ = w.Write([]byte(out))
	})
}
