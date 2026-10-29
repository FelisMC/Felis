package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"felis.lolicon.best/internal/rcon"
)

// PlayerCount.Known=false means unreadable, not empty, and does not imply a readiness failure.
type PlayerCount struct {
	Online int32
	Max    int32
	Known  bool
	// Stalled means RCON took the command but the server never answered, as when its main thread hangs.
	Stalled bool
}

// Prober readiness depends on RCON authentication; player counts are advisory, while Save waits for command completion.
type Prober interface {
	Probe(ctx context.Context, addr, password string) (PlayerCount, error)
	Save(ctx context.Context, addr, password string) error
	Commands(ctx context.Context, addr, password string, commands []string) error
	Broadcast(ctx context.Context, addr, password, text string) error
}

type RconProber struct {
	Timeout     time.Duration
	SaveTimeout time.Duration
	// Pool keeps a session per server between probes; nil opens one per call.
	Pool *rcon.Pool[*rcon.Conn]
}

// keptSessionIdle outlasts the slowest probe interval, so a running server keeps one session.
const keptSessionIdle = 3 * time.Minute

func NewRconProber() RconProber {
	return RconProber{Pool: rcon.NewPool(rcon.Dial, keptSessionIdle)}
}

// defaultSaveTimeout bounds the explicit flush; timeout still permits normal SIGTERM shutdown saving within pod grace.
const defaultSaveTimeout = 30 * time.Second

const stopCommandsTimeout = 30 * time.Second

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

// session runs fn on the server's session; repeatable commands get a second try when a kept session failed.
func (p RconProber) session(ctx context.Context, addr, password string, repeatable bool, fn func(*rcon.Conn) error) error {
	timeout := boundTimeout(ctx, p.Timeout, 5*time.Second)
	if p.Pool == nil {
		conn, err := rcon.Dial(addr, password, timeout)
		if err != nil {
			return fmt.Errorf("%w: %w", rcon.ErrDial, err)
		}
		defer conn.Close()
		return fn(conn)
	}
	if repeatable {
		return p.Pool.DoAgain(ctx, addr, password, timeout, fn)
	}
	return p.Pool.Do(ctx, addr, password, timeout, fn)
}

// Probe returns success after authentication even when list fails or cannot be parsed, avoiding readiness flaps from count sampling.
func (p RconProber) Probe(ctx context.Context, addr, password string) (PlayerCount, error) {
	timeout := boundTimeout(ctx, p.Timeout, 5*time.Second)
	var pc PlayerCount
	err := p.session(ctx, addr, password, true, func(conn *rcon.Conn) error {
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		reply, err := conn.Execute("list")
		if err != nil {
			var timeout net.Error
			pc.Stalled = errors.As(err, &timeout) && timeout.Timeout()
			return err
		}
		pc, _ = parseListReply(reply)
		return nil
	})
	if errors.Is(err, rcon.ErrDial) {
		return PlayerCount{}, err
	}
	if err != nil {
		return PlayerCount{Stalled: pc.Stalled}, nil
	}
	return pc, nil
}

// Save accepts any reply because loader-specific wording differs; a response marks completion on the server thread.
func (p RconProber) Save(ctx context.Context, addr, password string) error {
	return p.session(ctx, addr, password, true, func(conn *rcon.Conn) error {
		if err := conn.SetDeadline(time.Now().Add(boundTimeout(ctx, p.SaveTimeout, defaultSaveTimeout))); err != nil {
			return err
		}
		_, err := conn.Execute("save-all flush")
		return err
	})
}

func (p RconProber) Commands(ctx context.Context, addr, password string, commands []string) error {
	if len(commands) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, stopCommandsTimeout)
	defer cancel()
	return p.session(ctx, addr, password, false, func(conn *rcon.Conn) error {
		deadline, _ := ctx.Deadline()
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
		for _, command := range commands {
			if _, err := conn.ExecuteContext(ctx, command); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p RconProber) Broadcast(ctx context.Context, addr, password, text string) error {
	timeout := boundTimeout(ctx, p.Timeout, 5*time.Second)
	return p.session(ctx, addr, password, false, func(conn *rcon.Conn) error {
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		_, err := conn.Execute(tellrawCommand(text))
		return err
	})
}

// tellrawCommand escapes non-ASCII as JSON Unicode so RCON implementations without UTF-8 support preserve chat text.
func tellrawCommand(text string) string {
	component, _ := json.Marshal(map[string]string{"text": text, "color": "yellow"})
	var b strings.Builder
	b.WriteString("tellraw @a ")
	for _, r := range string(component) {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// listReplyPatterns are unanchored and capture online, hidden and maximum counts; hidden players count as online.
var listReplyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`There are (\d+)() of a max(?:imum)? of (\d+) players online`),
	regexp.MustCompile(`There are (\d+)(?:/(\d+))? out of (?:a )?maximum (?:of )?(\d+) players online`),
	regexp.MustCompile(`There are (\d+)()/(\d+) players online`),
}

var colorCode = regexp.MustCompile(`(?i)§[0-9a-fk-orx]`)

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
