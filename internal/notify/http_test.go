package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestAPI(t *testing.T, configRules []Rule) (*RulesAPI, string) {
	t.Helper()
	e, err := New(Config{Rules: configRules, Webhook: WebhookConfig{URL: "http://unused.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rules.json")
	api, err := NewRulesAPI(e, path)
	if err != nil {
		t.Fatal(err)
	}
	return api, path
}

func doJSON(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRulesAPICRUD(t *testing.T) {
	cfgRule := Rule{Name: "cfg", Type: TypeErrorThreshold, Threshold: 5,
		Window: time.Minute, Channels: []string{"webhook"}}
	api, path := newTestAPI(t, []Rule{cfgRule})
	h := api.Handler()

	// Create a UI rule with a match tree.
	body := `{"name":"billing cascade","type":"error_threshold","threshold":2,"window":"1m",
		"match":{"app":"billing","or":[{"lvl":"ERROR"},{"msg":{"contains":"pool exhausted"}}]}}`
	if w := doJSON(t, h, http.MethodPost, "/api/v1/notify/rules", body); w.Code != http.StatusNoContent {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}

	// List: config rule first, then the UI rule.
	w := doJSON(t, h, http.MethodGet, "/api/v1/notify/rules", "")
	var res rulesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Enabled || len(res.Channels) != 1 || res.Channels[0] != "webhook" {
		t.Fatalf("enabled/channels: %+v", res)
	}
	if len(res.Rules) != 2 || res.Rules[0].Name != "cfg" || res.Rules[0].Source != "config" ||
		res.Rules[1].Name != "billing cascade" || res.Rules[1].Source != "ui" {
		t.Fatalf("rules: %+v", res.Rules)
	}
	if res.Rules[1].Match == nil || res.Rules[1].Match.App != "billing" {
		t.Fatalf("match lost: %+v", res.Rules[1].Match)
	}

	// The rules file has exactly the UI rule.
	saved, err := loadRulesFile(path)
	if err != nil || len(saved) != 1 || saved[0].Name != "billing cascade" {
		t.Fatalf("saved: %v %v", saved, err)
	}

	// Config rules are read-only.
	if w := doJSON(t, h, http.MethodPost, "/api/v1/notify/rules",
		`{"name":"cfg","type":"error_threshold","threshold":1,"window":"1m"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("config overwrite: %d %s", w.Code, w.Body)
	}
	if w := doJSON(t, h, http.MethodDelete, "/api/v1/notify/rules?name=cfg", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("config delete: %d %s", w.Code, w.Body)
	}

	// Delete the UI rule; the file empties out.
	if w := doJSON(t, h, http.MethodDelete, "/api/v1/notify/rules?name=billing+cascade", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if saved, _ := loadRulesFile(path); len(saved) != 0 {
		t.Fatalf("file after delete: %v", saved)
	}
}

func TestRulesAPIValidation(t *testing.T) {
	api, _ := newTestAPI(t, nil)
	h := api.Handler()

	for _, body := range []string{
		"{not json",
		`{"name":"x","type":"bogus"}`,
		`{"name":"x","type":"error_threshold","threshold":1,"window":"nonsense"}`,
		`{"name":"x","type":"error_threshold","threshold":1,"window":"1m","channels":["nope"]}`,
		`{"name":"x","type":"error_threshold","threshold":1,"window":"1m","match":{"lvl":"FATAL"}}`,
	} {
		if w := doJSON(t, h, http.MethodPost, "/api/v1/notify/rules", body); w.Code != http.StatusBadRequest {
			t.Fatalf("body %s: %d %s", body, w.Code, w.Body)
		}
	}
	if w := doJSON(t, h, http.MethodDelete, "/api/v1/notify/rules", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("delete without name: %d", w.Code)
	}
	if w := doJSON(t, h, http.MethodPut, "/api/v1/notify/rules", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: %d", w.Code)
	}
}

func TestRulesAPIDisabled(t *testing.T) {
	api, err := NewRulesAPI(nil, filepath.Join(t.TempDir(), "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := api.Handler()

	w := doJSON(t, h, http.MethodGet, "/api/v1/notify/rules", "")
	var res rulesResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Enabled || res.Channels == nil || res.Rules == nil {
		t.Fatalf("disabled listing: %s", w.Body)
	}
	if w := doJSON(t, h, http.MethodPost, "/api/v1/notify/rules",
		`{"name":"x","type":"error_threshold","threshold":1,"window":"1m"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("post while disabled: %d", w.Code)
	}
	if w := doJSON(t, h, http.MethodDelete, "/api/v1/notify/rules?name=x", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete while disabled: %d", w.Code)
	}
}

func TestRulesAPIPersistenceRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")

	e1, err := New(Config{Webhook: WebhookConfig{URL: "http://unused.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	api1, err := NewRulesAPI(e1, path)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"name":"restartable","type":"error_threshold","app":"api","threshold":3,
		"window":"5m","cooldown":"10m","max_fires":0}`
	if w := doJSON(t, api1.Handler(), http.MethodPost, "/api/v1/notify/rules", body); w.Code != http.StatusNoContent {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}

	// A fresh engine + API (a restart) picks the rule up from the file.
	e2, err := New(Config{Webhook: WebhookConfig{URL: "http://unused.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	api2, err := NewRulesAPI(e2, path)
	if err != nil {
		t.Fatal(err)
	}
	w := doJSON(t, api2.Handler(), http.MethodGet, "/api/v1/notify/rules", "")
	var res rulesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Rules) != 1 || res.Rules[0].Name != "restartable" || res.Rules[0].Source != "ui" {
		t.Fatalf("after restart: %+v", res.Rules)
	}
	if res.Rules[0].Cooldown != "10m0s" || res.Rules[0].MaxFires == nil || *res.Rules[0].MaxFires != 0 {
		t.Fatalf("fields lost: %+v", res.Rules[0])
	}

	// A corrupt file must fail loudly, not silently drop rules.
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRulesAPI(e2, path); err == nil {
		t.Fatal("corrupt rules file must be an error")
	}
}
