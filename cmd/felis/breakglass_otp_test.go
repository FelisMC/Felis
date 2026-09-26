package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/mail"
	"felis.lolicon.best/internal/platform"

	tea "github.com/charmbracelet/bubbletea"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// These tests cover the recovery proof (#13): the mailed code's rules, where
// naming an admin leads, what the mail says, how the console walks from a name to
// a proven (or overridden) run, and what the audit row then records. No mail
// leaves the process: the relay is a fake that keeps what it was handed.

type sentMail struct{ to, subject, body string }

type fakeRelay struct {
	sent []sentMail
	err  error
}

func (r *fakeRelay) SendNotice(_ context.Context, to, subject, body string) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, sentMail{to, subject, body})
	return nil
}

// sentCode pulls the code out of the one mail the relay carried.
func (r *fakeRelay) sentCode(t *testing.T) string {
	t.Helper()
	if len(r.sent) != 1 {
		t.Fatalf("relay carried %d mails, want 1", len(r.sent))
	}
	_, after, ok := strings.Cut(r.sent[0].body, "Recovery code: ")
	if !ok || len(after) < 6 {
		t.Fatalf("mail carries no recovery code:\n%s", r.sent[0].body)
	}
	return after[:6]
}

func relayConfig(r *fakeRelay, now func() time.Time) recoveryConfig {
	return recoveryConfig{
		open: func(context.Context) (recoveryMailer, error) { return r, nil },
		host: "felis-host-1",
		now:  now,
	}
}

func verifiedAdmin(username, email string) *api.StaffUser {
	return &api.StaffUser{ID: "usr-" + username, Username: username, Role: "owner", Email: email, EmailVerified: true}
}

func TestRecoveryCodeRules(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)

	t.Run("a fresh code is six digits and lives for the TTL", func(t *testing.T) {
		seen := map[string]bool{}
		for i := 0; i < 20; i++ {
			c, err := newRecoveryCode(t0)
			if err != nil {
				t.Fatal(err)
			}
			if !isRecoveryCodeShape(c.value) {
				t.Fatalf("code %q is not six digits", c.value)
			}
			if !c.expires.Equal(t0.Add(recoveryCodeTTL)) {
				t.Fatalf("expires = %v, want %v", c.expires, t0.Add(recoveryCodeTTL))
			}
			seen[c.value] = true
		}
		if len(seen) < 2 {
			t.Error("twenty codes were all the same")
		}
	})

	t.Run("the right code is accepted, surrounding space ignored", func(t *testing.T) {
		c := &recoveryCode{value: "042917", expires: t0.Add(recoveryCodeTTL)}
		if v := c.check(" 042917 ", t0); v != codeAccepted {
			t.Errorf("check = %v, want accepted", v)
		}
	})

	t.Run("wrong codes count down and the last one exhausts it", func(t *testing.T) {
		c := &recoveryCode{value: "042917", expires: t0.Add(recoveryCodeTTL)}
		for i := 1; i < recoveryCodeAttempts; i++ {
			if v := c.check("000000", t0); v != codeWrong {
				t.Fatalf("wrong code %d: check = %v, want wrong", i, v)
			}
			if left := c.attemptsLeft(); left != recoveryCodeAttempts-i {
				t.Fatalf("after %d wrong codes attemptsLeft = %d, want %d", i, left, recoveryCodeAttempts-i)
			}
		}
		if v := c.check("000000", t0); v != codeExhausted {
			t.Fatalf("wrong code %d: check = %v, want exhausted", recoveryCodeAttempts, v)
		}
		if v := c.check("042917", t0); v != codeExhausted {
			t.Errorf("the right code after exhaustion: check = %v, want exhausted", v)
		}
	})

	t.Run("an expired code accepts nothing", func(t *testing.T) {
		c := &recoveryCode{value: "042917", expires: t0.Add(recoveryCodeTTL)}
		if v := c.check("042917", t0.Add(recoveryCodeTTL-time.Second)); v != codeAccepted {
			t.Fatalf("a second before expiry: check = %v, want accepted", v)
		}
		c = &recoveryCode{value: "042917", expires: t0.Add(recoveryCodeTTL)}
		if v := c.check("042917", t0.Add(recoveryCodeTTL)); v != codeExpired {
			t.Errorf("at expiry: check = %v, want expired", v)
		}
	})
}

