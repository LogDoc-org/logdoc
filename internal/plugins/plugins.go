// Package plugins is the core-side half of the Plugin SDK v2: it launches
// plugin executables from the `plugins:` config section, performs the stdout
// handshake, connects over gRPC and supervises the subprocess — a crashed or
// disconnected plugin is restarted with exponential backoff.
//
// Source plugins stream entries into the ingest pipeline; pipe plugins are
// exposed as notification senders under the instance name from the config.
package plugins

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/LogDoc-org/logdoc/internal/model"
	"github.com/LogDoc-org/logdoc/internal/notify"
	"github.com/LogDoc-org/logdoc/pkg/sdk"
	"github.com/LogDoc-org/logdoc/pkg/sdk/pluginpb"
)

// Spec — one entry of the `plugins:` config section.
type Spec struct {
	Name   string            `yaml:"name"` // instance name; pipe plugins become a channel with this name
	Kind   string            `yaml:"kind"` // source | pipe
	Exec   string            `yaml:"exec"` // path to the plugin executable
	Args   []string          `yaml:"args"`
	Config map[string]string `yaml:"config"` // passed to the plugin verbatim
}

// Appender — where source-plugin entries go (implemented by the ingest fanout).
type Appender interface {
	Append(model.Entry)
}

const (
	handshakeTimeout = 10 * time.Second
	backoffMin       = time.Second
	backoffMax       = 30 * time.Second
	stableAfter      = time.Minute // a run this long resets the backoff
)

// proc — one supervised plugin instance.
type proc struct {
	spec Spec

	mu     sync.Mutex
	client *grpc.ClientConn // nil while the plugin is down
}

// Host owns every configured plugin.
type Host struct {
	procs  []*proc
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New validates the specs and prepares the host; nothing is launched yet
// (Senders is usable immediately, delivery starts after Start).
func New(specs []Spec) (*Host, error) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &Host{ctx: ctx, cancel: cancel}
	seen := map[string]bool{}
	for i, s := range specs {
		if s.Name == "" {
			cancel()
			return nil, fmt.Errorf("plugins: entry #%d has no name", i+1)
		}
		if s.Kind != "source" && s.Kind != "pipe" {
			cancel()
			return nil, fmt.Errorf("plugins: %q: kind must be source or pipe, got %q", s.Name, s.Kind)
		}
		if s.Exec == "" {
			cancel()
			return nil, fmt.Errorf("plugins: %q has no exec", s.Name)
		}
		if seen[s.Name] {
			cancel()
			return nil, fmt.Errorf("plugins: duplicate name %q", s.Name)
		}
		seen[s.Name] = true
		h.procs = append(h.procs, &proc{spec: s})
	}
	return h, nil
}

// Senders returns a notification sender per pipe plugin. Safe to call before
// Start; a sender errors while its plugin is down.
func (h *Host) Senders() []notify.Sender {
	var out []notify.Sender
	for _, p := range h.procs {
		if p.spec.Kind == "pipe" {
			out = append(out, &pipeSender{p: p})
		}
	}
	return out
}

// Start launches every plugin and begins supervision. Source entries go to app.
func (h *Host) Start(app Appender) {
	for _, p := range h.procs {
		h.wg.Add(1)
		go func(p *proc) {
			defer h.wg.Done()
			h.supervise(p, app)
		}(p)
	}
	if len(h.procs) > 0 {
		slog.Info("plugins started", "count", len(h.procs))
	}
}

// Close stops every plugin (stdin close, then kill) and waits.
func (h *Host) Close() {
	h.cancel()
	h.wg.Wait()
}

// supervise runs one plugin in a launch → serve → restart loop.
func (h *Host) supervise(p *proc, app Appender) {
	backoff := backoffMin
	for {
		started := time.Now()
		err := h.runOnce(p, app)
		if h.ctx.Err() != nil {
			return
		}
		if time.Since(started) > stableAfter {
			backoff = backoffMin
		}
		slog.Warn("plugin exited, restarting", "plugin", p.spec.Name, "err", err, "backoff", backoff)
		select {
		case <-h.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// runOnce launches the plugin process and serves it until it dies or the
// host shuts down.
func (h *Host) runOnce(p *proc, app Appender) error {
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.spec.Exec, p.spec.Args...)
	stdin, err := cmd.StdinPipe() // held open; closing it tells the plugin to exit
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec %s: %w", p.spec.Exec, err)
	}
	defer func() {
		_ = stdin.Close()
		cancel() // SIGKILL via CommandContext if still alive
		_ = cmd.Wait()
	}()

	// Plugin stderr → core log.
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			slog.Info("plugin", "plugin", p.spec.Name, "msg", sc.Text())
		}
	}()

	addr, err := readHandshake(ctx, stdout)
	if err != nil {
		return err
	}
	go func() { // drain post-handshake stdout
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
		}
	}()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	cfg := &pluginpb.PluginConfig{Name: p.spec.Name, Config: p.spec.Config}

	switch p.spec.Kind {
	case "source":
		client := pluginpb.NewSourcePluginClient(conn)
		if err := describe(ctx, client.Describe, p.spec); err != nil {
			return err
		}
		return h.runSource(ctx, client, cfg, app, p.spec.Name)
	default: // pipe
		client := pluginpb.NewPipePluginClient(conn)
		if err := describe(ctx, client.Describe, p.spec); err != nil {
			return err
		}
		cctx, ccancel := context.WithTimeout(ctx, 15*time.Second)
		_, err = client.Configure(cctx, cfg)
		ccancel()
		if err != nil {
			return fmt.Errorf("configure: %w", err)
		}
		p.mu.Lock()
		p.client = conn
		p.mu.Unlock()
		slog.Info("plugin ready", "plugin", p.spec.Name, "kind", "pipe")
		defer func() {
			p.mu.Lock()
			p.client = nil
			p.mu.Unlock()
		}()
		// A pipe plugin is passive; watch the process until it exits.
		<-ctx.Done()
		return ctx.Err()
	}
}

