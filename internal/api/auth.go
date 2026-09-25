package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Principal is the authenticated external-face caller (spec §7, §14). The
// internal face (service token) never produces a Principal — it is a trusted
// machine caller, not a person.
type Principal struct {
	// UserID is the account's users.id.
	UserID string
	// Username is the account's login name.
	Username string
	// Email is the account's address. Only EmailVerified vouches for it: a player
	// can set any address before verifying it (auditActor).
	Email string
	// Role is "owner", "admin", or "user" (mirrors users.role).
	Role string
	// ViaAdminAccess is true only when a staff session arrived on the operator
	// console host (hostIsAdminConsole). Admin-tier operations require it in
	// addition to a staff role (spec §14: ZT is graded by operation). A staff
	// session arriving on the player console (console.*) never sets it.
	ViaAdminAccess bool
	// EmailVerified mirrors users.email_verified. The lockdown middleware gates
	// setup-incomplete accounts (EmailVerified=false, e.g. a freshly bootstrapped
	// Owner who has not yet proven control of their mailbox) to the setup-wizard
	// routes only, so an intercepted setup URL cannot yield full admin access
	// before the email-OTP verification step completes.
	EmailVerified bool
	// ViaSession is true for every principal SessionAuth resolves from a session
	// cookie. The session-scoped gates (setup lockdown, reauth, the device list)
	// key on it.
	ViaSession bool
	// ReauthAt is when the holder of the session last proved a factor of the
	// account; zero for a session that never did.
	ReauthAt time.Time
}

// staffRole reports whether a stored user role carries staff standing: admin,
// or owner (the superset of admin, see IsAdmin). This is the single place the
// role set is spelled out — every staff gate (web IsAdmin, session grading,
// the in-game op-login approve and admin wake) routes through it.
func staffRole(role string) bool {
	return role == "admin" || role == "owner"
}

// IsAdmin reports whether the principal may perform admin-tier operations.
// Both the staff role and arrival on the operator console host are required: a
// staff session arriving on the player console must not reach admin routes.
// An owner implicitly passes this check (the owner role is a superset of admin).
func (p *Principal) IsAdmin() bool {
	return p != nil && staffRole(p.Role) && p.ViaAdminAccess
}

// IsOwner reports whether the principal holds the platform-level owner role
// — the single identity that may manage users, quotas, and sessions. Only the
// first staff account minted by break-glass carries this role; every subsequent
// Operator is a plain admin. Like IsAdmin, it requires the operator console host.
func (p *Principal) IsOwner() bool {
	return p != nil && p.Role == "owner" && p.ViaAdminAccess
}

// Caller names the machine behind an internal-face token. Each caller holds a
// token of its own and each internal route lists the callers it serves
// (apiRoute.Callers), so a token copied out of one namespace opens only what
// that caller needs: the build Job's token reads a submission's context and
// nothing else, and only the proxy and the login gate can mint link codes.
type Caller string

const (
	// CallerVelocity is the proxy's felis-link plugin (felis-service-token,
	// written into felis-link.properties on the host).
	CallerVelocity Caller = "velocity"
	// CallerLimbo is the login gate's felis-limbo plugin (felis-limbo-token,
	// injected into the login pod only).
	CallerLimbo Caller = "limbo"
	// CallerBuild is the build Job's context-fetch initContainer
	// (felis-build-token in the build namespace).
	CallerBuild Caller = "build"
	// CallerOps is the on-node console, `felis backup-now` (felis-ops-token,
	// control namespace only).
	CallerOps Caller = "ops"
)

// InternalAuth authenticates the internal face: a per-caller static token
// presented as a Bearer credential, answered with the caller it belongs to. The
// internal face is never wrapped in Zero Trust (spec §1.8, §14 red line).
type InternalAuth interface {
	Authenticate(r *http.Request) (Caller, error)
}

// ExternalAuth authenticates the external face (people / panel) and returns the
// resolved Principal. Production is SessionAuth: the local session cookie the
// sign-in doors mint. Cloudflare Access, when an install sits behind it, is
// enforced at the edge only; felis-api does not read the Access JWT, so the
// identity and role always come from the users table.
type ExternalAuth interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// CallerTokens is the production InternalAuth: each caller's token, compared in
// constant time. A caller with no token cannot authenticate, so a missing
// Secret fails closed for that caller alone.
type CallerTokens map[Caller]string

// NewCallerTokens refuses a set that would make the caller ambiguous: two
// callers sharing a value, which is also what an install whose callers all
// still hold the one old service token would look like.
func NewCallerTokens(tokens map[Caller]string) (CallerTokens, error) {
	seen := map[string]Caller{}
	for caller, tok := range tokens {
		if tok == "" {
			continue
		}
		if other, dup := seen[tok]; dup {
			return nil, fmt.Errorf("the %s and %s tokens are the same value; each caller needs its own", other, caller)
		}
		seen[tok] = caller
	}
	return CallerTokens(tokens), nil
}

// Authenticate matches the Authorization: Bearer header against every caller's
// token, comparing each so the time taken does not say which one matched.
func (c CallerTokens) Authenticate(r *http.Request) (Caller, error) {
	got := bearerToken(r)
	if got == "" {
		return "", fmt.Errorf("missing bearer token")
	}
	var match Caller
	for caller, tok := range c {
		if tok != "" && subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1 {
			match = caller
		}
	}
	if match == "" {
		return "", fmt.Errorf("invalid service token")
	}
	return match, nil
}

// bearerToken extracts a Bearer credential from the Authorization header.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
