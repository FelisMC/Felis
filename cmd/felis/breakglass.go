package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mailsettings"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/store"

	tea "github.com/charmbracelet/bubbletea"
)

// felis breakGlass: root-only local recovery console that talks directly to Postgres.

// breakGlassOverrideToken is typed to proceed when no admin was verified by a mailed code.
const breakGlassOverrideToken = "OVERRIDE"

// ownerStore is the repo surface the break-glass and setup consoles need.
type ownerStore interface {
	AdminExists(ctx context.Context) (bool, error)
	UserByUsername(ctx context.Context, username string) (*api.StaffUser, error)
	// OwnerUsername returns the active Owner seat, or "" when none exists.
	OwnerUsername(ctx context.Context) (string, error)
	UpsertOwner(ctx context.Context, id, username, email string) error
	// InsertOperator is insert-only: a taken username returns api.ErrConflict instead of resetting.
	InsertOperator(ctx context.Context, id, username, email string) error
	// CompleteOwnerSetup creates or resumes the first panel login atomically.
	CompleteOwnerSetup(ctx context.Context, newUserID string, now time.Time,
		tokenHash string, tokenExpiresAt time.Time) (userID, username string, err error)
	SetSetting(ctx context.Context, key string, value []byte) error
	// Audit records the break-glass accountability row.
	Audit(ctx context.Context, e api.AuditEntry) error
}

func cmdBreakGlass(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("breakGlass", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// Local root is the only authority; os.Geteuid() is -1 on non-Unix, which also refuses.
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis breakGlass: refused — the break-glass emergency console must run as root (try: sudo felis breakGlass)")
		return 1
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: %v\n", err)
		return 1
	}

	ctx := context.Background()
	drv, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	repo := api.NewPGRepo(drv.DB())

	// Checked before the alt-screen TUI starts so a database fault prints as a plain error.
	adminExists, err := repo.AdminExists(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: detect existing admin: %v\n", err)
		return 1
	}

	host, _ := os.Hostname()
	recovery := recoveryConfig{open: panelFirstRecoveryMailer(func(ctx context.Context) (*mailsettings.Settings, error) {
		raw, err := repo.GetSetting(ctx, mailsettings.Key)
		if errors.Is(err, api.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return mailsettings.Parse(raw)
	}, hostRecoveryMailer(cfg.SMTP, hostSMTPPasswordPath, platform.DefaultControlNamespace)), host: host}

	res, err := runBreakGlassTUI(ctx, repo, cfg.Database, cfg.Server.RootDomain, cfg.Auth.AdminHostname, cfg.Auth.PanelHostname, cfg.Auth.AccessJWTAud, cfg.K8s.Namespace, accountableOSUser(), adminExists, recovery)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: %v\n", err)
		return 1
	}

	if !res.provisioned && !res.edgeConfigured && !res.halted && !res.backedUp {
		fmt.Fprintln(stdout, "felis breakGlass: cancelled — no changes made.")
		return 0
	}

	// The alt screen vanishes on exit; reprint the outcome and setup URL to the normal screen.
	if res.provisioned {
		if res.isOperator {
			// Adding an Operator does not enable local auth.
			fmt.Fprintf(stdout, "\nfelis breakGlass: Operator account %q provisioned.\n", res.username)
		} else {
			fmt.Fprintf(stdout, "\nfelis breakGlass: Owner account %q provisioned; local session sign-in is ENABLED.\n", res.username)
		}
		fmt.Fprintf(stdout, "Recorded as %q (mode: %s, os user: %s).\n", res.accountable, res.mode, res.osUser)
		if res.mode == "root_override" {
			fmt.Fprintln(stdout, "No admin was proven by an email code; the audit row records this run as an unverified root override and why.")
		}
		if res.setupTokenURL != "" {
			fmt.Fprintf(stdout, "One-time setup URL (opens a lockdown session to verify email / enroll passkey):\n\n    %s\n\n", res.setupTokenURL)
		}
		if res.auditWarning != "" {
			fmt.Fprintf(stdout, "WARNING: the accountability audit row was NOT written: %s\n", res.auditWarning)
		}
		if url := adminLoginURL(res.rootDomain, res.adminHostname); url != "" {
			fmt.Fprintf(stdout, "Admin console: %s\n", url)
		}
	}

	if res.edgeConfigured {
		// felis.toml is left alone; the operator sets the Access audience and runs the tunnel.
		fmt.Fprintf(stdout, "\nfelis breakGlass: Cloudflare Tunnel + Access edge configured.\n")
		if len(res.edgeRoutedHosts) > 0 {
			fmt.Fprintf(stdout, "Routed web hostnames: %s\n", strings.Join(res.edgeRoutedHosts, ", "))
		}
		if res.edgeConfigPath != "" {
			fmt.Fprintf(stdout, "Wrote tunnel config: %s\n", res.edgeConfigPath)
		}
		fmt.Fprintf(stdout, "\nACTION REQUIRED — make felis-api trust the edge:\n")
		fmt.Fprintf(stdout, "  in %s under [auth], set: access_jwt_aud = %q\n", *cfgPath, res.edgeAud)
		fmt.Fprintln(stdout, "Then start the tunnel:  cloudflared tunnel run")
		fmt.Fprintln(stdout, "Verify the Access app actually guards the admin face before relying on it.")
	}

	if res.halted {
		verb := "is now stopping"
		if res.haltAlreadyStopped {
			verb = "was already stopped"
		}
		fmt.Fprintf(stdout, "\nfelis breakGlass: server %q %s (namespace %s).\n", res.haltServer, verb, res.haltNamespace)
		if res.haltSystemServer {
			// A system server has no fallback: stopping it takes the proxy front door down.
			fmt.Fprintf(stdout, "WARNING: %q is a system server — the shared front door is down until it is running again.\n", res.haltServer)
		}
		if res.haltAuditWarning != "" {
			fmt.Fprintf(stdout, "WARNING: the accountability audit row was NOT written: %s\n", res.haltAuditWarning)
		}
		fmt.Fprintf(stdout, "Restart it from the panel, or set the MinecraftServer's spec.desiredState back to Running.\n")
	}

	if res.backedUp {
		fmt.Fprintf(stdout, "\nfelis breakGlass: backup of %q started (status: %s).\n", res.backupServer, res.backupStatus)
		fmt.Fprintln(stdout, "A one-shot Job writes the archive asynchronously; it appears in the panel's backups list when finished.")
	}
	return 0
}

