package ingest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Python sink: the native logging protocol of Python's
// logging.handlers.SocketHandler / DatagramHandler — a pickled LogRecord dict
// prefixed with a 4-byte big-endian length. The unpickler below is a safe
// data-only subset (protocols 0–2 scalars, strings, dicts, lists, tuples);
// it never constructs objects or executes anything, unknown opcodes abort
// the record. Mapping is v1 PythonHandler-compatible: msg→msg, module→app,
// processName+filename→src, process→pid, created→ts, levelname→lvl
// (CRITICAL→SEVERE, WARNING→WARN, NOTSET→LOG).

// maxPythonFrame caps one pickled record.
const maxPythonFrame = 1 << 20

// entryFromPython maps an unpickled LogRecord dict onto model.Entry.
func entryFromPython(rec map[string]any, now time.Time) (model.Entry, bool) {
	msg := pyString(rec["msg"])
	if msg == "" {
		msg = pyString(rec["message"])
	}
	if msg == "" {
		return model.Entry{}, false
	}

	e := model.Entry{
		TenantID: model.DefaultTenant,
		Ts:       now,
		App:      pyString(rec["module"]),
		PID:      pyString(rec["process"]),
		Lvl:      pythonLevel(pyString(rec["levelname"])),
		Msg:      msg,
		Fields:   map[string]string{},
	}
	if e.App == "" {
		e.App = "python"
	}
	src := strings.TrimSpace(pyString(rec["processName"]) + " " + pyString(rec["filename"]))
	if src == "" {
		src = "python"
	}
	e.Src = src

	if created, ok := rec["created"].(float64); ok && created > 0 {
		sec, frac := math.Modf(created)
		e.Ts = time.Unix(int64(sec), int64(frac*1e9))
	}

	for k, v := range rec {
		switch k {
		case "msg", "message", "module", "process", "processName", "filename",
			"created", "levelname", "args", "exc_info", "msecs", "relativeCreated":
			continue // mapped, formatted elsewhere, or noise
		}
		if v == nil {
			continue
		}
		e.Fields[k] = pyString(v)
	}
	return e, true
}

// pythonLevel — the v1 levelname mapping.
func pythonLevel(name string) model.Level {
	switch name {
	case "CRITICAL":
		return model.LevelSevere
	case "ERROR":
		return model.LevelError
	case "WARNING":
		return model.LevelWarn
	case "INFO":
		return model.LevelInfo
	case "DEBUG":
		return model.LevelDebug
	case "NOTSET":
		return model.LevelLog
	}
	return model.LevelInfo
}

// pyString renders an unpickled value as a string.
func pyString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyString(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		parts := make([]string, 0, len(x))
		for k, val := range x {
			parts = append(parts, k+"="+pyString(val))
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		return fmt.Sprint(x)
	}
}

// --- minimal safe unpickler ---

var errPickle = errors.New("unsupported pickle")

type mark struct{} // sentinel on the stack

