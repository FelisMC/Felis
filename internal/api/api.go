// Package api implements felis-api: an internal face (service tokens) and an external face (session cookies).
package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/naming"
)

type API struct {
	Distribution Distribution
	NodeControl  NodeControl
	Repo         Repo
	Cluster      Cluster
	Usage        *UsageSampler
	Internal     InternalAuth
	External     ExternalAuth

	Builder ImageBuilder // nil: /images routes answer 503

	// Images pins a whitelisted image ref to its digest on server create or image change; nil stores refs as given.
	Images ImagePinner
	// Software reads upstream server-software metadata; nil answers its routes 503.
	Software SoftwareCatalog
	// RuntimeRegistry is the registry host[:port] the Java runtime images are mirrored into.
	RuntimeRegistry string

	Console   Console
	Logs      LogStreamer
	BuildLogs LogStreamer

	Restorer Restorer

	// AuditExport writes JSON lines over [since,until); zero bounds are open and nil disables export.
	AuditExport func(ctx context.Context, since, until time.Time, w io.Writer) (int, error)

	Backuper  Backuper
	JobStatus JobStatusReader

	// RestoreChains requires both a snapshot-capable Backuper and a Restorer.
	RestoreChains RestoreChains

	Files FileEditor
	// FileStage holds uploads until the landing Job fetches them from InternalBaseURL; uploads answer 503 unless both are set.
	FileStage          *fileedit.Stage
	FileBrowser        *fileedit.Browser
	FileDownloadClient *http.Client
	InternalBaseURL    string

	Exporter  Exporter // starts the Job behind world/backup downloads; the export routes answer 503 unless it and InternalBaseURL are set
	Schedules ServerSchedules

	Submissions SubmissionService

	// Mailer=nil disables OTP delivery and removes email as a usable login or reauth factor.
	Mailer OTPMailer

	Passkey PasskeyVerifier

	RootDomain string // deployment zone; hostnames are validated against it

	// AdminHostname defaults to op.console.<RootDomain>; non-staff sessions are refused on this host.
	AdminHostname string

	// PanelHostname is the player console host used for panel URLs; empty falls back to console.<RootDomain>.
	PanelHostname string

	WakeCooldown time.Duration

	// BackupCooldown and BackupStoreCap exempt staff and internal operations; zero disables the limit.
	BackupCooldown time.Duration
	BackupStoreCap int64
	// ArchivePolicy, when set, supersedes both with the retention in force and spaces scheduled restore points.
	ArchivePolicy *ArchivePolicy

	SubmitCreateCooldown time.Duration
	SubmitUploadCooldown time.Duration

	// MaxRunningServers and WakeCooldown are defaults overridden by the saved wake policy; zero disables the limit.
	MaxRunningServers int

	MaxStreamsPerPrincipal int

	// AuthSources is the upstream Yggdrasil list for hasJoined, in priority order; nil rejects every login (204).
	AuthSources        []AuthSource
	AuthSourceSettings *AuthSourceSettings
	// MailSettings=nil retains the static Mailer; otherwise it resolves the effective relay per send.
	MailSettings *MailSettings

	// AuthDoorLimit is per client address and MailLimit install-wide; zero disables each limit.
	AuthDoorLimit RateLimit
	MailLimit     RateLimit
	// ClientIPHeader names the edge header carrying the client address; empty means the TCP peer.
	ClientIPHeader string

	AccessLog *slog.Logger

	Now func() time.Time

	fileCommits sync.Map

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

	failover failoverState

	brandingCache brandingCache

	ownerBound atomic.Bool

	drainInit  sync.Once
	drainClose sync.Once
	drain      chan struct{}
}

func (a *API) streamsClosing() <-chan struct{} {
	a.drainInit.Do(func() { a.drain = make(chan struct{}) })
	return a.drain
}

// CloseStreams must be registered with RegisterOnShutdown because http.Server.Shutdown does not cancel streaming handlers.
func (a *API) CloseStreams() {
	a.streamsClosing()
	a.drainClose.Do(func() { close(a.drain) })
}

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

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *API) limiter() *cooldownLimiter {
	a.cooldownOnce.Do(func() {
		a.cooldown = &cooldownLimiter{now: a.now, last: map[string]time.Time{}}
	})
	return a.cooldown
}

func (a *API) otpLimiter() *cooldownLimiter {
	a.otpCooldownOnce.Do(func() {
		a.otpCooldown = &cooldownLimiter{now: a.now, last: map[string]time.Time{}}
	})
	return a.otpCooldown
}

