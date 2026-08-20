package plugins

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/notify"
)

// buildPlugin compiles a testdata plugin once per test binary.
var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

func plugin(t *testing.T, name string) string {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "logdoc-plugins")
		if buildErr != nil {
			return
		}
		for _, p := range []string{"sourceplug", "pipeplug"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(buildDir, p), "./testdata/"+p)
			out, err := cmd.CombinedOutput()
			if err != nil {
				buildErr = err
				t.Logf("build %s: %s", p, out)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("building test plugins: %v", buildErr)
	}
	return filepath.Join(buildDir, name)
}

type collectAppender struct {
	mu      sync.Mutex
	entries []model.Entry
}

func (c *collectAppender) Append(e model.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
}

func (c *collectAppender) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *collectAppender) get(i int) model.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[i]
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestSpecValidation(t *testing.T) {
	for _, specs := range [][]Spec{
		{{Kind: "source", Exec: "x"}},                                                  // no name
		{{Name: "a", Kind: "weird", Exec: "x"}},                                        // bad kind
		{{Name: "a", Kind: "source"}},                                                  // no exec
		{{Name: "a", Kind: "source", Exec: "x"}, {Name: "a", Kind: "pipe", Exec: "y"}}, // dup
	} {
		if _, err := New(specs); err == nil {
			t.Fatalf("specs %+v must be rejected", specs)
		}
	}
	h, err := New([]Spec{{Name: "ok", Kind: "source", Exec: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
}

func TestSourcePlugin(t *testing.T) {
	h, err := New([]Spec{{
		Name: "test-src", Kind: "source", Exec: plugin(t, "sourceplug"),
		Config: map[string]string{"msg": "hello from plugin"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	app := &collectAppender{}
	h.Start(app)

	waitFor(t, 5*time.Second, func() bool { return app.count() >= 1 }, "plugin entry")
	e := app.get(0)
	if e.Msg != "hello from plugin" || e.App != "plugtest" || e.Lvl != model.LevelWarn {
		t.Fatalf("entry = %+v", e)
	}
	if e.Fields["from"] != "test-src" {
		t.Fatalf("fields = %v", e.Fields)
	}
}

func TestSourcePluginRestart(t *testing.T) {
	h, err := New([]Spec{{
		Name: "crashy", Kind: "source", Exec: plugin(t, "sourceplug"),
		Config: map[string]string{"crash": "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	app := &collectAppender{}
	h.Start(app)

	// The plugin emits one entry and dies; supervision must bring it back.
	waitFor(t, 10*time.Second, func() bool { return app.count() >= 2 }, "restarted plugin entry")
}

func TestPipePlugin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "fired.log")
	h, err := New([]Spec{{
		Name: "test-pipe", Kind: "pipe", Exec: plugin(t, "pipeplug"),
		Config: map[string]string{"out": out},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	senders := h.Senders()
	if len(senders) != 1 || senders[0].Name() != "test-pipe" {
		t.Fatalf("senders = %v", senders)
	}

	// Before Start the sender must fail, not hang.
	if err := senders[0].Send(context.Background(), notify.Event{Rule: "r"}); err == nil {
		t.Fatal("send before start must fail")
	}

	h.Start(&collectAppender{})

	ev := notify.Event{
		Rule: "billing burst", Type: "error_threshold", Message: "12 errors in 1m",
		Ts: time.Now(),
		Entries: []notify.EventEntry{
			{Ts: time.Now(), App: "billing", Lvl: "ERROR", Msg: "boom"},
			{Ts: time.Now(), App: "billing", Lvl: "ERROR", Msg: "boom 2"},
		},
	}
	waitFor(t, 5*time.Second, func() bool {
		return senders[0].Send(context.Background(), ev) == nil
	}, "pipe plugin ready")

	waitFor(t, 2*time.Second, func() bool {
		b, err := os.ReadFile(out)
		return err == nil && strings.Contains(string(b), "billing burst|12 errors in 1m|2")
	}, "fired event in output file")
}

func TestKindMismatch(t *testing.T) {
	// A pipe binary declared as source: describe must reject it, and the
	// sender list stays empty.
	h, err := New([]Spec{{
		Name: "wrong", Kind: "source", Exec: plugin(t, "pipeplug"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if len(h.Senders()) != 0 {
		t.Fatal("source spec must not produce senders")
	}
	app := &collectAppender{}
	h.Start(app)
	time.Sleep(300 * time.Millisecond)
	if app.count() != 0 {
		t.Fatal("mismatched plugin must not deliver entries")
	}
}
