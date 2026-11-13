package main

// Felis-nano is login control for a network Felis does not run: hasJoined over Mojang and
// the auth sources, sign-in, accounts and the proxy plugin's calls, kept in PostgreSQL.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mailsettings"
	"felis.lolicon.best/internal/panel"
	"felis.lolicon.best/internal/passkey"
	"felis.lolicon.best/internal/retention"
)

// Bind loopback by default because hasJoined has no authentication token.
const nanoDefaultListen = "127.0.0.1:8081"

// nanoDefaultPanelPort serves the panel over HTTPS; the installer opens it in the host firewall.
const nanoDefaultPanelPort = 8443

func cmdNano(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "setup" {
		return cmdNanoSetup(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("nano", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	listen := fs.String("listen", nanoDefaultListen, "address for hasJoined and the proxy plugin; plain HTTP, so keep it on the proxy's network")
	panelPort := fs.Int("panel-port", nanoDefaultPanelPort, "HTTPS port the panel listens on")
	publicPort := fs.Int("public-port", 0, "port in the panel's public address, e.g. 443 behind a reverse proxy (default: -panel-port)")
	tlsCert := fs.String("tls-cert", "/etc/felis/panel-tls.crt", "panel TLS certificate")
	tlsKey := fs.String("tls-key", "/etc/felis/panel-tls.key", "panel TLS private key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *publicPort == 0 {
		*publicPort = *panelPort
	}
	cfg, err := loadNanoConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "felis nano:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	drv, err := openStore(ctx, cfg.Database.URL, false)
	if err != nil {
		fmt.Fprintln(stderr, "felis nano:", err)
		return 1
	}
	defer drv.Close()
	a, err := nanoAPI(cfg, api.NewPGRepo(drv.DB()), *publicPort, func(ctx context.Context, since, until time.Time, w io.Writer) (int, error) {
		return retention.ExportAudit(ctx, drv.DB(), since, until, w)
	}, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "felis nano:", err)
		return 1
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "felis nano:", err)
		return 1
	}
	panelLn, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(*panelPort)))
	if err != nil {
		ln.Close()
		fmt.Fprintln(stderr, "felis nano:", err)
		return 1
	}
	panelSrv := newAPIServer("", panel.Handler(a.ExternalHandler(), cfg.Server.RootDomain,
		defaultPanelHostname(cfg.Server.RootDomain, cfg.Auth.PanelHostname),
		defaultAdminHostname(cfg.Server.RootDomain, cfg.Auth.AdminHostname),
		cfg.Velocity.GamePort, cfg.Velocity.GameVersion, resolvedVersion(), panel.Topology{Nano: true}))
	fmt.Fprintf(stderr, "felis nano: hasJoined and the proxy plugin on %s; panel on port %d, operators sign in at %s\n",
		*listen, *panelPort, httpsOrigin(defaultAdminHostname(cfg.Server.RootDomain, cfg.Auth.AdminHostname), *publicPort))
	return serveNano(ctx, stderr,
		nanoListener{newAPIServer("", a.InternalHandler()), ln, nil},
		nanoListener{panelSrv, panelLn, &tlsFiles{*tlsCert, *tlsKey}})
}

// loadNanoConfig reads felis.toml, telling a host still on the database-free nano how to move on.
func loadNanoConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if raw, rerr := config.LoadNano(path); rerr == nil && raw.Database.URL == "" {
			return nil, fmt.Errorf("%s has no [database]: Felis-nano keeps accounts in PostgreSQL now; rerun the installer with FELIS_INSTALL_MODE=nano to set it up (your [[auth_source]] entries carry over)", path)
		}
		return nil, err
	}
	return cfg, nil
}

