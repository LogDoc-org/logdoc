package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testEvent = Event{
	Rule:    "billing errors",
	Type:    TypeErrorThreshold,
	App:     "billing",
	Count:   14,
	Window:  "1m0s",
	Ts:      time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
	Message: "billing errors: 14 error entries from billing in the last 1m0s",
}

func TestTelegramSend(t *testing.T) {
	type msg struct {
		ChatID  int64  `json:"chat_id"`
		Text    string `json:"text"`
		Preview bool   `json:"disable_web_page_preview"`
	}
	var got []msg
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN/sendMessage" {
			t.Errorf("path: %s", r.URL.Path)
		}
		var m msg
		_ = json.NewDecoder(r.Body).Decode(&m)
		got = append(got, m)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{cfg: TelegramConfig{Token: "TOKEN", ChatIDs: []int64{101, 202}, APIURL: srv.URL}}
	if err := tg.Send(context.Background(), testEvent); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ChatID != 101 || got[1].ChatID != 202 {
		t.Fatalf("messages: %+v", got)
	}
	if got[0].Text != "[LogDoc] "+testEvent.Message || !got[0].Preview {
		t.Fatalf("first message: %+v", got[0])
	}
}

func TestTelegramSendError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"ok":false,"description":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	tg := &Telegram{cfg: TelegramConfig{Token: "BAD", ChatIDs: []int64{1}, APIURL: srv.URL}}
	err := tg.Send(context.Background(), testEvent)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 error, got %v", err)
	}
}

func TestWebhookSend(t *testing.T) {
	var got Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type: %s", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wh := &Webhook{cfg: WebhookConfig{URL: srv.URL}}
	if err := wh.Send(context.Background(), testEvent); err != nil {
		t.Fatal(err)
	}
	if got.Rule != testEvent.Rule || got.Count != 14 || got.Message != testEvent.Message {
		t.Fatalf("delivered event: %+v", got)
	}
}

func TestEmailBody(t *testing.T) {
	body := string(emailBody("logdoc@example.com", []string{"a@example.com", "b@example.com"}, testEvent))
	for _, want := range []string{
		"From: logdoc@example.com\r\n",
		"To: a@example.com, b@example.com\r\n",
		"Subject: [LogDoc] billing errors\r\n",
		"\r\n\r\n" + testEvent.Message,
		"fired at 2026-08-19 12:00:00 UTC",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}
}

func TestBuildSenders(t *testing.T) {
	names := func(cfg Config) []string {
		var out []string
		for _, s := range buildSenders(cfg) {
			out = append(out, s.Name())
		}
		return out
	}
	if got := names(Config{}); got != nil {
		t.Fatalf("empty config: %v", got)
	}
	full := Config{
		Telegram: TelegramConfig{Token: "t", ChatIDs: []int64{1}},
		Webhook:  WebhookConfig{URL: "http://h"},
		Email:    EmailConfig{SMTPAddr: "smtp:25", From: "a@b", To: []string{"c@d"}},
	}
	if got := names(full); len(got) != 3 || got[0] != "telegram" || got[1] != "webhook" || got[2] != "email" {
		t.Fatalf("full config: %v", got)
	}
	// Telegram without chat_ids is not a configured channel.
	if got := names(Config{Telegram: TelegramConfig{Token: "t"}}); got != nil {
		t.Fatalf("token without chats: %v", got)
	}
}
