package api

import (
	"context"
	"encoding/json"
	"time"
)

type ServerRecord struct {
	Name      string
	Subdomain string
	OwnerID   string // "" when unclaimed
	// CachedPhase is non-authoritative.
	CachedPhase string
	Retire      *RetireState // nil when no retirement is pending
	Tags        []string     // set by ServerByName only
}

type RetireState struct {
	RequestedAt time.Time `json:"requested_at"`
	Delete      bool      `json:"delete"`
}

// MyServerView exposes detailed lifecycle fields only for owned rows; other status fields are best effort.
type MyServerView struct {
	Name               string            `json:"name"`
	Subdomain          string            `json:"subdomain"`
	Owned              bool              `json:"owned"`
	Claimable          bool              `json:"claimable"`
	Phase              string            `json:"phase,omitempty"`
	PlayersOnline      int32             `json:"playersOnline"`
	PlayersMax         int32             `json:"playersMax"`
	DisplayName        string            `json:"displayName,omitempty"`
	DesiredState       string            `json:"desiredState,omitempty"`
	AutostartPolicy    string            `json:"autostartPolicy,omitempty"`
	PlayerCountUnknown bool              `json:"playerCountUnknown,omitempty"`
	AutoRestarts       int32             `json:"autoRestarts,omitempty"`
	StartGaveUp        bool              `json:"startGaveUp,omitempty"`
	CPU                string            `json:"cpu,omitempty"`
	Memory             string            `json:"memory,omitempty"`
	CPUMilli           *int64            `json:"cpuMilli,omitempty"`
	MemoryBytes        *int64            `json:"memoryBytes,omitempty"`
	Disk               *ServerDiskUsage  `json:"disk,omitempty"`
	DiskFull           bool              `json:"diskFull,omitempty"`
	ExtraPorts         []ServerExtraPort `json:"extraPorts,omitempty"`
	ExtraPortsPending  bool              `json:"extraPortsPending,omitempty"`
	Retiring           *RetireState      `json:"retiring,omitempty"`
	Tags               []string          `json:"tags,omitempty"`
}

type ServerOwnership struct {
	// OwnerID is "" while unclaimed; compare it, not Owner, to decide whose server it is.
	OwnerID string
	Owner   string // display identity: email, or username without one; "" while unclaimed
	// Retire=nil means no request; pending deletion prevents claims.
	Retire *RetireState
}

// AuditEntry is one audit_logs row. Actor is display text; ActorUserID is the binding attribution.
type AuditEntry struct {
	Actor       string
	ActorUserID string
	Source      string
	Action      string
	ServerName  string
	RequestID   string
	ClientIP    string // empty for internal callers
	UserAgent   string
	Payload     []byte // valid JSON or nil (stored as NULL)
}

// AuditRecord is one stored audit_logs row; columns without a value serialize as null.
type AuditRecord struct {
	ID          int64           `json:"id"`
	CreatedAt   time.Time       `json:"created_at"`
	Actor       string          `json:"actor"`
	ActorUserID *string         `json:"actor_user_id"`
	Source      string          `json:"source"`
	Action      string          `json:"action"`
	ServerName  *string         `json:"server_name"`
	RequestID   *string         `json:"request_id"`
	ClientIP    *string         `json:"client_ip"`
	UserAgent   *string         `json:"user_agent"`
	Payload     json.RawMessage `json:"payload"`
}

// AuditQuery filters ListAudit; a zero field does not filter, and BeforeID 0 starts at the newest row.
type AuditQuery struct {
	Actor    string    // case-insensitive substring
	Action   string    // prefix
	Server   string    // exact name
	Since    time.Time // inclusive
	Until    time.Time // exclusive
	Keyword  string    // case-insensitive substring of actor, action, server name or payload text
	BeforeID int64     // only rows with a smaller id
	Limit    int
}