// accountableOSUser returns $SUDO_USER, or "root" for a direct root shell; attribution only, the environment can be forged.
func accountableOSUser() string {
	if u := strings.TrimSpace(os.Getenv("SUDO_USER")); u != "" {
		return u
	}
	return "root"
}

// newOwnerID returns 16 random bytes as hex; UpsertOwner keeps the existing id on a username conflict.
func newOwnerID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Never mint a predictable id for a privileged account.
		return ""
	}
	return "usr-" + hex.EncodeToString(b[:])
}

// provisionOwner mints or resets the passwordless Owner. An occupied seat must be named (ownerSeatTakenError),
// otherwise the upsert would mint a second owner that the panel cannot delete.
func provisionOwner(ctx context.Context, s ownerStore, username, email string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("owner username is required")
	}
	if seat, err := s.OwnerUsername(ctx); err != nil {
		return fmt.Errorf("check the owner seat: %w", err)
	} else if seat != "" && seat != username {
		return &ownerSeatTakenError{seat: seat}
	}
	id := newOwnerID()
	if id == "" {
		return errors.New("generate owner id: entropy source failed")
	}
	if err := s.UpsertOwner(ctx, id, username, strings.TrimSpace(email)); err != nil {
		return fmt.Errorf("write owner: %w", err)
	}
	return nil
}

// ownerSeatTakenError matches api.ErrConflict so the TUI re-prompts instead of exiting.
type ownerSeatTakenError struct{ seat string }

