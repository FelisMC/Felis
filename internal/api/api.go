// Package api implements felis-api: one binary serving two faces (spec §7).
//
// The internal face (velocity / backend callbacks) authenticates with a static
// service token and is never wrapped in Zero Trust. The external face (people /
// panel) authenticates with a Cloudflare Access JWT; admin-tier operations
// additionally require the admin Access path (spec §14, graded by operation).
//
// Handlers depend on the Repo and Cluster interfaces, so the request routing,
// dual-face auth, input validation and authorization are all unit-tested with
// in-memory fakes. The Postgres (pgRepo) and controller-runtime (k8sCluster)
// implementations compile here but are exercised only by integration tests
// against a live database / cluster.
package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

// API holds the dependencies shared by every handler.
type API struct {
	Repo     Repo
	Cluster  Cluster
	Internal InternalAuth
	External ExternalAuth

	// Builder is the image build subsystem (spec §16). It is optional: when nil
	// the /images routes report 503 rather than 404, so the admin boundary is
	// still exercised even before the subsystem is wired in.
	Builder ImageBuilder

	// Console is the synchronous RCON write channel (spec §8 写=RCON). It is
	// wired in production (cmd/felis); a nil Console makes the command route report
	// 503 rather than panic, so the ownership boundary is still exercised in tests.
	Console Console

	// Logs is the read-side console channel (spec §8 读=pods/log follow). Like
	// Console it is wired in production (cmd/felis); a nil Logs makes the console
	// route report 503 rather than panic, so the ownership boundary is still
	// exercised in tests.
	Logs LogStreamer

	// BuildLogs is the read-side build-log channel (spec §16, §416 日志流复用 §8):
	// it streams the build Job's Pod log (kaniko) in the build namespace, distinct
	// from Logs which streams a server pod in the minecraft namespace. Like Logs it
	// is wired in production (cmd/felis); a nil BuildLogs makes the admin-only
	// build-logs route report 503 rather than panic, so the admin boundary is still
	// exercised in tests.
	BuildLogs LogStreamer

	// Restorer starts a world restore from a backup (spec §466). It is optional:
	// when nil the restore-backup route reports 503, so the restore authorization
	// boundary is exercised before the restore-Job executor is wired. The list and
	// authorization paths do not need it (they read the Repo), only the kick-off.
	Restorer Restorer

	// Submissions is the user-modpack approval lane (a user-directed extension over
	// the §16 build subsystem; see internal/submit). It is optional: when
	// nil the /me/submissions and /submissions routes report 503 rather than 404, so
	// the app/admin boundary is still exercised even before the subsystem is wired
	// in. An ordinary user may only SUBMIT here; an admin approves before any build
	// runs, distinct from Builder which an admin drives directly.
	Submissions SubmissionService

	// Mailer delivers player email one-time codes (spec §B2 onboarding). It is
	// optional: when nil the email-OTP start route mints and persists the code but
	// logs it server-side instead of mailing it (a KNOWN-LIMITATION — the demo has no
	// SMTP), so the verify flow is still exercised end-to-end. Production wires a real
	// sender. The code is never returned to the client on either path.
	Mailer OTPMailer

	// Passkey verifies WebAuthn credential-creation ceremonies (spec §14 / Phase 6
	// passkey bind). It is optional: when nil the passkey register routes report 503
	// rather than panic, so the authenticated enrollment boundary is exercised before
	// the go-webauthn verifier is wired in (cmd/felis). The credential-management
	// reads/deletes do not need it (they read the Repo), only the begin/finish
	// ceremony. Tests inject a fake verifier so the enrollment state machine is
	// exercised without real attestation crypto.
	Passkey PasskeyVerifier

	// RootDomain is injected from config (spec §2). It is the only place the
	// deployment zone enters the API; hostnames are validated against it and
	// never hardcoded.
	RootDomain string

	// WakeCooldown throttles repeated wakes per server (spec §9.1: cooldown hangs
	// on the wake lever). Zero disables throttling.
	WakeCooldown time.Duration

	// MaxRunningServers caps how many servers may be desired-Running cluster-wide
	// (spec §9.1: the concurrency-上限 lever hanging on the same wake chokepoint as
	// cooldown and autostartPolicy). Zero — the default — disables it: §9.2 wires
	// only autostartPolicy + cooldown as active wake gates, so this lever ships
	// inert, exactly like a zero WakeCooldown, and a deployment opts in by setting
	// a positive value. Enforced via withinRunningCap on the wake path.
	MaxRunningServers int

	// MaxConcurrentLogins bounds how many password logins may run their (CPU-costly)
	// bcrypt compare at once on the public /auth/login route. bcrypt is deliberately
	// expensive and the anti-enumeration path runs a full compare on EVERY request,
	// so an unbounded flood of concurrent logins would pin every core; capping the
	// simultaneous compares sheds the excess with a cheap 429 instead. Zero — the
	// default — disables the cap (same "zero disables" idiom as WakeCooldown /
	// MaxRunningServers); cmd/felis wires a positive value. It is a concurrency cap,
	// NOT a per-account lockout, so it never fences a break-glass admin out of the
	// one account they need. Enforced via loginLimiter in handleLogin.
	MaxConcurrentLogins int

	// MaxStreamsPerPrincipal caps how many concurrent Server-Sent Event streams
	// (console + build-log relays, spec §8) a single principal may hold open at once.
	// Each relay blocks for the lifetime of a client's attachment and, under a stalled
	// reader, pins a goroutine plus a kube-apiserver follow connection (relayLogStream).
	// The cap does NOT fix that leak — the per-write deadline that severs a stalled
	// stream is a separate slice — but it bounds the blast radius so one principal
	// cannot accumulate unbounded leaked control-plane connections. Zero — the default
	// — disables it (same "zero disables" idiom as the levers above); cmd/felis wires a
	// positive value. Enforced via streamGate in the two relay handlers.
	MaxStreamsPerPrincipal int

	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time

	cooldownOnce sync.Once
	cooldown     *cooldownLimiter

	otpCooldownOnce sync.Once
	otpCooldown     *cooldownLimiter

	loginCapOnce sync.Once
	loginCap     *concurrencyLimiter

	streamCapOnce sync.Once
	streamCap     *streamLimiter
}

