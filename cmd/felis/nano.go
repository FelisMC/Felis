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
	"net/http"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
)

// nanoStubRepo satisfies api.Repo but implements only the one method handleHasJoined calls.
// The reclaim username blacklist is a felis-api/DB concern; a nano host has no Postgres, so
// nothing is barred here. ponytail: a real blacklist would need the very DB nano exists to
// avoid — YAGNI until a nano host grows a reclaim store.
type nanoStubRepo struct{ api.Repo }

func (nanoStubRepo) IsUsernameBlacklisted(context.Context, string) (bool, error) { return false, nil }

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

	handler := api.HasJoinedHandler(authSourcesFromConfig(cfg.AuthSources), nanoStubRepo{})
	fmt.Fprintf(stderr, "felis nano: hasJoined multiplexer on %s — Mojang + %d third-party source(s)\n", *listen, len(cfg.AuthSources))
	for i, s := range cfg.AuthSources {
		fmt.Fprintf(stderr, "  [%d] %s -> %s\n", i+1, s.Tag, s.URL)
	}

	// Log each request so a live login attempt is visible while testing against a real
	// Velocity — "is authlib even reaching me?" is the first question during verification.
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(stderr, "felis nano: %s %s\n", r.Method, r.RequestURI)
		handler.ServeHTTP(w, r)
	})

	srv := newAPIServer(*listen, logged)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(stderr, "felis nano:", err)
		return 1
	}
	return 0
}
