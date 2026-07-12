package main

// felis nano: the Felis-nano hasJoined multiplexer as a felis subcommand — the lightweight
// serve path for a third-party server operator who wants multi-Yggdrasil federation without a
// full Felis control plane (no k3s, no Postgres, no DB). It reads [[auth_source]] from
// felis.toml, leads with Mojang as the code-owned identity anchor (正版优先), and serves the
// vanilla sessionserver hasJoined endpoint. Point Velocity at it with
//   -Dmojang.sessionserver=http://<this-host>:8081/session/minecraft/hasJoined
// and authlib verifies logins against Mojang plus every configured third-party source.
//
// This is the no-database delivery of the identical brain `felis api` mounts through its
// route table (internal/api.HasJoinedHandler). `felis setup --nano` / the bootstrap nano
// choice install this as the runtime service; here it just serves.

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
	listen := fs.String("listen", ":8081", "listen address for the hasJoined endpoint")
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
