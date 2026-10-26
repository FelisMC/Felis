package software

import (
	"archive/zip"
	"context"
	"crypto/sha1" //nolint:gosec // Mojang's digests are sha1
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Use captured JAR metadata rather than invented upstream values.
func writeZip(t *testing.T, files map[string][]byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.jar")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return p
}

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestForgeLibrariesFromTheRealInstallerProfile(t *testing.T) {
	profile := testdata(t, "forge/install_profile-1.20.1-47.4.10.json")
	version := testdata(t, "forge/version-1.20.1-47.4.10.json")
	jar := writeZip(t, map[string][]byte{"install_profile.json": profile, "version.json": version})

	files, serverJar, err := ForgeLibraries(jar, "1.20.1")
	if err != nil {
		t.Fatal(err)
	}
	if serverJar != "libraries/net/minecraft/server/1.20.1/server-1.20.1.jar" {
		t.Errorf("server jar = %q, want serverJarPath with its placeholders filled", serverJar)
	}
	got := map[string]string{}
	for _, f := range files {
		if f.Digest.Algo != SHA1 {
			t.Errorf("%s digest %s, want the sha1 the profile gives", f.Path, f.Digest.Algo)
		}
		got[f.Path] = f.Digest.Hex
	}
	// 36 profile and 29 version libraries, two of them listed in both.
	if len(files) != 63 {
		t.Errorf("libraries = %d, want 63", len(files))
	}
	for _, want := range [][2]string{
		{"libraries/com/github/jponge/lzma-java/1.3/lzma-java-1.3.jar", "a25db9d4d385ccda4825ae1b47a7a61d86e595af"},
		{"libraries/cpw/mods/securejarhandler/2.1.10/securejarhandler-2.1.10.jar", "51e6a22c6c716beb11e244bf5b8be480f51dd6b5"},
		{"libraries/net/minecraftforge/fmlloader/1.20.1-47.4.10/fmlloader-1.20.1-47.4.10.jar", "55c46d27e7cfc8b829af8a28e562a7d290fa38b6"},
	} {
		if got[want[0]] != want[1] {
			t.Errorf("%s digest = %q, want %s", want[0], got[want[0]], want[1])
		}
	}
}

func TestForgeLaunchReadsTheRealRunScripts(t *testing.T) {
	for _, tc := range []struct {
		script string
		want   []string
	}{
		{"run-1.20.1-47.4.10.sh", []string{"@user_jvm_args.txt", "@libraries/net/minecraftforge/forge/1.20.1-47.4.10/unix_args.txt"}},
		{"run-26.3-66.0.12.sh", []string{"@user_jvm_args.txt", "@libraries/net/minecraftforge/forge/26.3-66.0.12/unix_args.txt"}},
		{"run-neoforge-21.4.150.sh", []string{"@user_jvm_args.txt", "@libraries/net/neoforged/neoforge/21.4.150/unix_args.txt"}},
		{"run-neoforge-26.3.0.85.sh", []string{"@user_jvm_args.txt", "@libraries/net/neoforged/neoforge/26.3.0.85/unix_args.txt"}},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), testdata(t, "forge/"+tc.script), 0o755); err != nil {
			t.Fatal(err)
		}
		l, err := ForgeLaunch(dir, LoaderInstaller{})
		if err != nil || !reflect.DeepEqual(l.Args, tc.want) {
			t.Errorf("%s: args %v, %v; want %v", tc.script, l.Args, err, tc.want)
		}
	}
	// Forge 1.12.2's installer leaves no script, only the server jar.
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "forge-1.12.2-14.23.5.2859.jar"), []byte("jar"), 0o644)
	if l, err := ForgeLaunch(dir, LoaderInstaller{Version: "1.12.2-14.23.5.2859"}); err != nil || l.Jar != "forge-1.12.2-14.23.5.2859.jar" {
		t.Errorf("legacy launch = %+v, %v", l, err)
	}
	if _, err := ForgeLaunch(t.TempDir(), LoaderInstaller{Version: "1.20.1-47.4.10"}); !errors.Is(err, ErrNoForgeLaunch) {
		t.Errorf("empty directory: err = %v, want ErrNoForgeLaunch", err)
	}
}

