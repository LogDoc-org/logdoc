package query

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// Backend — исполнитель логического плана (реализуется storage-бэкендом).
type Backend interface {
	Query(ctx context.Context, p Plan) ([]model.Entry, error)
}

// Stats — workload telemetry одного запроса: сырьё для проектирования
// будущего Engine (какие фильтры реально используются и почём они обходятся).
type Stats struct {
	Ts         time.Time
	TenantID   string
	PlanJSON   string
	DurationMs int64
	Rows       int
}

// StatsSink — приёмник телеметрии (реализуется storage-бэкендом).
type StatsSink interface {
	RecordQueryStats(ctx context.Context, s Stats)
}

// EntryDTO — JSON-представление записи в API-ответах (query и tail).
type EntryDTO struct {
	Ts     time.Time         `json:"ts"`
	App    string            `json:"app,omitempty"`
	Src    string            `json:"src,omitempty"`
	Lvl    string            `json:"lvl"`
	PID    string            `json:"pid,omitempty"`
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

func ToDTO(e model.Entry) EntryDTO {
	return EntryDTO{
		Ts:     e.Ts,
		App:    e.App,
		Src:    e.Src,
		Lvl:    e.Lvl.String(),
		PID:    e.PID,
		Msg:    e.Msg,
		Fields: e.Fields,
	}
}

type queryResponse struct {
	Entries []EntryDTO `json:"entries"`
	Count   int        `json:"count"`
	TookMs  int64      `json:"took_ms"`
}

// NewHTTPHandler — обработчик GET /api/v1/query.
func NewHTTPHandler(backend Backend, stats StatsSink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plan, err := ParsePlan(r.URL.Query())
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}

		start := time.Now()
		entries, err := backend.Query(r.Context(), plan)
		took := time.Since(start)
		if err != nil {
			slog.Error("query failed", "err", err, "plan", plan.JSON())
			httpError(w, http.StatusInternalServerError, "ошибка выполнения запроса")
			return
		}

		if stats != nil {
			// асинхронно: телеметрия не должна задерживать ответ
			go stats.RecordQueryStats(context.WithoutCancel(r.Context()), Stats{
				Ts:         start,
				TenantID:   plan.TenantID,
				PlanJSON:   plan.JSON(),
				DurationMs: took.Milliseconds(),
				Rows:       len(entries),
			})
		}

		dtos := make([]EntryDTO, len(entries))
		for i, e := range entries {
			dtos[i] = ToDTO(e)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(queryResponse{Entries: dtos, Count: len(dtos), TookMs: took.Milliseconds()})
	})
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
