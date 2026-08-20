package ingest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Syslog sink: RFC 3164 (BSD) and RFC 5424 (IETF) with auto-detection,
// behavior-compatible with the v1 SyslogHandler. TCP framing: newline-delimited
// (transparent) and octet-counting (RFC 6587), auto-detected per message.

// facilityNames — the 24 standard syslog facilities.
var facilityNames = [...]string{
	"kern", "user", "mail", "daemon", "auth", "syslog", "lpr", "news",
	"uucp", "cron", "authpriv", "ftp", "ntp", "audit", "alert", "clock",
	"local0", "local1", "local2", "local3", "local4", "local5", "local6", "local7",
}

// severityToLevel maps syslog severity 0–7 onto the 7 LogDoc levels (v1 mapping).
var severityToLevel = [...]model.Level{
	model.LevelPanic,  // 0 emerg
	model.LevelSevere, // 1 alert
	model.LevelSevere, // 2 crit
	model.LevelError,  // 3 err
	model.LevelWarn,   // 4 warning
	model.LevelLog,    // 5 notice
	model.LevelInfo,   // 6 info
	model.LevelDebug,  // 7 debug
}

// EntryFromSyslog parses one syslog message (either RFC). It never rejects
// input: unparseable text becomes a raw INFO entry (v1 behavior), so the
// second return value is false only for blank input.
func EntryFromSyslog(line []byte, remoteIP string, now time.Time) (model.Entry, bool) {
	line = bytes.TrimRight(line, "\r\n")
	if len(bytes.TrimSpace(line)) == 0 {
		return model.Entry{}, false
	}

	e := model.Entry{
		TenantID: model.DefaultTenant,
		Ts:       now,
		App:      "syslog",
		Src:      "syslog",
		Lvl:      model.LevelInfo,
		Msg:      string(line),
		Fields:   map[string]string{},
	}
	if remoteIP != "" {
		e.Fields["ip"] = remoteIP
	}

	rest, pri, ok := parsePRI(line)
	if !ok {
		return e, true // raw fallback
	}
	facility, severity := pri/8, pri%8
	e.Lvl = severityToLevel[severity]
	fac := "user"
	if int(facility) < len(facilityNames) {
		fac = facilityNames[facility]
	}
	e.Fields["facility"] = fac

	if bytes.HasPrefix(rest, []byte("1 ")) {
		parse5424(rest[2:], &e)
	} else {
		parse3164(rest, &e, now)
	}

	if e.App == "" {
		e.App = "syslog"
	}
	e.Src = "syslog." + fac + "." + e.App
	return e, true
}

// parsePRI extracts <PRI> from the head of the message.
func parsePRI(b []byte) (rest []byte, pri int, ok bool) {
	if len(b) < 3 || b[0] != '<' {
		return nil, 0, false
	}
	end := bytes.IndexByte(b[:min(len(b), 5)], '>')
	if end < 2 {
		return nil, 0, false
	}
	n, err := strconv.Atoi(string(b[1:end]))
	if err != nil || n < 0 || n > 191 {
		return nil, 0, false
	}
	return b[end+1:], n, true
}

// parse5424 fills the entry from an RFC 5424 body (after "<PRI>1 ").
// TIMESTAMP HOSTNAME APP-NAME PROCID MSGID STRUCTURED-DATA [MSG]
func parse5424(b []byte, e *model.Entry) {
	s := string(b)

	next := func() (string, bool) {
		s = strings.TrimLeft(s, " ")
		if s == "" {
			return "", false
		}
		i := strings.IndexByte(s, ' ')
		if i < 0 {
			tok := s
			s = ""
			return tok, true
		}
		tok := s[:i]
		s = s[i+1:]
		return tok, true
	}

	if ts, ok := next(); ok && ts != "-" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.Ts = t
		}
	}
	if host, ok := next(); ok && host != "-" {
		e.Fields["host"] = host
	}
	if app, ok := next(); ok && app != "-" {
		e.App = app
	}
	if pid, ok := next(); ok && pid != "-" {
		e.PID = pid
	}
	if msgid, ok := next(); ok && msgid != "-" {
		e.Fields["msgid"] = msgid
	}

	// STRUCTURED-DATA: "-" or one or more [id k="v" ...] elements.
	s = strings.TrimLeft(s, " ")
	if strings.HasPrefix(s, "-") {
		s = strings.TrimPrefix(s, "-")
	} else {
		s = parseSD(s, e.Fields)
	}

	msg := strings.TrimLeft(s, " ")
	msg = strings.TrimPrefix(msg, "\ufeff") // BOM allowed by the RFC
	if msg != "" {
		e.Msg = msg
	}
}

