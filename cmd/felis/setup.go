package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/store"
)

// cmdSetup is the normal first-run operator console. It is intentionally separate
// from breakGlass: setup creates the initial Owner and optional web edge; breakGlass
// is reserved for emergency local recovery/reset.
func cmdSetup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis setup: refused — the setup console must run as root (try: sudo felis setup)")
		return 1
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis setup: %v\n", err)
		return 1
	}

	ctx := context.Background()
	drv, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintf(stderr, "felis setup: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	repo := api.NewPGRepo(drv.DB())
	adminExists, err := repo.AdminExists(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "felis setup: detect existing admin: %v\n", err)
		return 1
	}

	res, err := runSetupTUI(ctx, repo, cfg.Server.RootDomain, cfg.Auth.AdminHostname, cfg.Auth.PanelHostname, accountableOSUser(), adminExists)
	if err != nil {
		fmt.Fprintf(stderr, "felis setup: %v\n", err)
		return 1
	}

	if !res.provisioned && !res.edgeConfigured {
		fmt.Fprintln(stdout, "felis setup: cancelled — no changes made.")
		return 0
	}

	if res.provisioned {
		fmt.Fprintf(stdout, "\nfelis setup: Owner account %q provisioned; local-password login is ENABLED.\n", res.username)
		fmt.Fprintf(stdout, "Recorded as %q (mode: %s, os user: %s).\n", res.accountable, res.mode, res.osUser)
		if res.displayPassword != "" {
			fmt.Fprintf(stdout, "One-time password (you MUST change it on first login):\n\n    %s\n\n", res.displayPassword)
		} else {
			fmt.Fprintln(stdout, "Log in with the password you just entered (you MUST change it on first login).")
		}
		if res.auditWarning != "" {
			fmt.Fprintf(stdout, "WARNING: the accountability audit row was NOT written: %s\n", res.auditWarning)
		}
		if url := adminLoginURL(res.rootDomain, res.adminHostname); url != "" {
			fmt.Fprintf(stdout, "Log in at %s with that username and password.\n", url)
		}
	}

	if res.edgeConfigured {
		fmt.Fprintf(stdout, "\nfelis setup: Cloudflare Tunnel + Access edge configured.\n")
		if len(res.edgeRoutedHosts) > 0 {
			fmt.Fprintf(stdout, "Routed web hostnames: %s\n", strings.Join(res.edgeRoutedHosts, ", "))
		}
		if res.edgeConfigPath != "" {
			fmt.Fprintf(stdout, "Wrote tunnel config: %s\n", res.edgeConfigPath)
		}
		fmt.Fprintf(stdout, "\nACTION REQUIRED — make felis-api trust the edge:\n")
		fmt.Fprintf(stdout, "  in %s under [auth], set:\n", *cfgPath)
		if res.edgePanelHostname != "" {
			fmt.Fprintf(stdout, "    panel_hostname = %q\n", res.edgePanelHostname)
		}
		if res.edgeAdminHostname != "" {
			fmt.Fprintf(stdout, "    admin_hostname = %q\n", res.edgeAdminHostname)
		}
		fmt.Fprintf(stdout, "    access_jwt_aud = %q\n", res.edgeAud)
		fmt.Fprintln(stdout, "Then start the tunnel:  cloudflared tunnel run")
		fmt.Fprintln(stdout, "Verify the Access app actually guards the admin face before relying on it.")
	}
	return 0
}

func adminLoginURL(rootDomain, adminHostname string) string {
	if h := strings.TrimSpace(adminHostname); h != "" {
		return "https://" + h
	}
	if rootDomain != "" {
		return "https://op.console." + rootDomain
	}
	return ""
}