func TestBeginRecovery(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { return t0 }

	t.Run("mails a code to the named admin's verified address", func(t *testing.T) {
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{"root": verifiedAdmin("root", "root@example.com")}}
		r := &fakeRelay{}
		st, err := beginRecovery(ctx, f, relayConfig(r, clock), "root", "alice", bgAddOperator)
		if err != nil {
			t.Fatal(err)
		}
		if st.code == nil || st.skip != "" {
			t.Fatalf("start = %+v, want a code and no skip", st)
		}
		if st.admin == nil || st.admin.Username != "root" {
			t.Fatalf("start.admin = %+v, want root", st.admin)
		}
		if got := r.sentCode(t); got != st.code.value {
			t.Errorf("mailed code %q, want the code the console checks (%q)", got, st.code.value)
		}
		m := r.sent[0]
		if m.to != "root@example.com" {
			t.Errorf("mail went to %q, want root@example.com", m.to)
		}
		for _, want := range []string{"felis-host-1", "OS user alice", `"root"`, "add an Operator account", "添加 Operator 账号", "10 minutes"} {
			if !strings.Contains(m.body, want) {
				t.Errorf("mail body lacks %q:\n%s", want, m.body)
			}
		}
		if !st.code.expires.Equal(t0.Add(recoveryCodeTTL)) {
			t.Errorf("code expires %v, want %v", st.code.expires, t0.Add(recoveryCodeTTL))
		}
	})

	t.Run("the Owner reset is named as such", func(t *testing.T) {
		f := &fakeOwnerStore{users: map[string]*api.StaffUser{"root": verifiedAdmin("root", "root@example.com")}}
		r := &fakeRelay{}
		if _, err := beginRecovery(ctx, f, relayConfig(r, clock), "root", "alice", bgProvisionOwner); err != nil {
			t.Fatal(err)
		}
		if body := r.sent[0].body; !strings.Contains(body, "reset the Owner account") || !strings.Contains(body, "重置 Owner 账号") {
			t.Errorf("mail body does not name the Owner reset:\n%s", body)
		}
	})

	// Each way the code cannot go out ends in a skip reason and no mail.
	unverified := verifiedAdmin("root", "root@example.com")
	unverified.EmailVerified = false
	noEmail := verifiedAdmin("root", "")
	cases := []struct {
		name       string
		user       *api.StaffUser
		rc         func(r *fakeRelay) recoveryConfig
		skip       string
		detail     string
		wantAdmin  bool
		relayError error
	}{
		{name: "unknown admin", user: nil, rc: func(r *fakeRelay) recoveryConfig { return relayConfig(r, clock) }, skip: otpSkipUnknownAdmin},
		{name: "unverified address", user: unverified, rc: func(r *fakeRelay) recoveryConfig { return relayConfig(r, clock) }, skip: otpSkipNoVerifiedEmail, wantAdmin: true},
		{name: "no address", user: noEmail, rc: func(r *fakeRelay) recoveryConfig { return relayConfig(r, clock) }, skip: otpSkipNoVerifiedEmail, wantAdmin: true},
		{name: "no relay wired", user: verifiedAdmin("root", "root@example.com"), rc: func(*fakeRelay) recoveryConfig { return recoveryConfig{} }, skip: otpSkipNoRelay, detail: "this console has no mail relay", wantAdmin: true},
		{name: "relay cannot open", user: verifiedAdmin("root", "root@example.com"), rc: func(*fakeRelay) recoveryConfig {
			return recoveryConfig{open: func(context.Context) (recoveryMailer, error) {
				return nil, errors.New("[smtp] is not configured in felis.toml")
			}}
		}, skip: otpSkipNoRelay, detail: "[smtp] is not configured in felis.toml", wantAdmin: true},
		{name: "send fails", user: verifiedAdmin("root", "root@example.com"), rc: func(r *fakeRelay) recoveryConfig { return relayConfig(r, clock) },
			skip: otpSkipSendFailed, detail: "554 relay refused", wantAdmin: true, relayError: errors.New("554 relay refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeOwnerStore{users: map[string]*api.StaffUser{}}
			if tc.user != nil {
				f.users["root"] = tc.user
			}
			r := &fakeRelay{err: tc.relayError}
			st, err := beginRecovery(ctx, f, tc.rc(r), "root", "alice", bgProvisionOwner)
			if err != nil {
				t.Fatal(err)
			}
			if st.code != nil || st.skip != tc.skip || st.detail != tc.detail {
				t.Errorf("start = {code:%v skip:%q detail:%q}, want no code, skip %q, detail %q", st.code, st.skip, st.detail, tc.skip, tc.detail)
			}
			if (st.admin != nil) != tc.wantAdmin {
				t.Errorf("start.admin = %+v, want present=%v", st.admin, tc.wantAdmin)
			}
			if len(r.sent) != 0 {
				t.Errorf("relay carried %d mails, want none", len(r.sent))
			}
		})
	}

	t.Run("a datastore fault is an error", func(t *testing.T) {
		f := &fakeOwnerStore{userErr: errors.New("db down")}
		if _, err := beginRecovery(ctx, f, relayConfig(&fakeRelay{}, clock), "root", "alice", bgProvisionOwner); err == nil {
			t.Fatal("want the store fault")
		}
	})
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"alice@example.com": "a****@example.com",
		"al@example.com":    "a***@example.com",
		"@example.com":      "***",
		"nonsense":          "***",
	} {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostRecoveryMailer(t *testing.T) {
	ctx := context.Background()

	t.Run("no [smtp] host is no relay", func(t *testing.T) {
		_, err := hostRecoveryMailer(config.SMTPConfig{}, "felis")(ctx)
		if err == nil || !strings.Contains(err.Error(), "[smtp]") {
			t.Fatalf("err = %v, want it to name [smtp]", err)
		}
	})

	t.Run("the password_ref env var supplies the password", func(t *testing.T) {
		t.Setenv("FELIS_TEST_RELAY_PW", "from-env")
		off := false
		c := config.SMTPConfig{Host: "mail.example.com", Port: 2525, From: "felis@example.com", Username: "felis", PasswordRef: "FELIS_TEST_RELAY_PW", RequireTLS: &off}
		got, err := hostRecoveryMailer(c, "felis")(ctx)
		if err != nil {
			t.Fatal(err)
		}
		relay, ok := got.(*mail.SMTP)
		if !ok {
			t.Fatalf("relay is %T, want *mail.SMTP", got)
		}
		if relay.Password != "from-env" || relay.Host != "mail.example.com" || relay.Port != 2525 || relay.From != "felis@example.com" || relay.Username != "felis" || relay.RequireTLS {
			t.Errorf("relay = %+v, want the [smtp] fields with the env password and TLS as configured", *relay)
		}
	})
}

func TestSMTPSecretPassword(t *testing.T) {
	ctx := context.Background()
	scheme := haltScheme(t)

	t.Run("reads the password key", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "felis", Name: platform.SMTPSecretName},
			Data:       map[string][]byte{platform.SMTPSecretPasswordKey: []byte("s3cret")},
		}).Build()
		if pw, err := smtpSecretPassword(ctx, cl, "felis"); err != nil || pw != "s3cret" {
			t.Errorf("smtpSecretPassword = (%q, %v), want (s3cret, nil)", pw, err)
		}
	})

	t.Run("a missing Secret is a relay without AUTH", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		if pw, err := smtpSecretPassword(ctx, cl, "felis"); err != nil || pw != "" {
			t.Errorf("smtpSecretPassword = (%q, %v), want (\"\", nil)", pw, err)
		}
	})

	t.Run("any other read failure is an error", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, platform.SMTPSecretName, errors.New("rbac"))
			},
		}).Build()
		if _, err := smtpSecretPassword(ctx, cl, "felis"); err == nil {
			t.Fatal("want the read failure")
		}
	})
}

