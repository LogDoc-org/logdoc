// syslog-source — the reference source plugin of the Plugin SDK v2.
//
// It does the same job as the built-in syslog listener (RFC 3164/5424 over
// UDP), but lives in its own process and talks to the core over gRPC. Use it
// as the template for your own source plugin: implement sdk.Source, call
// sdk.ServeSource, read your settings from cfg.Config.
//
// Try it:
//
//	go build -o syslog-source ./plugins/syslog-source
//
//	# logdoc.yml
//	plugins:
//	  - name: syslog-plugin
//	    kind: source
//	    exec: ./syslog-source
//	    config: {udp_addr: ":6514"}
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/LogDoc-org/logdoc/internal/ingest"
	"github.com/LogDoc-org/logdoc/pkg/sdk"
	"github.com/LogDoc-org/logdoc/pkg/sdk/pluginpb"
)

func main() {
	if err := sdk.ServeSource("syslog-source", "1.0.0", source{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type source struct{}

func (source) Run(ctx context.Context, cfg *pluginpb.PluginConfig, emit func(*pluginpb.Entry)) error {
	addr := cfg.GetConfig()["udp_addr"]
	if addr == "" {
		return fmt.Errorf("config udp_addr is required")
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()
	fmt.Fprintf(os.Stderr, "syslog-source listening on %s\n", addr)

	go func() {
		<-ctx.Done()
		_ = pc.Close() // unblock ReadFrom on shutdown
	}()

	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		ip := ""
		if a, ok := from.(*net.UDPAddr); ok {
			ip = a.IP.String()
		}
		// The parser is shared with the built-in listener; a third-party
		// plugin would bring its own parsing here.
		e, ok := ingest.EntryFromSyslog(buf[:n], ip, time.Now())
		if !ok {
			continue
		}
		emit(&pluginpb.Entry{
			TsUnixNano: e.Ts.UnixNano(),
			App:        e.App,
			Src:        e.Src,
			Pid:        e.PID,
			Lvl:        uint32(e.Lvl),
			Msg:        e.Msg,
			Fields:     e.Fields,
		})
	}
}
