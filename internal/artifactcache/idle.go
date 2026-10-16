package artifactcache

import (
	"io"
	"time"
)

// DefaultIdleTimeout aborts a transfer that has received nothing for this
// long. Downloads have an overall timeout as well, but a CDN connection that
// stalls halfway (seen live from fill-data.papermc.io: 26 MB of 51, then
// nothing) would otherwise hold an install for the whole of it.
const DefaultIdleTimeout = 60 * time.Second

// idleReader cancels its transfer when Read makes no progress for d. The timer
// starts when it is made, so it also bounds the wait for the response headers.
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
