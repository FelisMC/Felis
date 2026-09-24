// Package registrygate is the write-authorization front of the in-cluster image
// registry. registry:2 runs with no auth of its own and listens on the pod's
// loopback only; this gate owns the registry port and forwards to it.
//
// Why a gate rather than registry:2's own htpasswd auth: htpasswd is all-or-nothing
// (every principal may write every repository, and anonymous pulls stop working),
// while the platform needs two distinct writers and anonymous reads:
//
//   - reads (GET/HEAD) stay anonymous, because the node's containerd pulls through
//     the loopback hostPort, Kaniko pulls FROM images and Trivy pulls its DB mirror,
//     and none of them should carry a credential;
//   - the "platform" principal (the installer) may write anything;
//   - the "build" principal (the push step of a build Job) may write any repository
//     outside the platform-reserved ones (felis/…, mirror/…), and may not delete;
//   - the "prune" principal (felis-api's registry pruner) may only delete a
//     manifest by digest, the one write that frees space.
//
// The gate also holds the registry read-only while the GC sidecar runs registry
// garbage-collect (maint.go): a blob pushed during the sweep could be deleted
// under a manifest that is about to reference it.
//
// Before this gate any pod that could reach the registry — a game server running a
// tenant's plugin, or a Dockerfile RUN step inside Kaniko — could overwrite
// felis/felis and take over the control plane on its next pull.
package registrygate

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Principal names. They are the basic-auth usernames and the file names under the
// gate's auth directory (cmd/felis registry-gate --auth-dir).
const (
	PrincipalPlatform = "platform"
	PrincipalBuild    = "build"
	PrincipalPrune    = "prune"
)

// Principals lists every principal the gate knows, for loading their tokens.
var Principals = []string{PrincipalPlatform, PrincipalBuild, PrincipalPrune}

// ReservedRepoRoots are the first path components the build principal may never
// write: felis/ holds the control-plane and game images the platform runs, mirror/
// holds the Trivy DB mirrors the scan gate trusts. A build that could overwrite
// either would own the platform or blind its own scanner.
var ReservedRepoRoots = []string{"felis", "mirror"}

// Realm is the basic-auth realm the gate challenges with.
const Realm = "felis-registry"

// Gate authorizes registry requests and proxies the allowed ones upstream.
type Gate struct {
	// Tokens maps a principal to its secret. A principal with an empty or missing
	// token cannot authenticate: writes fail closed while reads keep working.
	Tokens map[string]string
	// Upstream is the loopback registry, e.g. http://127.0.0.1:5001.
	Upstream *url.URL
	// Log receives one line per refused write. Nil discards.
	Log *slog.Logger
	// DataDir is the registry's storage root, mounted read-only, for the manifest
	// index (index.go). Empty turns the index off.
	DataDir string

	proxy  *httputil.ReverseProxy
	health *http.Client
	maint  maintenance
}

// New builds a Gate for upstream.
func New(upstream *url.URL, tokens map[string]string, log *slog.Logger) *Gate {
	g := &Gate{Tokens: tokens, Upstream: upstream, Log: log}
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(upstream)
		// The registry builds upload Location URLs from Host: keep the one the client
		// dialled, not the loopback upstream.
		pr.Out.Host = pr.In.Host
		pr.SetXForwarded()
		// The registry has no auth of its own; the credential stops here.
		pr.Out.Header.Del("Authorization")
	}}
	// Blob uploads and pulls are streamed; flush as bytes arrive so a large layer
	// pull is not buffered in the gate.
	rp.FlushInterval = -1
	g.proxy = rp
	g.health = &http.Client{Timeout: 3 * time.Second}
	g.maint.now = time.Now
	g.maint.quiet = DefaultQuiet
	// A gate that just started cannot tell whether a push was mid-way through the
	// previous one, and the installer pushes right after the registry rolls out:
	// count the start as a write, so the first window waits for quiet too.
	g.maint.lastWrite = g.maint.now()
	return g
}

// ServeHTTP implements http.Handler.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/livez":
		w.WriteHeader(http.StatusOK)
		return
	case "/healthz":
		g.serveHealth(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, IndexPathPrefix) {
		g.serveIndex(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v2/") && r.URL.Path != "/v2" {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "not a registry API path")
		return
	}
	// The gate and the registry must read the same path, or an authorization check
	// on one repository could be spent on another. Refuse every shape that a later
	// clean-up or decode could turn into a different path.
	if !canonicalPath(r) {
		writeError(w, http.StatusBadRequest, "NAME_INVALID", "non-canonical request path")
		return
	}

	principal, authErr := g.authenticate(r)
	if authErr != nil {
		challenge(w, "invalid credentials")
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// The API root is where Docker-compatible clients learn which auth scheme
		// the registry wants; the daemon sends credentials on later writes only if
		// this answer challenged it. Everything else stays anonymous for reads.
		if isAPIRoot(r.URL.Path) && principal == "" {
			challenge(w, "authentication required")
			return
		}
		g.proxy.ServeHTTP(w, r)
		return
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}

	if principal == "" {
		challenge(w, "authentication required")
		return
	}
	if reason := Authorize(principal, r.Method, r.URL.Path); reason != "" {
		if g.Log != nil {
			g.Log.Warn("registry write refused", "principal", principal, "method", r.Method, "path", r.URL.Path, "reason", reason)
		}
		writeError(w, http.StatusForbidden, "DENIED", reason)
		return
	}
	if !g.maint.beginWrite() {
		// Retry-After is what imagepush and docker push back off on; a GC sweep
		// over a few GiB takes well under a minute.
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"the registry is read-only while garbage collection runs; retry shortly")
		return
	}
	defer g.maint.endWrite()
	g.proxy.ServeHTTP(w, r)
}

