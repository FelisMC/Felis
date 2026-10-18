package imagepush

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Bundle entries point directly to complete platform manifests so containerd imports the digest Pods use.

type BundleImage struct {
	Role   string
	Name   string
	Layout string
	// Source is used when Layout is empty and must be digest-pinned.
	Source string
}

type BundleEntry struct {
	Role, Name string
	// Digest identifies the manifest; Config identifies the image ID reported by CRI.
	Digest, Config string
}

var bundleTime = time.Unix(0, 0).UTC()

type resolvedImage struct {
	BundleImage
	mediaType string
	body      []byte
	m         *manifest
	config    []byte
	open      func(ctx context.Context, d descriptor) (io.ReadCloser, error)
}

// WriteBundle verifies every blob digest and image config platform.
func WriteBundle(ctx context.Context, s *Source, images []BundleImage, w io.Writer) ([]BundleEntry, error) {
	goos, arch, _ := s.platform()
	seenRole, seenName := map[string]bool{}, map[string]bool{}
	var resolved []*resolvedImage
	for _, img := range images {
		if img.Role == "" || img.Name == "" {
			return nil, fmt.Errorf("imagepush: bundle image %+v needs a role and a name", img)
		}
		if seenRole[img.Role] || seenName[img.Name] {
			return nil, fmt.Errorf("imagepush: bundle names role %s or image %s twice", img.Role, img.Name)
		}
		seenRole[img.Role], seenName[img.Name] = true, true
		var (
			r   *resolvedImage
			err error
		)
		switch {
		case img.Layout != "" && img.Source == "":
			r, err = resolveLayoutImage(img, s)
		case img.Source != "" && img.Layout == "":
			r, err = resolveSourceImage(ctx, img, s)
		default:
			err = fmt.Errorf("imagepush: bundle image %s needs exactly one of a layout and a source", img.Name)
		}
		if err != nil {
			return nil, err
		}
		var cfg struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		}
		if err := json.Unmarshal(r.config, &cfg); err != nil {
			return nil, fmt.Errorf("imagepush: %s: config: %w", img.Name, err)
		}
		if cfg.OS != goos || cfg.Architecture != arch {
			return nil, fmt.Errorf("imagepush: %s is a %s/%s image, the bundle is for %s/%s", img.Name, cfg.OS, cfg.Architecture, goos, arch)
		}
		resolved = append(resolved, r)
	}

	tw := tar.NewWriter(w)
	written := map[string]int64{}
	writeBytes := func(name string, b []byte) error {
		if err := tw.WriteHeader(bundleHeader(name, int64(len(b)))); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	if err := writeBytes(layoutMarkerFile, []byte(layoutMarkerContent)); err != nil {
		return nil, err
	}
	for _, dir := range []string{"blobs/", "blobs/sha256/"} {
		h := bundleHeader(dir, 0)
		h.Typeflag, h.Mode = tar.TypeDir, 0o755
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
	}
	var (
		index   = layoutIndex{SchemaVersion: 2, MediaType: mediaOCIIndex}
		entries []BundleEntry
	)
	for _, r := range resolved {
		for _, d := range append([]descriptor{r.m.Config}, r.m.Layers...) {
			if size, ok := written[d.Digest]; ok {
				if size != d.Size {
					return nil, fmt.Errorf("imagepush: %s: blob %s is %d bytes here and %d bytes in an earlier image", r.Name, d.Digest, d.Size, size)
				}
				continue
			}
			if err := copyBlob(ctx, tw, r, d); err != nil {
				return nil, err
			}
			written[d.Digest] = d.Size
		}
		digest := digestOf(r.body)
		if _, ok := written[digest]; !ok {
			if err := writeBytes(blobPath(digest), r.body); err != nil {
				return nil, err
			}
			written[digest] = int64(len(r.body))
		}
		desc := layoutDescriptor{MediaType: r.mediaType, Digest: digest, Size: int64(len(r.body))}
		for _, name := range bundleNames(r.Name, digest) {
			e := desc
			e.Annotations = map[string]string{annotationImageName: name}
			if tag := refTag(name); tag != "" {
				e.Annotations[annotationRefName] = tag
			}
			index.Manifests = append(index.Manifests, e)
		}
		entries = append(entries, BundleEntry{Role: r.Role, Name: r.Name, Digest: digest, Config: r.m.Config.Digest})
	}
	body, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	if err := writeBytes(layoutIndexFile, body); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return entries, nil
}

func bundleHeader(name string, size int64) *tar.Header {
	return &tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg, ModTime: bundleTime}
}

// Add repository@digest aliases so CRI can find imported images for digest-pinned Pods.
func bundleNames(name, digest string) []string {
	if refTag(name) == "" {
		return []string{name}
	}
	return []string{name, refRepo(name) + "@" + digest}
}

func refTag(name string) string {
	if strings.Contains(name, "@") {
		return ""
	}
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		return name[i+1:]
	}
	return ""
}

func refRepo(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		return name[:i]
	}
	return name
}

// PinnedName drops the tag to match CRI's repository@digest naming.
func PinnedName(ref string) string {
	_, digest, _ := strings.Cut(ref, "@")
	return refRepo(ref) + "@" + digest
}