func TestPaperclipDownloadFromRealJars(t *testing.T) {
	jar := writeZip(t, map[string][]byte{"META-INF/download-context": testdata(t, "paperclip/download-context-1.21.4-232")})
	f, err := PaperclipDownload(jar)
	if err != nil || f == nil {
		t.Fatalf("download-context: %+v, %v", f, err)
	}
	want := File{Path: "cache/mojang_1.21.4.jar",
		URL:    "https://piston-data.mojang.com/v1/objects/4707d00eb834b446575d89a61a11b5d548d8c001/server.jar",
		Digest: Digest{SHA256, "1066970b09e9c671844572291c4a871cc1ac2b85838bf7004fa0e778e10f1358"}}
	if *f != want {
		t.Errorf("1.21.4 = %+v, want %+v", *f, want)
	}

	jar = writeZip(t, map[string][]byte{"patch.properties": testdata(t, "paperclip/patch.properties-1.16.5-794")})
	f, err = PaperclipDownload(jar)
	if err != nil || f == nil {
		t.Fatalf("patch.properties: %+v, %v", f, err)
	}
	want = File{Path: "cache/mojang_1.16.5.jar",
		URL:    "https://launcher.mojang.com/v1/objects/1b557e7b033b583cd9f66746b7a9ab1ec1673ced/server.jar",
		Digest: Digest{SHA256, "58f329c7d2696526f948470aa6fd0b45545039b64cb75015e64c12194b373da6"}}
	if *f != want {
		t.Errorf("1.16.5 = %+v, want %+v", *f, want)
	}

	if f, err := PaperclipDownload(writeZip(t, map[string][]byte{"plugin.yml": []byte("x")})); f != nil || err != nil {
		t.Errorf("a jar with no record = %+v, %v; want nothing", f, err)
	}
}

func TestJarModIDsFromRealModMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		body []byte
		want []string
	}{
		{"FabricProxy-Lite", "fabric.mod.json", testdata(t, "mods/fabricproxy-lite.fabric.mod.json"), []string{"fabricproxy-lite"}},
		{"Quilted Fabric API", "quilt.mod.json", testdata(t, "mods/qfapi.quilt.mod.json"), []string{"quilted_fabric_api"}},
		{"Proxy Compatible Forge", "META-INF/mods.toml", testdata(t, "mods/pcf.mods.toml"), []string{"pcf"}},
		// The MDK template: "[[mods]] — A list of mods - how many allowed here is
		// determined by the individual mod loader"; javafml takes several.
		{"two mods in one jar", "META-INF/neoforge.mods.toml", []byte(`modLoader="javafml"
loaderVersion="[4,)"
license="MIT"

[[mods]]
modId="bundledlib"
version="1.0"

[[mods]]
modId="pcf"
version="1.3.1"

[[dependencies.pcf]]
modId="neoforge"
type="required"
versionRange="[21,)"
`), []string{"bundledlib", "pcf"}},
	} {
		got := JarModIDs(writeZip(t, map[string][]byte{tc.file: tc.body}))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: ids %v, want %v", tc.name, got, tc.want)
		}
	}
	if ids := JarModIDs(filepath.Join(t.TempDir(), "missing.jar")); ids != nil {
		t.Errorf("unreadable jar: %v", ids)
	}
}

// testServer is a tiny vanilla "release" served through fakeUpstream's
// transport: a manifest naming one version JSON, which names one server jar,
// each by the sha1 of what is served.
type testServer struct {
	up      *fakeUpstream
	jars    map[string][]byte // download URL -> body
	fetches []string
}

