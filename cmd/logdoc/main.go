// LogDoc v2 — a single binary: ingest, storage, query, UI.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/LogDoc-org/logdoc/internal/config"
	"github.com/LogDoc-org/logdoc/internal/ingest"
	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/query"
	"github.com/LogDoc-org/logdoc/internal/selflog"
	"github.com/LogDoc-org/logdoc/internal/storage"
	"github.com/LogDoc-org/logdoc/internal/storage/clickhouse"
	"github.com/LogDoc-org/logdoc/internal/tail"
	"github.com/LogDoc-org/logdoc/ui"
)

// fanout delivers an incoming entry to all receivers (writer + live tail).
type fanout []ingest.Appender

func (f fanout) Append(e model.Entry) {
	for _, a := range f {
		a.Append(e)
	}
}

// selfSink is a non-blocking receiver for self-logs: writer via TryAppend + live tail.
type selfSink struct {
	batcher *storage.Batcher
	hub     *tail.Hub
}

func (s selfSink) TryAppend(e model.Entry) bool {
	s.hub.Append(e) // the hub is non-blocking by construction
	return s.batcher.TryAppend(e)
}

var version = "dev" // injected via -ldflags "-X main.version=..."

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "logdoc:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.Log.Level)
	slog.SetDefault(logger)
	logger.Info("logdoc starting", "version", version, "http", cfg.HTTP.Addr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := clickhouse.Open(ctx, clickhouse.Options{
		Addr:     cfg.ClickHouse.Addr,
		Database: cfg.ClickHouse.Database,
		Username: cfg.ClickHouse.Username,
		Password: cfg.ClickHouse.Password,
		TTLDays:  cfg.ClickHouse.TTLDays,
	})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	logger.Info("clickhouse connected", "addr", cfg.ClickHouse.Addr, "db", cfg.ClickHouse.Database)

	batcher := storage.NewBatcher(store, storage.BatcherOptions{
		BatchSize:     cfg.ClickHouse.BatchSize,
		FlushInterval: cfg.ClickHouse.FlushInterval,
	})
	defer batcher.Close() // flush the remaining tail before closing the store

	hub := tail.NewHub()
	sink := fanout{batcher, hub}

	// Dogfooding: from this point on, LogDoc's own logs go into LogDoc itself.
	logger = slog.New(selflog.New(logger.Handler(), selfSink{batcher, hub}))
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
	})
	mux.Handle("POST /api/v1/ingest",
		ingest.RequireAPIKey(cfg.Ingest.APIKey, ingest.NewHTTPHandler(sink, 0)))
	mux.Handle("GET /api/v1/query",
		ingest.RequireAPIKey(cfg.Ingest.APIKey, query.NewHTTPHandler(store, store)))
	mux.Handle("GET /api/v1/tail",
		ingest.RequireAPIKey(cfg.Ingest.APIKey, tail.NewWSHandler(hub)))

	uiFS, err := fs.Sub(ui.Dist, "dist")
	if err != nil {
		return fmt.Errorf("ui embed: %w", err)
	}
	mux.Handle("GET /", spaHandler(uiFS))

	native, err := ingest.StartNative(sink, cfg.Ingest.Native.TCPAddr, cfg.Ingest.Native.UDPAddr)
	if err != nil {
		return fmt.Errorf("native listeners: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", cfg.HTTP.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	native.Shutdown(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	logger.Info("bye")
	return nil
}

// spaHandler serves the UI static files; unknown paths get index.html (SPA routing).
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if _, err := fs.Stat(fsys, path); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
