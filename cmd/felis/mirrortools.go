package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/imagepush"
	"felis.lolicon.best/internal/store"
)

// defaultBuildToolsStatus is where mirror-build-tools records its last run; the
// watchdog reads it to tell a vulnerability DB that stopped refreshing.
const defaultBuildToolsStatus = "/var/lib/felis/build-tools/status.json"

var readCustomImages = readCustomImagesSetting

// Mirror build tools and Java runtimes using platform credentials through the registry loopback port.
func cmdMirrorBuildTools(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mirror-build-tools", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "127.0.0.1:5000", "host[:port] of the registry to write to (plain HTTP)")
	only := fs.String("only", "", "comma-separated tool names to copy (default: all of "+toolNames()+")")
	status := fs.String("status", defaultBuildToolsStatus, `file to record the run in ("" records nothing)`)
	secrets := fs.String("secrets-env", "/etc/felis/secrets.env", "installer secrets file holding REGISTRY_PLATFORM_TOKEN, read when FELIS_REGISTRY_PASSWORD is unset")
	platformFlag := fs.String("platform", "", "os/arch of the images to copy (default: this machine's)")
	cfgPath := fs.String("config", "", "optional path to felis.toml to read the custom_images feature state")
	includeBuildTools := fs.String("include-build-tools", "auto", "whether to mirror kaniko/trivy build tools: auto, true, or false")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var cfg *config.Config
	if *cfgPath != "" {
		loaded, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "felis mirror-build-tools: %v\n", err)
			return 2
		}
		cfg = loaded
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	withBuildTools, err := resolveIncludeBuildTools(ctx, *includeBuildTools, cfg, readCustomImages)
	if err != nil {
		fmt.Fprintf(stderr, "felis mirror-build-tools: %v\n", err)
		return 2
	}
	tools, err := selectTools(*only, withBuildTools)
	if err != nil {
		fmt.Fprintf(stderr, "felis mirror-build-tools: %v\n", err)
		return 2
	}
	user, pass, err := registryWriteCredential(*secrets)
	if err != nil {
		fmt.Fprintf(stderr, "felis mirror-build-tools: %v\n", err)
		return 2
	}
	p := &imagepush.Pusher{Scheme: "http", Username: user, Password: pass, Log: stdout}
	src := &imagepush.Source{Platform: *platformFlag}
	started := time.Now()
	var failed []string
	for _, t := range tools {
		dst := strings.TrimSuffix(*endpoint, "/") + "/" + t.Mirror
		if _, err := p.Mirror(ctx, src, t.Source, dst); err != nil {
			fmt.Fprintf(stderr, "felis mirror-build-tools: %s: %v\n", t.Name, err)
			failed = append(failed, t.Name+": "+err.Error())
		}
	}
	if *status != "" {
		st, err := imagepush.ReadMirrorStatus(*status)
		if err != nil || st == nil {
			st = &imagepush.MirrorStatus{}
		}
		st.LastAttempt = started
		st.LastError = strings.Join(failed, "; ")
		if len(failed) == 0 {
			st.LastSuccess = started
		}
		if err := imagepush.WriteMirrorStatus(*status, *st); err != nil {
			fmt.Fprintf(stderr, "felis mirror-build-tools: record %s: %v\n", *status, err)
		}
	}
	if len(failed) > 0 {
		return 1
	}
	return 0
}

func readCustomImagesSetting(ctx context.Context, dbURL string) (bool, bool) {
	if dbURL == "" {
		return false, false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, dbURL)
	if err != nil {
		return false, false
	}
	defer drv.Close()
	var raw []byte
	if err := drv.DB().QueryRowContext(ctx, `SELECT value FROM platform_settings WHERE key = 'platform_features'`).Scan(&raw); err != nil {
		return false, false
	}
	var parsed struct {
		CustomImages bool `json:"customImages"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, false
	}
	return parsed.CustomImages, true
}

func effectiveCustomImages(ctx context.Context, cfg *config.Config, readSetting func(context.Context, string) (bool, bool)) bool {
	if cfg == nil {
		return false
	}
	if readSetting != nil && cfg.Database.URL != "" {
		if enabled, found := readSetting(ctx, cfg.Database.URL); found {
			return enabled
		}
	}
	return cfg.Features.DefaultCustomImages()
}

func resolveIncludeBuildTools(ctx context.Context, mode string, cfg *config.Config, readSetting func(context.Context, string) (bool, bool)) (bool, error) {
	switch strings.TrimSpace(mode) {
	case "", "auto":
		if cfg == nil {
			return true, nil
		}
		return effectiveCustomImages(ctx, cfg, readSetting), nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid -include-build-tools %q (want auto, true, or false)", mode)
	}
}

// mirroredTools are the build tools and the servers' runtime images, every
// image this command copies.
func mirroredTools() []build.Tool {
	return append(append([]build.Tool{}, build.Tools...), build.RuntimeImages...)
}

func toolNames() string {
	var names []string
	for _, t := range mirroredTools() {
		names = append(names, t.Name)
	}
	return strings.Join(names, ",")
}

func selectTools(only string, includeBuildTools bool) ([]build.Tool, error) {
	if only == "" {
		if !includeBuildTools {
			return append([]build.Tool{}, build.RuntimeImages...), nil
		}
		return mirroredTools(), nil
	}
	var out []build.Tool
	for _, name := range strings.Split(only, ",") {
		name = strings.TrimSpace(name)
		found := false
		for _, t := range mirroredTools() {
			if t.Name == name {
				out = append(out, t)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown tool %q (known: %s)", name, toolNames())
		}
	}
	return out, nil
}
