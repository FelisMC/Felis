package updates

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSource returns a canned latest per component name, or an error for names in
// failFor, so a discovery failure can be exercised.
type fakeSource struct {
	latest  map[string]Version
	failFor map[string]bool
}

func (f fakeSource) Latest(_ context.Context, c Component) (Version, error) {
	if f.failFor[c.Name] {
		return Version{}, errors.New("boom")
	}
	v, ok := f.latest[c.Name]
	if !ok {
		return Version{}, errors.New("not found")
	}
	return v, nil
}

// recordingNotifier / recordingApplier capture what the orchestrator drove.
type recordingNotifier struct {
	got  []Action
	fail bool
}

func (r *recordingNotifier) Notify(_ context.Context, pending []Action) error {
	if r.fail {
		return errors.New("smtp down")
	}
	r.got = append(r.got, pending...)
	return nil
}

type recordingApplier struct {
	got     []string
	failFor map[string]bool
}

func (r *recordingApplier) Apply(_ context.Context, a Action) error {
	if r.failFor[a.Component] {
		return errors.New("rollout failed")
	}
	r.got = append(r.got, a.Component)
	return nil
}

func TestRunDiscoversPlansNotifiesApplies(t *testing.T) {
	now := time.Date(2026, 7, 1, 3, 30, 0, 0, time.UTC)
	win := Window{
		Start: time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC),
	}
	comps := []Component{
		{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: win}, // apply
		{Name: "k3s", Current: mustV(t, "v1.30.2+k3s1"), Policy: PolicyNotify, Manageable: true},                // notify
		{Name: "velocity", Current: mustV(t, "3.3.0"), Policy: PolicyScheduled, Manageable: false},              // notify (off-cluster)
		{Name: "mc-survival", Current: mustV(t, "1.20.1"), Policy: PolicyPinned},                                // pinned, never queried
	}
	source := fakeSource{latest: map[string]Version{
		"felis-api": mustV(t, "1.5.0"),
		"k3s":       mustV(t, "v1.30.3+k3s1"),
		"velocity":  mustV(t, "3.4.0"),
		// mc-survival intentionally absent: a pinned component must not be queried.
	}}
	notifier := &recordingNotifier{}
	applier := &recordingApplier{}

	res, err := Run(context.Background(), source, notifier, applier, comps, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Exactly felis-api was applied; k3s and velocity were notify-only; pinned untouched.
	if len(res.Applied) != 1 || res.Applied[0].Component != "felis-api" {
		t.Errorf("Applied = %+v, want just felis-api", res.Applied)
	}
	if len(applier.got) != 1 || applier.got[0] != "felis-api" {
		t.Errorf("applier ran for %v, want just [felis-api]", applier.got)
	}
	// Notifier saw all three pending (felis-api apply + k3s notify + velocity notify).
	if len(notifier.got) != 3 {
		t.Errorf("notifier saw %d pending, want 3: %+v", len(notifier.got), notifier.got)
	}
	// A pinned component is never queried upstream.
	if _, queried := res.SourceErrors["mc-survival"]; queried {
		t.Error("pinned mc-survival must not be queried upstream")
	}
	if len(res.ApplyErrors) != 0 {
		t.Errorf("unexpected apply errors: %v", res.ApplyErrors)
	}
}

