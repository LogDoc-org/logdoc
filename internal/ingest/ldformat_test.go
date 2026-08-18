package ingest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// --- fixture builders mirroring the real appenders ---

func simplePair(key, value string) []byte {
	return []byte(key + "=" + value + "\n")
}

// complexPair — the way the Go/Java appenders write it: no trailing '\n'.
func complexPair(key, value string) []byte {
	var b bytes.Buffer
	b.WriteString(key)
	b.WriteByte('\n')
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(value)))
	b.Write(lenBuf[:])
	b.WriteString(value)
	return b.Bytes()
}

// complexPairSpec — as in the spec: with a trailing '\n'.
func complexPairSpec(key, value string) []byte {
	return append(complexPair(key, value), '\n')
}

func event(pairs ...[]byte) []byte {
	out := []byte{6, 3}
	for _, p := range pairs {
		out = append(out, p...)
	}
	return append(out, '\n')
}

func parseAll(t *testing.T, data []byte) []ldEvent {
	t.Helper()
	var evs []ldEvent
	err := ParseLDStream(bufio.NewReader(bytes.NewReader(data)), func(ev ldEvent) {
		evs = append(evs, ev)
	})
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	return evs
}

// --- parser tests ---

func TestParseSimpleEvent(t *testing.T) {
	data := event(
		simplePair("msg", "hello"),
		simplePair("app", "svc"),
		simplePair("lvl", "ERROR"),
		simplePair("src", "main.go:42"),
		simplePair("pid", "1234"),
	)
	evs := parseAll(t, data)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if evs[0]["msg"] != "hello" || evs[0]["app"] != "svc" || evs[0]["lvl"] != "ERROR" {
		t.Fatalf("wrong pairs: %v", evs[0])
	}
}

func TestParseComplexPairAppenderDialect(t *testing.T) {
	// msg with a stack trace (contains a newline) → complex pair without a
	// trailing '\n', as written by logdoc-go-appender and logback-appenders.
	msg := "boom\nat main.main(main.go:10)"
	data := event(
		complexPair("msg", msg),
		// after a trailer-less complex pair the end-of-event '\n' follows
		// immediately — the parser must recover the boundary from the header
		// of the next event
	)
	data = append(data, event(simplePair("msg", "next"))...)

	evs := parseAll(t, data)
	if len(evs) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(evs), evs)
	}
	if evs[0]["msg"] != msg {
		t.Fatalf("msg=%q", evs[0]["msg"])
	}
	if evs[1]["msg"] != "next" {
		t.Fatalf("second event: %v", evs[1])
	}
}

func TestParseComplexPairSpecDialect(t *testing.T) {
	// Spec dialect: a complex pair with a trailing '\n', followed by more pairs.
	data := []byte{6, 3}
	data = append(data, complexPairSpec("msg", "a\nb")...)
	data = append(data, simplePair("app", "svc")...)
	data = append(data, '\n')

	evs := parseAll(t, data)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d: %v", len(evs), evs)
	}
	if evs[0]["msg"] != "a\nb" || evs[0]["app"] != "svc" {
		t.Fatalf("pairs: %v", evs[0])
	}
}

func TestParseBinaryValueWithNULs(t *testing.T) {
	val := "bin\x00\x01\x02data"
	data := event(complexPair("msg", val), simplePair("app", "svc"))
	evs := parseAll(t, data)
	if len(evs) != 1 || evs[0]["msg"] != val {
		t.Fatalf("binary value lost: %v", evs)
	}
}

func TestParseMultipleEventsStream(t *testing.T) {
	var data []byte
	for i := 0; i < 3; i++ {
		data = append(data, event(simplePair("msg", "m"), simplePair("pid", string(rune('0'+i))))...)
	}
	evs := parseAll(t, data)
	if len(evs) != 3 {
		t.Fatalf("expected 3 events, got %d", len(evs))
	}
}

