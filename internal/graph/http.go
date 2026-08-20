package graph

import (
	"encoding/json"
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

// NewExportHandler — GET /api/v1/topology/export?format=mermaid|markdown&window=5m
// Renders the current architecture map as text for docs and diagrams.
func NewExportHandler(m *Manager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		format := r.URL.Query().Get("format")
		if format == "" {
			format = "mermaid"
		}
		if format != "mermaid" && format != "markdown" {
			http.Error(w, `{"error":"invalid format (want mermaid or markdown)"}`, http.StatusBadRequest)
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
		if format == "markdown" {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			out = Markdown(topo)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			out = Mermaid(topo)
		}
		_, _ = w.Write([]byte(out))
	})
}