// authenticate returns the principal the request's basic credentials name, "" for
// an anonymous request, and an error for credentials that do not verify.
func (g *Gate) authenticate(r *http.Request) (string, error) {
	if r.Header.Get("Authorization") == "" {
		return "", nil
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		return "", fmt.Errorf("malformed authorization")
	}
	want, known := g.Tokens[user]
	if !known || want == "" {
		// Still spend a comparison so an unknown user is not faster to refuse.
		subtle.ConstantTimeCompare([]byte(pass), []byte("x"))
		return "", fmt.Errorf("unknown principal")
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(want)) != 1 {
		return "", fmt.Errorf("bad secret")
	}
	return user, nil
}

// Authorize decides whether an authenticated principal may send a write request
// for path. It returns "" to allow, or the refusal reason.
func Authorize(principal, method, path string) string {
	switch principal {
	case PrincipalPlatform:
		return ""
	case PrincipalBuild:
		if method == http.MethodDelete {
			return "the build principal may not delete"
		}
		repo := RepoFromPath(path)
		if repo == "" {
			return "the build principal may only write to a repository"
		}
		root, _, _ := strings.Cut(repo, "/")
		for _, reserved := range ReservedRepoRoots {
			if root == reserved {
				return fmt.Sprintf("repository %s/ is reserved for the platform", reserved)
			}
		}
		return ""
	case PrincipalPrune:
		// Deleting a manifest only unlinks it; the blobs go at the next GC. The
		// pruner never needs anything else, so a leaked prune token can neither
		// plant an image nor delete a blob a live manifest still references.
		if method != http.MethodDelete || !isManifestDigestPath(path) {
			return "the prune principal may only delete a manifest by digest"
		}
		return ""
	default:
		return "unknown principal"
	}
}

// isManifestDigestPath reports whether path is /v2/<repo>/manifests/sha256:<hex>.
func isManifestDigestPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return false
	}
	seg := strings.Split(rest, "/")
	n := len(seg)
	return n >= 3 && seg[n-2] == "manifests" && digestRE.MatchString(seg[n-1])
}

// RepoFromPath extracts the repository name from a registry API v2 path, or ""
// when the path addresses no repository (/v2/, /v2/_catalog). The shapes are the
// distribution API's: <name>/manifests/<ref>, <name>/blobs/<digest>,
// <name>/blobs/uploads/[<uuid>], <name>/tags/list, <name>/referrers/<digest>.
// Neither a reference, a digest nor an upload id contains '/', so the repository
// is everything before the fixed tail.
func RepoFromPath(path string) string {
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return ""
	}
	seg := strings.Split(rest, "/")
	n := len(seg)
	switch {
	case n >= 4 && seg[n-3] == "blobs" && seg[n-2] == "uploads":
		return strings.Join(seg[:n-3], "/")
	case n >= 3 && (seg[n-2] == "manifests" || seg[n-2] == "blobs" || seg[n-2] == "tags" || seg[n-2] == "referrers"):
		return strings.Join(seg[:n-2], "/")
	}
	return ""
}

// canonicalPath rejects any request path the upstream could read differently from
// the gate: percent-escapes Go would decode (RawPath set), dot segments a router
// would clean, and empty segments other than the trailing slash of an upload POST.
func canonicalPath(r *http.Request) bool {
	if r.URL.RawPath != "" && r.URL.RawPath != r.URL.Path {
		return false
	}
	p := r.URL.Path
	if strings.ContainsAny(p, "%\\") {
		return false
	}
	seg := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range seg {
		if s == "." || s == ".." {
			return false
		}
		if s == "" && i != len(seg)-1 {
			return false
		}
	}
	return true
}

func isAPIRoot(p string) bool { return p == "/v2/" || p == "/v2" }

// serveHealth answers 200 when the upstream registry answers its API root, the
// check registry:2's own probes used before the gate took its port.
func (g *Gate) serveHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Upstream.JoinPath("/v2/").String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	resp, err := g.health.Do(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("upstream answered %d", resp.StatusCode), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func challenge(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="`+Realm+`"`)
	writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", msg)
}

// writeError answers in the registry's own error envelope, which Docker and
// containerd both surface verbatim to the operator.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": msg}},
	})
}
