package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// Principal is the authenticated external-face caller (spec §7, §14). The
// internal face (service token) never produces a Principal — it is a trusted
// machine caller, not a person.
type Principal struct {
	// UserID is the stable web identity (SSO subject → users.id).
	UserID string
	// Email is the audited actor identity (spec §14: audit actor = Access email).
	Email string
	// Role is "owner", "admin", or "user" (mirrors users.role).
	Role string
	// ViaAdminAccess is true only when the request arrived through an admin-graded
	// path: the admin.* Zero-Trust hostname (Cloudflare Access, the remote face) OR
	// a local session presented on the op.console host (SessionAuth, the
	// passwordless face). Admin-tier operations require it in addition to
	// a staff role (spec §14: ZT is graded by operation). A staff session
	// arriving on the player console (console.*) never sets it.
	ViaAdminAccess bool
	// EmailVerified mirrors users.email_verified. The lockdown middleware gates
	// setup-incomplete accounts (EmailVerified=false, e.g. a freshly bootstrapped
	// Owner who has not yet proven control of their mailbox) to the setup-wizard
	// routes only, so an intercepted setup URL cannot yield full admin access
	// before the email-OTP verification step completes.
	EmailVerified bool
	// ViaSession is true when the principal was authenticated via a local session
	// cookie (SessionAuth), not a Cloudflare-Access JWT. The setup-lockdown gate
	// only applies to session-authenticated principals — a JWT caller already
	// passed Zero Trust at the edge, so the local-email-verification gate is not
	// the right boundary for them.
	ViaSession bool
}

// staffRole reports whether a stored user role carries staff standing: admin,
// or owner (the superset of admin, see IsAdmin). This is the single place the
// role set is spelled out — every staff gate (web IsAdmin, session grading,
// the in-game op-login approve and admin wake) routes through it.
func staffRole(role string) bool {
	return role == "admin" || role == "owner"
}

// IsAdmin reports whether the principal may perform admin-tier operations.
// Both the role claim and the admin Access path are required: a staff
// session arriving on panel.* must not bypass the Zero-Trust boundary.
// An owner implicitly passes this check (the owner role is a superset of admin).
func (p *Principal) IsAdmin() bool {
	return p != nil && staffRole(p.Role) && p.ViaAdminAccess
}

// IsOwner reports whether the principal holds the platform-level owner role
// — the single identity that may manage users, quotas, and sessions. Only the
// first staff account minted by break-glass carries this role; every subsequent
// Operator is a plain admin. Like IsAdmin, it requires the admin Access path.
func (p *Principal) IsOwner() bool {
	return p != nil && p.Role == "owner" && p.ViaAdminAccess
}

// InternalAuth authenticates the internal face (velocity / backend callbacks):
// a static service token presented as a Bearer credential. The internal face
// is never wrapped in Zero Trust (spec §1.8, §14 red line).
type InternalAuth interface {
	Authenticate(r *http.Request) error
}

// ExternalAuth authenticates the external face (people / panel) and returns the
// resolved Principal. Production verifies a Cloudflare Access JWT and checks its
// audience; the verification key source (JWKS) is injected so the audience and
// expiry logic stay unit-testable.
type ExternalAuth interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// BearerTokenAuth is the production InternalAuth: a constant-time comparison
// against the configured service token. A zero token fails closed so a
// misconfiguration can never silently disable internal-face auth.
type BearerTokenAuth struct {
	Token string
}

// Authenticate checks the Authorization: Bearer header against the token.
func (b BearerTokenAuth) Authenticate(r *http.Request) error {
	if b.Token == "" {
		return fmt.Errorf("internal auth not configured")
	}
	got := bearerToken(r)
	if got == "" {
		return fmt.Errorf("missing bearer token")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(b.Token)) != 1 {
		return fmt.Errorf("invalid service token")
	}
	return nil
}

// AccessVerifier is the production ExternalAuth: it parses a Cloudflare Access
// JWT, verifies the signature with the injected key function, and enforces the
// configured audience (spec §7 "验 aud"). AdminAudience, when set, marks a token
// minted for the admin.* application so admin-tier routes can require it.
type AccessVerifier struct {
	// Audience is the required `aud` claim for any external request.
	Audience string
	// AdminAudience, if non-empty and present in the token's aud set, flags the
	// principal as having passed the admin Zero-Trust path.
	AdminAudience string
	// Keyfunc resolves the signing key (production: a JWKS-backed keyfunc).
	Keyfunc jwt.Keyfunc
}

// accessClaims are the subset of Access JWT claims we consume.
type accessClaims struct {
	Email string `json:"email"`
	Role  string `json:"felis_role"`
	jwt.RegisteredClaims
}

// Authenticate verifies the Access JWT and maps it onto a Principal.
func (v AccessVerifier) Authenticate(r *http.Request) (*Principal, error) {
	if v.Keyfunc == nil {
		return nil, fmt.Errorf("external auth not configured")
	}
	raw := accessToken(r)
	if raw == "" {
		return nil, fmt.Errorf("missing access token")
	}

	var claims accessClaims
	parser := jwt.NewParser(jwt.WithExpirationRequired())
	if _, err := parser.ParseWithClaims(raw, &claims, v.Keyfunc); err != nil {
		return nil, fmt.Errorf("invalid access token: %w", err)
	}

	// Audience check: the configured app aud must be present. We do not delegate
	// to jwt.WithAudience so we can additionally detect the admin audience.
	if !audienceContains(claims.Audience, v.Audience) {
		return nil, fmt.Errorf("token audience does not include %q", v.Audience)
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("token missing subject")
	}

	role := claims.Role
	if role == "" {
		role = "user"
	}
	return &Principal{
		UserID:         claims.Subject,
		Email:          claims.Email,
		Role:           role,
		ViaAdminAccess: v.AdminAudience != "" && audienceContains(claims.Audience, v.AdminAudience),
	}, nil
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

// accessToken prefers the Cloudflare Access assertion header, falling back to a
// Bearer credential so the same verifier works behind a proxy or directly.
func accessToken(r *http.Request) string {
	if h := r.Header.Get("Cf-Access-Jwt-Assertion"); h != "" {
		return h
	}
	return bearerToken(r)
}

// audienceContains reports whether want appears in the aud claim set.
func audienceContains(aud jwt.ClaimStrings, want string) bool {
	for _, a := range aud {
		if a == want {
			return true
		}
	}
	return false
}
