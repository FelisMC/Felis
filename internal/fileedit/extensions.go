package fileedit

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"felis.lolicon.best/internal/software"
)

const (
	MaxExtensions              = 500
	MaxExtensionsResponseBytes = 4 << 20
)

type ExtensionMetadata = software.JarMetadata

type Extension struct {
	Path           string              `json:"path"`
	Filename       string              `json:"filename"`
	Size           int64               `json:"size"`
	Enabled        bool                `json:"enabled"`
	Kind           string              `json:"kind"`
	MetadataStatus string              `json:"metadata_status"`
	Metadata       []ExtensionMetadata `json:"metadata"`
	ConfigPath     string              `json:"config_path,omitempty"`
}

type ExtensionList struct {
	Entries   []Extension `json:"entries"`
	Truncated bool        `json:"truncated"`
}

func extensions(r *os.Root) Result {
	result := Result{Extensions: []Extension{}}
	bytes := 128
	guard := NewGuard(r)
	for _, folder := range []string{"plugins", "mods"} {
		info, err := r.Lstat(folder)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return failure(err, folder)
		}
		if !info.IsDir() {
			continue
		}
		dir, err := r.Open(folder)
		if err != nil {
			return failure(err, folder)
		}
		current, err := dir.Stat()
		if err != nil || !os.SameFile(info, current) {
			dir.Close()
			return Result{Code: CodeConflict, Error: "the extensions folder changed while being listed"}
		}
		entries, err := dir.ReadDir(MaxEntries + 1)
		dir.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return failure(err, folder)
		}
		if len(entries) > MaxEntries {
			entries = entries[:MaxEntries]
			result.Truncated = true
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, candidate := range entries {
			filename := candidate.Name()
			lower := strings.ToLower(filename)
			if InternalPath(filename) || !strings.HasSuffix(lower, ".jar") && !strings.HasSuffix(lower, ".jar.disabled") {
				continue
			}
			info, err := candidate.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			withheld, redact := guard.Rule(info)
			currentWithheld, currentRedact := NewGuard(r).Rule(info)
			if withheld || redact || currentWithheld || currentRedact {
				continue
			}
			if len(result.Extensions) == MaxExtensions {
				result.Truncated = true
				return result
			}
			entry := Extension{Path: path.Join(folder, filename), Filename: filename, Size: info.Size(), Enabled: !strings.HasSuffix(lower, ".disabled"), Kind: "plugin", MetadataStatus: "unavailable", Metadata: []ExtensionMetadata{}}
			if folder == "mods" {
				entry.Kind = "mod"
			}
			f, err := r.Open(entry.Path)
			if err == nil {
				current, statErr := f.Stat()
				if statErr == nil && os.SameFile(info, current) {
					metadata, readErr := software.ReadJarMetadata(f, current.Size())
					if readErr == nil && len(metadata) > 0 {
						entry.Metadata, entry.MetadataStatus = metadata, "ok"
					}
				}
				f.Close()
			}
			entry.ConfigPath = extensionConfigPath(r, entry)
			encoded, _ := json.Marshal(entry)
			if bytes+len(encoded)+1 > MaxExtensionsResponseBytes {
				result.Truncated = true
				return result
			}
			bytes += len(encoded) + 1
			result.Extensions = append(result.Extensions, entry)
		}
	}
	return result
}

func extensionConfigPath(r *os.Root, entry Extension) string {
	var candidates []string
	for _, metadata := range entry.Metadata {
		candidates = append(candidates, metadata.ID, metadata.Name)
	}
	filename := entry.Filename
	if !entry.Enabled {
		filename = filename[:len(filename)-len(".disabled")]
	}
	candidates = append(candidates, filename[:len(filename)-len(".jar")])
	base := "plugins"
	if entry.Kind == "mod" {
		base = "config"
	}
	for _, name := range candidates {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			continue
		}
		candidate := path.Join(base, name)
		if safeExtensionDirectory(r, candidate) {
			return candidate
		}
	}
	if entry.Kind == "mod" && safeExtensionDirectory(r, base) {
		return base
	}
	return ""
}

func safeExtensionDirectory(r *os.Root, name string) bool {
	if InternalPath(name) || noLinks(r, name).Code != "" {
		return false
	}
	info, err := r.Lstat(name)
	return err == nil && info.IsDir()
}
