package software

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeUpstream answers the resolver's requests with responses captured from
// the real hosts (testdata/, fetched 2026-10-16), keyed by host and path, so
// every value a test expects is one the upstream itself published. A request
// it has no capture for is a 404, which is how the real hosts answer a version
// or build they do not have.
type fakeUpstream struct {
	t      *testing.T
	routes map[string]string // "host/path" -> testdata file
	mu     sync.Mutex
	hits   []string
	down   bool
	// override replaces a route's body, for tampering tests.
	override map[string][]byte
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{t: t, override: map[string][]byte{}, routes: map[string]string{
		"piston-meta.mojang.com/mc/game/version_manifest_v2.json": "mojang/manifest.json",

		"fill.papermc.io/v3/projects/paper":                            "fill/project.json",
		"fill.papermc.io/v3/projects/paper/versions/1.21.4":            "fill/1.21.4.json",
		"fill.papermc.io/v3/projects/paper/versions/1.12.2":            "fill/1.12.2.json",
		"fill.papermc.io/v3/projects/paper/versions/1.21.4/builds":     "fill/1.21.4-builds.json",
		"fill.papermc.io/v3/projects/paper/versions/1.21.4/builds/232": "fill/1.21.4-232.json",

		"api.purpurmc.org/v2/purpur":             "purpur/project.json",
		"api.purpurmc.org/v2/purpur/1.21.4":      "purpur/1.21.4.json",
		"api.purpurmc.org/v2/purpur/1.21.4/2416": "purpur/1.21.4-2416.json",

		"meta.fabricmc.net/v2/versions/game":                                                   "fabric/game.json",
		"meta.fabricmc.net/v2/versions/loader/1.21.4":                                          "fabric/loader-1.21.4.json",
		"meta.fabricmc.net/v2/versions/loader/1.21.4/0.19.5/server/json":                       "fabric/server-1.21.4-0.19.5.json",
		"maven.fabricmc.net/net/fabricmc/fabric-loader/0.19.5/fabric-loader-0.19.5.jar.sha256": "maven/fabric-loader-0.19.5.jar.sha256",
		"maven.fabricmc.net/net/fabricmc/intermediary/1.21.4/intermediary-1.21.4.jar.sha256":   "maven/intermediary-1.21.4.jar.sha256",

		"meta.quiltmc.org/v3/versions/loader/1.20.1": "quilt/loader-1.20.1.json",

		"files.minecraftforge.net/net/minecraftforge/forge/maven-metadata.json":                                      "forge/maven-metadata.json",
		"files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json":                                     "forge/promotions_slim.json",
		"maven.minecraftforge.net/net/minecraftforge/forge/1.20.1-47.4.10/forge-1.20.1-47.4.10-installer.jar.sha256": "maven/forge-1.20.1-47.4.10-installer.jar.sha256",

		"maven.neoforged.net/api/maven/versions/releases/net/neoforged/neoforge":                              "neoforge/versions.json",
		"maven.neoforged.net/releases/net/neoforged/neoforge/21.4.150/neoforge-21.4.150-installer.jar.sha256": "maven/neoforge-21.4.150-installer.jar.sha256",
	}}
	// Each version JSON lives at the URL the manifest names for it.
	var m struct {
		Versions []struct{ ID, URL string } `json:"versions"`
	}
	if err := json.Unmarshal(f.read("mojang/manifest.json"), &m); err != nil {
		t.Fatal(err)
	}
	for _, v := range m.Versions {
		if _, err := os.Stat(filepath.Join("testdata", "mojang", v.ID+".json")); err == nil {
			u, _ := url.Parse(v.URL)
			f.routes[u.Host+u.Path] = "mojang/" + v.ID + ".json"
		}
	}
	return f
}

func (f *fakeUpstream) read(name string) []byte {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fakeUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	key := r.URL.Host + r.URL.Path
	f.mu.Lock()
	f.hits = append(f.hits, key)
	down := f.down
	body, overridden := f.override[key]
	f.mu.Unlock()
	if down {
		return nil, io.ErrUnexpectedEOF
	}
	if !overridden {
		name, ok := f.routes[key]
		if !ok && r.URL.Host == "api.modrinth.com" {
			name, ok = f.modrinth(r.URL)
		}
		if !ok {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: http.Header{}, Request: r}, nil
		}
		body = f.read(name)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

// modrinth maps a project version query to the captured answer for that
// project, loader and game version.
func (f *fakeUpstream) modrinth(u *url.URL) (string, bool) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/") // v2 project <id> version
	if len(parts) != 4 || parts[3] != "version" {
		return "", false
	}
	var loaders, games []string
	_ = json.Unmarshal([]byte(u.Query().Get("loaders")), &loaders)
	_ = json.Unmarshal([]byte(u.Query().Get("game_versions")), &games)
	if len(loaders) != 1 || len(games) != 1 {
		return "", false
	}
	name := "modrinth/" + parts[2] + "-" + loaders[0] + "-" + games[0] + ".json"
	if _, err := os.Stat(filepath.Join("testdata", name)); err != nil {
		return "", false
	}
	return name, true
}

func (f *fakeUpstream) resolver() *Resolver {
	return &Resolver{HTTP: &http.Client{Transport: f}}
}

// sidecar reads a captured Maven digest file.
func (f *fakeUpstream) sidecar(name string) string {
	return strings.TrimSpace(string(f.read("maven/" + name)))
}
