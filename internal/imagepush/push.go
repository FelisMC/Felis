package imagepush

import (
	"archive/tar"
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
	"strconv"
	"strings"
	"time"
)

// Non-gzip layer compression requires OCI manifests rather than Docker schema 2.
const (
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaDockerConfig   = "application/vnd.docker.container.image.v1+json"
	mediaDockerLayerGz  = "application/vnd.docker.image.rootfs.diff.tar.gzip"

	mediaOCIManifest  = "application/vnd.oci.image.manifest.v1+json"
	mediaOCIConfig    = "application/vnd.oci.image.config.v1+json"
	mediaOCILayerGz   = "application/vnd.oci.image.layer.v1.tar+gzip"
	mediaOCILayerZstd = "application/vnd.oci.image.layer.v1.tar+zstd"
	mediaOCILayerTar  = "application/vnd.oci.image.layer.v1.tar"
)

type Pusher struct {
	// Nil Client uses the shared push client.
	Client             *http.Client
	Scheme             string
	Username, Password string
	Log                io.Writer
	// Attempts defaults to 3.
	Attempts int
	// MaxWait bounds Retry-After waits independently of Attempts; zero defaults to 20 minutes.
	MaxWait time.Duration

	after func(time.Duration) <-chan time.Time
}

type Ref struct {
	Host, Repo, Tag string
}

func (r Ref) String() string { return r.Host + "/" + r.Repo + ":" + r.Tag }

// ParseRef requires a registry host and tag; digest-only references cannot be push targets.
func ParseRef(ref string) (Ref, error) {
	if strings.Contains(ref, "@") {
		return Ref{}, fmt.Errorf("imagepush: %q is a digest reference; push needs a tag", ref)
	}
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || rest == "" || !strings.ContainsAny(host, ".:") {
		return Ref{}, fmt.Errorf("imagepush: %q must be host/repository:tag", ref)
	}
	repo, tag := rest, "latest"
	if i := strings.LastIndexByte(rest, ':'); i > strings.LastIndexByte(rest, '/') {
		repo, tag = rest[:i], rest[i+1:]
	}
	if repo == "" || tag == "" {
		return Ref{}, fmt.Errorf("imagepush: %q must be host/repository:tag", ref)
	}
	return Ref{Host: host, Repo: repo, Tag: tag}, nil
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest"`

	file string
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type tarManifest struct {
	Config   string
	RepoTags []string
	Layers   []string
}

// Push returns the manifest digest recorded by the registry.
func (p *Pusher) Push(ctx context.Context, tarPath, ref string) (string, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	m, err := p.describe(tarPath)
	if err != nil {
		return "", err
	}
	blobs := append([]descriptor{m.Config}, m.Layers...)
	for i, b := range blobs {
		if err := p.retry(ctx, func() error { return p.pushBlob(ctx, r, tarPath, b) }); err != nil {
			return "", fmt.Errorf("imagepush: blob %d/%d (%s): %w", i+1, len(blobs), b.Digest, err)
		}
	}
	body, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	var digest string
	err = p.retry(ctx, func() error {
		d, err := p.putManifest(ctx, r, m.MediaType, body)
		digest = d
		return err
	})
	if err != nil {
		return "", fmt.Errorf("imagepush: manifest: %w", err)
	}
	p.logf("pushed %s@%s", r, digest)
	return digest, nil
}

func (p *Pusher) describe(tarPath string) (*manifest, error) {
	raw, err := readEntry(tarPath, "manifest.json", 1<<20)
	if err != nil {
		return nil, err
	}
	var entries []tarManifest
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("imagepush: manifest.json: %w", err)
	}
	if len(entries) != 1 {
		return nil, fmt.Errorf("imagepush: manifest.json describes %d images, want exactly 1", len(entries))
	}
	e := entries[0]
	if e.Config == "" || len(e.Layers) == 0 {
		return nil, errors.New("imagepush: manifest.json names no config or no layers")
	}

	cfg, _, err := digestEntry(tarPath, e.Config)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(e.Config, "sha256:") && e.Config != cfg.Digest {
		return nil, fmt.Errorf("imagepush: config %s hashes to %s", e.Config, cfg.Digest)
	}

	m := &manifest{SchemaVersion: 2, MediaType: mediaDockerManifest}
	oci := false
	for _, name := range e.Layers {
		d, magic, err := digestEntry(tarPath, name)
		if err != nil {
			return nil, err
		}
		switch {
		case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
			d.MediaType = mediaDockerLayerGz
		case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
			d.MediaType, oci = mediaOCILayerZstd, true
		default:
			d.MediaType, oci = mediaOCILayerTar, true
		}
		m.Layers = append(m.Layers, d)
	}
	cfg.MediaType = mediaDockerConfig
	if oci {
		m.MediaType = mediaOCIManifest
		cfg.MediaType = mediaOCIConfig
		for i := range m.Layers {
			if m.Layers[i].MediaType == mediaDockerLayerGz {
				m.Layers[i].MediaType = mediaOCILayerGz
			}
		}
	}
	m.Config = cfg
	return m, nil
}

