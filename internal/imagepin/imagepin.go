// Package imagepin pins platform registry images by digest to prevent mutable tags from upgrading worlds on wake.
package imagepin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var ErrNotFound = errors.New("imagepin: tag not found in the registry")

// Advertise supported manifest types to avoid schema-1 conversion changing the digest.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const maxManifestBytes = 4 << 20

func Pinned(ref string) bool { return strings.Contains(ref, "@") }

type Resolver struct {
	// References outside Registry are left unchanged.
	Registry string
	// Endpoint defaults to Registry; host commands may use its loopback port.
	Endpoint string
	// Nil Client uses a 10s timeout.
	Client *http.Client
}

func (r Resolver) Covers(ref string) bool {
	return r.Registry != "" && strings.HasPrefix(ref, r.Registry+"/")
}

// Pin resolves local tags and checks existing pinned manifests; external references remain unchanged.
func (r Resolver) Pin(ctx context.Context, ref string) (string, error) {
	if !r.Covers(ref) {
		return ref, nil
	}
	if name, pinned, ok := strings.Cut(ref, "@"); ok {
		repo, _ := splitTag(strings.TrimPrefix(name, r.Registry+"/"))
		if repo == "" || !digestRE.MatchString(pinned) {
			return "", fmt.Errorf("imagepin: %q is not a valid pinned reference", ref)
		}
		if _, err := r.digest(ctx, repo, pinned); err != nil {
			return "", fmt.Errorf("imagepin: resolve %s: %w", ref, err)
		}
		return ref, nil
	}
	repo, tag := splitTag(strings.TrimPrefix(ref, r.Registry+"/"))
	if repo == "" {
		return "", fmt.Errorf("imagepin: %q names no repository", ref)
	}
	digest, err := r.digest(ctx, repo, tag)
	if err != nil {
		return "", fmt.Errorf("imagepin: resolve %s: %w", ref, err)
	}
	if !strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":") {
		ref += ":" + tag
	}
	return ref + "@" + digest, nil
}

func splitTag(path string) (repo, tag string) {
	slash := strings.LastIndex(path, "/")
	if colon := strings.LastIndex(path, ":"); colon > slash {
		return path[:colon], path[colon+1:]
	}
	return path, "latest"
}

func (r Resolver) digest(ctx context.Context, repo, tag string) (string, error) {
	endpoint := r.Endpoint
	if endpoint == "" {
		endpoint = r.Registry
	}
	url := "http://" + endpoint + "/v2/" + repo + "/manifests/" + tag
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// Use HEAD for the digest, falling back to GET and hashing when the header is absent.
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", manifestAccept)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		d, err := readDigest(resp, method == http.MethodGet)
		resp.Body.Close()
		if err != nil || d != "" {
			return d, err
		}
	}
	return "", errors.New("registry sent neither a digest header nor a manifest")
}

// readDigest returns an empty digest without error when the next retrieval method should be tried.
func readDigest(resp *http.Response, hash bool) (string, error) {
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("registry answered %s", resp.Status)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		if !digestRE.MatchString(d) {
			return "", fmt.Errorf("registry sent a malformed digest %q", d)
		}
		return d, nil
	}
	if !hash {
		return "", nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxManifestBytes {
		return "", errors.New("manifest is implausibly large")
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
