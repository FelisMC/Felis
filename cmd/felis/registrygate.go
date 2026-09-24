package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/imagepush"
	"felis.lolicon.best/internal/registrygate"
)

// cmdRegistryGate is the sidecar entrypoint in the registry pod: it owns the
// registry port (and the loopback hostPort containerd pulls through), lets reads
// through anonymously, and forwards writes to the loopback-only registry:2 only
// for an authenticated principal allowed to write that repository. See
// internal/registrygate for the policy.
//
// Tokens are files under --auth-dir, one per principal (platform, build), mounted
// from the registry-auth Secret. A missing file disables that principal: writes
// fail closed while every pull keeps working, which is the right way round for a
// registry the running workloads depend on.
func cmdRegistryGate(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("registry-gate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":5000", "address the gate serves the registry API on")
	upstream := fs.String("upstream", "http://127.0.0.1:5001", "the loopback registry the gate forwards to")
	authDir := fs.String("auth-dir", "/etc/felis-registry-auth", "directory holding one token file per principal")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	target, err := url.Parse(*upstream)
	if err != nil || target.Scheme == "" || target.Host == "" {
		fmt.Fprintf(stderr, "felis registry-gate: bad --upstream %q\n", *upstream)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	tokens := map[string]string{}
	for _, p := range []string{registrygate.PrincipalPlatform, registrygate.PrincipalBuild} {
		b, err := os.ReadFile(filepath.Join(*authDir, p))
		tok := strings.TrimSpace(string(b))
		if err != nil || tok == "" {
			log.Warn("registry principal disabled: no token", "principal", p, "dir", *authDir)
			continue
		}
		tokens[p] = tok
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           registrygate.New(target, tokens, log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("registry gate listening", "addr", *listen, "upstream", target.String(), "principals", len(tokens))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "felis registry-gate: %v\n", err)
		return 1
	}
	return 0
}

// cmdPushImage is the build Job's publish step. It runs after Kaniko built the
// image into a tarball (--no-push) and Trivy passed that tarball, and it is the
// only container of the build pod that holds the registry credential — the one
// executing the untrusted Dockerfile never sees it.
func cmdPushImage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("push-image", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tarPath := fs.String("tar", "", "image tarball Kaniko wrote with --tar-path")
	ref := fs.String("ref", "", "host/repository:tag to publish it as")
	scheme := fs.String("scheme", "http", "registry scheme: http for the in-cluster registry, https otherwise")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tarPath == "" || *ref == "" {
		fmt.Fprintln(stderr, "felis push-image: --tar and --ref are required")
		return 2
	}
	if *scheme != "http" && *scheme != "https" {
		fmt.Fprintf(stderr, "felis push-image: bad --scheme %q\n", *scheme)
		return 2
	}
	user := os.Getenv("FELIS_REGISTRY_USERNAME")
	pass := os.Getenv("FELIS_REGISTRY_PASSWORD")
	if user == "" || pass == "" {
		fmt.Fprintln(stderr, "felis push-image: FELIS_REGISTRY_USERNAME/FELIS_REGISTRY_PASSWORD are empty — the registry refuses anonymous writes")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p := &imagepush.Pusher{Scheme: *scheme, Username: user, Password: pass, Log: stderr}
	digest, err := p.Push(ctx, *tarPath, *ref)
	if err != nil {
		fmt.Fprintf(stderr, "felis push-image: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, digest)
	return 0
}