// submitLimiter is process-local, so cooldown spacing applies per replica.
func (a *API) submitLimiter() *cooldownLimiter {
	a.submitCooldownOnce.Do(func() {
		a.submitCooldown = &cooldownLimiter{now: a.now, last: map[string]time.Time{}}
	})
	return a.submitCooldown
}

func (a *API) streamGate() *streamLimiter {
	a.streamCapOnce.Do(func() {
		a.streamCap = newStreamLimiter(a.MaxStreamsPerPrincipal)
	})
	return a.streamCap
}

// streamKey uses user ID, then email, then a shared empty bucket.
func streamKey(p *Principal) string {
	if p == nil {
		return ""
	}
	if p.UserID != "" {
		return p.UserID
	}
	return p.Email
}

// Route tables also drive OpenAPI parity checks.
type apiRoute struct {
	Method  string
	Pattern string

	// Public routes are mounted on the outer mux without auth middleware.
	Public bool

	Admin bool

	Owner bool

	SetupAllowed bool

	AuthDoor bool

	Callers []Caller

	h http.HandlerFunc
}

func (a *API) internalAPIRoutes() []apiRoute {
	proxy := []Caller{CallerVelocity}
	gate := []Caller{CallerVelocity, CallerLimbo}
	build := []Caller{CallerBuild}
	ops := []Caller{CallerOps}
	return []apiRoute{
		{Method: "GET", Pattern: "/healthz", Public: true, h: a.handleHealthz},
		{Method: "GET", Pattern: "/readyz", Public: true, h: a.handleReadyz},
		{Method: "GET", Pattern: "/metrics", Public: true, h: a.handleMetrics},

		{Method: "GET", Pattern: "/api/v1/servers", Callers: proxy, h: a.handleListServers},
		{Method: "GET", Pattern: "/api/v1/internal/submissions/{id}/context", Callers: build, h: a.handleInternalSubmissionContext},
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/ready", Callers: proxy, h: a.handleReady},
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/join-event", Callers: proxy, h: a.handleJoinEvent},
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/wake", Callers: proxy, h: a.handleInternalWake},
		{Method: "GET", Pattern: "/api/v1/internal/servers/{name}/status", Callers: proxy, h: a.handleStatus},
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/claim", Callers: proxy, h: a.handleInternalClaim},
		{Method: "GET", Pattern: "/api/v1/internal/servers/{name}/menu", Callers: proxy, h: a.handleInternalMenuStatus},
		{Method: "GET", Pattern: "/api/v1/internal/player/menu-access/{mc_uuid}", Callers: proxy, h: a.handleInternalMenuAccess},
		{Method: "POST", Pattern: "/api/v1/internal/account/link/code", Callers: gate, h: a.handleCreateLinkCode},
		{Method: "GET", Pattern: "/api/v1/internal/account/link/status/{mc_uuid}", Callers: gate, h: a.handleLinkStatus},
		{Method: "GET", Pattern: "/api/v1/internal/settings/entry-policy", Callers: gate, h: a.handleGetEntryPolicy},
		{Method: "POST", Pattern: "/api/v1/internal/account/migrate/start", Callers: proxy, h: a.handleMigrateStart},
		{Method: "POST", Pattern: "/api/v1/internal/player/reclaim", Callers: proxy, h: a.handleReclaimUsername},
		{Method: "GET", Pattern: "/api/v1/internal/player/blacklist/{mc_uuid}", Callers: gate, h: a.handleCheckBlacklist},
		{Method: "GET", Pattern: "/session/minecraft/hasJoined", Public: true, h: a.handleHasJoined},
		{Method: "GET", Pattern: "/api/v1/internal/op-login/pending", Callers: proxy, h: a.handleOpLoginPending},
		{Method: "GET", Pattern: "/api/v1/internal/op-login/{id}", Callers: proxy, h: a.handleOpLoginShow},
		{Method: "POST", Pattern: "/api/v1/internal/op-login/{id}/approve", Callers: proxy, h: a.handleOpLoginApprove},

		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/backup", Callers: ops, h: a.handleInternalBackup},
		{Method: "POST", Pattern: "/api/v1/internal/servers/{name}/restore", Callers: ops, h: a.handleInternalRestore},

		// Upload Jobs authenticate with their single-use upload token rather than a service token.
		{Method: "GET", Pattern: "/api/v1/internal/file-uploads/{id}", Public: true, h: a.handleInternalFileUpload},
		{Method: "DELETE", Pattern: "/api/v1/internal/file-uploads/{id}", Public: true, h: a.handleInternalFileUploadLanded},
		{Method: "POST", Pattern: "/api/v1/internal/file-browser/{id}", Public: true, h: a.handleInternalFileBrowser},

		// Exports use a one-time token and replace the minimum-rate body deadline with a stall timeout.
		{Method: "PUT", Pattern: "/api/v1/internal/exports/{id}", Public: true, h: a.handleInternalExportUpload},
	}
}

