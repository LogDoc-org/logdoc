package notify

// Dynamic rules (P3 UI): rules created through the API live alongside the
// yaml-configured ones inside the engine and persist in a small JSON file.
// Config rules are read-only from the API; UI rules survive restarts.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RuleDTO — the JSON form of a Rule (API and the rules file): durations are
// strings ("90s", "5m"), everything else mirrors the yaml.
type RuleDTO struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	App       string   `json:"app,omitempty"`
	Threshold int      `json:"threshold,omitempty"`
	Window    string   `json:"window,omitempty"`
	Cooldown  string   `json:"cooldown,omitempty"`
	Channels  []string `json:"channels,omitempty"`
	Match     *Match   `json:"match,omitempty"`
	MaxFires  *int     `json:"max_fires,omitempty"`
}

// RuleStatus — a rule plus its live state, as listed by the API.
type RuleStatus struct {
	RuleDTO
	Source    string     `json:"source"` // config | ui
	Fires     int        `json:"fires"`
	LastFired *time.Time `json:"last_fired,omitempty"`
	Disabled  bool       `json:"disabled"` // max_fires exhausted
}

// ToRule parses the DTO into a Rule, or fails on a bad duration.
func (d RuleDTO) ToRule() (Rule, error) {
	r := Rule{
		Name: d.Name, Type: d.Type, App: d.App, Threshold: d.Threshold,
		Channels: d.Channels, Match: d.Match, MaxFires: d.MaxFires,
	}
	var err error
	if d.Window != "" {
		if r.Window, err = time.ParseDuration(d.Window); err != nil || r.Window <= 0 {
			return Rule{}, fmt.Errorf("rule %q: invalid window %q", d.Name, d.Window)
		}
	}
	if d.Cooldown != "" {
		if r.Cooldown, err = time.ParseDuration(d.Cooldown); err != nil || r.Cooldown <= 0 {
			return Rule{}, fmt.Errorf("rule %q: invalid cooldown %q", d.Name, d.Cooldown)
		}
	}
	return r, nil
}

func toDTO(r Rule) RuleDTO {
	d := RuleDTO{
		Name: r.Name, Type: r.Type, App: r.App, Threshold: r.Threshold,
		Channels: r.Channels, Match: r.Match, MaxFires: r.MaxFires,
	}
	if r.Window > 0 {
		d.Window = r.Window.String()
	}
	if r.Cooldown > 0 {
		d.Cooldown = r.Cooldown.String()
	}
	return d
}

// --- engine: dynamic rules ---

// RuleStatuses returns every rule with its live counters, config rules first.
func (e *Engine) RuleStatuses() []RuleStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]RuleStatus, 0, len(e.rules))
	for i := range e.rules {
		r := &e.rules[i]
		st := RuleStatus{RuleDTO: toDTO(r.Rule), Source: r.source, Fires: r.fires, Disabled: r.disabled}
		if !r.lastFired.IsZero() {
			t := r.lastFired
			st.LastFired = &t
		}
		out = append(out, st)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source == "config"
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// SetRule adds or replaces a UI rule (counters reset on replace).
// A name owned by a config rule is rejected.
func (e *Engine) SetRule(rc Rule) error {
	built, err := e.buildRule(rc, sourceUI)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.rules {
		if e.rules[i].Name == rc.Name {
			if e.rules[i].source != sourceUI {
				return fmt.Errorf("rule %q is defined in the config file and is read-only", rc.Name)
			}
			e.rules[i] = built
			return nil
		}
	}
	e.rules = append(e.rules, built)
	return nil
}

// DeleteRule removes a UI rule. Unknown names and config rules are errors.
func (e *Engine) DeleteRule(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.rules {
		if e.rules[i].Name == name {
			if e.rules[i].source != sourceUI {
				return fmt.Errorf("rule %q is defined in the config file and is read-only", name)
			}
			e.rules = append(e.rules[:i], e.rules[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("unknown rule %q", name)
}

// Channels lists the configured sender names (for the UI rule form).
func (e *Engine) Channels() []string {
	out := make([]string, 0, len(e.senders))
	for _, s := range e.senders {
		out = append(out, s.Name())
	}
	sort.Strings(out)
	return out
}

// uiRules — a snapshot of the UI-created rules for persistence.
func (e *Engine) uiRules() []RuleDTO {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []RuleDTO{}
	for i := range e.rules {
		if e.rules[i].source == sourceUI {
			out = append(out, toDTO(e.rules[i].Rule))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- the rules file ---

// loadRulesFile reads UI rules from path; a missing file is an empty list.
func loadRulesFile(path string) ([]RuleDTO, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rules []RuleDTO
	if err := json.Unmarshal(b, &rules); err != nil {
		return nil, fmt.Errorf("rules file %s: %w", path, err)
	}
	return rules, nil
}

// saveRulesFile writes UI rules atomically (tmp + rename).
func saveRulesFile(path string, rules []RuleDTO) error {
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
