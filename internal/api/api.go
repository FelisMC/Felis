// Package api implements felis-api: one binary serving two faces (spec §7).
//
// The internal face (velocity / backend callbacks) authenticates with a static
// per-caller service token and is never wrapped in Zero Trust. The external face
// (people / panel) authenticates the local session cookie; admin-tier operations
// additionally require a staff session on the operator console host (spec §14,
// graded by operation).
//
// Handlers depend on the Repo and Cluster interfaces, so the request routing,
// dual-face auth, input validation and authorization are all unit-tested with
// in-memory fakes. The Postgres (pgRepo) and controller-runtime (k8sCluster)
// implementations compile here but are exercised only by integration tests
// against a live database / cluster.
package api

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
)

// API holds the dependencies shared by every handler.
type API struct {
	Distribution Distribution
	Repo         Repo
	Cluster      Cluster
	Internal     InternalAuth
	External     ExternalAuth

	// Builder is the image build subsystem (spec §16). It is optional: when nil
	// the /images routes report 503 rather than 404, so the admin boundary is
	// still exercised even before the subsystem is wired in.
	Builder ImageBuilder

	// Images pins a whitelisted image ref to the digest it names when a server is
	// created or its image is changed (internal/imagepin), so a later push over
	// the same tag never reaches an existing world. Nil stores refs as given.
	Images ImagePinner

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

	// Backuper starts an on-demand world backup (spec §18/§19 WorldArchiver). Like
	// Restorer it is optional: when nil the backup route reports 503, so the backup
	// authorization boundary is exercised before the backup-Job executor is wired.
	Backuper Backuper

	// JobStatus reads the latest backup/restore Job outcomes for GET
	// /servers/{name}/jobs — the status outlet for async failures (the enqueue
	// endpoints only answer 202). Optional: nil → that route reports 503.
	JobStatus JobStatusReader

	// RestoreChains finds and settles the safety snapshots that run in front of a
	// restore (restorechain.go). A restore takes a snapshot first only when this
	// is set, the Backuper can chain a restore and the Restorer is wired, since
	// something has to start the restore once the snapshot is done.
	RestoreChains RestoreChains

	// Files is the server file manager (list, read, write, make a folder, delete,
	// rename and upload inside a stopped server's world volume).
	// Like Restorer and Backuper it is optional: when nil the file routes report
	// 503, so the owner-or-admin and stopped gates are exercised before the
	// file-Job executor is wired. Unlike them its calls are synchronous, because
	// the caller wants the listing or the bytes back, not a 202.
	Files FileEditor
	// FileStage holds uploads until the Job landing them fetches them, and
	// InternalBaseURL is where that Job reaches felis-api's internal face to do so
	// (handleUploadFile). Uploads report 503 unless both are set.
	FileStage       *fileedit.Stage
	InternalBaseURL string

	// Exporter starts the Job behind a world or backup download (exports.go),
	// which PUTs the archive to InternalBaseURL. Optional like Restorer: the
	// export routes report 503 unless both are set, after the owner-or-admin,
	// backup and stopped gates.
	Exporter Exporter

	// Schedules stores the servers' scheduled tasks (schedules.go), which
	// RunSchedules fires. Optional: when nil the schedule routes report 503 and
	// RunSchedules does nothing.
	Schedules ServerSchedules

	// Submissions is the user-modpack approval lane (a user-directed extension over
	// the §16 build subsystem; see internal/submit). It is optional: when
	// nil the /me/submissions and /submissions routes report 503 rather than 404, so
	// the app/admin boundary is still exercised even before the subsystem is wired
	// in. An ordinary user may only SUBMIT here; an admin approves before any build
	// runs, distinct from Builder which an admin drives directly.
	Submissions SubmissionService

	// Mailer delivers player email one-time codes (spec §B2 onboarding). It is
	// optional: when nil (no [smtp] relay) every door that mails a code answers 503
	// mail_unavailable before minting one, auth options stops offering email_otp,
	// and a verified email stops counting as a reauth factor. The code is never
	// returned to the client or logged.
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

	// AdminHostname is the operator console host (op.console.<root_domain>) from
	// config. requireExternal refuses any non-admin principal that arrives on it,
	// so op.console is staff-only at the DOOR — not merely per-route — even on the
	// passwordless demo face where Cloudflare Access is not fronting it. Empty
	// falls back to op.console.<RootDomain> (see hostIsAdminConsole). On the player
	// console (console.<root_domain>) the gate is inert.
	AdminHostname string

	// PanelHostname is the player console host (console.<root_domain>) from
	// config. Used to render user-facing panel URLs (the /link code's panel_url
	// hint); empty falls back to console.<RootDomain> (see panelURL), mirroring
	// AdminHostname's fallback.
	PanelHostname string

	// WakeCooldown throttles repeated wakes per server (spec §9.1: cooldown hangs
	// on the wake lever). Zero disables throttling.
	WakeCooldown time.Duration

	// BackupCooldown spaces out an owner's on-demand backups of one server, and
	// BackupStoreCap refuses them once the present backups reach [archive]
	// max_local_bytes (data-durability-9): each archive lands on the node disk
	// the worlds and the database share. Admins and the break-glass console are
	// exempt. Zero disables each lever.
	BackupCooldown time.Duration
	BackupStoreCap int64

	// SubmitCreateCooldown / SubmitUploadCooldown throttle the user-modpack
	// submission lane per user: create bounds how quickly review-queue rows can
	// appear, upload bounds how often a user may stream a (up to 1 GiB) build
	// context. The keys are separate, so the lane's normal shape — create, then
	// upload — is never blocked by its own throttle. Zero disables each lever
	// (the same idiom as WakeCooldown); cmd/felis wires positive values.
	SubmitCreateCooldown time.Duration
	SubmitUploadCooldown time.Duration

	// MaxRunningServers caps how many servers may be desired-Running cluster-wide
	// (spec §9.1: the concurrency-上限 lever hanging on the same wake chokepoint as
	// cooldown and autostartPolicy). Zero — the default — disables it: §9.2 wires
	// only autostartPolicy + cooldown as active wake gates, so this lever ships
	// inert, exactly like a zero WakeCooldown, and a deployment opts in by setting
	// a positive value. Enforced via withinRunningCap on the wake path.
	MaxRunningServers int

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

	// AuthSources is the Felis-nano multi-source hasJoined multiplexer's upstream
	// Yggdrasil list, in priority order (config order; the Mojang Identity source
	// first for 正版优先). Nil makes the session verifier reject every login (204);
	// cmd/felis always wires at least the Mojang source through authSourcesFromConfig.
	// Consumed by handleHasJoined (handlers_hasjoined.go).
	AuthSources []AuthSource

	// AuthDoorLimit bounds how often one client address may call the public
	// pre-session auth doors (ratelimit.go). MailLimit bounds all mail the API
	// sends, install-wide. Zero values disable them; cmd/felis wires both.
	AuthDoorLimit RateLimit
	MailLimit     RateLimit
	// ClientIPHeader names the header the install's edge writes the client
	// address into (CF-Connecting-IP behind the Cloudflare tunnel,
	// X-Forwarded-For behind an operator proxy). Empty means the TCP peer.
	ClientIPHeader string

	// AccessLog receives one line per API request (observe.go). Nil logs logfmt
	// to stderr.
	AccessLog *slog.Logger

	// Now is the clock, injectable for tests. Defaults to time.Now.
	Now func() time.Time

	cooldownOnce sync.Once
	cooldown     *cooldownLimiter

	otpCooldownOnce sync.Once
	otpCooldown     *cooldownLimiter

	submitCooldownOnce sync.Once
	submitCooldown     *cooldownLimiter

	streamCapOnce sync.Once
	streamCap     *streamLimiter

	exportsOnce sync.Once
	exports     *exportRegistry

	authDoorOnce    sync.Once
	authDoorBuckets *bucketSet
	mailOnce        sync.Once
	mailBuckets     *bucketSet

	// ownerBound caches the first "an Owner exists" answer (handleOwnerStatus).
	ownerBound atomic.Bool

	drainInit  sync.Once
	drainClose sync.Once
	drain      chan struct{}
}

