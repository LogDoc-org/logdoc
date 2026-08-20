package pipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/LogDoc-org/logdoc/internal/model"
)

func compileStep(s Step) (stepFunc, error) {
	set := 0
	for _, on := range []bool{s.Grok != nil, s.Regex != nil, s.JSON != nil,
		s.Severity != nil, s.Set != nil, s.Drop} {
		if on {
			set++
		}
	}
	if set != 1 {
		return nil, fmt.Errorf("a step needs exactly one of grok/regex/json/severity/set/drop")
	}
	switch {
	case s.Grok != nil:
		re, err := compileGrok(s.Grok.Pattern)
		if err != nil {
			return nil, err
		}
		return patternStep(re, s.Grok.From), nil
	case s.Regex != nil:
		re, err := regexp.Compile(s.Regex.Pattern)
		if err != nil {
			return nil, fmt.Errorf("regex: %w", err)
		}
		return patternStep(re, s.Regex.From), nil
	case s.JSON != nil:
		return jsonStep(s.JSON.From), nil
	case s.Severity != nil:
		return severityStep(s.Severity)
	case s.Set != nil:
		return setStep(s.Set)
	default:
		return func(*model.Entry) bool { return true }, nil // drop
	}
}

// input picks the step source: the message, or a named field.
func input(e *model.Entry, from string) string {
	if from == "" || from == "msg" {
		return e.Msg
	}
	return e.Fields[from]
}

// patternStep matches the input and stores non-empty named captures as
// fields. No match (or empty input) leaves the entry untouched.
func patternStep(re *regexp.Regexp, from string) stepFunc {
	names := re.SubexpNames()
	return func(e *model.Entry) bool {
		in := input(e, from)
		if in == "" {
			return false
		}
		m := re.FindStringSubmatch(in)
		if m == nil {
			return false
		}
		for i, name := range names {
			if name != "" && m[i] != "" {
				e.Fields[name] = m[i]
			}
		}
		return false
	}
}

const maxJSONFields = 100

// jsonStep parses a JSON object into fields. Nested objects flatten with
// dot keys (3 levels deep, then raw JSON); arrays stay raw JSON. A
// top-level "msg"/"message" replaces the entry message; a recognizable
// top-level "level"/"lvl"/"severity" sets the entry level.
func jsonStep(from string) stepFunc {
	return func(e *model.Entry) bool {
		in := strings.TrimSpace(input(e, from))
		if len(in) < 2 || in[0] != '{' {
			return false
		}
		dec := json.NewDecoder(strings.NewReader(in))
		dec.UseNumber()
		var obj map[string]any
		if dec.Decode(&obj) != nil {
			return false
		}
		for k, v := range obj {
			switch {
			case (k == "msg" || k == "message") && v != nil:
				if s, ok := v.(string); ok {
					e.Msg = s
					continue
				}
			case k == "level" || k == "lvl" || k == "severity":
				if s, ok := v.(string); ok {
					if lvl, ok := parseLevelLoose(s); ok {
						e.Lvl = lvl
						continue
					}
				}
			}
			flatten(k, v, e.Fields, 1)
		}
		return false
	}
}

func flatten(key string, v any, out map[string]string, depth int) {
	if len(out) >= maxJSONFields {
		return
	}
	switch x := v.(type) {
	case nil:
	case string:
		out[key] = x
	case bool:
		if x {
			out[key] = "true"
		} else {
			out[key] = "false"
		}
	case json.Number:
		out[key] = x.String()
	case map[string]any:
		if depth >= 3 {
			raw, _ := json.Marshal(x)
			out[key] = string(raw)
			return
		}
		for k, vv := range x {
			flatten(key+"."+k, vv, out, depth+1)
		}
	default: // arrays and anything exotic stay as raw JSON
		raw, _ := json.Marshal(x)
		out[key] = string(raw)
	}
}

func severityStep(s *SeverityStep) (stepFunc, error) {
	if s.From == "" {
		return nil, fmt.Errorf("severity: `from` is required (a field name or \"msg\")")
	}
	type rule struct {
		SeverityRule
		lvl model.Level
	}
	rules := make([]rule, 0, len(s.Rules))
	for i, r := range s.Rules {
		lvl, err := parseLevelStrict(r.Lvl)
		if err != nil {
			return nil, fmt.Errorf("severity rule %d: %w", i+1, err)
		}
		rules = append(rules, rule{r, lvl})
	}
	return func(e *model.Entry) bool {
		v := input(e, s.From)
		if v == "" {
			return false
		}
		if len(rules) == 0 { // no rules = parse the value as a level name
			if lvl, ok := parseLevelLoose(v); ok {
				e.Lvl = lvl
			}
			return false
		}
		for _, r := range rules {
			switch {
			case r.Equals != "" && v != r.Equals:
			case r.Prefix != "" && !strings.HasPrefix(v, r.Prefix):
			case r.Contains != "" && !strings.Contains(v, r.Contains):
			default:
				e.Lvl = r.lvl
				return false
			}
		}
		return false
	}, nil
}

func setStep(s *SetStep) (stepFunc, error) {
	// A literal (non-reference) level is validated at compile time.
	if s.Lvl != "" && !strings.HasPrefix(s.Lvl, "$") {
		if _, err := parseLevelStrict(s.Lvl); err != nil {
			return nil, fmt.Errorf("set: %w", err)
		}
	}
	if s.App == "" && s.Src == "" && s.Pid == "" && s.Msg == "" && s.Lvl == "" && len(s.Fields) == 0 {
		return nil, fmt.Errorf("set: nothing to set")
	}
	assign := func(dst *string, spec string, e *model.Entry) {
		if v, ok := resolve(spec, e); ok {
			*dst = v
		}
	}
	return func(e *model.Entry) bool {
		if s.App != "" {
			assign(&e.App, s.App, e)
		}
		if s.Src != "" {
			assign(&e.Src, s.Src, e)
		}
		if s.Pid != "" {
			assign(&e.PID, s.Pid, e)
		}
		if s.Msg != "" {
			assign(&e.Msg, s.Msg, e)
		}
		if s.Lvl != "" {
			if v, ok := resolve(s.Lvl, e); ok {
				if lvl, ok := parseLevelLoose(v); ok {
					e.Lvl = lvl
				}
			}
		}
		for k, spec := range s.Fields {
			if v, ok := resolve(spec, e); ok {
				e.Fields[k] = v
			}
		}
		return false
	}, nil
}

// resolve interprets a set value: "$name" reads a field (false when the
// field is empty/absent), "$$..." unescapes to a literal "$...", anything
// else is a literal.
func resolve(spec string, e *model.Entry) (string, bool) {
	if strings.HasPrefix(spec, "$$") {
		return spec[1:], true
	}
	if strings.HasPrefix(spec, "$") {
		v := e.Fields[spec[1:]]
		return v, v != ""
	}
	return spec, true
}
