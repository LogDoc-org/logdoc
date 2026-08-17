// Package model — the LogDoc v2 domain model.
package model

import "time"

// Level — entry level; values are compatible with v1 ld_format (0–6).
type Level uint8

const (
	LevelDebug Level = iota
	LevelInfo
	LevelLog
	LevelWarn
	LevelError
	LevelSevere
	LevelPanic
)

var levelNames = [...]string{"DEBUG", "INFO", "LOG", "WARN", "ERROR", "SEVERE", "PANIC"}

func (l Level) String() string {
	if int(l) < len(levelNames) {
		return levelNames[l]
	}
	return "INFO"
}

// MarshalJSON serializes the level as a name ("ERROR"), not a number:
// otherwise a []Level turns into base64 in JSON (a uint8 slice).
func (l Level) MarshalJSON() ([]byte, error) {
	return []byte(`"` + l.String() + `"`), nil
}

// ParseLevel accepts a level name (case-insensitive). Unknown name → INFO.
func ParseLevel(s string) Level {
	for i, name := range levelNames {
		if equalFold(s, name) {
			return Level(i)
		}
	}
	return LevelInfo
}

// equalFold — an ASCII-only variant of strings.EqualFold without allocations.
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

// DefaultTenant — the only tenant of the S1 single-user mode.
// tenant_id is present in the schema from day one (a v2 invariant).
const DefaultTenant = "default"

// Entry — a LogDoc v2 log entry.
type Entry struct {
	TenantID string            // tenant_id, always DefaultTenant in S1
	Ts       time.Time         // event time (source tsrc or receive time)
	App      string            // application
	Src      string            // source within the application
	Lvl      Level             // level 0–6
	PID      string            // pid of the source process
	Msg      string            // entry text (mandatory)
	Fields   map[string]string // arbitrary structured fields
}
