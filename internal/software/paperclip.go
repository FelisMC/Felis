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

// PaperclipDownload reads which vanilla jar a Paper-family paperclip jar
// downloads on its first start, and where it keeps it. Paperclip records the
// download in the jar itself: META-INF/download-context ("<sha256>\t<url>\t<file
// name>", Minecraft 1.18 on) or patch.properties (originalHash, sourceUrl and
// version, before). Fetching that jar ahead, through the cache, makes the first
// start need no network; paperclip still checks it against the same sha256.
// A jar carrying neither record (an old non-paperclip build) yields nil.
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
			// java.util.Properties escapes ':' and '=' in values with a
			// backslash ("https\://launcher.mojang.com/...").
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
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return "", fmt.Errorf("software: read %s: %w", name, err)
	}
	return string(b), nil
}