func newTestServer(t *testing.T, versions ...string) *testServer {
	ts := &testServer{up: &fakeUpstream{t: t, routes: map[string]string{}, override: map[string][]byte{}}, jars: map[string][]byte{}}
	type entry struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
		SHA1 string `json:"sha1"`
	}
	var manifest struct {
		Versions []entry `json:"versions"`
	}
	for _, v := range versions {
		jar := []byte("server jar for " + v)
		jarURL := "https://piston-data.mojang.com/v1/objects/" + sum1(jar) + "/server.jar"
		ts.jars[jarURL] = jar
		vj, _ := json.Marshal(map[string]any{
			"id":          v,
			"javaVersion": map[string]int{"majorVersion": 21},
			"downloads":   map[string]any{"server": map[string]any{"sha1": sum1(jar), "size": len(jar), "url": jarURL}},
		})
		vURL := "piston-meta.mojang.com/v1/packages/" + sum1(vj) + "/" + v + ".json"
		ts.up.override[vURL] = vj
		manifest.Versions = append(manifest.Versions, entry{ID: v, Type: "release", URL: "https://" + vURL, SHA1: sum1(vj)})
	}
	m, _ := json.Marshal(manifest)
	ts.up.override["piston-meta.mojang.com/mc/game/version_manifest_v2.json"] = m
	return ts
}

func sum1(b []byte) string {
	h := sha1.Sum(b) //nolint:gosec // see import
	return hex.EncodeToString(h[:])
}

// Fetch is the installer's Fetcher: it serves the jars, checking the digest the way artifactcache.Client does.
func (ts *testServer) Fetch(_ context.Context, f File, dest string) error {
	ts.fetches = append(ts.fetches, f.Path)
	body, ok := ts.jars[f.URL]
	if !ok {
		return fmt.Errorf("404 %s", f.URL)
	}
	h, _ := NewHash(f.Digest.Algo)
	h.Write(body)
	if hex.EncodeToString(h.Sum(nil)) != f.Digest.Hex {
		return ErrDigestMismatch
	}
	return os.WriteFile(dest, body, 0o644)
}

func (ts *testServer) installer(dir string) *Installer {
	return &Installer{DataDir: dir, Resolver: &Resolver{HTTP: &http.Client{Transport: ts.up}}, Fetch: ts, Log: io.Discard}
}

func TestInstallStartsFromTheRecordWhileUpstreamIsDown(t *testing.T) {
	ts := newTestServer(t, "1.21.4")
	dir := t.TempDir()
	s := Spec{Type: Vanilla, MinecraftVersion: "1.21.4", Java: 21}
	first, err := ts.installer(dir).Install(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	ts.up.down = true
	ts.fetches = nil
	again, err := ts.installer(dir).Install(context.Background(), s)
	if err != nil {
		t.Fatalf("second start with upstream down: %v", err)
	}
	if !reflect.DeepEqual(first, again) || len(ts.fetches) != 0 {
		t.Errorf("second start launched %+v after fetching %v; want the same launch and no downloads", again, ts.fetches)
	}
	// A file damaged on the volume is fetched again (the jars themselves are
	// not behind the metadata APIs).
	jar := filepath.Join(dir, ".felis/server/minecraft_server.1.21.4.jar")
	_ = os.WriteFile(jar, []byte("corrupt"), 0o644)
	if _, err := ts.installer(dir).Install(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if ok, _ := FileMatches(jar, Digest{SHA1, sum1([]byte("server jar for 1.21.4"))}); !ok {
		t.Error("the damaged jar was not replaced")
	}
}

func TestInstallRemovesTheOldVersionsFilesOnly(t *testing.T) {
	ts := newTestServer(t, "1.21.4", "1.21.3")
	dir := t.TempDir()
	in := ts.installer(dir)
	if _, err := in.Install(context.Background(), Spec{Type: Vanilla, MinecraftVersion: "1.21.3", Java: 21}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"world/level.dat", "server.properties", "libraries/org/user/thing.jar"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		_ = os.WriteFile(filepath.Join(dir, p), []byte("owner's"), 0o644)
	}
	if _, err := in.Install(context.Background(), Spec{Type: Vanilla, MinecraftVersion: "1.21.4", Java: 21}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".felis/server/minecraft_server.1.21.3.jar")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the 1.21.3 jar is still there (stat err %v)", err)
	}
	for _, p := range []string{"world/level.dat", "server.properties", "libraries/org/user/thing.jar", ".felis/server/minecraft_server.1.21.4.jar"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s is gone: %v", p, err)
		}
	}
	rec, _ := ReadRecord(dir)
	if rec == nil || rec.Plan.Spec.MinecraftVersion != "1.21.4" {
		t.Errorf("record = %+v, want 1.21.4", rec)
	}
}

