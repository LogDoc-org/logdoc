// Package sdk is the plugin-side half of the LogDoc Plugin SDK v2.
//
// A plugin is a standalone executable launched by the LogDoc core. It serves
// one gRPC service (SourcePlugin or PipePlugin, see proto/plugin.proto) on a
// loopback port, announces the address with a single handshake line on
// stdout, and exits when its stdin closes (the core is gone). ServeSource
// and ServePipe implement all of that; a plugin only provides the callbacks:
//
//	func main() {
//		err := sdk.ServeSource("my-source", "1.0.0", mySource{})
//		...
//	}
//
// The core restarts a crashed plugin with backoff and re-runs the callbacks,
// so plugins can be stateless about their lifecycle.
package sdk

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"google.golang.org/grpc"

	"github.com/LogDoc-org/logdoc/pkg/sdk/pluginpb"
)

// Entry levels (Entry.Lvl), matching the LogDoc model.
const (
	LevelDebug uint32 = iota
	LevelInfo
	LevelLog
	LevelWarn
	LevelError
	LevelSevere
	LevelPanic
)

// HandshakePrefix — the first stdout line of a plugin:
// HandshakePrefix + "tcp|127.0.0.1:PORT".
const HandshakePrefix = "LOGDOC-PLUGIN|1|"

// Source — a data-intake plugin. Run must block, parsing whatever the plugin
// receives and passing entries to emit (safe for concurrent use); it returns
// when ctx is canceled (the core is shutting down) or on a fatal error.
type Source interface {
	Run(ctx context.Context, cfg *pluginpb.PluginConfig, emit func(*pluginpb.Entry)) error
}

// Pipe — a notification-delivery plugin. Configure is called once after
// every (re)start, Fire once per fired rule.
type Pipe interface {
	Configure(cfg *pluginpb.PluginConfig) error
	Fire(ctx context.Context, ev *pluginpb.Event) error
}

// ServeSource runs a source plugin until the core disconnects.
func ServeSource(name, version string, src Source) error {
	return serve(&pluginpb.Info{Name: name, Version: version, Kind: "source"}, func(s *grpc.Server, info *pluginpb.Info) {
		pluginpb.RegisterSourcePluginServer(s, &sourceServer{info: info, src: src})
	})
}

// ServePipe runs a pipe plugin until the core disconnects.
func ServePipe(name, version string, p Pipe) error {
	return serve(&pluginpb.Info{Name: name, Version: version, Kind: "pipe"}, func(s *grpc.Server, info *pluginpb.Info) {
		pluginpb.RegisterPipePluginServer(s, &pipeServer{info: info, pipe: p})
	})
}

func serve(info *pluginpb.Info, register func(*grpc.Server, *pluginpb.Info)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("plugin listen: %w", err)
	}
	srv := grpc.NewServer()
	register(srv, info)

	// The handshake line must be the first thing on stdout.
	if _, err := fmt.Fprintf(os.Stdout, "%stcp|%s\n", HandshakePrefix, ln.Addr().String()); err != nil {
		return fmt.Errorf("plugin handshake: %w", err)
	}

	// Stdin EOF = the core died or asked us to stop.
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		srv.GracefulStop()
	}()

	if err := srv.Serve(ln); err != nil {
		return fmt.Errorf("plugin serve: %w", err)
	}
	return nil
}

type sourceServer struct {
	pluginpb.UnimplementedSourcePluginServer
	info *pluginpb.Info
	src  Source
}

func (s *sourceServer) Describe(context.Context, *pluginpb.Empty) (*pluginpb.Info, error) {
	return s.info, nil
}

func (s *sourceServer) Run(cfg *pluginpb.PluginConfig, stream grpc.ServerStreamingServer[pluginpb.Entry]) error {
	var mu sync.Mutex // grpc streams forbid concurrent Send
	emit := func(e *pluginpb.Entry) {
		mu.Lock()
		defer mu.Unlock()
		_ = stream.Send(e) // a broken stream also cancels the context below
	}
	return s.src.Run(stream.Context(), cfg, emit)
}

type pipeServer struct {
	pluginpb.UnimplementedPipePluginServer
	info *pluginpb.Info
	pipe Pipe
}

func (s *pipeServer) Describe(context.Context, *pluginpb.Empty) (*pluginpb.Info, error) {
	return s.info, nil
}

func (s *pipeServer) Configure(_ context.Context, cfg *pluginpb.PluginConfig) (*pluginpb.Empty, error) {
	if err := s.pipe.Configure(cfg); err != nil {
		return nil, err
	}
	return &pluginpb.Empty{}, nil
}

func (s *pipeServer) Fire(ctx context.Context, ev *pluginpb.Event) (*pluginpb.Empty, error) {
	if err := s.pipe.Fire(ctx, ev); err != nil {
		return nil, err
	}
	return &pluginpb.Empty{}, nil
}
