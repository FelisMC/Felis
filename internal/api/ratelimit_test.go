package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// These pin the two volumetric limits in front of the public sign-in doors:
// the per-client-address bucket (keyed on the edge's visitor header only when
// the install names it) and the install-wide mail budget, which must refuse
// every address alike so it never becomes an existence oracle. They also pin
// that neither the buckets nor the OTP cooldown map grow without bound.

func TestBucketSetBurstRefillAndWait(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	s := newBucketSet(RateLimit{Burst: 3, PerMinute: 6}, func() time.Time { return clock })
	for i := 0; i < 3; i++ {
		if ok, _ := s.take("a"); !ok {
			t.Fatalf("take %d refused inside the burst", i+1)
		}
	}
	ok, wait := s.take("a")
	if ok || wait != 10*time.Second {
		t.Fatalf("4th take = %v wait %v, want refused with 10s wait (6/min)", ok, wait)
	}
	if ok, _ := s.take("b"); !ok {
		t.Fatal("another key shares a's bucket")
	}
	if ok, _ := s.peek("a"); ok {
		t.Fatal("peek admitted an empty bucket")
	}
	clock = clock.Add(10 * time.Second)
	if ok, _ := s.peek("a"); !ok {
		t.Fatal("peek refused after one token refilled")
	}
	if ok, _ := s.take("a"); !ok {
		t.Fatal("take refused after one token refilled (peek must not spend it)")
	}
	if ok, _ := s.take("a"); ok {
		t.Fatal("a second token appeared from nowhere")
	}
}

func TestBucketSetDropsIdleKeysAndCapsTheMap(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	s := newBucketSet(RateLimit{Burst: 2, PerMinute: 60}, func() time.Time { return clock })
	s.maxKeys = 100
	for i := 0; i < 100; i++ {
		s.take(fmt.Sprintf("10.0.0.%d", i))
	}
	// At the cap with every bucket still draining: fresh keys share the
	// overflow bucket instead of growing the map.
	s.take("fresh-1")
	s.take("fresh-2")
	if ok, _ := s.take("fresh-3"); ok {
		t.Fatal("overflow bucket admitted past its burst")
	}
	if n := len(s.buckets); n != 101 {
		t.Fatalf("map holds %d buckets, want 100 + overflow", n)
	}
	// Once they refill, the sweep drops them all.
	clock = clock.Add(2 * bucketSweepEvery)
	s.take("later")
	if n := len(s.buckets); n != 1 {
		t.Fatalf("after refill the map holds %d buckets, want only the new one", n)
	}
}

func TestDisabledLimitAdmitsEverything(t *testing.T) {
	var nilSet *bucketSet
	if ok, _ := nilSet.take("x"); !ok {
		t.Fatal("nil set refused")
	}
	s := newBucketSet(RateLimit{}, time.Now)
	for i := 0; i < 1000; i++ {
		if ok, _ := s.take("x"); !ok {
			t.Fatal("zero limit refused")
		}
	}
}

