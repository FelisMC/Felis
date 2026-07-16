package api

import (
	"context"
	"time"
)

// ServerRecord is the business-layer projection of a server (spec §6 servers /
// server_aliases). It carries only what the CRD cannot express — ownership and
// the cached subdomain alias — never authoritative lifecycle fields.
type ServerRecord struct {
	Name      string
	Subdomain string
	// OwnerID is the claiming user, or "" when the server is unclaimed.
	OwnerID string
	// CachedPhase is the non-authoritative phase projection used for fast lists.
	CachedPhase string
}

// MyServerView is a row of GET /api/v1/me/servers: a server the caller owns,
// may auto-start, or may claim.
type MyServerView struct {
	Name      string `json:"name"`
	Subdomain string `json:"subdomain"`
	Owned     bool   `json:"owned"`
	Claimable bool   `json:"claimable"`
	Phase     string `json:"phase,omitempty"`
}

// AuditEntry is one row written to audit_logs (spec §6). The actor is the Access
// email for human callers and the component name for internal callers.
type AuditEntry struct {
	Actor      string
	Source     string
	Action     string
	ServerName string
	RequestID  string
	// Payload is an optional structured detail blob stored in the audit_logs.payload
	// jsonb column. It MUST be valid JSON or nil; nil (the zero value) is stored as
	// SQL NULL, so existing callers that leave it unset are unaffected. The
	// break-glass console uses it to record the accountability detail (mode, target
	// owner, OS user, admin account) that does not fit the flat columns.
	Payload []byte
}

