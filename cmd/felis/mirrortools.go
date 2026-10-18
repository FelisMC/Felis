package main

import (
	"context"
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
	"felis.lolicon.best/internal/imagepush"
)

// defaultBuildToolsStatus is where mirror-build-tools records its last run; the
// watchdog reads it to tell a vulnerability DB that stopped refreshing.
const defaultBuildToolsStatus = "/var/lib/felis/build-tools/status.json"

// Mirror build tools and Java runtimes using platform credentials through the registry loopback port.
func cmdMirrorBuildTools(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mirror-build-tools", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "127.0.0.1:5000", "host[:port] of the registry to write to (plain HTTP)")
	only := fs.String("only", "", "comma-separated tool names to copy (default: all of "+toolNames()+")")
	status := fs.String("status", defaultBuildToolsStatus, `file to record the run in ("" records nothing)`)
	secrets := fs.String("secrets-env", "/etc/felis/secrets.env", "installer secrets file holding REGISTRY_PLATFORM_TOKEN, read when FELIS_REGISTRY_PASSWORD is unset")
	platformFlag := fs.String("platform", "", "os/arch of the images to copy (default: this machine's)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	tools, err := selectTools(*only)
	if err != nil {
		fmt.Fprintf(stderr, "felis mirror-build-tools: %v\n", err)
		return 2
	}
	user, pass, err := registryWriteCredential(*secrets)
	if err != nil {
		fmt.Fprintf(stderr, "felis mirror-build-tools: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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

func selectTools(only string) ([]build.Tool, error) {
	if only == "" {
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
