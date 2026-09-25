package build

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fakeOutcomes struct {
	out   Outcome
	err   error
	calls []string
}

func (f *fakeOutcomes) Outcome(_ context.Context, id string) (Outcome, error) {
	f.calls = append(f.calls, id)
	return f.out, f.err
}

// runningBuild wires a Builder whose bld-1 is building under a Job in phase.
func runningBuild(t *testing.T, phase JobPhase, out *fakeOutcomes) (*Builder, *fakeStore) {
	t.Helper()
	b, st, jb := newBuilder()
	if out != nil {
		b.Outcomes = out
	}
	st.builds["bld-1"] = &Build{ID: "bld-1", ImageRef: "registry.felis.svc:5000/mc/pack:1", Status: StatusBuilding,
		JobName: "build-bld-1", RequestedBy: "admin@example.net", CreatedAt: testNow}
	jb.phase = phase
	return b, st
}

func blockedEnvelope(t *testing.T) *ScanEnvelope {
	t.Helper()
	s, err := Summarize([]byte(trivyFixture), ScanPolicy{FailOn: []string{"CRITICAL", "HIGH"}})
	if err != nil {
		t.Fatal(err)
	}
	return &ScanEnvelope{Summary: s, Report: json.RawMessage(trivyFixture), SBOM: json.RawMessage(`{"bomFormat":"CycloneDX"}`)}
}

