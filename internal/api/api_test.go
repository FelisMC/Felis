package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"github.com/golang-jwt/jwt/v5"
)

const testRoot = "mc.example.net" // neutral; never a deployment domain

// ---- fakes ----

type fakeRepo struct {
	bySub     map[string]*ServerRecord
	byName    map[string]*ServerRecord
	linked    map[string]bool
	quota     map[string]bool
	allowlist map[string]map[string]bool // name -> user_id -> on list (web face)
	// allowUUID mirrors the UUID-keyed server_allowlist table itself, the gate the
	// internal/velocity wake uses (name -> mc_uuid -> on list). allowlist above is
	// the account_links-bridged web view of the same data.
	allowUUID map[string]map[string]bool
	mine      map[string][]MyServerView
	// owners mirrors the ServerOwners join (name -> owner display identity); only
	// claimed servers appear. ownersErr forces the lookup to fail so a test can
	// prove the fleet read degrades to owner-less rows rather than 500ing.
	owners    map[string]string
	ownersErr error
	claimOK   map[string]bool // name -> claim succeeds; absent name -> ErrNotFound
	// claimQuotaRefuse simulates ClaimServer's atomic quota gate (audit #4)
	// refusing a name whose advisory pre-check already passed.
	claimQuotaRefuse map[string]bool
	// serverResources / resourceUpdates mirror the cached resource columns:
	// ServerResources is what the resize path reads (to preserve storage), and
	// UpdateServerResources records the write for assertions.
	serverResources map[string]ResourceSpec
	resourceUpdates map[string]ResourceSpec
	audits          []AuditEntry
	joins           []string
	// create-server seeding (spec §15)
	seeded  map[string]bool   // name -> servers row exists
	aliases map[string]string // subdomain -> bound server name
	seedErr error
	// account linking (spec §10)
	linkCodes map[string]fakeLinkCode // code -> pending binding
	links     map[string]string       // mc_uuid -> user_id (mirrors UNIQUE(mc_uuid))
	// linkAuthSource mirrors account_links.auth_source (mc_uuid -> mojang|thirdparty),
	// the value copied from the consumed code at verify (migration 0005).
	linkAuthSource map[string]string
	// world backups (spec §7, §22). A nil slice lists empty.
	backups []fakeBackup
	// session auth (spec §B, passwordless). staff is keyed by username (the login key);
	// sessions by token_hash; settings by key. They mirror the PG contract so the
	// hermetic tests exercise the same fail-closed semantics the integration impl
	// honors.
	staff    map[string]*StaffUser   // username -> staff login row
	sessions map[string]*fakeSession // token_hash -> session
	settings map[string][]byte       // key -> jsonb value
	// failSessionUser / failGetSetting force those reads to fail with a generic
	// (non-ErrNotFound) error, simulating a store outage for the 503 auth path.
	failSessionUser error
	failGetSetting  error
	// player email OTPs (spec §B2). Keyed by row id; the verify path scans for the
	// newest live (user, purpose) just as the PG query does.
	otps map[string]*fakeEmailOTP
	// op-login requests (spec §B op-login). opLogins mirrors op_login_requests keyed
	// by id; the in-game approve/finish paths mutate status/consumed in place, and
	// tests plant rows directly to drive the status/finish/pending-list paths.
	opLogins map[string]*fakeOpLogin
	// setup tokens (spec §B setup)
	setupTokens map[string]fakeSetupToken
	// username-collision reclaim (spec §B3). blacklist mirrors username_blacklist
	// (mc_uuid -> barred), holds mirrors player_data_holds keyed by the held
	// (squatter) mc_uuid — both keyed by UUID, matching the PG UNIQUE(mc_uuid)
	// idempotency. They are written together by ReclaimUsername so the fake encodes
	// the same all-or-nothing contract the PG transaction enforces.
	blacklist map[string]bool
	holds     map[string]fakeDataHold
	// player passkey enrollment (spec §14 / Phase 6). passkeyCreds is keyed by row id
	// and mirrors webauthn_credentials (the credential_id UNIQUE guard is enforced in
	// CreatePasskeyCredential); passkeyChallenges is keyed by row id and mirrors
	// webauthn_challenges, so the consume path scans the newest live (user, purpose)
	// just as the PG query does.
	passkeyCreds      map[string]PasskeyCredential
	passkeyChallenges map[string]*fakePasskeyChallenge
	// discoverable ("usernameless") login challenge store (task #40), keyed by opaque handle id
	// with no user key, mirroring migration 0013. discoverableFull forces the capped-out path
	// (ErrTooManyDiscoverableChallenges) so the begin 429 branch is reachable without inserting
	// thousands of rows.
	discoverableChallenges map[string]*fakeDiscoverableChallenge
	discoverableFull       bool
	// user admin fakes
	seededUsers []seededUser
	fakeQuotas  map[string]*QuotaView
	// pingErr, when non-nil, is returned by Ping to simulate DB liveness check
	// failures in /readyz tests.
	pingErr error
	// account migration (spec §B3 inherit, scenario A) keyed by row id, mirroring
	// account_migrations. Server ownership for the transfer is read/written on
	// byName[*].OwnerID (the servers projection), and the source's retirement is
	// applied to seededUsers, so the fake exercises the same all-or-nothing shape the
	// PG transaction enforces.
	migrations map[string]*fakeMigration
}

// fakePasskeyChallenge mirrors a webauthn_challenges row: its owner and purpose, the
// opaque stashed SessionData, single-use via consumed, and createdAt to order the
// newest-live lookup the consume path performs.
type fakePasskeyChallenge struct {
	id          string
	userID      string
	purpose     string
	sessionData []byte
	expiresAt   time.Time
	consumed    bool
	createdAt   time.Time
}

// fakeDiscoverableChallenge mirrors a webauthn_discoverable_challenges row (task #40): no user
// or purpose (a from-zero begin has neither), just the opaque stashed SessionData, its expiry,
// and single-use via consumed. Keyed by the opaque handle in the map, like the real table's id.
type fakeDiscoverableChallenge struct {
	sessionData []byte
	expiresAt   time.Time
	consumed    bool
}

// fakeDataHold mirrors a player_data_holds row at the granularity the verifiable
// (write-only) layer exercises: which name/data was stashed for the squatter UUID
// and when the 30-day window ends. reclaimed_by_user_id/reclaimed_at have no fake
// fields — the inherit flow that would set them is CODE-ONLY (deferred).
type fakeDataHold struct {
	id        string
	username  string
	dataRef   string
	expiresAt time.Time
}

// fakeEmailOTP mirrors an email_otps row: only the code hash is held (never the
// digits), attempts caps brute force, consumed marks single-use, and createdAt
// orders the newest-live lookup.
type fakeEmailOTP struct {
	id        string
	userID    string
	email     string
	codeHash  string
	purpose   string
	attempts  int
	expiresAt time.Time
	consumed  bool
	createdAt time.Time
}

// fakeSession mirrors a sessions row: its owner, its expiry, and whether it has
// been revoked.
type fakeSession struct {
	userID    string
	expiresAt time.Time
	revoked   bool
}

// fakeBackup mirrors a world_backups row: the client-facing view plus the
// server-side backup_ref the list queries never expose.
type fakeBackup struct {
	view BackupView
	ref  string
}

// fakeLinkCode mirrors an account_link_codes row.
type fakeLinkCode struct {
	mcUUID     string
	authSource string
	expiresAt  time.Time
}

// fakeOpLogin mirrors an op_login_requests row (spec §B op-login) at the granularity
// the verifiable layer exercises: status ('pending'|'approved'|'denied') is the
// projection of (approved_at, denied_at) the handler's status/finish gates read,
// consumed mirrors consumed_at (the single-use guard), and createdAt orders the
// pending list oldest-first (the PG ORDER BY created_at).
type fakeOpLogin struct {
	id        string
	userID    string
	email     string
	status    string
	consumed  bool
	expiresAt time.Time
	createdAt time.Time
}

// fakeSetupToken mirrors a setup_tokens row (spec §B setup): a one-time
// lockdown-enrollment token. ConsumedAt is zero until the /setup flow redeems it.
type fakeSetupToken struct {
	TokenHash, UserID string
	ExpiresAt         time.Time
	ConsumedAt        time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		bySub: map[string]*ServerRecord{}, byName: map[string]*ServerRecord{},
		linked: map[string]bool{}, quota: map[string]bool{},
		allowlist: map[string]map[string]bool{}, allowUUID: map[string]map[string]bool{},
		mine:    map[string][]MyServerView{},
		owners:  map[string]string{},
		claimOK: map[string]bool{}, claimQuotaRefuse: map[string]bool{},
		serverResources: map[string]ResourceSpec{}, resourceUpdates: map[string]ResourceSpec{},
		seeded: map[string]bool{}, aliases: map[string]string{},
		linkCodes: map[string]fakeLinkCode{}, links: map[string]string{},
		linkAuthSource:    map[string]string{},
		staff:             map[string]*StaffUser{},
		sessions:          map[string]*fakeSession{},
		settings:          map[string][]byte{},
		otps:              map[string]*fakeEmailOTP{},
		opLogins:          map[string]*fakeOpLogin{},
		setupTokens:       map[string]fakeSetupToken{},
		blacklist:         map[string]bool{},
		holds:             map[string]fakeDataHold{},
		passkeyCreds:      map[string]PasskeyCredential{},
		passkeyChallenges: map[string]*fakePasskeyChallenge{},

		discoverableChallenges: map[string]*fakeDiscoverableChallenge{},
		fakeQuotas:             map[string]*QuotaView{},
		migrations:             map[string]*fakeMigration{},
	}
}

