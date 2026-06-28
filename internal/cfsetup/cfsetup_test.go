package cfsetup

import (
	"context"
	"errors"
	"testing"

	"sigs.k8s.io/yaml"
)

// The Cloudflare Tunnel + Access setup is INTEGRATION-ONLY end to end: the tunnel,
// DNS, and Access resources only exist against the operator's live account. These
// tests pin the two things this box CAN verify and that actually protect the
// operator:
//
//  1. the recommended Access policy is FAIL-CLOSED — it can never serialize to
//     public/allow-everyone or skip authentication (the load-bearing property,
//     the analog of "reclaim never catches the genuine Mojang player"); and
//  2. the gating refuses with NO side effects when the operator's own
//     preconditions (cloudflared, login, API token) are not met.
//
// We deliberately do NOT assert "Setup calls the runner in this exact order" —
// that only restates the code. testRoot is a placeholder; real domains are
// config-only and red-line-forbidden in source.

const testRoot = "mc.example.net"

// recordingRunner is a fake Runner that records every side-effecting call so a
// test can assert that a refusal happened BEFORE any Cloudflare mutation.
type recordingRunner struct {
	calls       []string
	tunnelID    string
	credentials string
	appID       string
	aud         string
}

type verifyingRunner struct {
	recordingRunner
	verified  bool
	verifyErr error
}

func (r *verifyingRunner) VerifyAPIToken(_ context.Context) error {
	r.verified = true
	return r.verifyErr
}

func (r *recordingRunner) CreateTunnel(_ context.Context, name string) (string, string, error) {
	r.calls = append(r.calls, "CreateTunnel:"+name)
	id := r.tunnelID
	if id == "" {
		id = "11111111-2222-3333-4444-555555555555"
	}
	cred := r.credentials
	if cred == "" {
		cred = "/root/.cloudflared/" + id + ".json"
	}
	return id, cred, nil
}

func (r *recordingRunner) RouteDNS(_ context.Context, tunnelID, hostname string) error {
	r.calls = append(r.calls, "RouteDNS:"+hostname)
	return nil
}

func (r *recordingRunner) WriteTunnelConfig(path string, _ []byte) error {
	r.calls = append(r.calls, "WriteTunnelConfig:"+path)
	return nil
}

func (r *recordingRunner) CreateAccessApplication(_ context.Context, app AccessApplication) (string, string, error) {
	r.calls = append(r.calls, "CreateAccessApplication:"+app.Domain)
	appID := r.appID
	if appID == "" {
		appID = "app-123"
	}
	aud := r.aud
	if aud == "" {
		aud = "aud-abc"
	}
	return appID, aud, nil
}

func (r *recordingRunner) CreateAccessPolicy(_ context.Context, appID string, policy AccessPolicy) error {
	r.calls = append(r.calls, "CreateAccessPolicy:"+policy.Name)
	return nil
}

func goodPreconditions() Preconditions {
	return Preconditions{CloudflaredPath: "/usr/local/bin/cloudflared", CertExists: true, APIToken: "tok"}
}

