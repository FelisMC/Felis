package fileedit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func extensionJar(t *testing.T, root, name string, entries ...zent) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	writeZip(t, full, entries...)
}

func TestExtensionsCatalogReadsOnlyInstalledJarFiles(t *testing.T) {
	root := t.TempDir()
	extensionJar(t, root, "plugins/Example.jar", file("plugin.yml", "name: Example\nversion: '2.3'\nauthors: [Author]\n"))
	extensionJar(t, root, "mods/Disabled.jar.disabled", file("fabric.mod.json", `{"id":"disabled_mod","name":"Disabled Mod","version":"1.2","authors":["Mod Author"]}`))
	extensionJar(t, root, "plugins/nested/ignored.jar", file("plugin.yml", "name: Nested\nversion: '1'\n"))
	putTransferFile(t, root, "plugins/broken.jar", "not a jar")
	putTransferFile(t, root, "plugins/notes.txt", "notes")
	before := tree(t, root)
	res, err := Execute(root, Request{Op: OpExtensions})
	if err != nil || res.Code != "" || res.Truncated || len(res.Extensions) != 3 {
		t.Fatalf("catalog: %+v %v", res, err)
	}
	byPath := map[string]Extension{}
	for _, entry := range res.Extensions {
		byPath[entry.Path] = entry
	}
	plugin := byPath["plugins/Example.jar"]
	if !plugin.Enabled || plugin.Kind != "plugin" || plugin.Filename != "Example.jar" || plugin.Size <= 0 || plugin.MetadataStatus != "ok" || !reflect.DeepEqual(plugin.Metadata, []ExtensionMetadata{{ID: "Example", Name: "Example", Version: "2.3", Authors: []string{"Author"}, Loaders: []string{"bukkit"}}}) {
		t.Fatalf("plugin: %+v", plugin)
	}
	mod := byPath["mods/Disabled.jar.disabled"]
	if mod.Enabled || mod.Kind != "mod" || mod.MetadataStatus != "ok" || len(mod.Metadata) != 1 || mod.Metadata[0].ID != "disabled_mod" {
		t.Fatalf("disabled mod: %+v", mod)
	}
	broken := byPath["plugins/broken.jar"]
	if broken.MetadataStatus != "unavailable" || broken.Metadata == nil || len(broken.Metadata) != 0 {
		t.Fatalf("broken jar disappeared or gained metadata: %+v", broken)
	}
	assertSameTree(t, before, tree(t, root))
}