func (e *ownerSeatTakenError) Error() string {
	return fmt.Sprintf("an Owner already exists as %q — enter that username to reset the Owner", e.seat)
}

func (e *ownerSeatTakenError) Is(target error) bool { return target == api.ErrConflict }

// provisionOperator inserts a passwordless role=admin account; a taken username returns api.ErrConflict.
func provisionOperator(ctx context.Context, s ownerStore, username, email string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("operator username is required")
	}
	id := newOwnerID()
	if id == "" {
		return errors.New("generate operator id: entropy source failed")
	}
	if err := s.InsertOperator(ctx, id, username, strings.TrimSpace(email)); err != nil {
		if errors.Is(err, api.ErrConflict) {
			return fmt.Errorf("operator %q already exists: %w", username, err)
		}
		return fmt.Errorf("write operator: %w", err)
	}
	return nil
}

// enableLocalAuth turns local sign-in on; while off, every session-minting endpoint returns 403 local_auth_disabled.
func enableLocalAuth(ctx context.Context, s ownerStore) error {
	if err := s.SetSetting(ctx, api.LocalAuthEnabledKey, []byte("true")); err != nil {
		return fmt.Errorf("enable local auth: %w", err)
	}
	return nil
}

type breakGlassOp struct {
	mode           string // "bootstrap" | "recovery" | "root_override"
	accountable    string // recorded as the audit actor (verified admin, or OS user)
	osUser         string // $SUDO_USER (or "root"); recorded in the payload
	ownerUsername  string
	ownerEmail     string
	attemptedAdmin string // recovery / override: the admin username the operator typed
	verifiedBy     string // recovery: how the admin was proven (verifiedByEmailOTP)
	codeSentTo     string // recovery: the address the proving code went to
	otpSkipped     string // root_override: why no code proved an admin (otpSkip*)
	otpSkipDetail  string // root_override: what failed, when something did
}

type breakGlassOutcome struct {
	setupTokenURL string // non-empty when setup minted a one-time first-login URL
	ownerUsername string // panel Owner created or resumed by setup
	auditErr      error  // non-nil if the accountability row could not be written
}

// performBreakGlass provisions the Owner, enables local sign-in and audits best-effort: an audit failure surfaces as auditErr, not an error.
func performBreakGlass(ctx context.Context, s ownerStore, op breakGlassOp) (breakGlassOutcome, error) {
	if err := provisionOwner(ctx, s, op.ownerUsername, op.ownerEmail); err != nil {
		return breakGlassOutcome{}, err
	}
	// Audit before enableLocalAuth, which can still fail.
	out := breakGlassOutcome{auditErr: auditBreakGlass(ctx, s, op)}
	if err := enableLocalAuth(ctx, s); err != nil {
		return breakGlassOutcome{}, err
	}
	return out, nil
}

const setupTokenTTL = 30 * time.Minute // one-time setup URL lifetime

// newSetupToken returns a 256-bit URL-safe token and its sha-256 hex hash; only the hash is stored.
func newSetupToken() (raw, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("generate setup token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(sum[:]), nil
}

func performSetupOwner(ctx context.Context, s ownerStore, panelURL, osUser string) (breakGlassOutcome, error) {
	base, err := url.Parse(strings.TrimRight(panelURL, "/"))
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return breakGlassOutcome{}, errors.New("a configured HTTPS operator console is required")
	}
	newID := newOwnerID()
	if newID == "" {
		return breakGlassOutcome{}, errors.New("generate owner id: entropy source failed")
	}
	raw, hash, err := newSetupToken()
	if err != nil {
		return breakGlassOutcome{}, err
	}
	now := time.Now()
	userID, username, err := s.CompleteOwnerSetup(ctx, newID, now, hash, now.Add(setupTokenTTL))
	out := breakGlassOutcome{ownerUsername: username}
	if err != nil {
		return out, err
	}
	base.Path = "/setup"
	base.RawQuery = url.Values{"token": {raw}}.Encode()
	out.setupTokenURL = base.String()
	blob, _ := json.Marshal(map[string]string{"os_user": osUser, "user_id": userID, "username": username})
	out.auditErr = s.Audit(ctx, api.AuditEntry{
		Actor: osUser, Source: "setup", Action: "setup.owner_login", Payload: blob,
	})
	return out, nil
}

