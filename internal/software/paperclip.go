package software

import (
	"archive/zip"
	"bufio"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// PaperclipDownload reads embedded vanilla-download metadata for cache prefetch; non-paperclip jars return nil.
func PaperclipDownload(jarPath string) (*File, error) {
	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return nil, fmt.Errorf("software: open %s: %w", jarPath, err)
	}
	defer zr.Close()

	if body, err := readZipText(&zr.Reader, "META-INF/download-context"); err == nil {
		fields := strings.Split(strings.TrimSpace(body), "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("software: download-context has %d fields, want 3", len(fields))
		}
		return paperclipFile(fields[0], fields[1], fields[2])
	} else if !errors.Is(err, errZipEntryMissing) {
		return nil, err
	}

	body, err := readZipText(&zr.Reader, "patch.properties")
	if errors.Is(err, errZipEntryMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	props := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			// Java properties escape colons and equals signs inside URL values.
			props[strings.TrimSpace(k)] = strings.NewReplacer(`\:`, ":", `\=`, "=", `\\`, `\`).Replace(strings.TrimSpace(v))
		}
	}
	src := props["sourceUrl"]
	if src == "" {
		src = props["originalUrl"]
	}
	if props["originalHash"] == "" || src == "" || props["version"] == "" {
		return nil, nil
	}
	return paperclipFile(props["originalHash"], src, "mojang_"+props["version"]+".jar")
}

func paperclipFile(hash, rawURL, name string) (*File, error) {
	d, err := ParseDigest(SHA256, hash)
	if err != nil {
		return nil, fmt.Errorf("software: paperclip download hash: %w", err)
	}
	if err := requireHTTPS(rawURL); err != nil {
		return nil, err
	}
	base := path.Base(name)
	if base != name || !strings.HasSuffix(base, ".jar") {
		return nil, fmt.Errorf("software: paperclip download name %q", name)
	}
	return &File{Path: path.Join("cache", base), URL: rawURL, Digest: d}, nil
}

var errZipEntryMissing = errors.New("zip entry missing")

func readZipText(zr *zip.Reader, name string) (string, error) {
	f, err := zr.Open(name)
	if err != nil {
		return "", errZipEntryMissing
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxJarManifestBytes+1))
	if err != nil {
		return "", fmt.Errorf("software: read %s: %w", name, err)
	}
	if len(b) > MaxJarManifestBytes {
		return "", fmt.Errorf("software: %s exceeds %d bytes", name, MaxJarManifestBytes)
	}
	return string(b), nil
}
