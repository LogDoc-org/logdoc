// Package notify — built-in notifications (S4).
//
// Two rule kinds evaluated over the live ingest stream, no query layer involved:
//
//	error_threshold — N entries with lvl >= ERROR within a sliding window
//	silence         — a service that used to log stopped logging for a window
//
// Fired events fan out to the configured senders (Telegram, webhook, email).
// This is deliberately not the full v1 watchdog language: rules stay flat
// until real users ask for more (S5+).
package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

type Config struct {
	Rules    []Rule         `yaml:"rules"`
	Telegram TelegramConfig `yaml:"telegram"`
	Webhook  WebhookConfig  `yaml:"webhook"`
	Email    EmailConfig    `yaml:"email"`
	Kafka    KafkaConfig    `yaml:"kafka"`
	// RulesPath — the JSON file where rules created through the UI/API
	// persist across restarts.
	RulesPath string `yaml:"rules_path"`
}

type Rule struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // error_threshold | silence
	// App narrows the rule to one application. Mandatory for silence
	// (the absence of everyone's logs is a monitoring outage, not a signal).
	App       string        `yaml:"app"`
	Threshold int           `yaml:"threshold"` // error_threshold: entries per window, default 10
	Window    time.Duration `yaml:"window"`    // default 1m (threshold) / 5m (silence)
	Cooldown  time.Duration `yaml:"cooldown"`  // min pause between fires, default 5m
	Channels  []string      `yaml:"channels"`  // sender names; empty = all configured
	// Match replaces the default "lvl >= ERROR (+ app)" condition of
	// error_threshold with a composite one (see Match).
	Match *Match `yaml:"match"`
	// MaxFires limits how many times the rule fires: unset = unlimited,
	// 0 = once, N = at most N times (the window counter restarts between).
	MaxFires *int `yaml:"max_fires"`
}

// Event — a fired rule, the payload every sender receives.
type Event struct {
	Rule    string    `json:"rule"`
	Type    string    `json:"type"`
	App     string    `json:"app,omitempty"`
	Count   int       `json:"count,omitempty"`
	Window  string    `json:"window"`
	Ts      time.Time `json:"ts"`
	Message string    `json:"message"`
	// Entries — the matched entries behind the fire (error_threshold only,
	// capped at maxCaptured). Text channels ignore them; webhook and Kafka
	// deliver them.
	Entries []EventEntry `json:"entries,omitempty"`
}

