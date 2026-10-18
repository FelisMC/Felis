package imagepush

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// OCI layout names use io.containerd.image.name; push manifests verbatim to preserve imported digests.

const (
	annotationImageName = "io.containerd.image.name"
	annotationRefName   = "org.opencontainers.image.ref.name"
	layoutIndexFile     = "index.json"
	layoutMarkerFile    = "oci-layout"
	layoutMarkerContent = `{"imageLayoutVersion":"1.0.0"}`
)

type layoutDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *platformSpec     `json:"platform,omitempty"`
}

type platformSpec struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type layoutIndex struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType,omitempty"`
	Manifests     []layoutDescriptor `json:"manifests"`
}

func blobPath(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

func isManifestType(mt string) bool { return mt == mediaOCIManifest || mt == mediaDockerManifest }
func isIndexType(mt string) bool    { return mt == mediaOCIIndex || mt == mediaDockerList }

func readLayoutIndex(tarPath string) (*layoutIndex, error) {
	raw, err := readEntry(tarPath, layoutIndexFile, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	var idx layoutIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("imagepush: %s: %s: %w", tarPath, layoutIndexFile, err)
	}
	return &idx, nil
}

// readLayoutBlob verifies the blob against its descriptor.
func readLayoutBlob(tarPath string, d layoutDescriptor) ([]byte, error) {
	if !sha256DigestRE.MatchString(d.Digest) {
		return nil, fmt.Errorf("imagepush: %s: bad digest %q", tarPath, d.Digest)
	}
	if d.Size < 0 || d.Size > maxManifestBytes {
		return nil, fmt.Errorf("imagepush: %s: blob %s has size %d", tarPath, d.Digest, d.Size)
	}
	body, err := readEntry(tarPath, blobPath(d.Digest), maxManifestBytes)
	if err != nil {
		return nil, err
	}
	if got := digestOf(body); got != d.Digest || int64(len(body)) != d.Size {
		return nil, fmt.Errorf("imagepush: %s: blob %s (%d bytes) holds %s (%d bytes)", tarPath, d.Digest, d.Size, got, len(body))
	}
	return body, nil
}

// PushLayout preserves the named OCI manifest bytes and digest.
func (p *Pusher) PushLayout(ctx context.Context, tarPath, name, ref string) (string, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	idx, err := readLayoutIndex(tarPath)
	if err != nil {
		return "", err
	}
	var (
		desc  layoutDescriptor
		found int
		names []string
	)
	for _, m := range idx.Manifests {
		n := m.Annotations[annotationImageName]
		names = append(names, n)
		if n == name {
			desc = m
			found++
		}
	}
	switch {
	case found == 0:
		sort.Strings(names)
		return "", fmt.Errorf("imagepush: %s holds no image named %s (it holds: %s)", tarPath, name, strings.Join(names, ", "))
	case found > 1:
		return "", fmt.Errorf("imagepush: %s names %d images %s", tarPath, found, name)
	}
	if !isManifestType(desc.MediaType) {
		return "", fmt.Errorf("imagepush: %s: %s is a %q, want an image manifest", tarPath, name, desc.MediaType)
	}
	body, err := readLayoutBlob(tarPath, desc)
	if err != nil {
		return "", err
	}
	m, err := parseManifest(body, desc.MediaType)
	if err != nil {
		return "", fmt.Errorf("imagepush: %s: %s: %w", tarPath, name, err)
	}
	blobs := append([]descriptor{m.Config}, m.Layers...)
	for i, b := range blobs {
		err := p.retry(ctx, func() error {
			return p.uploadBlob(ctx, r, b.Digest, b.Size, func() (io.ReadCloser, error) {
				f, entry, err := openEntry(tarPath, blobPath(b.Digest))
				if err != nil {
					return nil, err
				}
				return struct {
					io.Reader
					io.Closer
				}{entry, f}, nil
			})
		})
		if err != nil {
			return "", fmt.Errorf("imagepush: %s: blob %d/%d (%s): %w", name, i+1, len(blobs), b.Digest, err)
		}
	}
	var digest string
	err = p.retry(ctx, func() error {
		d, err := p.putManifest(ctx, r, desc.MediaType, body)
		digest = d
		return err
	})
	if err != nil {
		return "", fmt.Errorf("imagepush: %s: manifest: %w", r, err)
	}
	p.logf("pushed %s as %s@%s", name, r, digest)
	return digest, nil
}

// parseManifest rejects malformed descriptors and mismatched media types.
func parseManifest(body []byte, mt string) (*manifest, error) {
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.MediaType != "" && m.MediaType != mt {
		return nil, fmt.Errorf("manifest says it is a %q, its descriptor a %q", m.MediaType, mt)
	}
	if len(m.Layers) == 0 {
		return nil, fmt.Errorf("manifest has no layers")
	}
	for i, b := range append([]descriptor{m.Config}, m.Layers...) {
		if !sha256DigestRE.MatchString(b.Digest) || b.Size < 0 {
			return nil, fmt.Errorf("blob %d has digest %q size %d", i+1, b.Digest, b.Size)
		}
	}
	return &m, nil
}
