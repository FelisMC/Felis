// Package cfsetup builds the recommended Cloudflare Tunnel and Access configuration for the breakGlass TUI.
package cfsetup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

const defaultPanelOrigin = "https://127.0.0.1:30443"

const defaultSessionDuration = "24h"

// catchAllService is the hostname-less final ingress rule cloudflared requires; it never forwards to the origin.
const catchAllService = "http_status:404"

var (
	ErrCloudflaredMissing = errors.New("cfsetup: cloudflared binary not found on PATH — install cloudflared first")
	ErrNotLoggedIn        = errors.New("cfsetup: not logged in to Cloudflare — run `cloudflared tunnel login` first (browser consent on your own account)")
	ErrNoAPIToken         = errors.New("cfsetup: a Cloudflare API token is required to configure Access")
)

// AccessIdentity requires an identity; emails/domains form include rules and login methods form require rules.
type AccessIdentity struct {
	Emails       []string
	EmailDomains []string
	LoginMethods []string
}

func (id AccessIdentity) empty() bool {
	return len(id.Emails) == 0 && len(id.EmailDomains) == 0 && len(id.LoginMethods) == 0
}

type accessRule map[string]map[string]any

// AccessPolicy is the Access policy request body; include, require and exclude combine as union, intersection and negation.
type AccessPolicy struct {
	Name     string       `json:"name"`
	Decision string       `json:"decision"`
	Include  []accessRule `json:"include"`
	Require  []accessRule `json:"require,omitempty"`
	Exclude  []accessRule `json:"exclude,omitempty"`
}

type AccessApplication struct {
	Name                    string   `json:"name"`
	Domain                  string   `json:"domain"`
	Type                    string   `json:"type"`
	SessionDuration         string   `json:"session_duration,omitempty"`
	AllowedIdPs             []string `json:"allowed_idps,omitempty"`
	AppLauncherVisible      bool     `json:"app_launcher_visible"`
	EnableBindingCookie     bool     `json:"enable_binding_cookie"`
	HTTPOnlyCookieAttribute bool     `json:"http_only_cookie_attribute"`
}

// BuildAccessApplication builds the Access application; empty allowedIdPs allows every configured IdP.
func BuildAccessApplication(domain, name string, sessionDuration string, allowedIdPs []string) AccessApplication {
	if sessionDuration == "" {
		sessionDuration = defaultSessionDuration
	}
	return AccessApplication{
		Name:                    name,
		Domain:                  domain,
		Type:                    "self_hosted",
		SessionDuration:         sessionDuration,
		AllowedIdPs:             allowedIdPs,
		AppLauncherVisible:      false,
		EnableBindingCookie:     true,
		HTTPOnlyCookieAttribute: true,
	}
}

// BuildRecommendedPolicy builds a fail-closed policy admitting exactly id; it errors instead of returning a public policy.
func BuildRecommendedPolicy(name string, id AccessIdentity) (AccessPolicy, error) {
	if id.empty() {
		return AccessPolicy{}, errors.New("cfsetup: recommended policy needs at least one identity (email, email domain, or SSO login method) — refusing to build a public policy")
	}
	p := AccessPolicy{Name: name, Decision: "allow"}
	for _, e := range id.Emails {
		p.Include = append(p.Include, accessRule{"email": {"email": e}})
	}
	for _, d := range id.EmailDomains {
		p.Include = append(p.Include, accessRule{"email_domain": {"domain": d}})
	}
	for _, m := range id.LoginMethods {
		p.Require = append(p.Require, accessRule{"login_method": {"id": m}})
	}
	// login-method-only identities need an "everyone" include base for the require to narrow
	if len(p.Include) == 0 {
		p.Include = append(p.Include, accessRule{"everyone": {}})
	}
	if err := validateFailClosed(p); err != nil {
		return AccessPolicy{}, err
	}
	return p, nil
}

