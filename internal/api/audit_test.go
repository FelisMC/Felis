package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"felis.lolicon.best/internal/metrics"
)

// Audit attribution: a row names the acting account by id and never by an
// address the caller merely asserted; refused sign-ins, throttling and logouts
// leave rows; a failed write is counted instead of vanishing.

func TestAuditActorIgnoresUnverifiedEmail(t *testing.T) {
	cases := []struct {
		name string
		p    *Principal
		want string
	}{
		{"verified session email", &Principal{UserID: "u1", Username: "alice", Email: "alice@example.net", EmailVerified: true, ViaSession: true}, "alice@example.net"},
		{"unverified session email", &Principal{UserID: "u2", Username: "mallory", Email: "owner@example.net", ViaSession: true}, "mallory"},
		{"access jwt email", &Principal{UserID: "sub", Email: "ops@example.net"}, "ops@example.net"},
		{"no email", &Principal{UserID: "u3", Username: "bob", ViaSession: true}, "bob"},
		{"id only", &Principal{UserID: "u4", ViaSession: true}, "u4"},
		{"nobody", nil, anonymousActor},
	}
	for _, c := range cases {
		if got := auditActor(c.p); got != c.want {
			t.Errorf("%s: auditActor = %q, want %q", c.name, got, c.want)
		}
	}
}

// A player who sets their address to the owner's still signs every row as
// themselves, by username and by id.
func TestAuditCannotBeSignedWithAnotherPersonsEmail(t *testing.T) {
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.staff["owner"] = &StaffUser{ID: "u1", Username: "owner", Email: "owner@example.net", Role: "owner", EmailVerified: true}
	repo.staff["mallory"] = &StaffUser{ID: "u2", Username: "mallory", Email: "mallory@example.net", Role: "user", EmailVerified: true}
	repo.sessions[hashCookie("tok")] = &fakeSession{userID: "u2", expiresAt: time.Unix(1_700_000_000, 0).Add(time.Hour)}
	api := newTestAPI(repo, newFakeCluster())
	api.External = SessionAuth{Repo: repo, RootDomain: testRoot, Now: api.now}
	api.ClientIPHeader = "CF-Connecting-IP"
	eh := api.ExternalHandler()
	hdr := map[string]string{
		"Content-Type": "application/json", "Cookie": sessionCookieName + "=tok",
		"CF-Connecting-IP": "203.0.113.5", "User-Agent": "probe/1.0",
	}
	for _, email := range []string{"owner@example.net", "owner@example.net"} {
		if w := do(eh, "POST", "/api/v1/account/email", `{"email":"`+email+`"}`, hdr); w.Code != http.StatusOK {
			t.Fatalf("set email = %d (%s)", w.Code, w.Body.String())
		}
	}
	// The first write ran while the address was still verified; the second
	// ran with the owner's address set and unverified.
	last := repo.audits[len(repo.audits)-1]
	if last.Actor != "mallory" || last.ActorUserID != "u2" {
		t.Fatalf("audit after spoofing = actor %q user %q, want mallory/u2", last.Actor, last.ActorUserID)
	}
	if last.ClientIP != "203.0.113.5" || last.UserAgent != "probe/1.0" || last.RequestID == "" {
		t.Fatalf("audit request detail = %+v", last)
	}
}

