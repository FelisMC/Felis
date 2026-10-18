package artifactcache

import (
	"io"
	"time"
)

// DefaultIdleTimeout bounds transfers that stop receiving data.
const DefaultIdleTimeout = 60 * time.Second

// idleReader also bounds the wait for response headers.
type idleReader struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func newIdleWatch(d time.Duration, cancel func()) *idleReader {
	if d <= 0 {
		d = DefaultIdleTimeout
	}
	return &idleReader{t: time.AfterFunc(d, cancel), d: d}
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.t.Reset(i.d)
	}
	return n, err
}

func (i *idleReader) stop() { i.t.Stop() }
