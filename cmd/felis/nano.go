package main

// felis nano: the Felis-nano hasJoined multiplexer as a felis subcommand — the lightweight
// serve path for a third-party server operator who wants multi-Yggdrasil federation without a
// full Felis control plane (no k3s, no Postgres, no DB). It reads [[auth_source]] from
// felis.toml, leads with Mojang as the code-owned identity anchor (正版优先), and serves the
// vanilla sessionserver hasJoined endpoint. Point Velocity at it with
//   -Dmojang.sessionserver=http://127.0.0.1:8081/session/minecraft/hasJoined
// — Velocity's property takes the FULL endpoint URL, path included (its default is the
// full https://sessionserver.mojang.com/session/minecraft/hasJoined), and Velocity issues
// that request itself rather than through authlib. Then it verifies logins against Mojang
// plus every configured third-party source. Serving a proxy on another host means binding
// off-loopback with -listen; see the flag below for why that is an explicit opt-in.
//
// This is the no-database delivery of the identical brain `felis api` mounts through its
// route table (internal/api.HasJoinedHandler). The bootstrap installer's nano choice — its
// `[2] Felis-nano` prompt, or FELIS_INSTALL_MODE=nano — installs this as the
// felis-nano.service unit; here it just serves.
//
// There is deliberately no `felis setup --nano`. setup re-images the host through the full
// bootstrap TUI and carries no install-mode parameter anywhere (nothing in Go reads or sets
// FELIS_INSTALL_MODE), so such a flag would either re-run the installer — which is what
// pointing at the installer already does — or tear a full install down into a nano one,
// which is an uninstall, not a flag. This is the same reasoning that makes setup's --dev
// refuse and name the installer rather than pretend to choose a channel.

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

// nanoStubRepo satisfies api.Repo but implements only the one method handleHasJoined calls.
// The reclaim username blacklist is a felis-api/DB concern; a nano host has no Postgres, so
// nothing is barred here. A real blacklist would need the very DB nano exists to
// avoid — YAGNI until a nano host grows a reclaim store.
type nanoStubRepo struct{ api.Repo }

func (nanoStubRepo) IsUsernameBlacklisted(context.Context, string) (bool, error) { return false, nil }

// nanoLogURIMax is room for a real hasJoined query (a 16-character name, a 41-character
// serverId, an address) several times over.
const nanoLogURIMax = 256

func cmdNano(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nano", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml (reads [[auth_source]])")
	// Loopback default: hasJoined carries no auth token (authlib speaks the vanilla
	// sessionserver protocol), so a public bind is an open auth relay — anyone can point
	// their proxy at it and spend this host's egress IP on Mojang. A same-host Velocity
	// reaches 127.0.0.1; serving an off-host proxy is an explicit -listen opt-in.
	listen := fs.String("listen", "127.0.0.1:8081", "listen address for the hasJoined endpoint")
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

// serveNano serves until ctx ends, then drains. A restart, the documented way to pick up a
// config edit, sends SIGTERM; without the drain a login already waiting on an upstream has
// its connection reset, and Velocity tells that player the auth servers are down.
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
	// Log each request so a live login attempt is visible while testing against a real
	// Velocity — "is Velocity even reaching me?" is the first question during verification.
	// The URI is the caller's text: quoted so a control or bidi character cannot rewrite the
	// line and invalid UTF-8 cannot turn the journal entry into a blob, and capped so one
	// request cannot write a megabyte of log.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := r.RequestURI
		if len(uri) > nanoLogURIMax {
			uri = uri[:nanoLogURIMax] + "..."
		}
		fmt.Fprintf(stderr, "felis nano: %s %q\n", r.Method, uri)
		handler.ServeHTTP(w, r)
	})
}