// nanoAPI is the login-control API: no cluster, console, builds or backups, so it mounts only api.nanoRoutes.
func nanoAPI(cfg *config.Config, repo api.Repo, publicPort int, auditExport func(context.Context, time.Time, time.Time, io.Writer) (int, error), stderr io.Writer) (*api.API, error) {
	internal, err := internalCallerTokens(os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("proxy plugin token: %w", err)
	}
	if internal["velocity"] == "" {
		fmt.Fprintln(stderr, "felis nano: FELIS_SERVICE_TOKEN is not set, so the proxy plugin's /link, /felis approve and /felis migrate calls are refused")
	}
	mailer, smtpPassword := hostMailer(cfg.SMTP, "felis nano", stderr)
	a := &api.API{
		Nano:     true,
		Repo:     repo,
		Internal: internal,
		Mailer:   mailer,
		External: api.SessionAuth{
			Repo:          repo,
			RootDomain:    cfg.Server.RootDomain,
			AdminHostname: cfg.Auth.AdminHostname,
		},
		RootDomain:     cfg.Server.RootDomain,
		AdminHostname:  cfg.Auth.AdminHostname,
		PanelHostname:  cfg.Auth.PanelHostname,
		PanelPort:      publicPort,
		AuditExport:    auditExport,
		AuthDoorLimit:  api.RateLimit{Burst: 20, PerMinute: 20},
		ClientIPHeader: cfg.Auth.EffectiveClientIPHeader(),
		MailLimit:      api.MailRateLimit(cfg.SMTP.MaxPerHour),
	}
	a.AuthSources = authSourcesFromConfig(cfg.AuthSources)
	a.AuthSourceSettings = &api.AuthSourceSettings{Repo: repo, Defaults: a.AuthSources}
	a.MailSettings = &api.MailSettings{Repo: repo, Host: hostMailSettings(cfg.SMTP, smtpPassword), NewMailer: func(s mailsettings.Settings) api.OTPMailer { return s.SMTP() }}
	if rpID, origins := passkeyRelyingParty(cfg, publicPort); rpID == "" {
		fmt.Fprintln(stderr, "felis nano: passkey verifier disabled (no panel host: set server.root_domain or auth.panel_hostname) — passkey endpoints return 503")
	} else if pv, err := passkey.New(rpID, "Felis", origins); err != nil {
		fmt.Fprintf(stderr, "felis nano: passkey verifier disabled: %v — passkey endpoints return 503\n", err)
	} else {
		a.Passkey = pv
	}
	return a, nil
}

func httpsOrigin(host string, port int) string {
	if port == 443 {
		return "https://" + host
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port))
}

type tlsFiles struct{ cert, key string }

type nanoListener struct {
	srv *http.Server
	ln  net.Listener
	tls *tlsFiles
}

// nanoDrainTimeout outlasts the source scan of any realistic list (each source is given
// five seconds) and stays well inside systemd's default 90-second stop timeout.
const nanoDrainTimeout = 30 * time.Second

// serveNano drains in-flight logins on SIGTERM so a restart does not cut an upstream authentication call short.
func serveNano(ctx context.Context, stderr io.Writer, listeners ...nanoListener) int {
	errc := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() {
			if l.tls != nil {
				errc <- l.srv.ServeTLS(l.ln, l.tls.cert, l.tls.key)
				return
			}
			errc <- l.srv.Serve(l.ln)
		}()
	}
	rc := 0
	select {
	case err := <-errc:
		fmt.Fprintln(stderr, "felis nano:", err)
		rc = 1
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), nanoDrainTimeout)
	defer cancel()
	for _, l := range listeners {
		if err := l.srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "felis nano: shutdown:", err)
			rc = 1
		}
	}
	return rc
}

// cmdNanoSetup creates or resumes the Owner and prints the one-time link that opens the panel to them.
func cmdNanoSetup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nano setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	publicPort := fs.Int("public-port", nanoDefaultPanelPort, "port in the panel's public address (443 behind a reverse proxy)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis nano setup: refused — run it as root (try: sudo felis nano setup)")
		return 1
	}
	cfg, err := loadNanoConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "felis nano setup:", err)
		return 1
	}
	admin := defaultAdminHostname(cfg.Server.RootDomain, cfg.Auth.AdminHostname)
	if admin == "" {
		fmt.Fprintln(stderr, "felis nano setup: no operator console host; set [server] root_domain or [auth] admin_hostname")
		return 1
	}
	ctx := context.Background()
	drv, err := openStore(ctx, cfg.Database.URL, false)
	if err != nil {
		fmt.Fprintln(stderr, "felis nano setup:", err)
		return 1
	}
	defer drv.Close()
	out, err := performSetupOwner(ctx, api.NewPGRepo(drv.DB()), httpsOrigin(admin, *publicPort), accountableOSUser())
	if errors.Is(err, api.ErrConflict) {
		fmt.Fprintf(stdout, "The Owner %s has already signed in; sign in at %s\n", out.ownerUsername, httpsOrigin(admin, *publicPort))
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "felis nano setup:", err)
		return 1
	}
	if out.auditErr != nil {
		fmt.Fprintf(stderr, "felis nano setup: warning: the audit row was not written: %v\n", out.auditErr)
	}
	fmt.Fprintf(stdout, "Open this link within %s to finish the Owner's sign-in (record an email or create a passkey):\n  %s\n", setupTokenTTL, out.setupTokenURL)
	return 0
}
