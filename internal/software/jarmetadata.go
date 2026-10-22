package software

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"sigs.k8s.io/yaml"
)

const (
	MaxJarManifestBytes = 1 << 20
	MaxJarMetadata      = 32
	MaxJarAuthors       = 16
	MaxJarTextBytes     = 256
	MaxJarVersionBytes  = 128
	maxJarReadBytes     = 16 << 20
)

type JarMetadata struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Version string   `json:"version,omitempty"`
	Authors []string `json:"authors"`
	Loaders []string `json:"loaders"`
}

func ReadJarMetadata(src io.ReaderAt, size int64) ([]JarMetadata, error) {
	zr, err := zip.NewReader(&jarReadBudget{src: src, left: maxJarReadBytes}, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, err
	}
	var metadata []JarMetadata
	for _, name := range []string{"paper-plugin.yml", "plugin.yml", "quilt.mod.json", "fabric.mod.json", "META-INF/neoforge.mods.toml", "META-INF/mods.toml", "velocity-plugin.json"} {
		body, err := readZipText(zr, name)
		if err != nil {
			continue
		}
		rows, err := parseJarMetadata(zr, name, body)
		if err != nil {
			continue
		}
		for _, row := range rows {
			row.ID = jarText(row.ID, MaxJarTextBytes)
			row.Name = jarText(row.Name, MaxJarTextBytes)
			if row.Name == "" {
				row.Name = row.ID
			}
			if row.ID == "" || row.Name == "" {
				continue
			}
			row.Version = jarVersion(row.Version)
			row.Authors = jarAuthors(row.Authors)
			merged := false
			for i := range metadata {
				entry := &metadata[i]
				if entry.ID == row.ID && entry.Name == row.Name && entry.Version == row.Version {
					entry.Authors = jarAuthors(append(entry.Authors, row.Authors...))
					for _, loader := range row.Loaders {
						if !slices.Contains(entry.Loaders, loader) {
							entry.Loaders = append(entry.Loaders, loader)
						}
					}
					merged = true
					break
				}
			}
			if !merged {
				metadata = append(metadata, row)
			}
			if len(metadata) > MaxJarMetadata {
				return nil, fmt.Errorf("software: jar declares more than %d metadata entries", MaxJarMetadata)
			}
		}
	}
	return metadata, nil
}