// unpickle decodes a data-only pickle (the LogRecord dict). Any opcode
// outside the data subset returns an error.
func unpickle(data []byte) (map[string]any, error) {
	var (
		stack []any
		memo  = map[int]any{}
		pos   = 0
	)
	push := func(v any) { stack = append(stack, v) }
	pop := func() (any, error) {
		if len(stack) == 0 {
			return nil, errPickle
		}
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		return v, nil
	}
	// popMark returns the items above the topmost mark and removes both.
	popMark := func() ([]any, error) {
		for i := len(stack) - 1; i >= 0; i-- {
			if _, ok := stack[i].(mark); ok {
				items := append([]any(nil), stack[i+1:]...)
				stack = stack[:i]
				return items, nil
			}
		}
		return nil, errPickle
	}
	need := func(n int) error {
		if pos+n > len(data) {
			return errPickle
		}
		return nil
	}
	readLine := func() (string, error) {
		i := pos
		for i < len(data) && data[i] != '\n' {
			i++
		}
		if i >= len(data) {
			return "", errPickle
		}
		s := string(data[pos:i])
		pos = i + 1
		return s, nil
	}

	for pos < len(data) {
		op := data[pos]
		pos++
		switch op {
		case '\x80': // PROTO
			if err := need(1); err != nil {
				return nil, err
			}
			pos++
		case '.': // STOP
			top, err := pop()
			if err != nil {
				return nil, err
			}
			d, ok := top.(map[string]any)
			if !ok {
				return nil, errPickle
			}
			return d, nil
		case '(': // MARK
			push(mark{})
		case 'N': // NONE
			push(nil)
		case '\x88': // NEWTRUE
			push(true)
		case '\x89': // NEWFALSE
			push(false)
		case 'I': // INT (decimal line; also proto-1 bools 01/00)
			s, err := readLine()
			if err != nil {
				return nil, err
			}
			switch s {
			case "01":
				push(true)
			case "00":
				push(false)
			default:
				n, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					return nil, errPickle
				}
				push(n)
			}
		case 'L': // LONG (decimal line, optional py2 "L" suffix)
			s, err := readLine()
			if err != nil {
				return nil, err
			}
			n, err := strconv.ParseInt(strings.TrimSuffix(s, "L"), 10, 64)
			if err != nil {
				return nil, errPickle
			}
			push(n)
		case 'F': // FLOAT (decimal line)
			s, err := readLine()
			if err != nil {
				return nil, err
			}
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, errPickle
			}
			push(f)
		case 'K': // BININT1
			if err := need(1); err != nil {
				return nil, err
			}
			push(int64(data[pos]))
			pos++
		case 'M': // BININT2 (LE)
			if err := need(2); err != nil {
				return nil, err
			}
			push(int64(binary.LittleEndian.Uint16(data[pos:])))
			pos += 2
		case 'J': // BININT (4-byte LE signed)
			if err := need(4); err != nil {
				return nil, err
			}
			push(int64(int32(binary.LittleEndian.Uint32(data[pos:]))))
			pos += 4
		case '\x8a': // LONG1 (n bytes LE two's complement)
			if err := need(1); err != nil {
				return nil, err
			}
			n := int(data[pos])
			pos++
			if n > 8 {
				return nil, errPickle
			}
			if err := need(n); err != nil {
				return nil, err
			}
			var v uint64
			for i := range n {
				v |= uint64(data[pos+i]) << (8 * i)
			}
			if n > 0 && data[pos+n-1]&0x80 != 0 { // sign-extend
				for i := n; i < 8; i++ {
					v |= 0xff << (8 * i)
				}
			}
			push(int64(v))
			pos += n
		case 'G': // BINFLOAT (8-byte BE)
			if err := need(8); err != nil {
				return nil, err
			}
			push(math.Float64frombits(binary.BigEndian.Uint64(data[pos:])))
			pos += 8
		case 'X': // BINUNICODE (4-byte LE length + UTF-8)
			if err := need(4); err != nil {
				return nil, err
			}
			n := int(binary.LittleEndian.Uint32(data[pos:]))
			pos += 4
			if n > maxPythonFrame {
				return nil, errPickle
			}
			if err := need(n); err != nil {
				return nil, err
			}
			push(string(data[pos : pos+n]))
			pos += n
		case '\x8c': // SHORT_BINUNICODE (proto 4)
			if err := need(1); err != nil {
				return nil, err
			}
			n := int(data[pos])
			pos++
			if err := need(n); err != nil {
				return nil, err
			}
			push(string(data[pos : pos+n]))
			pos += n
		case 'U': // SHORT_BINSTRING
			if err := need(1); err != nil {
				return nil, err
			}
			n := int(data[pos])
			pos++
			if err := need(n); err != nil {
				return nil, err
			}
			push(string(data[pos : pos+n]))
			pos += n
		case '}': // EMPTY_DICT
			push(map[string]any{})
		case ']': // EMPTY_LIST
			push([]any{})
		case ')': // EMPTY_TUPLE
			push([]any{})
		case 't': // TUPLE (from mark)
			items, err := popMark()
			if err != nil {
				return nil, err
			}
			push(items)
		case '\x85', '\x86', '\x87': // TUPLE1..3
			n := int(op - '\x84')
			if len(stack) < n {
				return nil, errPickle
			}
			items := append([]any(nil), stack[len(stack)-n:]...)
			stack = stack[:len(stack)-n]
			push(items)
		case 'a': // APPEND
			v, err := pop()
			if err != nil {
				return nil, err
			}
			l, err := pop()
			if err != nil {
				return nil, err
			}
			list, ok := l.([]any)
			if !ok {
				return nil, errPickle
			}
			push(append(list, v))
		case 'e': // APPENDS (from mark)
			items, err := popMark()
			if err != nil {
				return nil, err
			}
			l, err := pop()
			if err != nil {
				return nil, err
			}
			list, ok := l.([]any)
			if !ok {
				return nil, errPickle
			}
			push(append(list, items...))
		case 's': // SETITEM
			v, err := pop()
			if err != nil {
				return nil, err
			}
			k, err := pop()
			if err != nil {
				return nil, err
			}
			if err := dictSet(stack, k, v); err != nil {
				return nil, err
			}
		case 'u': // SETITEMS (k v pairs from mark)
			items, err := popMark()
			if err != nil {
				return nil, err
			}
			if len(items)%2 != 0 {
				return nil, errPickle
			}
			for i := 0; i < len(items); i += 2 {
				if err := dictSet(stack, items[i], items[i+1]); err != nil {
					return nil, err
				}
			}
		case 'q': // BINPUT
			if err := need(1); err != nil {
				return nil, err
			}
			if len(stack) > 0 {
				memo[int(data[pos])] = stack[len(stack)-1]
			}
			pos++
		case 'r': // LONG_BINPUT
			if err := need(4); err != nil {
				return nil, err
			}
			if len(stack) > 0 {
				memo[int(binary.LittleEndian.Uint32(data[pos:]))] = stack[len(stack)-1]
			}
			pos += 4
		case 'h': // BINGET
			if err := need(1); err != nil {
				return nil, err
			}
			push(memo[int(data[pos])])
			pos++
		case 'j': // LONG_BINGET
			if err := need(4); err != nil {
				return nil, err
			}
			push(memo[int(binary.LittleEndian.Uint32(data[pos:]))])
			pos += 4
		case '\x94': // MEMOIZE (proto 4)
			if len(stack) > 0 {
				memo[len(memo)] = stack[len(stack)-1]
			}
		default:
			return nil, fmt.Errorf("%w: opcode %#x", errPickle, op)
		}
	}
	return nil, errPickle
}

