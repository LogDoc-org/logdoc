package notify

import (
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
	"gopkg.in/yaml.v3"
)

// newTestEngine builds an engine with a controllable clock. The webhook
// channel exists only to satisfy the "rules need a channel" invariant —
// tests read fires straight from tick(), nothing is sent.
func newTestEngine(t *testing.T, rules []Rule, at time.Time) (*Engine, func(d time.Duration) time.Time) {
	t.Helper()
	e, err := New(Config{Rules: rules, Webhook: WebhookConfig{URL: "http://unused.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	now := at
	e.now = func() time.Time { return now }
	advance := func(d time.Duration) time.Time {
		now = now.Add(d)
		return now
	}
	return e, advance
}

func entry(app string, lvl model.Level) model.Entry {
	return model.Entry{TenantID: model.DefaultTenant, App: app, Lvl: lvl, Msg: "x"}
}

func TestThresholdFiresAndCoolsDown(t *testing.T) {
	start := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	e, advance := newTestEngine(t, []Rule{{
		Name:      "billing errors",
		Type:      TypeErrorThreshold,
		App:       "billing",
		Threshold: 3,
		Window:    time.Minute,
		Cooldown:  5 * time.Minute,
	}}, start)

	// Two errors: below the threshold.
	e.Append(entry("billing", model.LevelError))
	e.Append(entry("billing", model.LevelSevere))
	if fires := e.tick(advance(slot)); len(fires) != 0 {
		t.Fatalf("fired below threshold: %+v", fires)
	}

	// Noise that must not count: other app, low level.
	e.Append(entry("web", model.LevelError))
	e.Append(entry("billing", model.LevelWarn))
	// The third matching error crosses the threshold (window keeps the first two).
	e.Append(entry("billing", model.LevelPanic))
	fires := e.tick(advance(slot))
	if len(fires) != 1 {
		t.Fatalf("want 1 fire, got %d", len(fires))
	}
	ev := fires[0].ev
	if ev.Rule != "billing errors" || ev.Count != 3 || ev.App != "billing" {
		t.Fatalf("event: %+v", ev)
	}
	if ev.Message != "billing errors: 3 error entries from billing in the last 1m0s" {
		t.Fatalf("message: %q", ev.Message)
	}

	// More errors inside the cooldown: silent.
	for i := 0; i < 5; i++ {
		e.Append(entry("billing", model.LevelError))
	}
	if fires := e.tick(advance(slot)); len(fires) != 0 {
		t.Fatalf("fired inside cooldown: %+v", fires)
	}

	// After the cooldown a fresh burst fires again.
	advance(5 * time.Minute)
	for i := 0; i < 3; i++ {
		e.Append(entry("billing", model.LevelError))
	}
	if fires := e.tick(advance(slot)); len(fires) != 1 {
		t.Fatalf("want a fire after cooldown, got %d", len(fires))
	}
}

func TestThresholdWindowSlides(t *testing.T) {
	start := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	e, advance := newTestEngine(t, []Rule{{
		Name:      "err",
		Type:      TypeErrorThreshold,
		Threshold: 2,
		Window:    30 * time.Second, // 3 slots
	}}, start)

	// One error, then let the window fully rotate past it.
	e.Append(entry("a", model.LevelError))
	e.tick(advance(slot))
	e.tick(advance(slot))
	e.tick(advance(slot))
	// A single new error joins an empty window: still below 2.
	e.Append(entry("a", model.LevelError))
	if fires := e.tick(advance(slot)); len(fires) != 0 {
		t.Fatalf("old error still counted after window slid: %+v", fires)
	}
}

func TestSilenceFiresOnceAndRecovers(t *testing.T) {
	start := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	e, advance := newTestEngine(t, []Rule{{
		Name:     "web silent",
		Type:     TypeSilence,
		App:      "web",
		Window:   time.Minute,
		Cooldown: time.Minute,
	}}, start)

	// Never seen: no fire, even long after start.
	if fires := e.tick(advance(10 * time.Minute)); len(fires) != 0 {
		t.Fatalf("fired for an app never seen: %+v", fires)
	}

	// Seen once, then goes quiet past the window.
	e.Append(entry("web", model.LevelInfo))
	if fires := e.tick(advance(30 * time.Second)); len(fires) != 0 {
		t.Fatal("fired inside the window")
	}
	fires := e.tick(advance(time.Minute))
	if len(fires) != 1 {
		t.Fatalf("want 1 fire, got %d", len(fires))
	}
	if fires[0].ev.Message != "web silent: no logs from web for 1m0s" {
		t.Fatalf("message: %q", fires[0].ev.Message)
	}

	// Still silent: reported once, not every tick.
	if fires := e.tick(advance(10 * time.Minute)); len(fires) != 0 {
		t.Fatalf("silence re-fired: %+v", fires)
	}

	// Speaks again, then silence returns: a new fire.
	e.Append(entry("web", model.LevelInfo))
	if fires := e.tick(advance(slot)); len(fires) != 0 {
		t.Fatal("fired right after recovery")
	}
	if fires := e.tick(advance(2 * time.Minute)); len(fires) != 1 {
		t.Fatalf("want a fire for the second outage, got %d", len(fires))
	}
}

func TestNewValidation(t *testing.T) {
	if e, err := New(Config{}); e != nil || err != nil {
		t.Fatalf("no rules must disable the engine: %v %v", e, err)
	}
	bad := []Config{
		{Rules: []Rule{{Name: "x", Type: "bogus"}}, Webhook: WebhookConfig{URL: "http://h"}},
		{Rules: []Rule{{Name: "x", Type: TypeSilence}}, Webhook: WebhookConfig{URL: "http://h"}},
		{Rules: []Rule{{Type: TypeSilence, App: "a"}}, Webhook: WebhookConfig{URL: "http://h"}},
		{Rules: []Rule{{Name: "x", Type: TypeSilence, App: "a"}}}, // no channels at all
		{Rules: []Rule{{Name: "x", Type: TypeSilence, App: "a", Channels: []string{"telegram"}}},
			Webhook: WebhookConfig{URL: "http://h"}}, // references a channel that is not configured
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Fatalf("config #%d must be rejected", i)
		}
	}
}

func TestDurationYAML(t *testing.T) {
	var cfg Config
	src := `
rules:
  - name: errs
    type: error_threshold
    threshold: 5
    window: 90s
    cooldown: 10m
webhook:
  url: http://example.test/hook
`
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Rules[0].Window != 90*time.Second {
		t.Fatalf("window: %v", cfg.Rules[0].Window)
	}
	if cfg.Rules[0].Cooldown != 10*time.Minute {
		t.Fatalf("cooldown: %v", cfg.Rules[0].Cooldown)
	}
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}

	if err := yaml.Unmarshal([]byte("rules:\n  - window: nonsense\n"), &cfg); err == nil {
		t.Fatal("bad duration must be rejected")
	}
}
