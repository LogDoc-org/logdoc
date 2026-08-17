// Package query — the logical query plan and the search HTTP API.
//
// v2 invariant: the plan knows nothing about SQL. The backend (ClickHouse now,
// Engine later) translates the plan into its own language itself. Even while
// the filters are primitive, the plan/executor split is mandatory.
package query

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

const (
	DefaultLimit = 100
	MaxLimit     = 10000
)

// Plan — the logical plan of a log query.
type Plan struct {
	TenantID string            `json:"tenant_id"`
	Apps     []string          `json:"apps,omitempty"`
	Levels   []model.Level     `json:"levels,omitempty"`
	From     *time.Time        `json:"from,omitempty"`
	To       *time.Time        `json:"to,omitempty"`
	FieldEq  map[string]string `json:"field_eq,omitempty"`
	Search   string            `json:"search,omitempty"` // case-insensitive substring in msg
	Limit    int               `json:"limit"`
}

// ParsePlan parses the query parameters of an HTTP request:
//
//	app=svc1&app=svc2      — filter by applications
//	lvl=ERROR,WARN         — levels (names or digits 0–6)
//	from=RFC3339&to=RFC3339 — time window
//	field.user=u1          — exact match on a structured field
//	q=timeout              — full-text over msg
//	limit=500
func ParsePlan(values url.Values) (Plan, error) {
	p := Plan{
		TenantID: model.DefaultTenant,
		Limit:    DefaultLimit,
	}

	p.Apps = append(p.Apps, values["app"]...)

	for _, raw := range values["lvl"] {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if len(part) == 1 && part[0] >= '0' && part[0] <= '6' {
				p.Levels = append(p.Levels, model.Level(part[0]-'0'))
			} else {
				p.Levels = append(p.Levels, model.ParseLevel(part))
			}
		}
	}

	var err error
	if p.From, err = parseTime(values.Get("from")); err != nil {
		return Plan{}, fmt.Errorf("parameter from: %w", err)
	}
	if p.To, err = parseTime(values.Get("to")); err != nil {
		return Plan{}, fmt.Errorf("parameter to: %w", err)
	}

	for key, vals := range values {
		if name, ok := strings.CutPrefix(key, "field."); ok && name != "" && len(vals) > 0 {
			if p.FieldEq == nil {
				p.FieldEq = make(map[string]string)
			}
			p.FieldEq[name] = vals[0]
		}
	}

	p.Search = values.Get("q")

	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return Plan{}, fmt.Errorf("parameter limit: %q", raw)
		}
		p.Limit = n
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}

	return p, nil
}

func parseTime(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Matches checks an entry against the plan's filters (excluding time and limit) —
// used by live tail for in-memory stream filtering.
func (p Plan) Matches(e model.Entry) bool {
	if p.TenantID != "" && e.TenantID != p.TenantID {
		return false
	}
	if len(p.Apps) > 0 && !contains(p.Apps, e.App) {
		return false
	}
	if len(p.Levels) > 0 {
		found := false
		for _, l := range p.Levels {
			if e.Lvl == l {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for k, v := range p.FieldEq {
		if e.Fields[k] != v {
			return false
		}
	}
	if p.Search != "" && !strings.Contains(strings.ToLower(e.Msg), strings.ToLower(p.Search)) {
		return false
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// JSON — plan serialization for workload telemetry.
func (p Plan) JSON() string {
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}