// BackupView is one row of GET /api/v1/backups (spec §7, world_backups in §22).
// It deliberately omits backup_ref — the opaque internal storage handle (a tar
// path / VolumeSnapshot name / Longhorn URL, spec §19) — on the same principle
// that keeps the RCON password server-side (spec §286): the client lists backups
// and triggers a restore by server, never by handle.
type BackupView struct {
	ID          string    `json:"id"`
	ServerName  string    `json:"server_name"`
	FormerOwner string    `json:"former_owner,omitempty"`
	SizeBytes   int64     `json:"size_bytes"`
	Reason      string    `json:"reason"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// BackupRecord is the server-side view of a backup used to drive a restore (spec
// §466). It carries the opaque backup_ref the Restorer needs and the former_owner
// the restore authorization compares against — neither is ever serialized to the
// client (contrast BackupView).
type BackupRecord struct {
	ID          string
	ServerName  string
	FormerOwner string
	BackupRef   string
	SizeBytes   int64
}

// StaffUser is the login-side projection of a users row (spec §B passwordless
// auth). Owner/Operator are role=admin rows, minted by `felis setup` (MC link)
// and recovered by `felis breakGlass` (email OTP); players are role=user rows.
// There is no password column — staff authenticate via email-OTP / passkey +
// in-game approve, never a password.
type StaffUser struct {
	ID       string
	Username string
	Email    string
	Role     string
	// EmailVerified mirrors users.email_verified (spec §B2): the address was proven
	// via an email OTP, not merely asserted. Players carry it through onboarding;
	// staff rows seeded by setup leave it false until a code is redeemed.
	EmailVerified bool
}

// PasskeyCredential is one bound passkey (Phase 6 WebAuthn enrollment). It carries
// only public, non-secret attestation material: a WebAuthn public key is meant to
// be public (unlike a session token), so it is safe at rest. CredentialID is the
// authenticator's globally-unique handle (base64url) and PublicKey the COSE key
// (base64); SignCount is the uint32 signature counter captured at registration.
// LastUsedAt is nil until an assertion stamps it. The passkey login door now exists
// (Public /auth/passkey/login/{begin,finish}, #72), but no path yet writes
// last_used_at, so in practice it stays nil; wiring the stamp is a follow-up there.
type PasskeyCredential struct {
	ID           string
	UserID       string
	CredentialID string
	PublicKey    string
	SignCount    uint32
	AAGUID       string
	Name         string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	// Ceremony flags captured at enrollment (migration 0009). UserVerified records that a
	// PIN/biometric was performed at bind; BackupEligible/BackupState record whether the
	// credential is syncable/backed up. Persisted so a future login path can enforce UV
	// per credential and reason about single-device vs. synced authenticators.
	UserVerified   bool
	BackupEligible bool
	BackupState    bool
}

// SessionedUser is the projection resolved from a live session cookie: the
// identity SessionAuth needs to build a Principal. EmailVerified mirrors
// users.email_verified so the lockdown middleware can gate setup-incomplete
// accounts without a second DB read.
type SessionedUser struct {
	ID            string
	Email         string
	Role          string
	EmailVerified bool
}

// OpLoginRequest is one op.console staff-login attempt (spec §B op-login): the
// durable second factor (in-game approval) that pairs with an email_otps code under
// purpose 'op_login'. Username is populated only by ListPendingOpLogins (the join the
// in-game admin needs to name who is waiting); Consumed reflects consumed_at, so the
// finish path can refuse an already-spent request without a second query.
type OpLoginRequest struct {
	ID        string
	UserID    string
	Username  string // joined for the in-game pending list; "" elsewhere
	Email     string
	Status    string // 'pending' | 'approved' | 'denied'
	Consumed  bool   // consumed_at IS NOT NULL (single-use guard)
	ExpiresAt time.Time
	CreatedAt time.Time
}

// MigrationView is the live account-migration for a source user (spec §B3 inherit,
// scenario A): its state-machine position and the fields the web step-up, issue-code,
// and status paths read. TargetUserID is empty until a code is issued; ConfirmFactor
// and ConfirmedAt are empty/nil until the source completes step-up; CodeExpiresAt is
// nil until code_issued.
type MigrationView struct {
	ID            string
	SourceUserID  string
	TargetUserID  string
	State         string
	ConfirmFactor string
	ConfirmedAt   *time.Time
	CodeExpiresAt *time.Time
	CreatedAt     time.Time
}

// Repo is the business-layer data access the API depends on. It is an interface
// so handlers are tested against an in-memory fake; the Postgres implementation
// (pgRepo) is integration-tested only — it requires a live database.
type Repo interface {
	// Ping probes the database — used by the /readyz endpoint (spec §7) to verify
	// the DB connection is alive.
	Ping(ctx context.Context) error

	// ServerBySubdomain resolves a subdomain alias to its server, or ErrNotFound.
	ServerBySubdomain(ctx context.Context, subdomain string) (*ServerRecord, error)
	// ServerByName loads a server's business projection, or ErrNotFound.
	ServerByName(ctx context.Context, name string) (*ServerRecord, error)
	// IsLinked reports whether the user has a verified MC account link (spec §10),
	// a precondition for every ownership operation.
	IsLinked(ctx context.Context, userID string) (bool, error)
	// CreateLinkCode mints a one-time account-link code for an in-game player
	// (spec §10: 游戏内 /link → 生成一次性码). The code is born knowing only the
	// verified mc_uuid (online-mode=true established it) and the authSource that
	// established it (mojang|thirdparty, spec §10 dual-Yggdrasil); a web user binds
	// it to their user_id later via VerifyLinkCode. This is internal-face only — the
	// web has no verified UUID to mint against (the account_link_codes schema has no
	// user_id column, which forces the in-game origin), nor does it see the
	// authentication, which is why authSource also originates here.
	CreateLinkCode(ctx context.Context, code, mcUUID, authSource string, expiresAt time.Time) error
	// VerifyLinkCode consumes a non-expired code for the logged-in user and writes
	// the account_links binding, atomically (spec §10: 网页 verify 填码 → 写
	// account_links). It returns the bound mc_uuid and the authSource captured at
	// mint (copied from the code onto the durable link). A missing or expired code →
	// ErrLinkCodeInvalid; a uuid already linked to a *different* user → ErrConflict;
	// re-verifying the same (user, uuid) pair is idempotent and refreshes the stored
	// authSource. now is the API clock so expiry is testable.
	VerifyLinkCode(ctx context.Context, userID, code string, now time.Time) (mcUUID, authSource string, err error)
	// RedeemPlayerBindCode is the account-less player-console bootstrap (console-tier
	// access model): it redeems a one-time Bind Code into a PLAYER account + link in
	// one atomic step, so a first-time player with no Felis account can create one
	// from the console.<root_domain> door. Unlike VerifyLinkCode it takes NO prior
	// user — it creates or fetches one, keyed on the verified mc_uuid the code carries:
	//
	//   - code missing/expired → ErrLinkCodeInvalid (does not consume it);
	//   - the uuid is not yet linked → create a role='user' player row with id
	//     newUserID (NULL password_hash, username derived from the uuid so it is unique
	//     and deterministic), write the account_links binding, consume the code, and
	//     return newUserID;
	//   - the uuid is already linked to a role='user' player → return THAT user
	//     (idempotent "log in via the game"), consuming the code;
	//   - the uuid is linked to a role='admin' STAFF account → ErrPlayerBindForbidden
	//     WITHOUT consuming the code (operators use op.console behind Zero Trust; the
	//     public bootstrap never mints a session for an admin identity).
	//
	// Safe as an unauthenticated entrypoint because a Bind Code is minted internal-face
	// only (CreateLinkCode), against an online-mode-verified UUID, short-TTL and
	// single-use — possession already proves control of a Minecraft identity. now is
	// the API clock so expiry is testable. It returns the effective userID plus the
	// bound mc_uuid and authSource (for the response + audit).
	RedeemPlayerBindCode(ctx context.Context, newUserID, code string, now time.Time) (userID, mcUUID, authSource string, err error)
	// QuotaAvailable reports whether the user is under their max_servers quota
	// (spec §9.3 step ②, evaluated before provisioning).
	QuotaAvailable(ctx context.Context, userID string) (bool, error)
	// QuotaCheck reports whether claiming a server with the given resource spec
	// would push the user over any of their four quota caps: max_servers,
	// max_cpu_milli, max_memory_mb, and max_storage_gb (spec §9.3 / §22). A nil
	// or missing quota row/unset column means unlimited for that dimension.
	// excludeName is the server being claimed/edited ("" when checking a fresh
	// claim without an existing cached row) so its own current resources are
	// not double-counted.
	QuotaCheck(ctx context.Context, userID string, excludeName string, incoming ResourceSpec) (bool, error)
	// ClaimServer atomically sets owner_id where it is currently NULL and returns
	// whether a row changed. false means the server was already claimed (spec §9.3:
	// 0 rows → 409).
	ClaimServer(ctx context.Context, name, userID string) (bool, error)
	// UserInAllowlist reports whether the user's linked UUID is on the server
	// allowlist (spec §9.4).
	UserInAllowlist(ctx context.Context, name, userID string) (bool, error)
	// UUIDInAllowlist reports whether an in-game UUID is on the server allowlist
	// (spec §9.4). It is the internal-face symmetric of UserInAllowlist: velocity
	// drives a domain-autostart wake knowing the joining player only by their
	// online-mode UUID, never a web user_id, so the allowlist gate for that path
	// must be keyed by UUID directly (the allowlist table is UUID-keyed; the
	// account_links join in UserInAllowlist only exists to bridge the web side).
	UUIDInAllowlist(ctx context.Context, name, mcUUID string) (bool, error)
	// UserByMCUUID resolves a verified in-game UUID to the user_id it is linked to
	// (spec §10 account_links), or ErrNotFound when the UUID is not linked. The
	// internal-face wake uses it to apply the owner bypass for a player known only
	// by UUID; an unlinked UUID simply falls through to the autostartPolicy gate.
	UserByMCUUID(ctx context.Context, mcUUID string) (userID string, err error)
	// RecordJoin updates last_active_at, clears reaper warnings, and auto-appends
	// the UUID to the allowlist (spec §7 join-event, §9.4).
	RecordJoin(ctx context.Context, name, mcUUID string) error
	// MyServers lists the servers a user owns or may claim.
	MyServers(ctx context.Context, userID string) ([]MyServerView, error)
	// ServerOwners maps each currently-owned server to its owner's display identity
	// (email, or username when the address is absent), for the SysAdmin cockpit's
	// fleet read. It is a READ-ONLY presentational join: owner stays authored in
	// Postgres (§6 business authority) and is never written back to the CRD, so this
	// does not breach §1's store-of-record split. Unclaimed and soft-deleted servers
	// are simply absent from the map, so a missing key reads as "no owner". The
	// cockpit treats it as best-effort — a lookup error degrades to owner-less rows
	// rather than failing the fleet read — so callers may ignore the error.
	ServerOwners(ctx context.Context) (map[string]string, error)
	// AllBackups lists every present world backup, newest first (spec §7 GET
	// /backups, admin scope). Expired/deleted rows are never returned.
	AllBackups(ctx context.Context) ([]BackupView, error)
	// BackupsForUser lists the present world backups of worlds the user formerly
	// owned, newest first (spec §7 GET /backups, former_owner scope). The
	// former_owner column is stamped when the world is archived at release time, so
	// a user sees their own released worlds even after the server is re-seeded.
	BackupsForUser(ctx context.Context, userID string) ([]BackupView, error)
	// LatestBackup returns the most recent present backup for a server, or
	// ErrNotFound when none exists (spec §466 restore). The returned BackupRecord
	// carries the server-side backup_ref + former_owner the restore path needs; the
	// client never sees them.
	LatestBackup(ctx context.Context, serverName string) (*BackupRecord, error)
	// BackupByID returns a single present backup by its id, or ErrNotFound when
	// none matches. Like LatestBackup the returned BackupRecord carries the
	// server-side backup_ref the restore path needs; the client never sees it.
	BackupByID(ctx context.Context, id string) (*BackupRecord, error)
	// SeedServer inserts the business-layer rows for a newly created server (spec
	// §15): a servers row (owner_id NULL — claimed later, spec §9.3) and its
	// subdomain alias, both idempotent. The resource cache (cpuMilli, memoryMB,
	// storageMB) is seeded alongside so QuotaCheck can aggregate per-owner usage
	// without cross-system CRD reads. It returns ErrConflict if the subdomain is
	// already bound to a different server.
	SeedServer(ctx context.Context, name, subdomain string, cpuMilli, memoryMB, storageMB int) error
	// UpdateServerResources updates the resource cache columns for a server
	// after a spec mutation (spec §7 PATCH), so the per-owner aggregate stays in
	// sync.
	UpdateServerResources(ctx context.Context, name string, cpuMilli, memoryMB, storageMB int) error
	// ServerResources returns the cached resource spec for a server, or zeroes
	// when the row does not exist or has been cleared.
	ServerResources(ctx context.Context, name string) (ResourceSpec, error)
	// Audit appends one audit row.
	Audit(ctx context.Context, e AuditEntry) error

	// ---- player email verification (spec §B2 onboarding) ----

	// CreateEmailOTP persists a freshly minted one-time code for (userID, purpose):
	// only its sha-256 (codeHash), never the digits. It supersedes any prior live
	// (unconsumed) code for the same (userID, purpose) so a user has at most one
	// outstanding code per purpose — a re-request invalidates the earlier mail.
	// expiresAt is the API clock + TTL so expiry is driven by one authoritative clock.
	CreateEmailOTP(ctx context.Context, id, userID, email, codeHash, purpose string, expiresAt time.Time) error
	// VerifyEmailOTP redeems the newest live code for (userID, purpose) against
	// codeHash, atomically (spec §B2). No live code, an expired one, or a consumed
	// one → ErrOTPInvalid; an exhausted attempt budget → ErrOTPLocked; a hash
	// mismatch increments attempts and returns ErrOTPInvalid WITHOUT consuming the
	// code (so a typo does not burn it). On a match the code is consumed and the
	// user row is flipped to email=<the proven address>, email_verified=true; the
	// proven email is returned. now is the API clock so expiry is testable.
	//
	// This is the ONBOARDING primitive: verifying the code is the moment the address
	// becomes proven, so the write is load-bearing. The pre-session LOGIN door must
	// NOT use it — see ConsumeLoginEmailOTP.
	VerifyEmailOTP(ctx context.Context, userID, purpose, codeHash string, now time.Time) (email string, err error)
	// SetUserEmail records email on the user row WITHOUT proving control of it, and
	// clears email_verified in the same write (proving nothing, it must never leave a
	// stale verified flag — see the PGRepo impl). This backs the setup wizard's Step 1:
	// the bootstrap has no SMTP, so the Owner cannot receive an emailed code, and the
	// address is stored unverified for a later Settings/SMTP flow to verify. An unknown
	// userID returns ErrNotFound.
	SetUserEmail(ctx context.Context, userID, email string) error
	// ConsumeLoginEmailOTP redeems the newest live code for (userID, purpose) against
	// codeHash for the PRE-SESSION email LOGIN door, with the SAME code lifecycle as
	// VerifyEmailOTP (FOR UPDATE, expiry+lockout before hash compare, mismatch charges
	// one attempt without consuming) but with NO identity side-effects: it neither
	// writes users.email nor runs the verified-email uniqueness guard. Login resolved
	// userID via UserByEmail, which already requires email_verified, so the address is
	// settled — re-proving control of a code this session must not re-touch the row.
	// Returning only an error is deliberate: unlike onboarding, login has nothing to
	// prove about the address, so there is no email to hand back. Errors are exactly
	// ErrOTPInvalid / ErrOTPLocked (ErrEmailTaken is structurally impossible here).
	ConsumeLoginEmailOTP(ctx context.Context, userID, purpose, codeHash string, now time.Time) error

	// ---- op.console staff login: in-game approval state machine (spec §B op-login) ----

	// CreateOpLoginRequest records a fresh pending op.console login attempt for a staff
	// account (spec §B op-login). id is the opaque handle the browser polls; email is a
	// snapshot for the audit trail. It writes the SECOND factor only — the email-OTP
	// itself is minted separately under purpose 'op_login' (CreateEmailOTP) — so a row
	// here means "this staff account is waiting for an in-game admin to vouch". expiresAt
	// is the API clock + TTL so expiry is driven by one authoritative clock.
	CreateOpLoginRequest(ctx context.Context, id, userID, email string, expiresAt time.Time) error
	// OpLoginRequestByID loads a request by its handle, or ErrNotFound. The status poll
	// and the finish path both use it: finish additionally checks Status=='approved',
	// !Consumed, and ExpiresAt>now before it will mint a session, so a pending, spent, or
	// expired request can never be exchanged. Username is left empty (no join needed here).
	OpLoginRequestByID(ctx context.Context, id string) (*OpLoginRequest, error)
	// ListPendingOpLogins returns the live (pending, unconsumed, unexpired at now)
	// requests oldest-first, each joined to its staff username, for the in-game admin's
	// approval prompt (Velocity polls this on the internal face). A resolved or expired
	// request drops out of the list, so an admin only ever sees actionable attempts.
	ListPendingOpLogins(ctx context.Context, now time.Time) ([]OpLoginRequest, error)
	// ApproveOpLogin marks a pending request approved by approverUserID (the in-game
	// admin, resolved from their online-mode UUID via UserByMCUUID), atomically: it
	// stamps status='approved', approved_by, approved_at only WHERE the row is still
	// pending, unconsumed, and unexpired at now. A request that is gone, already
	// resolved, or expired affects zero rows and returns ErrNotFound, so a double
	// approval or an approval of a dead request is a no-op the caller can surface.
	ApproveOpLogin(ctx context.Context, id, approverUserID string, now time.Time) error
	// ConsumeOpLoginRequest stamps consumed_at on an APPROVED, unconsumed, unexpired
	// request, atomically, so it can be exchanged for a session exactly once. Zero rows
	// affected (pending, already consumed, or expired) → ErrNotFound. This is the finish
	// path's single-use guard; the email-OTP is consumed separately, so a lost race here
	// never silently mints a second session.
	ConsumeOpLoginRequest(ctx context.Context, id string, now time.Time) error

	// ---- player passkey enrollment (spec §14 WebAuthn / Phase 6 bind) ----

	// CreatePasskeyChallenge persists the server-side state of a credential-creation
	// ceremony for (userID, purpose): the opaque go-webauthn SessionData blob and its
	// expiry. Only the server holds it, so the client cannot forge the challenge it
	// must answer at finish. It supersedes any prior live (unconsumed) challenge for
	// the same (userID, purpose) so a re-begin invalidates the earlier ceremony —
	// at most one outstanding challenge per (user, purpose). expiresAt is the API
	// clock + TTL so expiry is driven by one authoritative clock.
	CreatePasskeyChallenge(ctx context.Context, id, userID, purpose string, sessionData []byte, expiresAt time.Time) error
	// ConsumePasskeyChallengeByUser redeems the newest live (unconsumed, unexpired at
	// now) challenge for (userID, purpose), atomically: it stamps consumed_at and
	// returns the stashed SessionData so finish can validate the attestation against
	// it. No live challenge → ErrPasskeyChallengeInvalid. Single-use: a second finish
	// for the same ceremony finds nothing live and fails. now is the API clock so
	// expiry is testable. Bound to user_id — enrollment and username-first login both
	// know the principal at begin; the usernameless from-zero door instead uses the
	// non-user-keyed pair below.
	ConsumePasskeyChallengeByUser(ctx context.Context, userID, purpose string, now time.Time) (sessionData []byte, err error)
	// CreateDiscoverableChallenge persists a DISCOVERABLE ("usernameless") login ceremony
	// (task #40, migration 0013), keyed by an opaque server-minted handle id — NOT a user,
	// since a from-zero begin has no principal. In one transaction it reaps expired/consumed
	// rows (the non-user-keyed analog of CreatePasskeyChallenge's supersede) and then, if the
	// live count is at the hard cap, refuses with ErrTooManyDiscoverableChallenges rather than
	// inserting — the cap, not the reap, bounds an adversarial begin-flood, since a burst
	// inside the TTL leaves every fresh row live. now and expiresAt are both the API clock
	// (now drives the reap; expiresAt = now + TTL drives liveness).
	CreateDiscoverableChallenge(ctx context.Context, id string, sessionData []byte, now, expiresAt time.Time) error
	// ConsumeDiscoverableChallenge redeems the discoverable challenge under handle id,
	// atomically and single-use (mirrors ConsumePasskeyChallengeByUser without the user key):
	// it takes the row FOR UPDATE, checks expiry against now, stamps consumed_at, and returns
	// the stashed SessionData. An unknown, expired, or already-consumed handle →
	// ErrPasskeyChallengeInvalid, so the finish door never doubles as a state oracle.
	ConsumeDiscoverableChallenge(ctx context.Context, id string, now time.Time) (sessionData []byte, err error)
	// CreatePasskeyCredential stores a freshly verified passkey for a user (Phase 6
	// enrollment). It writes only public attestation material (credential_id,
	// public_key, sign_count, aaguid) plus the caller's nickname. A credential_id
	// already bound to ANY account → ErrConflict (the UNIQUE guard); the handler maps
	// that to 409 rather than silently rebinding an authenticator.
	CreatePasskeyCredential(ctx context.Context, c PasskeyCredential) error
	// PasskeyCredentialsForUser lists the passkeys a user has bound, newest first, for
	// the credential-management view. It returns only display fields (never a secret —
	// a passkey carries none); LastUsedAt is nil where no assertion has been verified.
	PasskeyCredentialsForUser(ctx context.Context, userID string) ([]PasskeyCredential, error)
	// DeletePasskeyCredential removes the passkey row id, scoped to userID so a caller
	// can only unbind their OWN credential. No matching (user, id) row → ErrNotFound,
	// so a stale or cross-user id cannot silently no-op as success.
	DeletePasskeyCredential(ctx context.Context, userID, id string) error
	// DeleteAllPasskeyCredentialsForUser unbinds every passkey a user holds — the
	// remediation that stops a passkey planted via a transiently-hijacked session from
	// surviving as a standing login foothold. Its production caller is the owner-tier
	// DELETE /users/{id}/passkeys (handleUnbindUserPasskeys); a complete remediation
	// pairs it with a session revoke, since unbinding the credential without revoking
	// live sessions leaves the hijacked session itself, and revoking sessions without
	// unbinding leaves a re-enrollable credential. Removing zero rows is success, not an
	// error — an account with no passkeys is the intended post-condition either way.
	DeleteAllPasskeyCredentialsForUser(ctx context.Context, userID string) error
	// AdvanceCredentialSignCount records a successful assertion on the passkey identified by
	// credentialID (base64url): it sets the stored signature counter to newSignCount and stamps
	// last_used_at. credential_id is UNIQUE, so exactly one row is updated; a missing row (the
	// credential was unbound mid-ceremony) is a successful no-op, never an error. The login doors
	// call it only after clone policy allows the assertion, so for a counter-keeping authenticator
	// the stored counter only ever moves forward — the baseline a later regression is judged against.
	AdvanceCredentialSignCount(ctx context.Context, credentialID string, newSignCount uint32, usedAt time.Time) error

	// ---- player game-login: username-collision reclaim (spec §B3) ----

	// ReclaimUsername records a Mojang-priority username reclaim, atomically (spec
	// §B3 正版优先): in one transaction it bars the non-genuine squatter UUID
	// (username_blacklist) and stashes that account's data as a hold the velocity
	// reclaim callback drives. The two writes are all-or-nothing — a half-applied
	// reclaim (a barred UUID whose data was never held, or a hold for a UUID still
	// able to connect) would either lose the player's data or let the squatter back
	// in. id is the opaque hold row id; dataRef is the opaque archiver handle (""
	// stored as NULL when archival is deferred); expiresAt is the proposed held_at +
	// the 30-day window on the API clock. Keyed by mc_uuid on both tables, so a
	// repeat reclaim of an already-barred UUID is idempotent and never errors on a
	// duplicate — the block stays on the squatting UUID, never the contested name.
	// It returns the EFFECTIVE persisted expiry: on a fresh reclaim that is the
	// passed expiresAt, but on an idempotent retry it is the FIRST reclaim's expiry,
	// so the caller never reports a window the stored hold does not actually have.
	ReclaimUsername(ctx context.Context, id, squatterUUID, username, dataRef string, expiresAt time.Time) (time.Time, error)
	// IsUsernameBlacklisted reports whether an in-game UUID was barred by a prior
	// reclaim (spec §B3). The velocity login gate calls it on the internal face to
	// reject a squatter while letting the genuine Mojang UUID — same username,
	// different UUID — through: the check is keyed by UUID, never by the name.
	IsUsernameBlacklisted(ctx context.Context, mcUUID string) (bool, error)
	// IsProtectedAdminLink reports whether an in-game UUID belongs to a Linked
	// Operator/SysAdmin who authenticates through the configured third-party
	// Yggdrasil — the admin-on-Yggdrasil reclaim exception (spec §B3). Such a holder
	// is staff logging in via the Login Server, not a Mojang squatter, so a
	// Mojang-priority reclaim must never bar them. The predicate is exactly three
	// conjuncts: the UUID is linked (account_links), that link authenticated via
	// 'thirdparty' (auth_source), and the linked user is an admin (role='admin').
	// It deliberately does NOT require a local password hash: an Operator who signs
	// in through SSO (Cloudflare Access, IdP-agnostic per §14) carries role='admin'
	// with no password_hash, and must be protected all the same — a password hash is
	// orthogonal to both "is staff" and "logs in via the Login Server". An unlinked
	// UUID, a Mojang-sourced link, or a non-admin link all yield false, so the
	// exception never broadens to ordinary thirdparty players (Mojang priority still
	// displaces them) nor to Mojang-authenticated identities (who have no Login-Server
	// name to protect). Keyed by UUID — the only identity velocity knows.
	IsProtectedAdminLink(ctx context.Context, mcUUID string) (bool, error)

	// ---- staff account lookups (spec §B, passwordless) ----

	// UserByUsername loads the login projection of a staff account by its unique
	// username, or ErrNotFound. Its caller is the `felis breakGlass` recovery TUI,
	// which resolves an Owner/Operator username before sending an email-OTP — there is
	// no password compare (the account is passwordless). A non-staff (role='user') row
	// resolves too; callers that require staff enforce the role themselves.
	UserByUsername(ctx context.Context, username string) (*StaffUser, error)
	// UserByID loads the same staff projection by user id, or ErrNotFound. Callers hold
	// a session (which yields a user id, not a username) and need the account behind it
	// — e.g. setup redeem/status resolving the lockdown session's owner. It is the
	// id-keyed counterpart of UserByUsername.
	UserByID(ctx context.Context, id string) (*StaffUser, error)
	// UserByEmail resolves a VERIFIED email address to its login projection,
	// case-insensitively, or ErrNotFound (spec §B email-first login). It is the
	// entry point every email-first web login shares: the address must be proven
	// (email_verified true), so a merely-asserted or unverified address never
	// resolves to a session-mintable identity — an attacker cannot claim someone
	// else's login by typing their email. Matching is on lower(email) to align with
	// the users_verified_email_unique partial index (migration 0010), which
	// guarantees at most one verified row per normalized address, so the result is
	// unambiguous. A player (role='user') row resolves too — email-first login is
	// passwordless and role-agnostic here; the door that consumes this result decides
	// what each role may do.
	UserByEmail(ctx context.Context, email string) (*StaffUser, error)
	// UpsertOwner creates or resets the single Owner account direct-to-Postgres
	// (the `felis setup` / `felis breakGlass` recovery path). role is forced to
	// 'admin'; on a username conflict the existing row's email is overwritten so
	// a reset is idempotent. The account is passwordless by design.
	UpsertOwner(ctx context.Context, id, username, email string) error
	// CreateSession records a minted session: the sha-256 of the opaque cookie
	// value, its owner, and its expiry (spec §B sessions). Only the hash is stored,
	// mirroring tokens, so a database read never yields a usable cookie.
	CreateSession(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error
	// SessionUser resolves a live (unrevoked, unexpired at now) session hash to its
	// user, or ErrNotFound. It is the cookie half of SessionAuth.
	SessionUser(ctx context.Context, tokenHash string, now time.Time) (*SessionedUser, error)
	// RevokeSession marks a session revoked (logout). It is idempotent: revoking an
	// absent or already-revoked session is not an error.
	RevokeSession(ctx context.Context, tokenHash string) error

	// ConsumeSetupToken atomically marks a one-time setup token consumed and returns
	// its user_id, or ErrNotFound when the token is absent, already consumed, or
	// expired. The /setup?token=... web flow redeems it for a lockdown session that
	// can only complete passwordless login setup (verify email / enroll passkey).
	ConsumeSetupToken(ctx context.Context, tokenHash string, now time.Time) (userID string, err error)

	// ---- runtime platform settings (spec §B platform_settings) ----

	// GetSetting reads a runtime setting's raw jsonb value, or ErrNotFound when the
	// key is absent. The live API reads these per-request so the break-glass TUI can
	// flip toggles (e.g. local_auth_enabled) direct-to-DB without rolling the pod.
	GetSetting(ctx context.Context, key string) ([]byte, error)
	// SetSetting upserts a runtime setting's raw jsonb value by key.
	SetSetting(ctx context.Context, key string, value []byte) error

	// ---- user admin (spec §7, admin-only) ----

	// ListUsers returns a page of non-deleted users matching the optional filters,
	// newest first. total is the unfiltered count so the admin page can render
	// pagination without a second round-trip.
	ListUsers(ctx context.Context, opts ListUsersOpts) ([]UserView, int, error)
	// UserDetail loads one user with its linked MC accounts, or ErrNotFound.
	// A deleted user is returned (the row lives for audit) but flagged.
	UserDetail(ctx context.Context, userID string) (*UserDetail, error)
	// CreateUser mints a new user row (role forced to either 'admin' or 'user').
	// createdBy is the actor email for audit. A username conflict → ErrConflict.
	CreateUser(ctx context.Context, input CreateUserInput, createdBy string) (*UserView, error)
	// UpdateUser applies the non-nil fields of patch to the user identified by
	// userID and returns the updated view. A username conflict → ErrConflict;
	// a non-existent user → ErrNotFound. updatedBy is the actor email.
	UpdateUser(ctx context.Context, userID string, patch UpdateUserInput, updatedBy string) (*UserView, error)
	// DeleteUser soft-deletes the user: sets deleted_at, revokes every live
	// session, and releases every owned server (owner_id → NULL). deletedBy is
	// the actor email. A non-existent or already-deleted user → ErrNotFound.
	// The row is preserved so audit_logs.actor references survive.
	DeleteUser(ctx context.Context, userID, deletedBy string) error
	// SetUserDisabled flips the disabled flag on a live (non-deleted) user.
	// Setting disabled→true additionally revokes every live session so a
	// disabled account is immediately locked out. A non-existent user →
	// ErrNotFound; a deleted user → ErrNotFound.
	SetUserDisabled(ctx context.Context, userID string, disabled bool) error

	// ---- quota admin (spec §6 quotas, admin-only) ----

	// GetQuotas returns the quotas row for a user, or a zero-value view when no
	// row exists (which means unlimited per spec §9.3).
	GetQuotas(ctx context.Context, userID string) (*QuotaView, error)
	// SetQuotas upserts a quotas row for userID. Nil fields leave the column
	// untouched; a zero-value (non-nil) field clears the cap (unlimited).
	SetQuotas(ctx context.Context, userID string, q QuotaInput, setBy string) (*QuotaView, error)

	// ---- session admin (admin-only) ----

	// ListUserSessions returns every live (unrevoked, unexpired at now) session
	// for a user, newest first. An empty list is not an error.
	ListUserSessions(ctx context.Context, userID string, now time.Time) ([]SessionView, error)
	// RevokeAllUserSessions marks every live session of userID revoked.
	// Revoking zero sessions is not an error.
	RevokeAllUserSessions(ctx context.Context, userID string) error

	// ---- account-link admin (admin-only) ----

	// UnlinkAccount removes a single (user_id, mc_uuid) binding. It does not
	// consume the UUID's link code — a re-link by the player later is still
	// possible — but the admin can unlink without going through the player.
	// A non-existent binding → ErrNotFound.
	UnlinkAccount(ctx context.Context, userID, mcUUID string) error
	// LinkAccount force-binds a verified MC UUID to a user, bypassing the
	// normal code-verification flow. The UUID must not already be linked to a
	// different user (→ ErrConflict). A duplicate bind of the same pair is
	// idempotent. authSource records which Yggdrasil established the UUID
	// (mojang | thirdparty, spec §10 dual-Yggdrasil).
	LinkAccount(ctx context.Context, userID, mcUUID, authSource string) error

	// ---- account migration (spec §B3 inherit, scenario A) ----

	// StartMigration puts a LIVE source account into migrate mode: it supersedes any
	// earlier unfinished migration for the source (so re-running /felis migrate
	// restarts cleanly, invalidating a prior outstanding code) and inserts a fresh row
	// in state 'initiated'. id is the opaque handle. The source must be a live
	// (non-deleted) account — ErrNotFound otherwise, so a retired account can never
	// re-initiate. now stamps the row.
	StartMigration(ctx context.Context, id, sourceUserID string, now time.Time) error
	// MigrationForSource loads the live (non-redeemed) migration for a source, or
	// ErrNotFound when none is in flight. The web status/confirm/issue paths use it to
	// gate each step on the correct prior state.
	MigrationForSource(ctx context.Context, sourceUserID string) (*MigrationView, error)
	// ConfirmMigration records that the source proved control via a FRESH step-up
	// (factor 'passkey' | 'email_otp'), advancing 'initiated' → 'confirmed'. It only
	// advances from 'initiated'; any other current state (or no migration) → ErrConflict,
	// so a confirmed/code_issued/redeemed migration can never be re-confirmed and the
	// step-up cannot be replayed. now stamps confirmed_at.
	ConfirmMigration(ctx context.Context, sourceUserID, factor string, now time.Time) error
	// IssueMigrationCode binds the named target and stores the one-time code hash,
	// advancing 'confirmed' → 'code_issued'. targetUserID must be a live account other
	// than the source (validated by the caller before this call); the target FK also
	// guarantees the row exists. codeHash is the sha-256 of the code; expiresAt is its
	// TTL. A migration not in 'confirmed' → ErrConflict.
	IssueMigrationCode(ctx context.Context, sourceUserID, targetUserID, codeHash string, expiresAt time.Time) error
	// RedeemMigration is the ATOMIC transfer: keyed by (codeHash, targetUserID) it
	// finds the 'code_issued', unexpired migration whose named target is exactly the
	// redeeming user, re-points every server owned by the source to the target, retires
	// the source account (disabled + soft-deleted, its live sessions revoked), and marks
	// the migration 'redeemed' — all in one transaction. It returns the source user id
	// and the moved server names for the audit trail. No matching or expired code, or a
	// code whose named target is a different user → ErrLinkCodeInvalid (an intercepted
	// code is useless to anyone but the named target). Server ownership is the only thing
	// moved — the mc_uuid link and web credentials stay with their accounts. now drives
	// expiry and the terminal timestamps.
	RedeemMigration(ctx context.Context, targetUserID, codeHash string, now time.Time) (sourceUserID string, movedServers []string, err error)
}

// ---- user admin types ----

// ListUsersOpts carries the optional filters and pagination for ListUsers.
// Zero values mean "no filter / default page."
type ListUsersOpts struct {
	Query  string // substring match on username or email
	Role   string // exact role match ("admin" / "user"), or "" for all
	Hidden string // "true" = disabled only, "false" = enabled only, "" = all
	Limit  int    // page size; 0 → default 20
	Offset int    // page offset; 0 → first page
}

// UserView is one row of the admin user list.
type UserView struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	Email         string    `json:"email,omitempty"`
	Role          string    `json:"role"`
	Disabled      bool      `json:"disabled"`
	EmailVerified bool      `json:"email_verified"`
	ServerCount   int       `json:"server_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// UserDetail is the full admin view of one user, including linked MC accounts.
type UserDetail struct {
	UserView
	DeletedAt      *time.Time      `json:"deleted_at,omitempty"`
	LinkedAccounts []LinkedAccount `json:"linked_accounts,omitempty"`
}

// LinkedAccount is one verified MC-UUID binding (account_links, spec §10).
type LinkedAccount struct {
	MCUUID     string    `json:"mc_uuid"`
	AuthSource string    `json:"auth_source"`
	VerifiedAt time.Time `json:"verified_at"`
}

// CreateUserInput is the admin create-user form.
type CreateUserInput struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role"`
}

