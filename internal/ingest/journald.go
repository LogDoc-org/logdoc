package ingest

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Journald sink: the systemd journal export format over UDP, behavior-
// compatible with the v1 JournaldHandler. Feed it with e.g.
//
//	journalctl -o export -f | socat - udp-sendto:logdoc:5514
//
// Export format: entries are FIELD=value lines; binary values are encoded as
// FIELD\n<uint64-LE size><size bytes>\n; an empty line terminates the entry.
// Datagrams may split entries anywhere, so the stream is reassembled per
// source address.

// entryFromJournal maps one decoded journal entry onto model.Entry (v1
// mapping): MESSAGE→msg (mandatory), PRIORITY→facility+level,
// SYSLOG_IDENTIFIER→app, SYSLOG_PID→pid, src=journald.<facility>.<app>.
func entryFromJournal(fields map[string]string, now time.Time) (model.Entry, bool) {
	msg, ok := fields["MESSAGE"]
	if !ok || msg == "" {
		return model.Entry{}, false
	}

	pri := 6 // default: info, facility kern→user below
	if v, err := strconv.Atoi(fields["PRIORITY"]); err == nil && v >= 0 && v <= 191 {
		pri = v
	}
	facility, severity := pri>>3, pri&7
	fac := "user"
	if facility < len(facilityNames) {
		fac = facilityNames[facility]
	}

	app := fields["SYSLOG_IDENTIFIER"]
	if app == "" {
		app = fields["_COMM"]
	}
	if app == "" {
		app = "unknown"
	}

	e := model.Entry{
		TenantID: model.DefaultTenant,
		Ts:       journalTime(fields, now),
		App:      app,
		Src:      "journald." + fac + "." + app,
		PID:      firstNonEmpty(fields["SYSLOG_PID"], fields["_PID"]),
		Lvl:      severityToLevel[severity],
		Msg:      msg,
		Fields:   map[string]string{"facility": fac},
	}

	for k, v := range fields {
		switch k {
		case "MESSAGE", "PRIORITY", "SYSLOG_IDENTIFIER", "SYSLOG_PID", "SYSLOG_TIMESTAMP",
			"__REALTIME_TIMESTAMP", "__MONOTONIC_TIMESTAMP", "__CURSOR", "__SEQNUM", "__SEQNUM_ID":
			continue // mapped or transport metadata
		}
		e.Fields[k] = v
	}
	return e, true
}

// journalTime picks the entry timestamp: __REALTIME_TIMESTAMP (µs since epoch,
// always present in real exports), then SYSLOG_TIMESTAMP, then now.
func journalTime(fields map[string]string, now time.Time) time.Time {
	if v := fields["__REALTIME_TIMESTAMP"]; v != "" {
		if us, err := strconv.ParseInt(v, 10, 64); err == nil && us > 0 {
			return time.UnixMicro(us)
		}
	}
	if v := strings.TrimSpace(fields["SYSLOG_TIMESTAMP"]); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t
		}
		if t, err := time.Parse(time.Stamp, v); err == nil {
			t = t.AddDate(now.Year(), 0, 0)
			if t.After(now.AddDate(0, 0, 7)) {
				t = t.AddDate(-1, 0, 0)
			}
			return t
		}
	}
	return now
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// journalDecoder reassembles the export stream of one source and yields
// complete entries.
type journalDecoder struct {
	buf    []byte
	fields map[string]string
}

// maxJournalBuf caps the per-source reassembly buffer; on overflow the
// buffered tail is dropped (a lost datagram would otherwise wedge the stream).
const maxJournalBuf = 1 << 20

// feed consumes a chunk of the export stream and returns the entries
// completed by it.
func (d *journalDecoder) feed(chunk []byte) []map[string]string {
	d.buf = append(d.buf, chunk...)
	if len(d.buf) > maxJournalBuf {
		d.buf = nil
		d.fields = nil
		return nil
	}

	var done []map[string]string
	for {
		if d.fields == nil {
			d.fields = make(map[string]string)
		}
		// A field is at least "X\n"; stop when no full line is buffered.
		nl := bytes.IndexByte(d.buf, '\n')
		if nl < 0 {
			return done
		}
		line := d.buf[:nl]

		if len(line) == 0 { // entry terminator
			d.buf = d.buf[nl+1:]
			if len(d.fields) > 0 {
				done = append(done, d.fields)
			}
			d.fields = nil
			continue
		}

		if eq := bytes.IndexByte(line, '='); eq >= 0 {
			d.fields[string(line[:eq])] = string(line[eq+1:])
			d.buf = d.buf[nl+1:]
			continue
		}

		// Binary field: NAME\n<size LE64><data>\n
		need := nl + 1 + 8
		if len(d.buf) < need {
			return done
		}
		size := binary.LittleEndian.Uint64(d.buf[nl+1 : need])
		if size > maxJournalBuf {
			d.buf = nil
			d.fields = nil
			return done
		}
		if uint64(len(d.buf)) < uint64(need)+size+1 {
			return done
		}
		d.fields[string(line)] = string(d.buf[need : uint64(need)+size])
		d.buf = d.buf[uint64(need)+size+1:] // +1 skips the trailing \n
	}
}

// JournaldServer — UDP listener for the journal export stream.
type JournaldServer struct {
	app Appender

	pc     net.PacketConn
	wg     sync.WaitGroup
	closed chan struct{}

	mu       sync.Mutex
	decoders map[string]*journalDecoder
}

// maxJournalSources bounds the per-source decoder map (stray senders).
const maxJournalSources = 1024

// StartJournald starts the UDP listener. An empty addr disables it.
func StartJournald(app Appender, udpAddr string) (*JournaldServer, error) {
	s := &JournaldServer{app: app, closed: make(chan struct{}), decoders: map[string]*journalDecoder{}}
	if udpAddr == "" {
		return s, nil
	}
	pc, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	s.pc = pc
	s.wg.Add(1)
	go s.loop()
	slog.Info("journald udp listening", "addr", udpAddr)
	return s, nil
}

func (s *JournaldServer) Close() {
	close(s.closed)
	if s.pc != nil {
		_ = s.pc.Close()
	}
	s.wg.Wait()
}

func (s *JournaldServer) loop() {
	defer s.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := s.pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			slog.Warn("journald udp read", "err", err)
			continue
		}
		key := addr.String()
		s.mu.Lock()
		d := s.decoders[key]
		if d == nil {
			if len(s.decoders) >= maxJournalSources {
				s.decoders = map[string]*journalDecoder{}
			}
			d = &journalDecoder{}
			s.decoders[key] = d
		}
		entries := d.feed(buf[:n])
		s.mu.Unlock()

		now := time.Now()
		ip := hostOnly(addr)
		for _, fields := range entries {
			if e, ok := entryFromJournal(fields, now); ok {
				if ip != "" {
					e.Fields["ip"] = ip
				}
				s.app.Append(e)
			}
		}
	}
}