// now returns the current time using the injected clock.
func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// limiter lazily builds the wake cooldown limiter bound to a's clock.
func (a *API) limiter() *cooldownLimiter {
	a.cooldownOnce.Do(func() {
		a.cooldown = &cooldownLimiter{now: a.now, last: map[string]time.Time{}}
	})
	return a.cooldown
}

// otpLimiter lazily builds a SEPARATE cooldown limiter for email-OTP sends, so an
// OTP resend throttle never shares state with the wake throttle. Keyed by
// principal and by recipient (see handleEmailOTPStart), it bounds how often a code
// may be mailed and closes the email-bomb vector.
func (a *API) otpLimiter() *cooldownLimiter {
	a.otpCooldownOnce.Do(func() {
		a.otpCooldown = &cooldownLimiter{now: a.now, last: map[string]time.Time{}}
	})
	return a.otpCooldown
}

// loginLimiter lazily builds the login bcrypt concurrency cap bound to
// MaxConcurrentLogins. A zero cap yields a disabled limiter that admits every
// caller, so a deployment (or test) that leaves it unset pays nothing.
func (a *API) loginLimiter() *concurrencyLimiter {
	a.loginCapOnce.Do(func() {
		a.loginCap = newConcurrencyLimiter(a.MaxConcurrentLogins)
	})
	return a.loginCap
}

// streamGate lazily builds the per-principal SSE stream cap bound to
// MaxStreamsPerPrincipal. A zero cap yields a disabled limiter that admits every
// stream, so a deployment (or test) that leaves it unset pays nothing.
func (a *API) streamGate() *streamLimiter {
	a.streamCapOnce.Do(func() {
		a.streamCap = newStreamLimiter(a.MaxStreamsPerPrincipal)
	})
	return a.streamCap
}

// streamKey identifies the principal a stream slot is charged to. It prefers the
// stable user id and falls back to the email so a JWT principal without a user id is
// still bucketed by identity; an empty key (no authenticated identity, which the
// external face's auth guard already precludes) shares one bucket, which is safe
// because it is more restrictive, never less.
func streamKey(p *Principal) string {
	if p == nil {
		return ""
	}
	if p.UserID != "" {
		return p.UserID
	}
	return p.Email
}