func gunzipString(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A build the scan blocked records the blocking findings as its error and keeps
// the report and SBOM.
func TestSyncRecordsTheScanThatBlocked(t *testing.T) {
	out := &fakeOutcomes{out: Outcome{FailedStep: ContainerScanGate, ExitCode: 1, Message: "the scan blocked…", Scan: blockedEnvelope(t)}}
	b, st := runningBuild(t, JobFailed, out)
	bld, err := b.Sync(context.Background(), "bld-1")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if bld.Status != StatusFailed || bld.Error != "the scan blocked the image: 2 CRITICAL, 1 HIGH (CVE-2024-0001, aws-access-key-id, CVE-2024-0004)" {
		t.Errorf("build = %s %q", bld.Status, bld.Error)
	}
	if st.builds["bld-1"].Error != bld.Error {
		t.Errorf("stored error = %q", st.builds["bld-1"].Error)
	}
	sc, ok := st.scans["bld-1"]
	if !ok {
		t.Fatal("no scan stored")
	}
	if !sc.Summary.Blocked || !sc.ScannedAt.Equal(testNow) || gunzipString(t, sc.SBOMGz) != `{"bomFormat":"CycloneDX"}` ||
		!strings.Contains(gunzipString(t, sc.ReportGz), `"CVE-2024-0001"`) {
		t.Errorf("scan = blocked %t at %s, sbom %q", sc.Summary.Blocked, sc.ScannedAt, gunzipString(t, sc.SBOMGz))
	}
	if len(st.admitted) != 0 {
		t.Errorf("a blocked image was admitted: %v", st.admitted)
	}
	if !reflect.DeepEqual(out.calls, []string{"bld-1"}) {
		t.Errorf("outcome reads = %v", out.calls)
	}
}

// An admitted image keeps its scan too.
func TestSyncKeepsTheScanOfAnAdmittedImage(t *testing.T) {
	env := blockedEnvelope(t)
	env.Summary.Blocked = false
	b, st := runningBuild(t, JobSucceeded, &fakeOutcomes{out: Outcome{Scan: env}})
	bld, err := b.Sync(context.Background(), "bld-1")
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if bld.Status != StatusSucceeded || len(st.admitted) != 1 {
		t.Fatalf("build %s, admitted %v", bld.Status, st.admitted)
	}
	if sc, ok := st.scans["bld-1"]; !ok || sc.Summary.Blocked || sc.Summary.Packages != 5 {
		t.Errorf("scan = %+v, %t", sc.Summary, ok)
	}
}

func TestSyncNamesWhatEndedAFailedBuild(t *testing.T) {
	long := strings.Repeat("x", 700)
	for _, tc := range []struct {
		name string
		out  Outcome
		want string
	}{
		{"kaniko", Outcome{FailedStep: ContainerKaniko, ExitCode: 1,
			Message: "INFO[0003] RUN ./setup.sh\nstep 1\n\nstep 2\r\nerror building image: error building stage: failed to execute command: exit status 2\n"},
			"the image build failed (exit 1): step 1 | step 2 | error building image: error building stage: failed to execute command: exit status 2"},
		{"trivy", Outcome{FailedStep: ContainerTrivy, ExitCode: 1, Message: "FATAL\tFatal error\tinit error: DB error: failed to download vulnerability DB"},
			"the vulnerability scan failed (exit 1): FATAL Fatal error init error: DB error: failed to download vulnerability DB"},
		{"push", Outcome{FailedStep: ContainerPush, ExitCode: 1}, "the registry push failed (exit 1)"},
		{"unknown step", Outcome{FailedStep: "sidecar", ExitCode: 137}, "the sidecar step failed (exit 137)"},
		{"unreadable scan", Outcome{FailedStep: ContainerScanGate, ExitCode: 2, Message: "the scan report is unreadable: EOF",
			ScanErr: errors.New("the scan-gate log holds no scan envelope")},
			"the scan gate failed (exit 2): the scan report is unreadable: EOF; the scan report could not be read back: the scan-gate log holds no scan envelope"},
		{"deadline", Outcome{DeadlineExceeded: true}, "the build ran past its 30m0s deadline"},
		{"nothing known", Outcome{}, "the build job failed"},
		{"long message", Outcome{FailedStep: ContainerKaniko, ExitCode: 1, Message: "start " + long},
			"the image build failed (exit 1): …" + strings.Repeat("x", 600)},
	} {
		b, _ := runningBuild(t, JobFailed, &fakeOutcomes{out: tc.out})
		bld, err := b.Sync(context.Background(), "bld-1")
		if err != nil {
			t.Fatalf("%s: Sync: %v", tc.name, err)
		}
		if bld.Error != tc.want {
			t.Errorf("%s: error = %q\nwant %q", tc.name, bld.Error, tc.want)
		}
	}
}

// A vanished Job has no pod to read; a failed read leaves the build to the next
// tick; without an outcome reader Sync records a plain failure.
func TestSyncOutcomeEdges(t *testing.T) {
	out := &fakeOutcomes{}
	b, _ := runningBuild(t, JobUnknown, out)
	bld, err := b.Sync(context.Background(), "bld-1")
	if err != nil || bld.Error != "the build job is gone: it was deleted before it finished" || len(out.calls) != 0 {
		t.Errorf("gone job: %q, %v, reads %v", bld.Error, err, out.calls)
	}

	b, st := runningBuild(t, JobFailed, &fakeOutcomes{err: errors.New("apiserver unavailable")})
	if _, err := b.Sync(context.Background(), "bld-1"); err == nil || err.Error() != "apiserver unavailable" {
		t.Errorf("read failure: err = %v", err)
	}
	if st.builds["bld-1"].Status != StatusBuilding {
		t.Errorf("a failed outcome read finished the build: %s", st.builds["bld-1"].Status)
	}

	b, _ = runningBuild(t, JobFailed, nil)
	if bld, err := b.Sync(context.Background(), "bld-1"); err != nil || bld.Error != "the build job failed" {
		t.Errorf("no reader: %q, %v", bld.Error, err)
	}

	out = &fakeOutcomes{}
	b, _ = runningBuild(t, JobRunning, out)
	if bld, err := b.Sync(context.Background(), "bld-1"); err != nil || bld.Status != StatusBuilding || len(out.calls) != 0 {
		t.Errorf("running: %s, %v, reads %v", bld.Status, err, out.calls)
	}
}

func TestSubmitCarriesTheScanPolicy(t *testing.T) {
	b, _, jb := newBuilder()
	if _, err := b.Submit(context.Background(), goodRequest()); err != nil {
		t.Fatal(err)
	}
	b.Config.ScanFailOn = []string{"CRITICAL", "HIGH"}
	b.Config.ScanFailUnfixed = true
	b.Config.ScanAccept = []string{"CVE-2021-35515"}
	req := goodRequest()
	req.ImageRef = "registry.felis.svc:5000/mc-paper:2.0"
	if _, err := b.Submit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(jb.created) != 2 {
		t.Fatalf("jobs = %d", len(jb.created))
	}
	if got := jb.created[0]; !reflect.DeepEqual(got.ScanFailOn, []string{"CRITICAL"}) || got.ScanFailUnfixed || got.ScanAccept != nil {
		t.Errorf("default policy = %v unfixed=%t accept=%v", got.ScanFailOn, got.ScanFailUnfixed, got.ScanAccept)
	}
	if got := jb.created[1]; !reflect.DeepEqual(got.ScanFailOn, []string{"CRITICAL", "HIGH"}) || !got.ScanFailUnfixed ||
		!reflect.DeepEqual(got.ScanAccept, []string{"CVE-2021-35515"}) {
		t.Errorf("configured policy = %v unfixed=%t accept=%v", got.ScanFailOn, got.ScanFailUnfixed, got.ScanAccept)
	}
}

func TestBuilderScanReadsTheStore(t *testing.T) {
	b, st, _ := newBuilder()
	if _, err := b.Scan(context.Background(), "bld-9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing scan: err = %v", err)
	}
	st.scans = map[string]Scan{"bld-9": {BuildID: "bld-9", Summary: ScanSummary{Packages: 42}}}
	if sc, err := b.Scan(context.Background(), "bld-9"); err != nil || sc.Summary.Packages != 42 {
		t.Errorf("scan = %+v, %v", sc, err)
	}
}