func (p *Pusher) pushBlob(ctx context.Context, r Ref, tarPath string, b descriptor) error {
	return p.uploadBlob(ctx, r, b.Digest, b.Size, func() (io.ReadCloser, error) {
		f, entry, err := openEntry(tarPath, b.file)
		if err != nil {
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{entry, f}, nil
	})
}

// uploadBlob skips blobs already present and relies on registry digest validation.
func (p *Pusher) uploadBlob(ctx context.Context, r Ref, digest string, size int64, open func() (io.ReadCloser, error)) error {
	head, err := p.do(ctx, http.MethodHead, p.url(r, "blobs/"+digest), nil, 0, "")
	if err != nil {
		return err
	}
	head.Body.Close()
	if head.StatusCode == http.StatusOK {
		p.logf("exists  %s", digest)
		return nil
	}

	start, err := p.do(ctx, http.MethodPost, p.url(r, "blobs/uploads/"), nil, 0, "")
	if err != nil {
		return err
	}
	if start.StatusCode != http.StatusAccepted {
		defer start.Body.Close()
		return statusError("start upload", start)
	}
	drainClose(start)
	loc, err := start.Location()
	if err != nil {
		return fmt.Errorf("start upload: %w", err)
	}
	q := loc.Query()
	q.Set("digest", digest)
	loc.RawQuery = q.Encode()

	body, err := open()
	if err != nil {
		return err
	}
	defer body.Close()
	put, err := p.do(ctx, http.MethodPut, loc.String(), body, size, "application/octet-stream")
	if err != nil {
		return err
	}
	if put.StatusCode != http.StatusCreated {
		defer put.Body.Close()
		return statusError("upload", put)
	}
	drainClose(put)
	p.logf("pushed  %s (%d bytes)", digest, size)
	return nil
}

func (p *Pusher) putManifest(ctx context.Context, r Ref, mediaType string, body []byte) (string, error) {
	resp, err := p.do(ctx, http.MethodPut, p.url(r, "manifests/"+r.Tag), bytes.NewReader(body), int64(len(body)), mediaType)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", statusError("put manifest", resp)
	}
	sum := sha256.Sum256(body)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != want {
		return "", fmt.Errorf("registry recorded %s for a manifest that hashes to %s", got, want)
	}
	return want, nil
}

func (p *Pusher) url(r Ref, tail string) string {
	scheme := p.Scheme
	if scheme == "" {
		scheme = "https"
	}
	u := url.URL{Scheme: scheme, Host: r.Host, Path: "/v2/" + r.Repo + "/" + tail}
	return u.String()
}

func (p *Pusher) do(ctx context.Context, method, target string, body io.Reader, size int64, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if p.Username != "" {
		req.SetBasicAuth(p.Username, p.Password)
	}
	c := p.Client
	if c == nil {
		c = pushClient
	}
	return c.Do(req)
}

// Share keep-alive connections; caller deadlines bound large uploads without an overall client timeout.
var pushClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Minute}}

