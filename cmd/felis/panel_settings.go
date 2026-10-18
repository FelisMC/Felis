package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"felis.lolicon.best/internal/config"
)

// Report only whether panel relay/heartbeat overrides exist, never their values.
func cmdPanelSettings(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("panel-settings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", hostSetupConfigPath, "path to felis.toml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis panel-settings: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	relay, beat, err := readPanelSettings(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintf(stderr, "felis panel-settings: %v\n", err)
		return 1
	}
	yes := func(saved bool) string {
		if saved {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(stdout, "relay=%s\nheartbeat=%s\n", yes(relay != nil), yes(beat != nil))
	return 0
}