// apiRoute is one served HTTP route. Each face exposes its routes as a single
// table (internalAPIRoutes / externalAPIRoutes) so that one declaration drives
// BOTH handler construction here AND the OpenAPI parity test (openapi_test.go):
// the doc is checked against the routes actually served — in both directions —
// rather than against a re-derivation, which a http.ServeMux cannot be asked to
// confirm (it exposes no way to enumerate its registered patterns).
type apiRoute struct {
	Method  string
	Pattern string

	// Public routes are unauthenticated and mounted on the outer mux — the health
	// probes, since kubelet / Cloudflare hold no token. Everything else is mounted
	// behind the face's auth middleware.
	Public bool

	// Admin marks an external-face route that is additionally gated on the admin
	// Zero-Trust path (the adminOnly wrapper + Principal.IsAdmin() inside the
	// handler). Internal-face routes never set it.
	Admin bool

	// AllowDuringPasswordChange opts a route OUT of the must_change_password
	// lockdown (spec §B). The lockdown is default-deny: every authenticated route is
	// fenced off for a staff principal that still owes a first-login password change
	// EXCEPT the few that let it escape the state — change-password, logout, and the
	// self-identity read /me. A new authenticated route is locked down unless it
	// sets this, so forgetting the flag fails safe (closed), never open.
	AllowDuringPasswordChange bool

	h http.HandlerFunc
}

// internalAPIRoutes is the internal face's served route table (spec §7, §14):
// service-token auth, never Zero Trust. It carries both health probes.
func (a *API) internalAPIRoutes() []apiRoute {
	return []apiRoute{
		{Method: "GET", Pattern: "/healthz", Public: true, h: a.handleHealthz},
		{Method: "GET", Pattern: "/readyz", Public: true, h: a.handleReadyz},

		{Method: "GET", Pattern: "/api/v1/servers", h: a.handleListServers},
		{Method: "GET", Pattern: "/api/v1/servers/by-host/{host}", h: a.handleByHost},
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/ready", h: a.handleReady},
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/join-event", h: a.handleJoinEvent},
		// Domain-autostart (spec §9.1, §14): velocity drives the wake lever and polls
		// status with its service token, identifying the joining player by online-mode
		// UUID. These live on the internal face because velocity holds no web Principal;
		// the external face keeps its own Principal-gated wake/status for the panel.
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/wake", h: a.handleInternalWake},
		{Method: "GET", Pattern: "/api/v1/internal/servers/{name}/status", h: a.handleStatus},
		// Lobby `/menu` (spec §12): the felis-paper lobby is a pure UI face holding no
		// token, so velocity drives these on its behalf — claim by online-mode UUID
		// (the lobby's `Claim & Start`, separate from the autostartPolicy-gated wake)
		// and the menu projection that adds the ownership-derived `claimable` the §11
		// list/status views never carry.
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/claim", h: a.handleInternalClaim},
		{Method: "GET", Pattern: "/api/v1/internal/servers/{name}/menu", h: a.handleInternalMenuStatus},
		// Account linking (spec §10): the in-game /link side mints a one-time code for a
		// verified UUID. Internal-only — the code is born from an online-mode UUID the
		// web never holds (account_link_codes has no user_id column).
		{Method: "POST", Pattern: "/api/v1/internal/account/link/code", h: a.handleCreateLinkCode},
		// QR scan-to-login completion poll (spec §B3 player game-login). After the player
		// scans the QR-encoded code and the web verify writes the durable link, velocity
		// polls this for the UUID it minted against and admits on {linked:true}. Read-only
		// and keyed by the verified UUID (not the scanned code), so it consumes nothing
		// and is safe to poll repeatedly.
		{Method: "GET", Pattern: "/api/v1/internal/account/link/status/{mc_uuid}", h: a.handleLinkStatus},
		// Username-collision reclaim (spec §B3): velocity records a Mojang-priority
		// reclaim (bar the squatter UUID + stash its data for 30 days) and gates the
		// limbo login by checking whether a connecting UUID was barred. Internal-only —
		// velocity holds a service token, and the bar is keyed by UUID so the genuine
		// Mojang player (same name, different UUID) always passes.
		{Method: "POST", Pattern: "/api/v1/internal/player/reclaim", h: a.handleReclaimUsername},
		{Method: "GET", Pattern: "/api/v1/internal/player/blacklist/{mc_uuid}", h: a.handleCheckBlacklist},
	}
}

