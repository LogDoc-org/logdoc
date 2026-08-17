// Package ingest — приём логов: HTTP JSON и native ld_format.
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

// Appender — приёмник записей (реализуется storage.Batcher).
type Appender interface {
	Append(e model.Entry)
}

// jsonRecord — одна запись в теле POST /api/v1/ingest.
type jsonRecord struct {
	Msg    string            `json:"msg"`
	Ts     *time.Time        `json:"ts,omitempty"`  // RFC 3339; пусто → время приёма
	App    string            `json:"app,omitempty"`
	Src    string            `json:"src,omitempty"`
	Lvl    string            `json:"lvl,omitempty"` // DEBUG|INFO|LOG|WARN|ERROR|SEVERE|PANIC
	PID    string            `json:"pid,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// NewHTTPHandler — обработчик POST /api/v1/ingest: JSON-массив записей.
// msg обязателен; записи без msg отклоняют весь запрос (явная ошибка лучше тихой потери).
func NewHTTPHandler(app Appender, maxBodyBytes int64) http.Handler {
	if maxBodyBytes <= 0 {
		maxBodyBytes = 32 << 20 // 32 MiB
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		var records []jsonRecord
		if err := json.NewDecoder(r.Body).Decode(&records); err != nil {
			httpError(w, http.StatusBadRequest, "невалидный JSON: "+err.Error())
			return
		}
		if len(records) == 0 {
			httpError(w, http.StatusBadRequest, "пустой массив записей")
			return
		}

		now := time.Now()
		entries := make([]model.Entry, 0, len(records))
		for i, rec := range records {
			if rec.Msg == "" {
				httpError(w, http.StatusBadRequest, fmt.Sprintf("запись %d: поле msg обязательно", i))
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
		fmt.Fprintf(w, `{"accepted":%d}`, len(entries))
	})
}

// RequireAPIKey — middleware авторизации по ключу.
// Ключ принимается в X-API-Key, Authorization: Bearer <key> или
// query-параметре api_key (для WebSocket из браузера, где заголовки недоступны).
// Пустой сконфигурированный ключ = авторизация выключена (dev-режим).
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
			httpError(w, http.StatusUnauthorized, "неверный API-ключ")
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