// recoveryModel is an Owner-reset console for admin "root" (verified address
// root@example.com), run by OS user alice, with the fake relay behind it.
func recoveryModel(t *testing.T, f *fakeOwnerStore, r *fakeRelay, now func() time.Time) *ownerModel {
	t.Helper()
	if f.users == nil {
		f.users = map[string]*api.StaffUser{"root": verifiedAdmin("root", "root@example.com")}
	}
	return newOwnerModel(context.Background(), f, "alice", true).withRecovery(relayConfig(r, now))
}

// nameAdmin submits the admin-name form and feeds the result of the send back in.
func nameAdmin(t *testing.T, m *ownerModel, name string) *ownerModel {
	t.Helper()
	m.authUser = name
	_, cmd := m.onFormComplete()
	if m.step != owWorking {
		t.Fatalf("after naming the admin step = %v, want owWorking", m.step)
	}
	msg := findMsg[owAuthMsg](t, cmd)
	next, _ := m.Update(msg)
	return next.(*ownerModel)
}

// findMsg runs a (possibly batched) command and returns the first T it produces.
func findMsg[T any](t *testing.T, cmd tea.Cmd) T {
	t.Helper()
	var zero T
	if cmd == nil {
		t.Fatalf("no command, want one producing %T", zero)
	}
	switch msg := cmd().(type) {
	case T:
		return msg
	case tea.BatchMsg:
		for _, c := range msg {
			if c == nil {
				continue
			}
			if got, ok := c().(T); ok {
				return got
			}
		}
	}
	t.Fatalf("command produced no %T", zero)
	return zero
}