// EventEntry — one matched log entry in the event payload.
type EventEntry struct {
	Ts     time.Time         `json:"ts"`
	App    string            `json:"app"`
	Src    string            `json:"src,omitempty"`
	Lvl    string            `json:"lvl"`
	Pid    string            `json:"pid,omitempty"`
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

func toEventEntry(e model.Entry) EventEntry {
	return EventEntry{
		Ts: e.Ts, App: e.App, Src: e.Src, Lvl: e.Lvl.String(),
		Pid: e.PID, Msg: e.Msg, Fields: e.Fields,
	}
}

// Sender delivers an event to one channel.
type Sender interface {
	Name() string
	Send(ctx context.Context, ev Event) error
}

// slot — the bucket granularity of the sliding window and the tick period.
const slot = 10 * time.Second

const (
	defaultThreshold     = 10
	defaultWindow        = time.Minute
	defaultSilenceWindow = 5 * time.Minute
	defaultCooldown      = 5 * time.Minute
	senderTimeout        = 15 * time.Second
	maxCaptured          = 50 // matched entries kept per rule for the payload
	TypeErrorThreshold   = "error_threshold"
	TypeSilence          = "silence"
)

// captured — a matched entry with its arrival time (for window pruning).
type captured struct {
	e  model.Entry
	at time.Time
}

const (
	sourceConfig = "config"
	sourceUI     = "ui"
)

type rule struct {
	Rule             // with defaults applied
	source    string // sourceConfig | sourceUI
	match     matchFunc
	buckets   []int // ring of per-slot match counts (error_threshold only)
	cur       int
	entries   []captured // matched entries since the last fire, capped
	fires     int
	disabled  bool // max_fires exhausted
	lastFired time.Time
	silenced  bool // silence already reported for the current gap
}

// maxFiresAllowed — how many fires MaxFires permits (see Rule.MaxFires).
func (r *rule) maxFiresAllowed() int {
	if r.MaxFires == nil {
		return -1 // unlimited
	}
	if *r.MaxFires <= 0 {
		return 1 // 0 = once
	}
	return *r.MaxFires
}

// Engine implements ingest.Appender: it observes the stream and never blocks.
type Engine struct {
	mu       sync.Mutex
	rules    []rule
	lastSeen map[string]time.Time // app → wall-clock time of the last entry
	senders  []Sender
	byName   map[string]bool // configured sender names
	now      func() time.Time
	done     chan struct{}
	wg       sync.WaitGroup
}

// New builds the engine. Nil (and no error) means notifications are off:
// no rules and no channels configured. With at least one channel the engine
// runs even without config rules — rules can be added through the API.
// extra — additional senders beyond the built-in channels (pipe plugins).
func New(cfg Config, extra ...Sender) (*Engine, error) {
	senders := append(buildSenders(cfg), extra...)
	if len(cfg.Rules) == 0 && len(senders) == 0 {
		return nil, nil
	}
	if len(senders) == 0 {
		return nil, fmt.Errorf("notify: rules configured but no channel (telegram/webhook/email/kafka) is")
	}

	e := &Engine{
		lastSeen: map[string]time.Time{},
		senders:  senders,
		byName:   map[string]bool{},
		now:      time.Now,
		done:     make(chan struct{}),
	}
	for _, s := range senders {
		e.byName[s.Name()] = true
	}
	for i, rc := range cfg.Rules {
		if rc.Name == "" {
			return nil, fmt.Errorf("notify: rule #%d has no name", i+1)
		}
		r, err := e.buildRule(rc, sourceConfig)
		if err != nil {
			return nil, err
		}
		e.rules = append(e.rules, r)
	}
	return e, nil
}

// buildRule validates a rule, applies the defaults and compiles the match.
func (e *Engine) buildRule(rc Rule, source string) (rule, error) {
	if rc.Name == "" {
		return rule{}, fmt.Errorf("notify: rule has no name")
	}
	r := rule{Rule: rc, source: source}
	if r.Cooldown <= 0 {
		r.Cooldown = defaultCooldown
	}
	switch rc.Type {
	case TypeErrorThreshold:
		if r.Window <= 0 {
			r.Window = defaultWindow
		}
		if r.Threshold <= 0 {
			r.Threshold = defaultThreshold
		}
		n := int((r.Window + slot - 1) / slot)
		r.buckets = make([]int, n)
		if rc.Match != nil {
			f, err := compileMatch(rc.Match, fmt.Sprintf("notify: rule %q: match", rc.Name))
			if err != nil {
				return rule{}, err
			}
			r.match = f
		}
	case TypeSilence:
		if rc.App == "" {
			return rule{}, fmt.Errorf("notify: silence rule %q requires app", rc.Name)
		}
		if rc.Match != nil {
			return rule{}, fmt.Errorf("notify: rule %q: match is only supported for %s rules", rc.Name, TypeErrorThreshold)
		}
		if r.Window <= 0 {
			r.Window = defaultSilenceWindow
		}
	default:
		return rule{}, fmt.Errorf("notify: rule %q: unknown type %q", rc.Name, rc.Type)
	}
	for _, ch := range rc.Channels {
		if !e.byName[ch] {
			return rule{}, fmt.Errorf("notify: rule %q references unconfigured channel %q", rc.Name, ch)
		}
	}
	return r, nil
}

// Append observes one entry. Counter bumps under a mutex only — safe on the hot path.
func (e *Engine) Append(entry model.Entry) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastSeen[entry.App] = now
	for i := range e.rules {
		r := &e.rules[i]
		if r.Type != TypeErrorThreshold || r.disabled {
			continue
		}
		matched := false
		if r.match != nil {
			matched = r.match(entry)
		} else {
			matched = entry.Lvl >= model.LevelError && (r.App == "" || r.App == entry.App)
		}
		if matched {
			r.buckets[r.cur]++
			if len(r.entries) == maxCaptured {
				copy(r.entries, r.entries[1:])
				r.entries = r.entries[:maxCaptured-1]
			}
			r.entries = append(r.entries, captured{e: entry, at: now})
		}
	}
}

