package pipeline

// A small self-contained grok engine: %{PATTERN} / %{PATTERN:field}
// references expand recursively into an RE2 regexp with named capture
// groups. The base pattern set is the commonly used subset of the classic
// grok-patterns library — enough for access logs, syslog-ish lines and
// generic key extraction, without an external dependency.

import (
	"fmt"
	"regexp"
	"strings"
)

var grokPatterns = map[string]string{
	"WORD":       `\b\w+\b`,
	"NOTSPACE":   `\S+`,
	"SPACE":      `\s*`,
	"DATA":       `.*?`,
	"GREEDYDATA": `.*`,

	"INT":    `[+-]?\d+`,
	"POSINT": `[1-9]\d*`,
	"NUMBER": `[+-]?\d+(?:\.\d+)?`,

	"IPV4":     `(?:\d{1,3}\.){3}\d{1,3}`,
	"IPV6":     `[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}(?:%\w+)?`,
	"IP":       `(?:%{IPV4}|%{IPV6})`,
	"HOSTNAME": `\b[0-9A-Za-z][0-9A-Za-z-]{0,62}(?:\.[0-9A-Za-z][0-9A-Za-z-]{0,62})*\.?\b`,
	"IPORHOST": `(?:%{IP}|%{HOSTNAME})`,
	"HOSTPORT": `%{IPORHOST}:%{POSINT}`,

	"USER":     `[a-zA-Z0-9._-]+`,
	"USERNAME": `[a-zA-Z0-9._-]+`,
	"UUID":     `[A-Fa-f0-9]{8}-(?:[A-Fa-f0-9]{4}-){3}[A-Fa-f0-9]{12}`,

	"QS":           `"(?:[^"\\]|\\.)*"`,
	"QUOTEDSTRING": `"(?:[^"\\]|\\.)*"`,

	"TIMESTAMP_ISO8601": `\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?`,
	"HTTPDATE":          `\d{2}/\w{3}/\d{4}:\d{2}:\d{2}:\d{2}\s+[+-]\d{4}`,
	"SYSLOGTIMESTAMP":   `\w{3} +\d{1,2} \d{2}:\d{2}:\d{2}`,
	"LOGLEVEL":          `(?i:trace|debug|notice|info|warn(?:ing)?|err(?:or)?|crit(?:ical)?|severe|alert|fatal|emerg(?:ency)?|panic)`,

	"URIPATH":      `(?:/[A-Za-z0-9$.+!*'(){},~:;=@#%&_\-]*)+`,
	"URIPARAM":     `\?[A-Za-z0-9$.+!*'|(){},~@#%&/=:;_?\-\[\]<>]*`,
	"URIPATHPARAM": `%{URIPATH}(?:%{URIPARAM})?`,

	"COMMONAPACHELOG":   `%{IPORHOST:clientip} %{USER:ident} %{USER:auth} \[%{HTTPDATE:timestamp}\] "(?:%{WORD:verb} %{NOTSPACE:request}(?: HTTP/%{NUMBER:httpversion})?|%{DATA:rawrequest})" %{NUMBER:response} (?:%{NUMBER:bytes}|-)`,
	"COMBINEDAPACHELOG": `%{COMMONAPACHELOG} %{QS:referrer} %{QS:agent}`,
}

var grokRef = regexp.MustCompile(`%\{(\w+)(?::(\w+))?\}`)

// expandGrok resolves %{NAME} and %{NAME:field} references into plain RE2.
func expandGrok(pattern string, depth int) (string, error) {
	if depth > 10 {
		return "", fmt.Errorf("grok patterns nested too deep")
	}
	var b strings.Builder
	last := 0
	for _, m := range grokRef.FindAllStringSubmatchIndex(pattern, -1) {
		b.WriteString(pattern[last:m[0]])
		name := pattern[m[2]:m[3]]
		def, ok := grokPatterns[name]
		if !ok {
			return "", fmt.Errorf("unknown grok pattern %%{%s}", name)
		}
		inner, err := expandGrok(def, depth+1)
		if err != nil {
			return "", err
		}
		if m[4] >= 0 { // %{NAME:field} — a named capture
			b.WriteString("(?P<" + pattern[m[4]:m[5]] + ">" + inner + ")")
		} else {
			b.WriteString("(?:" + inner + ")")
		}
		last = m[1]
	}
	b.WriteString(pattern[last:])
	return b.String(), nil
}

// compileGrok turns a grok expression into a compiled regexp.
func compileGrok(pattern string) (*regexp.Regexp, error) {
	expanded, err := expandGrok(pattern, 0)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(expanded)
	if err != nil {
		return nil, fmt.Errorf("grok %q: %w", pattern, err)
	}
	return re, nil
}
