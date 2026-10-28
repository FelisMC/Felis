package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Principal identifies a person authenticated on the external API; service tokens do not produce one.
type Principal struct {
	UserID   string
	Username string
	// Email is untrusted unless EmailVerified is true.
	Email string
	Role  string
	// ViaAdminAccess requires arrival on the operator console host, in addition to a staff role.
	ViaAdminAccess bool
	// EmailVerified=false participates in setup lockdown; passkey enrollment completes setup.
	EmailVerified bool
	ViaSession    bool
	// ReauthAt is zero until the session proves an account factor.
	ReauthAt time.Time
	// APIKey is the displayed prefix of the API key that authenticated the request.
	APIKey string
}

func staffRole(role string) bool {
	return role == "admin" || role == "owner"
}

func (p *Principal) IsAdmin() bool {
	return p != nil && staffRole(p.Role) && p.ViaAdminAccess
}

func (p *Principal) IsOwner() bool {
	return p != nil && p.Role == "owner" && p.ViaAdminAccess
}

// Caller scopes an internal service token to the routes allowed for that machine.
type Caller string

const (
	CallerVelocity Caller = "velocity"
	CallerLimbo    Caller = "limbo"
	CallerBuild    Caller = "build"
	CallerOps      Caller = "ops"
)

// InternalAuth authenticates per-caller Bearer tokens without Cloudflare Access.
type InternalAuth interface {
	Authenticate(r *http.Request) (Caller, error)
}

// ExternalAuth resolves identity and role from the users table; Cloudflare Access is enforced at the edge.
type ExternalAuth interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// CallerTokens rejects callers with missing tokens.
type CallerTokens map[Caller]string

// NewCallerTokens rejects shared token values so each credential identifies one caller.
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

// Authenticate compares every token to avoid revealing the matching caller through early return timing.
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

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}
