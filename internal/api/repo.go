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

// StaffUser is the login-side projection of a users row that carries a password
// (spec §B local-auth). Owner/Operator are role=admin rows WITH a bcrypt hash,
// minted by `felis breakGlass`; players are role=user rows whose PasswordHash is
// empty. It is loaded by username at login to verify the password and learn
// whether a first-login change is still pending.
type StaffUser struct {
	ID                 string
	Username           string
	Email              string
	Role               string
	PasswordHash       string
	MustChangePassword bool
	// EmailVerified mirrors users.email_verified (spec §B2): the address was proven
	// via an email OTP, not merely asserted. Players carry it through onboarding;
	// staff rows seeded by break-glass leave it false until a code is redeemed.
	EmailVerified bool
}

// PasskeyCredential is one bound passkey (Phase 6 WebAuthn enrollment). It carries
// only public, non-secret attestation material: a WebAuthn public key is meant to
// be public (unlike a session token), so it is safe at rest. CredentialID is the
// authenticator's globally-unique handle (base64url) and PublicKey the COSE key
// (base64); SignCount is the uint32 signature counter captured at registration.
// LastUsedAt is nil until an assertion is verified — the login/step-up path that
// would stamp it is out of scope for this enrollment-only slice (deferred), so it
// stays nil through the flow this type backs.
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
// identity SessionAuth needs to build a Principal. It omits the password hash —
// the session has already authenticated the caller — but carries the pending
// first-login change flag so the lockdown middleware can fence a half-onboarded
// staff account to the change-password surface.
type SessionedUser struct {
	ID                 string
	Email              string
	Role               string
	MustChangePassword bool
}

// Repo is the business-layer data access the API depends on. It is an interface
// so handlers are tested against an in-memory fake; the Postgres implementation
// (pgRepo) is integration-tested only — it requires a live database.
type Repo interface {
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
	// subdomain alias, both idempotent. It returns ErrConflict if the subdomain is
	// already bound to a different server, so the create handler can fail before
	// touching the CRD. ClaimServer requires this row to exist, so a CRD-only
	// server would be unclaimable — the create path must seed here first.
	SeedServer(ctx context.Context, name, subdomain string) error
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
	VerifyEmailOTP(ctx context.Context, userID, purpose, codeHash string, now time.Time) (email string, err error)

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
	// expiry is testable. Bound to user_id — enrollment always has a principal, so
	// there is no usernameless consume-by-hash variant (login is a deferred slice).
	ConsumePasskeyChallengeByUser(ctx context.Context, userID, purpose string, now time.Time) (sessionData []byte, err error)
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
	// DeleteAllPasskeyCredentialsForUser unbinds every passkey a user holds. The
	// change-password flow calls it so a passkey planted via a transiently-hijacked
	// session does not survive the remediation (password reset + session revoke) as a
	// standing login foothold. Removing zero rows is success, not an error — an account
	// with no passkeys is the intended post-condition either way.
	DeleteAllPasskeyCredentialsForUser(ctx context.Context, userID string) error

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

	// ---- local-password auth (spec §B) ----

	// UserByUsername loads the login projection of a staff account by its unique
	// username, or ErrNotFound. The caller compares PasswordHash itself so the
	// anti-enumeration dummy-hash compare runs even on a miss; a player row (NULL
	// password_hash → empty PasswordHash) is returned too and is rejected by the
	// caller's hash compare, never by leaking "no such user".
	UserByUsername(ctx context.Context, username string) (*StaffUser, error)
	// UserByID loads the same staff projection by user id, or ErrNotFound. The
	// change-password flow uses it to re-verify the caller's current password: the
	// session yields a user id, not a username, so this is the id-keyed counterpart
	// of UserByUsername.
	UserByID(ctx context.Context, id string) (*StaffUser, error)
	// UpsertOwner creates or resets the single Owner account direct-to-Postgres
	// (the `felis breakGlass` first-run / reset-password path). role is forced to
	// 'admin' and must_change_password to mustChange; on a username conflict the
	// existing row's email, hash and flag are overwritten so a reset is idempotent.
	UpsertOwner(ctx context.Context, id, username, email, passwordHash string, mustChange bool) error
	// SetPassword stores a new bcrypt hash for a user and clears
	// must_change_password (the panel change-password flow). ErrNotFound when no
	// row matches, so a stale session cannot silently no-op a password change.
	SetPassword(ctx context.Context, userID, passwordHash string) error
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
	// RevokeUserSessionsExcept revokes every live session of a user except the one
	// whose hash is keepTokenHash. The change-password flow calls it so a successful
	// password change logs out the account's other devices but not the current one.
	RevokeUserSessionsExcept(ctx context.Context, userID, keepTokenHash string) error

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
	// CreateUser mints a new user row (role forced to either 'admin' or 'user')
	// with an initial password hash. createdBy is the actor email for audit. A
	// username conflict → ErrConflict.
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
	// AdminResetPassword stores a new bcrypt hash for a user and forces
	// must_change_password, so the admin-set password is replaced on first
	// login. ErrNotFound when no live row matches.
	AdminResetPassword(ctx context.Context, userID, passwordHash string) error

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
	ID                 string    `json:"id"`
	Username           string    `json:"username"`
	Email              string    `json:"email,omitempty"`
	Role               string    `json:"role"`
	Disabled           bool      `json:"disabled"`
	EmailVerified      bool      `json:"email_verified"`
	ServerCount        int       `json:"server_count"`
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// UserDetail is the full admin view of one user, including linked MC accounts.
type UserDetail struct {
	UserView
	DeletedAt     *time.Time       `json:"deleted_at,omitempty"`
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
	Username        string `json:"username"`
	Email           string `json:"email,omitempty"`
	Role            string `json:"role"`
	PasswordHash    string `json:"-"`
	MustChange      bool   `json:"must_change_password"`
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

// SessionView is one live session row visible to an admin.
type SessionView struct {
	TokenHash string     `json:"token_hash"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}