// Policies must never be public: decision "allow" and any everyone include need a require clause.
func validateFailClosed(p AccessPolicy) error {
	switch p.Decision {
	case "bypass":
		return errors.New("cfsetup: refusing policy with decision \"bypass\" — it skips authentication for everyone (fail-open)")
	case "allow":
	default:
		return fmt.Errorf("cfsetup: refusing recommended policy with decision %q — must be \"allow\"", p.Decision)
	}
	if len(p.Include) == 0 {
		return errors.New("cfsetup: refusing policy with no include rules")
	}
	includeHasEveryone := false
	for _, r := range p.Include {
		scoped := false
		everyone := false
		for key := range r {
			switch key {
			case "email", "email_domain":
				scoped = true
			case "everyone":
				everyone = true
			default:
				return fmt.Errorf("cfsetup: refusing policy with unrecognized include rule %q — a fail-closed policy admits only scoped identities (email, email_domain) or an \"everyone\" base narrowed by a require", key)
			}
		}
		if everyone {
			includeHasEveryone = true
		}
		if !scoped && !everyone {
			return errors.New("cfsetup: refusing an include rule that is neither a scoped identity nor \"everyone\" (empty or malformed) — fail-closed")
		}
	}
	if !includeHasEveryone {
		return nil
	}
	requireConstrains := false
	for _, r := range p.Require {
		for key := range r {
			switch key {
			case "login_method", "email_domain", "email":
				requireConstrains = true
			}
		}
	}
	if !requireConstrains {
		return errors.New("cfsetup: refusing policy that admits \"everyone\" with no constraining require — that is public access (fail-open)")
	}
	return nil
}

// tunnelConfig is the cloudflared config.yml shape; the last ingress rule must be the catch-all.
type tunnelConfig struct {
	Tunnel          string        `json:"tunnel"`
	CredentialsFile string        `json:"credentials-file"`
	Ingress         []ingressRule `json:"ingress"`
}

// ingressRule is one ingress entry; an empty Hostname marks the catch-all.
type ingressRule struct {
	Hostname      string         `json:"hostname,omitempty"`
	Service       string         `json:"service"`
	OriginRequest *originRequest `json:"originRequest,omitempty"`
}

type originRequest struct {
	NoTLSVerify bool `json:"noTLSVerify,omitempty"`
}