// externalAPIRoutes is the external face's served route table (spec §7, §14):
// Cloudflare Access-JWT auth on every /api/v1 route; the Admin entries are
// additionally gated on the admin Zero-Trust path. It exposes liveness only —
// readiness is an internal concern.
func (a *API) externalAPIRoutes() []apiRoute {
	return []apiRoute{
		{Method: "GET", Pattern: "/healthz", Public: true, h: a.handleHealthz},

		// Local-password auth (spec §B), the op.console login surface. login/logout
		// are Public (pre-session: a caller has no principal yet, and logout reads the
		// cookie directly so it works even after expiry). change-password requires a
		// live session and stays reachable while must_change_password is set
		// (AllowDuringPasswordChange) so a forced first-login change can complete.
		{Method: "POST", Pattern: "/api/v1/auth/login", Public: true, h: a.handleLogin},
		{Method: "POST", Pattern: "/api/v1/auth/logout", Public: true, h: a.handleLogout},
		{Method: "POST", Pattern: "/api/v1/auth/change-password", AllowDuringPasswordChange: true, h: a.handleChangePassword},
		// Player-console bootstrap (console-tier access model): the account-less
		// player's door into console.<root_domain>. Public — like login there is no prior
		// principal — and session-minting, but the artifact it consumes is a one-time
		// Bind Code minted internal-face against an online-mode-verified UUID, so
		// possession already proves a Minecraft identity. A code whose UUID belongs to
		// staff is refused (403) so this never yields an admin session; op.console stays
		// behind Zero Trust (handlers_onboard.go).
		{Method: "POST", Pattern: "/api/v1/auth/bind", Public: true, h: a.handleBindRedeem},

		// App-auth tier: operations on your own servers (spec §14).
		{Method: "POST", Pattern: "/api/v1/servers/{name}/wake", h: a.handleWake},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/stop", h: a.handleStop},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/claim", h: a.handleClaim},
		// Console write (spec §8 写=RCON): owner/admin-gated inside the handler, so
		// it sits in the app-tier block (操作自己服 → app 鉴权), not behind adminOnly.
		{Method: "POST", Pattern: "/api/v1/servers/{name}/command", h: a.handleCommand},
		// Console read (spec §8 读=pods/log follow; §262 SSE). Owner/admin-gated inside
		// the handler, app-tier like the write side.
		{Method: "GET", Pattern: "/api/v1/servers/{name}/console", h: a.handleServerConsole},
		// Access / permissions (spec §7): structured whitelist / ban / LuckPerms
		// operations translated to RCON commands over the same owner-gated console
		// path as /command. An owner manages their OWN claimed node; an admin manages
		// ANY node — both via isOwnerOrAdmin inside issueAccessCommand, so these stay
		// app-tier (not behind adminOnly). Each command is assembled only from
		// strict-charset-validated structured fields (handlers_access.go).
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/whitelist", h: a.handleAccessWhitelist},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/access/whitelist", h: a.handleAccessWhitelistList},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/ban", h: a.handleAccessBan},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/permission", h: a.handleAccessPermission},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/group", h: a.handleAccessGroup},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/status", h: a.handleStatus},
		// Identity self-read (spec §14 tiering): the panel reads this once at boot to
		// learn its own tier and decide which navigation surfaces to render. App-tier —
		// every authenticated principal may read its OWN identity. is_admin is the
		// server-computed Principal.IsAdmin() (Role + admin Access path), so the client
		// never re-derives the graded-ZT rule; it remains UX truth, not enforcement.
		// /me is exempt from the first-login lockdown so the panel can read its own
		// identity (including must_change_password) to render the change-password card.
		{Method: "GET", Pattern: "/api/v1/me", AllowDuringPasswordChange: true, h: a.handleMe},
		{Method: "GET", Pattern: "/api/v1/me/servers", h: a.handleMyServers},
		// World backups (spec §7, §466). Both are app-tier: GET /backups is scoped
		// inside the handler (admin sees all; a user sees only worlds they formerly
		// owned), and restore is gated by owner-or-admin PLUS a former-owner match, so
		// neither sits behind adminOnly.
		{Method: "GET", Pattern: "/api/v1/backups", h: a.handleListBackups},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/restore-backup", h: a.handleRestoreBackup},
		// Account linking (spec §10), web side: /start reports link status (it is the
		// pointer handleClaim's 412 emits), /verify consumes the in-game code and binds
		// the account. App-tier, not admin — linking your own account is an ordinary
		// authenticated operation.
		{Method: "POST", Pattern: "/api/v1/account/link/start", h: a.handleLinkStart},
		{Method: "POST", Pattern: "/api/v1/account/link/verify", h: a.handleLinkVerify},
		// Email verification (spec §B2 onboarding), web side: /start mints+delivers a
		// one-time code for the caller's chosen address, /verify redeems it and flips
		// email_verified. App-tier like the link routes — proving control of your own
		// email is an ordinary authenticated operation, scoped to the principal.
		{Method: "POST", Pattern: "/api/v1/account/email/start", h: a.handleEmailOTPStart},
		{Method: "POST", Pattern: "/api/v1/account/email/verify", h: a.handleEmailOTPVerify},
		// Passkey enrollment (spec §14 WebAuthn / Phase 6 bind), web side: /register/begin
		// mints a credential-creation challenge for the caller, /register/finish verifies
		// the authenticator's attestation and binds the passkey, and the credentials
		// collection lists and unbinds the caller's OWN passkeys. App-tier like the email
		// routes — binding a passkey to your own account is an ordinary authenticated
		// operation, scoped entirely to the principal (the body never names a user). This
		// is enrollment only; passkey LOGIN/assertion is a deferred slice (see migration
		// 0007 and handlers_passkey.go).
		{Method: "POST", Pattern: "/api/v1/account/passkey/register/begin", h: a.handlePasskeyRegisterBegin},
		{Method: "POST", Pattern: "/api/v1/account/passkey/register/finish", h: a.handlePasskeyRegisterFinish},
		{Method: "GET", Pattern: "/api/v1/account/passkey/credentials", h: a.handlePasskeyList},
		{Method: "DELETE", Pattern: "/api/v1/account/passkey/credentials/{id}", h: a.handlePasskeyDelete},
		// Modpack submission (user-directed lane over §16), user side: a user files an upload for review
		// and lists their own. App-tier — the submitter and the "my uploads" scope are
		// both taken from the principal, never the body, so an ordinary authenticated
		// session is the correct gate (the admin verdict lives below, behind adminOnly).
		{Method: "POST", Pattern: "/api/v1/me/submissions", h: a.handleCreateSubmission},
		{Method: "GET", Pattern: "/api/v1/me/submissions", h: a.handleMySubmissions},
		// Admin (Zero-Trust) tier: create / mutate spec / image admission. These gate
		// on Principal.IsAdmin() inside the handler via the adminOnly wrapper, so the
		// boundary is exercised even where the body is a later-phase stub.
		{Method: "POST", Pattern: "/api/v1/servers", Admin: true, h: a.handleCreateServer},
		{Method: "PATCH", Pattern: "/api/v1/servers/{name}", Admin: true, h: a.handlePatchServer},
		// SysAdmin fleet read: the whole-fleet lifecycle list for the cockpit's
		// FleetTable (panel/DESIGN-WEB-3SIDES.md — a frontend extension, not a spec §7
		// route). Admin-tier — it reads every owner's server, so it gates on the admin
		// Zero-Trust path, unlike the app-tier /me/servers. A path distinct from the
		// internal velocity GET /api/v1/servers on purpose: the parity test forbids one
		// {method, path} from carrying both the service and admin tiers.
		{Method: "GET", Pattern: "/api/v1/fleet", Admin: true, h: a.handleFleet},
		// Image build + whitelist (spec §16, §15). Every route is admin-tier: a build
		// is build-time RCE against the cluster, so submission requires the admin
		// Zero-Trust path, not merely an authenticated session.
		{Method: "POST", Pattern: "/api/v1/images/build", Admin: true, h: a.handleBuildImage},
		{Method: "GET", Pattern: "/api/v1/images/build/{id}", Admin: true, h: a.handleGetBuild},
		{Method: "GET", Pattern: "/api/v1/images/build/{id}/logs", Admin: true, h: a.handleBuildLogs},
		{Method: "POST", Pattern: "/api/v1/images/build/{id}/cancel", Admin: true, h: a.handleCancelBuild},
		{Method: "GET", Pattern: "/api/v1/images", Admin: true, h: a.handleListImages},
		{Method: "POST", Pattern: "/api/v1/images", Admin: true, h: a.handleAddImage},
		{Method: "DELETE", Pattern: "/api/v1/images", Admin: true, h: a.handleRemoveImage},
		// Modpack submission review (user-directed lane over §16), admin side: the queue of every user's
		// uploads and the approve/reject verdicts. Admin-tier — listing reads other
		// users' uploads and approving starts a build-time Kaniko job, so both require
		// the admin Zero-Trust path, exactly like a direct /images/build.
		{Method: "GET", Pattern: "/api/v1/submissions", Admin: true, h: a.handleListSubmissions},
		{Method: "POST", Pattern: "/api/v1/submissions/{id}/approve", Admin: true, h: a.handleApproveSubmission},
		{Method: "POST", Pattern: "/api/v1/submissions/{id}/reject", Admin: true, h: a.handleRejectSubmission},
		// Auto-update maintenance window (spec §B; decision core internal/updates).
		// Admin-tier: it governs whether Felis may apply an update to itself, so setting
		// it requires the admin Zero-Trust path, not a mere session. API+persistence
		// only — the runner/executors that consume the window are still INTEGRATION-ONLY.
		{Method: "GET", Pattern: "/api/v1/updates/window", Admin: true, h: a.handleGetUpdateWindow},
		{Method: "PUT", Pattern: "/api/v1/updates/window", Admin: true, h: a.handleSetUpdateWindow},
	}
}