// describe verifies the plugin identifies as the configured kind.
func describe(ctx context.Context, call func(context.Context, *pluginpb.Empty, ...grpc.CallOption) (*pluginpb.Info, error), spec Spec) error {
	dctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	info, err := call(dctx, &pluginpb.Empty{})
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}
	if info.GetKind() != spec.Kind {
		return fmt.Errorf("plugin %s is a %q, config says %q", spec.Exec, info.GetKind(), spec.Kind)
	}
	slog.Info("plugin connected", "plugin", spec.Name, "reports", info.GetName(), "version", info.GetVersion(), "kind", info.GetKind())
	return nil
}

// runSource consumes the entry stream until it breaks.
func (h *Host) runSource(ctx context.Context, client pluginpb.SourcePluginClient, cfg *pluginpb.PluginConfig, app Appender, name string) error {
	stream, err := client.Run(ctx, cfg)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	slog.Info("plugin ready", "plugin", name, "kind", "source")
	for {
		pe, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("stream: %w", err)
		}
		if e, ok := entryFromProto(pe, name); ok {
			app.Append(e)
		}
	}
}

// entryFromProto maps a plugin entry onto the core model.
func entryFromProto(pe *pluginpb.Entry, pluginName string) (model.Entry, bool) {
	if pe.GetMsg() == "" {
		return model.Entry{}, false // msg is mandatory across all ingest paths
	}
	e := model.Entry{
		TenantID: model.DefaultTenant,
		App:      pe.GetApp(),
		Src:      pe.GetSrc(),
		PID:      pe.GetPid(),
		Msg:      pe.GetMsg(),
		Fields:   pe.GetFields(),
	}
	if e.App == "" {
		e.App = pluginName
	}
	if lvl := pe.GetLvl(); lvl <= uint32(model.LevelPanic) {
		e.Lvl = model.Level(lvl)
	} else {
		e.Lvl = model.LevelInfo
	}
	if ns := pe.GetTsUnixNano(); ns > 0 {
		e.Ts = time.Unix(0, ns)
	} else {
		e.Ts = time.Now()
	}
	return e, true
}

// readHandshake reads the "LOGDOC-PLUGIN|1|tcp|addr" line from stdout.
func readHandshake(ctx context.Context, stdout interface{ Read([]byte) (int, error) }) (string, error) {
	type res struct {
		addr string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if !sc.Scan() {
			ch <- res{err: fmt.Errorf("plugin exited before handshake: %w", sc.Err())}
			return
		}
		line := strings.TrimSpace(sc.Text())
		rest, ok := strings.CutPrefix(line, sdk.HandshakePrefix)
		if !ok {
			ch <- res{err: fmt.Errorf("bad handshake line %q", line)}
			return
		}
		network, addr, ok := strings.Cut(rest, "|")
		if !ok || network != "tcp" || addr == "" {
			ch <- res{err: fmt.Errorf("bad handshake transport %q", rest)}
			return
		}
		ch <- res{addr: addr}
	}()
	select {
	case r := <-ch:
		return r.addr, r.err
	case <-time.After(handshakeTimeout):
		return "", fmt.Errorf("handshake timeout")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// pipeSender adapts a pipe plugin to the notify.Sender interface.
type pipeSender struct {
	p *proc
}

func (s *pipeSender) Name() string { return s.p.spec.Name }

func (s *pipeSender) Send(ctx context.Context, ev notify.Event) error {
	s.p.mu.Lock()
	conn := s.p.client
	s.p.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("plugin %s is not running", s.p.spec.Name)
	}
	pev := &pluginpb.Event{
		Rule:       ev.Rule,
		Type:       ev.Type,
		App:        ev.App,
		Count:      int32(ev.Count),
		Window:     ev.Window,
		TsUnixNano: ev.Ts.UnixNano(),
		Message:    ev.Message,
	}
	for _, en := range ev.Entries {
		pev.Entries = append(pev.Entries, &pluginpb.Entry{
			TsUnixNano: en.Ts.UnixNano(),
			App:        en.App,
			Src:        en.Src,
			Pid:        en.Pid,
			Lvl:        uint32(model.ParseLevel(en.Lvl)),
			Msg:        en.Msg,
			Fields:     en.Fields,
		})
	}
	_, err := pluginpb.NewPipePluginClient(conn).Fire(ctx, pev)
	return err
}