func copyBlob(ctx context.Context, tw *tar.Writer, r *resolvedImage, d descriptor) error {
	rc, err := r.open(ctx, d)
	if err != nil {
		return fmt.Errorf("imagepush: %s: blob %s: %w", r.Name, d.Digest, err)
	}
	defer rc.Close()
	if err := tw.WriteHeader(bundleHeader(blobPath(d.Digest), d.Size)); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(rc, d.Size))
	if err != nil {
		return fmt.Errorf("imagepush: %s: blob %s: %w", r.Name, d.Digest, err)
	}
	// Check one byte beyond the declared size without writing it.
	if extra, _ := io.CopyN(io.Discard, rc, 1); extra > 0 {
		return fmt.Errorf("imagepush: %s: blob %s is longer than its %d bytes", r.Name, d.Digest, d.Size)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); n != d.Size || got != d.Digest {
		return fmt.Errorf("imagepush: %s: blob %s (%d bytes) holds %s (%d bytes)", r.Name, d.Digest, d.Size, got, n)
	}
	return nil
}

func resolveLayoutImage(img BundleImage, s *Source) (*resolvedImage, error) {
	idx, err := readLayoutIndex(img.Layout)
	if err != nil {
		return nil, err
	}
	entries := idx.Manifests
	for depth := 0; depth < 4; depth++ {
		d, err := pickLayoutEntry(entries, s)
		if err != nil {
			return nil, fmt.Errorf("imagepush: %s: %w", img.Layout, err)
		}
		body, err := readLayoutBlob(img.Layout, d)
		if err != nil {
			return nil, err
		}
		if isIndexType(d.MediaType) {
			var nested layoutIndex
			if err := json.Unmarshal(body, &nested); err != nil {
				return nil, fmt.Errorf("imagepush: %s: index %s: %w", img.Layout, d.Digest, err)
			}
			entries = nested.Manifests
			continue
		}
		if !isManifestType(d.MediaType) {
			return nil, fmt.Errorf("imagepush: %s: %s is a %q, want an image", img.Layout, d.Digest, d.MediaType)
		}
		m, err := parseManifest(body, d.MediaType)
		if err != nil {
			return nil, fmt.Errorf("imagepush: %s: %w", img.Layout, err)
		}
		config, err := readLayoutBlob(img.Layout, layoutDescriptor{Digest: m.Config.Digest, Size: m.Config.Size})
		if err != nil {
			return nil, err
		}
		path := img.Layout
		return &resolvedImage{BundleImage: img, mediaType: d.MediaType, body: body, m: m, config: config,
			open: func(_ context.Context, d descriptor) (io.ReadCloser, error) {
				f, entry, err := openEntry(path, blobPath(d.Digest))
				if err != nil {
					return nil, err
				}
				return struct {
					io.Reader
					io.Closer
				}{entry, f}, nil
			}}, nil
	}
	return nil, fmt.Errorf("imagepush: %s: indexes nest too deep", img.Layout)
}

// Accept a lone platform-less entry, verify its config later, and exclude unknown/unknown attestations.
func pickLayoutEntry(entries []layoutDescriptor, s *Source) (layoutDescriptor, error) {
	if len(entries) == 1 && entries[0].Platform == nil {
		return entries[0], nil
	}
	wantOS, wantArch, _ := s.platform()
	for _, e := range entries {
		if e.Platform != nil && e.Platform.OS == wantOS && e.Platform.Architecture == wantArch {
			return e, nil
		}
	}
	return layoutDescriptor{}, fmt.Errorf("no %s/%s image among %d entries", wantOS, wantArch, len(entries))
}

func resolveSourceImage(ctx context.Context, img BundleImage, s *Source) (*resolvedImage, error) {
	sr, err := ParseSourceRef(img.Source)
	if err != nil {
		return nil, err
	}
	if sr.Digest == "" {
		return nil, fmt.Errorf("imagepush: %s is not pinned by digest; a release bundles only pinned images", img.Source)
	}
	body, mt, err := s.manifest(ctx, sr, sr.Digest)
	if err != nil {
		return nil, fmt.Errorf("imagepush: %s: %w", sr, err)
	}
	if isIndexType(mt) {
		d, err := s.pick(body)
		if err != nil {
			return nil, fmt.Errorf("imagepush: %s: %w", sr, err)
		}
		if body, mt, err = s.manifest(ctx, sr, d); err != nil {
			return nil, fmt.Errorf("imagepush: %s: %w", sr, err)
		}
	}
	if !isManifestType(mt) {
		return nil, fmt.Errorf("imagepush: %s: unsupported manifest type %q", sr, mt)
	}
	m, err := parseManifest(body, mt)
	if err != nil {
		return nil, fmt.Errorf("imagepush: %s: %w", sr, err)
	}
	rc, err := s.blob(ctx, sr, m.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("imagepush: %s: config: %w", sr, err)
	}
	config, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("imagepush: %s: config: %w", sr, err)
	}
	if digestOf(config) != m.Config.Digest || int64(len(config)) != m.Config.Size {
		return nil, fmt.Errorf("imagepush: %s: config does not match %s", sr, m.Config.Digest)
	}
	return &resolvedImage{BundleImage: img, mediaType: mt, body: body, m: m, config: config,
		open: func(ctx context.Context, d descriptor) (io.ReadCloser, error) {
			return s.blob(ctx, sr, d.Digest)
		}}, nil
}