// parseSD consumes [id k="v" ...]... elements, storing pairs as "<id>.<key>".
// Returns the remainder (the free-form MSG part).
func parseSD(s string, fields map[string]string) string {
	for strings.HasPrefix(s, "[") {
		s = s[1:]
		i := strings.IndexAny(s, " ]")
		if i < 0 {
			return s
		}
		id := s[:i]
		s = s[i:]
		for {
			s = strings.TrimLeft(s, " ")
			if s == "" {
				return s
			}
			if s[0] == ']' {
				s = s[1:]
				break
			}
			eq := strings.IndexByte(s, '=')
			if eq < 0 || len(s) < eq+2 || s[eq+1] != '"' {
				// malformed; skip to the element end
				if j := strings.IndexByte(s, ']'); j >= 0 {
					s = s[j+1:]
				} else {
					s = ""
				}
				break
			}
			key := s[:eq]
			s = s[eq+2:]
			var val strings.Builder
			for len(s) > 0 {
				c := s[0]
				if c == '\\' && len(s) > 1 {
					val.WriteByte(s[1])
					s = s[2:]
					continue
				}
				if c == '"' {
					s = s[1:]
					break
				}
				val.WriteByte(c)
				s = s[1:]
			}
			fields[id+"."+key] = val.String()
		}
	}
	return s
}

// parse3164 fills the entry from an RFC 3164 body (after "<PRI>").
// TIMESTAMP(Mmm dd HH:mm:ss) HOSTNAME TAG[pid]: MSG — every part optional in the wild.
func parse3164(b []byte, e *model.Entry, now time.Time) {
	s := string(b)

	// Timestamp: "Jan _2 15:04:05" (15 chars).
	if len(s) >= 15 {
		if t, err := time.Parse(time.Stamp, s[:15]); err == nil {
			t = t.AddDate(now.Year(), 0, 0)
			// December logs read in January: pick the nearest year.
			if t.After(now.AddDate(0, 0, 7)) {
				t = t.AddDate(-1, 0, 0)
			}
			e.Ts = t
			s = strings.TrimLeft(s[15:], " ")
		}
	}

	// HOSTNAME (a single token before the tag; heuristic: present when
	// the next token has no ':' and one more token follows).
	if i := strings.IndexByte(s, ' '); i > 0 && !strings.ContainsAny(s[:i], ":[") {
		e.Fields["host"] = s[:i]
		s = strings.TrimLeft(s[i+1:], " ")
	}

	// TAG[pid]: msg
	if i := strings.IndexByte(s, ':'); i > 0 && i <= 48 {
		tag := s[:i]
		if j := strings.IndexByte(tag, '['); j >= 0 {
			if k := strings.IndexByte(tag[j:], ']'); k > 1 {
				e.PID = tag[j+1 : j+k]
			}
			tag = tag[:j]
		}
		if tag != "" && !strings.ContainsAny(tag, " ") {
			e.App = tag
			s = strings.TrimLeft(s[i+1:], " ")
		}
	}

	if s != "" {
		e.Msg = s
	}
}

// SyslogServer — TCP/UDP syslog listeners.
type SyslogServer struct {
	app Appender

	tcpLn  net.Listener
	udpPC  net.PacketConn
	wg     sync.WaitGroup
	closed chan struct{}
}

