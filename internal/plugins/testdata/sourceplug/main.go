// sourceplug — a test source plugin: emits entries with the message from
// config; with crash=1 exits with an error after the first entry (for the
// supervision test).
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/LogDoc-org/logdoc/pkg/sdk"
	"github.com/LogDoc-org/logdoc/pkg/sdk/pluginpb"
)

func main() {
	if err := sdk.ServeSource("sourceplug", "0.0.1", src{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type src struct{}

func (src) Run(ctx context.Context, cfg *pluginpb.PluginConfig, emit func(*pluginpb.Entry)) error {
	msg := cfg.GetConfig()["msg"]
	if msg == "" {
		msg = "tick"
	}
	emit(&pluginpb.Entry{App: "plugtest", Lvl: sdk.LevelWarn, Msg: msg, Fields: map[string]string{"from": cfg.GetName()}})
	if cfg.GetConfig()["crash"] == "1" {
		// Simulate a plugin dying mid-flight.
		time.Sleep(50 * time.Millisecond)
		os.Exit(3)
	}
	<-ctx.Done()
	return nil
}