// retry fails immediately on permanent 4xx errors and handles Retry-After without spending Attempts.
func (p *Pusher) retry(ctx context.Context, fn func() error) error {
	attempts := p.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	maxWait := p.MaxWait
	if maxWait <= 0 {
		maxWait = 20 * time.Minute
	}
	var (
		err    error
		waited time.Duration
	)
	for i := 0; i < attempts; {
		if err = fn(); err == nil {
			return nil
		}
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusServiceUnavailable && se.RetryAfter > 0 && waited+se.RetryAfter <= maxWait {
			p.logf("registry unavailable, waiting %s: %s", se.RetryAfter, se.Body)
			if err := p.sleep(ctx, se.RetryAfter); err != nil {
				return err
			}
			waited += se.RetryAfter
			continue
		}
		if errors.As(err, &se) && se.Code >= 400 && se.Code < 500 && se.Code != http.StatusRequestTimeout && se.Code != http.StatusTooManyRequests {
			return err
		}
		i++
		if i < attempts {
			p.logf("retrying after: %v", err)
			if err := p.sleep(ctx, time.Duration(i)*time.Second); err != nil {
				return err
			}
		}
	}
	return err
}

func (p *Pusher) sleep(ctx context.Context, d time.Duration) error {
	after := p.after
	if after == nil {
		after = time.After
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-after(d):
		return nil
	}
}

func (p *Pusher) logf(format string, args ...any) {
	if p.Log != nil {
		fmt.Fprintf(p.Log, format+"\n", args...)
	}
}

type StatusError struct {
	Op         string
	Code       int
	Body       string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: registry answered %d: %s", e.Op, e.Code, e.Body)
}

func drainClose(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

func statusError(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	se := &StatusError{Op: op, Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	if sec, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && sec > 0 {
		se.RetryAfter = time.Duration(sec) * time.Second
	}
	return se
}

// openEntry returns an open file positioned at the entry; the caller must close it.
func openEntry(tarPath, name string) (*os.File, io.Reader, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, nil, err
	}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			f.Close()
			return nil, nil, fmt.Errorf("imagepush: %s has no entry %q", tarPath, name)
		}
		if err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("imagepush: reading %s: %w", tarPath, err)
		}
		if h.Name == name || strings.TrimPrefix(h.Name, "./") == name {
			if h.Typeflag != tar.TypeReg {
				f.Close()
				return nil, nil, fmt.Errorf("imagepush: entry %q is not a regular file", name)
			}
			return f, tr, nil
		}
	}
}

func readEntry(tarPath, name string, limit int64) ([]byte, error) {
	f, r, err := openEntry(tarPath, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("imagepush: %s exceeds %d bytes", name, limit)
	}
	return b, nil
}

func digestEntry(tarPath, name string) (descriptor, []byte, error) {
	f, r, err := openEntry(tarPath, name)
	if err != nil {
		return descriptor{}, nil, err
	}
	defer f.Close()
	h := sha256.New()
	var magic bytes.Buffer
	n, err := io.Copy(io.MultiWriter(h, &prefixWriter{buf: &magic, max: 4}), r)
	if err != nil {
		return descriptor{}, nil, fmt.Errorf("imagepush: hashing %s: %w", name, err)
	}
	return descriptor{Size: n, Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), file: name}, magic.Bytes(), nil
}

type prefixWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		w.buf.Write(p[:room])
	}
	return len(p), nil
}

// UploadBlob calls open again for each upload attempt.
func (p *Pusher) UploadBlob(ctx context.Context, host, repo, digest string, size int64, open func() (io.ReadCloser, error)) error {
	r := Ref{Host: host, Repo: repo}
	return p.retry(ctx, func() error { return p.uploadBlob(ctx, r, digest, size, open) })
}

func (p *Pusher) PutManifest(ctx context.Context, host, repo, reference, mediaType string, body []byte) (string, error) {
	r := Ref{Host: host, Repo: repo, Tag: reference}
	var digest string
	err := p.retry(ctx, func() error {
		d, err := p.putManifest(ctx, r, mediaType, body)
		digest = d
		return err
	})
	return digest, err
}