func TestClientIPTrustsOnlyTheNamedHeader(t *testing.T) {
	req := func(remote string, h map[string]string) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = remote
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	for _, tc := range []struct {
		name   string
		header string
		r      *http.Request
		want   string
	}{
		{"no header configured ignores CF-Connecting-IP", "", req("10.42.0.1:5000", map[string]string{"CF-Connecting-IP": "203.0.113.9"}), "10.42.0.1"},
		{"cloudflare header", "CF-Connecting-IP", req("10.42.0.1:5000", map[string]string{"CF-Connecting-IP": "203.0.113.9"}), "203.0.113.9"},
		{"cloudflare header absent falls back to peer", "CF-Connecting-IP", req("10.42.0.1:5000", nil), "10.42.0.1"},
		{"garbage header falls back to peer", "CF-Connecting-IP", req("10.42.0.1:5000", map[string]string{"CF-Connecting-IP": "not-an-ip"}), "10.42.0.1"},
		{"X-Forwarded-For takes the proxy-appended rightmost hop", "X-Forwarded-For", req("10.42.0.1:5000", map[string]string{"X-Forwarded-For": "1.1.1.1, 198.51.100.7"}), "198.51.100.7"},
		{"v4-mapped v6 is unmapped", "CF-Connecting-IP", req("10.42.0.1:5000", map[string]string{"CF-Connecting-IP": "::ffff:203.0.113.9"}), "203.0.113.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &API{ClientIPHeader: tc.header}
			if got := a.clientIP(tc.r).String(); got != tc.want {
				t.Fatalf("clientIP = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSourceKeyGroupsIPv6By64(t *testing.T) {
	a := sourceKey(netip.MustParseAddr("2001:db8:1:2::1"))
	b := sourceKey(netip.MustParseAddr("2001:db8:1:2:ffff::9"))
	c := sourceKey(netip.MustParseAddr("2001:db8:1:3::1"))
	if a != b || a == c {
		t.Fatalf("keys %q %q %q: want one per /64", a, b, c)
	}
	if k := sourceKey(netip.MustParseAddr("203.0.113.9")); k != "203.0.113.9" {
		t.Fatalf("v4 key = %q", k)
	}
}

func TestAuthDoorsThrottlePerClientAddress(t *testing.T) {
	api, _, _ := seedLoginEmailAPI(t)
	api.AuthDoorLimit = RateLimit{Burst: 3, PerMinute: 3}
	api.ClientIPHeader = "CF-Connecting-IP"
	eh := api.ExternalHandler()
	from := func(ip string) map[string]string {
		return map[string]string{"Content-Type": "application/json", "CF-Connecting-IP": ip}
	}
	for i := 0; i < 3; i++ {
		if w := do(eh, "POST", "/api/v1/auth/options", `{"email":"x@example.net"}`, from("203.0.113.9")); w.Code != http.StatusOK {
			t.Fatalf("call %d = %d (%s)", i+1, w.Code, w.Body.String())
		}
	}
	// The bucket spans every door: a different door from the same address is
	// refused too.
	w := do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"x@example.net","code":"000000"}`, from("203.0.113.9"))
	if c, _ := errEnvelope(t, w); w.Code != http.StatusTooManyRequests || c != "rate_limited" {
		t.Fatalf("4th call = %d %s, want 429 rate_limited", w.Code, c)
	}
	if ra := w.Header().Get("Retry-After"); ra != "20" {
		t.Fatalf("Retry-After = %q, want 20 (3/min)", ra)
	}
	if w := do(eh, "POST", "/api/v1/auth/options", `{"email":"x@example.net"}`, from("198.51.100.7")); w.Code != http.StatusOK {
		t.Fatalf("another address was throttled: %d", w.Code)
	}
	// The op-login poll and logout are not doors: a waiting browser polls.
	for i := 0; i < 5; i++ {
		if w := do(eh, "GET", "/api/v1/auth/op-login/status/abc", "", from("203.0.113.9")); w.Code == http.StatusTooManyRequests {
			t.Fatal("op-login status poll was throttled")
		}
	}
}

func TestAuthDoorsIgnoreVisitorHeaderUnlessConfigured(t *testing.T) {
	api, _, _ := seedLoginEmailAPI(t)
	api.AuthDoorLimit = RateLimit{Burst: 2, PerMinute: 2}
	eh := api.ExternalHandler()
	// Without client_ip_header a forged CF-Connecting-IP must not mint fresh
	// buckets: every call below comes from httptest's one peer address.
	for i := 0; i < 2; i++ {
		do(eh, "POST", "/api/v1/auth/options", `{"email":"x@example.net"}`,
			map[string]string{"Content-Type": "application/json", "CF-Connecting-IP": fmt.Sprintf("203.0.113.%d", i)})
	}
	w := do(eh, "POST", "/api/v1/auth/options", `{"email":"x@example.net"}`,
		map[string]string{"Content-Type": "application/json", "CF-Connecting-IP": "203.0.113.200"})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("forged visitor header escaped the limit: %d", w.Code)
	}
}

func TestMailBudgetRefusesEveryAddressAlike(t *testing.T) {
	api, repo, mailer := seedLoginEmailAPI(t)
	repo.staff["second"] = &StaffUser{ID: "u2", Username: "second", Email: "second@example.net", Role: "user", EmailVerified: true}
	api.MailLimit = RateLimit{Burst: 1, PerMinute: 1}
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	eh := api.ExternalHandler()
	start := func(email string) *httptest.ResponseRecorder {
		return do(eh, "POST", "/api/v1/auth/email/start", `{"email":"`+email+`"}`, jsonHeader)
	}

	if w := start("player@example.net"); w.Code != http.StatusAccepted || mailer.calls != 1 {
		t.Fatalf("first send = %d, %d mails", w.Code, mailer.calls)
	}
	// Budget spent: a real account and an unknown address get the same 429,
	// and nothing reaches the relay.
	for _, email := range []string{"second@example.net", "nobody@example.net"} {
		w := start(email)
		if c, _ := errEnvelope(t, w); w.Code != http.StatusTooManyRequests || c != "mail_rate_limited" || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s while the budget is spent = %d %s (Retry-After %q)", email, w.Code, c, w.Header().Get("Retry-After"))
		}
	}
	if mailer.calls != 1 {
		t.Fatalf("a spent budget still mailed: %d sends", mailer.calls)
	}
	// The refused starts did not burn their recipients' cooldowns.
	clock = clock.Add(time.Minute)
	if w := start("second@example.net"); w.Code != http.StatusAccepted || mailer.calls != 2 {
		t.Fatalf("after refill = %d, %d mails", w.Code, mailer.calls)
	}
}

func TestSignedInDoorMailBudget(t *testing.T) {
	repo := newFakeRepo()
	repo.staff["player"] = &StaffUser{ID: "u1", Username: "player"}
	api := newTestAPI(repo, newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "u1", Email: "u1@example.net", Role: "user"}}
	mailer := &captureMailer{}
	api.Mailer = mailer
	// One mail per two minutes, so stepping past the per-principal resend
	// cooldown leaves the mail budget still empty.
	api.MailLimit = RateLimit{Burst: 1, PerMinute: 0.5}
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	eh := api.ExternalHandler()
	if w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"a@example.net"}`, nil); w.Code != http.StatusAccepted {
		t.Fatalf("first = %d (%s)", w.Code, w.Body.String())
	}
	clock = clock.Add(otpResendCooldown + time.Second)
	w := do(eh, "POST", "/api/v1/account/email/start", `{"email":"b@example.net"}`, nil)
	if c, _ := errEnvelope(t, w); w.Code != http.StatusTooManyRequests || c != "mail_rate_limited" {
		t.Fatalf("second = %d %s, want 429 mail_rate_limited", w.Code, c)
	}
	if mailer.calls != 1 {
		t.Fatalf("mails = %d", mailer.calls)
	}
}

func TestCooldownLimiterForgetsExpiredKeys(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	c := &cooldownLimiter{now: func() time.Time { return clock }, last: map[string]time.Time{}}
	for i := 0; i < 500; i++ {
		if _, ok := c.reserve(fmt.Sprintf("login:email:user%d@example.net", i), time.Minute); !ok {
			t.Fatalf("reserve %d refused", i)
		}
	}
	if _, ok := c.reserve("login:email:user7@example.net", time.Minute); ok {
		t.Fatal("a live reservation was forgotten")
	}
	clock = clock.Add(time.Minute + bucketSweepEvery)
	c.reserve("login:email:fresh@example.net", time.Minute)
	if n := len(c.last); n != 1 {
		t.Fatalf("after every window ended the map holds %d keys, want 1", n)
	}
}

func TestMailLimitCountsNotices(t *testing.T) {
	// The lock notice spends the same budget as codes; with none left it is
	// skipped rather than sent.
	api, _, _ := seedLoginEmailAPI(t)
	mailer := &noticeMailer{}
	api.Mailer = mailer
	api.MailLimit = RateLimit{Burst: 1, PerMinute: 1}
	api.mailGate().take(mailGateKey)
	r := httptest.NewRequest("POST", "/", strings.NewReader(""))
	api.noteOTPLock(r, &OTPAccountLockedError{Until: api.now().Add(time.Hour), JustLocked: true}, "u1", otpPurposeLogin)
	if len(mailer.notices) != 0 {
		t.Fatalf("notice sent past a spent budget: %q", mailer.notices)
	}
}
