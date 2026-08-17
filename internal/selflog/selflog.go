// Package selflog — dogfooding: LogDoc's own slog logs go into its own
// pipeline (app="logdoc") in addition to stderr.
package selflog

import (
	"context"
	"log/slog"
	"time"

	"github.com/LogDoc-org/logdoc/internal/model"
)

// TryAppender — a non-blocking receiver (Batcher.TryAppend, Hub via an adapter).
type TryAppender interface {
	TryAppend(e model.Entry) bool
}

// Handler decorates a regular slog.Handler, duplicating records into the sink.
// The sink must be non-blocking: logging must not slow down/deadlock the server.
type Handler struct {
	inner slog.Handler
	sink  TryAppender
}

func New(inner slog.Handler, sink TryAppender) *Handler {
	return &Handler{inner: inner, sink: sink}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	e := model.Entry{
		TenantID: model.DefaultTenant,
		Ts:       rec.Time,
		App:      "logdoc",
		Src:      "self",
		Lvl:      lvlFromSlog(rec.Level),
		Msg:      rec.Message,
	}
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	rec.Attrs(func(a slog.Attr) bool {
		if e.Fields == nil {
			e.Fields = make(map[string]string, rec.NumAttrs())
		}
		e.Fields[a.Key] = a.Value.String()
		return true
	})
	h.sink.TryAppend(e)
	return h.inner.Handle(ctx, rec)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{inner: h.inner.WithAttrs(attrs), sink: h.sink}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), sink: h.sink}
}

func lvlFromSlog(l slog.Level) model.Level {
	switch {
	case l >= slog.LevelError:
		return model.LevelError
	case l >= slog.LevelWarn:
		return model.LevelWarn
	case l >= slog.LevelInfo:
		return model.LevelInfo
	default:
		return model.LevelDebug
	}
}