// TestRecommendedPolicyIsFailClosed is the load-bearing safety property: the
// recommended policy admits a scoped identity and is NEVER public. It checks every
// way the policy could go wrong — an unscoped build, a bare-everyone allow, and a
// bypass decision must all be refused — and that legitimate scopings (a specific
// email, an org SSO domain, and an SSO-IdP-only login method) are accepted.
func TestRecommendedPolicyIsFailClosed(t *testing.T) {
	t.Run("scoped by email is allowed and fail-closed", func(t *testing.T) {
		p, err := BuildRecommendedPolicy("rec", AccessIdentity{Emails: []string{"owner@example.net"}})
		if err != nil {
			t.Fatalf("scoped policy rejected: %v", err)
		}
		if p.Decision != "allow" {
			t.Fatalf("decision = %q, want allow", p.Decision)
		}
		if err := validateFailClosed(p); err != nil {
			t.Fatalf("scoped policy failed the guard: %v", err)
		}
	})

	t.Run("scoped by org SSO domain is allowed", func(t *testing.T) {
		if _, err := BuildRecommendedPolicy("rec", AccessIdentity{EmailDomains: []string{"example.net"}}); err != nil {
			t.Fatalf("email_domain scoping rejected: %v", err)
		}
	})

	t.Run("scoped by SSO login method only is allowed (Google/Keycloak/Entra)", func(t *testing.T) {
		// IdP-only scoping yields include everyone + require login_method — the
		// Cloudflare-recommended authenticated-users shape, which is constrained.
		p, err := BuildRecommendedPolicy("rec", AccessIdentity{LoginMethods: []string{"idp-google-123"}})
		if err != nil {
			t.Fatalf("SSO login_method scoping rejected: %v", err)
		}
		if len(p.Require) == 0 {
			t.Fatal("SSO-only policy must carry a require login_method constraint")
		}
		if err := validateFailClosed(p); err != nil {
			t.Fatalf("SSO-only policy failed the guard: %v", err)
		}
	})

	t.Run("empty identity is refused (no public door)", func(t *testing.T) {
		if _, err := BuildRecommendedPolicy("rec", AccessIdentity{}); err == nil {
			t.Fatal("an unscoped identity must not build a policy")
		}
	})

	t.Run("bare everyone allow is refused", func(t *testing.T) {
		public := AccessPolicy{
			Name:     "public",
			Decision: "allow",
			Include:  []accessRule{{"everyone": {}}},
		}
		if err := validateFailClosed(public); err == nil {
			t.Fatal("guard accepted a bare-everyone allow policy — that is public access")
		}
	})

	t.Run("everyone OR identity is refused (include rules are OR — everyone wins)", func(t *testing.T) {
		// Cloudflare Access include rules combine as OR: a user matches if they
		// satisfy ANY rule. So {everyone} OR {email:X} admits everyone — the email
		// rule is pure redundancy. Only a require (AND) can narrow an everyone base.
		orPublic := AccessPolicy{
			Name:     "everyone-or-email",
			Decision: "allow",
			Include: []accessRule{
				{"everyone": {}},
				{"email": {"email": "owner@example.net"}},
			},
		}
		if err := validateFailClosed(orPublic); err == nil {
			t.Fatal("guard accepted everyone-OR-identity — that is public access (everyone wins the OR)")
		}
	})

	t.Run("unrecognized include type is refused (allowlist, not denylist)", func(t *testing.T) {
		// A fail-closed guard must reject include shapes it does not recognize as
		// scoped — an ip:0.0.0.0/0 include, for instance, is public but is not one
		// of the known-bad shapes a denylist would catch.
		ipAny := AccessPolicy{
			Name:     "ip-any",
			Decision: "allow",
			Include:  []accessRule{{"ip": {"ip": "0.0.0.0/0"}}},
		}
		if err := validateFailClosed(ipAny); err == nil {
			t.Fatal("guard accepted an unrecognized include type — a fail-closed guard must allowlist scoped identities only")
		}
	})

	t.Run("empty/malformed include rule is refused", func(t *testing.T) {
		// A degenerate include rule with no recognized key must not slip through the
		// allowlist as if it were scoped.
		empty := AccessPolicy{
			Name:     "empty-rule",
			Decision: "allow",
			Include:  []accessRule{{}},
		}
		if err := validateFailClosed(empty); err == nil {
			t.Fatal("guard accepted an empty include rule — a fail-closed guard must refuse rules it cannot recognize as scoped")
		}
	})

	t.Run("everyone narrowed by a non-constraining require is refused", func(t *testing.T) {
		// A require of {everyone} does not narrow anything; everyone base + such a
		// require is still public.
		noopRequire := AccessPolicy{
			Name:     "everyone-require-everyone",
			Decision: "allow",
			Include:  []accessRule{{"everyone": {}}},
			Require:  []accessRule{{"everyone": {}}},
		}
		if err := validateFailClosed(noopRequire); err == nil {
			t.Fatal("guard accepted everyone-include narrowed only by an everyone-require — still public")
		}
	})

	t.Run("bypass decision is refused", func(t *testing.T) {
		bypass := AccessPolicy{
			Name:     "bypass",
			Decision: "bypass",
			Include:  []accessRule{{"email": {"email": "owner@example.net"}}},
		}
		if err := validateFailClosed(bypass); err == nil {
			t.Fatal("guard accepted a bypass policy — bypass skips authentication for everyone")
		}
	})
}

// TestSetupGatingHasNoSideEffects proves each operator-only precondition is a hard
// gate: a missing cloudflared, an absent login (no cert.pem), or a missing API
// token each aborts Setup with the right error and — critically — with ZERO calls
// to the runner, so a gated refusal never half-creates a tunnel or DNS record.
func TestSetupGatingHasNoSideEffects(t *testing.T) {
	base := Params{
		PanelHostname:  "console." + testRoot,
		AdminHostname:  "op.console." + testRoot,
		TunnelName:     "felis",
		ConfigPath:     "/etc/felis/cloudflared.yml",
		AccessIdentity: AccessIdentity{Emails: []string{"owner@example.net"}},
	}

	cases := []struct {
		name string
		pre  Preconditions
		want error
	}{
		{"no cloudflared", Preconditions{CertExists: true, APIToken: "tok"}, ErrCloudflaredMissing},
		{"not logged in", Preconditions{CloudflaredPath: "/bin/cloudflared", APIToken: "tok"}, ErrNotLoggedIn},
		{"no api token", Preconditions{CloudflaredPath: "/bin/cloudflared", CertExists: true}, ErrNoAPIToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{}
			p := base
			p.Pre = tc.pre
			_, err := Setup(context.Background(), runner, p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("a gated refusal made side effects: %v", runner.calls)
			}
		})
	}
}