// InternalHandler builds the internal-face http.Handler: service-token auth, no
// Zero Trust (spec §14 red line). /healthz and /readyz are unauthenticated.
func (a *API) InternalHandler() http.Handler {
	return a.buildFace(a.internalAPIRoutes(), a.requireInternal)
}

// ExternalHandler builds the external-face http.Handler: Access-JWT auth on every
// /api/v1 route, with admin-tier routes additionally gated by the admin Access
// path inside their handlers.
func (a *API) ExternalHandler() http.Handler {
	return a.buildFace(a.externalAPIRoutes(), a.requireExternal)
}

// buildFace assembles one face from its route table. Public routes are mounted
// unauthenticated on the outer mux; the rest go on an inner mux behind guard
// (requireInternal / requireExternal), with Admin routes additionally wrapped in
// adminOnly. Because both faces are built from the same table the OpenAPI parity
// test reads, the served surface and the documented surface cannot drift apart
// without failing the build.
func (a *API) buildFace(routes []apiRoute, guard func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	auth := http.NewServeMux()
	for _, rt := range routes {
		pattern := rt.Method + " " + rt.Pattern
		if rt.Public {
			mux.HandleFunc(pattern, rt.h)
			continue
		}
		h := rt.h
		if rt.Admin {
			h = a.adminOnly(rt.h)
		}
		// Default-deny first-login lockdown (spec §B): wrap every authenticated route
		// unless it explicitly opts out. The wrapper is nil-principal safe, so it is
		// inert on the internal face (service-token callers carry no Principal).
		if !rt.AllowDuringPasswordChange {
			h = a.lockdownDuringPasswordChange(h)
		}
		auth.HandleFunc(pattern, h)
	}
	mux.Handle("/api/v1/", guard(auth))
	return a.baseChain(mux)
}

