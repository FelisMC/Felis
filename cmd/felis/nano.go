package main

// Serve hasJoined without a database; Velocity requires the complete endpoint URL.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
)

// nanoStubRepo satisfies api.Repo with only the method handleHasJoined calls; with no Postgres, nothing is barred here (the reclaim blacklist needs the database).
type nanoStubRepo struct{ api.Repo }

func (nanoStubRepo) IsUsernameBlacklisted(context.Context, string) (bool, error) { return false, nil }

// Bind loopback by default because hasJoined has no authentication token.
const nanoDefaultListen = "127.0.0.1:8081"

// nanoLogURIMax is room for a real hasJoined query (a 16-character name, a 41-character
// serverId, an address) several times over.
const nanoLogURIMax = 256

func cmdNano(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nano", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml (reads [[auth_source]])")
	listen := fs.String("listen", nanoDefaultListen, "listen address for the hasJoined endpoint")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.LoadNano(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "felis nano:", err)
		return 1
	}
	// [server] listen belongs to felis api. Someone moving nano off loopback naturally reaches
	// for it, and without this line would get connection refused with no hint why.
	if cfg.Server.Listen != "" {
		fmt.Fprintf(stderr, "felis nano: [server] listen = %q is ignored; nano binds -listen (%s), which the installer sets from FELIS_NANO_LISTEN\n", cfg.Server.Listen, *listen)
	}

	fmt.Fprintf(stderr, "felis nano: hasJoined multiplexer on %s — Mojang + %d third-party source(s)\n", *listen, len(cfg.AuthSources))
	for i, s := range cfg.AuthSources {
		fmt.Fprintf(stderr, "  [%d] %s -> %s\n", i+1, s.Tag, s.URL)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "felis nano:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveNano(ctx, newAPIServer(*listen, nanoHandler(cfg.AuthSources, stderr)), ln, stderr)
}

// nanoDrainTimeout outlasts the source scan of any realistic list (each source is given
// five seconds) and stays well inside systemd's default 90-second stop timeout.
const nanoDrainTimeout = 30 * time.Second

// Drain active logins on SIGTERM so configuration restarts do not reset upstream authentication calls.
func serveNano(ctx context.Context, srv *http.Server, ln net.Listener, stderr io.Writer) int {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		fmt.Fprintln(stderr, "felis nano:", err)
		return 1
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), nanoDrainTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintln(stderr, "felis nano: shutdown:", err)
			return 1
		}
		return 0
	}
}

// nanoHandler is what felis nano serves: the shared hasJoined handler, Mojang first, behind
// a request log.
func nanoHandler(sources []config.AuthSourceConfig, stderr io.Writer) http.Handler {
	handler := api.HasJoinedHandler(authSourcesFromConfig(sources), nanoStubRepo{})
	// Quote and truncate caller-controlled URIs to prevent log injection and oversized entries.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := r.RequestURI
		if len(uri) > nanoLogURIMax {
			uri = uri[:nanoLogURIMax] + "..."
		}
		fmt.Fprintf(stderr, "felis nano: %s %q\n", r.Method, uri)
		handler.ServeHTTP(w, r)
	})
}
