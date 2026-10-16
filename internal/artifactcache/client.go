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

// DefaultMaxDirectBytes caps a download whose size upstream did not publish.
const DefaultMaxDirectBytes = 2 << 30

// Client is the installer's Fetcher: it asks the cache first and goes to the
// upstream URL itself when the cache is not configured, unreachable, or answers
// with anything but the file. Either way the bytes are hashed here and must
// match the digest, so what reaches the volume never depends on trusting the
// cache.
type Client struct {
	// CacheURL is the cache's base URL; empty downloads directly.
	CacheURL string
	// HTTP talks to the cache; Direct to upstream. Nil uses a client with
	// DefaultTimeout.
	HTTP      *http.Client
	Direct    *http.Client
	UserAgent string
	// IdleTimeout aborts a transfer that receives nothing for this long
	// (default DefaultIdleTimeout).
	IdleTimeout time.Duration
	Log         io.Writer
}

func (c *Client) logf(format string, a ...any) {
	if c.Log != nil {
		fmt.Fprintf(c.Log, format+"\n", a...)
	}
}

// Fetch implements software.Fetcher.
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
		// A miss makes the cache download the whole file before it answers,
		// so no byte arrives here until it has; the cache watches its own
		// upstream for stalls, and this request waits out its overall timeout.
		err := c.get(ctx, c.HTTP, via, f, dest, DefaultTimeout)
		if err == nil {
			return nil
		}
		c.logf("felis install-server: the cache could not serve %s (%v); downloading it directly", f.Path, err)
	}
	// One retry: a dropped or stalled connection is the usual failure, and the
	// next attempt usually lands on a healthy edge.
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