// BuildTunnelConfig renders config.yml routing each web hostname to the panel origin; the game host stays off the tunnel.
func BuildTunnelConfig(tunnelID, credentialsFile, panelOrigin string, hostnames []string) ([]byte, error) {
	if tunnelID == "" {
		return nil, errors.New("cfsetup: tunnel id is required")
	}
	if panelOrigin == "" {
		panelOrigin = defaultPanelOrigin
	}
	if len(hostnames) == 0 {
		return nil, errors.New("cfsetup: at least one web hostname is required")
	}
	cfg := tunnelConfig{Tunnel: tunnelID, CredentialsFile: credentialsFile}
	seen := map[string]struct{}{}
	for _, h := range hostnames {
		if h == "" {
			return nil, errors.New("cfsetup: empty hostname in ingress")
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		rule := ingressRule{Hostname: h, Service: panelOrigin}
		if strings.HasPrefix(panelOrigin, "https://") {
			rule.OriginRequest = &originRequest{NoTLSVerify: true}
		}
		cfg.Ingress = append(cfg.Ingress, rule)
	}
	cfg.Ingress = append(cfg.Ingress, ingressRule{Service: catchAllService})
	return yaml.Marshal(cfg)
}

type Preconditions struct {
	CloudflaredPath string // empty when cloudflared is not found
	CertExists      bool   // set after `cloudflared tunnel login`; checks ~/.cloudflared/cert.pem
	APIToken        string
}

func (p Preconditions) check() error {
	if p.CloudflaredPath == "" {
		return ErrCloudflaredMissing
	}
	if !p.CertExists {
		return ErrNotLoggedIn
	}
	if strings.TrimSpace(p.APIToken) == "" {
		return ErrNoAPIToken
	}
	return nil
}

type apiTokenVerifier interface {
	VerifyAPIToken(ctx context.Context) error
}

type Runner interface {
	// CreateTunnel is idempotent: it returns the existing tunnel when the name is taken.
	CreateTunnel(ctx context.Context, name string) (id, credentialsFile string, err error)
	RouteDNS(ctx context.Context, tunnelID, hostname string) error
	WriteTunnelConfig(path string, contents []byte) error
	// CreateAccessApplication returns the app id and the JWT aud that [auth] access_jwt_aud must adopt.
	CreateAccessApplication(ctx context.Context, app AccessApplication) (appID, aud string, err error)
	CreateAccessPolicy(ctx context.Context, appID string, policy AccessPolicy) error
}

type Params struct {
	PanelHostname   string
	AdminHostname   string
	PanelOrigin     string // where the tunnel forwards; default HTTPS NodePort origin
	TunnelName      string
	ConfigPath      string
	SessionDuration string
	AllowedIdPs     []string // Restrict the Access application to these IdPs.
	AccessIdentity  AccessIdentity
	Pre             Preconditions
	OnProgress      func(string)
}

// Result reports what Setup produced; the caller records AccessAud in [auth] access_jwt_aud.
type Result struct {
	TunnelID        string
	CredentialsFile string
	ConfigPath      string
	AccessAppID     string
	AccessAud       string
	RoutedHostnames []string
	Progress        []string
}

// Setup provisions the tunnel and Access app; preconditions and the policy guard run before any Runner call.
func Setup(ctx context.Context, runner Runner, p Params) (*Result, error) {
	if runner == nil {
		return nil, errors.New("cfsetup: runner is required")
	}
	if p.AdminHostname == "" {
		return nil, errors.New("cfsetup: admin hostname is required")
	}
	if p.TunnelName == "" {
		return nil, errors.New("cfsetup: tunnel name is required")
	}
	notify := func(s string) {
		if p.OnProgress != nil {
			p.OnProgress(s)
		}
	}
	var prog []string
	if err := p.Pre.check(); err != nil {
		return nil, err
	}
	policy, err := BuildRecommendedPolicy("felis-recommended", p.AccessIdentity)
	if err != nil {
		return nil, err
	}
	if err := validateFailClosed(policy); err != nil {
		return nil, err
	}
	if verifier, ok := runner.(apiTokenVerifier); ok {
		notify("Verifying API token…")
		if err := verifier.VerifyAPIToken(ctx); err != nil {
			return nil, fmt.Errorf("cfsetup: verify Cloudflare API token: %w", err)
		}
		prog = append(prog, "Verified API token")
	}

	hostnames := webHostnames(p)
	origin := p.PanelOrigin
	if origin == "" {
		origin = defaultPanelOrigin
	}

	notify("Creating tunnel " + p.TunnelName + "…")
	id, cred, err := runner.CreateTunnel(ctx, p.TunnelName)
	if err != nil {
		return nil, fmt.Errorf("cfsetup: create tunnel: %w", err)
	}
	prog = append(prog, "Created tunnel")

	for _, h := range hostnames {
		notify("Routing DNS " + h + "…")
		if err := runner.RouteDNS(ctx, id, h); err != nil {
			return nil, fmt.Errorf("cfsetup: route dns %s: %w", h, err)
		}
		prog = append(prog, "Routed "+h)
	}
	notify("Writing tunnel config…")
	cfgBytes, err := BuildTunnelConfig(id, cred, origin, hostnames)
	if err != nil {
		return nil, err
	}
	if p.ConfigPath != "" {
		if err := runner.WriteTunnelConfig(p.ConfigPath, cfgBytes); err != nil {
			return nil, fmt.Errorf("cfsetup: write config: %w", err)
		}
		prog = append(prog, "Wrote "+p.ConfigPath)
		// Setup does not start a connector; callers must run one or Cloudflare returns error 1033.
	}
	notify("Creating Access application…")
	app := BuildAccessApplication(p.AdminHostname, "Felis SysAdmin Console", p.SessionDuration, p.AllowedIdPs)
	appID, aud, err := runner.CreateAccessApplication(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("cfsetup: create access application: %w", err)
	}
	prog = append(prog, "Created Access app")
	notify("Attaching Access policy…")
	if err := runner.CreateAccessPolicy(ctx, appID, policy); err != nil {
		return nil, fmt.Errorf("cfsetup: create access policy: %w", err)
	}
	prog = append(prog, "Attached Access policy")

	return &Result{
		TunnelID:        id,
		CredentialsFile: cred,
		ConfigPath:      p.ConfigPath,
		AccessAppID:     appID,
		AccessAud:       aud,
		RoutedHostnames: hostnames,
		Progress:        prog,
	}, nil
}

func webHostnames(p Params) []string {
	var hs []string
	if p.PanelHostname != "" {
		hs = append(hs, p.PanelHostname)
	}
	hs = append(hs, p.AdminHostname)
	return hs
}
