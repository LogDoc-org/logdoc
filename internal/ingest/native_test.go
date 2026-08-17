package ingest

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

type syncAppender struct {
	mu      sync.Mutex
	entries []model.Entry
}

func (s *syncAppender) Append(e model.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
}

func (s *syncAppender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *syncAppender) get(i int) model.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[i]
}

func waitCount(t *testing.T, sa *syncAppender, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sa.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("дождались %d записей из %d", sa.count(), want)
}

func TestNativeTCP(t *testing.T) {
	sa := &syncAppender{}
	srv, err := StartNative(sa, "127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.tcpLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// два события в одном соединении
	_, _ = conn.Write(event(simplePair("msg", "первое"), simplePair("app", "t"), simplePair("lvl", "2")))
	_, _ = conn.Write(event(simplePair("msg", "второе")))
	conn.Close()

	waitCount(t, sa, 2)
	e := sa.get(0)
	if e.Msg != "первое" || e.App != "t" || e.Lvl != model.LevelLog {
		t.Fatalf("маппинг: %+v", e)
	}
	if e.Fields["ip"] != "127.0.0.1" {
		t.Fatalf("ip=%q", e.Fields["ip"])
	}
}

func TestNativeUDP(t *testing.T) {
	sa := &syncAppender{}
	srv, err := StartNative(sa, "", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("udp", srv.udpPC.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write(event(simplePair("msg", "датаграмма"), simplePair("lvl", "ERROR")))
	conn.Close()

	waitCount(t, sa, 1)
	if e := sa.get(0); e.Msg != "датаграмма" || e.Lvl != model.LevelError {
		t.Fatalf("маппинг: %+v", e)
	}
}
