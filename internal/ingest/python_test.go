package ingest

import (
	"encoding/binary"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// pickleProto1 — a real logging.handlers.SocketHandler payload: pickle.dumps
// of a LogRecord dict, protocol 1 (what CPython's makePickle uses). Record:
// logger "myapp.db", ERROR, msg "connection lost: timeout", module db,
// filename db.py, lineno 42, funcName reconnect, process 80651.
const pickleProto1 = "7d71002858040000006e616d65710158080000006d796170702e6462710258030000006d736771035818000000636f6e6e656374696f6e206c6f73743a2074696d656f7574710458040000006172677371054e58090000006c6576656c6e616d65710658050000004552524f52710758070000006c6576656c6e6f71084b285808000000706174686e616d657109580e0000002f7372762f6170702f64622e7079710a580800000066696c656e616d65710b580500000064622e7079710c58060000006d6f64756c65710d58020000006462710e58080000006578635f696e666f710f4e58080000006578635f7465787471104e580a000000737461636b5f696e666f71114e58060000006c696e656e6f71124b2a580800000066756e634e616d65711358090000007265636f6e6e656374711458070000006372656174656471154741daa1c6b553b65758050000006d736563737116474073400000000000580f00000072656c6174697665437265617465647117473fe483126e978d50580600000074687265616471184c383334373230313636344c0a580a0000007468726561644e616d657119580a0000004d61696e546872656164711a580b00000070726f636573734e616d65711b580b0000004d61696e50726f63657373711c580700000070726f63657373711d4a0b3b010058080000007461736b4e616d65711e4e752e"

// pickleProto2 — protocol 2 dict with fixed created and bool/None values:
// {"msg": "hi", "levelname": "CRITICAL", "module": "worker",
//  "created": 1755700000.25, "process": 7, "processName": "MainProcess",
//  "filename": "w.py", "lineno": 5, "ok": True, "none": None}
const pickleProto2 = "80027d71002858030000006d7367710158020000006869710258090000006c6576656c6e616d6571035808000000435249544943414c710458060000006d6f64756c6571055806000000776f726b6572710658070000006372656174656471074741da2976c8100000580700000070726f6365737371084b07580b00000070726f636573734e616d657109580b0000004d61696e50726f63657373710a580800000066696c656e616d65710b5804000000772e7079710c58060000006c696e656e6f710d4b0558020000006f6b710e8858040000006e6f6e65710f4e752e"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUnpickleRealLogRecord(t *testing.T) {
	rec, err := unpickle(mustHex(t, pickleProto1))
	if err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "connection lost: timeout" {
		t.Fatalf("msg = %v", rec["msg"])
	}
	if rec["levelname"] != "ERROR" || rec["module"] != "db" {
		t.Fatalf("levelname/module = %v/%v", rec["levelname"], rec["module"])
	}
	if rec["lineno"] != int64(42) || rec["process"] != int64(80651) {
		t.Fatalf("lineno/process = %v/%v", rec["lineno"], rec["process"])
	}
	if rec["thread"] != int64(8347201664) { // LONG "L...L\n" opcode
		t.Fatalf("thread = %v", rec["thread"])
	}
	if rec["exc_info"] != nil {
		t.Fatalf("exc_info = %v", rec["exc_info"])
	}
	if _, ok := rec["created"].(float64); !ok {
		t.Fatalf("created = %T", rec["created"])
	}
}

func TestEntryFromPythonMapping(t *testing.T) {
	rec, err := unpickle(mustHex(t, pickleProto1))
	if err != nil {
		t.Fatal(err)
	}
	e, ok := entryFromPython(rec, time.Now())
	if !ok {
		t.Fatal("rejected")
	}
	if e.Msg != "connection lost: timeout" || e.App != "db" || e.PID != "80651" {
		t.Fatalf("msg/app/pid = %q/%q/%q", e.Msg, e.App, e.PID)
	}
	if e.Lvl != model.LevelError {
		t.Fatalf("lvl = %v", e.Lvl)
	}
	if e.Src != "MainProcess db.py" {
		t.Fatalf("src = %q", e.Src)
	}
	if e.Fields["funcName"] != "reconnect" || e.Fields["lineno"] != "42" {
		t.Fatalf("fields = %v", e.Fields)
	}
	if _, ok := e.Fields["exc_info"]; ok {
		t.Fatal("nil exc_info must be dropped")
	}
}

func TestEntryFromPythonProto2(t *testing.T) {
	rec, err := unpickle(mustHex(t, pickleProto2))
	if err != nil {
		t.Fatal(err)
	}
	e, ok := entryFromPython(rec, time.Now())
	if !ok {
		t.Fatal("rejected")
	}
	if e.Lvl != model.LevelSevere { // CRITICAL → SEVERE
		t.Fatalf("lvl = %v", e.Lvl)
	}
	if e.App != "worker" || e.Src != "MainProcess w.py" || e.PID != "7" {
		t.Fatalf("app/src/pid = %q/%q/%q", e.App, e.Src, e.PID)
	}
	if e.Ts.Unix() != 1755700000 {
		t.Fatalf("ts = %v", e.Ts)
	}
	if e.Fields["ok"] != "true" {
		t.Fatalf("ok = %q", e.Fields["ok"])
	}
	if _, ok := e.Fields["none"]; ok {
		t.Fatal("None value must be dropped")
	}
}

func TestUnpickleRejectsCode(t *testing.T) {
	// GLOBAL opcode (c) — object construction is out of the data subset.
	if _, err := unpickle([]byte("cos\nsystem\n(S'id'\ntR.")); err == nil {
		t.Fatal("GLOBAL must be rejected")
	}
	if _, err := unpickle([]byte{}); err == nil {
		t.Fatal("empty input must fail")
	}
	if _, err := unpickle([]byte("}q\x00")); err == nil {
		t.Fatal("truncated pickle must fail")
	}
}

func frame(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

func TestPythonServerTCP(t *testing.T) {
	sa := &syncAppender{}
	s, err := StartPython(sa, "127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	conn, err := net.Dial("tcp", s.tcpLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Two framed records back to back, like SocketHandler on a live socket.
	payload := append(frame(mustHex(t, pickleProto1)), frame(mustHex(t, pickleProto2))...)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}

	waitCount(t, sa, 2)
	if sa.get(0).App != "db" || sa.get(1).App != "worker" {
		t.Fatalf("apps = %q, %q", sa.get(0).App, sa.get(1).App)
	}
	if sa.get(0).Fields["ip"] != "127.0.0.1" {
		t.Fatalf("ip = %q", sa.get(0).Fields["ip"])
	}
}

func TestPythonServerUDP(t *testing.T) {
	sa := &syncAppender{}
	s, err := StartPython(sa, "", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	conn, err := net.Dial("udp", s.udpPC.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// DatagramHandler sends the same length-prefixed payload per datagram.
	if _, err := conn.Write(frame(mustHex(t, pickleProto2))); err != nil {
		t.Fatal(err)
	}
	waitCount(t, sa, 1)
	if sa.get(0).Lvl != model.LevelSevere || sa.get(0).Msg != "hi" {
		t.Fatalf("entry = %+v", sa.get(0))
	}
}
