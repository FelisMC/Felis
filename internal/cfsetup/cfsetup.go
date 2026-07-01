// Package cfsetup builds the RECOMMENDED, one-click Cloudflare Tunnel + Access
// configuration the felis breakGlass TUI can offer a SysAdmin (spec §14 Zero
// Trust edge). It is deliberately "锦上添花" — icing, not a mandate: the platform
// is domain-agnostic (every FQDN is composed from the configured root_domain) and
// IdP-agnostic (felis-api validates ANY valid Cloudflare Access JWT `aud`, no
// matter which identity provider — Google Workspace, Keycloak, Microsoft Entra —
// fronts it). A SysAdmin who brings their own domain or a different Zero-Trust
// scheme is fully supported; this package only makes the common case easy.
//
// The split is honest about what this box can verify:
//
//   - PURE + UNIT-VERIFIED here: the ingress-config generation, the Access
//     application/policy request bodies, the gating preconditions, and — the one
//     load-bearing safety property — the FAIL-CLOSED guard on the recommended
//     policy (it must never serialize to public/allow-everyone or skip auth).
//   - INTEGRATION-ONLY (see runner.go): actually creating the tunnel, routing
//     DNS, and POSTing the Access app/policy. Those require the operator's OWN
//     live Cloudflare account and the interactive `cloudflared tunnel login`
//     browser consent, which this package can neither perform nor fake.
//
// Setup wires the two together behind a Runner interface so the orchestration is
// testable with a fake while the real exec/HTTP impl stays integration-only.
package cfsetup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// defaultPanelOrigin is where the tunnel forwards the web hostnames when the
// caller does not override it: the local HTTPS NodePort exposed by bootstrap.
const defaultPanelOrigin = "https://127.0.0.1:30443"

// defaultSessionDuration is the recommended Access session length when unset.
const defaultSessionDuration = "24h"

// catchAllService is the cloudflared sentinel that returns a bare 404 for any
// hostname not explicitly routed. cloudflared REQUIRES the final ingress rule to
// be a hostname-less catch-all; we always make it this fail-shut 404 so the
// tunnel never forwards an unexpected Host to the origin.
const catchAllService = "http_status:404"

// Gating errors — Setup refuses (with NO side effects) unless every precondition
// the operator alone can satisfy is met. The TUI surfaces these as remediation.
var (
	// ErrCloudflaredMissing means the cloudflared binary is not on PATH.
	ErrCloudflaredMissing = errors.New("cfsetup: cloudflared binary not found on PATH — install cloudflared first")
	// ErrNotLoggedIn means ~/.cloudflared/cert.pem is absent: the operator has
	// not run `cloudflared tunnel login`. That step is an interactive browser
	// consent against the operator's OWN Cloudflare account; the TUI cannot and
	// must not bypass it.
	ErrNotLoggedIn = errors.New("cfsetup: not logged in to Cloudflare — run `cloudflared tunnel login` first (browser consent on your own account)")
	// ErrNoAPIToken means no Cloudflare API token was supplied for the Access
	// application/policy calls.
	ErrNoAPIToken = errors.New("cfsetup: a Cloudflare API token is required to configure Access")
)

// AccessIdentity scopes WHO the recommended Access policy admits. At least one
// field must be set — an empty identity is refused as fail-open. The three
// dimensions map straight onto Cloudflare Access rule types and cover the
// SSO-provider case the SysAdmin may want (Google Workspace / Keycloak / Entra
// are registered in Access as IdPs; LoginMethods names their IdP ids):
//
//   - Emails       → include {email:{email}}          (specific people)
//   - EmailDomains → include {email_domain:{domain}}  (an org's SSO domain)
//   - LoginMethods → require {login_method:{id}}       (only this IdP/SSO)
type AccessIdentity struct {
	Emails       []string
	EmailDomains []string
	LoginMethods []string
}

func (id AccessIdentity) empty() bool {
	return len(id.Emails) == 0 && len(id.EmailDomains) == 0 && len(id.LoginMethods) == 0
}