// streamsClosing is closed once CloseStreams runs.
func (a *API) streamsClosing() <-chan struct{} {
	a.drainInit.Do(func() { a.drain = make(chan struct{}) })
	return a.drain
}

// CloseStreams ends every log stream this API is relaying, now and from now on.
// http.Server.Shutdown waits for handlers to return and cancels nothing, so a
// console left open would hold the process until the pod's grace period ran out;
// register this with RegisterOnShutdown. The EventSource on the other end
// reconnects, and resumes from its Last-Event-ID on the next instance.
func (a *API) CloseStreams() {
	a.streamsClosing()
	a.drainClose.Do(func() { close(a.drain) })
}

// panelURL returns the public player-console origin ("https://console.<root>"),
// preferring the configured PanelHostname and falling back to the conventional
// console.<RootDomain> label — the same convention hostIsAdminConsole applies
// to the operator host. Empty when neither is configured (a bare test API).
func (a *API) panelURL() string {
	host := a.PanelHostname
	if host == "" && a.RootDomain != "" {
		host = "console." + a.RootDomain
	}
	if host == "" {
		return ""
	}
	return "https://" + host
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

// submitLimiter lazily builds a SEPARATE cooldown limiter for the user-modpack
// submission lane, so its throttles never share state with the wake or OTP
// keyspaces. One limiter backs both levers with prefixed keys (see the
// submissionCreateKey/UploadKey constants), so create and upload never contend
// with each other. Like the other cooldowns it is process-local; with multiple
// api replicas the effective spacing is per-replica, the same accepted
// KNOWN-LIMITATION the OTP resend throttle carries.
func (a *API) submitLimiter() *cooldownLimiter {
	a.submitCooldownOnce.Do(func() {
		a.submitCooldown = &cooldownLimiter{now: a.now, last: map[string]time.Time{}}
	})
	return a.submitCooldown
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
// stable user id and falls back to the email so a principal without a user id is
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

	// Owner marks an external-face route that requires the platform-level owner
	// role (Principal.IsOwner()). It is orthogonal to Admin: an owner
	// intrinsically passes the admin ZT gate (IsAdmin() accepts both admin and
	// owner), so a route that sets Owner does not also need Admin. Mixing both
	// on one route is harmless but redundant — an owner passes both.
	Owner bool

	// SetupAllowed marks a route as reachable during the setup-lockdown: a session
	// whose EmailVerified is false is restricted to these routes only.
	SetupAllowed bool

	// AuthDoor marks a public pre-session auth door: it is rate limited per
	// client address (throttleAuthDoor). The op-login status poll is left off,
	// since the browser calls it every few seconds while it waits.
	AuthDoor bool

	// Callers lists the machines an internal-face route serves; every
	// authenticated internal route names at least one, and external routes none.
	Callers []Caller

	h http.HandlerFunc
}

// internalAPIRoutes is the internal face's served route table (spec §7, §14):
// service-token auth, never Zero Trust. It carries both health probes and the
// metrics scrape.
func (a *API) internalAPIRoutes() []apiRoute {
	// Who may call what (Caller). The proxy drives the game-facing routes; the
	// login gate only checks a joining player's bar and link and mints their bind
	// code; the build Job only reads the context of the submission it builds; the
	// on-node console only asks for a break-glass backup.
	proxy := []Caller{CallerVelocity}
	gate := []Caller{CallerVelocity, CallerLimbo}
	build := []Caller{CallerBuild}
	ops := []Caller{CallerOps}
	return []apiRoute{
		{Method: "GET", Pattern: "/healthz", Public: true, h: a.handleHealthz},
		{Method: "GET", Pattern: "/readyz", Public: true, h: a.handleReadyz},
		// Prometheus scrape (felis_* collectors); public because a scrape carries
		// no token, internal-only so it is never exposed off-cluster.
		{Method: "GET", Pattern: "/metrics", Public: true, h: a.handleMetrics},

		{Method: "GET", Pattern: "/api/v1/servers", Callers: proxy, h: a.handleListServers},
		// The build Pod's context-fetch initContainer streams a submission's stored
		// modpack through this route (build namespace cannot mount the uploads PVC).
		{Method: "GET", Pattern: "/api/v1/internal/submissions/{id}/context", Callers: build, h: a.handleInternalSubmissionContext},
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/ready", Callers: proxy, h: a.handleReady},
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/join-event", Callers: proxy, h: a.handleJoinEvent},
		// Domain-autostart (spec §9.1, §14): velocity drives the wake lever and polls
		// status with its service token, identifying the joining player by online-mode
		// UUID. These live on the internal face because velocity holds no web Principal;
		// the external face keeps its own Principal-gated wake/status for the panel.
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/wake", Callers: proxy, h: a.handleInternalWake},
		{Method: "GET", Pattern: "/api/v1/internal/servers/{name}/status", Callers: proxy, h: a.handleStatus},
		// Lobby `/menu` (spec §12): the felis-paper lobby is a pure UI face holding no
		// token, so velocity drives these on its behalf — claim by online-mode UUID
		// (the lobby's `Claim & Start`, separate from the autostartPolicy-gated wake)
		// and the menu projection that adds the ownership-derived `claimable` the §11
		// list/status views never carry.
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/claim", Callers: proxy, h: a.handleInternalClaim},
		{Method: "GET", Pattern: "/api/v1/internal/servers/{name}/menu", Callers: proxy, h: a.handleInternalMenuStatus},
		// What this player may start, for every tile at once: one call per menu open.
		{Method: "GET", Pattern: "/api/v1/internal/player/menu-access/{mc_uuid}", Callers: proxy, h: a.handleInternalMenuAccess},
		// Account linking (spec §10): the in-game /link side mints a one-time code for a
		// verified UUID. Internal-only — the code is born from an online-mode UUID the
		// web never holds (account_link_codes has no user_id column).
		{Method: "POST", Pattern: "/api/v1/internal/account/link/code", Callers: gate, h: a.handleCreateLinkCode},
		// QR scan-to-login completion poll (spec §B3 player game-login). After the player
		// scans the QR-encoded code and the web verify writes the durable link, velocity
		// polls this for the UUID it minted against and admits on {linked:true}. Read-only
		// and keyed by the verified UUID (not the scanned code), so it consumes nothing
		// and is safe to poll repeatedly.
		{Method: "GET", Pattern: "/api/v1/internal/account/link/status/{mc_uuid}", Callers: gate, h: a.handleLinkStatus},
		// Account migration (spec §B3 inherit), in-game side: /felis migrate puts the
		// account linked to the running player's verified UUID into migrate mode. Internal
		// only — the initiator is proven by online-mode auth, and the sensitive proof
		// (step-up) still happens web-side before anything transfers.
		{Method: "POST", Pattern: "/api/v1/internal/account/migrate/start", Callers: proxy, h: a.handleMigrateStart},
		// Username-collision reclaim (spec §B3): velocity records a Mojang-priority
		// reclaim (bar the squatter UUID + stash its data for 30 days) and gates the
		// limbo login by checking whether a connecting UUID was barred. Internal-only —
		// velocity holds a service token, and the bar is keyed by UUID so the genuine
		// Mojang player (same name, different UUID) always passes.
		{Method: "POST", Pattern: "/api/v1/internal/player/reclaim", Callers: proxy, h: a.handleReclaimUsername},
		{Method: "GET", Pattern: "/api/v1/internal/player/blacklist/{mc_uuid}", Callers: gate, h: a.handleCheckBlacklist},
		// Felis-nano multi-source session verifier, behind player game-login. Velocity is
		// pointed here with -Dmojang.sessionserver and issues the request itself; it speaks
		// the vanilla sessionserver protocol and carries no token, so this is Public. It
		// fans hasJoined out to the configured Yggdrasil roots (Mojang-first) and rewrites
		// third-party UUIDs into a per-source namespace before returning the canonical
		// profile (handlers_hasjoined.go).
		{Method: "GET", Pattern: "/session/minecraft/hasJoined", Public: true, h: a.handleHasJoined},
		// Op-login (passwordless op.console login): a staff member starts the login
		// on the web, and an ONLINE in-game admin vouches for it via velocity's
		// /felis web op approve. Internal face carries the pending queue and the
		// approve action (service-token auth, no Principal); the public face carries
		// the start/status/finish the staff member's browser drives.
		{Method: "GET", Pattern: "/api/v1/internal/op-login/pending", Callers: proxy, h: a.handleOpLoginPending},
		{Method: "GET", Pattern: "/api/v1/internal/op-login/{id}", Callers: proxy, h: a.handleOpLoginShow},
		{Method: "POST", Pattern: "/api/v1/internal/op-login/{id}/approve", Callers: proxy, h: a.handleOpLoginApprove},

		// Break-glass backup (spec §B4 "Sync"): the on-node console POSTs here to
		// snapshot a stopped world while the API is alive. Service-token auth (no
		// Principal); the shared enqueueBackup tail enforces the RWO stopped-gate.
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/backup", Callers: ops, h: a.handleInternalBackup},

		// A file upload's staged bytes, fetched once by the Job landing them, which
		// then reports them landed so a file sent in parts is deleted. Public
		// because that Job holds no service token; the one-time bearer token minted
		// with the upload is the check (handlers_files.go).
		{Method: "GET", Pattern: "/api/v1/internal/file-uploads/{id}", Public: true, h: a.handleInternalFileUpload},
		{Method: "DELETE", Pattern: "/api/v1/internal/file-uploads/{id}", Public: true, h: a.handleInternalFileUploadLanded},

		// An export Job's archive, held open until the owner's browser downloads
		// it. Public for the same reason as file uploads: the Job holds no service
		// token, and the one-time bearer token minted with the export is the check
		// (exports.go). The body is read at the browser's pace, so the handler
		// lifts the minimum-rate body deadline and applies its own stall bound.
		{Method: "PUT", Pattern: "/api/v1/internal/exports/{id}", Public: true, h: a.handleInternalExportUpload},
	}
}

// externalAPIRoutes is the external face's served route table (spec §7, §14):
// session auth on every non-public /api/v1 route; the Admin entries are
// additionally gated on the operator console host. It exposes liveness only —
// readiness is an internal concern.
func (a *API) externalAPIRoutes() []apiRoute {
	return []apiRoute{
		{Method: "GET", Pattern: "/healthz", Public: true, h: a.handleHealthz},

		// logout is Public: it reads the cookie directly so it works even after
		// expiry. The rest of the auth surface (identifier-first options discovery,
		// setup redeem/status, passkey login, email OTP login, op-login) is Public and
		// pre-session: a caller has no principal yet.
		{Method: "POST", Pattern: "/api/v1/auth/logout", Public: true, h: a.handleLogout},
		// Identifier-first discovery (#71): given an email, report which console methods
		// it can use so the SPA prompts for the right authenticator. The deliberate
		// counter-slice to the anti-enumeration doors — the ONE sanctioned place existence
		// is disclosed — but it never reveals staffness (methods computed with no role
		// branch, so a staff and a player address in the same state are indistinguishable).
		{Method: "POST", Pattern: "/api/v1/auth/options", Public: true, AuthDoor: true, h: a.handleAuthOptions},
		{Method: "POST", Pattern: "/api/v1/auth/setup/redeem", Public: true, AuthDoor: true, h: a.handleSetupRedeem},
		{Method: "GET", Pattern: "/api/v1/auth/setup/status", SetupAllowed: true, h: a.handleSetupStatus},
		// Whether an Owner is bound yet: before one is, every login door here is off, and the
		// sign-in page says so instead of offering them (handlers_auth_owner.go).
		{Method: "GET", Pattern: "/api/v1/auth/owner-status", Public: true, h: a.handleOwnerStatus},
		{Method: "POST", Pattern: "/api/v1/auth/passkey/login/begin", Public: true, AuthDoor: true, h: a.handlePasskeyLoginBegin},
		{Method: "POST", Pattern: "/api/v1/auth/passkey/login/finish", Public: true, AuthDoor: true, h: a.handlePasskeyLoginFinish},
		// Discoverable ("usernameless") passkey login (task #40): the from-zero sibling of the
		// email-first pair above — no identifier typed, the account is resolved from the
		// userHandle inside the signed assertion (handlers_passkey_discoverable.go).
		{Method: "POST", Pattern: "/api/v1/auth/passkey/login/discoverable/begin", Public: true, AuthDoor: true, h: a.handlePasskeyLoginDiscoverableBegin},
		{Method: "POST", Pattern: "/api/v1/auth/passkey/login/discoverable/finish", Public: true, AuthDoor: true, h: a.handlePasskeyLoginDiscoverableFinish},
		{Method: "POST", Pattern: "/api/v1/auth/email/start", Public: true, AuthDoor: true, h: a.handleLoginEmailStart},
		{Method: "POST", Pattern: "/api/v1/auth/email/verify", Public: true, AuthDoor: true, h: a.handleLoginEmailVerify},
		{Method: "POST", Pattern: "/api/v1/auth/op-login/start", Public: true, AuthDoor: true, h: a.handleOpLoginStart},
		{Method: "GET", Pattern: "/api/v1/auth/op-login/status/{id}", Public: true, h: a.handleOpLoginStatus},
		{Method: "POST", Pattern: "/api/v1/auth/op-login/finish", Public: true, AuthDoor: true, h: a.handleOpLoginFinish},
		// Player-console bootstrap (console-tier access model): the account-less
		// player's door into console.<root_domain>. Public — like login there is no prior
		// principal — and session-minting, but the artifact it consumes is a one-time
		// Bind Code minted internal-face against an online-mode-verified UUID, so
		// possession already proves a Minecraft identity. A code whose UUID belongs to
		// staff is refused (403) so this never yields an admin session; op.console stays
		// behind Zero Trust (handlers_onboard.go).
		{Method: "POST", Pattern: "/api/v1/auth/bind", Public: true, AuthDoor: true, h: a.handleBindRedeem},

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
		{Method: "GET", Pattern: "/api/v1/servers/{name}/access/players", h: a.handleAccessPlayers},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/kick", h: a.handleAccessKick},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/ban", h: a.handleAccessBan},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/access/ban", h: a.handleAccessBanList},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/permission", h: a.handleAccessPermission},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/group", h: a.handleAccessGroup},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/access/luckperms/{player}", h: a.handleAccessLuckPermsInfo},
		// Wake allowlist (autostartPolicy=allowlist): Felis's own Postgres record of
		// who may wake the server, owner/admin-gated inside the handlers like the
		// access routes above (handlers_allowlist.go).
		{Method: "GET", Pattern: "/api/v1/servers/{name}/allowlist", h: a.handleAllowlistList},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/allowlist/{uuid}", h: a.handleAllowlistSetWake},
		// Retirement: the owner gives the server up, or an admin deletes it. The
		// request is recorded and the reaper carries it out on its next run
		// (handlers_retire.go); owner/admin-gated inside the handlers.
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/retirement", h: a.handleRetire},
		{Method: "DELETE", Pattern: "/api/v1/servers/{name}/retirement", h: a.handleCancelRetire},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/status", h: a.handleStatus},
		// Identity self-read (spec §14 tiering): the panel reads this once at boot to
		// learn its own tier and decide which navigation surfaces to render. App-tier —
		// every authenticated principal may read its OWN identity. is_admin is the
		// server-computed Principal.IsAdmin() (Role + admin Access path), so the client
		// never re-derives the graded-ZT rule; it remains UX truth, not enforcement.
		// /me is reachable during setup-lockdown so the panel can read its own
		// identity (including email_verified) to drive the setup flow.
		{Method: "GET", Pattern: "/api/v1/me", SetupAllowed: true, h: a.handleMe},
		{Method: "GET", Pattern: "/api/v1/me/servers", SetupAllowed: true, h: a.handleMyServers},
		// World backups (spec §7, §466). All are app-tier: GET /backups is scoped
		// inside the handler (admin sees all; a user sees only worlds they formerly
		// owned), DELETE takes the same scope, and restore is gated by owner-or-admin
		// PLUS a former-owner match, so none sits behind adminOnly.
		{Method: "GET", Pattern: "/api/v1/backups", h: a.handleListBackups},
		{Method: "DELETE", Pattern: "/api/v1/backups/{id}", h: a.handleDeleteBackup},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/jobs", h: a.handleServerJobs},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/restore-backup", h: a.handleRestoreBackup},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/backup", h: a.handleBackupNow},
		// World export (exports.go): download a backup, or a stopped server's world
		// as it is now, straight to the browser. App-tier with the restore gate
		// inside each start route; the ticket routes answer only the user who
		// started the export.
		{Method: "POST", Pattern: "/api/v1/servers/{name}/backups/{id}/export", h: a.handleExportBackup},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/world/export", h: a.handleExportWorld},
		{Method: "GET", Pattern: "/api/v1/exports/{ticket}", h: a.handleExportStatus},
		{Method: "GET", Pattern: "/api/v1/exports/{ticket}/download", h: a.handleExportDownload},
		// Server file manager: list, read, write, make a folder, delete, rename,
		// upload, unzip and download in a STOPPED server's world volume (handlers_files.go). App-tier,
		// exactly like the backup pair above and for the same reason — every route
		// gates on owner-or-admin inside the handler, so an owner repairs their own
		// broken server without an admin's Zero-Trust path. The path travels as ?path=
		// rather than a segment because a file path contains '/' (the same reason
		// DELETE /images takes ?ref=). {name}/files is the directory face; {name}/file
		// is the single-file face.
		//
		// "Config editor" undersells the surface, so be precise about what app-tier
		// now reaches: the mount is the server's WHOLE working directory, not a
		// config subtree, so a write can place a loadable plugin jar (a deliberate
		// capability — see the op list in fileedit/exec.go) and a read can pull any
		// file in it. Exactly one path is denied, config/paper-global.yml, because it
		// holds the cluster-wide forwarding secret and is therefore the one thing in
		// the mount that is not the caller's own data (fileedit.secretConfigPath).
		{Method: "GET", Pattern: "/api/v1/servers/{name}/files", h: a.handleListFiles},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/file", h: a.handleReadFile},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/file", h: a.handleWriteFile},
		{Method: "DELETE", Pattern: "/api/v1/servers/{name}/file", h: a.handleDeleteFile},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/mkdir", h: a.handleMkdir},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/rename", h: a.handleRenameFile},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/files/upload", h: a.handleUploadFile},
		// A file too big for one request goes up in parts as an upload session, and
		// lands, like an unzip, as a Job the request does not wait on; files/ops
		// reports how those went (handlers_fileops.go).
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/uploads", h: a.handleBeginFileUpload},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/files/uploads/{id}", h: a.handleFileUploadStatus},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/files/uploads/{id}", h: a.handleFileUploadPart},
		{Method: "DELETE", Pattern: "/api/v1/servers/{name}/files/uploads/{id}", h: a.handleDropFileUpload},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/uploads/{id}/commit", h: a.handleCommitFileUpload},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/unzip", h: a.handleUnzipFile},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/files/ops", h: a.handleListFileOps},
		// A file or folder download is an export (exports.go): it answers 202
		// with a ticket the export routes above serve.
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/download", h: a.handleDownloadFile},
		// Scheduled tasks (handlers_schedules.go): a console command, restart, stop,
		// start or backup at set times, which felis-api's runner fires. App-tier and
		// owner-or-admin inside the handler, like the console and power routes they
		// automate; a schedule reaches nothing its owner could not do by hand.
		{Method: "GET", Pattern: "/api/v1/servers/{name}/schedules", h: a.handleListSchedules},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/schedules", h: a.handleCreateSchedule},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/schedules/{id}", h: a.handleUpdateSchedule},
		{Method: "DELETE", Pattern: "/api/v1/servers/{name}/schedules/{id}", h: a.handleDeleteSchedule},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/schedules/{id}/run", h: a.handleRunSchedule},
		// Account linking (spec §10), web side: /start reports link status (it is the
		// pointer handleClaim's 412 emits), /verify consumes the in-game code and binds
		// the account. App-tier, not admin — linking your own account is an ordinary
		// authenticated operation.
		{Method: "POST", Pattern: "/api/v1/account/link/start", SetupAllowed: true, h: a.handleLinkStart},
		{Method: "POST", Pattern: "/api/v1/account/link/verify", SetupAllowed: true, h: a.handleLinkVerify},
		// Email verification (spec §B2 onboarding), web side: /start mints+delivers a
		// one-time code for the caller's chosen address, /verify redeems it and flips
		// email_verified. App-tier like the link routes — proving control of your own
		// email is an ordinary authenticated operation, scoped to the principal.
		{Method: "POST", Pattern: "/api/v1/account/email/start", SetupAllowed: true, h: a.handleEmailOTPStart},
		{Method: "POST", Pattern: "/api/v1/account/email/verify", SetupAllowed: true, h: a.handleEmailOTPVerify},
		// Record-only email: the setup wizard's Step 1 stores the Owner's address
		// UNVERIFIED (no SMTP at bootstrap ⇒ no code to mail). email_verified stays
		// false until a later Settings/SMTP flow proves control via /email/verify above.
		{Method: "POST", Pattern: "/api/v1/account/email", SetupAllowed: true, h: a.handleSetEmail},
		// Passkey enrollment (spec §14 WebAuthn / Phase 6 bind), web side: /register/begin
		// mints a credential-creation challenge for the caller, /register/finish verifies
		// the authenticator's attestation and binds the passkey, and the credentials
		// collection lists and unbinds the caller's OWN passkeys. App-tier like the email
		// routes — binding a passkey to your own account is an ordinary authenticated
		// operation, scoped entirely to the principal (the body never names a user). This
		// is the ENROLLMENT side; the passkey LOGIN/assertion door is the Public,
		// pre-session /api/v1/auth/passkey/login/{begin,finish} pair above.
		{Method: "POST", Pattern: "/api/v1/account/passkey/register/begin", SetupAllowed: true, h: a.handlePasskeyRegisterBegin},
		{Method: "POST", Pattern: "/api/v1/account/passkey/register/finish", SetupAllowed: true, h: a.handlePasskeyRegisterFinish},
		{Method: "GET", Pattern: "/api/v1/account/passkey/credentials", SetupAllowed: true, h: a.handlePasskeyList},
		{Method: "DELETE", Pattern: "/api/v1/account/passkey/credentials/{id}", SetupAllowed: true, h: a.handlePasskeyDelete},
		// Reauth (reauth.go): the fresh proof that passkey enrollment and removal and an
		// email change require once the account has a factor. Status says whether one
		// is needed and how to give it; the pairs below take a passkey assertion or an
		// email code and mark the caller's session. SetupAllowed like the routes they
		// unlock.
		{Method: "GET", Pattern: "/api/v1/account/reauth", SetupAllowed: true, h: a.handleReauthStatus},
		{Method: "POST", Pattern: "/api/v1/account/reauth/passkey/begin", SetupAllowed: true, h: a.handleReauthPasskeyBegin},
		{Method: "POST", Pattern: "/api/v1/account/reauth/passkey/finish", SetupAllowed: true, h: a.handleReauthPasskeyFinish},
		{Method: "POST", Pattern: "/api/v1/account/reauth/email/start", SetupAllowed: true, h: a.handleReauthEmailStart},
		{Method: "POST", Pattern: "/api/v1/account/reauth/email/verify", SetupAllowed: true, h: a.handleReauthEmailVerify},
		// The caller's own sessions (handlers_account_sessions.go): list every signed-in
		// device and sign out one or all the others. App-tier and scoped to the caller
		// inside the handler, like the passkey routes above.
		{Method: "GET", Pattern: "/api/v1/account/sessions", h: a.handleListMySessions},
		{Method: "DELETE", Pattern: "/api/v1/account/sessions/{hash}", h: a.handleRevokeMySession},
		{Method: "POST", Pattern: "/api/v1/account/sessions/revoke-others", h: a.handleRevokeMyOtherSessions},
		// Account migration (spec §B3 inherit), web side. App-tier, principal-scoped: the
		// SOURCE drives status → step-up confirm (passkey forced when enrolled, else
		// email-OTP) → issue-code+name-target; the TARGET drives redeem as itself. Not
		// SetupAllowed — migrating is a normal post-onboarding operation, never part of
		// lockdown enrollment. The step-up is a FRESH proof, so a stolen session alone
		// cannot advance a migration.
		{Method: "GET", Pattern: "/api/v1/account/migrate", h: a.handleMigrateStatus},
		{Method: "POST", Pattern: "/api/v1/account/migrate/confirm/otp/start", h: a.handleMigrateConfirmOTPStart},
		{Method: "POST", Pattern: "/api/v1/account/migrate/confirm/otp/verify", h: a.handleMigrateConfirmOTPVerify},
		{Method: "POST", Pattern: "/api/v1/account/migrate/confirm/passkey/begin", h: a.handleMigrateConfirmPasskeyBegin},
		{Method: "POST", Pattern: "/api/v1/account/migrate/confirm/passkey/finish", h: a.handleMigrateConfirmPasskeyFinish},
		{Method: "POST", Pattern: "/api/v1/account/migrate/issue-code", h: a.handleMigrateIssueCode},
		{Method: "POST", Pattern: "/api/v1/account/migrate/redeem", h: a.handleMigrateRedeem},
		// Modpack submission (user-directed lane over §16), user side: a user files an upload for review
		// and lists their own. App-tier — the submitter and the "my uploads" scope are
		// both taken from the principal, never the body, so an ordinary authenticated
		// session is the correct gate (the admin verdict lives below, behind adminOnly).
		{Method: "POST", Pattern: "/api/v1/me/submissions", h: a.handleCreateSubmission},
		{Method: "GET", Pattern: "/api/v1/me/submissions", h: a.handleMySubmissions},
		{Method: "GET", Pattern: "/api/v1/me/submissions/limits", h: a.handleSubmissionLimits},
		// The blob upload for a submission the caller owns: the request body is the
		// raw gzip build context, streamed to the derived, id-namespaced location.
		// App-tier and owner-scoped (the id must belong to the principal), exactly
		// like the create/list routes above.
		{Method: "POST", Pattern: "/api/v1/me/submissions/{id}/context", h: a.handleUploadSubmissionContext},
		// The chunked form of that upload, for a context larger than one request
		// carries through the edge (Cloudflare refuses bodies over 100 MB): GET
		// reports the staged length (the resume point), PUT ?offset= appends one
		// part, POST .../complete stores the staged whole. Same owner scoping.
		{Method: "GET", Pattern: "/api/v1/me/submissions/{id}/context/upload", h: a.handleContextUploadStatus},
		{Method: "PUT", Pattern: "/api/v1/me/submissions/{id}/context/upload", h: a.handleContextUploadPart},
		{Method: "POST", Pattern: "/api/v1/me/submissions/{id}/context/upload/complete", h: a.handleContextUploadComplete},
		// Withdraw the caller's OWN pending submission: the row and its uploaded
		// context are deleted, freeing the pending slot and storage budget. Same
		// owner-scoping as the upload route — a reviewed submission is frozen (409)
		// and another user's id is invisible (404).
		{Method: "DELETE", Pattern: "/api/v1/me/submissions/{id}", h: a.handleWithdrawSubmission},
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
		{Method: "GET", Pattern: "/api/v1/nodes", Admin: true, h: a.handleNodes},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/migrations", Admin: true, h: a.handleMigrationStatus},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/migrations", Admin: true, h: a.handleMigration},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/migrations/{id}", Admin: true, h: a.handleMigrationStatus},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/migrations/{id}/retry", Admin: true, h: a.handleMigrationRetry},
		{Method: "GET", Pattern: "/api/v1/fleet", Admin: true, h: a.handleFleet},
		// Image build + whitelist (spec §16, §15). Every route is admin-tier: a build
		// is build-time RCE against the cluster, so submission requires the admin
		// Zero-Trust path, not merely an authenticated session.
		{Method: "POST", Pattern: "/api/v1/images/build", Admin: true, h: a.handleBuildImage},
		{Method: "GET", Pattern: "/api/v1/images/build", Admin: true, h: a.handleListBuilds},
		{Method: "GET", Pattern: "/api/v1/images/build/{id}", Admin: true, h: a.handleGetBuild},
		{Method: "GET", Pattern: "/api/v1/images/build/{id}/logs", Admin: true, h: a.handleBuildLogs},
		{Method: "POST", Pattern: "/api/v1/images/build/{id}/cancel", Admin: true, h: a.handleCancelBuild},
		{Method: "GET", Pattern: "/api/v1/images/build/{id}/scan", Admin: true, h: a.handleBuildScan},
		{Method: "GET", Pattern: "/api/v1/images/build/{id}/scan/report", Admin: true, h: a.handleBuildScanReport},
		{Method: "GET", Pattern: "/api/v1/images/build/{id}/sbom", Admin: true, h: a.handleBuildSBOM},
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
		// Retire a submission outright (row + uploaded context), any status. The
		// lane's lifecycle valve: without it, rejected/consumed uploads accumulated
		// on the uploads PVC forever — there is no other delete path.
		{Method: "DELETE", Pattern: "/api/v1/submissions/{id}", Admin: true, h: a.handleDeleteSubmission},
		// The reviewer's read path to the uploaded blob: the executed Dockerfile
		// lives inside it, so approval would otherwise be blind.
		{Method: "GET", Pattern: "/api/v1/submissions/{id}/context", Admin: true, h: a.handleAdminSubmissionContext},
		// Auto-update maintenance window (spec §B; decision core internal/updates).
		// Admin-tier: it governs whether Felis may apply an update to itself, so setting
		// it requires the admin Zero-Trust path, not a mere session. Advisory: `felis
		// update` on the host reads it and warns before an apply outside it.
		{Method: "GET", Pattern: "/api/v1/updates/window", Admin: true, h: a.handleGetUpdateWindow},
		{Method: "PUT", Pattern: "/api/v1/updates/window", Admin: true, h: a.handleSetUpdateWindow},
		// The newest version check felis-update-check.timer recorded on the host.
		{Method: "GET", Pattern: "/api/v1/updates/report", Admin: true, h: a.handleGetUpdateReport},
		// Control-plane database backup freshness, as the host's felis-db-backup.timer
		// last recorded it. Admin-tier: it names the host backup directory.
		{Method: "GET", Pattern: "/api/v1/platform/db-backup", Admin: true, h: a.handleGetDBBackup},

		// User admin (spec §7, owner-only). Every route gates on the admin Zero-Trust
		// path AND the owner role: listing, mutating, disabling, or deleting users is
		// an owner-tier operation (one level above admin).
		{Method: "GET", Pattern: "/api/v1/users", Owner: true, h: a.handleListUsers},
		{Method: "POST", Pattern: "/api/v1/users", Owner: true, h: a.handleCreateUser},
		{Method: "GET", Pattern: "/api/v1/users/{id}", Owner: true, h: a.handleGetUser},
		{Method: "PATCH", Pattern: "/api/v1/users/{id}", Owner: true, h: a.handlePatchUser},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}", Owner: true, h: a.handleDeleteUser},
		{Method: "POST", Pattern: "/api/v1/users/{id}/disable", Owner: true, h: a.handleDisableUser},
		{Method: "GET", Pattern: "/api/v1/users/{id}/quotas", Owner: true, h: a.handleGetQuotas},
		{Method: "PUT", Pattern: "/api/v1/users/{id}/quotas", Owner: true, h: a.handleSetQuotas},
		{Method: "GET", Pattern: "/api/v1/users/{id}/sessions", Owner: true, h: a.handleListUserSessions},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}/sessions", Owner: true, h: a.handleRevokeUserSessions},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}/sessions/{hash}", Owner: true, h: a.handleRevokeUserSession},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}/passkeys", Owner: true, h: a.handleUnbindUserPasskeys},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}/links/{mc_uuid}", Owner: true, h: a.handleUnlinkAccount},
		{Method: "POST", Pattern: "/api/v1/users/{id}/links", Owner: true, h: a.handleLinkAccount},
	}
}

