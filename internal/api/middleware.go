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

// withRequestID assigns a request id (honoring a WELL-FORMED inbound X-Request-Id)
// and echoes it on the response and into the context for the error envelope.
//
// A caller-supplied id is honored for cross-service tracing, but only after
// validation: the id is echoed to the client, embedded in the error envelope, AND
// persisted verbatim into audit_logs.request_id, so an unvalidated one is an
// audit-integrity vector — an arbitrarily long value bloats the audit row and a
// stray control byte could smuggle a forged line into a log sink. A rejected id is
// replaced with a fresh server-minted one: that one request loses its inbound trace
// link, which is strictly better than storing attacker-controlled text.
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

// maxRequestIDLen caps an inbound X-Request-Id we are willing to echo and persist.
// 64 characters comfortably fits a UUID or a typical distributed-trace id while
// bounding what reaches audit_logs.request_id.
const maxRequestIDLen = 64

// validRequestID reports whether an inbound X-Request-Id is safe to echo and store:
// non-empty, within maxRequestIDLen, and restricted to an unambiguous, log-safe
// charset (ASCII alphanumerics plus '-', '_', '.'). The byte-length check bounds it
// regardless of encoding, and the charset excludes whitespace, CR/LF, and every
// other control or multibyte rune, so nothing that survives can pollute a log line
// or the audit row.
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

// withRecover turns a panicking handler into a 500 envelope instead of a
// dropped connection.
//
// The client gets an opaque "internal error", but the panic value and stack are
// logged FIRST, keyed by the same request_id — the same contract writeError keeps
// for unmapped errors (see errors.go). Without it a recovered panic is an
// untraceable 500: an operator holding "internal error" has nothing to grep for,
// and diagnosis degrades into guessing against a live install.
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					// A deliberate abort of a committed response (see
					// streamSubmissionContext): let net/http cut the connection.
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

// withSecurityHeaders sets the response headers every API answer carries. The
// API serves JSON, event streams and downloads, never a page, so its CSP allows
// nothing and no frame may embed it. HSTS goes out on what the Cloudflare edge
// served over HTTPS (viaTLSEdge).
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

// viaTLSEdge reports a request the Cloudflare edge took over HTTPS. The tunnel
// reaches the origin over TLS as well (the self-signed NodePort listener), so
// r.TLS says nothing about what the browser saw; the edge's X-Forwarded-Proto
// does. A browser that comes straight to the self-signed listener sends no such
// header, and would drop the policy anyway: HSTS that arrives over a certificate
// error, or for an IP-literal host, is ignored (RFC 6797 §8.1).
func viaTLSEdge(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// rejectCrossSiteWrites refuses a state-changing request a browser sent from
// another site. The session cookie is SameSite=Lax, which keeps it off cross-site
// POSTs but still sends it from a sibling subdomain (same-site), and a player's
// server can serve pages under the install's root domain. Browsers say where a
// request came from in Sec-Fetch-Site; one too old for that still sends Origin on
// a POST. A request with neither is not from a browser (the plugins, the CLI) and
// carries no ambient cookie to abuse.
func rejectCrossSiteWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !sameOriginRequest(r) {
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
	default: // same-site, cross-site
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// Request bodies get a read deadline that grows with what has arrived: bodyGrace
// to get going, plus the time the bytes so far would take at bodyMinRate. A JSON
// body (1 MiB at most) has well under a minute; a 1 GiB build context uploads as
// long as it keeps averaging bodyMinRate; a client trickling a byte now and then
// to hold a connection and a goroutine is cut off. The server sets no ReadTimeout
// (it would cut the long SSE responses), so without this nothing bounded the body.
// Vars only so a test can shrink them.
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
			next.ServeHTTP(w, r) // a writer without deadlines (tests)
			return
		}
		r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc, start: start}
		next.ServeHTTP(w, r)
	})
}

// deadlineBody moves the connection's read deadline as the body arrives, and
// clears it once the body ends: from then on net/http reads the connection only
// to notice a disconnect, and a deadline meant for the body would cancel the
// request's context under a handler that is still working.
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

// requireInternal enforces service-token auth for the internal face. It never
// applies Zero Trust (spec §14 red line).
func (a *API) requireInternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.Internal.Authenticate(r); err != nil {
			writeError(w, r, errUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireExternal enforces Access-JWT auth for the external face and stashes the
// resolved Principal in the request context.
func (a *API) requireExternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.External.Authenticate(r)
		if errors.Is(err, errAuthBackend) {
			// Session store unreachable — an outage, not a missing credential.
			writeError(w, r, errAuthUnavailable)
			return
		}
		if err != nil || p == nil {
			writeError(w, r, errUnauthorized)
			return
		}
		// op.console door gate: the operator console is staff-only, so a request that
		// arrives on the admin host from a non-admin principal is refused HERE, before
		// any handler. Authentication alone (a player's passkey/email/bind session) is
		// not access — internal permission is verified on top of it, so possessing a
		// valid credential never "lets you in" to op.console. On the player console
		// (console.<root_domain>) hostIsAdminConsole is false, so this is inert; in
		// production Cloudflare Access already blocks non-staff at the edge and this is
		// the defense-in-depth backstop for the passwordless (no-Zero-Trust) face.
		if hostIsAdminConsole(r, a.RootDomain, a.AdminHostname) && !p.IsAdmin() {
			writeError(w, r, errForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyPrincipal, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// adminOnly gates an external-face handler on the admin Zero-Trust path. The
// Access middleware has already authenticated; this enforces that admin-tier
// operations both carry a staff role (admin or owner) AND arrived via admin.*
// (spec §14).
func (a *API) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := principalFromContext(r.Context()); !p.IsAdmin() {
			writeError(w, r, errForbidden)
			return
		}
		next(w, r)
	}
}

// ownerOnly gates a handler on the owner role — the single platform-level
// identity above admin. It is stricter than adminOnly: a plain admin with
// role=admin and valid admin Access path is still refused here. The owner
// arrives through the same admin Zero-Trust path, so adminOnly is not a
// prerequisite (the two guards are orthogonal).
func (a *API) ownerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := principalFromContext(r.Context()); !p.IsOwner() {
			writeError(w, r, errForbidden)
			return
		}
		next(w, r)
	}
}

// newRequestID returns a short random hex id. crypto/rand never fails on the
// platforms we target; on the impossible error path we fall back to a constant
// so a request still gets a (non-unique) id rather than crashing.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}
