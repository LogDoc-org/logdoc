// Package ingest — log intake: HTTP JSON and the native ld_format.
package ingest

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Appender — receiver of entries (implemented by storage.Batcher).
type Appender interface {
	Append(e model.Entry)
}

// jsonRecord — a single record in the body of POST /api/v1/ingest.
type jsonRecord struct {
	Msg    string            `json:"msg"`
	Ts     *time.Time        `json:"ts,omitempty"` // RFC 3339; empty → receive time
	App    string            `json:"app,omitempty"`
	Src    string            `json:"src,omitempty"`
	Lvl    string            `json:"lvl,omitempty"` // DEBUG|INFO|LOG|WARN|ERROR|SEVERE|PANIC
	PID    string            `json:"pid,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// NewHTTPHandler — handler for POST /api/v1/ingest: a JSON array of records.
// msg is mandatory; records without msg reject the whole request (an explicit
// error is better than a silent loss).
func NewHTTPHandler(app Appender, maxBodyBytes int64) http.Handler {
	if maxBodyBytes <= 0 {
		maxBodyBytes = 32 << 20 // 32 MiB
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		var records []jsonRecord
		if err := json.NewDecoder(r.Body).Decode(&records); err != nil {
			httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if len(records) == 0 {
			httpError(w, http.StatusBadRequest, "empty array of records")
			return
		}

		now := time.Now()
		entries := make([]model.Entry, 0, len(records))
		for i, rec := range records {
			if rec.Msg == "" {
				httpError(w, http.StatusBadRequest, fmt.Sprintf("record %d: field msg is required", i))
				return
			}
			ts := now
			if rec.Ts != nil {
				ts = *rec.Ts
			}
			lvl := model.LevelInfo
			if rec.Lvl != "" {
				lvl = model.ParseLevel(rec.Lvl)
			}
			entries = append(entries, model.Entry{
				TenantID: model.DefaultTenant,
				Ts:       ts,
				App:      rec.App,
				Src:      rec.Src,
				Lvl:      lvl,
				PID:      rec.PID,
				Msg:      rec.Msg,
				Fields:   rec.Fields,
			})
		}

		for _, e := range entries {
			app.Append(e)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"accepted":%d}`, len(entries))
	})
}

// RequireAPIKey — key-based authorization middleware.
// The key is accepted in X-API-Key, Authorization: Bearer <key> or the
// api_key query parameter (for browser WebSocket, where headers are unavailable).
// An empty configured key = authorization disabled (dev mode).
func RequireAPIKey(key string, next http.Handler) http.Handler {
	if key == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-API-Key")
		if got == "" {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				got = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		if got == "" {
			got = r.URL.Query().Get("api_key")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			httpError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