// TestSetupRefusesUnscopedPolicyBeforeSideEffects is defense-in-depth: even with
// every precondition met, an empty AccessIdentity (which would yield a public
// policy) aborts Setup BEFORE any tunnel/DNS/app is created. The fail-closed guard
// is wired into the orchestrator, not merely a standalone helper.
func TestSetupVerifiesAPITokenBeforeCloudflareMutations(t *testing.T) {
	tokenErr := errors.New("token inactive")
	runner := &verifyingRunner{verifyErr: tokenErr}
	p := Params{
		PanelHostname:  "console." + testRoot,
		AdminHostname:  "op.console." + testRoot,
		TunnelName:     "felis",
		AccessIdentity: AccessIdentity{Emails: []string{"owner@example.net"}},
		Pre:            goodPreconditions(),
	}
	_, err := Setup(context.Background(), runner, p)
	if !errors.Is(err, tokenErr) {
		t.Fatalf("err = %v, want token verifier error", err)
	}
	if !runner.verified {
		t.Fatal("Setup did not verify the API token")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("token verification failure made Cloudflare mutations: %v", runner.calls)
	}
}

func TestSetupRefusesUnscopedPolicyBeforeSideEffects(t *testing.T) {
	runner := &recordingRunner{}
	p := Params{
		PanelHostname:  "console." + testRoot,
		AdminHostname:  "op.console." + testRoot,
		TunnelName:     "felis",
		AccessIdentity: AccessIdentity{}, // unscoped → public → must be refused
		Pre:            goodPreconditions(),
	}
	if _, err := Setup(context.Background(), runner, p); err == nil {
		t.Fatal("Setup accepted an unscoped (public) identity")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Setup made side effects before refusing the public policy: %v", runner.calls)
	}
}

// TestSetupSucceedsAndReportsAud is the happy path against the fake: with good
// preconditions and a scoped identity, Setup routes exactly the web hostnames
// (never the game host), and reports the Access aud the caller must adopt into
// felis [auth] access_jwt_aud.
func TestSetupSucceedsAndReportsAud(t *testing.T) {
	runner := &recordingRunner{aud: "felis-aud-xyz"}
	p := Params{
		PanelHostname:  "console." + testRoot,
		AdminHostname:  "op.console." + testRoot,
		TunnelName:     "felis",
		ConfigPath:     "/etc/felis/cloudflared.yml",
		AccessIdentity: AccessIdentity{Emails: []string{"owner@example.net"}},
		Pre:            goodPreconditions(),
	}
	res, err := Setup(context.Background(), runner, p)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if res.AccessAud != "felis-aud-xyz" {
		t.Fatalf("AccessAud = %q, want felis-aud-xyz (felis-api must validate this)", res.AccessAud)
	}
	// Exactly the two web hostnames are routed; the bare game host never is.
	if len(res.RoutedHostnames) != 2 {
		t.Fatalf("routed %v, want the two web hostnames only", res.RoutedHostnames)
	}
	for _, h := range res.RoutedHostnames {
		if h == testRoot {
			t.Fatalf("the game host %q must never be routed through the tunnel", testRoot)
		}
	}
}

// TestIngressSafetyInvariants parses the generated cloudflared config back and
// asserts the properties that protect the origin: the final rule is the
// hostname-less fail-shut 404 catch-all, every routed hostname points at the panel
// origin and nowhere else, and the raw game host is never an ingress hostname.
func TestIngressSafetyInvariants(t *testing.T) {
	const origin = "http://localhost:8080"
	hostnames := []string{"console." + testRoot, "op.console." + testRoot}
	raw, err := BuildTunnelConfig("11111111-2222-3333-4444-555555555555", "/root/.cloudflared/x.json", origin, hostnames)
	if err != nil {
		t.Fatalf("BuildTunnelConfig: %v", err)
	}
	var cfg tunnelConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("generated config is not valid YAML: %v\n%s", err, raw)
	}
	if len(cfg.Ingress) == 0 {
		t.Fatal("no ingress rules")
	}
	// The catch-all MUST be last and MUST be the fail-shut 404.
	last := cfg.Ingress[len(cfg.Ingress)-1]
	if last.Hostname != "" || last.Service != catchAllService {
		t.Fatalf("final ingress rule = %+v, want hostname-less %s catch-all", last, catchAllService)
	}
	// Every non-terminal rule routes a known web hostname to the panel origin, and
	// the raw game host is never routed.
	for _, rule := range cfg.Ingress[:len(cfg.Ingress)-1] {
		if rule.Hostname == testRoot {
			t.Fatalf("the raw game host %q must never be an ingress hostname", testRoot)
		}
		if rule.Hostname == "" {
			t.Fatal("a non-terminal rule has no hostname (only the catch-all may)")
		}
		if rule.Service != origin {
			t.Fatalf("hostname %q routes to %q, want the panel origin %q", rule.Hostname, rule.Service, origin)
		}
	}
}
