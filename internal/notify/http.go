package notify

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
)

// RulesAPI — /api/v1/notify/rules: list rules with live counters (GET),
// create or update a UI rule (POST, upsert by name), delete one (DELETE
// ?name=). Config-file rules are listed but read-only.
type RulesAPI struct {
	engine    *Engine // nil = notifications disabled
	rulesPath string
	mu        sync.Mutex // serializes rule mutations + file writes
}

// NewRulesAPI wires the API and loads previously saved UI rules into the
// engine. A nil engine (no channels configured) still serves GET with an
// explanatory listing and rejects writes.
func NewRulesAPI(engine *Engine, rulesPath string) (*RulesAPI, error) {
	api := &RulesAPI{engine: engine, rulesPath: rulesPath}
	if engine == nil {
		return api, nil
	}
	saved, err := loadRulesFile(rulesPath)
	if err != nil {
		return nil, err
	}
	for _, d := range saved {
		rc, err := d.ToRule()
		if err != nil {
			slog.Warn("saved notify rule is invalid, skipping", "rule", d.Name, "err", err)
			continue
		}
		if err := engine.SetRule(rc); err != nil {
			slog.Warn("saved notify rule rejected, skipping", "rule", d.Name, "err", err)
		}
	}
	return api, nil
}

type rulesResponse struct {
	Enabled  bool         `json:"enabled"`  // false = no channels configured
	Channels []string     `json:"channels"` // configured sender names
	Rules    []RuleStatus `json:"rules"`
}

func (a *RulesAPI) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			a.list(w, r)
		case http.MethodPost:
			a.upsert(w, r)
		case http.MethodDelete:
			a.delete(w, r)
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
}

func (a *RulesAPI) list(w http.ResponseWriter, _ *http.Request) {
	res := rulesResponse{Channels: []string{}, Rules: []RuleStatus{}}
	if a.engine != nil {
		res.Enabled = true
		res.Channels = a.engine.Channels()
		res.Rules = a.engine.RuleStatuses()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (a *RulesAPI) upsert(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
		jsonError(w, http.StatusServiceUnavailable,
			"notifications are disabled: configure a channel (telegram/webhook/email/kafka) first")
		return
	}
	var d RuleDTO
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&d); err != nil {
		jsonError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	rc, err := d.ToRule()
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.engine.SetRule(rc); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := saveRulesFile(a.rulesPath, a.engine.uiRules()); err != nil {
		slog.Error("saving notify rules failed", "path", a.rulesPath, "err", err)
		jsonError(w, http.StatusInternalServerError, "rule applied but not persisted: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *RulesAPI) delete(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
		jsonError(w, http.StatusServiceUnavailable, "notifications are disabled")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		jsonError(w, http.StatusBadRequest, "name is required")
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.engine.DeleteRule(name); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := saveRulesFile(a.rulesPath, a.engine.uiRules()); err != nil {
		slog.Error("saving notify rules failed", "path", a.rulesPath, "err", err)
		jsonError(w, http.StatusInternalServerError, "rule removed but not persisted: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = fmt.Fprintln(w, string(b))
}
