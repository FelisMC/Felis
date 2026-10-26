package software

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func jarMetadata(t *testing.T, entries map[string][]byte) ([]JarMetadata, error) {
	t.Helper()
	f, err := os.Open(writeZip(t, entries))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return ReadJarMetadata(f, info.Size())
}

func TestJarMetadataFromRealManifests(t *testing.T) {
	resource := func(name string) []byte {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("..", "..", "plugins", name))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	for _, tc := range []struct {
		name, manifest string
		body           []byte
		want           JarMetadata
	}{
		{"Felis Paper", "plugin.yml", resource("paper/src/main/resources/plugin.yml"), JarMetadata{ID: "FelisPaper", Name: "FelisPaper", Authors: []string{"Felis"}, Loaders: []string{"bukkit"}}},
		{"FabricProxy-Lite", "fabric.mod.json", testdata(t, "mods/fabricproxy-lite.fabric.mod.json"), JarMetadata{ID: "fabricproxy-lite", Name: "FabricProxy Lite", Version: "2.10.1", Authors: []string{"james58899"}, Loaders: []string{"fabric"}}},
		{"Quilted Fabric API", "quilt.mod.json", testdata(t, "mods/qfapi.quilt.mod.json"), JarMetadata{ID: "quilted_fabric_api", Name: "Quilted Fabric API", Version: "7.7.0+0.92.2-1.20.1", Authors: []string{"FabricMC", "The Quilt Project"}, Loaders: []string{"quilt"}}},
		{"Proxy Compatible Forge", "META-INF/mods.toml", testdata(t, "mods/pcf.mods.toml"), JarMetadata{ID: "pcf", Name: "ProxyCompatibleForge", Version: "1.3.1", Authors: []string{"adde0109,JT122406,p0t4t0sandwich"}, Loaders: []string{"forge", "neoforge"}}},
		{"Felis Forge", "META-INF/mods.toml", resource("forge/src/main/resources/META-INF/mods.toml"), JarMetadata{ID: "felis_link", Name: "Felis Link", Authors: []string{"Felis"}, Loaders: []string{"forge"}}},
		{"Felis old NeoForge", "META-INF/mods.toml", resource("neoforge/src/main/resources/META-INF/mods.toml"), JarMetadata{ID: "felis_link", Name: "Felis Link", Authors: []string{"Felis"}, Loaders: []string{"neoforge"}}},
		{"NeoForge manifest", "META-INF/neoforge.mods.toml", resource("neoforge/src/main/resources/META-INF/mods.toml"), JarMetadata{ID: "felis_link", Name: "Felis Link", Authors: []string{"Felis"}, Loaders: []string{"neoforge"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := jarMetadata(t, map[string][]byte{tc.manifest: tc.body})
			// An empty want.Version leaves the repo's own plugin versions unchecked.
			if len(got) == 1 && tc.want.Version == "" {
				got[0].Version = ""
			}
			if err != nil || !reflect.DeepEqual(got, []JarMetadata{tc.want}) {
				t.Fatalf("metadata=%+v error=%v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestJarMetadataOfficialPaperAndVelocityExamples(t *testing.T) {
	for _, tc := range []struct {
		manifest, body string
		want           JarMetadata
	}{
		{"paper-plugin.yml", "name: Example-Plugin\nversion: '1.0'\nmain: com.example.paperplugin.ExamplePlugin\napi-version: '26.3'\n", JarMetadata{ID: "Example-Plugin", Name: "Example-Plugin", Version: "1.0", Authors: []string{}, Loaders: []string{"paper"}}},
		{"velocity-plugin.json", `{"id":"example-plugin","main":"com.example.testplugin.TestPluginMain","name":"Velocity Example Plugin","version":"1.0.0","authors":["Strokkur24"]}`, JarMetadata{ID: "example-plugin", Name: "Velocity Example Plugin", Version: "1.0.0", Authors: []string{"Strokkur24"}, Loaders: []string{"velocity"}}},
		{"velocity-plugin.json", `{"id":"optional-fields","main":"example.Main"}`, JarMetadata{ID: "optional-fields", Name: "optional-fields", Authors: []string{}, Loaders: []string{"velocity"}}},
		{"fabric.mod.json", `{"id":"named-authors","version":"1.0","authors":["First",{"name":"Second"},"First",{"contact":{}}]}`, JarMetadata{ID: "named-authors", Name: "named-authors", Version: "1.0", Authors: []string{"First", "Second"}, Loaders: []string{"fabric"}}},
	} {
		t.Run(tc.manifest+tc.want.ID, func(t *testing.T) {
			got, err := jarMetadata(t, map[string][]byte{tc.manifest: []byte(tc.body)})
			if err != nil || !reflect.DeepEqual(got, []JarMetadata{tc.want}) {
				t.Fatalf("metadata=%+v error=%v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestJarMetadataKeepsMultipleModsAndMergesLoaders(t *testing.T) {
	body := []byte("[[mods]]\nmodId='one'\ndisplayName='First'\nversion='1.0'\n[[mods]]\nmodId='two'\nversion='2.0'\n[[dependencies.one]]\nmodId='minecraft'\n")
	got, err := jarMetadata(t, map[string][]byte{"META-INF/mods.toml": body, "META-INF/neoforge.mods.toml": body})
	want := []JarMetadata{
		{ID: "one", Name: "First", Version: "1.0", Authors: []string{}, Loaders: []string{"neoforge", "forge"}},
		{ID: "two", Name: "two", Version: "2.0", Authors: []string{}, Loaders: []string{"neoforge", "forge"}},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("multiple mods=%+v error=%v", got, err)
	}
	got, err = jarMetadata(t, map[string][]byte{
		"paper-plugin.yml": []byte("name: Both\nversion: '1.0'\nauthor: Paper\n"),
		"plugin.yml":       []byte("name: Both\nversion: '1.0'\nauthors: [Bukkit, Paper]\n"),
	})
	want = []JarMetadata{{ID: "Both", Name: "Both", Version: "1.0", Authors: []string{"Paper", "Bukkit"}, Loaders: []string{"paper", "bukkit"}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("preferred Paper metadata=%+v error=%v", got, err)
	}
}

func TestJarMetadataVersionPlaceholders(t *testing.T) {
	for _, tc := range []struct{ version, manifest, want string }{
		{"${file.jarVersion}", "Manifest-Version: 1.0\r\nimplementation-version: 1.2.\r\n 3+build\r\n\r\nName: other\r\nImplementation-Version: incorrect\r\n", "1.2.3+build"},
		{"${file.jarVersion}", "Manifest-Version: 1.0\n\nName: other\nImplementation-Version: incorrect\n", ""},
		{"${version}", "Implementation-Version: should-not-be-guessed\n", ""},
		{"@VERSION@", "", ""},
		{"1.0-beta+build", "", "1.0-beta+build"},
	} {
		t.Run(tc.version+tc.want, func(t *testing.T) {
			got, err := jarMetadata(t, map[string][]byte{
				"META-INF/mods.toml":   []byte("[[mods]]\nmodId='versioned'\nversion='" + tc.version + "'\n"),
				"META-INF/MANIFEST.MF": []byte(tc.manifest),
			})
			if err != nil || len(got) != 1 || got[0].Version != tc.want {
				t.Fatalf("version=%+v error=%v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestJarMetadataRejectsOversizedManifests(t *testing.T) {
	const limit = 1 << 20
	prefix := `{"id":"bounded","version":"1.0"}`
	body := prefix + strings.Repeat(" ", limit-len(prefix))
	got, err := jarMetadata(t, map[string][]byte{"fabric.mod.json": []byte(body)})
	if err != nil || len(got) != 1 {
		t.Fatalf("manifest at the limit: %+v %v", got, err)
	}
	got, err = jarMetadata(t, map[string][]byte{"fabric.mod.json": []byte(body + " ")})
	if err == nil && len(got) != 0 {
		t.Fatalf("oversized manifest was silently truncated: %+v", got)
	}
	zr, err := zip.OpenReader(writeZip(t, map[string][]byte{"record": []byte(body + " ")}))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if _, err := readZipText(&zr.Reader, "record"); err == nil {
		t.Fatal("the shared ZIP text reader accepted an oversized record")
	}
}

func TestJarMetadataBoundsFieldsAuthorsAndModCount(t *testing.T) {
	var authors []string
	for i := 0; i < 20; i++ {
		authors = append(authors, fmt.Sprintf("author-%d", i))
	}
	body := fmt.Sprintf(`{"id":"bounded","name":%q,"version":%q,"authors":[%s]}`, strings.Repeat("字", 100), strings.Repeat("v", 200), `"`+strings.Join(authors, `","`)+`"`)
	got, err := jarMetadata(t, map[string][]byte{"fabric.mod.json": []byte(body)})
	if err != nil || len(got) != 1 || len(got[0].Authors) != 16 || len(got[0].Name) > 256 || !utf8.ValidString(got[0].Name) || len(got[0].Version) > 128 {
		t.Fatalf("unbounded fields: %+v %v", got, err)
	}
	var mods strings.Builder
	for i := 0; i < 33; i++ {
		fmt.Fprintf(&mods, "[[mods]]\nmodId='mod_%d'\n", i)
	}
	if got, err := jarMetadata(t, map[string][]byte{"META-INF/mods.toml": []byte(mods.String())}); err == nil || len(got) != 0 {
		t.Fatalf("unbounded mod count: %+v %v", got, err)
	}
}
