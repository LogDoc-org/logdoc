package ingest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

type fakeAppender struct {
	entries []model.Entry
}

func (f *fakeAppender) Append(e model.Entry) { f.entries = append(f.entries, e) }

func post(h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestIngestHappyPath(t *testing.T) {
	fa := &fakeAppender{}
	h := NewHTTPHandler(fa, 0)

	w := post(h, `[
		{"msg":"hello","app":"svc","src":"main","lvl":"ERROR","pid":"42","ts":"2026-08-17T10:00:00Z","fields":{"user":"u1"}},
		{"msg":"world"}
	]`, nil)

	if w.Code != http.StatusAccepted {
		t.Fatalf("код %d, тело %s", w.Code, w.Body.String())
	}
	if len(fa.entries) != 2 {
		t.Fatalf("ожидалось 2 записи, получено %d", len(fa.entries))
	}
	e := fa.entries[0]
	if e.Msg != "hello" || e.App != "svc" || e.Lvl != model.LevelError || e.PID != "42" || e.Fields["user"] != "u1" {
		t.Fatalf("неверный маппинг: %+v", e)
	}
	want := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	if !e.Ts.Equal(want) {
		t.Fatalf("ts=%v, ожидалось %v", e.Ts, want)
	}
	if e.TenantID != model.DefaultTenant {
		t.Fatalf("tenant=%q", e.TenantID)
	}
	// вторая запись: дефолты
	if fa.entries[1].Lvl != model.LevelInfo || fa.entries[1].Ts.IsZero() {
		t.Fatalf("дефолты не применились: %+v", fa.entries[1])
	}
}

func TestIngestRejectsMissingMsg(t *testing.T) {
	fa := &fakeAppender{}
	w := post(NewHTTPHandler(fa, 0), `[{"app":"svc"}]`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("код %d", w.Code)
	}
	if len(fa.entries) != 0 {
		t.Fatalf("записи не должны были попасть в appender")
	}
}

func TestIngestRejectsBadJSONAndEmpty(t *testing.T) {
	fa := &fakeAppender{}
	h := NewHTTPHandler(fa, 0)
	if w := post(h, `{not json`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: код %d", w.Code)
	}
	if w := post(h, `[]`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("empty: код %d", w.Code)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	fa := &fakeAppender{}
	h := RequireAPIKey("secret", NewHTTPHandler(fa, 0))

	if w := post(h, `[{"msg":"x"}]`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("без ключа: код %d", w.Code)
	}
	if w := post(h, `[{"msg":"x"}]`, map[string]string{"X-API-Key": "wrong"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("неверный ключ: код %d", w.Code)
	}
	if w := post(h, `[{"msg":"x"}]`, map[string]string{"X-API-Key": "secret"}); w.Code != http.StatusAccepted {
		t.Fatalf("X-API-Key: код %d", w.Code)
	}
	if w := post(h, `[{"msg":"x"}]`, map[string]string{"Authorization": "Bearer secret"}); w.Code != http.StatusAccepted {
		t.Fatalf("Bearer: код %d", w.Code)
	}
}

func TestAPIKeyDisabledWhenEmpty(t *testing.T) {
	fa := &fakeAppender{}
	h := RequireAPIKey("", NewHTTPHandler(fa, 0))
	if w := post(h, `[{"msg":"x"}]`, nil); w.Code != http.StatusAccepted {
		t.Fatalf("код %d", w.Code)
	}
}