func TestSignInFailuresAreAuditedAndCounted(t *testing.T) {
	api, repo, mailer := seedLoginEmailAPI(t)
	eh := api.ExternalHandler()
	noAccount := metrics.AuthFailuresTotal.WithLabelValues("login_email", "no_account")
	badCode := metrics.AuthFailuresTotal.WithLabelValues("login_email", "bad_code")
	n0, b0 := testutil.ToFloat64(noAccount), testutil.ToFloat64(badCode)

	do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"ghost@example.net","code":"123456"}`, jsonHeader)
	if w := do(eh, "POST", "/api/v1/auth/email/start", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("start = %d", w.Code)
	}
	wrong := "000000"
	if mailer.code == wrong {
		wrong = "111111"
	}
	do(eh, "POST", "/api/v1/auth/email/verify", `{"email":"player@example.net","code":"`+wrong+`"}`, jsonHeader)

	if got := testutil.ToFloat64(noAccount) - n0; got != 1 {
		t.Errorf("no_account failures counted %v, want 1", got)
	}
	if got := testutil.ToFloat64(badCode) - b0; got != 1 {
		t.Errorf("bad_code failures counted %v, want 1", got)
	}
	var failed []AuditEntry
	for _, e := range repo.audits {
		if e.Action == "auth.login_email.failed" {
			failed = append(failed, e)
		}
	}
	if len(failed) != 2 {
		t.Fatalf("failure audits = %+v, want 2", failed)
	}
	if failed[0].Actor != anonymousActor || failed[0].ActorUserID != "" || !strings.Contains(string(failed[0].Payload), "no_account") {
		t.Errorf("unknown-address failure = %+v", failed[0])
	}
	if failed[1].Actor != "player" || failed[1].ActorUserID != "u1" || !strings.Contains(string(failed[1].Payload), "bad_code") {
		t.Errorf("wrong-code failure = %+v", failed[1])
	}
}

func TestThrottleAuditsOncePerEpisode(t *testing.T) {
	api, repo, _ := seedLoginEmailAPI(t)
	api.AuthDoorLimit = RateLimit{Burst: 1, PerMinute: 1}
	clock := time.Unix(1_700_000_000, 0)
	api.Now = func() time.Time { return clock }
	eh := api.ExternalHandler()
	options := func() int {
		return do(eh, "POST", "/api/v1/auth/options", `{"email":"x@example.net"}`, jsonHeader).Code
	}
	throttled := func() int {
		n := 0
		for _, e := range repo.audits {
			if e.Action == "auth.rate_limited" {
				n++
			}
		}
		return n
	}
	options()
	for i := 0; i < 5; i++ {
		if c := options(); c != http.StatusTooManyRequests {
			t.Fatalf("call %d = %d, want 429", i+2, c)
		}
	}
	if n := throttled(); n != 1 {
		t.Fatalf("5 refusals left %d audit rows, want 1", n)
	}
	clock = clock.Add(time.Minute)
	options()
	options()
	if n := throttled(); n != 2 {
		t.Fatalf("a second episode left %d rows in total, want 2", n)
	}
}

func TestAuditWriteFailureIsCountedNotFatal(t *testing.T) {
	api, repo, _ := seedLoginEmailAPI(t)
	repo.failAudit = errors.New("db down")
	before := testutil.ToFloat64(metrics.AuditWriteFailuresTotal)
	if w := do(api.ExternalHandler(), "POST", "/api/v1/auth/email/start", `{"email":"player@example.net"}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("start with the audit store down = %d, want 202", w.Code)
	}
	if got := testutil.ToFloat64(metrics.AuditWriteFailuresTotal) - before; got != 1 {
		t.Fatalf("audit write failures counted %v, want 1", got)
	}
}

func TestLogoutAuditsTheLiveSession(t *testing.T) {
	api, repo, _ := seedLoginEmailAPI(t)
	repo.sessions[hashCookie("tok")] = &fakeSession{userID: "u1", expiresAt: time.Unix(1_700_000_000, 0).Add(time.Hour)}
	eh := api.ExternalHandler()
	cookie := map[string]string{"Content-Type": "application/json", "Cookie": sessionCookieName + "=tok"}
	do(eh, "POST", "/api/v1/auth/logout", "", cookie)
	do(eh, "POST", "/api/v1/auth/logout", "", cookie) // already revoked: no second row
	if len(repo.audits) != 1 || repo.audits[0].Action != "auth.logout" || repo.audits[0].ActorUserID != "u1" || repo.audits[0].Actor != "player" {
		t.Fatalf("logout audits = %+v, want one auth.logout by player/u1", repo.audits)
	}
}

func TestTruncateUTF8KeepsRunesWhole(t *testing.T) {
	if got := truncateUTF8("ab\xffc", 10); got != "ab�c" {
		t.Errorf("invalid byte = %q", got)
	}
	if got := truncateUTF8("猫猫", 4); got != "猫" {
		t.Errorf("cut mid-rune = %q, want 猫", got)
	}
}
