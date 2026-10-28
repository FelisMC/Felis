package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

// withRequestID validates inbound IDs before echoing them or persisting them in audit logs.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

const maxRequestIDLen = 64

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// withRecover logs panic details under the request ID and returns an opaque 500.
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					// Let net/http abort an already-committed response instead of replacing it with a JSON error.
					panic(rec)
				}
				log.Printf("api: %s %s: panic (request_id=%s): %v\n%s",
					r.Method, r.URL.Path, requestIDFromContext(r.Context()), rec, debug.Stack())
				writeError(w, r, newError(http.StatusInternalServerError, "panic", "internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if viaTLSEdge(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// viaTLSEdge uses X-Forwarded-Proto; origin TLS does not identify the browser-to-edge scheme.
func viaTLSEdge(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// rejectCrossSiteWrites also rejects sibling-subdomain writes, which SameSite=Lax cookies allow.
func rejectCrossSiteWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !keyOnlyRequest(r) && !sameOriginRequest(r) {
				writeError(w, r, newError(http.StatusForbidden, "cross_site",
					"requests from another site may not change anything here"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameOriginRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// Body deadlines grow with received bytes at bodyMinRate so slow uploads are bounded without capping streaming response time.
var (
	bodyGrace   = 30 * time.Second
	bodyMinRate = float64(16 << 10) // bytes per second
)

func withBodyDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		start := time.Now()
		if err := rc.SetReadDeadline(start.Add(bodyGrace)); err != nil {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc, start: start}
		next.ServeHTTP(w, r)
	})
}

// deadlineBody clears its deadline at EOF so body timeouts cannot cancel later handler work.
type deadlineBody struct {
	io.ReadCloser
	rc    *http.ResponseController
	start time.Time
	read  int64
	done  bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	switch {
	case b.done:
	case err != nil:
		b.done = true
		_ = b.rc.SetReadDeadline(time.Time{})
	case n > 0:
		allowed := bodyGrace + time.Duration(float64(b.read)/bodyMinRate*float64(time.Second))
		_ = b.rc.SetReadDeadline(b.start.Add(allowed))
	}
	return n, err
}

// takeBodyDeadline transfers deadline ownership to handlers whose body consumption follows the browser's pace.
func takeBodyDeadline(w http.ResponseWriter, r *http.Request) *http.ResponseController {
	if b, ok := r.Body.(*deadlineBody); ok {
		r.Body = b.ReadCloser
		_ = b.rc.SetReadDeadline(time.Time{})
		return b.rc
	}
	return http.NewResponseController(w)
}

// requireInternal records the authenticated service caller for route-specific caller checks.
func (a *API) requireInternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := a.Internal.Authenticate(r)
		if err != nil {
			writeError(w, r, errUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyCaller, caller)))
	})
}

func callersOnly(callers []Caller, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := callerFromContext(r.Context())
		for _, c := range callers {
			if c == caller {
				next(w, r)
				return
			}
		}
		writeError(w, r, errWrongCaller)
	}
}

func (a *API) requireExternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.External.Authenticate(r)
		if errors.Is(err, errAuthBackend) {
			writeError(w, r, errAuthUnavailable)
			return
		}
		if err != nil || p == nil {
			writeError(w, r, errUnauthorized)
			return
		}
		// Reject non-staff on the operator host even when their player session is valid.
		if hostIsAdminConsole(r, a.RootDomain, a.AdminHostname) && !p.IsAdmin() {
			writeError(w, r, errForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyPrincipal, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := principalFromContext(r.Context()); !p.IsAdmin() {
			writeError(w, r, errForbidden)
			return
		}
		next(w, r)
	}
}

func (a *API) ownerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := principalFromContext(r.Context()); !p.IsOwner() {
			writeError(w, r, errForbidden)
			return
		}
		next(w, r)
	}
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}