func TestPlaceModsDefersToTheOwnersCopy(t *testing.T) {
	ts := newTestServer(t)
	dir := t.TempDir()
	modBody := []byte("fabricproxy-lite jar")
	modURL := "https://cdn.modrinth.com/data/8dI2tmqs/fpl.jar"
	ts.jars[modURL] = modBody
	h, _ := NewHash(SHA512)
	h.Write(modBody)
	mod := Mod{File: File{Path: "mods/" + ManagedPrefix + "fpl.jar", URL: modURL, Digest: Digest{SHA512, hex.EncodeToString(h.Sum(nil))}}, ModID: "fabricproxy-lite"}
	in := ts.installer(dir)

	if err := in.placeMods(context.Background(), []Mod{mod}); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(dir, mod.Path)
	if _, err := os.Stat(managed); err != nil {
		t.Fatalf("managed mod not placed: %v", err)
	}
	stale := filepath.Join(dir, "mods", ManagedPrefix+"old-mod.jar")
	_ = os.WriteFile(stale, []byte("old"), 0o644)
	own := writeZip(t, map[string][]byte{"fabric.mod.json": testdata(t, "mods/fabricproxy-lite.fabric.mod.json")})
	ownBody, _ := os.ReadFile(own)
	_ = os.WriteFile(filepath.Join(dir, "mods", "FabricProxy-Lite-mine.jar"), ownBody, 0o644)

	if err := in.placeMods(context.Background(), []Mod{mod}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(managed); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("managed copy kept beside the owner's own (stat err %v)", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a managed mod the plan no longer names was kept (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mods", "FabricProxy-Lite-mine.jar")); err != nil {
		t.Errorf("the owner's mod was touched: %v", err)
	}
}

func TestInstallCustomNeedsTheJar(t *testing.T) {
	dir := t.TempDir()
	in := &Installer{DataDir: dir, Log: io.Discard}
	s := Spec{Type: Custom, Jar: "server/start.jar", Java: 21}
	if _, err := in.Install(context.Background(), s); err == nil || !strings.Contains(err.Error(), "upload it") {
		t.Errorf("install without the jar: %v, want the upload hint", err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "server"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "server/start.jar"), []byte("jar"), 0o644)
	l, err := in.Install(context.Background(), s)
	if err != nil || l.Jar != "server/start.jar" {
		t.Errorf("launch = %+v, %v", l, err)
	}
}

func TestSemverLessOrdersLoaderReleases(t *testing.T) {
	order := []string{"0.29.2", "0.30.0-beta.2", "0.30.0-beta.10", "0.30.0", "0.30.1-beta.4", "0.30.1", "0.31.0-beta.1"}
	for i := 1; i < len(order); i++ {
		if !semverLess(order[i-1], order[i]) || semverLess(order[i], order[i-1]) {
			t.Errorf("%s should sort before %s", order[i-1], order[i])
		}
	}
	if semverLess("21.4.150", "21.4.150") {
		t.Error("a version sorts before itself")
	}
}
