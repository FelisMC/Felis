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
	// verified mc_uuid (online-mode=true established it); a web user binds it to
	// their user_id later via VerifyLinkCode. This is internal-face only — the web
	// has no verified UUID to mint against (the account_link_codes schema has no
	// user_id column, which forces the in-game origin).
	CreateLinkCode(ctx context.Context, code, mcUUID string, expiresAt time.Time) error
	// VerifyLinkCode consumes a non-expired code for the logged-in user and writes
	// the account_links binding, atomically (spec §10: 网页 verify 填码 → 写
	// account_links). It returns the bound mc_uuid. A missing or expired code →
	// ErrLinkCodeInvalid; a uuid already linked to a *different* user → ErrConflict;
	// re-verifying the same (user, uuid) pair is idempotent. now is the API clock so
	// expiry is testable.
	VerifyLinkCode(ctx context.Context, userID, code string, now time.Time) (mcUUID string, err error)
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
}
