package api

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"felis.lolicon.best/internal/metrics"
)

// Volumetric limits for the public auth doors and for outbound mail.
//
// The per-recipient OTP cooldown (otpLimiter) stops one mailbox being bombed,
// and the per-account wrong-code budget stops one account being guessed. Neither
// bounds a caller who sprays many addresses or many accounts, so two more limits
// sit in front of them:
//
//   - authDoorGate: a token bucket per client address over every pre-session
//     auth door (options, login, op-login, bind, setup redeem). The client
//     address comes from the edge's header only when the install says which
//     header its edge writes (ClientIPHeader); with Cloudflare that header is
//     CF-Connecting-IP, which is trustworthy because the edge setup fences the
//     panel NodePort to loopback, so every request reaching the origin came
//     through cloudflared. IPv6 clients are bucketed per /64, the unit one
//     subscriber is handed.
//   - mailGate: one install-wide bucket over every mail the API sends (codes
//     and lock notices), so no flood can burn the SMTP relay's quota and get
//     the sending account suspended. The public doors check it before they
//     resolve the address, so a spent budget answers every address alike.

// RateLimit is a token bucket: Burst calls at once, refilled at PerMinute. The
// zero value disables the limit.
type RateLimit struct {
	Burst     int
	PerMinute float64
}

func (l RateLimit) enabled() bool { return l.Burst > 0 && l.PerMinute > 0 }

// bucketSweepEvery is how often idle buckets are dropped; bucketMaxKeys bounds
// the map between sweeps. Past the bound, new keys share one overflow bucket,
// so a spray of fresh source addresses throttles itself instead of growing the
// map.
const (
	bucketSweepEvery = time.Minute
	bucketMaxKeys    = 50_000
	bucketOverflow   = "\x00overflow"
)

type tokenBucket struct {
	tokens float64
	at     time.Time
	// refusing is set from a refusal until the next admission, so one episode
	// of refusals can be recorded once.
	refusing bool
}

// bucketSet is a set of token buckets keyed by caller. A missing key is a full
// bucket, so a bucket that has refilled completely carries no information and
// is dropped by the sweep; memory is bounded by the keys active in the last
// refill period.
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

// refill brings b up to now. The caller holds mu.
func (s *bucketSet) refill(b *tokenBucket, now time.Time) {
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.tokens = math.Min(float64(s.limit.Burst), b.tokens+elapsed.Minutes()*s.limit.PerMinute)
	}
	b.at = now
}

// sweep drops full buckets at most once per bucketSweepEvery, or at once when
// force is set. The caller holds mu.
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

// bucket returns key's bucket, creating a full one. The caller holds mu.
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

// take spends one token from key's bucket. When none is left it reports how
// long until one is. A disabled limit always admits.
func (s *bucketSet) take(key string) (bool, time.Duration) {
	ok, wait, _ := s.admit(key)
	return ok, wait
}

// admit is take that also reports whether a refusal is the first since key was
// last admitted, so a flood leaves one audit row per episode instead of one
// per refused request.
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

// peek reports whether key's bucket holds a token, without spending it.
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

// wait is how long until b holds one token. The caller holds mu.
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

// mailGateKey is the single install-wide mail bucket.
const mailGateKey = "mail"

// throttleAuthDoor applies the per-source bucket to one public auth door.
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

// errMailRateLimited answers a door whose mail the install-wide budget refuses.
func errMailRateLimited(wait time.Duration) *apiError {
	return newError(http.StatusTooManyRequests, "mail_rate_limited",
		"this server is sending too much mail right now; try again shortly").retryAfter(wait)
}

// checkMailBudget is the public doors' pre-resolution check: it refuses every
// address alike while the budget is spent, so the refusal says nothing about
// whether the address has an account.
func (a *API) checkMailBudget() error {
	if a.Mailer == nil {
		return nil
	}
	if ok, wait := a.mailGate().peek(mailGateKey); !ok {
		metrics.MailTotal.WithLabelValues("otp", "throttled").Inc()
		return errMailRateLimited(wait)
	}
	return nil
}

// clientIP is the caller's address: the edge's header when the install names
// one and the request carries a parseable value, else the TCP peer. For
// X-Forwarded-For the rightmost entry is used, the one the trusted proxy
// appended itself.
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

// sourceKey buckets an address: IPv4 per host, IPv6 per /64. An unparseable
// address shares one key.
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
