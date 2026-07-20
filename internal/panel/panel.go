// Package panel serves the embedded Felis control panel next to the external API.
package panel

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

//go:embed static
var static embed.FS

type runtimeConfig struct {
	APIBase       string    `json:"apiBase"`
	RootDomain    string    `json:"rootDomain"`
	PanelHostname string    `json:"panelHostname,omitempty"`
	AdminHostname string    `json:"adminHostname,omitempty"`
	Build         buildInfo `json:"build"`
}

// buildInfo is the resolved build stamp the panel renders in its version badge.
// It is derived once, server-side, from the binary's `main.version` — the stamp
// deploy/bootstrap.sh links in per install channel — so the panel needs no brittle
// string parsing — it just renders `Release`, appending `+Commit` when `Dev`.
type buildInfo struct {
	// Version is the raw resolved stamp (e.g. "v1.0.0-earlyAccess+g1a2b3c4").
	Version string `json:"version"`
	// Release is the "big version" — the newest tag with any commit suffix
	// stripped (e.g. "v1.0.0-earlyAccess"). It is what the release channel shows.
	Release string `json:"release"`
	// Commit is the short commit the dev channel was built from (e.g. "1a2b3c4"),
	// empty for a clean release build.
	Commit string `json:"commit,omitempty"`
	// Dev is true for a non-release build — the dev channel (main past the tag), an
	// untagged/bare-SHA build, a dirty tree, or an un-stamped local `go build`.
	Dev bool `json:"dev"`
}

// devSuffix matches the trailing "+g<sha>" that deploy/bootstrap.sh's dev channel
// appends to the newest tag — the shape a dev build actually carries. The "+" is
// deliberate and load-bearing: it is semver BUILD METADATA, ignored for ordering,
// so a dev build ahead of v1.2.3 still compares as newer than v1.2.3. The
// git-describe form below puts that distance in the PRERELEASE field instead,
// which sorts BELOW the bare tag.
var devSuffix = regexp.MustCompile(`\+g([0-9a-f]+)$`)

// describeSuffix matches the trailing "-<commits>-g<sha>" that `git describe`
// appends to the newest tag once HEAD is past it. bootstrap.sh no longer produces
// this form, but a hand-rolled `go build -ldflags "-X main.version=$(git describe)"`
// still does, and parsing it costs one case. Match is anchored at end so a tag whose
// prerelease part itself contains hyphens (v1.0.0-earlyAccess) keeps that part in
// Release.
var describeSuffix = regexp.MustCompile(`-([0-9]+)-g([0-9a-f]+)$`)

// releaseTag matches a clean released semantic-version tag (vMAJOR.MINOR.PATCH
// with an optional -prerelease), i.e. the name the release channel builds. It is
// permissive on the prerelease so tags like v1.0.0-earlyAccess qualify.
var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// parseBuildVersion splits a build stamp into the fields the panel version badge
// renders. Cases: a dev stamp "<tag>+gSHA" (or the git-describe "<tag>-N-gSHA") →
// Release=<tag>, Commit=SHA, Dev=true; a clean release tag "vX.Y.Z[-pre]" →
// Release=tag, Dev=false; anything else ("dev", "unknown", a bare short SHA, a
// dirty tree) → best-effort Release with Dev=true.
func parseBuildVersion(raw string) buildInfo {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "dev"
	}
	bi := buildInfo{Version: raw}
	core := strings.TrimSuffix(raw, "-dirty")
	dirty := core != raw

	switch {
	case devSuffix.MatchString(core):
		m := devSuffix.FindStringSubmatch(core)
		bi.Release = core[:len(core)-len(m[0])]
		bi.Commit = m[1]
		bi.Dev = true
	case describeSuffix.MatchString(core):
		m := describeSuffix.FindStringSubmatch(core)
		bi.Release = core[:len(core)-len(m[0])]
		bi.Commit = m[2]
		bi.Dev = true
	case releaseTag.MatchString(core) && !dirty:
		bi.Release = core
		bi.Dev = false
	default:
		bi.Release = core
		bi.Dev = true
	}
	return bi
}

// Handler wraps the external API handler with the panel SPA. version is the
// binary's resolved build stamp (cmd/felis resolvedVersion()); it is surfaced to
// the SPA via /config.json for the version badge. panelHost and adminHost are the
// configured console.<root_domain> and op.console.<root_domain> hostnames (either
// may be empty when that face is not deployed); they let the SPA detect which home
// it is being served from by comparing location.host, so one bundle can render the
// right surface (player console vs SysAdmin console) without a rebuild.
func Handler(api http.Handler, rootDomain, panelHost, adminHost, version string) http.Handler {
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return &handler{
		api:           api,
		rootDomain:    rootDomain,
		panelHostname: panelHost,
		adminHostname: adminHost,
		build:         parseBuildVersion(version),
		files:         files,
		fileServer:    http.FileServer(http.FS(files)),
	}
}

type handler struct {
	api           http.Handler
	rootDomain    string
	panelHostname string
	adminHostname string
	build         buildInfo
	files         fs.FS
	fileServer    http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// WeChat/QQ in-app browsers cannot run WebAuthn, so steer their document
	// navigations to a "open in your system browser" interstitial before the SPA
	// (which is built around passkey enrollment) ever loads. See webview.go.
	if guardInAppWebView(w, r) {
		return
	}
	switch {
	case r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || strings.HasPrefix(r.URL.Path, "/api/"):
		h.api.ServeHTTP(w, r)
	case r.URL.Path == "/config.json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(runtimeConfig{
			APIBase:       "/api/v1",
			RootDomain:    h.rootDomain,
			PanelHostname: h.panelHostname,
			AdminHostname: h.adminHostname,
			Build:         h.build,
		})
	case h.hasStaticFile(r.URL.Path):
		h.fileServer.ServeHTTP(w, r)
	default:
		h.serveIndex(w, r)
	}
}

func (h *handler) hasStaticFile(urlPath string) bool {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" {
		name = "index.html"
	}
	info, err := fs.Stat(h.files, name)
	return err == nil && !info.IsDir()
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := h.files.Open("index.html")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "panel assets missing", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "open panel index", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "stat panel index", http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "read panel index", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, "index.html", info.ModTime(), bytes.NewReader(body))
}
