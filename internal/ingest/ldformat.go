package ingest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Parser for the v1 ld_format protocol (see sdk/ld_format.md + the real appenders).
//
// Event: [0x06 0x03] pairs... '\n'
// Simple (text) pair:   KEY '=' VALUE '\n'
// Complex (binary) pair: KEY '\n' be-uint32(len) VALUE ['\n' — optional:
//                       the spec requires it, real appenders (Go/Java) don't write it]
//
// Event boundaries: '\n' at a pair-start position, the 0x06 0x03 header
// at a pair-start position, or EOF.

var ldHeader = [2]byte{6, 3}

const (
	maxKeyLen   = 256
	maxValueLen = 16 << 20 // 16 MiB per value — guard against garbage lengths
)

// ldEvent — raw parsed event: key → value.
type ldEvent map[string]string

// ParseLDStream reads events from the stream until EOF, calling emit for each.
// Events without msg are ignored per the spec (but still parsed to keep boundaries).
func ParseLDStream(r *bufio.Reader, emit func(ldEvent)) error {
	for {
		ev, err := parseLDEvent(r)
		if ev != nil {
			emit(ev)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// parseLDEvent reads a single event. Returns the event (may be nil if a
// boundary was hit before the first pair) and an error (io.EOF — end of stream).
func parseLDEvent(r *bufio.Reader) (ldEvent, error) {
	var ev ldEvent

	skipHeader(r)

	for {
		// Pair-start position: check for event boundaries.
		b, err := r.Peek(1)
		if err != nil {
			return ev, io.EOF
		}
		if b[0] == '\n' { // empty line = end of event
			_, _ = r.Discard(1)
			return ev, nil
		}
		if b[0] == ldHeader[0] { // possibly the start of the next event
			if h, err := r.Peek(2); err == nil && h[1] == ldHeader[1] {
				return ev, nil // don't consume the header — the next call handles it
			}
		}

		key, delim, err := readKey(r)
		if err != nil {
			return ev, err
		}

		var value string
		if delim == '=' {
			raw, err := r.ReadString('\n')
			if err != nil {
				return ev, fmt.Errorf("unterminated simple pair %q: %w", key, err)
			}
			value = strings.TrimSuffix(raw, "\n")
		} else { // complex (binary) pair
			var lenBuf [4]byte
			if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
				return ev, fmt.Errorf("length of complex pair %q: %w", key, err)
			}
			n := binary.BigEndian.Uint32(lenBuf[:])
			if n > maxValueLen {
				return ev, fmt.Errorf("complex pair %q: length %d exceeds the limit", key, n)
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(r, buf); err != nil {
				return ev, fmt.Errorf("value of complex pair %q: %w", key, err)
			}
			value = string(buf)
			// Optional trailing '\n' (per the spec) — consume it, but note that the
			// appenders don't write the trailer, and for them a '\n' after the value
			// means end of event. The two cases are indistinguishable, so we consume
			// the '\n' and rely on the header/EOF boundary.
			if b, err := r.Peek(1); err == nil && b[0] == '\n' {
				_, _ = r.Discard(1)
			}
		}

		if ev == nil {
			ev = make(ldEvent, 8)
		}
		ev[key] = value
	}
}

func skipHeader(r *bufio.Reader) {
	if h, err := r.Peek(2); err == nil && h[0] == ldHeader[0] && h[1] == ldHeader[1] {
		_, _ = r.Discard(2)
	}
}

// readKey reads a key up to '=' (simple pair) or '\n' (complex pair).
// Validation per the spec: non-empty, ASCII, no control characters.
func readKey(r *bufio.Reader) (string, byte, error) {
	var sb strings.Builder
	for {
		c, err := r.ReadByte()
		if err != nil {
			return "", 0, fmt.Errorf("unterminated key %q: %w", sb.String(), err)
		}
		if c == '=' || c == '\n' {
			if sb.Len() == 0 {
				return "", 0, errors.New("empty key")
			}
			return sb.String(), c, nil
		}
		if c < 0x20 || c > 0x7e {
			return "", 0, fmt.Errorf("invalid byte 0x%02x in key %q", c, sb.String())
		}
		if sb.Len() >= maxKeyLen {
			return "", 0, errors.New("key exceeds the length limit")
		}
		sb.WriteByte(c)
	}
}

// tsrc layouts: the canonical one from the spec/Java, and the buggy one from
// logdoc-go-appender (yy dd MM order + a dot before the milliseconds).
var tsrcLayouts = []string{
	"060102150405000",  // yyMMddHHmmssSSS — the spec and the Java appender
	"060201150405.000", // logdoc-go-appender
}

// EntryFromLD builds a model.Entry from a raw event.
// Returns false if there is no event or the mandatory msg is missing.
func EntryFromLD(ev ldEvent, remoteIP string, now time.Time) (model.Entry, bool) {
	msg, ok := ev["msg"]
	if !ok || msg == "" {
		return model.Entry{}, false
	}

	e := model.Entry{
		TenantID: model.DefaultTenant,
		Ts:       now,
		Msg:      msg,
		App:      ev["app"],
		Src:      strings.TrimSpace(ev["src"]),
		PID:      strings.TrimSpace(ev["pid"]),
		Lvl:      parseLDLevel(ev["lvl"]),
	}

	if raw, ok := ev["tsrc"]; ok {
		if ts, ok := parseTsrc(strings.TrimSpace(raw)); ok {
			e.Ts = ts
		}
	}

	for k, v := range ev {
		switch k {
		case "msg", "app", "src", "pid", "lvl", "tsrc", "trcv", "ip":
			// reserved keys: client-sent trcv/ip are overwritten by the server (spec)
		default:
			if e.Fields == nil {
				e.Fields = make(map[string]string)
			}
			e.Fields[k] = v
		}
	}
	if remoteIP != "" {
		if e.Fields == nil {
			e.Fields = make(map[string]string)
		}
		e.Fields["ip"] = remoteIP
	}
	return e, true
}

func parseTsrc(s string) (time.Time, bool) {
	for _, layout := range tsrcLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseLDLevel accepts a digit byte 0–6 (spec) or a level name:
// Java sends DEBUG/INFO/LOG/WARN/ERROR, logrus — lowercase + warning/fatal/trace.
func parseLDLevel(s string) model.Level {
	s = strings.TrimSpace(s)
	if s == "" {
		return model.LevelInfo
	}
	if len(s) == 1 && s[0] >= '0' && s[0] <= '6' {
		return model.Level(s[0] - '0')
	}
	switch strings.ToUpper(s) {
	case "WARNING":
		return model.LevelWarn
	case "FATAL":
		return model.LevelSevere
	case "TRACE":
		return model.LevelDebug
	}
	return model.ParseLevel(s)
}