func (a *API) externalAPIRoutes() []apiRoute {
	return []apiRoute{
		{Method: "GET", Pattern: "/healthz", Public: true, h: a.handleHealthz},

		{Method: "POST", Pattern: "/api/v1/auth/logout", Public: true, h: a.handleLogout},
		{Method: "POST", Pattern: "/api/v1/auth/options", Public: true, AuthDoor: true, h: a.handleAuthOptions},
		{Method: "POST", Pattern: "/api/v1/auth/setup/redeem", Public: true, AuthDoor: true, h: a.handleSetupRedeem},
		{Method: "GET", Pattern: "/api/v1/auth/setup/status", SetupAllowed: true, h: a.handleSetupStatus},
		{Method: "GET", Pattern: "/api/v1/auth/owner-status", Public: true, h: a.handleOwnerStatus},
		{Method: "GET", Pattern: "/api/v1/panel/language", Public: true, h: a.handlePanelLanguage},
		{Method: "GET", Pattern: "/api/v1/branding", Public: true, h: a.handlePublicBranding},
		{Method: "GET", Pattern: "/api/v1/branding/logo", Public: true, h: a.handleBrandingImage(brandingLogo)},
		{Method: "GET", Pattern: "/api/v1/branding/background", Public: true, h: a.handleBrandingImage(brandingBackground)},
		{Method: "POST", Pattern: "/api/v1/auth/passkey/login/begin", Public: true, AuthDoor: true, h: a.handlePasskeyLoginBegin},
		{Method: "POST", Pattern: "/api/v1/auth/passkey/login/finish", Public: true, AuthDoor: true, h: a.handlePasskeyLoginFinish},
		{Method: "POST", Pattern: "/api/v1/auth/passkey/login/discoverable/begin", Public: true, AuthDoor: true, h: a.handlePasskeyLoginDiscoverableBegin},
		{Method: "POST", Pattern: "/api/v1/auth/passkey/login/discoverable/finish", Public: true, AuthDoor: true, h: a.handlePasskeyLoginDiscoverableFinish},
		{Method: "POST", Pattern: "/api/v1/auth/email/start", Public: true, AuthDoor: true, h: a.handleLoginEmailStart},
		{Method: "POST", Pattern: "/api/v1/auth/email/verify", Public: true, AuthDoor: true, h: a.handleLoginEmailVerify},
		{Method: "POST", Pattern: "/api/v1/auth/op-login/start", Public: true, AuthDoor: true, h: a.handleOpLoginStart},
		{Method: "GET", Pattern: "/api/v1/auth/op-login/status/{id}", Public: true, h: a.handleOpLoginStatus},
		{Method: "POST", Pattern: "/api/v1/auth/op-login/finish", Public: true, AuthDoor: true, h: a.handleOpLoginFinish},
		{Method: "POST", Pattern: "/api/v1/auth/bind", Public: true, AuthDoor: true, h: a.handleBindRedeem},

		{Method: "POST", Pattern: "/api/v1/servers/{name}/wake", h: a.handleWake},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/stop", h: a.handleStop},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/kill", h: a.handleKill},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/emergency-stop", h: a.handleEmergencyStop, Admin: true, Owner: true},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/claim", h: a.handleClaim},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/assignment", Owner: true, Admin: true, h: a.handleAssignPreview},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/assignment", Owner: true, Admin: true, h: a.handleAssignServer},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/command", h: a.handleCommand},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/console", h: a.handleServerConsole},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/console/history", h: a.handleConsoleHistory},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/whitelist", h: a.handleAccessWhitelist},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/access/whitelist", h: a.handleAccessWhitelistList},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/access/players", h: a.handleAccessPlayers},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/kick", h: a.handleAccessKick},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/ban", h: a.handleAccessBan},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/access/ban", h: a.handleAccessBanList},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/permission", h: a.handleAccessPermission},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/access/group", h: a.handleAccessGroup},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/access/luckperms/{player}", h: a.handleAccessLuckPermsInfo},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/allowlist", h: a.handleAllowlistList},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/allowlist/{uuid}", h: a.handleAllowlistSetWake},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/retirement", h: a.handleRetire},
		{Method: "DELETE", Pattern: "/api/v1/servers/{name}/retirement", h: a.handleCancelRetire},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/tags", h: a.handleSetServerTags},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/restart", h: a.handleRestart},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/status", h: a.handleStatus},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/startup-settings", h: a.handleStartupSettings},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/usage", h: a.handleUsage},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/extensions", h: a.handleExtensions},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/config/paper-global", h: a.handleReadPaperGlobal},
		{Method: "PATCH", Pattern: "/api/v1/servers/{name}/config/paper-global", h: a.handlePatchPaperGlobal},
		{Method: "GET", Pattern: "/api/v1/me", SetupAllowed: true, h: a.handleMe},
		{Method: "GET", Pattern: "/api/v1/me/servers", SetupAllowed: true, h: a.handleMyServers},
		{Method: "GET", Pattern: "/api/v1/software", h: a.handleSoftwareCatalog},
		{Method: "GET", Pattern: "/api/v1/software/{type}/versions", h: a.handleSoftwareVersions},
		{Method: "GET", Pattern: "/api/v1/software/{type}/versions/{version}", h: a.handleSoftwareVersion},
		{Method: "GET", Pattern: "/api/v1/me/server-quota", h: a.handleMyServerQuota},
		{Method: "POST", Pattern: "/api/v1/me/servers", h: a.handleSelfCreateServer},
		{Method: "PATCH", Pattern: "/api/v1/me/servers/{name}", h: a.handleOwnerPatchServer},
		{Method: "GET", Pattern: "/api/v1/backups", h: a.handleListBackups},
		{Method: "DELETE", Pattern: "/api/v1/backups/{id}", h: a.handleDeleteBackup},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/jobs", h: a.handleServerJobs},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/restore-backup", h: a.handleRestoreBackup},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/backup", h: a.handleBackupNow},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/backups/{id}/export", h: a.handleExportBackup},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/world/export", h: a.handleExportWorld},
		{Method: "GET", Pattern: "/api/v1/exports/{ticket}", h: a.handleExportStatus},
		{Method: "GET", Pattern: "/api/v1/exports/{ticket}/download", h: a.handleExportDownload},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/files", h: a.handleListFiles},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/files/search", h: a.handleSearchFiles},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/transfer", h: a.handleTransferFiles},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/zip", h: a.handleZipFiles},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/from-url", h: a.handleFileFromURL},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/file", h: a.handleReadFile},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/file", h: a.handleWriteFile},
		{Method: "DELETE", Pattern: "/api/v1/servers/{name}/file", h: a.handleDeleteFile},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/mkdir", h: a.handleMkdir},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/rename", h: a.handleRenameFile},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/files/upload", h: a.handleUploadFile},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/uploads", h: a.handleBeginFileUpload},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/files/uploads/{id}", h: a.handleFileUploadStatus},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/files/uploads/{id}", h: a.handleFileUploadPart},
		{Method: "DELETE", Pattern: "/api/v1/servers/{name}/files/uploads/{id}", h: a.handleDropFileUpload},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/uploads/{id}/commit", h: a.handleCommitFileUpload},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/unzip", h: a.handleUnzipFile},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/files/ops", h: a.handleListFileOps},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/files/download", h: a.handleDownloadFile},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/schedules", h: a.handleListSchedules},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/schedules", h: a.handleCreateSchedule},
		{Method: "PUT", Pattern: "/api/v1/servers/{name}/schedules/{id}", h: a.handleUpdateSchedule},
		{Method: "DELETE", Pattern: "/api/v1/servers/{name}/schedules/{id}", h: a.handleDeleteSchedule},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/schedules/{id}/run", h: a.handleRunSchedule},
		{Method: "POST", Pattern: "/api/v1/account/link/start", SetupAllowed: true, h: a.handleLinkStart},
		{Method: "POST", Pattern: "/api/v1/account/link/verify", SetupAllowed: true, h: a.handleLinkVerify},
		{Method: "GET", Pattern: "/api/v1/account/link/sources", Admin: true, h: a.handleLinkSources},
		{Method: "GET", Pattern: "/api/v1/settings/node-control", Owner: true, Admin: true, h: a.handleNodeTasks},
		{Method: "POST", Pattern: "/api/v1/settings/node-control/tasks", Owner: true, Admin: true, h: a.handleStartNodeTask},
		{Method: "GET", Pattern: "/api/v1/settings/node-control/tasks/{id}", Owner: true, Admin: true, h: a.handleNodeTask},
		{Method: "POST", Pattern: "/api/v1/settings/node-control/tasks/{id}/retry", Owner: true, Admin: true, h: a.handleRetryNodeTask},
		{Method: "GET", Pattern: "/api/v1/settings/node-control/tasks/{id}/artifact", Owner: true, Admin: true, h: a.handleNodeTaskArtifact},
		{Method: "GET", Pattern: "/api/v1/platform/host", Owner: true, Admin: true, h: a.handleHostOverview},
		{Method: "POST", Pattern: "/api/v1/platform/host/quiet", Owner: true, Admin: true, h: a.handleHostQuiet},
		{Method: "POST", Pattern: "/api/v1/platform/host/ignore", Owner: true, Admin: true, h: a.handleHostIgnore},
		{Method: "GET", Pattern: "/api/v1/platform/audit", Owner: true, Admin: true, h: a.handleListAudit},
		{Method: "GET", Pattern: "/api/v1/platform/audit-export", Owner: true, Admin: true, h: a.handleAuditExport},
		{Method: "POST", Pattern: "/api/v1/platform/offsite/key", Owner: true, Admin: true, h: a.handleOffsiteKey},
		{Method: "GET", Pattern: "/api/v1/settings/entry-policy", Owner: true, Admin: true, h: a.handleGetEntryPolicy},
		{Method: "PUT", Pattern: "/api/v1/settings/entry-policy", Owner: true, Admin: true, h: a.handleSetEntryPolicy},
		{Method: "GET", Pattern: "/api/v1/settings/wake-policy", Owner: true, Admin: true, h: a.handleGetWakePolicy},
		{Method: "PUT", Pattern: "/api/v1/settings/wake-policy", Owner: true, Admin: true, h: a.handleSetWakePolicy},
		{Method: "GET", Pattern: "/api/v1/settings/api-tokens", Owner: true, Admin: true, h: a.handleGetAPITokenSettings},
		{Method: "PUT", Pattern: "/api/v1/settings/api-tokens", Owner: true, Admin: true, h: a.handleSetAPITokenSettings},
		{Method: "GET", Pattern: "/api/v1/settings/default-language", Owner: true, Admin: true, h: a.handleGetDefaultLanguage},
		{Method: "PUT", Pattern: "/api/v1/settings/default-language", Owner: true, Admin: true, h: a.handleSetDefaultLanguage},
		{Method: "GET", Pattern: "/api/v1/settings/branding", Owner: true, Admin: true, h: a.handleGetBranding},
		{Method: "PUT", Pattern: "/api/v1/settings/branding", Owner: true, Admin: true, h: a.handleSetBranding},
		{Method: "PUT", Pattern: "/api/v1/settings/branding/logo", Owner: true, Admin: true, h: a.handleSetBrandingImage(brandingLogo)},
		{Method: "DELETE", Pattern: "/api/v1/settings/branding/logo", Owner: true, Admin: true, h: a.handleClearBrandingImage(brandingLogo)},
		{Method: "PUT", Pattern: "/api/v1/settings/branding/background", Owner: true, Admin: true, h: a.handleSetBrandingImage(brandingBackground)},
		{Method: "DELETE", Pattern: "/api/v1/settings/branding/background", Owner: true, Admin: true, h: a.handleClearBrandingImage(brandingBackground)},
		{Method: "GET", Pattern: "/api/v1/settings/auth-sources", Owner: true, Admin: true, h: a.handleGetAuthSources},
		{Method: "PUT", Pattern: "/api/v1/settings/auth-sources", Owner: true, Admin: true, h: a.handleSetAuthSources},
		{Method: "POST", Pattern: "/api/v1/settings/auth-sources/test", Owner: true, Admin: true, h: a.handleTestAuthSource},
		{Method: "GET", Pattern: "/api/v1/settings/smtp", Owner: true, Admin: true, h: a.handleGetMailSettings},
		{Method: "PUT", Pattern: "/api/v1/settings/smtp", Owner: true, Admin: true, h: a.handleSetMailSettings},
		{Method: "DELETE", Pattern: "/api/v1/settings/smtp", Owner: true, Admin: true, h: a.handleClearMailSettings},
		{Method: "POST", Pattern: "/api/v1/settings/smtp/test", Owner: true, Admin: true, h: a.handleTestMailSettings},
		{Method: "GET", Pattern: "/api/v1/settings/archive-policy", Owner: true, Admin: true, h: a.handleGetArchivePolicy},
		{Method: "PUT", Pattern: "/api/v1/settings/archive-policy", Owner: true, Admin: true, h: a.handleSetArchivePolicy},
		{Method: "DELETE", Pattern: "/api/v1/settings/archive-policy", Owner: true, Admin: true, h: a.handleClearArchivePolicy},
		{Method: "GET", Pattern: "/api/v1/settings/heartbeat", Owner: true, Admin: true, h: a.handleGetHeartbeat},
		{Method: "PUT", Pattern: "/api/v1/settings/heartbeat", Owner: true, Admin: true, h: a.handleSetHeartbeat},
		{Method: "DELETE", Pattern: "/api/v1/settings/heartbeat", Owner: true, Admin: true, h: a.handleClearHeartbeat},
		{Method: "GET", Pattern: "/api/v1/account/link/profile", Admin: true, h: a.handleLookupProfile},
		{Method: "POST", Pattern: "/api/v1/account/link/profile", Admin: true, h: a.handleLinkProfile},
		{Method: "POST", Pattern: "/api/v1/account/email/start", SetupAllowed: true, h: a.handleEmailOTPStart},
		{Method: "POST", Pattern: "/api/v1/account/email/verify", SetupAllowed: true, h: a.handleEmailOTPVerify},
		{Method: "POST", Pattern: "/api/v1/account/email", SetupAllowed: true, h: a.handleSetEmail},
		{Method: "POST", Pattern: "/api/v1/account/passkey/register/begin", SetupAllowed: true, h: a.handlePasskeyRegisterBegin},
		{Method: "POST", Pattern: "/api/v1/account/passkey/register/finish", SetupAllowed: true, h: a.handlePasskeyRegisterFinish},
		{Method: "GET", Pattern: "/api/v1/account/passkey/credentials", SetupAllowed: true, h: a.handlePasskeyList},
		{Method: "DELETE", Pattern: "/api/v1/account/passkey/credentials/{id}", SetupAllowed: true, h: a.handlePasskeyDelete},
		{Method: "GET", Pattern: "/api/v1/account/reauth", SetupAllowed: true, h: a.handleReauthStatus},
		{Method: "POST", Pattern: "/api/v1/account/reauth/passkey/begin", SetupAllowed: true, h: a.handleReauthPasskeyBegin},
		{Method: "POST", Pattern: "/api/v1/account/reauth/passkey/finish", SetupAllowed: true, h: a.handleReauthPasskeyFinish},
		{Method: "POST", Pattern: "/api/v1/account/reauth/email/start", SetupAllowed: true, h: a.handleReauthEmailStart},
		{Method: "POST", Pattern: "/api/v1/account/reauth/email/verify", SetupAllowed: true, h: a.handleReauthEmailVerify},
		{Method: "GET", Pattern: "/api/v1/account/sessions", h: a.handleListMySessions},
		{Method: "DELETE", Pattern: "/api/v1/account/sessions/{hash}", h: a.handleRevokeMySession},
		{Method: "POST", Pattern: "/api/v1/account/sessions/revoke-others", h: a.handleRevokeMyOtherSessions},
		{Method: "GET", Pattern: "/api/v1/account/api-tokens", h: a.handleListAPITokens},
		{Method: "POST", Pattern: "/api/v1/account/api-tokens", h: a.handleCreateAPIToken},
		{Method: "DELETE", Pattern: "/api/v1/account/api-tokens/{id}", h: a.handleRevokeAPIToken},
		{Method: "GET", Pattern: "/api/v1/account/migrate", h: a.handleMigrateStatus},
		{Method: "POST", Pattern: "/api/v1/account/migrate/confirm/otp/start", h: a.handleMigrateConfirmOTPStart},
		{Method: "POST", Pattern: "/api/v1/account/migrate/confirm/otp/verify", h: a.handleMigrateConfirmOTPVerify},
		{Method: "POST", Pattern: "/api/v1/account/migrate/confirm/passkey/begin", h: a.handleMigrateConfirmPasskeyBegin},
		{Method: "POST", Pattern: "/api/v1/account/migrate/confirm/passkey/finish", h: a.handleMigrateConfirmPasskeyFinish},
		{Method: "POST", Pattern: "/api/v1/account/migrate/issue-code", h: a.handleMigrateIssueCode},
		{Method: "POST", Pattern: "/api/v1/account/migrate/redeem", h: a.handleMigrateRedeem},
		{Method: "POST", Pattern: "/api/v1/me/submissions", h: a.handleCreateSubmission},
		{Method: "GET", Pattern: "/api/v1/me/submissions", h: a.handleMySubmissions},
		{Method: "GET", Pattern: "/api/v1/me/submissions/limits", h: a.handleSubmissionLimits},
		{Method: "POST", Pattern: "/api/v1/me/submissions/{id}/context", h: a.handleUploadSubmissionContext},
		{Method: "GET", Pattern: "/api/v1/me/submissions/{id}/context/upload", h: a.handleContextUploadStatus},
		{Method: "PUT", Pattern: "/api/v1/me/submissions/{id}/context/upload", h: a.handleContextUploadPart},
		{Method: "POST", Pattern: "/api/v1/me/submissions/{id}/context/upload/complete", h: a.handleContextUploadComplete},
		{Method: "DELETE", Pattern: "/api/v1/me/submissions/{id}", h: a.handleWithdrawSubmission},
		{Method: "POST", Pattern: "/api/v1/servers", Admin: true, h: a.handleCreateServer},
		{Method: "PATCH", Pattern: "/api/v1/servers/{name}", Admin: true, h: a.handlePatchServer},
		{Method: "GET", Pattern: "/api/v1/nodes", Admin: true, h: a.handleNodes},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/migrations", Admin: true, h: a.handleMigrationStatus},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/migrations", Admin: true, h: a.handleMigration},
		{Method: "GET", Pattern: "/api/v1/servers/{name}/migrations/{id}", Admin: true, h: a.handleMigrationStatus},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/migrations/{id}/retry", Admin: true, h: a.handleMigrationRetry},
		{Method: "POST", Pattern: "/api/v1/servers/{name}/migrations/{id}/abort", Admin: true, h: a.handleMigrationAbort},
		{Method: "GET", Pattern: "/api/v1/nodes/{node}/failover", Owner: true, Admin: true, h: a.handleFailoverPlan},
		{Method: "POST", Pattern: "/api/v1/nodes/{node}/failover", Owner: true, Admin: true, h: a.handleFailover},
		{Method: "GET", Pattern: "/api/v1/settings/failover", Owner: true, Admin: true, h: a.handleGetFailoverPolicy},
		{Method: "PUT", Pattern: "/api/v1/settings/failover", Owner: true, Admin: true, h: a.handleSetFailoverPolicy},
		{Method: "GET", Pattern: "/api/v1/settings/self-service", Owner: true, Admin: true, h: a.handleGetSelfServiceQuota},
		{Method: "PUT", Pattern: "/api/v1/settings/self-service", Owner: true, Admin: true, h: a.handleSetSelfServiceQuota},
		{Method: "GET", Pattern: "/api/v1/fleet", Admin: true, h: a.handleFleet},
		{Method: "GET", Pattern: "/api/v1/fleet/usage", Admin: true, h: a.handleFleetUsage},
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
		{Method: "GET", Pattern: "/api/v1/submissions", Admin: true, h: a.handleListSubmissions},
		{Method: "POST", Pattern: "/api/v1/submissions/{id}/approve", Admin: true, h: a.handleApproveSubmission},
		{Method: "POST", Pattern: "/api/v1/submissions/{id}/reject", Admin: true, h: a.handleRejectSubmission},
		{Method: "DELETE", Pattern: "/api/v1/submissions/{id}", Admin: true, h: a.handleDeleteSubmission},
		{Method: "GET", Pattern: "/api/v1/submissions/{id}/context", Admin: true, h: a.handleAdminSubmissionContext},
		{Method: "GET", Pattern: "/api/v1/updates/window", Admin: true, Owner: true, h: a.handleGetUpdateWindow},
		{Method: "PUT", Pattern: "/api/v1/updates/window", Admin: true, Owner: true, h: a.handleSetUpdateWindow},
		{Method: "GET", Pattern: "/api/v1/updates/report", Admin: true, Owner: true, h: a.handleGetUpdateReport},
		{Method: "GET", Pattern: "/api/v1/platform/db-backup", Admin: true, Owner: true, h: a.handleGetDBBackup},

		{Method: "GET", Pattern: "/api/v1/users", Owner: true, h: a.handleListUsers},
		{Method: "POST", Pattern: "/api/v1/users", Owner: true, h: a.handleCreateUser},
		{Method: "GET", Pattern: "/api/v1/users/{id}", Owner: true, h: a.handleGetUser},
		{Method: "PATCH", Pattern: "/api/v1/users/{id}", Owner: true, h: a.handlePatchUser},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}", Owner: true, h: a.handleDeleteUser},
		{Method: "POST", Pattern: "/api/v1/users/{id}/disable", Owner: true, h: a.handleDisableUser},
		{Method: "GET", Pattern: "/api/v1/users/{id}/quotas", Owner: true, h: a.handleGetQuotas},
		{Method: "PUT", Pattern: "/api/v1/users/{id}/quotas", Owner: true, h: a.handleSetQuotas},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}/quotas", Owner: true, h: a.handleClearQuotas},
		{Method: "GET", Pattern: "/api/v1/users/{id}/sessions", Owner: true, h: a.handleListUserSessions},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}/sessions", Owner: true, h: a.handleRevokeUserSessions},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}/sessions/{hash}", Owner: true, h: a.handleRevokeUserSession},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}/passkeys", Owner: true, h: a.handleUnbindUserPasskeys},
		{Method: "DELETE", Pattern: "/api/v1/users/{id}/links/{mc_uuid}", Owner: true, h: a.handleUnlinkAccount},
		{Method: "POST", Pattern: "/api/v1/users/{id}/links", Owner: true, h: a.handleLinkAccount},
	}
}