func TestParseGoAppenderRealisticEvent(t *testing.T) {
	// Exact imitation of logdoc-go-appender: header {6,3}, simple msg,
	// tsrc with a '\n' inside the value → complex pair, lowercase lvl.
	// msg is a deliberately non-ASCII (multi-byte UTF-8) payload:
	// keys must be ASCII, but values must pass through unmodified.
	msg := "héllo 世界 ✓"
	tsrcVal := time.Date(2026, 8, 17, 15, 4, 5, 123e6, time.Local).Format("060201150405.000") + "\n"
	var data []byte
	data = append(data, 6, 3)
	data = append(data, simplePair("msg", msg)...)
	data = append(data, simplePair("app", "demo")...)
	data = append(data, complexPair("tsrc", tsrcVal)...) // '\n' inside the value
	data = append(data, simplePair("lvl", "warn")...)
	data = append(data, simplePair("ip", "10.0.0.5:9999")...)
	data = append(data, simplePair("pid", "77")...)
	data = append(data, simplePair("src", "main.go:10")...)
	data = append(data, '\n')

	evs := parseAll(t, data)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d: %v", len(evs), evs)
	}
	e, ok := EntryFromLD(evs[0], "192.168.1.1", time.Now())
	if !ok {
		t.Fatal("event was dropped")
	}
	if e.Msg != msg {
		t.Fatalf("non-ASCII msg corrupted: %q", e.Msg)
	}
	if e.Lvl != model.LevelWarn || e.App != "demo" || e.PID != "77" {
		t.Fatalf("mapping: %+v", e)
	}
	if e.Ts.Year() != 2026 || e.Ts.Month() != 8 || e.Ts.Day() != 17 {
		t.Fatalf("tsrc not parsed: %v", e.Ts)
	}
	// the client-sent ip is overwritten by the server-side one
	if e.Fields["ip"] != "192.168.1.1" {
		t.Fatalf("ip=%q", e.Fields["ip"])
	}
}

func TestEntryFromLDMissingMsgIgnored(t *testing.T) {
	if _, ok := EntryFromLD(ldEvent{"app": "svc"}, "", time.Now()); ok {
		t.Fatal("event without msg must be ignored")
	}
	if _, ok := EntryFromLD(nil, "", time.Now()); ok {
		t.Fatal("nil event must be ignored")
	}
}

func TestEntryFromLDJavaTsrcAndLevels(t *testing.T) {
	// Java appender: tsrc = yyMMddHHmmssSSS, lvl = level name, TRACE → LOG on the client side.
	now := time.Now()
	e, ok := EntryFromLD(ldEvent{
		"msg":    "java msg",
		"tsrc":   "260817150405123",
		"lvl":    "SEVERE",
		"pid":    "12345@host",
		"custom": "value",
	}, "", now)
	if !ok {
		t.Fatal("event was dropped")
	}
	// Compare the full timestamp: a fallback to now must not pass by accident.
	want := time.Date(2026, 8, 17, 15, 4, 5, 123_000_000, time.Local)
	if !e.Ts.Equal(want) {
		t.Fatalf("java tsrc: got %v, want %v", e.Ts, want)
	}
	if e.Lvl != model.LevelSevere {
		t.Fatalf("lvl=%v", e.Lvl)
	}
	if e.Fields["custom"] != "value" {
		t.Fatalf("fields: %v", e.Fields)
	}
	if _, ok := e.Fields["tsrc"]; ok {
		t.Fatal("reserved key tsrc must not end up in fields")
	}
}

func TestParseLDLevelVariants(t *testing.T) {
	cases := map[string]model.Level{
		"0": model.LevelDebug, "6": model.LevelPanic, "3": model.LevelWarn,
		"DEBUG": model.LevelDebug, "warn": model.LevelWarn, "warning": model.LevelWarn,
		"error": model.LevelError, "fatal": model.LevelSevere, "trace": model.LevelDebug,
		"": model.LevelInfo, "garbage": model.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLDLevel(in); got != want {
			t.Errorf("parseLDLevel(%q)=%v, expected %v", in, got, want)
		}
	}
}

func TestParseRejectsBadKey(t *testing.T) {
	data := append([]byte{6, 3}, []byte("bad\x01key=v\n\n")...)
	err := ParseLDStream(bufio.NewReader(bytes.NewReader(data)), func(ldEvent) {})
	if err == nil || !strings.Contains(err.Error(), "invalid byte") {
		t.Fatalf("expected a key error, got %v", err)
	}
}

func TestParseRejectsHugeBinaryLength(t *testing.T) {
	var data []byte
	data = append(data, 6, 3)
	data = append(data, "msg\n"...)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 1<<30)
	data = append(data, lenBuf[:]...)
	err := ParseLDStream(bufio.NewReader(bytes.NewReader(data)), func(ldEvent) {})
	if err == nil {
		t.Fatal("expected a length-limit error")
	}
}
