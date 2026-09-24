package operator

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"felis.lolicon.best/internal/rcon"
)

// PlayerCount is a server's online/max player tally as read from RCON `list`.
// Known is false when the count could not be read (the command failed, or its
// reply matched no format below); that is not a probe error, only the absence
// of a fresh sample. The zero value is "unknown", so a caller that forgets to
// check can never mistake a failed read for an empty server.
type PlayerCount struct {
	Online int32
	Max    int32
	Known  bool
}

// Prober is the operator's RCON channel into a running server. It is an interface
// so the reconciler can be tested without a live server.
//
// Probe reports whether the endpoint is reachable and accepts the password, and
// best-effort returns the current player tally. A nil error is the loader-agnostic
// readiness gate (spec §5); the PlayerCount is advisory and has Known=false (with
// a nil error) whenever the tally could not be sampled.
//
// Save flushes the world to disk (`save-all flush`) and returns once the server
// has answered, i.e. once the save is done. The reconciler runs it right before
// scaling a server to zero (spec §7).
type Prober interface {
	Probe(ctx context.Context, addr, password string) (PlayerCount, error)
	Save(ctx context.Context, addr, password string) error
}

// RconProber is the production Prober: a successful Dial (TCP connect + auth)
// is the readiness gate; on that same connection it then runs `list` to sample
// the player tally before closing.
type RconProber struct {
	// Timeout bounds a single probe, and the connect+auth step of a save.
	// Defaults to 5s.
	Timeout time.Duration
	// SaveTimeout bounds the wait for `save-all flush` to answer. Defaults to
	// defaultSaveTimeout.
	SaveTimeout time.Duration
}

// defaultSaveTimeout is how long a stop waits for the pre-stop save. The server
// answers `save-all flush` only after every loaded chunk is written, which takes
// seconds on a large world. Past this the stop goes ahead anyway: SIGTERM runs the
// server's own shutdown save within the pod's grace period, so the explicit save
// only moves most of that work ahead of the kill deadline.
const defaultSaveTimeout = 30 * time.Second

// boundTimeout returns d (or def when d is unset), shortened to ctx's deadline.
func boundTimeout(ctx context.Context, d, def time.Duration) time.Duration {
	if d <= 0 {
		d = def
	}
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 && remaining < d {
			d = remaining
		}
	}
	return d
}

// Probe dials addr and authenticates with password, honoring the smaller of the
// configured timeout and any deadline already on ctx. Auth success gates
// readiness; the player tally is then read with `list` on a best-effort basis —
// a failed or unparseable `list` yields an unknown PlayerCount, never a probe error,
// so a transient count-read hiccup can never flap a healthy server out of Ready.
func (p RconProber) Probe(ctx context.Context, addr, password string) (PlayerCount, error) {
	timeout := boundTimeout(ctx, p.Timeout, 5*time.Second)
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

// Save runs `save-all flush` and waits for its reply. The reply text is not
// checked: vanilla, Paper and the modded loaders word it differently, and any
// reply at all means the command ran to completion on the server thread.
func (p RconProber) Save(ctx context.Context, addr, password string) error {
	conn, err := rcon.Dial(addr, password, boundTimeout(ctx, p.Timeout, 5*time.Second))
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(boundTimeout(ctx, p.SaveTimeout, defaultSaveTimeout))); err != nil {
		return err
	}
	_, err = conn.Execute("save-all flush")
	return err
}

// listReplyPatterns match the `list` replies of the loaders Felis runs, tried in
// order against the reply with § color codes stripped:
//
//   - vanilla 1.13+ / Paper / Fabric / Forge:
//     "There are 3 of a max of 20 players online: alice, bob, carol"
//   - vanilla 1.12 and older, Bukkit's own list:
//     "There are 3/20 players online:"
//   - EssentialsX (its /list replaces the vanilla one, RCON included); with
//     vanished players it prints visible/hidden, and both count as online:
//     "There are 3 out of maximum 20 players online."
//     "There are 3/1 out of maximum 20 players online."
//
// Each has groups (online, hidden, max); hidden is empty where the format has
// none. The search is unanchored so trailing player names do not defeat it.
var listReplyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`There are (\d+)() of a max(?:imum)? of (\d+) players online`),
	regexp.MustCompile(`There are (\d+)(?:/(\d+))? out of (?:a )?maximum (?:of )?(\d+) players online`),
	regexp.MustCompile(`There are (\d+)()/(\d+) players online`),
}

// colorCode matches a legacy § formatting code (color, bold, reset, ...).
var colorCode = regexp.MustCompile(`(?i)§[0-9a-fk-orx]`)

// parseListReply extracts the online/max tally from a `list` reply. ok is false
// (and the PlayerCount unknown) when the reply matches none of the known
// formats, so callers can distinguish "no sample" from a genuine "0 of N".
func parseListReply(reply string) (PlayerCount, bool) {
	plain := colorCode.ReplaceAllString(reply, "")
	for _, re := range listReplyPatterns {
		m := re.FindStringSubmatch(plain)
		if m == nil {
			continue
		}
		online, err := strconv.ParseInt(m[1], 10, 32)
		if err != nil {
			return PlayerCount{}, false
		}
		if m[2] != "" {
			hidden, err := strconv.ParseInt(m[2], 10, 32)
			if err != nil {
				return PlayerCount{}, false
			}
			online += hidden
		}
		max, err := strconv.ParseInt(m[3], 10, 32)
		if err != nil {
			return PlayerCount{}, false
		}
		return PlayerCount{Online: int32(online), Max: int32(max), Known: true}, true
	}
	return PlayerCount{}, false
}