// baseChain wraps a handler in the cross-cutting middleware shared by both faces.
func (a *API) baseChain(h http.Handler) http.Handler {
	return withRequestID(withRecover(h))
}

// ---- request context plumbing ----

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
)

func requestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// principalFromContext returns the authenticated external-face caller, or nil.
func principalFromContext(ctx context.Context) *Principal {
	if v, ok := ctx.Value(ctxKeyPrincipal).(*Principal); ok {
		return v
	}
	return nil
}

// ---- per-key cooldown (wake + OTP) ----

// cooldownLimiter is an in-memory per-key cooldown. It backs two throttles with
// separate keyspaces: the wake lever (key = server name, via allowed/record) and
// the email-OTP start (keys = principal and recipient, via the atomic
// reserve/release). It is process-local, so with multiple api replicas the
// effective cooldown is per-replica. For wake that is acceptable — the operator
// reconcile is idempotent, so a burst slipping through is harmless. For OTP it is a
// real KNOWN-LIMITATION: each admitted send is a non-idempotent email, so across N
// replicas a determined caller could draw up to N codes per window. The atomic
// reserve/release pair closes the intra-replica concurrent burst (the bug fixed in
// #35); cross-replica bounding would need a shared store (out of scope for the
// single-replica demo).
type cooldownLimiter struct {
	mu     sync.Mutex
	now    func() time.Time
	last   map[string]time.Time
	window time.Duration
}