func TestExtensionsCatalogHandlesAbsentAndEmptyFolders(t *testing.T) {
	for _, name := range []string{"absent", "empty"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if name == "empty" {
				if err := os.Mkdir(filepath.Join(root, "plugins"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "mods"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			res, err := Execute(root, Request{Op: OpExtensions})
			if err != nil || res.Code != "" || res.Truncated || res.Extensions == nil || len(res.Extensions) != 0 {
				t.Fatalf("empty catalog: %+v %v", res, err)
			}
		})
	}
}

func TestExtensionsCatalogRefusesLinksAndProtectedAliases(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	extensionJar(t, root, "plugins/real.jar", file("plugin.yml", "name: Real\nversion: '1'\n"))
	extensionJar(t, outside, "outside.jar", file("plugin.yml", "name: Outside\nversion: '1'\n"))
	putTransferFile(t, root, "config/paper-global.yml", "fake forwarding credential")
	putTransferFile(t, root, "server.properties", "rcon.password=fake-rcon\n")
	for _, pair := range [][2]string{{"config/paper-global.yml", "plugins/shared.jar"}, {"server.properties", "plugins/props.jar"}} {
		if err := os.Link(filepath.Join(root, pair[0]), filepath.Join(root, pair[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("real.jar", filepath.Join(root, "plugins/alias.jar")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "mods")); err != nil {
		t.Fatal(err)
	}
	res, err := Execute(root, Request{Op: OpExtensions})
	if err != nil || res.Code != "" || len(res.Extensions) != 1 || res.Extensions[0].Path != "plugins/real.jar" {
		t.Fatalf("unsafe entries reached catalog: %+v %v", res, err)
	}
}

func TestExtensionsConfigPathsUseExistingSafeDirectories(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	extensionJar(t, root, "plugins/named.jar", file("plugin.yml", "name: NamedPlugin\nversion: '1'\n"))
	extensionJar(t, root, "plugins/jar-stem.jar.disabled", file("plugin.yml", "name: MissingFolder\nversion: '1'\n"))
	extensionJar(t, root, "plugins/unsafe.jar", file("plugin.yml", "name: Unsafe\nversion: '1'\n"))
	extensionJar(t, root, "mods/specific.jar", file("fabric.mod.json", `{"id":"specific","version":"1"}`))
	extensionJar(t, root, "mods/fallback.jar", file("fabric.mod.json", `{"id":"other","name":"../outside","version":"1"}`))
	for _, name := range []string{"plugins/NamedPlugin", "plugins/jar-stem", "config/specific"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "plugins/Unsafe")); err != nil {
		t.Fatal(err)
	}
	res, err := Execute(root, Request{Op: OpExtensions})
	if err != nil || res.Code != "" || len(res.Extensions) != 5 {
		t.Fatalf("catalog: %+v %v", res, err)
	}
	want := map[string]string{"plugins/named.jar": "plugins/NamedPlugin", "plugins/jar-stem.jar.disabled": "plugins/jar-stem", "plugins/unsafe.jar": "", "mods/specific.jar": "config/specific", "mods/fallback.jar": "config"}
	for _, entry := range res.Extensions {
		if entry.ConfigPath != want[entry.Path] {
			t.Errorf("%s config=%q; want %q", entry.Path, entry.ConfigPath, want[entry.Path])
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "config")); err != nil {
		t.Fatal(err)
	}
	res, err = Execute(root, Request{Op: OpExtensions})
	if err != nil || res.Code != "" {
		t.Fatalf("catalog with config link: %+v %v", res, err)
	}
	for _, entry := range res.Extensions {
		if entry.Kind == "mod" && entry.ConfigPath != "" {
			t.Fatalf("linked config directory was offered: %+v", entry)
		}
	}
}

func TestExtensionsConfigPathsRejectContainedDirectoryLinks(t *testing.T) {
	root := t.TempDir()
	extensionJar(t, root, "mods/specific.jar", file("fabric.mod.json", `{"id":"specific","version":"1"}`))
	if err := os.MkdirAll(filepath.Join(root, "existing-config/specific"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("existing-config", filepath.Join(root, "config")); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := r.Stat("config/specific")
	r.Close()
	if err != nil || !info.IsDir() {
		t.Fatalf("contained link does not resolve inside the root: %v", err)
	}
	res, err := Execute(root, Request{Op: OpExtensions})
	if err != nil || res.Code != "" || len(res.Extensions) != 1 {
		t.Fatalf("catalog: %+v %v", res, err)
	}
	if res.Extensions[0].ConfigPath != "" {
		t.Fatalf("contained directory link was offered as a config path: %+v", res.Extensions[0])
	}
}

func TestExtensionsCatalogLimitsFileCount(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 501; i++ {
		putTransferFile(t, root, fmt.Sprintf("mods/%03d.jar", i), "invalid jar")
	}
	res, err := Execute(root, Request{Op: OpExtensions})
	if err != nil || res.Code != "" || !res.Truncated || len(res.Extensions) != 500 {
		t.Fatalf("file limit: count=%d truncated=%v code=%q error=%v", len(res.Extensions), res.Truncated, res.Code, err)
	}
}

func TestExtensionsCatalogBoundsEncodedResponse(t *testing.T) {
	root := t.TempDir()
	var manifest strings.Builder
	for i := 0; i < 32; i++ {
		fmt.Fprintf(&manifest, "[[mods]]\nmodId='%s%02d'\ndisplayName='%s'\nversion='%s'\nauthors='%s'\n", strings.Repeat("i", 245), i, strings.Repeat("n", 256), strings.Repeat("v", 128), strings.Repeat("a", 256))
	}
	extensionJar(t, root, "mods/source.jar", file("META-INF/mods.toml", manifest.String()))
	body, err := os.ReadFile(filepath.Join(root, "mods/source.jar"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 150; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("mods/%03d.jar", i)), body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Execute(root, Request{Op: OpExtensions})
	if err != nil || res.Code != "" || !res.Truncated || len(res.Extensions) == 0 {
		t.Fatalf("response limit: count=%d truncated=%v code=%q error=%v", len(res.Extensions), res.Truncated, res.Code, err)
	}
	encoded, err := json.Marshal(res)
	if err != nil || len(encoded) > 4<<20 {
		t.Fatalf("response grew to %d bytes: %v", len(encoded), err)
	}
}

func TestEditorExtensionsUsesReadOnlyOperation(t *testing.T) {
	runner := &fakeRunner{payload: mustPayload(t, Result{Extensions: []Extension{{Path: "plugins/example.jar", Filename: "example.jar", Metadata: []ExtensionMetadata{}}}, Truncated: true})}
	editor := &Editor{Runner: runner, Config: Config{Image: "image"}}
	list, err := editor.Extensions(context.Background(), "survival")
	if err != nil || len(list.Entries) != 1 || !list.Truncated || len(runner.got) != 1 || runner.got[0].Op != OpExtensions {
		t.Fatalf("extensions: %+v %v params=%+v", list, err, runner.got)
	}
	runner.payload = mustPayload(t, Result{})
	list, err = editor.Extensions(context.Background(), "survival")
	if err != nil || list.Entries == nil || len(list.Entries) != 0 {
		t.Fatalf("empty extensions: %+v %v", list, err)
	}
}
