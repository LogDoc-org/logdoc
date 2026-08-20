// pipeplug — a test pipe plugin: appends every fired event to the file given
// in config ("out"), one "rule|message" line per fire.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/LogDoc-org/logdoc/pkg/sdk"
	"github.com/LogDoc-org/logdoc/pkg/sdk/pluginpb"
)

func main() {
	if err := sdk.ServePipe("pipeplug", "0.0.1", &pipe{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type pipe struct {
	out string
}

func (p *pipe) Configure(cfg *pluginpb.PluginConfig) error {
	p.out = cfg.GetConfig()["out"]
	if p.out == "" {
		return fmt.Errorf("config out is required")
	}
	return nil
}

func (p *pipe) Fire(_ context.Context, ev *pluginpb.Event) error {
	f, err := os.OpenFile(p.out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s|%s|%d\n", ev.GetRule(), ev.GetMessage(), len(ev.GetEntries()))
	return err
}
