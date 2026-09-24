package imagepush

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Mirroring copies one image or OCI artifact from a public registry into the
// platform registry: the build tools (kaniko, trivy) and Trivy's vulnerability
// DBs, which a build Job cannot fetch itself because its namespace has no
// internet egress. Blobs stream from the source straight into the upload, and
// the destination registry checks each against its digest.
//
// An image index is narrowed to the one platform the node runs: the copy is a
// single-platform manifest under the destination tag. A source pinned by digest
// is checked against it before anything is copied.

const (
	mediaOCIIndex        = "application/vnd.oci.image.index.v1+json"
	mediaDockerList      = "application/vnd.docker.distribution.manifest.list.v2+json"
	manifestAccept       = mediaOCIIndex + "," + mediaDockerList + "," + mediaOCIManifest + "," + mediaDockerManifest
	maxManifestBytes     = 4 << 20
	sourceRequestTimeout = 2 * time.Minute
)

// SourceRef is host/repository[:tag][@digest].
type SourceRef struct {
	Host, Repo, Tag, Digest string
}

func (r SourceRef) String() string {
	s := r.Host + "/" + r.Repo
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// reference is what the manifest endpoint is asked for: the digest when pinned.
func (r SourceRef) reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

var sha256DigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ParseSourceRef parses a fully qualified reference. docker.io is served from
// registry-1.docker.io, and its single-component names live under library/.
func ParseSourceRef(ref string) (SourceRef, error) {
	name, digest, _ := strings.Cut(ref, "@")
	if digest != "" && !sha256DigestRE.MatchString(digest) {
		return SourceRef{}, fmt.Errorf("imagepush: %q: bad digest", ref)
	}
	host, rest, ok := strings.Cut(name, "/")
	if !ok || rest == "" || !strings.ContainsAny(host, ".:") {
		return SourceRef{}, fmt.Errorf("imagepush: %q must be host/repository[:tag][@digest]", ref)
	}
	repo, tag := rest, ""
	if i := strings.LastIndexByte(rest, ':'); i > strings.LastIndexByte(rest, '/') {
		repo, tag = rest[:i], rest[i+1:]
	}
	if repo == "" || (tag == "" && strings.HasSuffix(rest, ":")) {
		return SourceRef{}, fmt.Errorf("imagepush: %q must be host/repository[:tag][@digest]", ref)
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	return SourceRef{Host: host, Repo: repo, Tag: tag, Digest: digest}, nil
}

// Source reads manifests and blobs anonymously from public registries, answering
// their bearer-token challenges (ghcr.io, gcr.io, mirror.gcr.io, Docker Hub).
type Source struct {
	// Client performs the requests; nil uses one with response-header timeouts.
	Client *http.Client
	// Platform selects the manifest of an index, as "os/arch" or
	// "os/arch/variant". Empty means linux and this binary's architecture.
	Platform string
	// Scheme is "https" unless a test serves plain HTTP.
	Scheme string

	mu     sync.Mutex
	tokens map[string]string // host/repo → bearer token
}

// sourceClient is shared by every Source without its own Client, so reads
// reuse keep-alive connections instead of opening one per request.
var sourceClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: sourceRequestTimeout}}

func (s *Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return sourceClient
}

func (s *Source) url(r SourceRef, tail string) string {
	scheme := s.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: r.Host, Path: "/v2/" + r.Repo + "/" + tail}).String()
}

// get issues a GET, fetching a bearer token once when the registry challenges.
func (s *Source) get(ctx context.Context, r SourceRef, target, accept string) (*http.Response, error) {
	key := r.Host + "/" + r.Repo
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		s.mu.Lock()
		tok := s.tokens[key]
		s.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := s.client().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		tok, err = s.token(ctx, challenge)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		s.mu.Lock()
		if s.tokens == nil {
			s.tokens = map[string]string{}
		}
		s.tokens[key] = tok
		s.mu.Unlock()
	}
}

// token answers a Bearer challenge anonymously.
func (s *Source) token(ctx context.Context, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("registry wants %q authentication; only anonymous bearer tokens are supported", scheme)
	}
	p := parseChallenge(params)
	if p["realm"] == "" {
		return "", errors.New("bearer challenge without a realm")
	}
	u, err := url.Parse(p["realm"])
	if err != nil {
		return "", fmt.Errorf("bearer realm: %w", err)
	}
	q := u.Query()
	for _, k := range []string{"service", "scope"} {
		if p[k] != "" {
			q.Set(k, p[k])
		}
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError("token", resp)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", errors.New("token endpoint returned no token")
}

// parseChallenge splits key="value",key="value".
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		s = strings.TrimLeft(s, " ,")
		k, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		var v string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				break
			}
			v, s = rest[1:1+end], rest[2+end:]
		} else {
			v, s, _ = strings.Cut(rest, ",")
		}
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return out
}