// UpdateUserInput is the admin patch-user form. Every field is a pointer so
// an absent field ("leave unchanged") is distinguishable from a zero value.
type UpdateUserInput struct {
	Username *string `json:"username,omitempty"`
	Email    *string `json:"email,omitempty"`
	Role     *string `json:"role,omitempty"`
}

// QuotaView is the admin-visible quotas row (spec §6).
type QuotaView struct {
	UserID       string `json:"user_id"`
	MaxServers   *int   `json:"max_servers,omitempty"`
	MaxCPUMilli  *int   `json:"max_cpu_milli,omitempty"`
	MaxMemoryMB  *int   `json:"max_memory_mb,omitempty"`
	MaxStorageGB *int   `json:"max_storage_gb,omitempty"`
}

// QuotaInput is the admin set-quotas form. Nil fields are left unchanged;
// a non-nil zero-value field clears the cap (unlimited).
type QuotaInput struct {
	MaxServers   *int `json:"max_servers,omitempty"`
	MaxCPUMilli  *int `json:"max_cpu_milli,omitempty"`
	MaxMemoryMB  *int `json:"max_memory_mb,omitempty"`
	MaxStorageGB *int `json:"max_storage_gb,omitempty"`
}

// ResourceSpec is the resource footprint of one server, in the units that the
// quotas table uses. The resource cache on the servers row mirrors these values
// so QuotaCheck can aggregate per-owner usage with pure SQL.
type ResourceSpec struct {
	CPUMilli  int // CPU in millicores (e.g. 4000 = 4 cores)
	MemoryMB  int // memory in megabytes (e.g. 4096 = 4 GiB)
	StorageMB int // storage in megabytes (e.g. 10240 = 10 GiB)
}

// SessionView is one live session row visible to an admin.
type SessionView struct {
	TokenHash string     `json:"token_hash"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}
