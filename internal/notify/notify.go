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
	TypeErrorThreshold   = "error_threshold"
	TypeSilence          = "silence"
)

type rule struct {
	Rule            // with defaults applied
	buckets   []int // ring of per-slot error counts (error_threshold only)
	cur       int
	lastFired time.Time
	silenced  bool // silence already reported for the current gap
}

// Engine implements ingest.Appender: it observes the stream and never blocks.
type Engine struct {
	mu       sync.Mutex
	rules    []rule
	lastSeen map[string]time.Time // app → wall-clock time of the last entry
	senders  []Sender
	now      func() time.Time
	done     chan struct{}
	wg       sync.WaitGroup
}

// New builds the engine, or returns (nil, nil) when no rules are configured.
func New(cfg Config) (*Engine, error) {
	if len(cfg.Rules) == 0 {
		return nil, nil
	}

	senders := buildSenders(cfg)
	byName := map[string]bool{}
	for _, s := range senders {
		byName[s.Name()] = true
	}

	e := &Engine{
		lastSeen: map[string]time.Time{},
		senders:  senders,
		now:      time.Now,
		done:     make(chan struct{}),
	}
	for i, rc := range cfg.Rules {
		if rc.Name == "" {
			return nil, fmt.Errorf("notify: rule #%d has no name", i+1)
		}
		r := rule{Rule: rc}
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
		case TypeSilence:
			if rc.App == "" {
				return nil, fmt.Errorf("notify: silence rule %q requires app", rc.Name)
			}
			if r.Window <= 0 {
				r.Window = defaultSilenceWindow
			}
		default:
			return nil, fmt.Errorf("notify: rule %q: unknown type %q", rc.Name, rc.Type)
		}
		for _, ch := range rc.Channels {
			if !byName[ch] {
				return nil, fmt.Errorf("notify: rule %q references unconfigured channel %q", rc.Name, ch)
			}
		}
		e.rules = append(e.rules, r)
	}
	if len(senders) == 0 {
		return nil, fmt.Errorf("notify: rules configured but no channel (telegram/webhook/email) is")
	}
	return e, nil
}

// Append observes one entry. Counter bumps under a mutex only — safe on the hot path.
func (e *Engine) Append(entry model.Entry) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastSeen[entry.App] = now
	for i := range e.rules {
		r := &e.rules[i]
		if r.Type == TypeErrorThreshold && entry.Lvl >= model.LevelError &&
			(r.App == "" || r.App == entry.App) {
			r.buckets[r.cur]++
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

// Close stops the loop and waits for in-flight sends.
func (e *Engine) Close() {
	close(e.done)
	e.wg.Wait()
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
		switch r.Type {
		case TypeErrorThreshold:
			sum := 0
			for _, b := range r.buckets {
				sum += b
			}
			if sum >= r.Threshold && now.Sub(r.lastFired) >= r.Cooldown {
				fires = append(fires, fire{ev: Event{
					Rule:    r.Name,
					Type:    r.Type,
					App:     r.App,
					Count:   sum,
					Window:  r.Window.String(),
					Ts:      now,
					Message: thresholdMessage(r.Name, r.App, sum, r.Window),
				}, channels: r.Channels})
				r.lastFired = now
				for j := range r.buckets {
					r.buckets[j] = 0
				}
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
			}
		}
	}
	return fires
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