// accessRule is one Cloudflare Access rule object, e.g. {"email":{"email":...}}
// or {"everyone":{}}. Modeled as a map so the include/require/exclude arrays
// serialize to exactly the shapes the Access API expects.
type accessRule map[string]map[string]any

// AccessPolicy is the request body for an Access policy (decision + the
// include/require/exclude rule arrays, which compose as OR / AND / NOT).
type AccessPolicy struct {
	Name     string       `json:"name"`
	Decision string       `json:"decision"`
	Include  []accessRule `json:"include"`
	Require  []accessRule `json:"require,omitempty"`
	Exclude  []accessRule `json:"exclude,omitempty"`
}

// AccessApplication is the request body for a self-hosted Access application
// fronting one web hostname (spec §14: the op.console SysAdmin face). The cookie
// hardening defaults are on because this guards the most privileged surface.
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

// BuildAccessApplication assembles a self-hosted Access application for one
// hostname. allowedIdPs, when non-empty, restricts which configured IdPs (the
// SysAdmin's chosen SSO) may satisfy the app — left empty, Access offers all
// configured IdPs.
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

// BuildRecommendedPolicy assembles the recommended fail-closed Access policy that
// admits exactly the given identity. It returns an error rather than emit a
// policy that would be public — an empty identity, or any result that does not
// pass validateFailClosed, is refused here so a caller can never accidentally
// ship an open door. Scoping by LoginMethods alone yields "everyone who
// authenticates via this SSO IdP" (include everyone + require login_method),
// which is constrained, not public.
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
	// If the identity is scoped ONLY by SSO login method, the include set needs a
	// base match for the require to narrow; "everyone gated by require login_method"
	// is the Cloudflare-recommended authenticated-users shape and stays fail-closed.
	if len(p.Include) == 0 {
		p.Include = append(p.Include, accessRule{"everyone": {}})
	}
	if err := validateFailClosed(p); err != nil {
		return AccessPolicy{}, err
	}
	return p, nil
}

