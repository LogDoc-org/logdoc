package pipeline

import (
	"testing"

	"github.com/LogDoc-org/logdoc/internal/model"
	"gopkg.in/yaml.v3"
)

// capture collects everything the engine forwards.
type capture struct{ out []model.Entry }

func (c *capture) Append(e model.Entry) { c.out = append(c.out, e) }

// run parses a yaml `pipelines:` value, feeds one entry, returns the result.
func run(t *testing.T, cfgYAML string, e model.Entry) (model.Entry, bool) {
	t.Helper()
	var cfg []Pipeline
	if err := yaml.Unmarshal([]byte(cfgYAML), &cfg); err != nil {
		t.Fatal(err)
	}
	sink := &capture{}
	eng, err := New(cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	eng.Append(e)
	if len(sink.out) == 0 {
		return model.Entry{}, false
	}
	return sink.out[0], true
}

const nginxLine = `203.0.113.7 - alice [19/Aug/2026:12:00:01 +0000] ` +
	`"GET /api/v1/orders?id=7 HTTP/1.1" 502 157 "-" "curl/8.4.0" upstream=billing`

func TestNginxAccessLog(t *testing.T) {
	cfg := `
- name: nginx access
  when: {app: nginx}
  steps:
    - grok: '%{COMBINEDAPACHELOG} upstream=%{WORD:upstream}'
    - severity:
        from: response
        rules:
          - {prefix: "5", lvl: ERROR}
          - {prefix: "4", lvl: WARN}
          - {lvl: INFO}
    - set:
        src: access
        fields: {peer.service: $upstream}
`
	got, ok := run(t, cfg, model.Entry{App: "nginx", Lvl: model.LevelInfo, Msg: nginxLine})
	if !ok {
		t.Fatal("entry dropped")
	}
	want := map[string]string{
		"clientip": "203.0.113.7", "auth": "alice", "verb": "GET",
		"request": "/api/v1/orders?id=7", "response": "502", "bytes": "157",
		"agent": `"curl/8.4.0"`, "upstream": "billing", "peer.service": "billing",
	}
	for k, v := range want {
		if got.Fields[k] != v {
			t.Fatalf("field %s = %q, want %q (all: %v)", k, got.Fields[k], v, got.Fields)
		}
	}
	if got.Lvl != model.LevelError {
		t.Fatalf("lvl = %v, want ERROR", got.Lvl)
	}
	if got.Src != "access" {
		t.Fatalf("src = %q", got.Src)
	}

	// A 2xx line maps to INFO via the catch-all rule.
	okLine := `203.0.113.7 - - [19/Aug/2026:12:00:01 +0000] "GET / HTTP/1.1" 200 5 "-" "curl" upstream=billing`
	got, _ = run(t, cfg, model.Entry{App: "nginx", Lvl: model.LevelWarn, Msg: okLine})
	if got.Lvl != model.LevelInfo || got.Fields["response"] != "200" {
		t.Fatalf("2xx: lvl=%v fields=%v", got.Lvl, got.Fields)
	}

	// The selector keeps other apps untouched.
	got, _ = run(t, cfg, model.Entry{App: "postgres", Msg: nginxLine})
	if len(got.Fields) != 0 || got.Src != "" {
		t.Fatalf("selector leak: %+v", got)
	}
}

func TestJSONStep(t *testing.T) {
	cfg := `
- steps: [{json: {}}]
`
	e := model.Entry{App: "svc", Lvl: model.LevelInfo,
		Msg: `{"message":"charge failed","level":"critical","order":{"id":42,"paid":false},"tags":["a","b"]}`}
	got, _ := run(t, cfg, e)
	if got.Msg != "charge failed" {
		t.Fatalf("msg: %q", got.Msg)
	}
	if got.Lvl != model.LevelSevere {
		t.Fatalf("lvl: %v", got.Lvl)
	}
	if got.Fields["order.id"] != "42" || got.Fields["order.paid"] != "false" {
		t.Fatalf("nested: %v", got.Fields)
	}
	if got.Fields["tags"] != `["a","b"]` {
		t.Fatalf("array: %v", got.Fields)
	}

	// Non-JSON messages pass through unchanged.
	got, _ = run(t, cfg, model.Entry{App: "svc", Msg: "plain text"})
	if got.Msg != "plain text" || len(got.Fields) != 0 {
		t.Fatalf("plain: %+v", got)
	}
}

func TestRegexAndDrop(t *testing.T) {
	cfg := `
- name: extract latency
  steps:
    - regex: 'took (?P<ms>\d+)ms'
- name: drop health checks
  when: {msg_regex: 'GET /healthz'}
  steps:
    - drop: true
`
	got, ok := run(t, cfg, model.Entry{App: "api", Msg: "request took 250ms"})
	if !ok || got.Fields["ms"] != "250" {
		t.Fatalf("regex: %v %v", got.Fields, ok)
	}
	if _, ok := run(t, cfg, model.Entry{App: "api", Msg: `GET /healthz 200`}); ok {
		t.Fatal("health check not dropped")
	}
}

func TestSeverityFromLevelName(t *testing.T) {
	cfg := `
- steps:
    - regex: '^(?P<lvl_name>\w+):'
    - severity: {from: lvl_name}
`
	for msg, want := range map[string]model.Level{
		"WARNING: disk almost full": model.LevelWarn,
		"fatal: out of memory":      model.LevelPanic,
		"notice: rotated":           model.LevelLog,
	} {
		got, _ := run(t, cfg, model.Entry{App: "svc", Msg: msg})
		if got.Lvl != want {
			t.Fatalf("%q → %v, want %v", msg, got.Lvl, want)
		}
	}
}

func TestSetReferencesAndLiterals(t *testing.T) {
	cfg := `
- steps:
    - set:
        app: $vhost
        pid: "42"
        fields: {cost: $$5, region: eu}
`
	got, _ := run(t, cfg, model.Entry{App: "nginx", Msg: "x",
		Fields: map[string]string{"vhost": "shop"}})
	if got.App != "shop" || got.PID != "42" {
		t.Fatalf("set: %+v", got)
	}
	if got.Fields["cost"] != "$5" || got.Fields["region"] != "eu" {
		t.Fatalf("fields: %v", got.Fields)
	}
	// Missing reference leaves the attribute alone.
	got, _ = run(t, cfg, model.Entry{App: "nginx", Msg: "x"})
	if got.App != "nginx" {
		t.Fatalf("missing ref rewrote app: %q", got.App)
	}
}

func TestCallerFieldsNotMutated(t *testing.T) {
	cfg := `
- steps: [{set: {fields: {extra: "1"}}}]
`
	original := map[string]string{"keep": "me"}
	got, _ := run(t, cfg, model.Entry{App: "svc", Msg: "x", Fields: original})
	if got.Fields["extra"] != "1" || got.Fields["keep"] != "me" {
		t.Fatalf("fields: %v", got.Fields)
	}
	if _, leaked := original["extra"]; leaked {
		t.Fatal("caller's map mutated")
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []string{
		`[{name: x, steps: []}]`,                                   // no steps
		`[{steps: [{grok: '%{NOPE:x}'}]}]`,                         // unknown pattern
		`[{steps: [{regex: '['}]}]`,                                // bad regex
		`[{steps: [{severity: {rules: [{lvl: NOPE}]}}]}]`,          // bad level + no from
		`[{steps: [{severity: {from: s, rules: [{lvl: NOPE}]}}]}]`, // bad level
		`[{steps: [{set: {}}]}]`,                                   // empty set
		`[{steps: [{set: {lvl: NOPE}}]}]`,                          // bad literal level
		`[{when: {msg_regex: '['}, steps: [{drop: true}]}]`,        // bad selector
		`[{steps: [{drop: true, json: {}}]}]`,                      // two ops in one step
	}
	for _, y := range bad {
		var cfg []Pipeline
		if err := yaml.Unmarshal([]byte(y), &cfg); err != nil {
			t.Fatalf("yaml %s: %v", y, err)
		}
		if _, err := New(cfg, &capture{}); err == nil {
			t.Fatalf("config must be rejected: %s", y)
		}
	}
	// No pipelines = nil engine, no error.
	if eng, err := New(nil, &capture{}); eng != nil || err != nil {
		t.Fatalf("empty config: %v %v", eng, err)
	}
}

func TestGrokExpansion(t *testing.T) {
	re, err := compileGrok(`%{TIMESTAMP_ISO8601:ts} %{LOGLEVEL:level} %{GREEDYDATA:rest}`)
	if err != nil {
		t.Fatal(err)
	}
	m := re.FindStringSubmatch("2026-08-19T12:00:01Z Warning pool is low")
	if m == nil {
		t.Fatal("no match")
	}
	byName := map[string]string{}
	for i, n := range re.SubexpNames() {
		if n != "" {
			byName[n] = m[i]
		}
	}
	if byName["ts"] != "2026-08-19T12:00:01Z" || byName["level"] != "Warning" || byName["rest"] != "pool is low" {
		t.Fatalf("groups: %v", byName)
	}
}
