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
		t.Fatalf("план: %+v", p)
	}
}

func TestParsePlanDefaultsAndCaps(t *testing.T) {
	p, err := ParsePlan(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != DefaultLimit || len(p.Apps) != 0 || p.From != nil {
		t.Fatalf("дефолты: %+v", p)
	}

	p, err = ParsePlan(url.Values{"limit": {"999999"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Limit != MaxLimit {
		t.Fatalf("limit=%d, ожидался кап %d", p.Limit, MaxLimit)
	}
}

func TestParsePlanErrors(t *testing.T) {
	if _, err := ParsePlan(url.Values{"from": {"вчера"}}); err == nil {
		t.Fatal("ожидалась ошибка from")
	}
	if _, err := ParsePlan(url.Values{"limit": {"-5"}}); err == nil {
		t.Fatal("ожидалась ошибка limit")
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
		{"пустой план", Plan{TenantID: model.DefaultTenant}, true},
		{"по app", Plan{TenantID: model.DefaultTenant, Apps: []string{"svc1"}}, true},
		{"чужой app", Plan{TenantID: model.DefaultTenant, Apps: []string{"other"}}, false},
		{"по уровню", Plan{TenantID: model.DefaultTenant, Levels: []model.Level{model.LevelError}}, true},
		{"не тот уровень", Plan{TenantID: model.DefaultTenant, Levels: []model.Level{model.LevelDebug}}, false},
		{"по полю", Plan{TenantID: model.DefaultTenant, FieldEq: map[string]string{"user": "u1"}}, true},
		{"не то поле", Plan{TenantID: model.DefaultTenant, FieldEq: map[string]string{"user": "u2"}}, false},
		{"поиск без регистра", Plan{TenantID: model.DefaultTenant, Search: "timeout"}, true},
		{"поиск мимо", Plan{TenantID: model.DefaultTenant, Search: "panic"}, false},
		{"чужой тенант", Plan{TenantID: "other"}, false},
	}
	for _, c := range cases {
		if got := c.p.Matches(e); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}
