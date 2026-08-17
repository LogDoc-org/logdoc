package query

import (
	"net/url"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

func TestParsePlanFull(t *testing.T) {
	v := url.Values{}
	v.Add("app", "svc1")
	v.Add("app", "svc2")
	v.Set("lvl", "ERROR,3")
	v.Set("from", "2026-08-17T00:00:00Z")
	v.Set("to", "2026-08-18T00:00:00Z")
	v.Set("field.user", "u1")
	v.Set("q", "timeout")
	v.Set("limit", "500")

	p, err := ParsePlan(v)
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != model.DefaultTenant {
		t.Fatalf("tenant=%q", p.TenantID)
	}
	if len(p.Apps) != 2 || p.Apps[0] != "svc1" {
		t.Fatalf("apps: %v", p.Apps)
	}
	if len(p.Levels) != 2 || p.Levels[0] != model.LevelError || p.Levels[1] != model.LevelWarn {
		t.Fatalf("levels: %v", p.Levels)
	}
	if p.From == nil || p.From.UTC() != time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("from: %v", p.From)
	}
	if p.FieldEq["user"] != "u1" || p.Search != "timeout" || p.Limit != 500 {
		t.Fatalf("plan: %+v", p)
	}
}

func TestParsePlanDefaultsAndCaps(t *testing.T) {
	p, err := ParsePlan(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != DefaultLimit || len(p.Apps) != 0 || p.From != nil {
		t.Fatalf("defaults: %+v", p)
	}

	p, err = ParsePlan(url.Values{"limit": {"999999"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != MaxLimit {
		t.Fatalf("limit=%d, expected the cap %d", p.Limit, MaxLimit)
	}
}

func TestParsePlanErrors(t *testing.T) {
	if _, err := ParsePlan(url.Values{"from": {"yesterday"}}); err == nil {
		t.Fatal("expected a from error")
	}
	if _, err := ParsePlan(url.Values{"limit": {"-5"}}); err == nil {
		t.Fatal("expected a limit error")
	}
}

func TestPlanMatches(t *testing.T) {
	e := model.Entry{
		TenantID: model.DefaultTenant,
		App:      "svc1",
		Lvl:      model.LevelError,
		Msg:      "Connection Timeout occurred",
		Fields:   map[string]string{"user": "u1"},
	}

	cases := []struct {
		name string
		p    Plan
		want bool
	}{
		{"empty plan", Plan{TenantID: model.DefaultTenant}, true},
		{"by app", Plan{TenantID: model.DefaultTenant, Apps: []string{"svc1"}}, true},
		{"other app", Plan{TenantID: model.DefaultTenant, Apps: []string{"other"}}, false},
		{"by level", Plan{TenantID: model.DefaultTenant, Levels: []model.Level{model.LevelError}}, true},
		{"wrong level", Plan{TenantID: model.DefaultTenant, Levels: []model.Level{model.LevelDebug}}, false},
		{"by field", Plan{TenantID: model.DefaultTenant, FieldEq: map[string]string{"user": "u1"}}, true},
		{"wrong field", Plan{TenantID: model.DefaultTenant, FieldEq: map[string]string{"user": "u2"}}, false},
		{"case-insensitive search", Plan{TenantID: model.DefaultTenant, Search: "timeout"}, true},
		{"search miss", Plan{TenantID: model.DefaultTenant, Search: "panic"}, false},
		{"other tenant", Plan{TenantID: "other"}, false},
	}
	for _, c := range cases {
		if got := c.p.Matches(e); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}
