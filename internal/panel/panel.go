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
	"strings"
)

//go:embed static
var static embed.FS

type runtimeConfig struct {
	APIBase    string `json:"apiBase"`
	RootDomain string `json:"rootDomain"`
}

// Handler wraps the external API handler with the panel SPA.
func Handler(api http.Handler, rootDomain string) http.Handler {
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return &handler{
		api:        api,
		rootDomain: rootDomain,
		files:      files,
		fileServer: http.FileServer(http.FS(files)),
	}
}

type handler struct {
	api        http.Handler
	rootDomain string
	files      fs.FS
	fileServer http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || strings.HasPrefix(r.URL.Path, "/api/"):
		h.api.ServeHTTP(w, r)
	case r.URL.Path == "/config.json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(runtimeConfig{APIBase: "/api/v1", RootDomain: h.rootDomain})
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
