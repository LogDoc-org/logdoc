package graph

import (
	"encoding/json"
	"net/http"
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
