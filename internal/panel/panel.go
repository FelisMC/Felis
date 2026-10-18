// Package panel serves the embedded Felis control panel next to the external API.
package panel

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
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
	Distributed   bool   `json:"distributed,omitempty"`
	ControllerHA  bool   `json:"controllerHA,omitempty"`
	APIBase       string `json:"apiBase"`
	RootDomain    string `json:"rootDomain"`
	PanelHostname string `json:"panelHostname,omitempty"`
	AdminHostname string `json:"adminHostname,omitempty"`
	// GamePort is omitted for the default port 25565.
	GamePort    int       `json:"gamePort,omitempty"`
	GameVersion string    `json:"gameVersion,omitempty"`
	Build       buildInfo `json:"build"`
}

type buildInfo struct {
	Version string `json:"version"`
	Release string `json:"release"`
	// Commit is empty for a clean release build.
	Commit string `json:"commit,omitempty"`
	Dev    bool   `json:"dev"`
}

// Development stamps use build metadata (+gSHA), which does not lower semver precedence below the release.
var devSuffix = regexp.MustCompile(`\+g([0-9a-f]+)$`)

// Accept git-describe suffixes while preserving hyphens in prerelease tags.
var describeSuffix = regexp.MustCompile(`-([0-9]+)-g([0-9a-f]+)$`)

var releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

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

// Handler serves the SPA and exposes build, host, and game-port configuration through /config.json.
func Handler(api http.Handler, rootDomain, panelHost, adminHost string, gamePort int, gameVersion, version string, topology ...Topology) http.Handler {
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	var shape Topology
	if len(topology) > 0 {
		shape = topology[0]
	}
	return &handler{
		api:           api,
		topology:      shape,
		rootDomain:    rootDomain,
		panelHostname: panelHost,
		adminHostname: adminHost,
		gamePort:      publicGamePort(gamePort),
		gameVersion:   gameVersion,
		build:         parseBuildVersion(version),
		files:         files,
		fileServer:    http.FileServer(http.FS(files)),
		csp:           contentSecurityPolicy(files),
	}
}

const defaultGamePort = 25565

func publicGamePort(p int) int {
	if p == defaultGamePort {
		return 0
	}
	return p
}

type Topology struct {
	Distributed  bool
	ControllerHA bool
}

type handler struct {
	topology      Topology
	api           http.Handler
	rootDomain    string
	panelHostname string
	adminHostname string
	gamePort      int
	gameVersion   string
	build         buildInfo
	files         fs.FS
	fileServer    http.Handler
	csp           string
}

var inlineScript = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// Hash built inline scripts for CSP; inline styles support the UI library, and data/blob images support QR codes.
func contentSecurityPolicy(files fs.FS) string {
	scripts := "'self'"
	if index, err := fs.ReadFile(files, "index.html"); err == nil {
		for _, m := range inlineScript.FindAllSubmatch(index, -1) {
			sum := sha256.Sum256(m[1])
			scripts += " 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + scripts,
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// setPageHeaders prevents framing, MIME sniffing, and setup-token referrer leaks.
func (h *handler) setPageHeaders(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Content-Security-Policy", h.csp)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	// Trust the edge scheme; direct self-signed connections cannot establish HSTS (RFC 6797).
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		hdr.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || strings.HasPrefix(r.URL.Path, "/api/") {
		h.api.ServeHTTP(w, r)
		return
	}
	h.setPageHeaders(w, r)
	if guardInAppWebView(w, r) {
		return
	}
	switch {
	case r.URL.Path == "/config.json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(runtimeConfig{
			APIBase:       "/api/v1",
			Distributed:   h.topology.Distributed,
			ControllerHA:  h.topology.ControllerHA,
			RootDomain:    h.rootDomain,
			PanelHostname: h.panelHostname,
			AdminHostname: h.adminHostname,
			GamePort:      h.gamePort,
			GameVersion:   h.gameVersion,
			Build:         h.build,
		})
	case h.hasStaticFile(r.URL.Path):
		// Content-hashed assets are immutable; other responses revalidate.
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.fileServer.ServeHTTP(w, r)
	default:
		w.Header().Set("Cache-Control", "no-cache")
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
