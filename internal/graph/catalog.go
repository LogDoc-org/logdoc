package graph

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ServiceMeta — the declared half of a service's catalog entry: who owns it,
// what it is, where its repo/runbook/dashboard live. The observed half
// (edges, rates, deploys) comes from the graph itself.
type ServiceMeta struct {
	App         string            `json:"app" yaml:"app"`
	Owner       string            `json:"owner,omitempty" yaml:"owner"`
	Description string            `json:"description,omitempty" yaml:"description"`
	Links       map[string]string `json:"links,omitempty" yaml:"links"` // name → URL
	Tags        []string          `json:"tags,omitempty" yaml:"tags"`
	// Source — where the entry is defined: "config" (logdoc.yml, read-only
	// via the API) or "api" (created at runtime, persisted server-side).
	Source string `json:"source,omitempty" yaml:"-"`
}

// CatalogStore persists runtime-edited catalog entries.
type CatalogStore interface {
	UpsertMeta(ctx context.Context, tenantID string, m ServiceMeta) error
	DeleteMeta(ctx context.Context, tenantID, app string) error
	// CatalogMeta returns all stored entries of the tenant.
	CatalogMeta(ctx context.Context, tenantID string) ([]ServiceMeta, error)
}

// ErrConfigDefined — the entry comes from logdoc.yml and cannot be changed
// over the API; edit the config instead.
var ErrConfigDefined = errors.New("catalog entry is defined in the config file")

// Catalog merges the two sources of service metadata: entries declared in
// logdoc.yml (read-only, win on conflict) and entries edited at runtime
// (stored next to the graph state).
type Catalog struct {
	store  CatalogStore
	config map[string]ServiceMeta // keyed by app
}

func NewCatalog(store CatalogStore, config []ServiceMeta) (*Catalog, error) {
	c := &Catalog{store: store, config: make(map[string]ServiceMeta, len(config))}
	for _, m := range config {
		if err := ValidateMeta(m); err != nil {
			return nil, fmt.Errorf("catalog entry %q: %w", m.App, err)
		}
		if _, dup := c.config[m.App]; dup {
			return nil, fmt.Errorf("catalog entry %q: duplicate app", m.App)
		}
		m.Source = "config"
		c.config[m.App] = m
	}
	return c, nil
}

// List returns every catalog entry of the tenant, config entries first on
// conflict, sorted by app.
func (c *Catalog) List(ctx context.Context, tenantID string) ([]ServiceMeta, error) {
	stored, err := c.store.CatalogMeta(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceMeta, 0, len(stored)+len(c.config))
	for _, m := range stored {
		if _, shadowed := c.config[m.App]; shadowed {
			continue
		}
		m.Source = "api"
		out = append(out, m)
	}
	for _, m := range c.config {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out, nil
}

// Get returns the entry for one app, or a zero ServiceMeta (Source == "")
// when the app has no catalog entry.
func (c *Catalog) Get(ctx context.Context, tenantID, app string) (ServiceMeta, error) {
	if m, ok := c.config[app]; ok {
		return m, nil
	}
	stored, err := c.store.CatalogMeta(ctx, tenantID)
	if err != nil {
		return ServiceMeta{}, err
	}
	for _, m := range stored {
		if m.App == app {
			m.Source = "api"
			return m, nil
		}
	}
	return ServiceMeta{}, nil
}

// Put creates or replaces a runtime entry. Config-defined apps are read-only.
func (c *Catalog) Put(ctx context.Context, tenantID string, m ServiceMeta) error {
	if err := ValidateMeta(m); err != nil {
		return err
	}
	if _, ok := c.config[m.App]; ok {
		return ErrConfigDefined
	}
	m.Source = ""
	return c.store.UpsertMeta(ctx, tenantID, m)
}

// Delete removes a runtime entry. Config-defined apps are read-only.
func (c *Catalog) Delete(ctx context.Context, tenantID, app string) error {
	if _, ok := c.config[app]; ok {
		return ErrConfigDefined
	}
	return c.store.DeleteMeta(ctx, tenantID, app)
}

// ValidateMeta bounds every field: the catalog is metadata, not a document
// store, and entries end up in exports and MCP responses verbatim.
func ValidateMeta(m ServiceMeta) error {
	if strings.TrimSpace(m.App) == "" {
		return errors.New("app is required")
	}
	if len(m.App) > 200 {
		return errors.New("app is too long (max 200)")
	}
	if len(m.Owner) > 200 {
		return errors.New("owner is too long (max 200)")
	}
	if len(m.Description) > 2000 {
		return errors.New("description is too long (max 2000)")
	}
	if len(m.Links) > 20 {
		return errors.New("too many links (max 20)")
	}
	for name, raw := range m.Links {
		if strings.TrimSpace(name) == "" || len(name) > 100 {
			return fmt.Errorf("link name %q: must be 1..100 characters", name)
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("link %q: want an absolute http(s) URL", name)
		}
		if len(raw) > 1000 {
			return fmt.Errorf("link %q: URL is too long (max 1000)", name)
		}
	}
	if len(m.Tags) > 20 {
		return errors.New("too many tags (max 20)")
	}
	for _, t := range m.Tags {
		if strings.TrimSpace(t) == "" || len(t) > 63 {
			return fmt.Errorf("tag %q: must be 1..63 characters", t)
		}
	}
	return nil
}
