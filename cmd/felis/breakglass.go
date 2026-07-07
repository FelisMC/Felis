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
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/store"

	tea "github.com/charmbracelet/bubbletea"
)

// `felis breakGlass` is the local break-glass emergency console (spec §B). Its
// authority is local root, so it legitimately BYPASSES the web Zero-Trust + Passkey
// path: critical recovery runs direct-to-Postgres. The thin-thread operation it
// ships here is the one that bootstraps everything else — provision (or reset) the
// single Owner account and turn local-password login on — so that even with the web
// auth path unconfigured an operator can get into op.console. It is a genuine
// interactive TUI, NOT a CLI: bare `felis` prints CLI usage, while `felis breakGlass`
// opens this full-screen console. It refuses to run unless euid is 0 (sudo/root).
//
// Root is necessary but NOT sufficient for accountability: root is machine
// authority, not a human identity, so the console additionally captures WHO is
// breaking the glass. When a staff account already exists it asks the operator to
// authenticate as an existing admin (the verified identity is the accountable
// actor); when none exists yet it bootstraps the first Owner from the typed
// credential and attributes the act to the OS user. The audit row records the
// difference. This attribution is best-effort, not tamper-proof — whoever runs
// this is root and can edit Postgres directly — but it produces an honest trail
// for an honest operator, which is the point.
//
// When a staff account already exists the console opens on a thin top-level menu
// (menuModel) so that operations are peers, not tails of one wizard. Two account
// operations are wired today: (1) provision/reset the Owner — the thin thread above,
// which also re-enables local-password login — and (2) add an Operator: an
// insert-only mint of an additional staff admin (provisionOperator) that
// deliberately never touches the global local_auth toggle. On a fresh machine (no
// Owner yet) the menu is skipped: bootstrapping the first Owner is the only sensible
// operation, and adding an Operator first would mint a staff account the login gate
// still rejects. The remaining ops (halt, sync, S3) land in a later phase as further
// menu peers.
//
// An OPTIONAL Cloudflare Tunnel + Access edge (internal/cfsetup) is offered by the
// first-run SETUP flow (runSetupTUI / the connection chooser), not by this
// break-glass menu, though its helpers live in this file. It is kept "锦上添花":
// reachable WITHOUT touching the Owner credential, supported but never required, and
// gated entirely on the operator's own Cloudflare account. The edge flow's verifiable
// logic lives in cfsetup (fail-closed policy, ingress, gating, all unit-tested); what
// this file adds for it is the untested bubbletea shell plus a `tea.ExecProcess`
// suspension for the interactive `cloudflared tunnel login` browser consent.

// breakGlassOverrideToken is the literal an operator must type to proceed when no
// admin credential could be verified. Requiring an explicit, deliberate word (not a
// bare Enter) keeps the unverified root override from happening by reflex.
const breakGlassOverrideToken = "OVERRIDE"

// ownerStore is the minimal repo surface the break-glass / setup console needs.
// *api.PGRepo satisfies it; the unit tests drive a fake, so the core logic
// (recovery, provisioning, accountability audit) is exercised without a
// database or a terminal.
type ownerStore interface {
	// AdminExists reports whether any admin account already exists.
	// It is the bootstrap-vs-recovery switch.
	AdminExists(ctx context.Context) (bool, error)
	// UserByUsername loads a staff login projection.
	UserByUsername(ctx context.Context, username string) (*api.StaffUser, error)
	UpsertOwner(ctx context.Context, id, username, email string) error
	// InsertOperator mints a NEW Operator staff account. Unlike UpsertOwner it is
	// insert-only: a username already taken is a conflict (api.ErrConflict), never a
	// silent reset, so adding an Operator can never clobber the Owner or an existing
	// Operator. The row is role=admin, identical in shape to the Owner — Felis has no
	// separate operator DB role (migration 0003: staff = role=admin).
	InsertOperator(ctx context.Context, id, username, email string) error
	// RedeemLinkCodeForOwner consumes an in-game link code and creates-or-promotes
	// the bound user to role='admin' (Owner). It is the `felis setup` MC-bind path:
	// the operator enters limbo, runs /link, types the code here, and the bound
	// account becomes the passwordless Owner. Unlike RedeemPlayerBindCode it does NOT
	// refuse staff — setup deliberately elevates the bound account.
	RedeemLinkCodeForOwner(ctx context.Context, newUserID, code string, now time.Time) (userID, mcUUID, authSource string, err error)
	// CreateSetupToken mints a one-time setup token for first-web-login bootstrap.
	CreateSetupToken(ctx context.Context, tokenHash, userID string, expiresAt time.Time) error
	SetSetting(ctx context.Context, key string, value []byte) error
	// Audit records the break-glass accountability row.
	Audit(ctx context.Context, e api.AuditEntry) error
}

