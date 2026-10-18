// Package artifactcache caches verified software downloads by digest from approved upstream hosts.
package artifactcache

import (
	"context"
	"crypto/md5"  //nolint:gosec // indexes digests upstreams publish; see software.Digest
	"crypto/sha1" //nolint:gosec // indexes digests upstreams publish; see software.Digest
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"felis.lolicon.best/internal/software"
)

// AllowedHosts restricts both initial downloads and every redirect.
var AllowedHosts = []string{
	"piston-data.mojang.com",
	"piston-meta.mojang.com",
	"launcher.mojang.com",
	"libraries.minecraft.net",
	"fill-data.papermc.io",
	"api.purpurmc.org",
	"maven.fabricmc.net",
	"maven.quiltmc.org",
	"maven.minecraftforge.net",
	"maven.neoforged.net",
	"cdn.modrinth.com",
	"repo1.maven.org",
	"repo.maven.apache.org",
}

func HostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, h := range AllowedHosts {
		if host == h {
			return true
		}
	}
	return false
}

const (
	DefaultMaxFileBytes = 1 << 30
	DefaultParallel     = 4
	DefaultTimeout      = 15 * time.Minute
)

type Cache struct {
	Dir      string
	MaxBytes int64
	// MaxFileBytes defaults to 1 GiB.
	MaxFileBytes int64
	// Upstream redirects are restricted to AllowedHosts; nil uses DefaultTimeout.
	Upstream  *http.Client
	UserAgent string
	// IdleTimeout defaults to DefaultIdleTimeout.
	IdleTimeout time.Duration
	Log         *slog.Logger
	// Allow overrides host validation and receives the host including any port.
	Allow func(host string) bool

	initOnce sync.Once
	initErr  error
	mu       sync.Mutex
	total    int64
	calls    map[string]*call
	slots    chan struct{}
}

type call struct {
	done chan struct{}
	sum  string
	err  error
}

func (c *Cache) Init() error {
	c.initOnce.Do(func() {
		for _, d := range []string{"blobs", "index", "tmp"} {
			if err := os.MkdirAll(filepath.Join(c.Dir, d), 0o755); err != nil {
				c.initErr = err
				return
			}
		}
		// Downloads interrupted by a restart leave temp files behind.
		if entries, err := os.ReadDir(filepath.Join(c.Dir, "tmp")); err == nil {
			for _, e := range entries {
				_ = os.Remove(filepath.Join(c.Dir, "tmp", e.Name()))
			}
		}
		c.initErr = filepath.WalkDir(filepath.Join(c.Dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err == nil {
				c.total += info.Size()
			}
			return nil
		})
		n := DefaultParallel
		c.slots = make(chan struct{}, n)
		c.calls = map[string]*call{}
	})
	return c.initErr
}

func (c *Cache) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Cache) allowed(host string) bool {
	if c.Allow != nil {
		return c.Allow(host)
	}
	return HostAllowed(host)
}

func (c *Cache) blobPath(sum string) string {
	return filepath.Join(c.Dir, "blobs", sum[:2], sum)
}

func (c *Cache) indexPath(algo, digest string) string {
	return filepath.Join(c.Dir, "index", algo, digest[:2], digest)
}

func (c *Cache) lookup(d software.Digest) string {
	sum := d.Hex
	if d.Algo != software.SHA256 {
		b, err := os.ReadFile(c.indexPath(d.Algo, d.Hex))
		if err != nil {
			return ""
		}
		sum = strings.TrimSpace(string(b))
		if _, err := software.ParseDigest(software.SHA256, sum); err != nil {
			return ""
		}
	}
	if _, err := os.Stat(c.blobPath(sum)); err != nil {
		return ""
	}
	return sum
}

// ServeHTTP handles /healthz and digest-verified GET /v1/fetch downloads.
func (c *Cache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusOK)
		return
	case "/v1/fetch":
	default:
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	if err := c.Init(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		return
	}
	q := r.URL.Query()
	d, err := software.ParseDigest(q.Get("algo"), q.Get("digest"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_digest", err.Error())
		return
	}
	src, err := url.Parse(q.Get("url"))
	if err != nil || src.Scheme != "https" || src.Host == "" || src.User != nil {
		writeErr(w, http.StatusBadRequest, "bad_url", "url must be an https URL")
		return
	}
	// The host is matched with its port: no official host is named with one.
	if !c.allowed(src.Host) {
		writeErr(w, http.StatusForbidden, "host_not_allowed", "the cache does not download from "+src.Host)
		return
	}

	sum := c.lookup(d)
	hit := sum != ""
	if !hit {
		sum, err = c.download(r.Context(), src.String(), d)
		if err != nil {
			status, code := http.StatusBadGateway, "upstream_failed"
			if errors.Is(err, software.ErrDigestMismatch) {
				code = "digest_mismatch"
			} else if errors.Is(err, errTooLarge) {
				code = "too_large"
			}
			c.logger().Warn("artifact download failed", "url", src.String(), "digest", d.String(), "err", err)
			writeErr(w, status, code, err.Error())
			return
		}
	}
	f, err := os.Open(c.blobPath(sum))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "the file was evicted while it was being served; retry")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	now := time.Now()
	_ = os.Chtimes(c.blobPath(sum), now, now)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Content-SHA256", sum)
	if hit {
		w.Header().Set("X-Felis-Cache", "hit")
	} else {
		w.Header().Set("X-Felis-Cache", "miss")
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, f)
}

