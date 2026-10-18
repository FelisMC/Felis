package artifactcache

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"felis.lolicon.best/internal/software"
)

// DefaultMaxDirectBytes caps downloads without a published size.
const DefaultMaxDirectBytes = 2 << 30

// Client falls back to upstream on cache failure and always verifies the downloaded digest.
type Client struct {
	// Empty CacheURL downloads directly.
	CacheURL string
	// Nil HTTP or Direct clients use DefaultTimeout.
	HTTP      *http.Client
	Direct    *http.Client
	UserAgent string
	// IdleTimeout defaults to DefaultIdleTimeout.
	IdleTimeout time.Duration
	Log         io.Writer
}

func (c *Client) logf(format string, a ...any) {
	if c.Log != nil {
		fmt.Fprintf(c.Log, format+"\n", a...)
	}
}

func (c *Client) Fetch(ctx context.Context, f software.File, dest string) error {
	if !f.Digest.Valid() {
		return fmt.Errorf("artifactcache: %s has no valid digest", f.Path)
	}
	if c.CacheURL != "" {
		q := url.Values{}
		q.Set("url", f.URL)
		q.Set("algo", f.Digest.Algo)
		q.Set("digest", f.Digest.Hex)
		via := strings.TrimRight(c.CacheURL, "/") + "/v1/fetch?" + q.Encode()
		// Cache misses buffer the upstream file before responding, so only the overall timeout applies here.
		err := c.get(ctx, c.HTTP, via, f, dest, DefaultTimeout)
		if err == nil {
			return nil
		}
		c.logf("felis install-server: the cache could not serve %s (%v); downloading it directly", f.Path, err)
	}
	// Retry once for transient connection failures.
	err := c.get(ctx, c.Direct, f.URL, f, dest, c.IdleTimeout)
	if err == nil || errors.Is(err, software.ErrDigestMismatch) || ctx.Err() != nil {
		return err
	}
	c.logf("felis install-server: downloading %s failed (%v); retrying", f.Path, err)
	return c.get(ctx, c.Direct, f.URL, f, dest, c.IdleTimeout)
}

func (c *Client) get(ctx context.Context, hc *http.Client, src string, f software.File, dest string, idleAfter time.Duration) error {
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := newIdleWatch(idleAfter, cancel)
	defer idle.stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = software.DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	idle.r = resp.Body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	limit := int64(DefaultMaxDirectBytes)
	if f.Size > 0 {
		limit = f.Size
	}
	h, err := software.NewHash(f.Digest.Algo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".felis-dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(idle, limit+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("larger than its published size")
	}
	if hex.EncodeToString(h.Sum(nil)) != f.Digest.Hex {
		return software.ErrDigestMismatch
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}