// InternalHandler builds the internal-face http.Handler: service-token auth, no
// Zero Trust (spec §14 red line). /healthz and /readyz are unauthenticated.
func (a *API) InternalHandler() http.Handler {
	return a.buildFace("internal", a.internalAPIRoutes(), a.requireInternal)
}

// ExternalHandler builds the external-face http.Handler: session auth on every
// non-public /api/v1 route, with admin-tier routes additionally gated on the
// operator console host inside their handlers.
func (a *API) ExternalHandler() http.Handler {
	return a.buildFace("external", a.externalAPIRoutes(), a.requireExternal)
}

// buildFace assembles one face from its route table. Public routes are mounted
// unauthenticated on the outer mux; the rest go on an inner mux behind guard
// (requireInternal / requireExternal), with Admin routes additionally wrapped in
// adminOnly, and Owner routes in ownerOnly. Because both faces are built from the
// same table the OpenAPI parity test reads, the served surface and the documented
// surface cannot drift apart without failing the build.
func (a *API) buildFace(face string, routes []apiRoute, guard func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	auth := http.NewServeMux()
	for _, rt := range routes {
		pattern := rt.Method + " " + rt.Pattern
		if rt.Public {
			h := rt.h
			if rt.AuthDoor {
				h = a.throttleAuthDoor(h)
			}
			mux.HandleFunc(pattern, tagRoute(rt.Pattern, h))
			continue
		}
		h := rt.h
		if (face == "internal") != (len(rt.Callers) > 0) {
			panic(fmt.Sprintf("%s route %s %s: internal routes list their callers, external ones none", face, rt.Method, rt.Pattern))
		}
		if len(rt.Callers) > 0 {
			h = callersOnly(rt.Callers, h)
		}
		if rt.Owner {
			h = a.ownerOnly(h)
		}
		if rt.Admin {
			h = a.adminOnly(h)
		}
		// Default-deny setup-lockdown: wrap every authenticated route unless it
		// explicitly opts out. The wrapper is nil-principal safe, so it is inert on
		// the internal face (service-token callers carry no Principal).
		if !rt.SetupAllowed {
			h = a.requireOnboarded(h)
		}
		auth.HandleFunc(pattern, tagRoute(rt.Pattern, h))
	}
	guarded := guard(auth)
	mux.Handle("/api/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := auth.Handler(r)
		if pattern == "" {
			writeNoRoute(w, r, mux, auth)
			return
		}
		// Named before the guard runs, so a refused request is counted under
		// the route it asked for.
		noteRoute(r, pattern)
		guarded.ServeHTTP(w, r)
	}))
	top := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			writeNoRoute(w, r, mux, auth)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return a.baseChain(face, top)
}