// validateFailClosed is the load-bearing safety property of this whole package,
// the analog of "never catches the genuine Mojang player" in the reclaim flow: a
// recommended Access policy that guards op.console MUST NOT be public.
//
// It is an ALLOWLIST, not a denylist — the only fail-closed design for a guard
// whose whole job is to catch shapes the current builder does not produce. The
// critical fact is that Cloudflare Access `include` rules combine as OR: a user is
// admitted if they match ANY one include rule. So an `everyone` include makes the
// whole include set public no matter what other identity rules sit beside it (the
// identity rule is pure redundancy in an OR), and only a `require` clause — which
// is AND — can narrow an `everyone` base. A denylist of known-bad shapes would
// miss both everyone-OR-identity and unrecognized public includes (e.g. an
// ip:0.0.0.0/0 rule); the allowlist refuses anything it cannot positively
// recognize as scoped.
//
// A policy passes iff: decision is "allow" (not "bypass", which skips auth, nor
// anything else); the include set is non-empty; and EITHER every include rule is a
// recognized scoped identity (email, email_domain) with no `everyone`, OR the
// include set's `everyone` base is narrowed by a constraining require clause
// (login_method / email_domain / email). Setup runs this before the policy is ever
// POSTed, so even a future builder bug cannot open the door.
func validateFailClosed(p AccessPolicy) error {
	switch p.Decision {
	case "bypass":
		return errors.New("cfsetup: refusing policy with decision \"bypass\" — it skips authentication for everyone (fail-open)")
	case "allow":
		// the only decision the recommended path emits
	default:
		return fmt.Errorf("cfsetup: refusing recommended policy with decision %q — must be \"allow\"", p.Decision)
	}
	if len(p.Include) == 0 {
		return errors.New("cfsetup: refusing policy with no include rules")
	}
	// Classify the include set against the allowlist. Each rule must positively
	// resolve to a recognized scoped identity or the `everyone` base; anything
	// else — an unrecognized type OR a degenerate empty/malformed rule with no
	// recognized key — is treated as potentially-public and refused.
	includeHasEveryone := false
	for _, r := range p.Include {
		scoped := false
		everyone := false
		for key := range r {
			switch key {
			case "email", "email_domain":
				scoped = true // a scoped identity — safe to OR into the include set
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
		// Every include rule is a recognized scoped identity; the OR of scoped
		// identities is itself scoped. Fail-closed.
		return nil
	}
	// The include set admits everyone; only a constraining require (AND) can save
	// it. A require of `everyone` (or any unrecognized type) does not narrow.
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

// tunnelConfig is the cloudflared config.yml shape: the tunnel UUID, its
// credentials file, and the ordered ingress rules (the last of which MUST be the
// hostname-less catch-all).
type tunnelConfig struct {
	Tunnel          string        `json:"tunnel"`
	CredentialsFile string        `json:"credentials-file"`
	Ingress         []ingressRule `json:"ingress"`
}

// ingressRule is one cloudflared ingress entry. A rule with an empty Hostname is
// the catch-all (must be last).
type ingressRule struct {
	Hostname      string         `json:"hostname,omitempty"`
	Service       string         `json:"service"`
	OriginRequest *originRequest `json:"originRequest,omitempty"`
}

type originRequest struct {
	NoTLSVerify bool `json:"noTLSVerify,omitempty"`
}

// BuildTunnelConfig renders the cloudflared config.yml that routes each web
// hostname to the local panel origin and terminates in the required fail-shut
// catch-all 404. The game host (the bare root_domain / mc. host) is deliberately
// NOT a hostname here — Minecraft stays raw protocol off the tunnel; only the
// passed web hostnames (console., op.console.) are proxied.
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
	// The mandatory trailing catch-all: anything not explicitly routed gets a bare
	// 404, never a forward to the origin.
	cfg.Ingress = append(cfg.Ingress, ingressRule{Service: catchAllService})
	return yaml.Marshal(cfg)
}

// Preconditions are the gating facts only the operator can satisfy, detected off
// the box (see DetectPreconditions in runner.go) and passed in as data so Setup
// stays unit-testable.
type Preconditions struct {
	// CloudflaredPath is the resolved cloudflared binary path; empty = not found.
	CloudflaredPath string
	// CertExists reports whether ~/.cloudflared/cert.pem is present, i.e. the
	// operator has completed `cloudflared tunnel login`.
	CertExists bool
	// APIToken is the Cloudflare API token for the Access app/policy calls.
	APIToken string
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

// Runner is the integration seam: every side-effecting step of the setup. The
// real implementation (ExecRunner in runner.go) shells out to cloudflared and
// calls the Cloudflare API and is INTEGRATION-ONLY; tests pass a fake.
type apiTokenVerifier interface {
	VerifyAPIToken(ctx context.Context) error
}

type Runner interface {
	// CreateTunnel creates (or, idempotently, returns the existing) named tunnel,
	// yielding its UUID and the path to its credentials file.
	CreateTunnel(ctx context.Context, name string) (id, credentialsFile string, err error)
	// RouteDNS points hostname at the tunnel (a proxied CNAME).
	RouteDNS(ctx context.Context, tunnelID, hostname string) error
	// WriteTunnelConfig persists the rendered config.yml.
	WriteTunnelConfig(path string, contents []byte) error
	// CreateAccessApplication creates the self-hosted Access app and returns its
	// id and the issued JWT `aud` (which felis [auth] access_jwt_aud must adopt).
	CreateAccessApplication(ctx context.Context, app AccessApplication) (appID, aud string, err error)
	// CreateAccessPolicy attaches policy to the Access app.
	CreateAccessPolicy(ctx context.Context, appID string, policy AccessPolicy) error
}

// Params is the full input to Setup. Hostnames are passed in (composed by the
// caller from the configured root_domain) so this package never hardcodes a
// domain or subdomain scheme.
type Params struct {
	PanelHostname   string // console.<root_domain> (Player web)
	AdminHostname   string // op.console.<root_domain> (Operator+SysAdmin web)
	PanelOrigin     string // where the tunnel forwards; default HTTPS NodePort origin
	TunnelName      string
	ConfigPath      string // where to write config.yml
	SessionDuration string
	AllowedIdPs     []string       // restrict the Access app to these IdPs (SSO)
	AccessIdentity  AccessIdentity // WHO the policy admits (fail-closed)
	Pre             Preconditions
	OnProgress      func(string) // optional, called at each step for TUI display
}

// Result reports what Setup produced, including the Access `aud` the caller must
// write into felis [auth] access_jwt_aud to make felis-api accept the new edge.
type Result struct {
	TunnelID        string
	CredentialsFile string
	ConfigPath      string
	AccessAppID     string
	AccessAud       string
	RoutedHostnames []string
	Progress        []string // ordered steps completed, for TUI display
}

// Setup runs the recommended Cloudflare Tunnel + Access provisioning end to end
// behind the Runner. It is ordered so that EVERY check the box can make happens
// BEFORE any side effect: it gates on preconditions and builds + fail-closed-
// guards the policy first, returning early with no Runner calls if either fails.
// Only then does it create the tunnel, route the web hostnames (never the game
// host), write the config, create the Access app for the admin face, and attach
// the guarded policy.
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
	// 1. Gate on operator-only preconditions — no side effects on failure.
	if err := p.Pre.check(); err != nil {
		return nil, err
	}
	// 2. Build and fail-closed-guard the policy BEFORE touching Cloudflare, so a
	//    public/unscoped policy aborts the whole run with nothing created.
	policy, err := BuildRecommendedPolicy("felis-recommended", p.AccessIdentity)
	if err != nil {
		return nil, err
	}
	if err := validateFailClosed(policy); err != nil {
		return nil, err
	}
	// 3. When the real runner can verify the token, do that read-only Cloudflare API
	//    check before creating tunnels or DNS records.
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

	// 4. Create the tunnel.
	notify("Creating tunnel " + p.TunnelName + "…")
	id, cred, err := runner.CreateTunnel(ctx, p.TunnelName)
	if err != nil {
		return nil, fmt.Errorf("cfsetup: create tunnel: %w", err)
	}
	prog = append(prog, "Created tunnel")

	// 5. Route DNS for each WEB hostname only.
	for _, h := range hostnames {
		notify("Routing DNS " + h + "…")
		if err := runner.RouteDNS(ctx, id, h); err != nil {
			return nil, fmt.Errorf("cfsetup: route dns %s: %w", h, err)
		}
		prog = append(prog, "Routed "+h)
	}
	// 6. Render and persist the ingress config.
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
		// Setup stops at writing the config: RUNNING a connector for it is a
		// host-specific side effect (systemd/launchd/Windows service) that lives
		// with the caller, not in this host-agnostic package. The felis TUI does it
		// right after Setup returns (installCloudflaredService in tui_edge_apply.go),
		// closing the routed-but-dead 1033 the same way RouteDNS's --overwrite-dns
		// closes the stale-DNS 1033. A caller that skips that step gets a routed
		// tunnel with no connector — Cloudflare error 1033 — by its own choice.
	}
	// 7. Front the admin face with a self-hosted Access app.
	notify("Creating Access application…")
	app := BuildAccessApplication(p.AdminHostname, "Felis SysAdmin Console", p.SessionDuration, p.AllowedIdPs)
	appID, aud, err := runner.CreateAccessApplication(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("cfsetup: create access application: %w", err)
	}
	prog = append(prog, "Created Access app")
	// 8. Attach the guarded fail-closed policy.
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

// webHostnames returns the web hostnames to route, in order: panel (console.)
// first when present, then the admin (op.console.) face. The admin host is
// required; the panel host is optional (a SysAdmin may tunnel only the admin
// surface).
func webHostnames(p Params) []string {
	var hs []string
	if p.PanelHostname != "" {
		hs = append(hs, p.PanelHostname)
	}
	hs = append(hs, p.AdminHostname)
	return hs
}