var errTooLarge = errors.New("the file is larger than the cache accepts")

// Deduplicate concurrent downloads for the same digest.
func (c *Cache) download(ctx context.Context, src string, d software.Digest) (string, error) {
	key := d.String()
	c.mu.Lock()
	if cl, ok := c.calls[key]; ok {
		c.mu.Unlock()
		select {
		case <-cl.done:
			return cl.sum, cl.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	cl := &call{done: make(chan struct{})}
	c.calls[key] = cl
	c.mu.Unlock()

	// Finish downloads independently of the initiating request because other callers may be waiting.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultTimeout)
	cl.sum, cl.err = c.fetch(dctx, src, d)
	cancel()

	c.mu.Lock()
	delete(c.calls, key)
	c.mu.Unlock()
	close(cl.done)
	return cl.sum, cl.err
}

func (c *Cache) client() *http.Client {
	base := c.Upstream
	if base == nil {
		base = &http.Client{Timeout: DefaultTimeout}
	}
	cl := *base
	cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || !c.allowed(req.URL.Host) {
			return fmt.Errorf("redirect to %s is not allowed", req.URL.Host)
		}
		return nil
	}
	return &cl
}

func (c *Cache) fetch(ctx context.Context, src string, d software.Digest) (string, error) {
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-c.slots }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := newIdleWatch(c.IdleTimeout, cancel)
	defer idle.stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = software.DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	resp, err := c.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	idle.r = resp.Body
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream answered HTTP %d", resp.StatusCode)
	}
	maxFile := c.MaxFileBytes
	if maxFile <= 0 {
		maxFile = DefaultMaxFileBytes
	}
	if resp.ContentLength > maxFile {
		return "", errTooLarge
	}

	tmp, err := os.CreateTemp(filepath.Join(c.Dir, "tmp"), "dl-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	hashes := map[string]hash.Hash{
		software.SHA1:   sha1.New(), //nolint:gosec // see package doc
		software.SHA256: sha256.New(),
		software.SHA512: sha512.New(),
		software.MD5:    md5.New(), //nolint:gosec // see package doc
	}
	writers := []io.Writer{tmp}
	for _, h := range hashes {
		writers = append(writers, h)
	}
	n, err := io.Copy(io.MultiWriter(writers...), io.LimitReader(idle, maxFile+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n > maxFile {
		return "", errTooLarge
	}
	sums := map[string]string{}
	for algo, h := range hashes {
		sums[algo] = hex.EncodeToString(h.Sum(nil))
	}
	if sums[d.Algo] != d.Hex {
		return "", software.ErrDigestMismatch
	}
	sum := sums[software.SHA256]
	if err := c.reserve(n); err != nil {
		return "", err
	}
	dest := c.blobPath(sum)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	for algo, v := range sums {
		if algo == software.SHA256 {
			continue
		}
		p := c.indexPath(algo, v)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
			_ = os.WriteFile(p, []byte(sum+"\n"), 0o644)
		}
	}
	c.logger().Info("artifact cached", "url", src, "sha256", sum, "bytes", n)
	return sum, nil
}

// reserve evicts least recently served blobs until n new bytes fit.
func (c *Cache) reserve(n int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += n
	if c.MaxBytes <= 0 || c.total <= c.MaxBytes {
		return nil
	}
	type blob struct {
		path string
		size int64
		at   time.Time
	}
	var blobs []blob
	_ = filepath.WalkDir(filepath.Join(c.Dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			blobs = append(blobs, blob{p, info.Size(), info.ModTime()})
		}
		return nil
	})
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].at.Before(blobs[j].at) })
	for _, b := range blobs {
		if c.total <= c.MaxBytes {
			break
		}
		if os.Remove(b.path) == nil {
			c.total -= b.size
			c.logger().Info("artifact evicted", "path", filepath.Base(b.path), "bytes", b.size)
		}
	}
	if c.total > c.MaxBytes {
		c.total -= n
		return errTooLarge
	}
	return nil
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}
