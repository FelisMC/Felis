// Package registrygate authorizes registry writes by principal while allowing anonymous reads.
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

const (
	PrincipalPlatform = "platform"
	PrincipalBuild    = "build"
	PrincipalPrune    = "prune"
)

var Principals = []string{PrincipalPlatform, PrincipalBuild, PrincipalPrune}

// ReservedRepoRoots protects platform images and vulnerability mirrors from tenant builds.
var ReservedRepoRoots = []string{"felis", "mirror"}

const Realm = "felis-registry"

type Gate struct {
	// Missing or empty tokens disable authentication for that principal.
	Tokens   map[string]string
	Upstream *url.URL
	Log      *slog.Logger
	// Empty DataDir disables the manifest index.
	DataDir string

	proxy  *httputil.ReverseProxy
	health *http.Client
	maint  maintenance
}

func New(upstream *url.URL, tokens map[string]string, log *slog.Logger) *Gate {
	g := &Gate{Tokens: tokens, Upstream: upstream, Log: log}
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(upstream)
		// Preserve the client Host that upload Location URLs are built from.
		pr.Out.Host = pr.In.Host
		pr.SetXForwarded()
		// Strip credentials before forwarding to the unauthenticated registry.
		pr.Out.Header.Del("Authorization")
	}}
	// Flush streamed blobs without buffering entire layers.
	rp.FlushInterval = -1
	g.proxy = rp
	g.health = &http.Client{Timeout: 3 * time.Second}
	g.maint.now = time.Now
	g.maint.quiet = DefaultQuiet
	// Treat startup as a write because a push may have been interrupted by the restart.
	g.maint.lastWrite = g.maint.now()
	return g
}

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
	// Reject paths the gate and registry could decode or normalize differently.
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
		// Challenge at /v2/ so Docker-compatible clients send credentials on subsequent writes.
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
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
			"the registry is read-only while garbage collection runs; retry shortly")
		return
	}
	defer g.maint.endWrite()
	g.proxy.ServeHTTP(w, r)
}

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
		// Perform a comparison even for unknown users to reduce timing differences.
		subtle.ConstantTimeCompare([]byte(pass), []byte("x"))
		return "", fmt.Errorf("unknown principal")
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(want)) != 1 {
		return "", fmt.Errorf("bad secret")
	}
	return user, nil
}

// Authorize returns an empty string to allow a write, otherwise a refusal reason.
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
		// Prune credentials may unlink manifests only; blob removal belongs to GC.
		if method != http.MethodDelete || !isManifestDigestPath(path) {
			return "the prune principal may only delete a manifest by digest"
		}
		return ""
	default:
		return "unknown principal"
	}
}

func isManifestDigestPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return false
	}
	seg := strings.Split(rest, "/")
	n := len(seg)
	return n >= 3 && seg[n-2] == "manifests" && digestRE.MatchString(seg[n-1])
}

// RepoFromPath strips the fixed registry API suffix; references, digests, and upload IDs cannot contain slashes.
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

// canonicalPath rejects encoded, dot, and empty segments that upstream could interpret differently.
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

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": msg}},
	})
}