// baseChain wraps a handler in the cross-cutting middleware shared by both faces:
// the request id, then the access log and metrics (which see the final status of
// everything inside), the response security headers, the cross-site write fence,
// the request-body read deadline, and panic recovery.
func (a *API) baseChain(face string, h http.Handler) http.Handler {
	return withRequestID(a.observe(face, withSecurityHeaders(rejectCrossSiteWrites(withBodyDeadline(withRecover(h))))))
}

// writeNoRoute answers a request no route took, in the API's error envelope: 405
// with an Allow header when the path exists under other methods, 404 otherwise.
// ServeMux's own answers are plain text and turn a wrong method on a guarded
// route into a 404, since the guarded routes sit behind one catch-all.
func writeNoRoute(w http.ResponseWriter, r *http.Request, muxes ...*http.ServeMux) {
	var allow []string
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		probe := r.Clone(r.Context())
		probe.Method = m
		for _, mux := range muxes {
			if _, p := mux.Handler(probe); p != "" && p != "/api/v1/" {
				allow = append(allow, m)
				break
			}
		}
	}
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeError(w, r, newError(http.StatusMethodNotAllowed, "method_not_allowed",
			"%s is not allowed here; use %s", r.Method, strings.Join(allow, ", ")))
		return
	}
	writeError(w, r, newError(http.StatusNotFound, "not_found", "no such endpoint"))
}

