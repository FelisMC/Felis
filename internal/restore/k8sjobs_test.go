package restore

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return scheme
}

func testParams() JobParams {
	return JobParams{
		Server:    "survival",
		WorldPVC:  "world-survival-0",
		BackupPVC: "felis-backups",
		Namespace: "minecraft",
		Image:     "felis:test",
		BackupRef: "/backups/x.tar.gz",
	}
}

// A finished Job still holding the deterministic name must be replaced, not
// treated as an in-flight coalesce — otherwise the retry after a failed restore
// is answered 202 while nothing runs (found by an E2E audit).
func TestCreateRestoreJobReplacesFinishedJob(t *testing.T) {
	finished := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-survival", Namespace: "minecraft"},
		Status: batchv1.JobStatus{
			Failed:     1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "True"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(finished).Build()

	if err := NewK8sJobs(c).CreateRestoreJob(context.Background(), testParams()); err != nil {
		t.Fatalf("CreateRestoreJob: %v", err)
	}
	var got batchv1.Job
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "restore-survival"}, &got); err != nil {
		t.Fatalf("replacement job missing: %v", err)
	}
	if restoreJobFinished(&got) {
		t.Errorf("replacement job is already finished: %+v", got.Status)
	}
}

// An in-flight Job keeps the idempotent coalesce: a duplicate enqueue during a
// running restore is absorbed, and the running Job is left untouched.
func TestCreateRestoreJobCoalescesInFlightJob(t *testing.T) {
	inFlight := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-survival", Namespace: "minecraft"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(inFlight).Build()

	err := NewK8sJobs(c).CreateRestoreJob(context.Background(), testParams())
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateRestoreJob = %v, want ErrAlreadyExists", err)
	}
	var got batchv1.Job
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "restore-survival"}, &got); err != nil {
		t.Fatalf("in-flight job should stay: %v", err)
	}
	if got.Status.Active != 1 {
		t.Errorf("in-flight job was disturbed: %+v", got.Status)
	}
}

// restoreJobFinished decides whether a name collision is a genuine in-flight
// coalesce (ErrAlreadyExists) or a finished Job whose deterministic name must be
// replaced so a retry enqueues for real. Regression: a FAILED restore used to
// absorb every retry for the rest of its ten-minute TTL — the API answered 202
// "restoring" while nothing ran (found by an E2E audit against a live cluster).
func TestRestoreJobFinished(t *testing.T) {
	completion := metav1.NewTime(time.Now())
	cases := []struct {
		name string
		job  batchv1.Job
		want bool
	}{
		{"running", batchv1.Job{Status: batchv1.JobStatus{Active: 1}}, false},
		{"created-not-started", batchv1.Job{}, false},
		{"succeeded", batchv1.Job{Status: batchv1.JobStatus{
			Succeeded: 1, CompletionTime: &completion,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}},
		}}, true},
		{"failed", batchv1.Job{Status: batchv1.JobStatus{
			Failed:     1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "True"}},
		}}, true},
		{"failed-and-some-active", batchv1.Job{Status: batchv1.JobStatus{Active: 1, Failed: 1}}, false},
		// The failure is decided while the pod is still being torn down: the
		// world-volume lock already admits the next restore, so this must too.
		{"failure-target-pod-terminating", batchv1.Job{Status: batchv1.JobStatus{
			Active:     1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailureTarget, Status: "True"}},
		}}, true},
	}
	for _, tc := range cases {
		if got := restoreJobFinished(&tc.job); got != tc.want {
			t.Errorf("%s: restoreJobFinished = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A running restore of a different archive refuses the request instead of
// absorbing it: the caller would otherwise get a 202 naming the backup it chose
// while another one is extracted.
func TestCreateRestoreJobRefusesAnotherArchiveInFlight(t *testing.T) {
	inFlight := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "restore-survival", Namespace: "minecraft",
			Annotations: map[string]string{AnnotationBackupRef: "/backups/other.tar.gz"},
		},
		Status: batchv1.JobStatus{Active: 1},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(inFlight).Build()

	err := NewK8sJobs(c).CreateRestoreJob(context.Background(), testParams())
	if !errors.Is(err, ErrOtherRestoreRunning) {
		t.Fatalf("CreateRestoreJob = %v, want ErrOtherRestoreRunning", err)
	}
	if err := (&Restorer{Jobs: NewK8sJobs(c), Config: Config{Image: "felis:test", BackupPVC: "felis-backups"}}).
		Restore(context.Background(), "survival", "/backups/x.tar.gz"); !errors.Is(err, ErrOtherRestoreRunning) {
		t.Fatalf("Restore = %v, want ErrOtherRestoreRunning", err)
	}

	// The same archive still coalesces.
	p := testParams()
	p.BackupRef = "/backups/other.tar.gz"
	if err := NewK8sJobs(c).CreateRestoreJob(context.Background(), p); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("same archive: CreateRestoreJob = %v, want ErrAlreadyExists", err)
	}
}