func (a *API) InternalHandler() http.Handler {
	return a.buildFace("internal", a.internalAPIRoutes(), a.requireInternal)
}

func (a *API) ExternalHandler() http.Handler {
	return a.buildFace("external", a.externalAPIRoutes(), a.requireExternal)
}

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
		// Record the route before authentication so refused requests retain the correct metric label.
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

func (a *API) baseChain(face string, h http.Handler) http.Handler {
	return withRequestID(a.observe(face, withSecurityHeaders(rejectCrossSiteWrites(withBodyDeadline(withRecover(h))))))
}

// writeNoRoute returns JSON 405 with Allow for known paths and JSON 404 otherwise.
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

// requireOnboarded restricts unverified sessions until passkey enrollment; email verification alone does not lift setup lockdown.
func (a *API) requireOnboarded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFromContext(r.Context())
		if p != nil && (p.ViaSession || p.APIKey != "") && !p.EmailVerified {
			creds, err := a.Repo.PasskeyCredentialsForUser(r.Context(), p.UserID)
			if err != nil {
				// Treat store outages as retryable instead of incorrectly requiring passkey enrollment.
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

// setupRequired must match requireOnboarded; email presence cannot identify bind-code players needing setup.
func setupRequired(emailVerified, hasPasskey bool) bool {
	return !emailVerified && !hasPasskey
}

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

func principalFromContext(ctx context.Context) *Principal {
	if v, ok := ctx.Value(ctxKeyPrincipal).(*Principal); ok {
		return v
	}
	return nil
}

func callerFromContext(ctx context.Context) Caller {
	c, _ := ctx.Value(ctxKeyCaller).(Caller)
	return c
}

func internalSource(r *http.Request) string {
	if c := callerFromContext(r.Context()); c != "" {
		return "internal:" + string(c)
	}
	return "internal"
}

// cooldownLimiter is process-local, so replicas have independent cooldown windows.
type cooldownLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	last      map[string]time.Time
	maxWindow time.Duration
	swept     time.Time
}

// noteWindow requires mu and retains entries for the longest observed window.
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

// allowed does not consume cooldown so capacity refusals can retry; nonpositive windows disable throttling.
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

// record is separate from allowed because wake writes are idempotent; the pair is not atomic.
func (c *cooldownLimiter) record(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last[name] = c.now()
}

// reserve checks and records atomically, returning the existing reservation time on refusal; nonpositive windows disable it.
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

// release rolls back a reservation made at reservedAt unless a later reserve superseded it.
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

// streamLimiter bounds concurrent guarded sections per key; a non-positive max disables it.
type streamLimiter struct {
	mu  sync.Mutex
	n   map[string]int
	max int
}

func newStreamLimiter(max int) *streamLimiter {
	return &streamLimiter{n: map[string]int{}, max: max}
}

// acquire returns an idempotent release callback, or nil and false when all slots are held.
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

func (l *streamLimiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[key] <= 1 {
		delete(l.n, key)
		return
	}
	l.n[key]--
}

// withinRunningCap counts desired-Running servers; count-then-flip is non-atomic and may briefly overshoot during bursts.
func (a *API) withinRunningCap(ctx context.Context, info *ServerInfo, cap int) (bool, error) {
	if cap <= 0 || naming.IsSystemServer(info.Name) {
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
	return running < cap, nil
}
