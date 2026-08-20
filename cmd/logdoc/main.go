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

	"github.com/LogDoc-org/logdoc/internal/auth"
	"github.com/LogDoc-org/logdoc/internal/config"
	"github.com/LogDoc-org/logdoc/internal/graph"
	graphsqlite "github.com/LogDoc-org/logdoc/internal/graph/sqlite"
	"github.com/LogDoc-org/logdoc/internal/ingest"
	"github.com/LogDoc-org/logdoc/internal/mcpserver"
	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/notify"
	"github.com/LogDoc-org/logdoc/internal/pipeline"
	"github.com/LogDoc-org/logdoc/internal/plugins"
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

// selfSink is a non-blocking receiver for self-logs: writer via TryAppend,
// live tail, and the topology extractor (logdoc itself is a node on the map).
type selfSink struct {
	batcher   *storage.Batcher
	hub       *tail.Hub
	extractor *graph.Extractor
}

func (s selfSink) TryAppend(e model.Entry) bool {
	s.hub.Append(e)       // the hub is non-blocking by construction
	s.extractor.Append(e) // mutex + map bumps only, never blocks on I/O
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

	graphStore, err := graphsqlite.Open(cfg.Graph.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = graphStore.Close() }()
	manager := graph.NewManager(graphStore, store)
	extractor := graph.NewExtractor(manager, graph.ExtractorOptions{})
	defer extractor.Close() // flush graph aggregates before stores close

	deploys := graph.NewDeployDetector(graphStore)
	defer deploys.Close() // flush pending markers before the store closes

	hub := tail.NewHub()
	sink := fanout{batcher, hub, extractor, deploys}

	// Plugins (SDK v2): pipe plugins become notification senders, source
	// plugins start streaming once the ingest chain below is assembled.
	pluginHost, err := plugins.New(cfg.Plugins)
	if err != nil {
		return err
	}
	defer pluginHost.Close()

	notifier, err := notify.New(cfg.Notify, pluginHost.Senders()...)
	if err != nil {
		return err
	}
	if notifier != nil {
		sink = append(sink, notifier)
		notifier.Start()
		defer notifier.Close() // wait for in-flight notifications
		logger.Info("notifications enabled", "rules", len(cfg.Notify.Rules))
	}
	rulesAPI, err := notify.NewRulesAPI(notifier, cfg.Notify.RulesPath)
	if err != nil {
		return fmt.Errorf("notify rules file: %w", err)
	}

	// Users, roles, tokens. The config api_key stays as the bootstrap admin
	// credential; no key and no users = open dev mode.
	authStore, err := auth.OpenStore(cfg.Auth.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = authStore.Close() }()
	authSvc, err := auth.NewService(authStore, cfg.Ingest.APIKey, cfg.Auth.SessionTTL)
	if err != nil {
		return err
	}
	if !authSvc.Open() {
		logger.Info("auth enabled", "db", cfg.Auth.DBPath)
	}

	// Pipelines process entries before every consumer (storage, tail,
	// topology, notifications). Self-logs bypass them deliberately.
	var stream ingest.Appender = sink
	if pl, err := pipeline.New(cfg.Pipelines, sink); err != nil {
		return err
	} else if pl != nil {
		stream = pl
		logger.Info("pipelines enabled", "count", len(cfg.Pipelines))
	}

	pluginHost.Start(stream)

	// Dogfooding: from this point on, LogDoc's own logs go into LogDoc itself.
	logger = slog.New(selflog.New(logger.Handler(), selfSink{batcher, hub, extractor}))
	slog.SetDefault(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
	})
	mux.Handle("POST /api/v1/ingest",
		authSvc.Require(auth.RoleMember, ingest.NewHTTPHandler(stream, 0)))
	mux.Handle("GET /api/v1/query",
		authSvc.Require(auth.RoleMember, query.NewHTTPHandler(store, store)))
	mux.Handle("GET /api/v1/tail",
		authSvc.Require(auth.RoleMember, tail.NewWSHandler(hub)))
	mux.Handle("GET /api/v1/topology",
		authSvc.Require(auth.RoleMember, graph.NewHTTPHandler(manager)))
	mux.Handle("GET /api/v1/topology/export",
		authSvc.Require(auth.RoleMember, graph.NewExportHandler(manager)))
	mux.Handle("GET /api/v1/topology/diff",
		authSvc.Require(auth.RoleMember, graph.NewDiffHandler(manager)))
	mux.Handle("GET /api/v1/deploys",
		authSvc.Require(auth.RoleMember, graph.NewDeploysHandler(manager)))
	// Rules: members see them, only admins change them. Registered per method
	// so the catch-all "GET /" UI route stays valid.
	mux.Handle("GET /api/v1/notify/rules", authSvc.Require(auth.RoleMember, rulesAPI.Handler()))
	for _, m := range []string{"POST", "DELETE"} {
		mux.Handle(m+" /api/v1/notify/rules", authSvc.Require(auth.RoleAdmin, rulesAPI.Handler()))
	}

	// Sessions, personal tokens, user management.
	mux.Handle("POST /api/v1/auth/login", authSvc.LoginHandler())
	mux.Handle("GET /api/v1/auth/me", authSvc.MeHandler())
	for _, m := range []string{"GET", "POST", "DELETE"} {
		mux.Handle(m+" /api/v1/auth/tokens", authSvc.Require(auth.RoleMember, authSvc.TokensHandler()))
		mux.Handle(m+" /api/v1/users", authSvc.Require(auth.RoleAdmin, authSvc.UsersHandler()))
	}

	// Agent interface: MCP over Streamable HTTP (query_logs, get_topology,
	// get_topology_diff, get_service_card). The transport uses GET/POST/DELETE;
	// each method is registered separately so the catch-all "GET /" UI route
	// stays valid.
	mcpSrv := mcpserver.New(store, store, manager, version)
	mcpHandler := authSvc.Require(auth.RoleMember, mcpSrv.Handler())
	for _, m := range []string{"GET", "POST", "DELETE"} {
		mux.Handle(m+" /mcp", mcpHandler)
	}

	uiFS, err := fs.Sub(ui.Dist, "dist")
	if err != nil {
		return fmt.Errorf("ui embed: %w", err)
	}
	mux.Handle("GET /", spaHandler(uiFS))

	native, err := ingest.StartNative(stream, cfg.Ingest.Native.TCPAddr, cfg.Ingest.Native.UDPAddr)
	if err != nil {
		return fmt.Errorf("native listeners: %w", err)
	}

	syslogSrv, err := ingest.StartSyslog(stream, cfg.Ingest.Syslog.TCPAddr, cfg.Ingest.Syslog.UDPAddr)
	if err != nil {
		return fmt.Errorf("syslog listeners: %w", err)
	}

	journald, err := ingest.StartJournald(stream, cfg.Ingest.Journald.UDPAddr)
	if err != nil {
		return fmt.Errorf("journald listener: %w", err)
	}

	pythonSrv, err := ingest.StartPython(stream, cfg.Ingest.Python.TCPAddr, cfg.Ingest.Python.UDPAddr)
	if err != nil {
		return fmt.Errorf("python listeners: %w", err)
	}

	otlp, err := ingest.StartOTLP(stream, cfg.Ingest.OTLP.GRPCAddr, authSvc.Verify)
	if err != nil {
		return fmt.Errorf("otlp listener: %w", err)
	}
	if addr := otlp.Addr(); addr != "" {
		logger.Info("otlp grpc listening", "addr", addr)
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
	syslogSrv.Shutdown(shutdownCtx)
	journald.Close()
	pythonSrv.Close()
	otlp.Shutdown(shutdownCtx)
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