// StartSyslog starts the listeners. An empty address disables the corresponding listener.
func StartSyslog(app Appender, tcpAddr, udpAddr string) (*SyslogServer, error) {
	s := &SyslogServer{app: app, closed: make(chan struct{})}

	if tcpAddr != "" {
		ln, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			return nil, err
		}
		s.tcpLn = ln
		s.wg.Add(1)
		go s.acceptLoop()
		slog.Info("syslog tcp listening", "addr", tcpAddr)
	}

	if udpAddr != "" {
		pc, err := net.ListenPacket("udp", udpAddr)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.udpPC = pc
		s.wg.Add(1)
		go s.udpLoop()
		slog.Info("syslog udp listening", "addr", udpAddr)
	}

	return s, nil
}

func (s *SyslogServer) Close() {
	close(s.closed)
	if s.tcpLn != nil {
		_ = s.tcpLn.Close()
	}
	if s.udpPC != nil {
		_ = s.udpPC.Close()
	}
	s.wg.Wait()
}

// Shutdown — Close that respects the context (for symmetry with http.Server).
func (s *SyslogServer) Shutdown(ctx context.Context) {
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (s *SyslogServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.tcpLn.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			slog.Warn("syslog tcp accept", "err", err)
			continue
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn reads one TCP connection. Framing is auto-detected per message:
// octet-counting (RFC 6587: "NNN <msg>") or newline-delimited (transparent).
func (s *SyslogServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()

	remoteIP := hostOnly(conn.RemoteAddr())
	r := bufio.NewReaderSize(conn, 64<<10)

	for {
		msg, err := readSyslogFrame(r)
		if len(msg) > 0 {
			if e, ok := EntryFromSyslog(msg, remoteIP, time.Now()); ok {
				s.app.Append(e)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Warn("syslog tcp read", "remote", remoteIP, "err", err)
			}
			return
		}
	}
}

// maxSyslogFrame caps a single octet-counted frame (sanity limit).
const maxSyslogFrame = 1 << 20

// readSyslogFrame returns the next message from the stream. A frame starting
// with digits followed by a space is octet-counted (RFC 6587); anything else
// is read up to '\n'.
func readSyslogFrame(r *bufio.Reader) ([]byte, error) {
	// Peek at the first byte to pick the framing.
	b, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if b < '1' || b > '9' {
		_ = r.UnreadByte()
		line, err := r.ReadBytes('\n')
		return line, err
	}

	// Possible octet count: consume digits until space.
	n := int(b - '0')
	digits := 1
	for {
		c, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if c >= '0' && c <= '9' && digits < 8 {
			n = n*10 + int(c-'0')
			digits++
			continue
		}
		if c == ' ' && n > 0 && n <= maxSyslogFrame {
			frame := make([]byte, n)
			if _, err := io.ReadFull(r, frame); err != nil {
				return nil, err
			}
			return frame, nil
		}
		// Not octet-counting after all — treat the consumed bytes as the
		// start of a newline-delimited message.
		prefix := strconv.Itoa(n)
		if c == '\n' {
			return []byte(prefix), nil
		}
		rest, err := r.ReadBytes('\n')
		line := make([]byte, 0, len(prefix)+1+len(rest))
		line = append(line, prefix...)
		line = append(line, c)
		line = append(line, rest...)
		return line, err
	}
}

func (s *SyslogServer) udpLoop() {
	defer s.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := s.udpPC.ReadFrom(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			slog.Warn("syslog udp read", "err", err)
			continue
		}
		remoteIP := hostOnly(addr)
		now := time.Now()
		// One datagram is normally one message, but split on newlines leniently.
		for _, line := range bytes.Split(buf[:n], []byte{'\n'}) {
			if e, ok := EntryFromSyslog(line, remoteIP, now); ok {
				s.app.Append(e)
			}
		}
	}
}
