package ingest

import (
	"net"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

var testNow = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func TestSyslog3164Full(t *testing.T) {
	// PRI 34 = facility 4 (auth), severity 2 (crit → SEVERE).
	line := []byte("<34>Oct 11 22:14:15 mymachine su[123]: 'su root' failed for lonvick on /dev/pts/8")
	e, ok := EntryFromSyslog(line, "10.0.0.1", testNow)
	if !ok {
		t.Fatal("rejected")
	}
	if e.Lvl != model.LevelSevere {
		t.Fatalf("lvl=%v", e.Lvl)
	}
	if e.App != "su" || e.PID != "123" {
		t.Fatalf("app=%q pid=%q", e.App, e.PID)
	}
	if e.Fields["facility"] != "auth" || e.Fields["host"] != "mymachine" || e.Fields["ip"] != "10.0.0.1" {
		t.Fatalf("fields=%v", e.Fields)
	}
	if e.Src != "syslog.auth.su" {
		t.Fatalf("src=%q", e.Src)
	}
	if e.Msg != "'su root' failed for lonvick on /dev/pts/8" {
		t.Fatalf("msg=%q", e.Msg)
	}
	if e.Ts.Month() != time.October || e.Ts.Day() != 11 || e.Ts.Hour() != 22 {
		t.Fatalf("ts=%v", e.Ts)
	}
	// The BSD timestamp has no year: Oct 11 is >7 days ahead of testNow
	// (Aug 19), so the nearest-year rule picks the previous year.
	if e.Ts.Year() != 2025 {
		t.Fatalf("year=%d", e.Ts.Year())
	}
}

func TestSyslog3164NoHostNoPid(t *testing.T) {
	// PRI 13 = facility 1 (user), severity 5 (notice → LOG).
	line := []byte("<13>Aug 19 09:30:00 nginx: upstream timed out")
	e, ok := EntryFromSyslog(line, "", testNow)
	if !ok {
		t.Fatal("rejected")
	}
	if e.App != "nginx" || e.PID != "" {
		t.Fatalf("app=%q pid=%q", e.App, e.PID)
	}
	if e.Lvl != model.LevelLog || e.Fields["facility"] != "user" {
		t.Fatalf("lvl=%v fields=%v", e.Lvl, e.Fields)
	}
	if _, hasHost := e.Fields["host"]; hasHost {
		t.Fatalf("unexpected host: %v", e.Fields)
	}
	if e.Msg != "upstream timed out" {
		t.Fatalf("msg=%q", e.Msg)
	}
	if e.Ts.Year() != 2026 || e.Ts.Month() != time.August {
		t.Fatalf("ts=%v", e.Ts)
	}
}

func TestSyslog5424(t *testing.T) {
	// PRI 165 = facility 20 (local4), severity 5 (notice → LOG).
	line := []byte(`<165>1 2026-08-19T10:15:30.003Z web01 payments 4321 ID47 [exampleSDID@32473 iut="3" eventSource="app \"x\""] An application event`)
	e, ok := EntryFromSyslog(line, "192.168.1.5", testNow)
	if !ok {
		t.Fatal("rejected")
	}
	if e.App != "payments" || e.PID != "4321" {
		t.Fatalf("app=%q pid=%q", e.App, e.PID)
	}
	if e.Fields["facility"] != "local4" || e.Fields["host"] != "web01" || e.Fields["msgid"] != "ID47" {
		t.Fatalf("fields=%v", e.Fields)
	}
	if e.Fields["exampleSDID@32473.iut"] != "3" {
		t.Fatalf("sd iut=%q", e.Fields["exampleSDID@32473.iut"])
	}
	if e.Fields["exampleSDID@32473.eventSource"] != `app "x"` {
		t.Fatalf("sd escape=%q", e.Fields["exampleSDID@32473.eventSource"])
	}
	if e.Msg != "An application event" {
		t.Fatalf("msg=%q", e.Msg)
	}
	if !e.Ts.Equal(time.Date(2026, 8, 19, 10, 15, 30, 3e6, time.UTC)) {
		t.Fatalf("ts=%v", e.Ts)
	}
	if e.Src != "syslog.local4.payments" {
		t.Fatalf("src=%q", e.Src)
	}
}

func TestSyslog5424NilFields(t *testing.T) {
	line := []byte("<165>1 - - - - - - only a message")
	e, ok := EntryFromSyslog(line, "", testNow)
	if !ok {
		t.Fatal("rejected")
	}
	if e.App != "syslog" || e.PID != "" {
		t.Fatalf("app=%q pid=%q", e.App, e.PID)
	}
	if e.Msg != "only a message" {
		t.Fatalf("msg=%q", e.Msg)
	}
	if !e.Ts.Equal(testNow) {
		t.Fatalf("ts=%v", e.Ts)
	}
}

func TestSyslogRawFallback(t *testing.T) {
	line := []byte("plain text, no PRI at all")
	e, ok := EntryFromSyslog(line, "1.2.3.4", testNow)
	if !ok {
		t.Fatal("rejected")
	}
	if e.Msg != "plain text, no PRI at all" || e.Lvl != model.LevelInfo || e.App != "syslog" {
		t.Fatalf("entry=%+v", e)
	}
}

func TestSyslogBlankRejected(t *testing.T) {
	if _, ok := EntryFromSyslog([]byte("  \r\n"), "", testNow); ok {
		t.Fatal("blank line accepted")
	}
}

func TestSyslogPRIBounds(t *testing.T) {
	// 192 > 191 → invalid PRI → raw fallback keeps the whole line.
	e, ok := EntryFromSyslog([]byte("<192>Aug 19 09:30:00 x: y"), "", testNow)
	if !ok {
		t.Fatal("rejected")
	}
	if e.Msg != "<192>Aug 19 09:30:00 x: y" {
		t.Fatalf("msg=%q", e.Msg)
	}
	if _, hasFac := e.Fields["facility"]; hasFac {
		t.Fatal("facility set for invalid PRI")
	}
}

func TestSyslogSeverityMapping(t *testing.T) {
	want := []model.Level{
		model.LevelPanic, model.LevelSevere, model.LevelSevere, model.LevelError,
		model.LevelWarn, model.LevelLog, model.LevelInfo, model.LevelDebug,
	}
	for sev, lvl := range want {
		line := []byte("<" + string(rune('0'+sev)) + ">test message")
		e, ok := EntryFromSyslog(line, "", testNow)
		if !ok || e.Lvl != lvl {
			t.Fatalf("severity %d: lvl=%v want %v", sev, e.Lvl, lvl)
		}
	}
}

func TestSyslogTCPNewlineFraming(t *testing.T) {
	sa := &syncAppender{}
	srv, err := StartSyslog(sa, "127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.tcpLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("<13>Aug 19 09:30:00 app1: first\n<13>Aug 19 09:30:01 app2: second\r\n"))
	_ = conn.Close()

	waitCount(t, sa, 2)
	if e := sa.get(0); e.App != "app1" || e.Msg != "first" {
		t.Fatalf("first: %+v", e)
	}
	if e := sa.get(1); e.App != "app2" || e.Msg != "second" {
		t.Fatalf("second: %+v", e)
	}
}

func TestSyslogTCPOctetCounting(t *testing.T) {
	sa := &syncAppender{}
	srv, err := StartSyslog(sa, "127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.tcpLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	msg1 := "<165>1 - web01 svc - - - counted one"
	msg2 := "<165>1 - web01 svc - - - counted two"
	frame := []byte(nil)
	frame = append(frame, []byte(itoa(len(msg1))+" "+msg1)...)
	frame = append(frame, []byte(itoa(len(msg2))+" "+msg2)...)
	// Split the write mid-frame to exercise buffered reads.
	_, _ = conn.Write(frame[:20])
	time.Sleep(20 * time.Millisecond)
	_, _ = conn.Write(frame[20:])
	_ = conn.Close()

	waitCount(t, sa, 2)
	if e := sa.get(0); e.Msg != "counted one" || e.App != "svc" {
		t.Fatalf("first: %+v", e)
	}
	if e := sa.get(1); e.Msg != "counted two" {
		t.Fatalf("second: %+v", e)
	}
}

func TestSyslogUDP(t *testing.T) {
	sa := &syncAppender{}
	srv, err := StartSyslog(sa, "", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("udp", srv.udpPC.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("<11>Aug 19 09:30:00 host1 cron[7]: job failed"))
	_ = conn.Close()

	waitCount(t, sa, 1)
	e := sa.get(0)
	if e.App != "cron" || e.PID != "7" || e.Lvl != model.LevelError || e.Fields["host"] != "host1" {
		t.Fatalf("entry: %+v", e)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