// cmdBreakGlass is the `felis breakGlass` entrypoint: the root gate, config load,
// database open, accountability detection, and the interactive TUI. Everything
// below runBreakGlassTUI is I/O at the edge; the authentication/provisioning/audit
// logic itself is plain functions over ownerStore so it stays testable off a
// terminal.
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

	// Root gate. The break-glass console's whole authority model is "you already
	// have local root", so anything less is refused rather than degraded. On
	// non-Unix os.Geteuid() returns -1, which also refuses — correct, since this is
	// a Linux-VM recovery tool.
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

	// Decide bootstrap (no admin yet → typed credential mints the first Owner) vs
	// recovery (an admin exists → the operator must authenticate as one) BEFORE the
	// alt-screen TUI takes over, so a database fault surfaces as a plain error.
	adminExists, err := repo.AdminExists(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: detect existing admin: %v\n", err)
		return 1
	}

	res, err := runBreakGlassTUI(ctx, repo, cfg.Database.URL, cfg.Server.RootDomain, cfg.Auth.AdminHostname, cfg.Auth.PanelHostname, cfg.Auth.AccessJWTAud, cfg.K8s.Namespace, accountableOSUser(), adminExists)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: %v\n", err)
		return 1
	}

	if !res.provisioned && !res.edgeConfigured && !res.halted && !res.backedUp {
		fmt.Fprintln(stdout, "felis breakGlass: cancelled — no changes made.")
		return 0
	}

	// The TUI runs on the alternate screen, which is torn down on exit and takes its
	// display with it. Re-print a durable summary to the normal screen so the
	// outcome — and any generated one-time password — survives in scrollback long
	// enough for the operator to log in.
	if res.provisioned {
		if res.isOperator {
			// Adding an Operator does NOT flip local_auth_enabled (performAddOperator),
			// so the summary must not claim it did — only the Owner thread enables login.
			fmt.Fprintf(stdout, "\nfelis breakGlass: Operator account %q provisioned.\n", res.username)
		} else {
			fmt.Fprintf(stdout, "\nfelis breakGlass: Owner account %q provisioned; local-password login is ENABLED.\n", res.username)
		}
		fmt.Fprintf(stdout, "Recorded as %q (mode: %s, os user: %s).\n", res.accountable, res.mode, res.osUser)
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
		// The Cloudflare edge was provisioned. The remaining steps are the operator's
		// own (set the audience, run the tunnel) — we do NOT edit felis.toml for them:
		// editing a config we did not write is a hard-to-reverse change, and the aud is
		// the one value felis-api must adopt to trust the new edge (spec §14).
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
			// login has no fallback (systemservers.go): stopping it takes the whole proxy
			// front door down, so the summary says so explicitly rather than burying it.
			fmt.Fprintf(stdout, "WARNING: %q is a system server — the shared front door is down until it is running again.\n", res.haltServer)
		}
		if res.haltAuditWarning != "" {
			fmt.Fprintf(stdout, "WARNING: the accountability audit row was NOT written: %s\n", res.haltAuditWarning)
		}
		fmt.Fprintf(stdout, "Restart it from the panel, or set the MinecraftServer's spec.desiredState back to Running.\n")
	}

	if res.backedUp {
		// The backup runs through the live felis-api (which audits it), so unlike halt
		// there is no local audit-warning to surface — a resolve/HTTP failure would have
		// come back as an error, not a backedUp result.
		fmt.Fprintf(stdout, "\nfelis breakGlass: backup of %q started (status: %s).\n", res.backupServer, res.backupStatus)
		fmt.Fprintln(stdout, "A one-shot Job writes the archive asynchronously; it appears in the panel's backups list when finished.")
	}
	return 0
}

