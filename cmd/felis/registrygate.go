package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
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
// Tokens are files under --auth-dir, one per principal (platform, build, prune),
// mounted from the registry-auth Secret. A missing file disables that principal:
// writes fail closed while every pull keeps working, which is the right way round
// for a registry the running workloads depend on.
//
// --maint-listen is the GC sidecar's read-only handshake (registrygate.MaintHandler).
// It has no authentication, so it must name a loopback address; --maint-dir keeps
// an open window across a gate restart.
func cmdRegistryGate(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("registry-gate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":5000", "address the gate serves the registry API on")
	upstream := fs.String("upstream", "http://127.0.0.1:5001", "the loopback registry the gate forwards to")
	authDir := fs.String("auth-dir", "/etc/felis-registry-auth", "directory holding one token file per principal")
	maintListen := fs.String("maint-listen", "", "loopback address for the GC sidecar's read-only handshake (empty disables it)")
	maintDir := fs.String("maint-dir", "", "directory that keeps an open read-only window across a gate restart")
	quiet := fs.Duration("maint-quiet", registrygate.DefaultQuiet, "how long writes must be idle before a read-only window is granted")
	dataDir := fs.String("data-dir", "", "the registry's storage root, mounted read-only, for the manifest index (empty disables it)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *maintListen != "" && !loopbackAddr(*maintListen) {
		fmt.Fprintf(stderr, "felis registry-gate: --maint-listen %q must be a loopback address: the handshake has no authentication\n", *maintListen)
		return 2
	}
	target, err := url.Parse(*upstream)
	if err != nil || target.Scheme == "" || target.Host == "" {
		fmt.Fprintf(stderr, "felis registry-gate: bad --upstream %q\n", *upstream)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	tokens := map[string]string{}
	for _, p := range registrygate.Principals {
		b, err := os.ReadFile(filepath.Join(*authDir, p))
		tok := strings.TrimSpace(string(b))
		if err != nil || tok == "" {
			log.Warn("registry principal disabled: no token", "principal", p, "dir", *authDir)
			continue
		}
		tokens[p] = tok
	}

	gate := registrygate.New(target, tokens, log)
	gate.SetQuiet(*quiet)
	gate.DataDir = *dataDir
	if *maintDir != "" {
		if err := gate.SetMaintenanceState(registrygate.MaintStatePath(*maintDir)); err != nil {
			// A corrupt file must not keep the registry from serving pulls.
			log.Warn("ignoring the saved read-only window", "err", err)
		}
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           gate,
		ReadHeaderTimeout: 10 * time.Second,
	}
	var maint *http.Server
	if *maintListen != "" {
		maint = &http.Server{Addr: *maintListen, Handler: gate.MaintHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := maint.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("maintenance listener stopped; garbage collection cannot get a read-only window", "err", err)
			}
		}()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		if maint != nil {
			_ = maint.Shutdown(shutdown)
		}
	}()
	log.Info("registry gate listening", "addr", *listen, "upstream", target.String(), "principals", len(tokens))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "felis registry-gate: %v\n", err)
		return 1
	}
	return 0
}

// loopbackAddr reports whether a host:port listen address binds loopback only.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
