package software

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultUserAgent identifies Felis to the metadata APIs. PaperMC and Modrinth
// both ask for an agent naming the application and a contact.
const DefaultUserAgent = "felis-software/1 (+https://felis.lolicon.best)"

// Public upstream endpoints.
const (
	MojangManifestURL = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"
	FillURL           = "https://fill.papermc.io"
	PurpurURL         = "https://api.purpurmc.org"
	FabricMetaURL     = "https://meta.fabricmc.net"
	QuiltMetaURL      = "https://meta.quiltmc.org"
	ForgeFilesURL     = "https://files.minecraftforge.net"
	ForgeMavenURL     = "https://maven.minecraftforge.net"
	NeoForgeMavenURL  = "https://maven.neoforged.net"
	ModrinthURL       = "https://api.modrinth.com"
)

// maxMetadataBytes bounds one metadata response. The largest real one, Quilt's
// loader list for a version, is under 1 MiB.
const maxMetadataBytes = 16 << 20

// Resolver reads upstream metadata. The zero value talks to the public
// upstreams with a 30-second client; tests point the URL fields at an httptest
// server. With CacheTTL set, responses are kept that long, so the panel's
// version pickers do not hit upstream on every keystroke.
type Resolver struct {
	HTTP      *http.Client
	UserAgent string
	CacheTTL  time.Duration

	MojangManifest string
	Fill           string
	Purpur         string
	FabricMeta     string
	QuiltMeta      string
	ForgeFiles     string
	ForgeMaven     string
	NeoForgeMaven  string
	Modrinth       string

	mu    sync.Mutex
	cache map[string]cachedBody
	calls map[string]*inflight
}

type cachedBody struct {
	body []byte
	at   time.Time
}

type inflight struct {
	done chan struct{}
	body []byte
	err  error
}

func (r *Resolver) url(field, def string) string {
	if field != "" {
		return strings.TrimRight(field, "/")
	}
	return def
}

func (r *Resolver) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// UpstreamError reports an upstream that answered with something unusable.
type UpstreamError struct {
	URL    string
	Status int
	Err    error
}

func (e *UpstreamError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("software: %s answered HTTP %d", e.URL, e.Status)
	}
	return fmt.Sprintf("software: %s: %v", e.URL, e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// NotFoundError reports a version or build upstream does not publish.
type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return "software: " + e.What + " not found" }

// get fetches a URL's body, sharing one request between concurrent callers and
// serving from the cache while it is fresh.
func (r *Resolver) get(ctx context.Context, url string) ([]byte, error) {
	r.mu.Lock()
	if r.CacheTTL > 0 {
		if c, ok := r.cache[url]; ok && time.Since(c.at) < r.CacheTTL {
			r.mu.Unlock()
			return c.body, nil
		}
	}
	if call, ok := r.calls[url]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.body, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.calls == nil {
		r.calls = map[string]*inflight{}
	}
	call := &inflight{done: make(chan struct{})}
	r.calls[url] = call
	r.mu.Unlock()

	call.body, call.err = r.fetch(ctx, url)

	r.mu.Lock()
	delete(r.calls, url)
	if call.err == nil && r.CacheTTL > 0 {
		if r.cache == nil {
			r.cache = map[string]cachedBody{}
		}
		r.cache[url] = cachedBody{body: call.body, at: time.Now()}
	}
	r.mu.Unlock()
	close(call.done)
	return call.body, call.err
}

func (r *Resolver) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &UpstreamError{URL: url, Err: err}
	}
	ua := r.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, &UpstreamError{URL: url, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{What: url}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{URL: url, Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes+1))
	if err != nil {
		return nil, &UpstreamError{URL: url, Err: err}
	}
	if len(body) > maxMetadataBytes {
		return nil, &UpstreamError{URL: url, Err: fmt.Errorf("response larger than %d bytes", maxMetadataBytes)}
	}
	return body, nil
}

func (r *Resolver) getJSON(ctx context.Context, url string, v any) error {
	body, err := r.get(ctx, url)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return &UpstreamError{URL: url, Err: fmt.Errorf("decode: %w", err)}
	}
	return nil
}

// sidecarDigest reads the .sha256 (or, failing that, .sha1) file a Maven
// repository serves beside an artifact. The file holds the hex digest, sometimes
// followed by the file name.
func (r *Resolver) sidecarDigest(ctx context.Context, artifactURL string) (Digest, error) {
	var firstErr error
	for _, algo := range []string{SHA256, SHA1} {
		body, err := r.get(ctx, artifactURL+"."+algo)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		field := string(bytes.TrimSpace(body))
		if i := strings.IndexAny(field, " \t\r\n"); i >= 0 {
			field = field[:i]
		}
		d, err := ParseDigest(algo, field)
		if err != nil {
			return Digest{}, &UpstreamError{URL: artifactURL + "." + algo, Err: err}
		}
		return d, nil
	}
	return Digest{}, firstErr
}

func jsonUnmarshal(body []byte, v any) error { return json.Unmarshal(body, v) }

// bytesMatch reports whether body hashes to d.
func bytesMatch(body []byte, d Digest) bool {
	h, err := NewHash(d.Algo)
	if err != nil {
		return false
	}
	h.Write(body)
	return fmt.Sprintf("%x", h.Sum(nil)) == d.Hex
}

func asNotFound(err error, target **NotFoundError) bool { return errors.As(err, target) }
