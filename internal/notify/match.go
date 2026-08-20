package notify

// The composite rule language (P3): notify.rules[].match holds a condition
// tree over log entries. One node is a conjunction of everything set on it;
// `or` requires at least one child; nodes nest arbitrarily.
//
//	match:
//	  app: billing
//	  or:
//	    - lvl: ERROR
//	    - msg: { contains: pool exhausted }

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Match — a composite condition over a log entry. Everything set on one
// node must hold; `and` requires all children, `or` at least one.
type Match struct {
	And []Match `yaml:"and" json:"and,omitempty"`
	Or  []Match `yaml:"or" json:"or,omitempty"`

	App string `yaml:"app" json:"app,omitempty"` // exact application name
	Src string `yaml:"src" json:"src,omitempty"` // exact source
	Pid string `yaml:"pid" json:"pid,omitempty"` // exact pid
	Lvl string `yaml:"lvl" json:"lvl,omitempty"` // minimum level: ERROR = ERROR and above

	Msg    *Cond           `yaml:"msg" json:"msg,omitempty"`       // condition on the message text
	Regex  string          `yaml:"regex" json:"regex,omitempty"`   // RE2 regexp on the message
	Fields map[string]Cond `yaml:"fields" json:"fields,omitempty"` // conditions on structured fields
}

// Cond — one string comparison; exactly one operator must be set.
// Case-insensitive unless case_sensitive is true.
type Cond struct {
	Contains      string `yaml:"contains" json:"contains,omitempty"`
	Starts        string `yaml:"starts" json:"starts,omitempty"`
	Ends          string `yaml:"ends" json:"ends,omitempty"`
	Equals        string `yaml:"equals" json:"equals,omitempty"`
	CaseSensitive bool   `yaml:"case_sensitive" json:"case_sensitive,omitempty"`
}

// matchFunc — a compiled condition evaluated on the hot path.
type matchFunc func(model.Entry) bool

// compile turns a Cond into a string predicate, or fails on a malformed one.
func (c Cond) compile(where string) (func(string) bool, error) {
	type op struct {
		name, val string
		test      func(s, v string) bool
	}
	var ops []op
	if c.Contains != "" {
		ops = append(ops, op{"contains", c.Contains, strings.Contains})
	}
	if c.Starts != "" {
		ops = append(ops, op{"starts", c.Starts, strings.HasPrefix})
	}
	if c.Ends != "" {
		ops = append(ops, op{"ends", c.Ends, strings.HasSuffix})
	}
	if c.Equals != "" {
		ops = append(ops, op{"equals", c.Equals, func(s, v string) bool { return s == v }})
	}
	if len(ops) != 1 {
		return nil, fmt.Errorf("%s: want exactly one of contains/starts/ends/equals", where)
	}
	o := ops[0]
	if c.CaseSensitive {
		return func(s string) bool { return o.test(s, o.val) }, nil
	}
	val := strings.ToLower(o.val)
	return func(s string) bool { return o.test(strings.ToLower(s), val) }, nil
}

// parseLevelStrict — ParseLevel that rejects unknown names instead of
// silently mapping them to INFO.
func parseLevelStrict(s string) (model.Level, error) {
	l := model.ParseLevel(s)
	if !strings.EqualFold(l.String(), s) {
		return 0, fmt.Errorf("unknown level %q", s)
	}
	return l, nil
}

// compileMatch compiles the tree; a nil match compiles to nil (caller keeps
// the rule's default behavior).
func compileMatch(m *Match, where string) (matchFunc, error) {
	if m == nil {
		return nil, nil
	}
	var preds []matchFunc

	if m.App != "" {
		app := m.App
		preds = append(preds, func(e model.Entry) bool { return e.App == app })
	}
	if m.Src != "" {
		src := m.Src
		preds = append(preds, func(e model.Entry) bool { return e.Src == src })
	}
	if m.Pid != "" {
		pid := m.Pid
		preds = append(preds, func(e model.Entry) bool { return e.PID == pid })
	}
	if m.Lvl != "" {
		min, err := parseLevelStrict(m.Lvl)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		preds = append(preds, func(e model.Entry) bool { return e.Lvl >= min })
	}
	if m.Msg != nil {
		f, err := m.Msg.compile(where + ".msg")
		if err != nil {
			return nil, err
		}
		preds = append(preds, func(e model.Entry) bool { return f(e.Msg) })
	}
	if m.Regex != "" {
		re, err := regexp.Compile(m.Regex)
		if err != nil {
			return nil, fmt.Errorf("%s.regex: %w", where, err)
		}
		preds = append(preds, func(e model.Entry) bool { return re.MatchString(e.Msg) })
	}
	for name, cond := range m.Fields {
		f, err := cond.compile(fmt.Sprintf("%s.fields.%s", where, name))
		if err != nil {
			return nil, err
		}
		key := name
		preds = append(preds, func(e model.Entry) bool {
			v, ok := e.Fields[key]
			return ok && f(v)
		})
	}

	if len(m.And) > 0 {
		var kids []matchFunc
		for i := range m.And {
			k, err := compileMatch(&m.And[i], fmt.Sprintf("%s.and[%d]", where, i))
			if err != nil {
				return nil, err
			}
			kids = append(kids, k)
		}
		preds = append(preds, func(e model.Entry) bool {
			for _, k := range kids {
				if !k(e) {
					return false
				}
			}
			return true
		})
	}
	if len(m.Or) > 0 {
		var kids []matchFunc
		for i := range m.Or {
			k, err := compileMatch(&m.Or[i], fmt.Sprintf("%s.or[%d]", where, i))
			if err != nil {
				return nil, err
			}
			kids = append(kids, k)
		}
		preds = append(preds, func(e model.Entry) bool {
			for _, k := range kids {
				if k(e) {
					return true
				}
			}
			return false
		})
	}

	if len(preds) == 0 {
		return nil, fmt.Errorf("%s: empty condition", where)
	}
	return func(e model.Entry) bool {
		for _, p := range preds {
			if !p(e) {
				return false
			}
		}
		return true
	}, nil
}
