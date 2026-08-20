// Package pipeline — server-side log processing: parse raw messages into
// structured fields (grok/regex/json), derive the level, rewrite entry
// attributes, drop noise. Pipelines run on the ingest path before every
// consumer (storage, tail, topology, notifications), so extracted fields
// feed the architecture map and alert rules too.
package pipeline

import (
	"fmt"
	"regexp"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Appender — the downstream receiver (structurally ingest.Appender).
type Appender interface {
	Append(e model.Entry)
}

// Pipeline — one processing chain from the `pipelines:` config section.
type Pipeline struct {
	Name  string `yaml:"name"`
	When  When   `yaml:"when"`  // selector; empty = every entry
	Steps []Step `yaml:"steps"` // executed in order
}

// When selects the entries a pipeline applies to. All set fields must match.
type When struct {
	App      string `yaml:"app"`       // exact application
	Src      string `yaml:"src"`       // exact source
	MsgRegex string `yaml:"msg_regex"` // RE2 over the message
}

// Step — exactly one of the fields must be set.
type Step struct {
	Grok     *PatternStep  `yaml:"grok"`     // %{...} patterns → fields
	Regex    *PatternStep  `yaml:"regex"`    // RE2 named groups → fields
	JSON     *JSONStep     `yaml:"json"`     // JSON message → fields
	Severity *SeverityStep `yaml:"severity"` // field value → entry level
	Set      *SetStep      `yaml:"set"`      // rewrite app/src/pid/msg/lvl/fields
	Drop     bool          `yaml:"drop"`     // discard the entry
}

// PatternStep extracts fields from a string via named captures. From names
// the input: a field, or the message when empty/"msg". No match = no-op.
type PatternStep struct {
	Pattern string `yaml:"pattern"`
	From    string `yaml:"from"`
}

// UnmarshalYAML accepts both the full form and a bare pattern string:
// `- grok: '%{IP:client} ...'`.
func (p *PatternStep) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		p.Pattern = s
		return nil
	}
	type raw PatternStep
	return unmarshal((*raw)(p))
}

// JSONStep parses a JSON object into fields (nested keys flattened with
// dots). Top-level "msg"/"message" becomes the entry message; a top-level
// "level"/"lvl"/"severity" with a recognizable name sets the entry level.
type JSONStep struct {
	From string `yaml:"from"` // field to parse; empty/"msg" = the message
}

// SeverityStep sets the entry level from a value. Without rules the value
// is parsed as a level name (info/warning/critical/...); with rules the
// first matching rule wins.
type SeverityStep struct {
	From  string         `yaml:"from"` // field name, or "msg"
	Rules []SeverityRule `yaml:"rules"`
}

// SeverityRule — one mapping; a rule with no condition always matches
// (the catch-all default).
type SeverityRule struct {
	Equals   string `yaml:"equals"`
	Prefix   string `yaml:"prefix"`
	Contains string `yaml:"contains"`
	Lvl      string `yaml:"lvl"`
}

// SetStep rewrites entry attributes. A "$name" value reads the field with
// that name (skipped when absent); "$$" escapes a literal dollar.
type SetStep struct {
	App    string            `yaml:"app"`
	Src    string            `yaml:"src"`
	Pid    string            `yaml:"pid"`
	Msg    string            `yaml:"msg"`
	Lvl    string            `yaml:"lvl"`
	Fields map[string]string `yaml:"fields"`
}

// --- compilation ---

// stepFunc applies one step; true = the entry is dropped.
type stepFunc func(e *model.Entry) bool

type compiled struct {
	name  string
	when  func(e *model.Entry) bool // nil = always
	steps []stepFunc
}

// Engine runs the compiled pipelines and forwards surviving entries.
type Engine struct {
	next      Appender
	pipelines []compiled
}

