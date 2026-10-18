package api

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"felis.lolicon.best/internal/metrics"
)

// Auth limits apply per client network and mail limits install-wide; recipient and account limits alone do not bound spraying.

// RateLimit's zero value disables throttling.
type RateLimit struct {
	Burst     int
	PerMinute float64
}

func (l RateLimit) enabled() bool { return l.Burst > 0 && l.PerMinute > 0 }

// New keys share an overflow bucket at bucketMaxKeys to bound memory during source-address spraying.
const (
	bucketSweepEvery = time.Minute
	bucketMaxKeys    = 50_000
	bucketOverflow   = "\x00overflow"
)

type tokenBucket struct {
	tokens float64
	at     time.Time
	// refusing marks an episode until the next admission, allowing one audit event per episode.
	refusing bool
}

// bucketSet drops fully refilled buckets because they carry no state beyond the default full bucket.
type bucketSet struct {
	mu      sync.Mutex
	now     func() time.Time
	limit   RateLimit
	buckets map[string]*tokenBucket
	swept   time.Time
	maxKeys int
}

func newBucketSet(limit RateLimit, now func() time.Time) *bucketSet {
	return &bucketSet{now: now, limit: limit, buckets: map[string]*tokenBucket{}, maxKeys: bucketMaxKeys}
}

func (s *bucketSet) setLimit(limit RateLimit) {
	s.mu.Lock()
	s.limit = limit
	s.mu.Unlock()
}

// refill requires mu.
func (s *bucketSet) refill(b *tokenBucket, now time.Time) {
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.tokens = math.Min(float64(s.limit.Burst), b.tokens+elapsed.Minutes()*s.limit.PerMinute)
	}
	b.at = now
}

// sweep requires mu; force bypasses the sweep interval.
func (s *bucketSet) sweep(now time.Time, force bool) {
	if !force && now.Sub(s.swept) < bucketSweepEvery {
		return
	}
	s.swept = now
	for k, b := range s.buckets {
		s.refill(b, now)
		if b.tokens >= float64(s.limit.Burst) {
			delete(s.buckets, k)
		}
	}
}

// bucket requires mu and creates missing buckets full.
func (s *bucketSet) bucket(key string, now time.Time) *tokenBucket {
	s.sweep(now, false)
	if b, ok := s.buckets[key]; ok {
		s.refill(b, now)
		return b
	}
	if len(s.buckets) >= s.maxKeys {
		s.sweep(now, true)
		if len(s.buckets) >= s.maxKeys {
			key = bucketOverflow
			if b, ok := s.buckets[key]; ok {
				s.refill(b, now)
				return b
			}
		}
	}
	b := &tokenBucket{tokens: float64(s.limit.Burst), at: now}
	s.buckets[key] = b
	return b
}

// take returns the wait until a token is available; disabled limits always admit.
func (s *bucketSet) take(key string) (bool, time.Duration) {
	ok, wait, _ := s.admit(key)
	return ok, wait
}

// admit reports the first refusal since the last admission to bound refusal audit volume.
func (s *bucketSet) admit(key string) (ok bool, wait time.Duration, first bool) {
	if s == nil || !s.limit.enabled() {
		return true, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	b := s.bucket(key, now)
	if b.tokens >= 1 {
		b.tokens--
		b.refusing = false
		return true, 0, false
	}
	first = !b.refusing
	b.refusing = true
	return false, s.wait(b), first
}

func (s *bucketSet) peek(key string) (bool, time.Duration) {
	if s == nil || !s.limit.enabled() {
		return true, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	b, ok := s.buckets[key]
	if !ok {
		return true, 0
	}
	s.refill(b, now)
	if b.tokens >= 1 {
		return true, 0
	}
	return false, s.wait(b)
}

// wait requires mu.
func (s *bucketSet) wait(b *tokenBucket) time.Duration {
	missing := 1 - b.tokens
	return time.Duration(math.Ceil(missing / s.limit.PerMinute * float64(time.Minute)))
}

func (a *API) authDoorGate() *bucketSet {
	a.authDoorOnce.Do(func() { a.authDoorBuckets = newBucketSet(a.AuthDoorLimit, a.now) })
	return a.authDoorBuckets
}

func (a *API) mailGate() *bucketSet {
	a.mailOnce.Do(func() { a.mailBuckets = newBucketSet(a.MailLimit, a.now) })
	return a.mailBuckets
}

const mailGateKey = "mail"

func (a *API) throttleAuthDoor(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := sourceKey(a.clientIP(r))
		ok, wait, first := a.authDoorGate().admit(key)
		if !ok {
			metrics.RateLimitedTotal.WithLabelValues("auth_door").Inc()
			if first {
				a.auditEntry(r, AuditEntry{Actor: anonymousActor, Action: "auth.rate_limited",
					Payload: auditPayload(map[string]any{"source": key, "door": r.URL.Path})})
			}
			writeError(w, r, newError(http.StatusTooManyRequests, "rate_limited",
				"too many sign-in requests from this network; try again shortly").retryAfter(wait))
			return
		}
		h(w, r)
	}
}

func errMailRateLimited(wait time.Duration) *apiError {
	return newError(http.StatusTooManyRequests, "mail_rate_limited",
		"this server is sending too much mail right now; try again shortly").retryAfter(wait)
}

func errMailUnavailable() *apiError {
	return newError(http.StatusServiceUnavailable, "mail_unavailable",
		"this server has no mail relay configured, so it cannot send codes; sign in with a passkey or ask the server operator to set up email")
}

// checkMailBudget runs before address lookup so missing relay and exhausted budget responses reveal no account existence.
func (a *API) checkMailBudget(ctx context.Context) error {
	if a.mailer(ctx) == nil {
		return errMailUnavailable()
	}
	if ok, wait := a.mailGate().peek(mailGateKey); !ok {
		metrics.MailTotal.WithLabelValues("otp", "throttled").Inc()
		return errMailRateLimited(wait)
	}
	return nil
}

// clientIP trusts the configured edge header; X-Forwarded-For uses the rightmost entry appended by that proxy.
func (a *API) clientIP(r *http.Request) netip.Addr {
	if name := a.ClientIPHeader; name != "" {
		if v := r.Header.Get(name); v != "" {
			if strings.EqualFold(name, "X-Forwarded-For") {
				if i := strings.LastIndexByte(v, ','); i >= 0 {
					v = v[i+1:]
				}
			}
			if ip, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
				return ip.Unmap()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}

// sourceKey groups IPv4 by host and IPv6 by /64; malformed addresses share a key.
func sourceKey(ip netip.Addr) string {
	switch {
	case !ip.IsValid():
		return "unknown"
	case ip.Is4():
		return ip.String()
	default:
		p, _ := ip.Prefix(64)
		return p.String()
	}
}

// challengeSource groups IPv6 by /48 so one site cannot spread challenge creation across its /64 auth-limit buckets.
func challengeSource(ip netip.Addr) string {
	switch {
	case !ip.IsValid():
		return "unknown"
	case ip.Is4():
		return ip.String()
	default:
		p, _ := ip.Prefix(48)
		return p.String()
	}
}