func parseJarMetadata(zr *zip.Reader, name, body string) ([]JarMetadata, error) {
	switch name {
	case "plugin.yml", "paper-plugin.yml":
		var data struct {
			Name    string   `json:"name"`
			Version string   `json:"version"`
			Author  string   `json:"author"`
			Authors []string `json:"authors"`
		}
		if err := yaml.Unmarshal([]byte(body), &data); err != nil {
			return nil, err
		}
		loader := "bukkit"
		if name == "paper-plugin.yml" {
			loader = "paper"
		}
		return []JarMetadata{{ID: data.Name, Name: data.Name, Version: data.Version, Authors: append(data.Authors, data.Author), Loaders: []string{loader}}}, nil
	case "fabric.mod.json":
		var data struct {
			ID      string            `json:"id"`
			Name    string            `json:"name"`
			Version string            `json:"version"`
			Authors []json.RawMessage `json:"authors"`
		}
		if err := json.Unmarshal([]byte(body), &data); err != nil {
			return nil, err
		}
		authors := []string{}
		for _, raw := range data.Authors {
			var name string
			if json.Unmarshal(raw, &name) != nil {
				var person struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(raw, &person) != nil {
					continue
				}
				name = person.Name
			}
			authors = append(authors, name)
		}
		return []JarMetadata{{ID: data.ID, Name: data.Name, Version: data.Version, Authors: authors, Loaders: []string{"fabric"}}}, nil
	case "quilt.mod.json":
		var data struct {
			Loader struct {
				ID       string `json:"id"`
				Version  string `json:"version"`
				Metadata struct {
					Name         string                     `json:"name"`
					Contributors map[string]json.RawMessage `json:"contributors"`
				} `json:"metadata"`
			} `json:"quilt_loader"`
		}
		if err := json.Unmarshal([]byte(body), &data); err != nil {
			return nil, err
		}
		authors := []string{}
		for author := range data.Loader.Metadata.Contributors {
			authors = append(authors, author)
		}
		sort.Strings(authors)
		return []JarMetadata{{ID: data.Loader.ID, Name: data.Loader.Metadata.Name, Version: data.Loader.Version, Authors: authors, Loaders: []string{"quilt"}}}, nil
	case "META-INF/mods.toml", "META-INF/neoforge.mods.toml":
		var data struct {
			Mods []struct {
				ID      string `toml:"modId"`
				Name    string `toml:"displayName"`
				Version string `toml:"version"`
				Authors string `toml:"authors"`
			} `toml:"mods"`
			Dependencies map[string][]struct {
				ID string `toml:"modId"`
			} `toml:"dependencies"`
		}
		if _, err := toml.Decode(body, &data); err != nil {
			return nil, err
		}
		var implementationVersion string
		for _, mod := range data.Mods {
			if mod.Version == "${file.jarVersion}" {
				if manifest, err := readZipText(zr, "META-INF/MANIFEST.MF"); err == nil {
					implementationVersion = jarImplementationVersion(manifest)
				}
				break
			}
		}
		rows := []JarMetadata{}
		for _, mod := range data.Mods {
			loaders := []string{}
			if name == "META-INF/neoforge.mods.toml" {
				loaders = append(loaders, "neoforge")
			}
			for _, dependency := range data.Dependencies[mod.ID] {
				if (dependency.ID == "forge" || dependency.ID == "neoforge") && !slices.Contains(loaders, dependency.ID) {
					loaders = append(loaders, dependency.ID)
				}
			}
			if len(loaders) == 0 {
				loaders = []string{"forge"}
			}
			version := mod.Version
			if version == "${file.jarVersion}" {
				version = implementationVersion
			}
			rows = append(rows, JarMetadata{ID: mod.ID, Name: mod.Name, Version: version, Authors: []string{mod.Authors}, Loaders: loaders})
		}
		return rows, nil
	case "velocity-plugin.json":
		var data struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Version string   `json:"version"`
			Authors []string `json:"authors"`
		}
		if err := json.Unmarshal([]byte(body), &data); err != nil {
			return nil, err
		}
		return []JarMetadata{{ID: data.ID, Name: data.Name, Version: data.Version, Authors: data.Authors, Loaders: []string{"velocity"}}}, nil
	}
	return nil, nil
}

func jarImplementationVersion(manifest string) string {
	lines := strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(manifest), "\n")
	var current string
	for _, line := range lines {
		if strings.HasPrefix(line, " ") {
			current += line[1:]
			continue
		}
		if key, value, ok := strings.Cut(current, ":"); ok && strings.EqualFold(key, "Implementation-Version") {
			return strings.TrimSpace(value)
		}
		if line == "" {
			return ""
		}
		current = line
	}
	if key, value, ok := strings.Cut(current, ":"); ok && strings.EqualFold(key, "Implementation-Version") {
		return strings.TrimSpace(value)
	}
	return ""
}

func jarVersion(version string) string {
	if strings.Contains(version, "${") || strings.Contains(strings.ToLower(version), "@version@") {
		return ""
	}
	return jarText(version, MaxJarVersionBytes)
}

func jarText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	for !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func jarAuthors(values []string) []string {
	out := []string{}
	for _, value := range values {
		if value = jarText(value, MaxJarTextBytes); value != "" && !slices.Contains(out, value) {
			out = append(out, value)
			if len(out) == MaxJarAuthors {
				break
			}
		}
	}
	return out
}

type jarReadBudget struct {
	src  io.ReaderAt
	left int64
}

func (b *jarReadBudget) ReadAt(p []byte, off int64) (int, error) {
	if int64(len(p)) > b.left {
		return 0, fmt.Errorf("software: jar metadata exceeds its read budget")
	}
	n, err := b.src.ReadAt(p, off)
	b.left -= int64(n)
	return n, err
}
