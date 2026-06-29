package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/store"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/crypto/bcrypt"
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
// The console opens on a thin top-level router (stepMenu) so that operations
// are peers, not tails of one wizard. Two are wired today: (1) provision/reset
// the Owner — the thin thread above — and (2) an OPTIONAL Cloudflare Tunnel +
// Access edge (internal/cfsetup), kept "锦上添花": it is reachable WITHOUT touching
// the Owner credential, supported but never required, and gated entirely on the
// operator's own Cloudflare account. The remaining ops (halt, sync, S3) land in a
// later phase as further menu peers. The edge flow's verifiable logic lives in
// cfsetup (fail-closed policy, ingress, gating, all unit-tested); what this file
// adds for it is the untested bubbletea shell plus a `tea.ExecProcess` suspension
// for the interactive `cloudflared tunnel login` browser consent.

// breakGlassOverrideToken is the literal an operator must type to proceed when no
// admin credential could be verified. Requiring an explicit, deliberate word (not a
// bare Enter) keeps the unverified root override from happening by reflex.
const breakGlassOverrideToken = "OVERRIDE"

// bootstrapPasswordAlphabet excludes visually ambiguous glyphs (0/O, 1/I/l) so a
// human can transcribe a generated one-time password off a terminal without error.
const bootstrapPasswordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// ownerStore is the minimal repo surface the break-glass console needs.
// *api.PGRepo satisfies it; the unit tests drive a fake, so the core logic
// (authentication, provisioning, accountability audit) is exercised without a
// database or a terminal.
type ownerStore interface {
	// AdminExists reports whether any authenticatable staff account already exists.
	// It is the bootstrap-vs-recovery switch.
	AdminExists(ctx context.Context) (bool, error)
	// UserByUsername loads a staff login projection for credential verification.
	UserByUsername(ctx context.Context, username string) (*api.StaffUser, error)
	UpsertOwner(ctx context.Context, id, username, email, passwordHash string, mustChange bool) error
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

	res, err := runBreakGlassTUI(ctx, repo, cfg.Database.URL, cfg.Server.RootDomain, cfg.Auth.AdminHostname, cfg.Auth.PanelHostname, cfg.Auth.AccessJWTAud, accountableOSUser(), adminExists)
	if err != nil {
		fmt.Fprintf(stderr, "felis breakGlass: %v\n", err)
		return 1
	}

	if !res.provisioned && !res.edgeConfigured {
		fmt.Fprintln(stdout, "felis breakGlass: cancelled — no changes made.")
		return 0
	}

	// The TUI runs on the alternate screen, which is torn down on exit and takes its
	// display with it. Re-print a durable summary to the normal screen so the
	// outcome — and any generated one-time password — survives in scrollback long
	// enough for the operator to log in.
	if res.provisioned {
		fmt.Fprintf(stdout, "\nfelis breakGlass: Owner account %q provisioned; local-password login is ENABLED.\n", res.username)
		fmt.Fprintf(stdout, "Recorded as %q (mode: %s, os user: %s).\n", res.accountable, res.mode, res.osUser)
		if res.displayPassword != "" {
			// A one-time password was generated (recovery / root override). It is shown,
			// never persisted: only the bcrypt hash reached the database.
			fmt.Fprintf(stdout, "One-time password (you MUST change it on first login):\n\n    %s\n\n", res.displayPassword)
		} else {
			// Bootstrap: the operator typed the password themselves, so we do NOT echo it
			// back into scrollback.
			fmt.Fprintln(stdout, "Log in with the password you just entered (you MUST change it on first login).")
		}
		if res.auditWarning != "" {
			fmt.Fprintf(stdout, "WARNING: the accountability audit row was NOT written: %s\n", res.auditWarning)
		}
		if url := adminLoginURL(res.rootDomain, res.adminHostname); url != "" {
			fmt.Fprintf(stdout, "Log in at %s with that username and password.\n", url)
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

// generateBootstrapPassword returns a fresh one-time password from the unambiguous
// alphabet. It rejection-samples to avoid modulo bias, so every position is uniform
// over the alphabet. 20 chars over a 57-symbol alphabet is ~116 bits — far more than
// the must-change credential needs, and it is rotated on first login regardless.
func generateBootstrapPassword() (string, error) {
	const n = 20
	// Largest multiple of the alphabet size that fits in a byte; bytes at or above it
	// are discarded so the surviving values map uniformly (no modulo bias).
	limit := byte(256 - (256 % len(bootstrapPasswordAlphabet)))
	out := make([]byte, 0, n)
	var b [1]byte
	for len(out) < n {
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("generate bootstrap password: %w", err)
		}
		if b[0] >= limit {
			continue
		}
		out = append(out, bootstrapPasswordAlphabet[int(b[0])%len(bootstrapPasswordAlphabet)])
	}
	return string(out), nil
}

// validateOwnerPassword mirrors api.validateNewPassword (handlers_auth.go): a
// break-glass credential must satisfy the SAME 8–72-byte rule the panel's own
// change-password enforces, so an operator can never set a password here that the
// web change-password flow would later reject. 72 is bcrypt's hard input limit.
func validateOwnerPassword(pw string) error {
	if len(pw) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	if len(pw) > 72 {
		return errors.New("password must be at most 72 bytes")
	}
	return nil
}

// authenticateAdmin verifies a typed credential against an existing admin account
// for recovery-mode attribution. matched is the stored username on success.
//
// ok==false with err==nil is NOT a failure to surface — it means the credential did
// not match any admin password. The caller offers an explicit root override instead
// of refusing, because break-glass must still recover when no admin credential can
// be produced (a forgotten password is the canonical reason the web login is
// unreachable in the first place). Only a real datastore fault returns err.
func authenticateAdmin(ctx context.Context, s ownerStore, username, password string) (matched string, ok bool, err error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", false, nil
	}
	u, err := s.UserByUsername(ctx, username)
	if errors.Is(err, api.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// Only an admin row carrying a bcrypt hash is an authenticatable staff identity;
	// a player row (role=user, hash NULL → empty PasswordHash) can never attribute a
	// break-glass action.
	if u.Role != "admin" || u.PasswordHash == "" {
		return "", false, nil
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", false, nil
	}
	return u.Username, true, nil
}

// provisionOwner mints or resets the single Owner account direct-to-Postgres with
// the given (already-validated-by-the-caller) password. The account is created with
// must_change_password=true, which is load-bearing: it is what arms the API's
// lockdown middleware so the Owner can do nothing but change the password on first
// login. Only the bcrypt hash reaches the database; the plaintext never does.
func provisionOwner(ctx context.Context, s ownerStore, username, email, password string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("owner username is required")
	}
	if err := validateOwnerPassword(password); err != nil {
		return err
	}
	id := newOwnerID()
	if id == "" {
		return errors.New("generate owner id: entropy source failed")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash owner password: %w", err)
	}
	if err := s.UpsertOwner(ctx, id, username, strings.TrimSpace(email), string(hash), true); err != nil {
		return fmt.Errorf("write owner: %w", err)
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
	ownerPassword  string // typed (bootstrap); "" => generate a one-time password
	attemptedAdmin string // recovery / override: the admin username the operator typed
}

// breakGlassOutcome is what performBreakGlass reports back to the TUI.
type breakGlassOutcome struct {
	displayPassword string // non-empty only when a one-time password was generated
	auditErr        error  // non-nil if the accountability row could not be written
}

// performBreakGlass executes a resolved break-glass operation: provision (or reset)
// the Owner, enable local-password login, then record a best-effort accountability
// audit row. A typed ownerPassword (bootstrap) is used as-is; an empty one (recovery
// / root override) is replaced with a generated one-time password returned for
// one-time display. The audit write is best-effort: a logging failure is reported
// via auditErr but does NOT fail the recovery — break-glass must still work when the
// audit sink is unhappy.
func performBreakGlass(ctx context.Context, s ownerStore, op breakGlassOp) (breakGlassOutcome, error) {
	password := op.ownerPassword
	generated := false
	if password == "" {
		p, err := generateBootstrapPassword()
		if err != nil {
			return breakGlassOutcome{}, err
		}
		password, generated = p, true
	}
	if err := provisionOwner(ctx, s, op.ownerUsername, op.ownerEmail, password); err != nil {
		return breakGlassOutcome{}, err
	}
	// Record accountability the instant the credential changes — BEFORE enabling
	// local auth, which can still fail. Auditing only after both writes would let a
	// failed enableLocalAuth leave a just-reset credential with no "who did it" row;
	// the audit is best-effort, so doing it first never blocks the recovery.
	out := breakGlassOutcome{auditErr: auditBreakGlass(ctx, s, op)}
	if generated {
		out.displayPassword = password
	}
	if err := enableLocalAuth(ctx, s); err != nil {
		return breakGlassOutcome{}, err
	}
	return out, nil
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

// breakGlassResult is what the TUI hands back to cmdBreakGlass for the durable
// post-exit summary. provisioned is false on cancel.
type breakGlassResult struct {
	provisioned     bool
	mode            string
	accountable     string
	osUser          string
	username        string
	displayPassword string // empty when the operator typed their own bootstrap password
	auditWarning    string
	rootDomain      string
	adminHostname   string
	panelURL        string

	// connection outcome (independent of provisioned)
	connectMethod     connectMethod
	connectConfigured bool
	panelHostname     string
	reverseProxyGuide string

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

func runBreakGlassTUI(ctx context.Context, s ownerStore, dbURL, rootDomain, adminHostname, panelHostname, accessAud, osUser string, adminExists bool) (breakGlassResult, error) {
	return runConsoleTUI(ctx, s, dbURL, rootDomain, adminHostname, panelHostname, accessAud, osUser, adminExists, consoleModeBreakGlass)
}

func runSetupTUI(ctx context.Context, s ownerStore, dbURL, rootDomain, adminHostname, panelHostname, accessAud, osUser string, adminExists bool) (breakGlassResult, error) {
	return runConsoleTUI(ctx, s, dbURL, rootDomain, adminHostname, panelHostname, accessAud, osUser, adminExists, consoleModeSetup)
}

func runConsoleTUI(ctx context.Context, s ownerStore, dbURL, rootDomain, adminHostname, panelHostname, accessAud, osUser string, adminExists bool, mode consoleMode) (breakGlassResult, error) {
	rm := newRootModel(ctx, s, dbURL, rootDomain, adminHostname, panelHostname, accessAud, osUser, adminExists, mode)
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