// requireOnboarded fences an authenticated route behind the setup-lockdown: a
// freshly-onboarded principal that has not finished setup is restricted to
// SetupAllowed routes only. The lockdown lifts on a durable login credential, NOT
// on email verification: the bootstrap Owner has no verified email (no SMTP exists
// at bootstrap) and a passkey is the ONLY credential that logs the Owner in
// pre-SMTP (email-OTP login refuses admin accounts; op-login needs SMTP + a second
// admin). So passkey enrollment is what completes setup — and it must, or the
// unverified Owner could never reach the Settings page to configure SMTP.
//
// Only a session principal whose email is still unverified reaches the passkey
// lookup; after setup that is just the bootstrap Owner, so the extra query is not
// on any hot path. The wrapper is nil-principal safe, so it is inert on the
// internal face (service-token callers carry no Principal) and on the external
// face's admin Zero-Trust paths (those carry an IsAdmin/IsOwner principal that has
// already passed email verification at account creation).
func (a *API) requireOnboarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFromContext(r.Context())
		if p != nil && p.ViaSession && !p.EmailVerified {
			creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), p.UserID)
			if err != nil {
				// A store outage reads as retry-later; setup_required would send
				// the caller off to enroll a passkey they may already have.
				log.Printf("api: %s %s: onboarding check (request_id=%s): %v",
					r.Method, r.URL.Path, requestIDFromContext(r.Context()), err)
				writeError(w, r, errAuthUnavailable)
				return
			}
			if len(creds) == 0 {
				writeError(w, r, newError(http.StatusForbidden, "setup_required",
					"passkey enrollment is required before this action is available"))
				return
			}
		}
		h(w, r)
	}
}

