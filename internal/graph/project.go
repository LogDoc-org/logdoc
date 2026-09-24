package graph

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Project is a portable topology bundle: the declared graph plus the
// catalog, with a title — everything needed to carry a mapped system
// between LogDoc instances (or keep several projects as files and switch).
type Project struct {
	Format    string        `json:"format"` // "logdoc-project/v1"
	Title     string        `json:"title,omitempty"`
	Generated string        `json:"generated,omitempty"`
	Declared  DeclaredGraph `json:"declared"`
	Catalog   []ServiceMeta `json:"catalog"`
}

const projectFormat = "logdoc-project/v1"

// NewProjectHandler — GET exports the current project as a downloadable
// JSON file; POST imports one, REPLACING the declared graph and the
// runtime catalog (config-defined catalog entries stay untouched).
//
//	GET  /api/v1/topology/project?title=WB+Ads
//	POST /api/v1/topology/project   (body: the exported JSON)
func NewProjectHandler(m *Manager, c *Catalog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			decl, err := m.Declared(r.Context(), model.DefaultTenant)
			if err != nil {
				http.Error(w, `{"error":"declared graph unavailable"}`, http.StatusInternalServerError)
				return
			}
			if decl.Nodes == nil {
				decl.Nodes = []DeclaredNode{}
			}
			if decl.Edges == nil {
				decl.Edges = []DeclaredEdge{}
			}
			metas, err := c.List(r.Context(), model.DefaultTenant)
			if err != nil || metas == nil {
				metas = []ServiceMeta{}
			}
			p := Project{
				Format:    projectFormat,
				Title:     strings.TrimSpace(r.URL.Query().Get("title")),
				Generated: time.Now().UTC().Format(time.RFC3339),
				Declared:  decl,
				Catalog:   metas,
			}
			name := "logdoc-project"
			if p.Title != "" {
				name = slugify(p.Title)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Disposition",
				fmt.Sprintf(`attachment; filename="%s-%s.json"`, name, time.Now().Format("2006-01-02")))
			enc := json.NewEncoder(w)
			enc.SetIndent("", " ")
			_ = enc.Encode(p)
			return
		}

		var p Project
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&p); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
		if p.Format != "" && p.Format != projectFormat {
			b, _ := json.Marshal(map[string]string{"error": "unsupported format: " + p.Format})
			http.Error(w, string(b), http.StatusBadRequest)
			return
		}
		// Replace the declared graph first — it is the project's backbone.
		if err := m.DeclareTopology(r.Context(), model.DefaultTenant, p.Declared); err != nil {
			b, _ := json.Marshal(map[string]string{"error": "declared: " + err.Error()})
			http.Error(w, string(b), http.StatusBadRequest)
			return
		}
		// Replace the runtime catalog: drop entries not present in the file,
		// upsert the rest. Config-defined entries are read-only and skipped.
		incoming := make(map[string]bool, len(p.Catalog))
		for _, meta := range p.Catalog {
			incoming[meta.App] = true
		}
		existing, _ := c.List(r.Context(), model.DefaultTenant)
		removed := 0
		for _, meta := range existing {
			if meta.Source == "config" || incoming[meta.App] {
				continue
			}
			if err := c.Delete(r.Context(), model.DefaultTenant, meta.App); err == nil {
				removed++
			}
		}
		saved, skipped := 0, 0
		for _, meta := range p.Catalog {
			meta.Source = ""
			if err := c.Put(r.Context(), model.DefaultTenant, meta); err != nil {
				skipped++
				continue
			}
			saved++
		}
		_, _ = fmt.Fprintf(w,
			`{"status":"ok","nodes":%d,"edges":%d,"catalog_saved":%d,"catalog_removed":%d,"catalog_skipped":%d}`,
			len(p.Declared.Nodes), len(p.Declared.Edges), saved, removed, skipped)
	})
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "logdoc-project"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}