// TestRunToleratesSourceFailure proves one component's discovery failure neither
// sinks the cycle nor blocks the others.
func TestRunToleratesSourceFailure(t *testing.T) {
	now := time.Date(2026, 7, 1, 3, 30, 0, 0, time.UTC)
	comps := []Component{
		{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyNotify, Manageable: true},
		{Name: "cloudflared", Current: mustV(t, "2024.2.1"), Policy: PolicyNotify, Manageable: true},
	}
	source := fakeSource{
		latest:  map[string]Version{"cloudflared": mustV(t, "2024.3.0")},
		failFor: map[string]bool{"felis-api": true},
	}
	notifier := &recordingNotifier{}

	res, err := Run(context.Background(), source, notifier, nil, comps, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SourceErrors["felis-api"] == nil {
		t.Error("felis-api source failure should be recorded")
	}
	// felis-api plans to None (latest unknown); cloudflared notifies.
	if len(notifier.got) != 1 || notifier.got[0].Component != "cloudflared" {
		t.Errorf("notifier saw %+v, want just cloudflared", notifier.got)
	}
}

// TestRunRecordsMissingApplier proves an apply with no applier wired is a recorded
// error, not a silent success — the plan wanted to apply but nothing could.
func TestRunRecordsMissingApplier(t *testing.T) {
	now := time.Date(2026, 7, 1, 3, 30, 0, 0, time.UTC)
	win := Window{
		Start: time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC),
	}
	comps := []Component{
		{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: win},
	}
	source := fakeSource{latest: map[string]Version{"felis-api": mustV(t, "1.5.0")}}

	res, err := Run(context.Background(), source, &recordingNotifier{}, nil, comps, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !errors.Is(res.ApplyErrors["felis-api"], errNoApplier) {
		t.Errorf("ApplyErrors[felis-api] = %v, want errNoApplier", res.ApplyErrors["felis-api"])
	}
	if len(res.Applied) != 0 {
		t.Errorf("nothing should be marked Applied without an applier: %+v", res.Applied)
	}
}

// TestRunNotifyFailureDoesNotBlockApply proves a dead mailer is surfaced but the
// scheduled apply still runs (the SysAdmin can still see state in the Panel).
func TestRunNotifyFailureDoesNotBlockApply(t *testing.T) {
	now := time.Date(2026, 7, 1, 3, 30, 0, 0, time.UTC)
	win := Window{
		Start: time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC),
	}
	comps := []Component{
		{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: win},
	}
	source := fakeSource{latest: map[string]Version{"felis-api": mustV(t, "1.5.0")}}
	applier := &recordingApplier{}

	res, err := Run(context.Background(), source, &recordingNotifier{fail: true}, applier, comps, now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.NotifyErr == nil {
		t.Error("notify failure should be surfaced")
	}
	if len(res.Applied) != 1 {
		t.Errorf("apply should still run despite notify failure: %+v", res.Applied)
	}
}

func TestReport(t *testing.T) {
	now := time.Date(2026, 7, 1, 3, 30, 0, 0, time.UTC)
	win := Window{
		Start: time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC),
	}
	comps := []Component{
		{Name: "felis-api", Current: mustV(t, "1.4.0"), Policy: PolicyScheduled, Manageable: true, Window: win},
		{Name: "k3s", Current: mustV(t, "v1.30.2+k3s1"), Policy: PolicyNotify, Manageable: true},
		{Name: "cloudflared", Current: mustV(t, "2024.3.0"), Policy: PolicyNotify, Manageable: true},
		{Name: "mc-survival", Current: mustV(t, "1.20.1"), Policy: PolicyPinned},
	}
	latest := map[string]Version{
		"felis-api":   mustV(t, "1.5.0"),
		"k3s":         mustV(t, "v1.30.3+k3s1"),
		"cloudflared": mustV(t, "2024.3.0"), // same ⇒ up to date
	}
	out := Report(PlanUpdates(comps, latest, now))

	for _, want := range []string{
		"felis-api",
		"1.4.0",
		"1.5.0",
		"apply (scheduled window)",
		"k3s",
		"v1.30.3+k3s1",
		"update available (notify)",
		"cloudflared",
		"up to date",
		"mc-survival",
		"pinned",
	} {
		if !contains(out, want) {
			t.Errorf("Report missing %q; got:\n%s", want, out)
		}
	}
	// An empty plan is stated, not blank.
	if Report(nil) == "" {
		t.Error("Report(nil) should not be empty")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