func dictSet(stack []any, k, v any) error {
	if len(stack) == 0 {
		return errPickle
	}
	d, ok := stack[len(stack)-1].(map[string]any)
	if !ok {
		return errPickle
	}
	key, ok := k.(string)
	if !ok {
		return errPickle
	}
	d[key] = v
	return nil
}

// --- listeners ---

// PythonServer — TCP/UDP listeners for the Python logging protocol.
type PythonServer struct {
	app Appender

	tcpLn  net.Listener
	udpPC  net.PacketConn
	wg     sync.WaitGroup
	closed chan struct{}
}

// StartPython starts the listeners. Empty addresses disable them.
func StartPython(app Appender, tcpAddr, udpAddr string) (*PythonServer, error) {
	s := &PythonServer{app: app, closed: make(chan struct{})}

	if tcpAddr != "" {
		ln, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			return nil, err
		}
		s.tcpLn = ln
		s.wg.Add(1)
		go s.acceptLoop()
		slog.Info("python tcp listening", "addr", tcpAddr)
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
		slog.Info("python udp listening", "addr", udpAddr)
	}
	return s, nil
}

func (s *PythonServer) Close() {
	close(s.closed)
	if s.tcpLn != nil {
		_ = s.tcpLn.Close()
	}
	if s.udpPC != nil {
		_ = s.udpPC.Close()
	}
	s.wg.Wait()
}

func (s *PythonServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.tcpLn.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			slog.Warn("python tcp accept", "err", err)
			continue
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// handleConn reads length-prefixed pickled records from one connection.
func (s *PythonServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()

	remoteIP := hostOnly(conn.RemoteAddr())
	r := bufio.NewReaderSize(conn, 64<<10)
	var head [4]byte
	for {
		if _, err := io.ReadFull(r, head[:]); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Warn("python tcp read", "remote", remoteIP, "err", err)
			}
			return
		}
		n := binary.BigEndian.Uint32(head[:])
		if n == 0 || n > maxPythonFrame {
			slog.Warn("python tcp frame too large", "remote", remoteIP, "size", n)
			return
		}
		frame := make([]byte, n)
		if _, err := io.ReadFull(r, frame); err != nil {
			return
		}
		s.record(frame, remoteIP)
	}
}

func (s *PythonServer) udpLoop() {
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
			slog.Warn("python udp read", "err", err)
			continue
		}
		// DatagramHandler sends the same length-prefixed payload in one datagram.
		frame := buf[:n]
		if n > 4 {
			if l := binary.BigEndian.Uint32(frame[:4]); int(l) == n-4 {
				frame = frame[4:]
			}
		}
		s.record(frame, hostOnly(addr))
	}
}

func (s *PythonServer) record(frame []byte, remoteIP string) {
	rec, err := unpickle(frame)
	if err != nil {
		slog.Warn("python record", "remote", remoteIP, "err", err)
		return
	}
	e, ok := entryFromPython(rec, time.Now())
	if !ok {
		return
	}
	if remoteIP != "" {
		e.Fields["ip"] = remoteIP
	}
	s.app.Append(e)
}