// auditBreakGlass records the accountability row: the actor is the verified admin in recovery, else the OS user.
func auditBreakGlass(ctx context.Context, s ownerStore, op breakGlassOp) error {
	blob, err := json.Marshal(breakGlassPayload(op, "owner"))
	if err != nil {
		return err
	}
	return s.Audit(ctx, api.AuditEntry{
		Actor:   op.accountable,
		Source:  "break-glass",
		Action:  "break_glass." + op.mode,
		Payload: blob,
	})
}

// breakGlassPayload is the who/how both account audits carry; verified is true only when a mailed code proved the admin.
func breakGlassPayload(op breakGlassOp, subjectKey string) map[string]any {
	payload := map[string]any{
		"mode":     op.mode,
		subjectKey: op.ownerUsername,
		"os_user":  op.osUser,
		"verified": op.verifiedBy != "",
	}
	if op.attemptedAdmin != "" {
		payload["admin_account"] = op.attemptedAdmin
	}
	if op.verifiedBy != "" {
		payload["verified_by"] = op.verifiedBy
		payload["code_sent_to"] = op.codeSentTo
	}
	if op.otpSkipped != "" {
		payload["otp_skipped"] = op.otpSkipped
		if op.otpSkipDetail != "" {
			payload["otp_skip_detail"] = op.otpSkipDetail
		}
	}
	return payload
}

// performAddOperator inserts an Operator and audits after success; unlike the Owner thread it leaves local_auth_enabled untouched.
func performAddOperator(ctx context.Context, s ownerStore, op breakGlassOp) (breakGlassOutcome, error) {
	if err := provisionOperator(ctx, s, op.ownerUsername, op.ownerEmail); err != nil {
		return breakGlassOutcome{}, err
	}
	out := breakGlassOutcome{auditErr: auditAddOperator(ctx, s, op)}
	return out, nil
}

// auditAddOperator mirrors auditBreakGlass under break_glass.operator_create, naming the account under "operator".
func auditAddOperator(ctx context.Context, s ownerStore, op breakGlassOp) error {
	blob, err := json.Marshal(breakGlassPayload(op, "operator"))
	if err != nil {
		return err
	}
	return s.Audit(ctx, api.AuditEntry{
		Actor:   op.accountable,
		Source:  "break-glass",
		Action:  "break_glass.operator_create",
		Payload: blob,
	})
}

// breakGlassResult is the TUI outcome printed after exit; provisioned is false on cancel.
type breakGlassResult struct {
	provisioned   bool
	alreadySetUp  bool // setup found an Owner and changed nothing; distinguishes a re-run from a cancel
	isOperator    bool // an Operator was added rather than the Owner provisioned
	mode          string
	accountable   string
	osUser        string
	username      string
	setupTokenURL string // non-empty when setup minted a one-time first-login URL
	auditWarning  string
	rootDomain    string
	adminHostname string
	panelURL      string

	connectMethod     connectMethod
	connectConfigured bool
	panelHostname     string
	reverseProxyGuide string

	storageMethod storageMethod
	storageDetail string

	halted             bool
	haltServer         string
	haltNamespace      string
	haltAlreadyStopped bool
	haltSystemServer   bool
	haltAuditWarning   string

	backedUp     bool
	backupServer string
	backupStatus string

	edgeConfigured  bool
	edgeAud         string
	edgeRoutedHosts []string
	edgeConfigPath  string
}

type consoleMode string

const (
	consoleModeBreakGlass consoleMode = "breakGlass"
	consoleModeSetup      consoleMode = "setup"
)

