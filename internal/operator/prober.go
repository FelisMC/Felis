package operator

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"felis.lolicon.best/internal/rcon"
)

// PlayerCount is a server's online/max player tally as read from RCON `list`.
// Both fields are zero when the count could not be read; that is not an error,
// only the absence of a fresh sample (see Prober).
type PlayerCount struct {
	Online int32
	Max    int32
}

// Prober reports whether a server's RCON endpoint is reachable and accepts the
// password, and best-effort returns its current player tally. A nil error is the
// loader-agnostic readiness gate (spec §5); the PlayerCount is advisory and is
// zero (with a nil error) whenever the tally could not be sampled. It is an
// interface so the reconciler can be tested without a live server.
type Prober interface {
	Probe(ctx context.Context, addr, password string) (PlayerCount, error)
}

// RconProber is the production Prober: a successful Dial (TCP connect + auth)
// is the readiness gate; on that same connection it then runs `list` to sample
// the player tally before closing.
type RconProber struct {
	// Timeout bounds a single probe. Defaults to 5s.
	Timeout time.Duration
}

// Probe dials addr and authenticates with password, honoring the smaller of the
// configured timeout and any deadline already on ctx. Auth success gates
// readiness; the player tally is then read with `list` on a best-effort basis —
// a failed or unparseable `list` yields a zero PlayerCount, never a probe error,
// so a transient count-read hiccup can never flap a healthy server out of Ready.
func (p RconProber) Probe(ctx context.Context, addr, password string) (PlayerCount, error) {
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
		return PlayerCount{}, err
	}
	defer conn.Close()

	// Readiness is already established by the successful Dial+auth above. Reading
	// the tally must not jeopardize that verdict, so its errors are swallowed.
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return PlayerCount{}, nil
	}
	reply, err := conn.Execute("list")
	if err != nil {
		return PlayerCount{}, nil
	}
	pc, _ := parseListReply(reply)
	return pc, nil
}

// listReplyPattern matches the vanilla/Paper `list` response, e.g.
// "There are 3 of a max of 20 players online: alice, bob, carol". The search is
// unanchored so leading color codes or trailing player names do not defeat it.
var listReplyPattern = regexp.MustCompile(`There are (\d+) of a max of (\d+) players online`)

// parseListReply extracts the online/max tally from a `list` reply. ok is false
// (and the PlayerCount zero) when the reply does not match the known format, so
// callers can distinguish "no sample" from a genuine "0 of N".
func parseListReply(reply string) (PlayerCount, bool) {
	m := listReplyPattern.FindStringSubmatch(reply)
	if m == nil {
		return PlayerCount{}, false
	}
	online, err := strconv.ParseInt(m[1], 10, 32)
	if err != nil {
		return PlayerCount{}, false
	}
	max, err := strconv.ParseInt(m[2], 10, 32)
	if err != nil {
		return PlayerCount{}, false
	}
	return PlayerCount{Online: int32(online), Max: int32(max)}, true
}