// manifest fetches one manifest by tag or digest and returns its bytes and
// media type. A digest reference is verified against the bytes.
func (s *Source) manifest(ctx context.Context, r SourceRef, reference string) ([]byte, string, error) {
	resp, err := s.get(ctx, r, s.url(r, "manifests/"+reference), manifestAccept)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", statusError("get manifest "+reference, resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxManifestBytes {
		return nil, "", fmt.Errorf("manifest %s is larger than %d bytes", reference, maxManifestBytes)
	}
	if strings.HasPrefix(reference, "sha256:") {
		if got := digestOf(body); got != reference {
			return nil, "", fmt.Errorf("manifest %s hashes to %s", reference, got)
		}
	}
	mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.MediaType != "" {
		mt = probe.MediaType
	}
	return body, strings.TrimSpace(mt), nil
}

func (s *Source) blob(ctx context.Context, r SourceRef, digest string) (io.ReadCloser, error) {
	resp, err := s.get(ctx, r, s.url(r, "blobs/"+digest), "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, statusError("get blob "+digest, resp)
	}
	return resp.Body, nil
}

func (s *Source) platform() (goos, arch, variant string) {
	p := s.Platform
	if p == "" {
		p = "linux/" + runtime.GOARCH
	}
	parts := strings.SplitN(p, "/", 3)
	goos = parts[0]
	if len(parts) > 1 {
		arch = parts[1]
	}
	if len(parts) > 2 {
		variant = parts[2]
	}
	return goos, arch, variant
}

type indexEntry struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

// pick returns the digest of the index entry for the wanted platform.
func (s *Source) pick(body []byte) (string, error) {
	var idx struct {
		Manifests []indexEntry `json:"manifests"`
	}
	if err := json.Unmarshal(body, &idx); err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	wantOS, wantArch, wantVariant := s.platform()
	var fallback string
	for _, m := range idx.Manifests {
		if m.Platform == nil || m.Platform.OS != wantOS || m.Platform.Architecture != wantArch {
			continue
		}
		if wantVariant == "" || m.Platform.Variant == wantVariant {
			return m.Digest, nil
		}
		if fallback == "" {
			fallback = m.Digest
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("the index has no %s/%s manifest", wantOS, wantArch)
}

// Mirror copies src (a SourceRef string) to dst (host/repository:tag on p's
// registry) and returns the digest of the manifest it wrote.
func (p *Pusher) Mirror(ctx context.Context, s *Source, src, dst string) (string, error) {
	sr, err := ParseSourceRef(src)
	if err != nil {
		return "", err
	}
	dr, err := ParseRef(dst)
	if err != nil {
		return "", err
	}
	var body []byte
	var mt string
	err = p.retry(ctx, func() error {
		body, mt, err = s.manifest(ctx, sr, sr.reference())
		return err
	})
	if err != nil {
		return "", fmt.Errorf("imagepush: %s: %w", sr, err)
	}
	if mt == mediaOCIIndex || mt == mediaDockerList {
		d, err := s.pick(body)
		if err != nil {
			return "", fmt.Errorf("imagepush: %s: %w", sr, err)
		}
		err = p.retry(ctx, func() error {
			body, mt, err = s.manifest(ctx, sr, d)
			return err
		})
		if err != nil {
			return "", fmt.Errorf("imagepush: %s: %w", sr, err)
		}
	}
	if mt != mediaOCIManifest && mt != mediaDockerManifest {
		return "", fmt.Errorf("imagepush: %s: unsupported manifest type %q", sr, mt)
	}
	var m struct {
		Config descriptor   `json:"config"`
		Layers []descriptor `json:"layers"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", fmt.Errorf("imagepush: %s: manifest: %w", sr, err)
	}
	blobs := append([]descriptor{m.Config}, m.Layers...)
	for i, b := range blobs {
		if !sha256DigestRE.MatchString(b.Digest) || b.Size < 0 {
			return "", fmt.Errorf("imagepush: %s: blob %d has digest %q size %d", sr, i+1, b.Digest, b.Size)
		}
		err := p.retry(ctx, func() error {
			return p.uploadBlob(ctx, dr, b.Digest, b.Size, func() (io.ReadCloser, error) {
				return s.blob(ctx, sr, b.Digest)
			})
		})
		if err != nil {
			return "", fmt.Errorf("imagepush: %s: blob %d/%d (%s): %w", sr, i+1, len(blobs), b.Digest, err)
		}
	}
	var digest string
	err = p.retry(ctx, func() error {
		d, err := p.putManifest(ctx, dr, mt, bytes.Clone(body))
		digest = d
		return err
	})
	if err != nil {
		return "", fmt.Errorf("imagepush: %s: manifest: %w", dr, err)
	}
	p.logf("mirrored %s to %s@%s", sr, dr, digest)
	return digest, nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// MirrorStatus is what `felis mirror-build-tools --status` records, and the
// watchdog reads to tell a stale vulnerability DB.
type MirrorStatus struct {
	LastAttempt time.Time `json:"last_attempt"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
}

// ReadMirrorStatus reads a status file; a missing one is (nil, nil).
func ReadMirrorStatus(path string) (*MirrorStatus, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st MirrorStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}

// WriteMirrorStatus replaces the status file atomically.
func WriteMirrorStatus(path string, st MirrorStatus) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