// allowed reports whether name may wake now WITHOUT recording the attempt. A
// non-positive window disables the throttle. Splitting the check (allowed) from
// the commit (record) lets the wake path consult the cooldown for its 429 before
// a downstream gate — the §9.1 running-cap 503 — decides whether the wake will
// actually happen, so a wake refused at capacity never burns the per-server
// cooldown.
func (c *cooldownLimiter) allowed(name string, window time.Duration) bool {
	if window <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.last[name]; ok && c.now().Sub(last) < window {
		return false
	}
	return true
}

// record starts name's cooldown at the current time. The wake path calls it only
// after the wake actually flips desiredState, so neither a 503 at_capacity nor a
// SetDesiredState error consumes the cooldown. allowed→record is deliberately not
// atomic: like the running-cap above, the cooldown is a soft throttle (a burst of
// truly concurrent wakes may each pass allowed before any records), which is
// harmless because SetDesiredState is idempotent.
func (c *cooldownLimiter) record(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last[name] = c.now()
}

// reserve atomically checks name's cooldown AND, if the window is open, records it
// in the same critical section, returning the reservation time and true. Unlike
// allowed→record there is no gap between the check and the commit, so a burst of
// truly concurrent callers yields exactly one winner. Use it where the throttle is
// the SOLE defense and each admitted call has a non-idempotent side effect (an OTP
// email): an allowed peek would let N goroutines pass together before any records
// and bomb a mailbox. The wake path can stay on allowed→record because its real
// gate is the running cap and its side effect (SetDesiredState) is idempotent. A
// non-positive window disables the throttle (the reservation is a no-op).
func (c *cooldownLimiter) reserve(name string, window time.Duration) (time.Time, bool) {
	if window <= 0 {
		return time.Time{}, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.last[name]; ok && c.now().Sub(last) < window {
		return time.Time{}, false
	}
	t := c.now()
	c.last[name] = t
	return t, true
}

// release rolls back a reservation made at reservedAt, but only if it is still the
// current one — a later reserve that superseded it is left intact. It lets a caller
// undo its hold when a downstream step fails, so a failed mint or delivery never
// consumes the window, without a slow failing caller clobbering a newer holder. A
// zero reservedAt (a disabled-window reserve) matches nothing and is a no-op.
func (c *cooldownLimiter) release(name string, reservedAt time.Time) {
	if reservedAt.IsZero() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.last[name]; ok && last.Equal(reservedAt) {
		delete(c.last, name)
	}
}

// ---- login concurrency cap ----

// concurrencyLimiter bounds how many holders may run a guarded section at once. It
// backs the public login route's bcrypt cap (handleLogin): a buffered channel of n
// tokens; acquire takes one WITHOUT blocking (returning ok=false when the section
// is already full), release returns it. Unlike cooldownLimiter — a per-key time
// window — this bounds simultaneity, not frequency, which is the right shape for a
// CPU-costly section a flood would otherwise pin every core running. A non-positive
// cap disables it (acquire always admits, release is a no-op), mirroring the "zero
// disables" idiom of WakeCooldown and MaxRunningServers.
type concurrencyLimiter struct {
	slots chan struct{}
}

// newConcurrencyLimiter builds a limiter admitting at most n concurrent holders. A
// non-positive n yields a disabled limiter (nil slots) that admits everyone.
func newConcurrencyLimiter(n int) *concurrencyLimiter {
	if n <= 0 {
		return &concurrencyLimiter{}
	}
	return &concurrencyLimiter{slots: make(chan struct{}, n)}
}

// acquire tries to take a slot without blocking. It returns a release func and true
// on success, or nil and false when the section is already at capacity. The disabled
// limiter (nil slots) always admits and returns a no-op release. release MUST be
// called exactly once on the success path, so it reads naturally as `release, ok :=
// l.acquire(); if !ok { shed }; defer/inline release()`.
func (l *concurrencyLimiter) acquire() (release func(), ok bool) {
	if l.slots == nil {
		return func() {}, true
	}
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, true
	default:
		return nil, false
	}
}