func (f *fakeRepo) ServerBySubdomain(_ context.Context, s string) (*ServerRecord, error) {
	if r, ok := f.bySub[s]; ok {
		return r, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) ServerByName(_ context.Context, n string) (*ServerRecord, error) {
	if r, ok := f.byName[n]; ok {
		return r, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) IsLinked(_ context.Context, u string) (bool, error)       { return f.linked[u], nil }
func (f *fakeRepo) QuotaAvailable(_ context.Context, u string) (bool, error) { return f.quota[u], nil }

func (f *fakeRepo) QuotaCheck(_ context.Context, userID string, _ string, _ ResourceSpec) (bool, error) {
	// For hermetic tests, QuotaCheck delegates to the same QuotaAvailable
	// store — tests that care about per-dimension checks should use
	// fakeQuotas with direct inspection.
	return f.QuotaAvailable(context.TODO(), userID)
}

func (f *fakeRepo) UpdateServerResources(_ context.Context, name string, cpu, mem, stor int) error {
	f.resourceUpdates[name] = ResourceSpec{CPUMilli: cpu, MemoryMB: mem, StorageMB: stor}
	return nil
}

func (f *fakeRepo) ServerResources(_ context.Context, name string) (ResourceSpec, error) {
	return f.serverResources[name], nil
}
func (f *fakeRepo) CreateLinkCode(_ context.Context, code, mcUUID, authSource string, expiresAt time.Time) error {
	f.linkCodes[code] = fakeLinkCode{mcUUID: mcUUID, authSource: authSource, expiresAt: expiresAt}
	return nil
}

// VerifyLinkCode mirrors PGRepo.VerifyLinkCode exactly so the hermetic tests
// exercise the same contract the integration impl honors: strict expiry against
// the passed clock, a different-user UUID → ErrConflict WITHOUT consuming the
// code, same (user, uuid) idempotent, the code's auth_source copied onto the link
// (and refreshed on re-verify), and the code consumed only on success.
func (f *fakeRepo) VerifyLinkCode(_ context.Context, userID, code string, now time.Time) (string, string, error) {
	rec, ok := f.linkCodes[code]
	if !ok || !rec.expiresAt.After(now) {
		return "", "", ErrLinkCodeInvalid
	}
	if existing, ok := f.links[rec.mcUUID]; ok && existing != userID {
		return "", "", ErrConflict // do not consume another user's pending code
	}
	f.links[rec.mcUUID] = userID
	f.linkAuthSource[rec.mcUUID] = rec.authSource // copy/refresh, mirrors DO UPDATE
	f.linked[userID] = true
	delete(f.linkCodes, code)
	return rec.mcUUID, rec.authSource, nil
}

// RedeemPlayerBindCode mirrors PGRepo.RedeemPlayerBindCode: it create-or-fetches a
// player keyed on the code's verified mc_uuid. The staff map stands in for the single
// users table, so a newly created role='user' player is stored there (uuid-derived
// username) and resolves through SessionUser/UserByID just like the PG JOIN. An
// already-linked admin UUID is refused without consuming the code; an already-linked
// player is fetched idempotently.
//
// Contract gap vs PG (benign): on an orphan link (mc_uuid linked but its users row
// gone) the fake resolves no role and falls through to the idempotent return, minting
// a session for a ghost id, whereas PG's account_links⋈users JOIN would find no row,
// take the insert branch and 500 on the UNIQUE(mc_uuid) clash. The account_links.user_id
// FK makes an orphan link unreachable in production, so this divergence is untestable
// rather than a real behavioral difference.
func (f *fakeRepo) RedeemPlayerBindCode(_ context.Context, newUserID, code string, now time.Time) (string, string, string, error) {
	rec, ok := f.linkCodes[code]
	if !ok || !rec.expiresAt.After(now) {
		return "", "", "", ErrLinkCodeInvalid
	}
	if existing, ok := f.links[rec.mcUUID]; ok {
		for _, u := range f.staff { // resolve the linked identity to check its role
			if u.ID == existing && u.Role != "user" {
				return "", "", "", ErrPlayerBindForbidden // staff must use op.console; do not consume
			}
		}
		delete(f.linkCodes, code)
		return existing, rec.mcUUID, rec.authSource, nil
	}
	f.staff[rec.mcUUID] = &StaffUser{ID: newUserID, Username: rec.mcUUID, Role: "user"}
	f.links[rec.mcUUID] = newUserID
	f.linkAuthSource[rec.mcUUID] = rec.authSource
	f.linked[newUserID] = true
	delete(f.linkCodes, code)
	return newUserID, rec.mcUUID, rec.authSource, nil
}

// CreateEmailOTP / VerifyEmailOTP mirror PGRepo's contract so the hermetic tests
// exercise the same semantics the integration impl honors: a fresh code supersedes
// the prior live one for (user, purpose), expiry and the attempt cap are checked
// before the hash compare, a wrong guess costs an attempt without consuming the
// code, and a match consumes it and flips the user row verified.
func (f *fakeRepo) CreateEmailOTP(_ context.Context, id, userID, email, codeHash, purpose string, expiresAt time.Time) error {
	for k, o := range f.otps { // supersede any prior live code (DELETE ... consumed_at IS NULL)
		if o.userID == userID && o.purpose == purpose && !o.consumed {
			delete(f.otps, k)
		}
	}
	f.otps[id] = &fakeEmailOTP{
		id: id, userID: userID, email: email, codeHash: codeHash, purpose: purpose,
		expiresAt: expiresAt, createdAt: expiresAt, // createdAt proxy: constant TTL ⇒ later expiry == later creation
	}
	return nil
}
func (f *fakeRepo) VerifyEmailOTP(_ context.Context, userID, purpose, codeHash string, now time.Time) (string, error) {
	var live *fakeEmailOTP
	for _, o := range f.otps { // newest live (user, purpose)
		if o.userID != userID || o.purpose != purpose || o.consumed {
			continue
		}
		if live == nil || o.createdAt.After(live.createdAt) {
			live = o
		}
	}
	if live == nil {
		return "", ErrOTPInvalid
	}
	if !live.expiresAt.After(now) {
		return "", ErrOTPInvalid
	}
	if live.attempts >= otpMaxAttempts {
		return "", ErrOTPLocked
	}
	if live.codeHash != codeHash {
		live.attempts++ // a typo costs an attempt but does not consume the code
		return "", ErrOTPInvalid
	}
	// A DIFFERENT verified holder of the same address → ErrEmailTaken, code left
	// live — mirrors PGRepo's guard + the users_verified_email_unique index.
	for _, u := range f.staff {
		if u.ID != userID && u.EmailVerified && strings.EqualFold(u.Email, live.email) {
			return "", ErrEmailTaken
		}
	}
	live.consumed = true
	for _, u := range f.staff { // flip the user row verified (UPDATE users ...)
		if u.ID == userID {
			u.Email = live.email
			u.EmailVerified = true
		}
	}
	return live.email, nil
}
func (f *fakeRepo) SetUserEmail(_ context.Context, userID, email string) error {
	for _, u := range f.staff { // record + clear verified (proves nothing) — mirrors PGRepo
		if u.ID == userID {
			u.Email = email
			u.EmailVerified = false
			return nil
		}
	}
	return ErrNotFound
}

// CreatePasskeyChallenge / ConsumePasskeyChallengeByUser mirror PGRepo's contract so
// the hermetic tests exercise the same semantics: a fresh begin supersedes ALL prior
// rows for (user, purpose) — live, consumed, or expired — so the table holds at most one
// row per (user, purpose), and the consume path redeems the newest live one (expiry
// checked before consuming), single-use.
func (f *fakeRepo) CreatePasskeyChallenge(_ context.Context, id, userID, purpose string, sessionData []byte, expiresAt time.Time) error {
	for k, c := range f.passkeyChallenges { // supersede all prior (DELETE ... user_id=$1 AND purpose=$2)
		if c.userID == userID && c.purpose == purpose {
			delete(f.passkeyChallenges, k)
		}
	}
	f.passkeyChallenges[id] = &fakePasskeyChallenge{
		id: id, userID: userID, purpose: purpose, sessionData: sessionData,
		expiresAt: expiresAt, createdAt: expiresAt, // createdAt proxy: constant TTL ⇒ later expiry == later creation
	}
	return nil
}
func (f *fakeRepo) ConsumePasskeyChallengeByUser(_ context.Context, userID, purpose string, now time.Time) ([]byte, error) {
	var live *fakePasskeyChallenge
	for _, c := range f.passkeyChallenges { // newest live (user, purpose)
		if c.userID != userID || c.purpose != purpose || c.consumed {
			continue
		}
		if live == nil || c.createdAt.After(live.createdAt) {
			live = c
		}
	}
	if live == nil || !live.expiresAt.After(now) {
		return nil, ErrPasskeyChallengeInvalid
	}
	live.consumed = true
	return live.sessionData, nil
}

// CreateDiscoverableChallenge / ConsumeDiscoverableChallenge mirror PGRepo's non-user-keyed
// contract (task #40): begin reaps expired/consumed rows then stashes under the opaque handle,
// and consume redeems by handle, single-use, expiry checked. discoverableFull forces the capped
// path so the begin 429 branch is reachable without inserting thousands of rows.
func (f *fakeRepo) CreateDiscoverableChallenge(_ context.Context, id string, sessionData []byte, now, expiresAt time.Time) error {
	if f.discoverableFull {
		return ErrTooManyDiscoverableChallenges
	}
	for k, c := range f.discoverableChallenges { // reap (DELETE ... expires_at<=now OR consumed_at NOT NULL)
		if c.consumed || !c.expiresAt.After(now) {
			delete(f.discoverableChallenges, k)
		}
	}
	f.discoverableChallenges[id] = &fakeDiscoverableChallenge{sessionData: sessionData, expiresAt: expiresAt}
	return nil
}
func (f *fakeRepo) ConsumeDiscoverableChallenge(_ context.Context, id string, now time.Time) ([]byte, error) {
	c, ok := f.discoverableChallenges[id]
	if !ok || c.consumed || !c.expiresAt.After(now) {
		return nil, ErrPasskeyChallengeInvalid
	}
	c.consumed = true
	return c.sessionData, nil
}

// CreatePasskeyCredential mirrors PGRepo: a credential_id already bound to ANY account
// → ErrConflict (the UNIQUE guard), never a silent rebind.
func (f *fakeRepo) CreatePasskeyCredential(_ context.Context, c PasskeyCredential) error {
	for _, ex := range f.passkeyCreds {
		if ex.CredentialID == c.CredentialID {
			return ErrConflict
		}
	}
	f.passkeyCreds[c.ID] = c
	return nil
}

// PasskeyCredentialsForUser mirrors PGRepo: the user's own passkeys, newest first.
// CreatedAt orders the list; id is a deterministic tie-break for the frozen test clock
// (the PG ORDER BY is created_at DESC; same-instant rows are simply stable here).
func (f *fakeRepo) PasskeyCredentialsForUser(_ context.Context, userID string) ([]PasskeyCredential, error) {
	var out []PasskeyCredential
	for _, c := range f.passkeyCreds {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// DeletePasskeyCredential mirrors PGRepo: scoped to userID so a caller can only unbind
// their OWN credential; no matching (user, id) row → ErrNotFound.
func (f *fakeRepo) DeletePasskeyCredential(_ context.Context, userID, id string) error {
	if c, ok := f.passkeyCreds[id]; ok && c.UserID == userID {
		delete(f.passkeyCreds, id)
		return nil
	}
	return ErrNotFound
}

// DeleteAllPasskeyCredentialsForUser mirrors PGRepo: unbind every passkey the user holds,
// and removing zero is a successful no-op (never ErrNotFound).
func (f *fakeRepo) DeleteAllPasskeyCredentialsForUser(_ context.Context, userID string) error {
	for id, c := range f.passkeyCreds {
		if c.UserID == userID {
			delete(f.passkeyCreds, id)
		}
	}
	return nil
}

// AdvanceCredentialSignCount mirrors PGRepo: find the passkey by its UNIQUE credential_id, set
// the stored counter to newSignCount, and stamp last_used_at. A credential_id matching no row
// is a successful no-op (the PG UPDATE touches zero rows), so a test can assert both the
// advance-on-success and the never-error-on-missing contracts.
func (f *fakeRepo) AdvanceCredentialSignCount(_ context.Context, credentialID string, newSignCount uint32, usedAt time.Time) error {
	for id, c := range f.passkeyCreds {
		if c.CredentialID == credentialID {
			c.SignCount = newSignCount
			t := usedAt
			c.LastUsedAt = &t
			f.passkeyCreds[id] = c
			return nil
		}
	}
	return nil
}

// fakePasskeyVerifier is the hermetic PasskeyVerifier: it performs no real attestation
// or assertion crypto, so it exercises the enrollment AND login STATE MACHINES (challenge
// persistence, consume, conflict, audit, session mint) without go-webauthn.
// BeginRegistration/BeginLogin return a fixed options blob and an opaque session marker;
// FinishRegistration returns the credential the test preloaded and FinishLogin the
// assertion it preloaded, or a forced error when failErr is set (to drive the finish 400
// path). beginLoginErr drives BeginLogin's own failure branch — a user with no assertable
// credential — which the login-begin handler maps to passkey_login_failed.
type fakePasskeyVerifier struct {
	options       json.RawMessage
	credential    VerifiedCredential
	assertion     VerifiedAssertion
	failErr       error
	beginLoginErr error
	// lastUser/lastSession capture what the handler passed, so a test can assert the
	// stashed SessionData round-trips and the existing credentials reach the verifier.
	lastUser    PasskeyUser
	lastSession []byte
	// discoverableUserHandle is the userHandle the fake feeds to FinishDiscoverableLogin's
	// resolver, so a handler test drives the userHandle → UserByID → session-mint wiring for a
	// chosen account (or an unknown handle, to exercise the resolve-fails branch).
	discoverableUserHandle []byte
}

func (v *fakePasskeyVerifier) BeginRegistration(user PasskeyUser) (json.RawMessage, []byte, error) {
	v.lastUser = user
	opts := v.options
	if opts == nil {
		opts = json.RawMessage(`{"publicKey":{"challenge":"ZmFrZQ"}}`)
	}
	return opts, []byte("session:" + user.ID), nil
}

func (v *fakePasskeyVerifier) FinishRegistration(user PasskeyUser, sessionData []byte, _ io.Reader) (VerifiedCredential, error) {
	v.lastUser = user
	v.lastSession = sessionData
	if v.failErr != nil {
		return VerifiedCredential{}, v.failErr
	}
	return v.credential, nil
}

func (v *fakePasskeyVerifier) BeginLogin(user PasskeyUser) (json.RawMessage, []byte, error) {
	v.lastUser = user
	if v.beginLoginErr != nil {
		return nil, nil, v.beginLoginErr
	}
	opts := v.options
	if opts == nil {
		opts = json.RawMessage(`{"publicKey":{"challenge":"YXNzZXJ0"}}`)
	}
	return opts, []byte("login-session:" + user.ID), nil
}

func (v *fakePasskeyVerifier) FinishLogin(user PasskeyUser, sessionData []byte, _ io.Reader) (VerifiedAssertion, error) {
	v.lastUser = user
	v.lastSession = sessionData
	if v.failErr != nil {
		return VerifiedAssertion{}, v.failErr
	}
	return v.assertion, nil
}

func (v *fakePasskeyVerifier) BeginDiscoverableLogin() (json.RawMessage, []byte, error) {
	if v.beginLoginErr != nil {
		return nil, nil, v.beginLoginErr
	}
	opts := v.options
	if opts == nil {
		opts = json.RawMessage(`{"publicKey":{"challenge":"ZGlzYw"}}`)
	}
	return opts, []byte("disc-session"), nil
}

func (v *fakePasskeyVerifier) FinishDiscoverableLogin(resolveUser func([]byte) (PasskeyUser, error), sessionData []byte, _ io.Reader) (VerifiedAssertion, error) {
	v.lastSession = sessionData
	if v.failErr != nil {
		return VerifiedAssertion{}, v.failErr
	}
	// Drive the resolver with the configured user handle so the handler's userHandle → UserByID
	// → session-mint wiring runs end to end; a resolve error (unknown handle) fails the ceremony
	// exactly as the real ValidateDiscoverableLogin would when the handler cannot be resolved.
	u, err := resolveUser(v.discoverableUserHandle)
	if err != nil {
		return VerifiedAssertion{}, err
	}
	v.lastUser = u
	return v.assertion, nil
}

func (f *fakeRepo) UserInAllowlist(_ context.Context, n, u string) (bool, error) {
	return f.allowlist[n][u], nil
}
func (f *fakeRepo) UUIDInAllowlist(_ context.Context, n, uuid string) (bool, error) {
	return f.allowUUID[n][uuid], nil
}
func (f *fakeRepo) UserByMCUUID(_ context.Context, uuid string) (string, error) {
	if u, ok := f.links[uuid]; ok {
		return u, nil
	}
	return "", ErrNotFound
}

// ReclaimUsername mirrors PGRepo.ReclaimUsername: it bars the squatter UUID and
// stashes the data hold together (the all-or-nothing PG transaction), keyed by
// mc_uuid so a repeat reclaim of an already-barred UUID is an idempotent no-op
// (ON CONFLICT (mc_uuid) DO NOTHING on both tables) — the first reclaim wins and
// a duplicate neither errors nor overwrites the stored hold.
func (f *fakeRepo) ReclaimUsername(_ context.Context, id, squatterUUID, username, dataRef string, expiresAt time.Time) (time.Time, error) {
	if h, ok := f.holds[squatterUUID]; ok { // already stashed — idempotent no-op; keep & report the first window
		return h.expiresAt, nil
	}
	f.blacklist[squatterUUID] = true
	f.holds[squatterUUID] = fakeDataHold{id: id, username: username, dataRef: dataRef, expiresAt: expiresAt}
	return expiresAt, nil
}
func (f *fakeRepo) IsUsernameBlacklisted(_ context.Context, mcUUID string) (bool, error) {
	return f.blacklist[mcUUID], nil
}

// IsProtectedAdminLink mirrors PGRepo's JOIN of account_links to users: linked,
// auth_source 'thirdparty', and the linked user staff (admin OR owner) — no
// password-hash test, so an SSO Operator or the Owner is protected like any other.
func (f *fakeRepo) IsProtectedAdminLink(_ context.Context, mcUUID string) (bool, error) {
	userID, ok := f.links[mcUUID]
	if !ok || f.linkAuthSource[mcUUID] != authSourceThirdParty {
		return false, nil
	}
	for _, u := range f.staff {
		if u.ID == userID && staffRole(u.Role) {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeRepo) ClaimServer(_ context.Context, n, u string) (bool, error) {
	ok, present := f.claimOK[n]
	if !present {
		return false, ErrNotFound
	}
	if ok && f.claimQuotaRefuse[n] {
		return false, ErrQuotaExceeded // mirrors the atomic gate losing the race
	}
	return ok, nil
}
func (f *fakeRepo) RecordJoin(_ context.Context, n, uuid string) error {
	if _, ok := f.byName[n]; !ok {
		return ErrNotFound
	}
	f.joins = append(f.joins, n+":"+uuid)
	return nil
}
func (f *fakeRepo) MyServers(_ context.Context, u string) ([]MyServerView, error) {
	return f.mine[u], nil
}
func (f *fakeRepo) ServerOwners(_ context.Context) (map[string]string, error) {
	if f.ownersErr != nil {
		return nil, f.ownersErr
	}
	return f.owners, nil
}
func (f *fakeRepo) SeedServer(_ context.Context, name, subdomain string, _, _, _ int) error {
	if f.seedErr != nil {
		return f.seedErr
	}
	if bound, ok := f.aliases[subdomain]; ok && bound != name {
		return ErrConflict
	}
	f.seeded[name] = true
	f.aliases[subdomain] = name
	return nil
}
func (f *fakeRepo) Ping(_ context.Context) error { return f.pingErr }
func (f *fakeRepo) Audit(_ context.Context, e AuditEntry) error {
	f.audits = append(f.audits, e)
	return nil
}

// AllBackups / BackupsForUser / LatestBackup mirror the PG queries' contract so
// the hermetic tests can't pass against a too-lenient fake: only status='present'
// rows are visible, the user scope is the former_owner column, and LatestBackup
// is the newest present row for a server (or ErrNotFound).
func (f *fakeRepo) AllBackups(_ context.Context) ([]BackupView, error) {
	var out []BackupView
	for _, b := range f.backups {
		if b.view.Status == "present" {
			out = append(out, b.view)
		}
	}
	return out, nil
}
func (f *fakeRepo) BackupsForUser(_ context.Context, userID string) ([]BackupView, error) {
	var out []BackupView
	for _, b := range f.backups {
		if b.view.Status == "present" && b.view.FormerOwner == userID {
			out = append(out, b.view)
		}
	}
	return out, nil
}
func (f *fakeRepo) LatestBackup(_ context.Context, serverName string) (*BackupRecord, error) {
	var latest *fakeBackup
	for i := range f.backups {
		b := &f.backups[i]
		if b.view.Status != "present" || b.view.ServerName != serverName {
			continue
		}
		if latest == nil || b.view.CreatedAt.After(latest.view.CreatedAt) {
			latest = b
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	return &BackupRecord{
		ID: latest.view.ID, ServerName: latest.view.ServerName,
		FormerOwner: latest.view.FormerOwner, BackupRef: latest.ref,
		SizeBytes: latest.view.SizeBytes,
	}, nil
}

func (f *fakeRepo) BackupByID(_ context.Context, id string) (*BackupRecord, error) {
	for i := range f.backups {
		b := &f.backups[i]
		if b.view.Status == "present" && b.view.ID == id {
			return &BackupRecord{
				ID: b.view.ID, ServerName: b.view.ServerName,
				FormerOwner: b.view.FormerOwner, BackupRef: b.ref,
				SizeBytes: b.view.SizeBytes,
			}, nil
		}
	}
	return nil, ErrNotFound
}

// ---- staff / session auth fakes (spec §B, passwordless) ----
// Each method mirrors the PGRepo contract: a returned StaffUser is copied so a
// test cannot mutate the stored row by reference, SessionUser re-reads the
// CURRENT staff row (so a role change or a deleted account takes effect on live
// sessions just as the PG JOIN does), and the settings/sessions semantics match.

func (f *fakeRepo) UserByUsername(_ context.Context, username string) (*StaffUser, error) {
	if u, ok := f.staff[username]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) UserByID(_ context.Context, id string) (*StaffUser, error) {
	for _, u := range f.staff {
		if u.ID == id {
			cp := *u
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) UpsertOwner(_ context.Context, id, username, email string) error {
	// Mirror PG ON CONFLICT (username): preserve the existing id so live sessions
	// survive a re-bootstrap.
	if existing, ok := f.staff[username]; ok {
		id = existing.ID
	}
	f.staff[username] = &StaffUser{
		ID: id, Username: username, Email: email, Role: "admin",
	}
	return nil
}
func (f *fakeRepo) CreateSession(_ context.Context, tokenHash, userID string, expiresAt time.Time) error {
	f.sessions[tokenHash] = &fakeSession{userID: userID, expiresAt: expiresAt}
	return nil
}
func (f *fakeRepo) SessionUser(_ context.Context, tokenHash string, now time.Time) (*SessionedUser, error) {
	if f.failSessionUser != nil {
		return nil, f.failSessionUser
	}
	s, ok := f.sessions[tokenHash]
	if !ok || s.revoked || !s.expiresAt.After(now) {
		return nil, ErrNotFound
	}
	for _, u := range f.staff {
		if u.ID == s.userID {
			return &SessionedUser{
				ID: u.ID, Email: u.Email, Role: u.Role,
			}, nil
		}
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) RevokeSession(_ context.Context, tokenHash string) error {
	if s, ok := f.sessions[tokenHash]; ok {
		s.revoked = true
	}
	return nil
}
func (f *fakeRepo) GetSetting(_ context.Context, key string) ([]byte, error) {
	if f.failGetSetting != nil {
		return nil, f.failGetSetting
	}
	if v, ok := f.settings[key]; ok {
		return v, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) SetSetting(_ context.Context, key string, value []byte) error {
	f.settings[key] = value
	return nil
}

// ---- user admin fakes ----

// seededUser is a test-only user row held in the fake repo.
type seededUser struct {
	view   UserView
	detail UserDetail
}

func (f *fakeRepo) seedUser(u UserView) {
	su := &StaffUser{ID: u.ID, Username: u.Username, Email: u.Email, Role: u.Role}
	f.staff[u.Username] = su
	f.seededUsers = append(f.seededUsers, seededUser{view: u, detail: UserDetail{UserView: u}})
}

func (f *fakeRepo) ListUsers(_ context.Context, opts ListUsersOpts) ([]UserView, int, error) {
	var filtered []UserView
	for _, su := range f.seededUsers {
		u := su.view
		if opts.Query != "" {
			q := strings.ToLower(opts.Query)
			ul := strings.ToLower(u.Username)
			el := strings.ToLower(u.Email)
			if !strings.Contains(ul, q) && !strings.Contains(el, q) {
				continue
			}
		}
		if opts.Role != "" && u.Role != opts.Role {
			continue
		}
		switch opts.Hidden {
		case "true":
			if !u.Disabled {
				continue
			}
		case "false":
			if u.Disabled {
				continue
			}
		}
		filtered = append(filtered, u)
	}
	total := len(filtered)
	limit := opts.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= len(filtered) {
		return []UserView{}, total, nil
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	// Newest first — the PG orders by created_at DESC too.
	for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	return filtered[offset:end], total, nil
}

func (f *fakeRepo) UserDetail(_ context.Context, userID string) (*UserDetail, error) {
	for _, su := range f.seededUsers {
		if su.view.ID == userID {
			return &su.detail, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeRepo) CreateUser(_ context.Context, input CreateUserInput, _ string) (*UserView, error) {
	for _, su := range f.seededUsers {
		if su.view.Username == input.Username {
			return nil, ErrConflict
		}
	}
	id := "test-" + input.Username
	u := UserView{
		ID: id, Username: input.Username, Email: input.Email,
		Role:      input.Role,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	d := UserDetail{UserView: u}
	f.seededUsers = append(f.seededUsers, seededUser{view: u, detail: d})
	f.staff[input.Username] = &StaffUser{ID: u.ID, Username: u.Username, Email: u.Email, Role: u.Role}
	return &u, nil
}

func (f *fakeRepo) UpdateUser(_ context.Context, userID string, patch UpdateUserInput, _ string) (*UserView, error) {
	for i, su := range f.seededUsers {
		if su.view.ID != userID {
			continue
		}
		if patch.Username != nil {
			for _, other := range f.seededUsers {
				if other.view.ID != userID && other.view.Username == *patch.Username {
					return nil, ErrConflict
				}
			}
			f.seededUsers[i].view.Username = *patch.Username
			f.seededUsers[i].detail.Username = *patch.Username
		}
		if patch.Email != nil {
			// Changing the address voids the proof of it, exactly like PGRepo:
			// only VerifyEmailOTP may assert a verified address.
			if *patch.Email != f.seededUsers[i].view.Email {
				f.seededUsers[i].view.EmailVerified = false
				f.seededUsers[i].detail.EmailVerified = false
			}
			f.seededUsers[i].view.Email = *patch.Email
			f.seededUsers[i].detail.Email = *patch.Email
		}
		if patch.Role != nil {
			f.seededUsers[i].view.Role = *patch.Role
			f.seededUsers[i].detail.Role = *patch.Role
		}
		v := f.seededUsers[i].view
		return &v, nil
	}
	return nil, ErrNotFound
}

func (f *fakeRepo) DeleteUser(_ context.Context, userID, _ string) error {
	for i, su := range f.seededUsers {
		if su.view.ID == userID {
			f.seededUsers = append(f.seededUsers[:i], f.seededUsers[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeRepo) SetUserDisabled(_ context.Context, userID string, disabled bool) error {
	for i, su := range f.seededUsers {
		if su.view.ID == userID {
			f.seededUsers[i].view.Disabled = disabled
			f.seededUsers[i].detail.Disabled = disabled
			return nil
		}
	}
	return ErrNotFound
}

// ---- account migration fakes (spec §B3 inherit, scenario A) ----

// fakeMigration mirrors an account_migrations row through its state machine. Zero
// times mean the corresponding NULL column (not yet confirmed / no code issued).
type fakeMigration struct {
	id            string
	sourceUserID  string
	targetUserID  string
	state         string
	confirmFactor string
	confirmedAt   time.Time
	codeHash      string
	codeExpiresAt time.Time
	redeemedAt    time.Time
	createdAt     time.Time
}

func (m *fakeMigration) view() *MigrationView {
	v := &MigrationView{
		ID: m.id, SourceUserID: m.sourceUserID, TargetUserID: m.targetUserID,
		State: m.state, ConfirmFactor: m.confirmFactor, CreatedAt: m.createdAt,
	}
	if !m.confirmedAt.IsZero() {
		t := m.confirmedAt
		v.ConfirmedAt = &t
	}
	if !m.codeExpiresAt.IsZero() {
		t := m.codeExpiresAt
		v.CodeExpiresAt = &t
	}
	return v
}

// userLive mirrors the PG "id = $1 AND deleted_at IS NULL" guard: a seeded, not-yet-
// retired user is live; an unknown or soft-deleted one is not.
func (f *fakeRepo) userLive(userID string) bool {
	for _, su := range f.seededUsers {
		if su.view.ID == userID {
			return su.detail.DeletedAt == nil
		}
	}
	return false
}

func (f *fakeRepo) StartMigration(_ context.Context, id, sourceUserID string, now time.Time) error {
	if !f.userLive(sourceUserID) {
		return ErrNotFound
	}
	for k, m := range f.migrations { // supersede any prior non-redeemed row for the source
		if m.sourceUserID == sourceUserID && m.state != "redeemed" {
			delete(f.migrations, k)
		}
	}
	f.migrations[id] = &fakeMigration{
		id: id, sourceUserID: sourceUserID, state: "initiated", createdAt: now,
	}
	return nil
}

func (f *fakeRepo) MigrationForSource(_ context.Context, sourceUserID string) (*MigrationView, error) {
	for _, m := range f.migrations {
		if m.sourceUserID == sourceUserID && m.state != "redeemed" {
			return m.view(), nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeRepo) ConfirmMigration(_ context.Context, sourceUserID, factor string, now time.Time) error {
	for _, m := range f.migrations {
		if m.sourceUserID == sourceUserID && m.state == "initiated" {
			m.state = "confirmed"
			m.confirmFactor = factor
			m.confirmedAt = now
			return nil
		}
	}
	return ErrConflict
}

func (f *fakeRepo) IssueMigrationCode(_ context.Context, sourceUserID, targetUserID, codeHash string, expiresAt time.Time) error {
	for _, m := range f.migrations {
		if m.sourceUserID == sourceUserID && m.state == "confirmed" {
			m.state = "code_issued"
			m.targetUserID = targetUserID
			m.codeHash = codeHash
			m.codeExpiresAt = expiresAt
			return nil
		}
	}
	return ErrConflict
}

func (f *fakeRepo) RedeemMigration(_ context.Context, targetUserID, codeHash string, now time.Time) (string, []string, error) {
	var mig *fakeMigration
	for _, m := range f.migrations {
		if m.codeHash == codeHash && m.targetUserID == targetUserID &&
			m.state == "code_issued" && m.codeExpiresAt.After(now) {
			mig = m
			break
		}
	}
	if mig == nil {
		return "", nil, ErrLinkCodeInvalid
	}
	// Re-point every server the source owns to the target (byName holds pointers).
	var moved []string
	for name, rec := range f.byName {
		if rec.OwnerID == mig.sourceUserID {
			rec.OwnerID = targetUserID
			moved = append(moved, name)
		}
	}
	sort.Strings(moved) // deterministic for assertions
	// Retire the source: disable + soft-delete.
	for i, su := range f.seededUsers {
		if su.view.ID == mig.sourceUserID {
			f.seededUsers[i].view.Disabled = true
			f.seededUsers[i].detail.Disabled = true
			t := now
			f.seededUsers[i].detail.DeletedAt = &t
		}
	}
	mig.state = "redeemed"
	mig.redeemedAt = now
	return mig.sourceUserID, moved, nil
}

// ---- quota admin fakes ----

func (f *fakeRepo) GetQuotas(_ context.Context, userID string) (*QuotaView, error) {
	v := &QuotaView{UserID: userID}
	if f.fakeQuotas == nil {
		return v, nil
	}
	if qv, ok := f.fakeQuotas[userID]; ok {
		v.MaxServers = qv.MaxServers
		v.MaxCPUMilli = qv.MaxCPUMilli
		v.MaxMemoryMB = qv.MaxMemoryMB
		v.MaxStorageGB = qv.MaxStorageGB
	}
	return v, nil
}

func (f *fakeRepo) SetQuotas(_ context.Context, userID string, qi QuotaInput, _ string) (*QuotaView, error) {
	if f.fakeQuotas == nil {
		f.fakeQuotas = map[string]*QuotaView{}
	}
	if _, ok := f.fakeQuotas[userID]; !ok {
		f.fakeQuotas[userID] = &QuotaView{UserID: userID}
	}
	if qi.MaxServers != nil {
		f.fakeQuotas[userID].MaxServers = qi.MaxServers
	}
	if qi.MaxCPUMilli != nil {
		f.fakeQuotas[userID].MaxCPUMilli = qi.MaxCPUMilli
	}
	if qi.MaxMemoryMB != nil {
		f.fakeQuotas[userID].MaxMemoryMB = qi.MaxMemoryMB
	}
	if qi.MaxStorageGB != nil {
		f.fakeQuotas[userID].MaxStorageGB = qi.MaxStorageGB
	}
	return f.fakeQuotas[userID], nil
}

// ---- session admin fakes ----

func (f *fakeRepo) ListUserSessions(_ context.Context, userID string, now time.Time) ([]SessionView, error) {
	var out []SessionView
	for hash, s := range f.sessions {
		if s.userID == userID && !s.revoked && s.expiresAt.After(now) {
			out = append(out, SessionView{TokenHash: hash, CreatedAt: time.Now(), ExpiresAt: s.expiresAt})
		}
	}
	return out, nil
}

func (f *fakeRepo) RevokeAllUserSessions(_ context.Context, userID string) error {
	for _, s := range f.sessions {
		if s.userID == userID {
			s.revoked = true
		}
	}
	return nil
}

func (f *fakeRepo) UnlinkAccount(_ context.Context, userID, mcUUID string) error {
	if f.links[mcUUID] != userID {
		return ErrNotFound
	}
	delete(f.links, mcUUID)
	delete(f.linkAuthSource, mcUUID)
	return nil
}

func (f *fakeRepo) LinkAccount(_ context.Context, userID, mcUUID, authSource string) error {
	if existing, ok := f.links[mcUUID]; ok && existing != userID {
		return ErrConflict
	}
	f.links[mcUUID] = userID
	f.linkAuthSource[mcUUID] = authSource
	f.linked[userID] = true
	return nil
}

// ---- op-login & setup token fakes (spec §B op-login / setup) ----

// UserByEmail mirrors PGRepo.UserByEmail: only a VERIFIED address resolves (the
// address was proven via an email OTP, not merely asserted), and the match is
// case-insensitive so the caller may type the address in any casing — the STORED
// casing is what the mailer and audit trail use. A non-verified or unknown address
// is indistinguishable from no account: both yield ErrNotFound.
func (f *fakeRepo) UserByEmail(_ context.Context, email string) (*StaffUser, error) {
	for _, u := range f.staff {
		if u.EmailVerified && strings.EqualFold(u.Email, email) {
			su := *u
			return &su, nil
		}
	}
	return nil, ErrNotFound
}

// ConsumeLoginEmailOTP mirrors PGRepo.ConsumeLoginEmailOTP: it redeems the newest
// live code for (user, purpose) WITHOUT the identity side-effect (login already
// resolved the userID via UserByEmail, so the address is settled). It charges an
// attempt on a hash mismatch (exactly like VerifyEmailOTP) but never writes
// users.email or runs the verified-email guard. A missing/expired/consumed code →
// ErrOTPInvalid; a mismatch → ErrOTPInvalid too (and costs an attempt without
// consuming); a locked code → ErrOTPLocked; a match → consumed, nil.
func (f *fakeRepo) ConsumeLoginEmailOTP(_ context.Context, userID, purpose, codeHash string, now time.Time) error {
	var live *fakeEmailOTP
	for _, o := range f.otps { // newest live (user, purpose), mirroring VerifyEmailOTP
		if o.userID != userID || o.purpose != purpose || o.consumed {
			continue
		}
		if live == nil || o.createdAt.After(live.createdAt) {
			live = o
		}
	}
	if live == nil || !live.expiresAt.After(now) {
		return ErrOTPInvalid
	}
	if live.attempts >= otpMaxAttempts {
		return ErrOTPLocked
	}
	if live.codeHash != codeHash {
		live.attempts++ // a typo costs an attempt but does not consume the code
		return ErrOTPInvalid
	}
	live.consumed = true
	return nil
}

// CreateOpLoginRequest records a fresh pending op.console login attempt. status is
// born 'pending'; createdAt orders the pending list (the PG ORDER BY created_at).
func (f *fakeRepo) CreateOpLoginRequest(_ context.Context, id, userID, email string, expiresAt time.Time) error {
	f.opLogins[id] = &fakeOpLogin{
		id: id, userID: userID, email: email, status: "pending",
		expiresAt: expiresAt, createdAt: expiresAt, // createdAt proxy: constant TTL ⇒ later expiry == later creation
	}
	return nil
}

// OpLoginRequestByID loads a request by handle, projecting the fake row into the
// OpLoginRequest the status/finish paths read (Status, Consumed, ExpiresAt). Status
// is the (approved_at, denied_at) projection the handler gates on.
func (f *fakeRepo) OpLoginRequestByID(_ context.Context, id string) (*OpLoginRequest, error) {
	r, ok := f.opLogins[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &OpLoginRequest{
		ID: r.id, UserID: r.userID, Email: r.email, ExpiresAt: r.expiresAt,
		Status: r.status, Consumed: r.consumed,
	}, nil
}

// ConsumeOpLoginRequest stamps consumed on an unconsumed, unexpired request (the
// finish path's single-use guard), mirroring the PG zero-rows-else UPDATE. The
// approval gate is read by the handler BEFORE this call, so consume only checks
// consumed_at and expiry (exactly as PG does).
func (f *fakeRepo) ConsumeOpLoginRequest(_ context.Context, id string, now time.Time) error {
	r, ok := f.opLogins[id]
	if !ok || r.consumed || !r.expiresAt.After(now) {
		return ErrNotFound
	}
	r.consumed = true
	return nil
}

// ListPendingOpLogins returns the live (pending, unconsumed, unexpired) requests
// oldest-first, mirroring the PG WHERE consumed_at IS NULL AND approved_at IS NULL
// AND expires_at > now ORDER BY created_at. Username is joined from the staff map
// (the in-game admin needs to name who is waiting), exactly as the repo.go contract
// documents — ListPendingOpLogins is the ONLY path that populates Username.
func (f *fakeRepo) ListPendingOpLogins(_ context.Context, now time.Time) ([]OpLoginRequest, error) {
	var out []OpLoginRequest
	for _, r := range f.opLogins {
		if r.consumed || r.status != "pending" || !r.expiresAt.After(now) {
			continue
		}
		out = append(out, OpLoginRequest{
			ID: r.id, UserID: r.userID, Username: f.usernameFor(r.userID),
			Email: r.email, ExpiresAt: r.expiresAt, Status: "pending", CreatedAt: r.createdAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// ApproveOpLogin marks a pending request approved by approverUserID, atomically: it
// flips status to 'approved' only on a still-pending, unconsumed, unexpired row, else
// ErrNotFound (double approve / dead request is a no-op the caller surfaces as 404).
func (f *fakeRepo) ApproveOpLogin(_ context.Context, id, approverUserID string, now time.Time) error {
	r, ok := f.opLogins[id]
	if !ok || r.consumed || r.status != "pending" || !r.expiresAt.After(now) {
		return ErrNotFound
	}
	r.status = "approved"
	return nil
}

// ConsumeSetupToken atomically marks a one-time setup token consumed and returns
// its user_id, or ErrNotFound when absent, already consumed, or expired.
func (f *fakeRepo) ConsumeSetupToken(_ context.Context, tokenHash string, now time.Time) (string, error) {
	tok, ok := f.setupTokens[tokenHash]
	if !ok || !tok.ConsumedAt.IsZero() || !tok.ExpiresAt.After(now) {
		return "", ErrNotFound
	}
	tok.ConsumedAt = now
	f.setupTokens[tokenHash] = tok
	return tok.UserID, nil
}

// usernameFor joins a userID to its staff username (the ListPendingOpLogins
// projection the in-game admin needs to name who is waiting). "" when the user is
// gone — mirroring a missing JOIN row.
func (f *fakeRepo) usernameFor(userID string) string {
	for _, u := range f.staff {
		if u.ID == userID {
			return u.Username
		}
	}
	return ""
}

// fakeRestorer records the restore it was asked to start and returns a canned
// error, mirroring the Restorer kick-off contract. The real restore Job is
// integration-only, so the handler is tested against this fake (spec §466).
type fakeRestorer struct {
	err     error
	calls   int
	gotName string
	gotRef  string
}

func (f *fakeRestorer) Restore(_ context.Context, name, ref string) error {
	f.calls++
	f.gotName, f.gotRef = name, ref
	return f.err
}

type fakeCluster struct {
	byName    map[string]*ServerInfo
	bySub     map[string]*ServerInfo
	list      []ServerInfo
	desired   map[string]v1alpha1.DesiredState
	created   map[string]CreateServerInput // name -> the validated input it was created from
	patched   map[string]ServerSpecPatch   // name -> the validated spec patch it received
	createErr error
	pingErr   error
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{byName: map[string]*ServerInfo{}, bySub: map[string]*ServerInfo{},
		desired: map[string]v1alpha1.DesiredState{}, created: map[string]CreateServerInput{},
		patched: map[string]ServerSpecPatch{}}
}
func (c *fakeCluster) GetServer(_ context.Context, n string) (*ServerInfo, error) {
	if s, ok := c.byName[n]; ok {
		return s, nil
	}
	return nil, ErrNotFound
}
func (c *fakeCluster) GetBySubdomain(_ context.Context, s string) (*ServerInfo, error) {
	if v, ok := c.bySub[s]; ok {
		return v, nil
	}
	return nil, ErrNotFound
}
func (c *fakeCluster) ListServers(_ context.Context) ([]ServerInfo, error) { return c.list, nil }
func (c *fakeCluster) Ping(_ context.Context) error                        { return c.pingErr }
func (c *fakeCluster) SetDesiredState(_ context.Context, n string, s v1alpha1.DesiredState) error {
	c.desired[n] = s
	return nil
}
func (c *fakeCluster) CreateServer(_ context.Context, in CreateServerInput) error {
	if c.createErr != nil {
		return c.createErr
	}
	if _, ok := c.byName[in.Name]; ok {
		return ErrConflict
	}
	c.created[in.Name] = in
	info := &ServerInfo{Name: in.Name, Subdomain: in.Subdomain,
		AutostartPolicy: string(in.AutostartPolicy),
		DesiredState:    string(v1alpha1.DesiredStopped), Phase: string(v1alpha1.PhaseStopped)}
	c.byName[in.Name] = info
	c.bySub[in.Subdomain] = info
	return nil
}
func (c *fakeCluster) PatchServerSpec(_ context.Context, n string, p ServerSpecPatch) error {
	info, ok := c.byName[n]
	if !ok {
		return ErrNotFound
	}
	c.patched[n] = p
	// Apply only the fields the lifecycle view exposes, so a follow-up read sees
	// the mutation (mirrors the real merge patch touching only non-nil fields).
	if p.AutostartPolicy != nil {
		info.AutostartPolicy = string(*p.AutostartPolicy)
	}
	return nil
}

// fakeConsole records the command it was asked to run and returns a canned reply
// or error, mirroring the Console contract. The real K8sConsole's password
// resolution and RCON dial are integration-only, so the handler is tested
// against this fake (spec §8 写=RCON).
type fakeConsole struct {
	reply      string
	err        error
	calls      int
	gotName    string
	gotCommand string
}

func (f *fakeConsole) RunCommand(_ context.Context, name, command string) (string, error) {
	f.calls++
	f.gotName, f.gotCommand = name, command
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

// staticExternal injects a fixed principal so handler logic is tested without
// real JWT crypto (which is exercised separately in TestAccessVerifier).
type staticExternal struct {
	p   *Principal
	err error
}

func (s staticExternal) Authenticate(*http.Request) (*Principal, error) { return s.p, s.err }

type okInternal struct{}

func (okInternal) Authenticate(*http.Request) error { return nil }

// ---- helpers ----

func newTestAPI(repo Repo, cl Cluster) *API {
	return &API{Repo: repo, Cluster: cl, Internal: okInternal{}, RootDomain: testRoot,
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
}

func do(h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var jsonHeader = map[string]string{"Content-Type": "application/json"}

func ctHeader(ct string) map[string]string { return map[string]string{"Content-Type": ct} }

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var raw map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("error body not JSON: %v (%s)", err, w.Body.String())
	}
	return raw["error"]["code"]
}

// ---- dual-face separation ----

func TestInternalFaceRequiresServiceToken(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.Internal = BearerTokenAuth{Token: "s3cr3t"}
	h := api.InternalHandler()

	// no token -> 401
	if w := do(h, "GET", "/api/v1/servers", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: code = %d, want 401", w.Code)
	}
	// wrong token -> 401
	if w := do(h, "GET", "/api/v1/servers", "", map[string]string{"Authorization": "Bearer nope"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code = %d, want 401", w.Code)
	}
	// right token -> 200
	if w := do(h, "GET", "/api/v1/servers", "", map[string]string{"Authorization": "Bearer s3cr3t"}); w.Code != http.StatusOK {
		t.Fatalf("right token: code = %d, want 200", w.Code)
	}
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.Internal = BearerTokenAuth{Token: "s3cr3t"}
	if w := do(api.InternalHandler(), "GET", "/healthz", "", nil); w.Code != http.StatusOK {
		t.Fatalf("healthz code = %d, want 200", w.Code)
	}
}

// TestReadyzPingsDependencies proves /readyz verifies DB and K8s API liveness
// before declaring ready, and returns 503 when either is down (spec §7).
func TestReadyzPingsDependencies(t *testing.T) {
	repo := newFakeRepo()
	cl := newFakeCluster()
	api := newTestAPI(repo, cl)
	api.Internal = BearerTokenAuth{Token: "s3cr3t"}

	// Both healthy.
	if w := do(api.InternalHandler(), "GET", "/readyz", "", nil); w.Code != http.StatusOK {
		t.Fatalf("readyz code = %d, want 200 when both deps are healthy (%s)", w.Code, w.Body.String())
	}

	// DB down.
	repo.pingErr = errors.New("connection refused")
	if w := do(api.InternalHandler(), "GET", "/readyz", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz code = %d, want 503 when DB is down (%s)", w.Code, w.Body.String())
	}
	repo.pingErr = nil

	// K8s API down.
	cl.pingErr = errors.New("cannot reach apiserver")
	if w := do(api.InternalHandler(), "GET", "/readyz", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz code = %d, want 503 when K8s API is down (%s)", w.Code, w.Body.String())
	}
}

func TestExternalFaceRequiresPrincipal(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{err: http.ErrNoCookie} // any auth error
	if w := do(api.ExternalHandler(), "GET", "/api/v1/me/servers", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
}

// TestUnbindUserPasskeys proves the authenticator-remediation door
// (DELETE /users/{id}/passkeys) severs every passkey a target account holds, is
// gated to the owner role (an Operator-grade admin is refused, so it is stricter
// than the app-admin surface), and treats an account with no passkeys as a 200
// no-op rather than a 404 — remediation must be idempotent.
func TestUnbindUserPasskeys(t *testing.T) {
	repo := newFakeRepo()
	api := newTestAPI(repo, newFakeCluster())

	// Seed the target account with two bound passkeys.
	ctx := context.Background()
	for _, id := range []string{"pk1", "pk2"} {
		if err := repo.CreatePasskeyCredential(ctx, PasskeyCredential{
			ID: id, UserID: "victim", CredentialID: "cred-" + id, PublicKey: "pub",
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	owner := &Principal{UserID: "owner1", Email: "owner@mc.example.net", Role: "owner", ViaAdminAccess: true}

	t.Run("owner unbinds every passkey", func(t *testing.T) {
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "DELETE", "/api/v1/users/victim/passkeys", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		creds, _ := repo.PasskeyCredentialsForUser(ctx, "victim")
		if len(creds) != 0 {
			t.Fatalf("passkeys remaining = %d, want 0", len(creds))
		}
	})

	t.Run("no passkeys is a 200 no-op, not a 404", func(t *testing.T) {
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "DELETE", "/api/v1/users/ghost/passkeys", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("an Operator-grade admin is refused (owner-only)", func(t *testing.T) {
		api.External = staticExternal{p: &Principal{UserID: "op1", Role: "admin", ViaAdminAccess: true}}
		w := do(api.ExternalHandler(), "DELETE", "/api/v1/users/victim/passkeys", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
	})
}

// TestMeIdentity proves GET /api/v1/me reports the server-computed identity the
// panel uses to gate its Admin / SysAdmin navigation. The load-bearing assertion
// is the third subtest: is_admin tracks Principal.IsAdmin(), so the admin ROLE is
// not sufficient — the request must ALSO have arrived via the admin Access path
// (ViaAdminAccess). An admin who reached the panel through the ordinary app path
// therefore reads is_admin=false and the panel hides the admin surfaces (which the
// backend would 403 regardless). This keeps the client from re-deriving graded ZT.
func TestMeIdentity(t *testing.T) {
	get := func(p *Principal) map[string]any {
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.External = staticExternal{p: p}
		w := do(api.ExternalHandler(), "GET", "/api/v1/me", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		return got
	}

	t.Run("ordinary user: is_admin false", func(t *testing.T) {
		got := get(&Principal{UserID: "u1", Email: "u1@example.net", Role: "user"})
		if got["user_id"] != "u1" || got["role"] != "user" || got["is_admin"] != false {
			t.Fatalf("got %v, want user_id=u1 role=user is_admin=false", got)
		}
	})

	t.Run("admin via admin Access path: is_admin true", func(t *testing.T) {
		got := get(&Principal{UserID: "a1", Email: "a1@example.net", Role: "admin", ViaAdminAccess: true})
		if got["role"] != "admin" || got["is_admin"] != true {
			t.Fatalf("got %v, want role=admin is_admin=true", got)
		}
	})

	t.Run("admin via ordinary app path: is_admin false (graded ZT)", func(t *testing.T) {
		got := get(&Principal{UserID: "a1", Email: "a1@example.net", Role: "admin", ViaAdminAccess: false})
		if got["role"] != "admin" {
			t.Fatalf("role = %v, want admin", got["role"])
		}
		if got["is_admin"] != false {
			t.Fatalf("is_admin = %v, want false — the admin role alone must not grant admin tier "+
				"without the admin Access path", got["is_admin"])
		}
	})
}

// ---- fleet (SysAdmin cockpit read) ----

// TestFleetAdminRead proves the SysAdmin cockpit's fleet read is admin-tier AND
// fleet-wide. Two properties distinguish it from the app-tier /me/servers: a plain
// user is rejected by adminOnly before the handler runs, and an admin sees EVERY
// server the cluster reports (CRD truth via ListServers, §1) rather than a
// caller-scoped slice.
func TestFleetAdminRead(t *testing.T) {
	cl := newFakeCluster()
	cl.list = []ServerInfo{
		{Name: "survival", Phase: "Running", Ready: true},
		{Name: "creative", Phase: "Stopped"},
		{Name: "skyblock", Phase: "Running", Ready: true},
	}

	t.Run("plain user forbidden", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), cl)
		api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/fleet", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (a plain user must not read the fleet)", w.Code)
		}
	})

	t.Run("admin via ordinary app path forbidden (graded ZT)", func(t *testing.T) {
		// The admin ROLE alone is not enough: without the admin Access path adminOnly
		// rejects, so the cockpit read cannot be reached by an admin who arrived via
		// the ordinary app face — exactly as GET /api/v1/me reports is_admin=false there.
		api := newTestAPI(newFakeRepo(), cl)
		api.External = staticExternal{p: &Principal{UserID: "a1", Email: "a1@example.net",
			Role: "admin", ViaAdminAccess: false}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/fleet", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (admin role without the admin Access path)", w.Code)
		}
	})

	// fleetRow mirrors the on-the-wire fleetServerView: the lifecycle fields plus
	// the presentational owner join. A server absent from ServerOwners (unclaimed)
	// or a failed lookup must serialize owner as "" (omitempty drops it).
	type fleetRow struct {
		Name  string `json:"name"`
		Owner string `json:"owner"`
	}
	adminAPI := func(repo *fakeRepo) *API {
		api := newTestAPI(repo, cl)
		api.External = staticExternal{p: &Principal{UserID: "a1", Email: "a1@example.net",
			Role: "admin", ViaAdminAccess: true}}
		return api
	}
	readFleet := func(t *testing.T, api *API) []fleetRow {
		t.Helper()
		w := do(api.ExternalHandler(), "GET", "/api/v1/fleet", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		var got map[string][]fleetRow
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		return got["servers"]
	}

	t.Run("admin reads the whole fleet", func(t *testing.T) {
		rows := readFleet(t, adminAPI(newFakeRepo()))
		// Fleet-wide: all three servers, not a caller-scoped subset.
		if len(rows) != 3 {
			t.Fatalf("servers = %d, want 3 (the fleet read must not be caller-scoped)", len(rows))
		}
	})

	t.Run("owner merges for claimed, absent for unclaimed", func(t *testing.T) {
		repo := newFakeRepo()
		// Only "survival" is claimed; "creative"/"skyblock" stay unowned.
		repo.owners["survival"] = "alice@example.net"
		byName := map[string]string{}
		for _, r := range readFleet(t, adminAPI(repo)) {
			byName[r.Name] = r.Owner
		}
		if byName["survival"] != "alice@example.net" {
			t.Fatalf("survival owner = %q, want alice@example.net", byName["survival"])
		}
		if byName["creative"] != "" {
			t.Fatalf("creative owner = %q, want empty (unclaimed)", byName["creative"])
		}
	})

	t.Run("owner lookup failure degrades to owner-less rows", func(t *testing.T) {
		repo := newFakeRepo()
		repo.owners["survival"] = "alice@example.net" // would merge, but the lookup errors
		repo.ownersErr = fmt.Errorf("postgres unreachable")
		rows := readFleet(t, adminAPI(repo)) // must still be 200, not 500
		if len(rows) != 3 {
			t.Fatalf("servers = %d, want 3 (a Postgres blip must not drop the fleet)", len(rows))
		}
		for _, r := range rows {
			if r.Owner != "" {
				t.Fatalf("%s owner = %q, want empty (owner lookup failed → degrade)", r.Name, r.Owner)
			}
		}
	})
}

// ---- join-event ----

func TestJoinEvent(t *testing.T) {
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival"}
	api := newTestAPI(repo, newFakeCluster())
	h := api.InternalHandler()

	t.Run("missing uuid 400", func(t *testing.T) {
		w := do(h, "POST", "/api/v1/internal/servers/survival/join-event", `{}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})
	t.Run("records join", func(t *testing.T) {
		w := do(h, "POST", "/api/v1/internal/servers/survival/join-event",
			`{"mc_uuid":"11111111-1111-1111-1111-111111111111"}`, nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("code = %d, want 204 (%s)", w.Code, w.Body.String())
		}
		if len(repo.joins) != 1 {
			t.Fatalf("expected 1 recorded join, got %d", len(repo.joins))
		}
	})
	t.Run("unknown server 404", func(t *testing.T) {
		w := do(h, "POST", "/api/v1/internal/servers/missing/join-event",
			`{"mc_uuid":"11111111-1111-1111-1111-111111111111"}`, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})
}

// ---- claim (§9.3) ----

func TestClaimStateMachine(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user", ViaAdminAccess: false}

	t.Run("not linked -> 412", func(t *testing.T) {
		repo := newFakeRepo()
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil)
		if w.Code != http.StatusPreconditionFailed || decodeErr(t, w) != "not_linked" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("over quota -> 403", func(t *testing.T) {
		repo := newFakeRepo()
		repo.linked["u1"] = true
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil)
		if w.Code != http.StatusForbidden || decodeErr(t, w) != "quota_exceeded" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("atomic gate refusal -> 403 quota_exceeded", func(t *testing.T) {
		// The advisory pre-check passed, but ClaimServer's serialized re-check
		// (audit #4) refuses: the caller must see the same 403, not a 500.
		repo := newFakeRepo()
		repo.linked["u1"] = true
		repo.quota["u1"] = true
		repo.claimOK["survival"] = true
		repo.claimQuotaRefuse["survival"] = true
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil)
		if w.Code != http.StatusForbidden || decodeErr(t, w) != "quota_exceeded" {
			t.Fatalf("code = %d body %s, want 403 quota_exceeded", w.Code, w.Body.String())
		}
	})
	t.Run("already claimed -> 409", func(t *testing.T) {
		repo := newFakeRepo()
		repo.linked["u1"] = true
		repo.quota["u1"] = true
		repo.claimOK["survival"] = false // row exists but owner already set
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "already_claimed" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("success -> 200 + audit", func(t *testing.T) {
		repo := newFakeRepo()
		repo.linked["u1"] = true
		repo.quota["u1"] = true
		repo.claimOK["survival"] = true
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "claim" || repo.audits[0].Actor != "u1@example.net" {
			t.Fatalf("audit not written as expected: %+v", repo.audits)
		}
	})
}

// ---- wake (autostartPolicy gate + cooldown) ----

func TestWakeAutostartGate(t *testing.T) {
	mk := func(policy string) (*API, *fakeCluster) {
		repo := newFakeRepo()
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", AutostartPolicy: policy}
		api := newTestAPI(repo, cl)
		return api, cl
	}
	stranger := &Principal{UserID: "stranger", Role: "user"}

	t.Run("public: any user wakes", func(t *testing.T) {
		api, cl := mk("public")
		api.External = staticExternal{p: stranger}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desiredState = %q, want Running", cl.desired["survival"])
		}
	})
	t.Run("ownerOnly: stranger forbidden", func(t *testing.T) {
		api, cl := mk("ownerOnly")
		api.External = staticExternal{p: stranger}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("desiredState must not change on a forbidden wake")
		}
	})
	t.Run("ownerOnly: owner wakes", func(t *testing.T) {
		api, _ := mk("ownerOnly")
		api.Repo.(*fakeRepo).byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		api.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("allowlist: only listed user wakes", func(t *testing.T) {
		api, _ := mk("allowlist")
		repo := api.Repo.(*fakeRepo)
		repo.allowlist["survival"] = map[string]bool{"friend": true}
		api.External = staticExternal{p: &Principal{UserID: "friend", Role: "user"}}
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("listed user: code = %d", w.Code)
		}
		api.External = staticExternal{p: stranger}
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusForbidden {
			t.Fatalf("stranger: code = %d, want 403", w.Code)
		}
	})
}

func TestWakeCooldown(t *testing.T) {
	repo := newFakeRepo()
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", AutostartPolicy: "public"}
	api := newTestAPI(repo, cl)
	api.WakeCooldown = time.Minute
	api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
	h := api.ExternalHandler()

	if w := do(h, "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
		t.Fatalf("first wake code = %d", w.Code)
	}
	// clock is frozen, so the second wake is inside the cooldown window
	if w := do(h, "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second wake code = %d, want 429", w.Code)
	}
}

// TestWakeRunningCap exercises the §9.1 cluster-wide concurrency lever on the
// external wake path. The cap counts CRD truth via ListServers (never Postgres,
// per §1) and is a default-off lever: MaxRunningServers <= 0 disables it exactly
// as a zero WakeCooldown disables the per-server throttle. A full cluster answers
// 503 at_capacity — distinct from the cooldown's 429 — and an already-Running
// target re-wakes idempotently regardless of the cap.
func TestWakeRunningCap(t *testing.T) {
	// capAPI builds an external-face API whose cluster already holds `running`
	// servers desired-Running plus a stopped "survival" target, with the cap set.
	capAPI := func(cap, running int) (*API, *fakeCluster) {
		cl := newFakeCluster()
		target := &ServerInfo{Name: "survival", AutostartPolicy: "public",
			DesiredState: string(v1alpha1.DesiredStopped)}
		cl.byName["survival"] = target
		cl.list = []ServerInfo{*target}
		for i := 0; i < running; i++ {
			cl.list = append(cl.list, ServerInfo{Name: fmt.Sprintf("running-%d", i),
				DesiredState: string(v1alpha1.DesiredRunning)})
		}
		api := newTestAPI(newFakeRepo(), cl)
		api.MaxRunningServers = cap
		api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
		return api, cl
	}

	t.Run("at cap: wake rejected with 503 at_capacity", func(t *testing.T) {
		api, cl := capAPI(2, 2)
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("code = %d, want 503", w.Code)
		}
		if code := decodeErr(t, w); code != "at_capacity" {
			t.Fatalf("error code = %q, want at_capacity", code)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("desiredState must not change when the cluster is at capacity")
		}
	})

	t.Run("below cap: wake accepted", func(t *testing.T) {
		api, cl := capAPI(3, 2)
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202", w.Code)
		}
		if cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desired = %q, want Running", cl.desired["survival"])
		}
	})

	t.Run("cap disabled (0): wake accepted even when many run", func(t *testing.T) {
		api, _ := capAPI(0, 5)
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202", w.Code)
		}
	})

	t.Run("already-Running target re-wakes despite a full cap (idempotent)", func(t *testing.T) {
		api, cl := capAPI(2, 2)
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (idempotent re-wake)", w.Code)
		}
	})
}

// TestWakeCooldownNotBurnedAtCapacity is the §9.1 regression guard for the
// cooldown/cap ordering: a wake the running-cap refuses with 503 must NOT start
// the per-server cooldown. Otherwise a player held because the cluster was
// momentarily full would, once a slot frees, still be made to wait out a 30s
// cooldown their refused wake never earned. With the clock frozen, a 503 followed
// by the same server waking the instant capacity frees must return 202, not 429.
func TestWakeCooldownNotBurnedAtCapacity(t *testing.T) {
	cl := newFakeCluster()
	target := &ServerInfo{Name: "survival", AutostartPolicy: "public",
		DesiredState: string(v1alpha1.DesiredStopped)}
	cl.byName["survival"] = target
	cl.list = []ServerInfo{*target, {Name: "other", DesiredState: string(v1alpha1.DesiredRunning)}}
	api := newTestAPI(newFakeRepo(), cl)
	api.WakeCooldown = time.Minute
	api.MaxRunningServers = 1
	api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
	h := api.ExternalHandler()

	// The cluster is full (1 running == cap): the wake is refused with 503 and must
	// leave the cooldown unstarted.
	if w := do(h, "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("at-capacity wake code = %d, want 503", w.Code)
	}
	if _, set := cl.desired["survival"]; set {
		t.Fatal("desiredState must not change when refused at capacity")
	}

	// A slot frees (the other server is gone). The same server, same frozen clock,
	// must now wake — a 429 here would prove the 503 had burned the cooldown.
	cl.list = []ServerInfo{*target}
	if w := do(h, "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
		t.Fatalf("post-capacity wake code = %d, want 202 (the 503 must not burn the cooldown)", w.Code)
	}
	if cl.desired["survival"] != v1alpha1.DesiredRunning {
		t.Fatalf("desired = %q, want Running", cl.desired["survival"])
	}
}

// ---- Zero-Trust admin boundary (§14) ----

func TestAdminBoundary(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())

	t.Run("user role rejected before handler", func(t *testing.T) {
		api.External = staticExternal{p: &Principal{UserID: "u", Role: "user", ViaAdminAccess: false}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers", `{}`, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
	})
	t.Run("admin role without admin access rejected", func(t *testing.T) {
		api.External = staticExternal{p: &Principal{UserID: "a", Role: "admin", ViaAdminAccess: false}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers", `{}`, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("role=admin but panel path: code = %d, want 403", w.Code)
		}
	})
	t.Run("admin via admin-access reaches handler", func(t *testing.T) {
		api.External = staticExternal{p: &Principal{UserID: "a", Role: "admin", ViaAdminAccess: true}}
		// The body is empty so the real handler rejects it (validation 400 with no
		// Builder → 503), but the point is that the admin Zero-Trust path is NOT
		// stopped at the 403 boundary — it reaches the handler.
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers", `{}`, nil)
		if w.Code == http.StatusForbidden {
			t.Fatalf("admin via admin-access must reach the handler, got 403")
		}
	})
}

// TestOpConsoleDoorGate proves op.console is staff-only at the DOOR: a non-admin
// principal that reaches the operator host is refused before any handler, even on an
// app-tier route (/me) that no adminOnly wraps. Authentication is not access on
// op.console — the "internal permission verification" the model requires on top of
// Zero-Trust. The gate is scoped to the admin host, so the identical principal is
// unaffected on the player console.
func TestOpConsoleDoorGate(t *testing.T) {
	opHost := "op.console." + testRoot
	playerHost := "console." + testRoot

	newAPI := func(p *Principal) *API {
		a := newTestAPI(newFakeRepo(), newFakeCluster())
		a.AdminHostname = opHost
		a.External = staticExternal{p: p}
		return a
	}

	t.Run("non-admin on op.console refused at the door", func(t *testing.T) {
		a := newAPI(&Principal{UserID: "u", Role: "user", ViaAdminAccess: false})
		w := do(a.ExternalHandler(), "GET", "https://"+opHost+"/api/v1/me", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("non-admin on op.console: code = %d, want 403 (staff-only door)", w.Code)
		}
	})
	t.Run("same non-admin on the player console passes the door", func(t *testing.T) {
		a := newAPI(&Principal{UserID: "u", Role: "user", ViaAdminAccess: false})
		w := do(a.ExternalHandler(), "GET", "https://"+playerHost+"/api/v1/me", "", nil)
		if w.Code == http.StatusForbidden {
			t.Fatalf("player console must not be gated by the op.console door, got 403")
		}
	})
	t.Run("admin on op.console reaches the handler", func(t *testing.T) {
		a := newAPI(&Principal{UserID: "a", Role: "admin", ViaAdminAccess: true})
		w := do(a.ExternalHandler(), "GET", "https://"+opHost+"/api/v1/me", "", nil)
		if w.Code == http.StatusForbidden {
			t.Fatalf("admin must pass the op.console door, got 403")
		}
	})
}

// TestSessionAuthOwnerGetsAdminAccess guards the owner-inclusion fix: a role=owner
// local session on op.console must carry ViaAdminAccess (and thus IsOwner()). The
// owner is a superset of admin, so excluding it from the session admin-path — as the
// code once did (u.Role == "admin" only) — silently made IsOwner() unreachable via a
// passwordless session, locking the platform owner out of the operator console.
func TestSessionAuthOwnerGetsAdminAccess(t *testing.T) {
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["owner"] = &StaffUser{ID: "u1", Email: "owner@" + testRoot, Role: "owner"}
	token := "session-token"
	repo.sessions[hashCookie(token)] = &fakeSession{userID: "u1", expiresAt: time.Now().Add(time.Hour)}
	auth := SessionAuth{Repo: repo, RootDomain: testRoot, AdminHostname: "op.console." + testRoot}

	r := httptest.NewRequest("GET", "https://op.console."+testRoot+"/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !p.ViaAdminAccess || !p.IsOwner() {
		t.Fatalf("owner via local session on op.console must carry admin access AND IsOwner, got %+v", p)
	}
}

// ---- error envelope ----

func TestErrorEnvelopeHasRequestID(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
	w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/status", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
	if w.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing X-Request-Id response header")
	}
	var raw map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if raw["error"]["request_id"] == "" {
		t.Fatal("error envelope missing request_id")
	}
}

// ---- real AccessVerifier (JWT aud) ----

func TestSessionAuthUsesConfiguredAdminHostname(t *testing.T) {
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["owner"] = &StaffUser{ID: "u1", Email: "owner@mc.example.net", Role: "admin"}
	token := "session-token"
	repo.sessions[hashCookie(token)] = &fakeSession{userID: "u1", expiresAt: time.Now().Add(time.Hour)}
	auth := SessionAuth{Repo: repo, RootDomain: "old.example.net", AdminHostname: "op.console.mc.example.net"}

	r := httptest.NewRequest("GET", "https://op.console.mc.example.net/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	p, err := auth.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !p.ViaAdminAccess {
		t.Fatalf("configured admin hostname should grant admin-path access, got %+v", p)
	}

	r = httptest.NewRequest("GET", "https://op.console.old.example.net/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	p, err = auth.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate fallback host: %v", err)
	}
	if p.ViaAdminAccess {
		t.Fatalf("root-domain fallback host must not grant admin-path access when admin_hostname is configured")
	}

	r = httptest.NewRequest("GET", "https://10.211.55.4:30443/api/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	p, err = auth.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate private IP host: %v", err)
	}
	if !p.ViaAdminAccess {
		t.Fatalf("private IP local panel should grant admin-path access, got %+v", p)
	}
}
func TestAccessVerifier(t *testing.T) {
	key := []byte("test-signing-key")
	keyfunc := func(*jwt.Token) (any, error) { return key, nil }
	v := AccessVerifier{Audience: "felis-app", AdminAudience: "felis-admin", Keyfunc: keyfunc}

	sign := func(claims accessClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}
	exp := jwt.NewNumericDate(time.Now().Add(time.Hour))

	t.Run("valid app token", func(t *testing.T) {
		s := sign(accessClaims{Email: "u@example.net", RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Audience: jwt.ClaimStrings{"felis-app"}, ExpiresAt: exp}})
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+s)
		p, err := v.Authenticate(r)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if p.UserID != "u1" || p.Email != "u@example.net" || p.Role != "user" || p.ViaAdminAccess {
			t.Fatalf("unexpected principal %+v", p)
		}
	})
	t.Run("admin audience sets ViaAdminAccess", func(t *testing.T) {
		s := sign(accessClaims{Role: "admin", RegisteredClaims: jwt.RegisteredClaims{
			Subject: "a1", Audience: jwt.ClaimStrings{"felis-app", "felis-admin"}, ExpiresAt: exp}})
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", s)
		p, err := v.Authenticate(r)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if !p.IsAdmin() {
			t.Fatalf("expected admin principal, got %+v", p)
		}
	})
	t.Run("wrong audience rejected", func(t *testing.T) {
		s := sign(accessClaims{RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Audience: jwt.ClaimStrings{"someone-else"}, ExpiresAt: exp}})
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+s)
		if _, err := v.Authenticate(r); err == nil {
			t.Fatal("expected audience rejection")
		}
	})
	t.Run("wrong signing key rejected", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Audience: jwt.ClaimStrings{"felis-app"}, ExpiresAt: exp}})
		s, _ := tok.SignedString([]byte("attacker-key"))
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+s)
		if _, err := v.Authenticate(r); err == nil {
			t.Fatal("expected signature rejection")
		}
	})
	t.Run("missing expiry rejected", func(t *testing.T) {
		s := sign(accessClaims{RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Audience: jwt.ClaimStrings{"felis-app"}}})
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+s)
		if _, err := v.Authenticate(r); err == nil {
			t.Fatal("expected missing-expiry rejection")
		}
	})
}

// TestSessionAuthOutageIs503Not401: a session-store outage must surface as 503
// auth_unavailable, not a 401 that reads as "please log in again". Both failure
// points are covered — the local_auth_enabled read and the session row read —
// plus the regression that a genuinely missing session still answers 401.
func TestSessionAuthOutageIs503Not401(t *testing.T) {
	apiWith := func(repo *fakeRepo) *API {
		a := newTestAPI(repo, newFakeCluster())
		a.External = SessionAuth{Repo: repo, RootDomain: testRoot, AdminHostname: "op.console." + testRoot}
		return a
	}
	cookie := map[string]string{"Cookie": sessionCookieName + "=any"}
	outage := errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")

	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.failGetSetting = outage
	if w := do(apiWith(repo).ExternalHandler(), "GET", "/api/v1/me", "", cookie); w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "auth_unavailable" {
		t.Fatalf("settings read outage = %d body %s, want 503 auth_unavailable", w.Code, w.Body.String())
	}

	repo = newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.failSessionUser = outage
	if w := do(apiWith(repo).ExternalHandler(), "GET", "/api/v1/me", "", cookie); w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "auth_unavailable" {
		t.Fatalf("session read outage = %d body %s, want 503 auth_unavailable", w.Code, w.Body.String())
	}

	// Regression: fail-closed auth (missing/invalid session) stays a 401.
	repo = newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	if w := do(apiWith(repo).ExternalHandler(), "GET", "/api/v1/me", "", cookie); w.Code != http.StatusUnauthorized || decodeErr(t, w) != "unauthorized" {
		t.Fatalf("missing session = %d body %s, want 401 unauthorized", w.Code, w.Body.String())
	}
}
