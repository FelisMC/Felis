package software

import (
	"archive/zip"
	"encoding/json"

	"github.com/BurntSushi/toml"
)

// JarModIDs returns none for unreadable or non-mod jars; it only prevents duplicate managed forwarding mods.
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
	// Read mod IDs only from mods tables; dependency tables name required mods rather than identities declared by this jar.
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
