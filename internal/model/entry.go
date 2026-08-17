// Package model — доменная модель LogDoc v2.
package model

import "time"

// Level — уровень записи, значения совместимы с v1 ld_format (0–6).
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

// MarshalJSON сериализует уровень именем ("ERROR"), а не числом:
// иначе []Level в JSON превращается в base64 (срез uint8).
func (l Level) MarshalJSON() ([]byte, error) {
	return []byte(`"` + l.String() + `"`), nil
}

// ParseLevel принимает имя уровня (без учёта регистра). Неизвестное имя → INFO.
func ParseLevel(s string) Level {
	for i, name := range levelNames {
		if equalFold(s, name) {
			return Level(i)
		}
	}
	return LevelInfo
}

// equalFold — ASCII-вариант strings.EqualFold без аллокаций.
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

// DefaultTenant — единственный тенант однопользовательского режима S1.
// tenant_id присутствует в схеме с первого дня (инвариант v2).
const DefaultTenant = "default"

// Entry — запись лога LogDoc v2.
type Entry struct {
	TenantID string            // tenant_id, в S1 всегда DefaultTenant
	Ts       time.Time         // время события (tsrc источника или время приёма)
	App      string            // приложение
	Src      string            // источник внутри приложения
	Lvl      Level             // уровень 0–6
	PID      string            // pid процесса-источника
	Msg      string            // текст записи (обязателен)
	Fields   map[string]string // произвольные структурные поля
}
