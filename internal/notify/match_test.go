package notify

import (
	"strings"
	"testing"

	"github.com/LogDoc-org/logdoc/internal/model"
	"gopkg.in/yaml.v3"
)

func compileYAML(t *testing.T, src string) matchFunc {
	t.Helper()
	var m Match
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	f, err := compileMatch(&m, "match")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return f
}

func TestMatch(t *testing.T) {
	entry := func(app string, lvl model.Level, msg string, fields map[string]string) model.Entry {
		return model.Entry{App: app, Src: "http", Lvl: lvl, PID: "42", Msg: msg, Fields: fields}
	}

	cases := []struct {
		name  string
		yaml  string
		entry model.Entry
		want  bool
	}{
		{"app exact hit", `app: billing`, entry("billing", model.LevelInfo, "x", nil), true},
		{"app exact miss", `app: billing`, entry("api", model.LevelInfo, "x", nil), false},
		{"lvl is a minimum", `lvl: ERROR`, entry("a", model.LevelPanic, "x", nil), true},
		{"lvl below", `lvl: ERROR`, entry("a", model.LevelWarn, "x", nil), false},
		{"src and pid", "src: http\npid: \"42\"", entry("a", model.LevelInfo, "x", nil), true},
		{"pid miss", `pid: "43"`, entry("a", model.LevelInfo, "x", nil), false},

		{"msg contains, case-insensitive by default",
			`msg: { contains: Pool Exhausted }`,
			entry("a", model.LevelInfo, "pgx: pool exhausted", nil), true},
		{"msg contains, case-sensitive miss",
			`msg: { contains: Pool, case_sensitive: true }`,
			entry("a", model.LevelInfo, "pool exhausted", nil), false},
		{"msg starts", `msg: { starts: "GET " }`, entry("a", model.LevelInfo, "GET /checkout", nil), true},
		{"msg ends", `msg: { ends: declined }`, entry("a", model.LevelInfo, "charge declined", nil), true},
		{"msg equals", `msg: { equals: ping }`, entry("a", model.LevelInfo, "PING", nil), true},

		{"regex on msg", `regex: "timeout after \\d+ms"`, entry("a", model.LevelInfo, "timeout after 250ms", nil), true},
		{"regex miss", `regex: "timeout after \\d+ms"`, entry("a", model.LevelInfo, "timeout", nil), false},

		{"field equals", `fields: { region: { equals: eu-1 } }`,
			entry("a", model.LevelInfo, "x", map[string]string{"region": "EU-1"}), true},
		{"field absent", `fields: { region: { equals: eu-1 } }`,
			entry("a", model.LevelInfo, "x", nil), false},

		{"conjunction on one node",
			"app: billing\nlvl: ERROR",
			entry("billing", model.LevelWarn, "x", nil), false},

		{"nested or: the parity example, lvl branch",
			"app: billing\nor:\n  - lvl: ERROR\n  - msg: { contains: pool exhausted }",
			entry("billing", model.LevelError, "boom", nil), true},
		{"nested or: msg branch",
			"app: billing\nor:\n  - lvl: ERROR\n  - msg: { contains: pool exhausted }",
			entry("billing", model.LevelInfo, "pgx: pool exhausted", nil), true},
		{"nested or: neither branch",
			"app: billing\nor:\n  - lvl: ERROR\n  - msg: { contains: pool exhausted }",
			entry("billing", model.LevelInfo, "ok", nil), false},
		{"nested or: wrong app",
			"app: billing\nor:\n  - lvl: ERROR\n  - msg: { contains: pool exhausted }",
			entry("api", model.LevelError, "boom", nil), false},

		{"and of two conditions",
			"and:\n  - msg: { contains: charge }\n  - fields: { region: { starts: eu } }",
			entry("a", model.LevelInfo, "charge declined", map[string]string{"region": "eu-1"}), true},
		{"and, one fails",
			"and:\n  - msg: { contains: charge }\n  - fields: { region: { starts: eu } }",
			entry("a", model.LevelInfo, "charge declined", map[string]string{"region": "us-1"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compileYAML(t, tc.yaml)(tc.entry); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchCompileErrors(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"empty node", `{}`, "empty condition"},
		{"two operators", `msg: { contains: a, equals: b }`, "exactly one"},
		{"no operator", `fields: { region: { case_sensitive: true } }`, "exactly one"},
		{"bad level", `lvl: FATAL`, "unknown level"},
		{"bad regex", `regex: "["`, "regex"},
		{"empty or child", "or:\n  - {}", "empty condition"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m Match
			if err := yaml.Unmarshal([]byte(tc.yaml), &m); err != nil {
				t.Fatalf("yaml: %v", err)
			}
			_, err := compileMatch(&m, "match")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