// setupRequired reports whether the caller still owes the forced-onboarding step,
// for the SPA to poll. It MUST stay in lockstep with requireOnboarded's unlock
// condition above: the lockdown lifts on a verified email OR an enrolled passkey.
// Keying on email PRESENCE instead of a passkey would trap a console-tier player —
// they join through the bind-code door with no email by design (no SMTP) and can
// only ever complete setup by enrolling a passkey, so any email term loops them
// forever. Passkey alone is the durable gate; email verification is a later,
// SMTP-dependent step.
func setupRequired(emailVerified, hasPasskey bool) bool {
	return !emailVerified && !hasPasskey
}

// ---- request context plumbing ----

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
	ctxKeyReqInfo
	ctxKeyCaller
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

// callerFromContext returns the internal-face caller, or "" off that face.
func callerFromContext(ctx context.Context) Caller {
	c, _ := ctx.Value(ctxKeyCaller).(Caller)
	return c
}

// internalSource is the audit Source for an action taken on the internal face,
// naming the caller whose token asked for it ("internal:velocity").
func internalSource(r *http.Request) string {
	if c := callerFromContext(r.Context()); c != "" {
		return "internal:" + string(c)
	}
	return "internal"
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
//
// Entries older than the longest window the limiter has been asked about can
// no longer block anything, so checks sweep them out (at most once per
// bucketSweepEvery). Without that, every distinct address typed into a public
// door, whose neutral branch keeps its reservation, stayed in the map for the
// life of the process.
type cooldownLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	last      map[string]time.Time
	maxWindow time.Duration
	swept     time.Time
}