// accountableOSUser returns the human who escalated to root, best-effort, for the
// audit trail. sudo sets SUDO_USER to the invoking account; a direct root shell
// leaves it empty, in which case we record "root". This is attribution, not proof:
// the environment can be forged, so sudo's own syslog entry — not this value — is
// the tamper-evident record. The root gate is the real authority gate; this only
// answers "which human" for an honest operator.
func accountableOSUser() string {
	if u := strings.TrimSpace(os.Getenv("SUDO_USER")); u != "" {
		return u
	}
	return "root"
}

// newOwnerID returns a fresh, unguessable id for the Owner row. It mirrors the
// crypto/rand hex idiom used elsewhere (internal/submit, internal/build): 16 bytes
// give uuid-equivalent entropy and the value is path/argv-safe lowercase hex. A
// fresh id per run is fine because UpsertOwner preserves the existing id on a
// username conflict, so a reset keeps live sessions valid.
func newOwnerID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand only fails on a broken entropy source. Refuse rather than mint a
		// predictable id for a privileged account.
		return ""
	}
	return "usr-" + hex.EncodeToString(b[:])
}

// authenticateAdmin resolves a typed admin username for recovery-mode attribution.
// Password verification is gone (passwordless design); Phase 3 replaces this with
// email-OTP recovery. For now it confirms the named admin exists.
func authenticateAdmin(ctx context.Context, s ownerStore, username string) (matched string, ok bool, err error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", false, nil
	}
	u, err := s.UserByUsername(ctx, username)
	if errors.Is(err, api.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if u.Role != "admin" {
		return "", false, nil
	}
	return u.Username, true, nil
}

// provisionOwner mints or resets the single Owner account direct-to-Postgres,
// passwordless. The account is role=admin with no password — the Owner completes
// passwordless login setup via the web setup-token flow after `felis setup`.
func provisionOwner(ctx context.Context, s ownerStore, username, email string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("owner username is required")
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

// provisionOperator mints a NEW Operator staff account direct-to-Postgres. Like the
// Owner it is role=admin and passwordless — Felis has no separate operator DB role,
// so an Operator is simply an additional staff admin (migration 0003). UNLIKE
// provisionOwner, which upserts the single Owner and resets it on a username
// conflict, this is insert-only: a username already taken returns api.ErrConflict
// rather than overwriting a live account, so adding an Operator can never silently
// clobber the Owner's or another Operator's account.
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
			// Wrap %w so errors.Is(err, api.ErrConflict) still holds — the TUI can render
			// a "name already taken" message — while keeping a clear human string.
			return fmt.Errorf("operator %q already exists: %w", username, err)
		}
		return fmt.Errorf("write operator: %w", err)
	}
	return nil
}

// enableLocalAuth flips the runtime local_auth_enabled toggle on
// direct-to-Postgres. It is a load-bearing write of break-glass: without it
// handleLogin returns 403 and the freshly provisioned Owner cannot log in, so a
// successful provisionOwner with local auth off is not a usable thin thread.
func enableLocalAuth(ctx context.Context, s ownerStore) error {
	// The setting is read back with json.Unmarshal into a bool, so the stored jsonb
	// value must be the literal true.
	if err := s.SetSetting(ctx, api.LocalAuthEnabledKey, []byte("true")); err != nil {
		return fmt.Errorf("enable local auth: %w", err)
	}
	return nil
}

// breakGlassOp is a fully-resolved operation the TUI hands to the core once the
// operator has been identified (bootstrap) or authenticated (recovery / override).
type breakGlassOp struct {
	mode           string // "bootstrap" | "recovery" | "root_override"
	accountable    string // recorded as the audit actor (verified admin, or OS user)
	osUser         string // $SUDO_USER (or "root"); recorded in the payload
	ownerUsername  string
	ownerEmail     string
	attemptedAdmin string // recovery / override: the admin username the operator typed
}

// breakGlassOutcome is what performBreakGlass reports back to the TUI.
type breakGlassOutcome struct {
	setupTokenURL string // non-empty when setup minted a one-time first-login URL
	auditErr      error  // non-nil if the accountability row could not be written
}