// ---- per-principal stream cap ----

// streamLimiter bounds how many concurrent guarded sections a single KEY may hold at
// once. It backs the per-principal SSE stream cap (console + build-log relays): each
// relay blocks for the life of a client's attachment and, under a stalled reader,
// pins a goroutine plus a kube-apiserver follow connection, so an unbounded number of
// them from one principal is a control-plane connection-exhaustion vector. Unlike the
// login concurrencyLimiter (a single global semaphore), this counts per key. A
// non-positive max disables it (acquire always admits, release is a no-op), the same
// "zero disables" idiom as the other levers.
type streamLimiter struct {
	mu  sync.Mutex
	n   map[string]int
	max int
}

// newStreamLimiter builds a per-key stream cap admitting at most max concurrent
// holders per key. A non-positive max yields a disabled limiter that admits everyone.
func newStreamLimiter(max int) *streamLimiter {
	return &streamLimiter{n: map[string]int{}, max: max}
}

// acquire reserves a slot for key. It returns a release func and true on success, or
// nil and false when key already holds max slots. A non-positive max disables the cap
// (always admits, no-op release). The returned release is guarded by a sync.Once, so
// a defer that runs it exactly once — or even twice on some paths — never
// over-decrements the counter.
func (l *streamLimiter) acquire(key string) (release func(), ok bool) {
	if l.max <= 0 {
		return func() {}, true
	}
	l.mu.Lock()
	if l.n[key] >= l.max {
		l.mu.Unlock()
		return nil, false
	}
	l.n[key]++
	l.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { l.release(key) }) }, true
}

// release returns one of key's slots. The counter entry is deleted when it reaches
// zero so the map does not accumulate a permanent entry per principal ever seen.
func (l *streamLimiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[key] <= 1 {
		delete(l.n, key)
		return
	}
	l.n[key]--
}

// ---- running-server cap ----

// withinRunningCap reports whether waking info's server is allowed under the
// global running-server cap (spec §9.1: the concurrency-上限 lever hangs on the
// same wake chokepoint as cooldown and autostartPolicy). The cap counts servers
// whose spec.desiredState is already Running — CRD lifecycle truth read through
// ListServers, never the Postgres business layer (spec §1) — and admits the wake
// only while that count stays below the cap.
//
// Two short-circuits keep it both correct and cheap:
//
//   - A non-positive cap disables the lever (the default; mirrors WakeCooldown's
//     "zero disables"). §9.2 wires only autostartPolicy + cooldown as active wake
//     gates, so the cap ships inert and a deployment opts in by setting a positive
//     value. Disabled, it never lists the cluster — the default wake path pays
//     nothing for a lever nobody turned on.
//   - A target already desired-Running is idempotent: re-waking it adds no load,
//     so it is always admitted and need not be counted (and a stop→wake flip of a
//     server that was the Nth running one is never wedged by its own slot).
//
// Like the cooldown limiter this is a soft throttle, not a transactional
// invariant: the count-then-flip is not atomic, so a burst of concurrent wakes
// can momentarily exceed the cap. That is acceptable because the operator
// reconcile is idempotent and the §18 reaper / §9.3 quota bound steady-state
// load; the cap exists to refuse an obvious flood, not to hold a hard ceiling.
func (a *API) withinRunningCap(ctx context.Context, info *ServerInfo) (bool, error) {
	if a.MaxRunningServers <= 0 {
		return true, nil
	}
	if info.DesiredState == string(v1alpha1.DesiredRunning) {
		return true, nil
	}
	servers, err := a.Cluster.ListServers(ctx)
	if err != nil {
		return false, err
	}
	running := 0
	for _, s := range servers {
		if s.DesiredState == string(v1alpha1.DesiredRunning) {
			running++
		}
	}
	return running < a.MaxRunningServers, nil
}