// noteWindow widens the retention to window and sweeps stale entries when due.
// The caller holds mu.
func (c *cooldownLimiter) noteWindow(window time.Duration, now time.Time) {
	if window > c.maxWindow {
		c.maxWindow = window
	}
	if c.maxWindow <= 0 || now.Sub(c.swept) < bucketSweepEvery {
		return
	}
	c.swept = now
	for k, t := range c.last {
		if now.Sub(t) >= c.maxWindow {
			delete(c.last, k)
		}
	}
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
	now := c.now()
	c.noteWindow(window, now)
	if last, ok := c.last[name]; ok && now.Sub(last) < window {
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
// in the same critical section, returning the reservation time and true; inside the
// window it returns the standing reservation's time and false. Unlike
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
	t := c.now()
	c.noteWindow(window, t)
	if last, ok := c.last[name]; ok && t.Sub(last) < window {
		return last, false
	}
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

// ---- per-principal stream cap ----

// streamLimiter bounds how many concurrent guarded sections a single KEY may hold at
// once. It backs the per-principal SSE stream cap (console + build-log relays): each
// relay blocks for the life of a client's attachment and, under a stalled reader,
// pins a goroutine plus a kube-apiserver follow connection, so an unbounded number of
// them from one principal is a control-plane connection-exhaustion vector. Unlike a
// single global semaphore, this counts per key. A
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
