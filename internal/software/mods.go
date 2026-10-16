package software

import (
	"archive/zip"
	"encoding/json"

	"github.com/BurntSushi/toml"
)

// JarModIDs returns the mod ids a mod jar declares: fabric.mod.json's id,
// quilt.mod.json's quilt_loader.id, and the modId of every [[mods]] table of a
// Forge or NeoForge mods.toml. A jar that is not a mod, or cannot be read, declares none; the
// installer uses this only to avoid adding a forwarding mod the owner already
// put in themselves.
func JarModIDs(jarPath string) []string {
	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return nil
	}
	defer zr.Close()
	var ids []string
	if body, err := readZipText(&zr.Reader, "fabric.mod.json"); err == nil {
		var m struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(body), &m) == nil && m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	if body, err := readZipText(&zr.Reader, "quilt.mod.json"); err == nil {
		var m struct {
			QuiltLoader struct {
				ID string `json:"id"`
			} `json:"quilt_loader"`
		}
		if json.Unmarshal([]byte(body), &m) == nil && m.QuiltLoader.ID != "" {
			ids = append(ids, m.QuiltLoader.ID)
		}
	}
	// Only the [[mods]] tables declare mods; the modId keys under
	// [[dependencies.<id>]] name what a mod depends on (PCF's own file lists
	// forge, neoforge and minecraft there), and counting those would take a mod
	// that merely depends on PCF for PCF itself.
	for _, name := range []string{"META-INF/mods.toml", "META-INF/neoforge.mods.toml"} {
		body, err := readZipText(&zr.Reader, name)
		if err != nil {
			continue
		}
		var manifest struct {
			Mods []struct {
				ModID string `toml:"modId"`
			} `toml:"mods"`
		}
		if _, err := toml.Decode(body, &manifest); err != nil {
			continue
		}
		for _, m := range manifest.Mods {
			if m.ModID != "" {
				ids = append(ids, m.ModID)
			}
		}
	}
	return ids
}
