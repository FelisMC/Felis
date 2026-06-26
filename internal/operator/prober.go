package operator

import (
	"context"
	"time"

	"felis.lolicon.best/internal/rcon"
)

// Prober reports whether a server's RCON endpoint is reachable and accepts the
// password. A nil error is the loader-agnostic readiness gate (spec §5). It is
// an interface so the reconciler can be tested without a live server.
type Prober interface {
	Probe(ctx context.Context, addr, password string) error
}

// RconProber is the production Prober: a successful Dial (TCP connect + auth)
// is sufficient; the connection is closed immediately.
type RconProber struct {
	// Timeout bounds a single probe. Defaults to 5s.
	Timeout time.Duration
}

// Probe dials addr and authenticates with password, honoring the smaller of the
// configured timeout and any deadline already on ctx.
func (p RconProber) Probe(ctx context.Context, addr, password string) error {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	conn, err := rcon.Dial(addr, password, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}