// performBreakGlass executes a resolved break-glass operation: provision (or reset)
// the Owner, enable local-password login, then record a best-effort accountability
// audit row. The Owner is passwordless — the setup-token flow handles first-login
// setup. The audit write is best-effort: a logging failure is reported via auditErr
// but does NOT fail the recovery — break-glass must still work when the audit sink
// is unhappy.
func performBreakGlass(ctx context.Context, s ownerStore, op breakGlassOp) (breakGlassOutcome, error) {
	if err := provisionOwner(ctx, s, op.ownerUsername, op.ownerEmail); err != nil {
		return breakGlassOutcome{}, err
	}
	// Record accountability the instant the account is written — BEFORE enabling
	// local auth, which can still fail. The audit is best-effort, so doing it first
	// never blocks the recovery.
	out := breakGlassOutcome{auditErr: auditBreakGlass(ctx, s, op)}
	if err := enableLocalAuth(ctx, s); err != nil {
		return breakGlassOutcome{}, err
	}
	return out, nil
}

// setupTokenTTL bounds how long a one-time setup URL is valid. The operator opens
// it right after setup completes, so a generous-but-bounded window is enough.
const setupTokenTTL = 30 * time.Minute

// newSetupToken returns a fresh opaque setup token (256 bits, URL-safe) and its
// sha-256 hex hash. Only the hash is persisted; the raw value rides in the URL.
func newSetupToken() (raw, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("generate setup token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(sum[:]), nil
}

// performSetupMCBind is the `felis setup` Owner-establishment path: the operator
// binds their Minecraft account via an in-game /link code, the bound user is
// promoted to role='admin' (passwordless Owner), and a one-time setup URL is
// minted for the first web login where the Owner verifies email / enrolls a
// passkey. adminHostname is the op.console host the URL points at.
func performSetupMCBind(ctx context.Context, s ownerStore, code, adminHostname string) (breakGlassOutcome, error) {
	code = strings.TrimSpace(strings.ToUpper(code))
	if code == "" {
		return breakGlassOutcome{}, errors.New("link code is required")
	}
	newID := newOwnerID()
	if newID == "" {
		return breakGlassOutcome{}, errors.New("generate owner id: entropy source failed")
	}
	userID, _, _, err := s.RedeemLinkCodeForOwner(ctx, newID, code, time.Now())
	if err != nil {
		return breakGlassOutcome{}, fmt.Errorf("bind minecraft account: %w", err)
	}
	raw, hash, err := newSetupToken()
	if err != nil {
		return breakGlassOutcome{}, err
	}
	if err := s.CreateSetupToken(ctx, hash, userID, time.Now().Add(setupTokenTTL)); err != nil {
		return breakGlassOutcome{}, fmt.Errorf("mint setup token: %w", err)
	}
	host := strings.TrimSpace(adminHostname)
	if host == "" {
		host = "op.console.localhost"
	}
	url := "https://" + host + "/setup?token=" + raw
	return breakGlassOutcome{setupTokenURL: url}, nil
}

// auditBreakGlass writes the break-glass accountability row. The actor is the
// resolved human identity (a verified admin in recovery, the OS user otherwise);
// the payload carries the full who/what/how so an after-the-fact reader can tell a
// verified recovery from an unverified root override. It is best-effort — the caller
// does not fail the recovery if this write fails — and intentionally honest: it
// records attribution, it does not prove it (a malicious root can edit the row).
func auditBreakGlass(ctx context.Context, s ownerStore, op breakGlassOp) error {
	payload := map[string]any{
		"mode":     op.mode,
		"owner":    op.ownerUsername,
		"os_user":  op.osUser,
		"verified": op.mode == "recovery",
	}
	if op.attemptedAdmin != "" {
		payload["admin_account"] = op.attemptedAdmin
	}
	blob, err := json.Marshal(payload)
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

// performAddOperator mints a NEW Operator account and records a best-effort
// accountability row. It mirrors performBreakGlass — passwordless — with two
// deliberate differences. (1) It provisions insert-only (provisionOperator), so it
// can never reset an existing account the way the Owner upsert does. (2) It does NOT
// touch local_auth_enabled: adding an Operator presupposes an already-configured,
// running system (an admin is present to authorize it), so flipping the global auth
// gate as a side effect of "add a user" would be surprising — that toggle belongs to
// the Owner break-glass thread alone. The audit is best-effort and written only after
// a successful provision; a conflict mints nothing, so there is nothing to attribute.
func performAddOperator(ctx context.Context, s ownerStore, op breakGlassOp) (breakGlassOutcome, error) {
	if err := provisionOperator(ctx, s, op.ownerUsername, op.ownerEmail); err != nil {
		return breakGlassOutcome{}, err
	}
	out := breakGlassOutcome{auditErr: auditAddOperator(ctx, s, op)}
	return out, nil
}

// auditAddOperator writes the operator-creation accountability row. It mirrors
// auditBreakGlass — same actor/source/payload shape, so one reader can tell a
// verified (recovery) add from an unverified (root_override) one — under the distinct
// break_glass.operator_create action, naming the new account under an "operator" key
// rather than "owner".
func auditAddOperator(ctx context.Context, s ownerStore, op breakGlassOp) error {
	payload := map[string]any{
		"mode":     op.mode,
		"operator": op.ownerUsername,
		"os_user":  op.osUser,
		"verified": op.mode == "recovery",
	}
	if op.attemptedAdmin != "" {
		payload["admin_account"] = op.attemptedAdmin
	}
	blob, err := json.Marshal(payload)
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

// breakGlassResult is what the TUI hands back to cmdBreakGlass for the durable
// post-exit summary. provisioned is false on cancel.
type breakGlassResult struct {
	provisioned   bool
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

	// connection outcome (independent of provisioned)
	connectMethod     connectMethod
	connectConfigured bool
	panelHostname     string
	reverseProxyGuide string

	// storage backend outcome
	storageMethod storageMethod
	storageDetail string

	// halt outcome (break-glass "halt a server" op #31)
	halted             bool
	haltServer         string
	haltNamespace      string
	haltAlreadyStopped bool
	haltSystemServer   bool
	haltAuditWarning   string

	// backup outcome (break-glass "back up a world now / Sync" op #31 §B4)
	backedUp     bool
	backupServer string
	backupStatus string

	// Cloudflare-specific edge detail (set only when connectMethod is Cloudflare)
	edgeConfigured    bool
	edgeAud           string
	edgeRoutedHosts   []string
	edgeConfigPath    string
	edgePanelHostname string
	edgeAdminHostname string
}

type consoleMode string

const (
	consoleModeBreakGlass consoleMode = "breakGlass"
	consoleModeSetup      consoleMode = "setup"
)

const (
	defaultTunnelName       = "felis"
	defaultTunnelConfigPath = "/etc/felis/cloudflared.yml"

	// Cloudflare Dashboard prefill URL for the API token this flow actually uses:
	// Access app/policy management. Tunnel creation and DNS routing are authorized by
	// the operator's cloudflared browser login, not by this token.
	cloudflareAccessTokenTemplateURL = "https://dash.cloudflare.com/profile/api-tokens?permissionGroupKeys=%5B%7B%22key%22%3A%22access%22%2C%22type%22%3A%22edit%22%7D%5D&accountId=*&zoneId=all"
	cloudflareAPITokenDocsURL        = "https://developers.cloudflare.com/fundamentals/api/how-to/account-owned-token-template/"
)

func runBreakGlassTUI(ctx context.Context, s ownerStore, dbURL, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser string, adminExists bool) (breakGlassResult, error) {
	return runConsoleTUI(ctx, s, dbURL, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser, adminExists, consoleModeBreakGlass)
}

func runSetupTUI(ctx context.Context, s ownerStore, dbURL, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser string, adminExists bool) (breakGlassResult, error) {
	return runConsoleTUI(ctx, s, dbURL, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser, adminExists, consoleModeSetup)
}

func runConsoleTUI(ctx context.Context, s ownerStore, dbURL, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser string, adminExists bool, mode consoleMode) (breakGlassResult, error) {
	rm := newRootModel(ctx, s, dbURL, rootDomain, adminHostname, panelHostname, accessAud, namespace, osUser, adminExists, mode)
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