const (
	defaultTunnelName       = "felis"
	defaultTunnelConfigPath = "/etc/felis/cloudflared.yml"

	// Access app/policy token template; tunnel and DNS rights come from the cloudflared browser login.
	cloudflareAccessTokenTemplateURL = "https://dash.cloudflare.com/profile/api-tokens?permissionGroupKeys=%5B%7B%22key%22%3A%22access%22%2C%22type%22%3A%22edit%22%7D%5D&accountId=*&zoneId=all"
	cloudflareAPITokenDocsURL        = "https://developers.cloudflare.com/fundamentals/api/how-to/account-owned-token-template/"
)

func runBreakGlassTUI(ctx context.Context, s ownerStore, db config.DatabaseConfig, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser string, adminExists bool, recovery recoveryConfig) (breakGlassResult, error) {
	return runConsoleTUI(ctx, s, db, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser, adminExists, consoleModeBreakGlass, recovery)
}

func runSetupTUI(ctx context.Context, s ownerStore, db config.DatabaseConfig, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser string, adminExists bool) (breakGlassResult, error) {
	return runConsoleRoot(newConsoleRoot(ctx, s, db, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser, adminExists, consoleModeSetup, recoveryConfig{}))
}

// newConsoleRoot builds the console root model; the summary reads this host's alert route.
func newConsoleRoot(ctx context.Context, s ownerStore, db config.DatabaseConfig, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser string, adminExists bool, mode consoleMode, recovery recoveryConfig) *rootModel {
	rm := newRootModel(ctx, s, db, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser, adminExists, mode)
	rm.recovery = recovery
	rm.alertRoute = func(ctx context.Context) alertRoute {
		return hostAlertRoute(ctx, hostSetupConfigPath, db.URL, defaultHeartbeatFile)
	}
	return rm
}

func runConsoleTUI(ctx context.Context, s ownerStore, db config.DatabaseConfig, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser string, adminExists bool, mode consoleMode, recovery recoveryConfig) (breakGlassResult, error) {
	return runConsoleRoot(newConsoleRoot(ctx, s, db, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser, adminExists, mode, recovery))
}

func runConsoleRoot(rm *rootModel) (breakGlassResult, error) {
	final, err := tea.NewProgram(rm, tea.WithAltScreen()).Run()
	if err != nil {
		return breakGlassResult{}, err
	}
	root, ok := final.(*rootModel)
	if !ok {
		return breakGlassResult{}, errors.New("unexpected final model type")
	}
	if root.err != nil {
		return breakGlassResult{}, root.err
	}
	return root.result, nil
}

func defaultPanelHostname(rootDomain, configured string) string {
	if h := strings.TrimSpace(configured); h != "" {
		return h
	}
	if rootDomain != "" {
		return "console." + rootDomain
	}
	return ""
}

func defaultAdminHostname(rootDomain, configured string) string {
	if h := strings.TrimSpace(configured); h != "" {
		return h
	}
	if rootDomain != "" {
		return "op.console." + rootDomain
	}
	return ""
}

func normalizeEdgeHostname(s string) string {
	return strings.Trim(strings.TrimSpace(s), ".")
}

func validateEdgeHostname(label, host string, required bool) error {
	if host == "" {
		if required {
			return fmt.Errorf("%s is required", label)
		}
		return nil
	}
	if strings.Contains(host, "://") || strings.ContainsAny(host, "/\\ \t\r\n") {
		return fmt.Errorf("%s must be a hostname, not a URL", label)
	}
	if strings.Contains(host, ":") {
		return fmt.Errorf("%s must not include a port", label)
	}
	if strings.HasPrefix(host, ".") {
		return fmt.Errorf("%s must not start with a dot", label)
	}
	return nil
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func adminLoginURL(rootDomain, adminHostname string) string {
	if h := strings.TrimSpace(adminHostname); h != "" {
		return "https://" + h
	}
	if rootDomain != "" {
		return "https://op.console." + rootDomain
	}
	return ""
}