// typeCode submits the code form with typed.
func typeCode(t *testing.T, m *ownerModel, typed string) {
	t.Helper()
	if m.step != owCode {
		t.Fatalf("step = %v, want owCode", m.step)
	}
	m.codeInput = typed
	m.onFormComplete()
}

// provisionAudit finishes the run as an Owner reset and returns its audit payload.
func provisionAudit(t *testing.T, m *ownerModel, f *fakeOwnerStore) (api.AuditEntry, map[string]any) {
	t.Helper()
	if m.step != owProvision {
		t.Fatalf("step = %v, want owProvision", m.step)
	}
	m.username = "owner"
	msg := m.provisionCmd()().(owProvisionMsg)
	if msg.err != nil {
		t.Fatalf("provision: %v", msg.err)
	}
	return auditOf(t, f)
}

func TestRecoveryConsoleFlow(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	fixed := func() time.Time { return t0 }

	t.Run("the mailed code proves the admin and the audit says so", func(t *testing.T) {
		f, r := &fakeOwnerStore{}, &fakeRelay{}
		m := nameAdmin(t, recoveryModel(t, f, r, fixed), "root")
		typeCode(t, m, r.sentCode(t))
		if m.mode != "recovery" || m.accountable != "root" {
			t.Fatalf("mode=%q accountable=%q, want recovery attributed to root", m.mode, m.accountable)
		}
		e, payload := provisionAudit(t, m, f)
		if e.Actor != "root" || e.Action != "break_glass.recovery" {
			t.Errorf("audit = %+v, want actor=root action=break_glass.recovery", e)
		}
		if payload["verified"] != true || payload["verified_by"] != verifiedByEmailOTP || payload["code_sent_to"] != "root@example.com" || payload["os_user"] != "alice" {
			t.Errorf("payload = %v, want verified by email_otp to root@example.com, os_user alice", payload)
		}
	})

	t.Run("a wrong code asks again, and the last wrong one leads to the override", func(t *testing.T) {
		f, r := &fakeOwnerStore{}, &fakeRelay{}
		m := nameAdmin(t, recoveryModel(t, f, r, fixed), "root")
		wrong := "000000"
		if r.sentCode(t) == wrong {
			wrong = "111111"
		}
		for i := 1; i < recoveryCodeAttempts; i++ {
			typeCode(t, m, wrong)
			if m.step != owCode || !strings.Contains(m.codeNote, "wrong") {
				t.Fatalf("wrong code %d: step=%v note=%q, want the code form again with a note", i, m.step, m.codeNote)
			}
		}
		typeCode(t, m, wrong)
		if m.step != owOverride || m.skip != otpSkipCodeRejected {
			t.Fatalf("after %d wrong codes step=%v skip=%q, want the override for code_rejected", recoveryCodeAttempts, m.step, m.skip)
		}
		m.onFormComplete() // OVERRIDE typed
		e, payload := provisionAudit(t, m, f)
		if e.Actor != "alice" || e.Action != "break_glass.root_override" {
			t.Errorf("audit = %+v, want actor=alice action=break_glass.root_override", e)
		}
		if payload["verified"] != false || payload["otp_skipped"] != otpSkipCodeRejected || payload["admin_account"] != "root" {
			t.Errorf("payload = %v, want unverified, otp_skipped=code_rejected, admin_account=root", payload)
		}
	})

	t.Run("a late code leads to the override", func(t *testing.T) {
		now := t0
		f, r := &fakeOwnerStore{}, &fakeRelay{}
		m := nameAdmin(t, recoveryModel(t, f, r, func() time.Time { return now }), "root")
		now = t0.Add(recoveryCodeTTL)
		typeCode(t, m, r.sentCode(t))
		if m.step != owOverride || m.skip != otpSkipCodeExpired {
			t.Fatalf("step=%v skip=%q, want the override for code_expired", m.step, m.skip)
		}
	})

	t.Run("OVERRIDE at the code prompt goes on unverified, saying the operator skipped", func(t *testing.T) {
		f, r := &fakeOwnerStore{}, &fakeRelay{}
		m := nameAdmin(t, recoveryModel(t, f, r, fixed), "root")
		typeCode(t, m, breakGlassOverrideToken)
		if m.mode != "root_override" || m.accountable != "alice" {
			t.Fatalf("mode=%q accountable=%q, want root_override as alice", m.mode, m.accountable)
		}
		_, payload := provisionAudit(t, m, f)
		if payload["verified"] != false || payload["otp_skipped"] != otpSkipByOperator {
			t.Errorf("payload = %v, want unverified, otp_skipped=operator_skipped", payload)
		}
	})

	t.Run("a relay failure leads to the override naming it", func(t *testing.T) {
		f, r := &fakeOwnerStore{}, &fakeRelay{err: errors.New("dial tcp 10.0.0.9:587: connect: connection refused")}
		m := nameAdmin(t, recoveryModel(t, f, r, fixed), "root")
		if m.step != owOverride || m.skip != otpSkipSendFailed {
			t.Fatalf("step=%v skip=%q, want the override for send_failed", m.step, m.skip)
		}
		if reason := m.overrideReason(); !strings.Contains(reason, "connection refused") {
			t.Errorf("override reason %q does not name the failure", reason)
		}
		m.onFormComplete()
		_, payload := provisionAudit(t, m, f)
		if payload["otp_skipped"] != otpSkipSendFailed || !strings.Contains(payload["otp_skip_detail"].(string), "connection refused") {
			t.Errorf("payload = %v, want otp_skipped=send_failed with the relay's error", payload)
		}
	})

	t.Run("esc at the code prompt starts over and forgets the code", func(t *testing.T) {
		f, r := &fakeOwnerStore{}, &fakeRelay{}
		m := nameAdmin(t, recoveryModel(t, f, r, fixed), "root")
		code := r.sentCode(t)
		next, _ := m.Update(key(tea.KeyEsc))
		m = next.(*ownerModel)
		if m.step != owAuth || m.code != nil || m.admin != nil {
			t.Fatalf("after esc step=%v code=%v admin=%v, want owAuth with the attempt forgotten", m.step, m.code, m.admin)
		}
		// The old code cannot be replayed: the next name mails a new one.
		m = nameAdmin(t, m, "root")
		if len(r.sent) != 2 {
			t.Fatalf("relay carried %d mails, want a second one for the new attempt", len(r.sent))
		}
		_, fresh, _ := strings.Cut(r.sent[1].body, "Recovery code: ")
		if m.code.value != fresh[:6] {
			t.Errorf("the console checks %q, want the newly mailed %q (old one was %q)", m.code.value, fresh[:6], code)
		}
	})
}

func TestRootHandsRecoveryToAccountOperations(t *testing.T) {
	for _, op := range []bgOperation{bgProvisionOwner, bgAddOperator} {
		m := newTestRoot(true, consoleModeBreakGlass, "")
		m.recovery = recoveryConfig{host: "felis-host-1"}
		m = drive(t, m, menuChoiceMsg{op: op})
		om, ok := m.screen.(*ownerModel)
		if !ok {
			t.Fatalf("op %v: screen = %T, want *ownerModel", op, m.screen)
		}
		if om.recovery.host != "felis-host-1" {
			t.Errorf("op %v: the account screen has no relay config; its codes could never go out", op)
		}
	}
}
