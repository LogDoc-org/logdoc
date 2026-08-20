package ingest

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

func journalExport(fields [][2]string) []byte {
	var b []byte
	for _, kv := range fields {
		b = append(b, kv[0]...)
		b = append(b, '=')
		b = append(b, kv[1]...)
		b = append(b, '\n')
	}
	b = append(b, '\n')
	return b
}

func TestJournalDecoderTextFields(t *testing.T) {
	d := &journalDecoder{}
	data := journalExport([][2]string{
		{"MESSAGE", "unit started"},
		{"PRIORITY", "6"},
		{"SYSLOG_IDENTIFIER", "nginx"},
		{"_PID", "42"},
	})
	entries := d.feed(data)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0]["MESSAGE"] != "unit started" || entries[0]["SYSLOG_IDENTIFIER"] != "nginx" {
		t.Fatalf("fields = %v", entries[0])
	}
}

func TestJournalDecoderBinaryField(t *testing.T) {
	// MESSAGE as a binary field containing a newline.
	val := "line one\nline two"
	var data []byte
	data = append(data, "MESSAGE\n"...)
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(val)))
	data = append(data, size[:]...)
	data = append(data, val...)
	data = append(data, '\n')
	data = append(data, "PRIORITY=3\n"...)
	data = append(data, '\n')

	d := &journalDecoder{}
	entries := d.feed(data)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0]["MESSAGE"] != val {
		t.Fatalf("MESSAGE = %q, want %q", entries[0]["MESSAGE"], val)
	}
	if entries[0]["PRIORITY"] != "3" {
		t.Fatalf("PRIORITY = %q", entries[0]["PRIORITY"])
	}
}

func TestJournalDecoderSplitAcrossChunks(t *testing.T) {
	data := journalExport([][2]string{
		{"MESSAGE", "split entry"},
		{"PRIORITY", "4"},
	})
	data = append(data, journalExport([][2]string{
		{"MESSAGE", "second"},
		{"PRIORITY", "6"},
	})...)

	// Feed byte by byte: entries must come out exactly twice, intact.
	d := &journalDecoder{}
	var got []map[string]string
	for i := range data {
		got = append(got, d.feed(data[i:i+1])...)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	if got[0]["MESSAGE"] != "split entry" || got[1]["MESSAGE"] != "second" {
		t.Fatalf("messages = %q, %q", got[0]["MESSAGE"], got[1]["MESSAGE"])
	}
}

func TestEntryFromJournalMapping(t *testing.T) {
	now := time.Now()
	fields := map[string]string{
		"MESSAGE":              "connection refused",
		"PRIORITY":             "3", // daemon would be 27; 3 = kern.err? no: 3 = facility 0 severity 3
		"SYSLOG_IDENTIFIER":    "sshd",
		"SYSLOG_PID":           "1337",
		"_HOSTNAME":            "web-1",
		"_SYSTEMD_UNIT":        "sshd.service",
		"__REALTIME_TIMESTAMP": "1755700000000000", // µs
		"__CURSOR":             "s=abc",
	}
	e, ok := entryFromJournal(fields, now)
	if !ok {
		t.Fatal("entry rejected")
	}
	if e.App != "sshd" || e.PID != "1337" || e.Msg != "connection refused" {
		t.Fatalf("app/pid/msg = %q/%q/%q", e.App, e.PID, e.Msg)
	}
	if e.Lvl != model.LevelError {
		t.Fatalf("lvl = %v, want ERROR", e.Lvl)
	}
	if e.Src != "journald.kern.sshd" {
		t.Fatalf("src = %q", e.Src)
	}
	if e.Ts.UnixMicro() != 1755700000000000 {
		t.Fatalf("ts = %v", e.Ts)
	}
	if e.Fields["_SYSTEMD_UNIT"] != "sshd.service" || e.Fields["_HOSTNAME"] != "web-1" {
		t.Fatalf("fields = %v", e.Fields)
	}
	if _, ok := e.Fields["__CURSOR"]; ok {
		t.Fatal("__CURSOR must be dropped")
	}
	if e.Fields["facility"] != "kern" {
		t.Fatalf("facility = %q", e.Fields["facility"])
	}
}

func TestEntryFromJournalDaemonFacility(t *testing.T) {
	e, ok := entryFromJournal(map[string]string{
		"MESSAGE":  "started",
		"PRIORITY": "30", // facility 3 (daemon), severity 6 (info)
		"_COMM":    "cron",
	}, time.Now())
	if !ok {
		t.Fatal("entry rejected")
	}
	if e.App != "cron" || e.Src != "journald.daemon.cron" || e.Lvl != model.LevelInfo {
		t.Fatalf("app/src/lvl = %q/%q/%v", e.App, e.Src, e.Lvl)
	}
}

func TestEntryFromJournalNoMessage(t *testing.T) {
	if _, ok := entryFromJournal(map[string]string{"PRIORITY": "6"}, time.Now()); ok {
		t.Fatal("entry without MESSAGE must be rejected")
	}
}

func TestJournaldServerUDP(t *testing.T) {
	sa := &syncAppender{}
	s, err := StartJournald(sa, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	conn, err := net.Dial("udp", s.pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	data := journalExport([][2]string{
		{"MESSAGE", "over the wire"},
		{"PRIORITY", "5"},
		{"SYSLOG_IDENTIFIER", "kernel"},
	})
	// Split mid-entry across two datagrams: reassembly must survive it.
	if _, err := conn.Write(data[:10]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := conn.Write(data[10:]); err != nil {
		t.Fatal(err)
	}

	waitCount(t, sa, 1)
	got := sa.get(0)
	if got.Msg != "over the wire" || got.App != "kernel" || got.Lvl != model.LevelLog {
		t.Fatalf("entry = %+v", got)
	}
	if got.Fields["ip"] != "127.0.0.1" {
		t.Fatalf("ip = %q", got.Fields["ip"])
	}
}