// BackupView is one row of GET /api/v1/backups; it omits backup_ref, the internal storage handle.
type BackupView struct {
	ID          string    `json:"id"`
	ServerName  string    `json:"server_name"`
	FormerOwner string    `json:"former_owner,omitempty"`
	SizeBytes   int64     `json:"size_bytes"`
	Reason      string    `json:"reason"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	// Corrupt blocks restore; SkippedEntries counts links, devices and sockets excluded from the archive.
	Corrupt        bool       `json:"corrupt,omitempty"`
	VerifiedAt     *time.Time `json:"verified_at,omitempty"`
	SkippedEntries int        `json:"skipped_entries,omitempty"`
}

// BackupListOpts.Server empty includes every server in the caller's scope.
type BackupListOpts struct {
	Server string
	Limit  int
	Offset int
}

const (
	DefaultBackupListLimit = 20
	MaxBackupListLimit     = 100
)

// BackupRecord drives a restore; BackupRef and FormerOwner are never serialized to the client.
type BackupRecord struct {
	ID          string
	ServerName  string
	FormerOwner string
	BackupRef   string
	SizeBytes   int64
	Corrupt     bool
	SHA256      string // empty for archives written before digests were kept
	CreatedAt   time.Time
}

type StaffUser struct {
	ID            string
	Username      string
	Email         string
	Role          string
	EmailVerified bool
}

// PasskeyCredential is one bound passkey; it holds only public attestation material.
type PasskeyCredential struct {
	ID             string
	UserID         string
	CredentialID   string // base64url
	PublicKey      string // base64 COSE key
	SignCount      uint32
	AAGUID         string
	Name           string
	CreatedAt      time.Time
	LastUsedAt     *time.Time // nil until a passkey sign-in or step-up
	UserVerified   bool
	BackupEligible bool
	BackupState    bool
}

type SessionedUser struct {
	ID            string
	Username      string
	Email         string
	Role          string
	EmailVerified bool
	LastSeenAt    time.Time // as last recorded by TouchSession, up to sessionTouchEvery stale
	ReauthAt      time.Time // last factor proof (see requireReauth); zero if never
}

// NewSession is one session to record at sign-in; TokenHash is the sha-256 of the cookie value.
type NewSession struct {
	TokenHash string
	UserID    string
	CreatedAt time.Time // API clock; the staff idle cutoff measures from it
	ExpiresAt time.Time
	UserAgent string
	ClientIP  string
	ReauthAt  time.Time // zero for a bind-code sign-in
}

type OpLoginRequest struct {
	ID        string
	UserID    string
	Username  string
	Email     string
	Status    string // 'pending' | 'approved' | 'denied'
	Consumed  bool   // consumed_at is set (single-use guard)
	ExpiresAt time.Time
	CreatedAt time.Time
	ClientIP  string
	UserAgent string
}

type NewOpLoginRequest struct {
	ID, UserID, Email   string
	ClientIP, UserAgent string
	ExpiresAt           time.Time
}

// MigrationView fields remain unset until their step; ConfirmSession is the proving session's token hash.
type MigrationView struct {
	ID             string
	SourceUserID   string
	TargetUserID   string
	State          string
	ConfirmFactor  string
	ConfirmSession string
	ConfirmedAt    *time.Time
	CodeExpiresAt  *time.Time
	CreatedAt      time.Time
}

type Repo interface {
	Ping(ctx context.Context) error

	ServerBySubdomain(ctx context.Context, subdomain string) (*ServerRecord, error)
	ServerByName(ctx context.Context, name string) (*ServerRecord, error)
	IsLinked(ctx context.Context, userID string) (bool, error)
	CreateLinkCode(ctx context.Context, code, mcUUID, authSource string, expiresAt time.Time) error
	// VerifyLinkCode binds atomically and idempotently; invalid codes remain unconsumed on ownership conflicts.
	VerifyLinkCode(ctx context.Context, userID, code string, now time.Time) (mcUUID, authSource string, err error)
	// RedeemPlayerBindCode creates or reuses a player account atomically; invalid, staff-linked or retired accounts leave the code unconsumed.
	RedeemPlayerBindCode(ctx context.Context, newUserID, code string, now time.Time) (userID, mcUUID, authSource string, err error)
	// QuotaCheck is advisory, excludes excludeName from existing usage and treats unset caps as unlimited.
	QuotaCheck(ctx context.Context, userID string, excludeName string, incoming ResourceSpec) (bool, error)
	// ClaimServer atomically checks owner and quota, resetting activity and wake state; false means already claimed or retiring.
	ClaimServer(ctx context.Context, name, userID string) (bool, error)
	UserInAllowlist(ctx context.Context, name, userID string) (bool, error)
	UUIDInAllowlist(ctx context.Context, name, mcUUID string) (bool, error)
	// ServerAllowlist lists the wake allowlist, newest first, including revoked entries.
	ServerAllowlist(ctx context.Context, name string) ([]AllowlistEntry, error)
	// SetAllowlistWake retains revoked entries until ownership changes so rejoining cannot restore access.
	SetAllowlistWake(ctx context.Context, name, mcUUID string, canWake bool) error
	// RequestRetire preserves the first request time and never downgrades deletion to release.
	RequestRetire(ctx context.Context, name string, deleteServer bool) (RetireState, error)
	// CancelRetire requires mayCancelDelete for a pending deletion, otherwise ErrConflict.
	CancelRetire(ctx context.Context, name string, mayCancelDelete bool) error
	// UserByMCUUID returns ErrNotFound for missing, disabled or deleted accounts.
	UserByMCUUID(ctx context.Context, mcUUID string) (userID string, err error)
	// RecordJoin clears inactivity warnings but preserves revoked wake entries.
	RecordJoin(ctx context.Context, name, mcUUID string) error
	MyServers(ctx context.Context, userID string) ([]MyServerView, error)
	// ServerOwners omits deleted servers and uses empty OwnerID for unclaimed ones.
	ServerOwners(ctx context.Context) (map[string]ServerOwnership, error)
	// AssignPreview reports the current owner and the caps userID would pass with one more server of size res.
	AssignPreview(ctx context.Context, name, userID string, res ResourceSpec) (owner string, over []QuotaExcess, err error)
	// AssignServer gives name to userID as a claim would, refusing ErrQuotaExceeded unless allowOver; it returns the previous owner.
	AssignServer(ctx context.Context, name, userID string, res ResourceSpec, allowOver bool) (previous string, over []QuotaExcess, err error)
	// ServerTags lists the tags of live servers that have any.
	ServerTags(ctx context.Context) (map[string][]string, error)
	// SetServerTags replaces the whole tag set; the caller has validated it, and ErrNotFound means no live server.
	SetServerTags(ctx context.Context, name string, tags []string) error
	// ServerNoticeRecipients is the owner's verified email, or every Owner's if unclaimed.
	ServerNoticeRecipients(ctx context.Context, server string) ([]string, error)
	AllBackups(ctx context.Context, opts BackupListOpts) ([]BackupView, int, error)
	// BackupsForUser scopes to former ownership recorded at archive time.
	BackupsForUser(ctx context.Context, userID string, opts BackupListOpts) ([]BackupView, int, error)
	LatestBackup(ctx context.Context, serverName string) (*BackupRecord, error)
	BackupByID(ctx context.Context, id string) (*BackupRecord, error)
	// ExpireBackup removes restore and budget eligibility immediately; retention and offsite sync clean up archive bytes later.
	ExpireBackup(ctx context.Context, id string, at time.Time) error
	// LastBackupRequest returns zero when no manual backup was accepted at or after since.
	LastBackupRequest(ctx context.Context, serverName string, since time.Time) (time.Time, error)
	BackupStoreBytes(ctx context.Context) (int64, error)
	// SeedServer resets a same-name row but rolls back on a subdomain conflict.
	SeedServer(ctx context.Context, name, subdomain string, cpuMilli, memoryMB, storageMB int) error
	// SeedOwnedServer checks effective quotas and inserts ownership atomically; absent quotas disable self-service creation.
	SeedOwnedServer(ctx context.Context, name, subdomain, userID string, res ResourceSpec) error
	DiscardSeededServer(ctx context.Context, name string) error
	SelfServiceState(ctx context.Context, userID string) (QuotaState, error)
	UpdateServerResources(ctx context.Context, name string, cpuMilli, memoryMB, storageMB int) error
	// ResizeServer serializes with claims, caps growth and returns previous values; absent live rows are unchanged.
	ResizeServer(ctx context.Context, name string, to ResourceSpec) (prev ResourceSpec, err error)
	// PatchServerPorts locks the server row and checks a nonempty ownerID before apply.
	PatchServerPorts(ctx context.Context, name, ownerID string, apply func() error) error
	Audit(ctx context.Context, e AuditEntry) error
	// ListAudit returns rows matching q, newest id first.
	ListAudit(ctx context.Context, q AuditQuery) ([]AuditRecord, error)
	// CountAuthFailures counts the sign-in failures recorded at or after since.
	CountAuthFailures(ctx context.Context, since time.Time) (int, error)

	// CreateEmailOTP supersedes live codes for the same (user,purpose), storing only the hash.
	CreateEmailOTP(ctx context.Context, id, userID, email, codeHash, purpose string, expiresAt time.Time) error
	// AddLoginEmailOTP keeps recent codes live so unauthenticated starts cannot invalidate existing sign-in proof.
	AddLoginEmailOTP(ctx context.Context, id, userID, email, codeHash, purpose string, now, expiresAt time.Time) error
	// VerifyEmailOTP consumes a match and verifies its address; wrong hashes count attempts, while email conflicts leave the code live.
	VerifyEmailOTP(ctx context.Context, userID, purpose, codeHash string, now time.Time) (email string, err error)
	SetUserEmail(ctx context.Context, userID, email string) error
	// ConsumeLoginEmailOTP consumes all live codes on a match, applies OTP lockout rules and never changes account identity.
	ConsumeLoginEmailOTP(ctx context.Context, userID, purpose, codeHash string, now time.Time) error
	// OTPLockedUntil returns zero without a current account lock; starts must not mail codes into a locked account.
	OTPLockedUntil(ctx context.Context, userID, purpose string, now time.Time) (time.Time, error)

	CreateOpLoginRequest(ctx context.Context, req NewOpLoginRequest) error
	OpLoginRequestByID(ctx context.Context, id string) (*OpLoginRequest, error)
	// ListPendingOpLogins returns pending, unconsumed, unexpired requests oldest first.
	ListPendingOpLogins(ctx context.Context, now time.Time) ([]OpLoginRequest, error)
	ApproveOpLogin(ctx context.Context, id, approverUserID string, now time.Time) error
	ConsumeOpLoginRequest(ctx context.Context, id string, now time.Time) error

	CreatePasskeyChallenge(ctx context.Context, id, userID, purpose string, sessionData []byte, expiresAt time.Time) error
	ConsumePasskeyChallengeByUser(ctx context.Context, userID, purpose string, now time.Time) (sessionData []byte, err error)
	// AddPasskeyLoginChallenge keeps existing challenges, pruning expired or consumed rows and enforcing the per-source live cap.
	AddPasskeyLoginChallenge(ctx context.Context, id, userID, purpose, source, challenge string, sessionData []byte, now, expiresAt time.Time) error
	ConsumePasskeyLoginChallenge(ctx context.Context, userID, purpose, challenge string, now time.Time) (sessionData []byte, err error)
	// CreateDiscoverableChallenge enforces per-source and global live-row bounds after pruning expired challenges.
	CreateDiscoverableChallenge(ctx context.Context, id, source string, sessionData []byte, now, expiresAt time.Time) error
	// ConsumeDiscoverableChallenge atomically consumes once; unknown, expired and consumed handles share ErrPasskeyChallengeInvalid.
	ConsumeDiscoverableChallenge(ctx context.Context, id string, now time.Time) (sessionData []byte, err error)
	CreatePasskeyCredential(ctx context.Context, c PasskeyCredential) error
	PasskeyCredentialsForUser(ctx context.Context, userID string) ([]PasskeyCredential, error)
	// DeletePasskeyCredential locks across checking the last factor and deletion; an unverified account cannot lose its last passkey.
	DeletePasskeyCredential(ctx context.Context, userID, id string) error
	// DeleteAllPasskeyCredentialsForUser unbinds every passkey of a user; zero rows is success. Pair it with a session revoke.
	DeleteAllPasskeyCredentialsForUser(ctx context.Context, userID string) error
	// AdvanceCredentialSignCount requires accepted clone policy; missing credentials are a no-op and counters cannot regress.
	AdvanceCredentialSignCount(ctx context.Context, credentialID string, newSignCount uint32, usedAt time.Time) error

	// ReclaimUsername atomically bans and holds data, preserving the first expiry on retry; empty dataRef stores NULL.
	ReclaimUsername(ctx context.Context, id, squatterUUID, username, dataRef string, expiresAt time.Time) (time.Time, error)
	IsUsernameBlacklisted(ctx context.Context, mcUUID string) (bool, error)
	IsProtectedAdminLink(ctx context.Context, mcUUID string) (bool, error)

	UserByUsername(ctx context.Context, username string) (*StaffUser, error)
	UserByID(ctx context.Context, id string) (*StaffUser, error)
	// UserByEmail is case-insensitive and resolves only verified addresses on live accounts, regardless of role.
	UserByEmail(ctx context.Context, email string) (*StaffUser, error)
	UpsertOwner(ctx context.Context, id, username, email string) error
	AdminExists(ctx context.Context) (bool, error)
	CreateSession(ctx context.Context, s NewSession) error
	// SessionUser requires an unrevoked, unexpired session on a live account; staff must also satisfy staffSessionIdle.
	SessionUser(ctx context.Context, tokenHash string, now time.Time) (*SessionedUser, error)
	// TouchSession records activity at now; it never moves last_seen_at backwards, and an absent session is not an error.
	TouchSession(ctx context.Context, tokenHash string, now time.Time) error
	MarkSessionReauth(ctx context.Context, tokenHash string, at time.Time) error

	// CreateAPIToken fills CreatedAt, or returns ErrAPITokenLimit when the user already has limit live keys.
	CreateAPIToken(ctx context.Context, t *APIToken, hash string, limit int, now time.Time) error
	// ListAPITokens lists the user's live keys, newest first.
	ListAPITokens(ctx context.Context, userID string, now time.Time) ([]APIToken, error)
	// RevokeAPIToken returns ErrNotFound for another user's or an already revoked key.
	RevokeAPIToken(ctx context.Context, userID, id string, now time.Time) error
	// APITokenUser resolves a live key of an active account.
	APITokenUser(ctx context.Context, hash string, now time.Time) (*StaffUser, *APIToken, error)
	TouchAPIToken(ctx context.Context, id string, now time.Time) error
	RevokeSession(ctx context.Context, tokenHash string) error
	RevokeUserSession(ctx context.Context, userID, tokenHash string) error
	// RevokeOtherUserSessions revokes every live session of userID except keepTokenHash (all when empty) and returns the count.
	RevokeOtherUserSessions(ctx context.Context, userID, keepTokenHash string) (int, error)

	// RedeemSetupToken atomically spends the token and creates its user's session, ignoring s.UserID; failure leaves the token usable.
	RedeemSetupToken(ctx context.Context, tokenHash string, now time.Time, s NewSession) (userID string, err error)

	// GetSetting returns raw jsonb or ErrNotFound; callers choose their refresh policy.
	GetSetting(ctx context.Context, key string) ([]byte, error)
	SetSetting(ctx context.Context, key string, value []byte) error
	// CompareAndSetSetting returns ErrConflict on a concurrent edit; a nil expected creates only a never-written setting.
	CompareAndSetSetting(ctx context.Context, key string, expected, value []byte) error

	ListUsers(ctx context.Context, opts ListUsersOpts) ([]UserView, int, error)
	// UserDetail includes deleted users because their rows remain for auditing.
	UserDetail(ctx context.Context, userID string) (*UserDetail, error)
	CreateUser(ctx context.Context, input CreateUserInput, createdBy string) (*UserView, error)
	UpdateUser(ctx context.Context, userID string, patch UpdateUserInput, updatedBy string) (*UserView, error)
	// DeleteUser soft-deletes and revokes sessions, releases servers and removes credentials while retaining the audit identity.
	DeleteUser(ctx context.Context, userID, deletedBy string, clearPorts func(context.Context, string) error) error
	// SetUserDisabled flips the disabled flag of a live user, revoking every session when disabling; ErrNotFound if unknown or deleted.
	SetUserDisabled(ctx context.Context, userID string, disabled bool) error

	// GetQuotas returns the user's quotas row, or a zero-value view when none exists (default quota applies, see readQuota).
	GetQuotas(ctx context.Context, userID string) (*QuotaView, error)
	// SetQuotas replaces the quotas row: a nil field is NULL (unlimited), any other value is the cap, 0 included.
	SetQuotas(ctx context.Context, userID string, q QuotaInput, setBy string) (*QuotaView, error)
	ClearQuotas(ctx context.Context, userID string) (*QuotaView, error)

	ListUserSessions(ctx context.Context, userID string, now time.Time) ([]SessionView, error)
	// RevokeAllUserSessions revokes every live session of userID; zero sessions is not an error.
	RevokeAllUserSessions(ctx context.Context, userID string) error

	UnlinkAccount(ctx context.Context, userID, mcUUID string) error
	LinkAccount(ctx context.Context, userID, mcUUID, authSource string) error

	// StartMigration puts a live source account into migrate mode, superseding any unfinished migration; ErrNotFound for a retired account.
	StartMigration(ctx context.Context, id, sourceUserID string, now time.Time) error
	MigrationForSource(ctx context.Context, sourceUserID string) (*MigrationView, error)
	// ConfirmMigration accepts initiated, expired or other-session confirmation states; other transitions return ErrConflict.
	ConfirmMigration(ctx context.Context, sourceUserID, factor, session string, now time.Time) error
	// IssueMigrationCode requires fresh same-session confirmation; callers must validate a live target distinct from the source.
	IssueMigrationCode(ctx context.Context, sourceUserID, targetUserID, session, codeHash string, now, expiresAt time.Time) error
	// RedeemMigration atomically transfers ownership and retires the source; quota refusals preserve the code and credentials never move.
	RedeemMigration(ctx context.Context, targetUserID, codeHash string, now time.Time) (sourceUserID string, movedServers []string, err error)
}

type ListUsersOpts struct {
	Query  string // substring match on username or email
	Role   string // exact role match ("admin" / "user"), or "" for all
	Hidden string // "true" = disabled only, "false" = enabled only, "" = all
	Limit  int    // page size; 0 → default 20
	Offset int
}

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

type UserDetail struct {
	UserView
	DeletedAt      *time.Time      `json:"deleted_at,omitempty"`
	LinkedAccounts []LinkedAccount `json:"linked_accounts,omitempty"`
}

// AllowlistEntry retains revoked entries; Username is empty without a live linked account.
type AllowlistEntry struct {
	MCUUID   string    `json:"mc_uuid"`
	Username string    `json:"username,omitempty"`
	AddedAt  time.Time `json:"added_at"`
	CanWake  bool      `json:"can_wake"`
}

type LinkedAccount struct {
	MCUUID     string    `json:"mc_uuid"`
	AuthSource string    `json:"auth_source"`
	VerifiedAt time.Time `json:"verified_at"`
}

type CreateUserInput struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role"`
}