// New compiles the configuration. No pipelines = nil engine (use the
// downstream appender directly).
func New(cfg []Pipeline, next Appender) (*Engine, error) {
	if len(cfg) == 0 {
		return nil, nil
	}
	e := &Engine{next: next}
	for i, p := range cfg {
		name := p.Name
		if name == "" {
			name = fmt.Sprintf("pipeline #%d", i+1)
		}
		c, err := compile(name, p)
		if err != nil {
			return nil, err
		}
		e.pipelines = append(e.pipelines, c)
	}
	return e, nil
}

// Append runs every matching pipeline in order; a drop step swallows the
// entry, everything else flows to the downstream appender.
func (e *Engine) Append(entry model.Entry) {
	owned := false // fields map cloned (entries share maps with the caller)
	for i := range e.pipelines {
		p := &e.pipelines[i]
		if p.when != nil && !p.when(&entry) {
			continue
		}
		if !owned {
			entry.Fields = cloneFields(entry.Fields)
			owned = true
		}
		for _, step := range p.steps {
			if step(&entry) {
				return // dropped
			}
		}
	}
	e.next.Append(entry)
}

func cloneFields(src map[string]string) map[string]string {
	out := make(map[string]string, len(src)+4)
	for k, v := range src {
		out[k] = v
	}
	return out
}

func compile(name string, p Pipeline) (compiled, error) {
	c := compiled{name: name}
	if len(p.Steps) == 0 {
		return c, fmt.Errorf("pipeline %q: no steps", name)
	}
	var err error
	if c.when, err = compileWhen(p.When); err != nil {
		return c, fmt.Errorf("pipeline %q: %w", name, err)
	}
	for i, s := range p.Steps {
		fn, err := compileStep(s)
		if err != nil {
			return c, fmt.Errorf("pipeline %q, step %d: %w", name, i+1, err)
		}
		c.steps = append(c.steps, fn)
	}
	return c, nil
}

func compileWhen(w When) (func(e *model.Entry) bool, error) {
	var re *regexp.Regexp
	if w.MsgRegex != "" {
		var err error
		if re, err = regexp.Compile(w.MsgRegex); err != nil {
			return nil, fmt.Errorf("when.msg_regex: %w", err)
		}
	}
	if w.App == "" && w.Src == "" && re == nil {
		return nil, nil
	}
	return func(e *model.Entry) bool {
		if w.App != "" && e.App != w.App {
			return false
		}
		if w.Src != "" && e.Src != w.Src {
			return false
		}
		if re != nil && !re.MatchString(e.Msg) {
			return false
		}
		return true
	}, nil
}

// parseLevelStrict accepts only the seven known level names.
func parseLevelStrict(s string) (model.Level, error) {
	for i := model.LevelDebug; i <= model.LevelPanic; i++ {
		if equalFold(s, i.String()) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown level %q", s)
}

// parseLevelLoose maps common level spellings (syslog, python, java) onto
// the seven LogDoc levels. false = not recognizable.
func parseLevelLoose(s string) (model.Level, bool) {
	switch {
	case equalFoldAny(s, "trace", "debug", "fine", "finer", "finest"):
		return model.LevelDebug, true
	case equalFoldAny(s, "info", "informational"):
		return model.LevelInfo, true
	case equalFoldAny(s, "log", "notice"):
		return model.LevelLog, true
	case equalFoldAny(s, "warn", "warning"):
		return model.LevelWarn, true
	case equalFoldAny(s, "error", "err"):
		return model.LevelError, true
	case equalFoldAny(s, "severe", "critical", "crit", "alert"):
		return model.LevelSevere, true
	case equalFoldAny(s, "panic", "fatal", "emerg", "emergency"):
		return model.LevelPanic, true
	}
	return 0, false
}

func equalFoldAny(s string, names ...string) bool {
	for _, n := range names {
		if equalFold(s, n) {
			return true
		}
	}
	return false
}

// equalFold — ASCII-only case-insensitive compare without allocations.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'a' <= ca && ca <= 'z' {
			ca -= 'a' - 'A'
		}
		if 'a' <= cb && cb <= 'z' {
			cb -= 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