// Start launches the evaluation loop.
func (e *Engine) Start() {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(slot)
		defer t.Stop()
		for {
			select {
			case <-e.done:
				return
			case now := <-t.C:
				for _, f := range e.tick(now) {
					e.dispatch(f)
				}
			}
		}
	}()
}

// Close stops the loop, waits for in-flight sends and closes the senders
// that hold connections (Kafka).
func (e *Engine) Close() {
	close(e.done)
	e.wg.Wait()
	for _, s := range e.senders {
		if c, ok := s.(io.Closer); ok {
			if err := c.Close(); err != nil {
				slog.Warn("notification channel close failed", "channel", s.Name(), "err", err)
			}
		}
	}
}

// fire — an event plus the channels of the rule that produced it.
type fire struct {
	ev       Event
	channels []string
}

// tick advances the sliding windows and returns the rules that fired.
func (e *Engine) tick(now time.Time) []fire {
	e.mu.Lock()
	defer e.mu.Unlock()
	var fires []fire
	for i := range e.rules {
		r := &e.rules[i]
		if r.disabled {
			continue
		}
		switch r.Type {
		case TypeErrorThreshold:
			// Expire captured entries that slid out of the window.
			cut := now.Add(-r.Window)
			for len(r.entries) > 0 && r.entries[0].at.Before(cut) {
				r.entries = r.entries[1:]
			}
			sum := 0
			for _, b := range r.buckets {
				sum += b
			}
			if sum >= r.Threshold && now.Sub(r.lastFired) >= r.Cooldown {
				ev := Event{
					Rule:    r.Name,
					Type:    r.Type,
					App:     r.App,
					Count:   sum,
					Window:  r.Window.String(),
					Ts:      now,
					Message: thresholdMessage(r.Name, r.App, sum, r.Window),
					Entries: make([]EventEntry, len(r.entries)),
				}
				for j, c := range r.entries {
					ev.Entries[j] = toEventEntry(c.e)
				}
				fires = append(fires, fire{ev: ev, channels: r.Channels})
				r.lastFired = now
				r.entries = nil
				for j := range r.buckets {
					r.buckets[j] = 0
				}
				e.countFire(r)
			}
			r.cur = (r.cur + 1) % len(r.buckets)
			r.buckets[r.cur] = 0
		case TypeSilence:
			last := e.lastSeen[r.App]
			if last.IsZero() || now.Sub(last) < r.Window {
				// Never seen (nothing to compare against) or speaking again.
				r.silenced = false
				continue
			}
			if !r.silenced && now.Sub(r.lastFired) >= r.Cooldown {
				fires = append(fires, fire{ev: Event{
					Rule:    r.Name,
					Type:    r.Type,
					App:     r.App,
					Window:  r.Window.String(),
					Ts:      now,
					Message: fmt.Sprintf("%s: no logs from %s for %s", r.Name, r.App, r.Window),
				}, channels: r.Channels})
				r.silenced = true
				r.lastFired = now
				e.countFire(r)
			}
		}
	}
	return fires
}

// countFire bumps the rule's fire counter and retires it when max_fires
// is exhausted.
func (e *Engine) countFire(r *rule) {
	r.fires++
	if allowed := r.maxFiresAllowed(); allowed >= 0 && r.fires >= allowed {
		r.disabled = true
	}
}

func thresholdMessage(name, app string, count int, window time.Duration) string {
	scope := ""
	if app != "" {
		scope = " from " + app
	}
	return fmt.Sprintf("%s: %d error entries%s in the last %s", name, count, scope, window)
}

// dispatch fans the event out to the senders selected by the rule.
func (e *Engine) dispatch(f fire) {
	for _, s := range e.senders {
		if len(f.channels) > 0 && !containsStr(f.channels, s.Name()) {
			continue
		}
		e.wg.Add(1)
		go func(s Sender) {
			defer e.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), senderTimeout)
			defer cancel()
			if err := s.Send(ctx, f.ev); err != nil {
				slog.Warn("notification failed", "channel", s.Name(), "rule", f.ev.Rule, "err", err)
			} else {
				slog.Info("notification sent", "channel", s.Name(), "rule", f.ev.Rule)
			}
		}(s)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