type UpdateUserInput struct {
	Username *string `json:"username,omitempty"`
	Email    *string `json:"email,omitempty"`
	Role     *string `json:"role,omitempty"`
}

// QuotaView is the admin-visible quotas row; Custom is false when the user has no row of their own (default quota applies).
type QuotaView struct {
	UserID       string `json:"user_id"`
	Custom       bool   `json:"custom"`
	MaxServers   *int   `json:"max_servers,omitempty"`
	MaxCPUMilli  *int   `json:"max_cpu_milli,omitempty"`
	MaxMemoryMB  *int   `json:"max_memory_mb,omitempty"`
	MaxStorageGB *int   `json:"max_storage_gb,omitempty"`
}

// QuotaInput replaces all four caps at once: absent or null is unlimited, 0 grants none. The handler rejects values outside 0..MaxInt32.
type QuotaInput struct {
	MaxServers   *int `json:"max_servers,omitempty"`
	MaxCPUMilli  *int `json:"max_cpu_milli,omitempty"`
	MaxMemoryMB  *int `json:"max_memory_mb,omitempty"`
	MaxStorageGB *int `json:"max_storage_gb,omitempty"`
}

// QuotaState is the quota in force for a user. Source is "user", "default" or "" (unlimited claims, no server creation, nil Limits).
type QuotaState struct {
	Source string      `json:"source"`
	Limits *QuotaInput `json:"limits"`
	Usage  QuotaUsage  `json:"usage"`
}

type QuotaUsage struct {
	Servers   int `json:"servers"`
	CPUMilli  int `json:"cpu_milli"`
	MemoryMB  int `json:"memory_mb"`
	StorageMB int `json:"storage_mb"`
}

type ResourceSpec struct {
	CPUMilli  int // millicores
	MemoryMB  int
	StorageMB int
}

type SessionView struct {
	TokenHash  string     `json:"token_hash"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	UserAgent  string     `json:"user_agent"`
	ClientIP   string     `json:"client_ip"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	Current    bool       `json:"current,omitempty"`
}