// K8sOutcomes reads the first failed container in pod order, the Job's deadline
// verdict, and looks for an envelope only when scan-gate ran. The fake clientset
// answers every log read with "fake logs", which holds no envelope.
func TestK8sOutcomesReadsThePod(t *testing.T) {
	ctx := context.Background()
	term := func(name string, code int32, msg string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: msg}}}
	}
	pod := func(inits ...corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "build-bld-1-abcde", Namespace: "felis-build", Labels: map[string]string{LabelBuildID: "bld-1"}},
			Status:     corev1.PodStatus{InitContainerStatuses: inits},
		}
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "build-bld-1", Namespace: "felis-build"}}

	// Another build's pod, failed at a different step, sorts ahead of bld-1's.
	other := pod(term("egress-gate", 7, "denied"))
	other.Name, other.Labels = "build-bld-0-zzzzz", map[string]string{LabelBuildID: "bld-0"}
	k := NewK8sOutcomes(fake.NewSimpleClientset(job, other, pod(term("egress-gate", 0, ""), term("kaniko", 1, "boom"),
		corev1.ContainerStatus{Name: "trivy", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}})), Config{})
	out, err := k.Outcome(ctx, "bld-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.FailedStep != "kaniko" || out.ExitCode != 1 || out.Message != "boom" || out.Scan != nil || out.ScanErr != nil || out.DeadlineExceeded {
		t.Errorf("kaniko failure = %+v", out)
	}

	cs := fake.NewSimpleClientset(job, pod(term("egress-gate", 0, ""), term("kaniko", 0, ""),
		term("trivy", 0, ""), term("sbom", 0, ""), term("scan-gate", 1, "the scan blocked the image: 1 HIGH (CVE-1)")))
	k = NewK8sOutcomes(cs, Config{})
	out, err = k.Outcome(ctx, "bld-1")
	if err != nil {
		t.Fatal(err)
	}
	// The fake API serves "fake logs" for every log read, which holds no envelope.
	if out.FailedStep != "scan-gate" || out.ScanErr == nil || out.ScanErr.Error() != "the scan-gate log holds no scan envelope" {
		t.Errorf("scan-gate failure = %+v", out)
	}
	var logReads []string
	for _, a := range cs.Actions() {
		if a.GetSubresource() == "log" {
			o := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
			logReads = append(logReads, fmt.Sprintf("%s/%s limit %d", a.GetNamespace(), o.Container, *o.LimitBytes))
		}
	}
	if !reflect.DeepEqual(logReads, []string{"felis-build/scan-gate limit 9437184"}) {
		t.Errorf("log reads = %q", logReads)
	}

	pushed := pod(term("egress-gate", 0, ""), term("kaniko", 0, ""), term("trivy", 0, ""), term("sbom", 0, ""), term("scan-gate", 0, "the scan passed"))
	pushed.Status.ContainerStatuses = []corev1.ContainerStatus{term("push", 1, "UNAUTHORIZED: authentication required")}
	k = NewK8sOutcomes(fake.NewSimpleClientset(job, pushed), Config{})
	out, err = k.Outcome(ctx, "bld-1")
	if err != nil || out.FailedStep != "push" || out.ExitCode != 1 || out.Message != "UNAUTHORIZED: authentication required" {
		t.Errorf("push failure = %+v, %v", out, err)
	}

	deadline := job.DeepCopy()
	deadline.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"}}
	k = NewK8sOutcomes(fake.NewSimpleClientset(deadline), Config{})
	out, err = k.Outcome(ctx, "bld-1")
	if err != nil || !out.DeadlineExceeded || out.FailedStep != "" {
		t.Errorf("deadline = %+v, %v", out, err)
	}

	backoff := job.DeepCopy()
	backoff.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	k = NewK8sOutcomes(fake.NewSimpleClientset(backoff), Config{})
	if out, err = k.Outcome(ctx, "bld-1"); err != nil || out.DeadlineExceeded {
		t.Errorf("backoff = %+v, %v", out, err)
	}

	k = NewK8sOutcomes(fake.NewSimpleClientset(pod(term("egress-gate", 0, ""))), Config{Namespace: "elsewhere"})
	if out, err = k.Outcome(ctx, "bld-1"); err != nil || out.FailedStep != "" {
		t.Errorf("other namespace = %+v, %v", out, err)
	}
}
