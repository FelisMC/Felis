package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeJobStatus struct {
	jobs []AsyncJob
	err  error
	got  string
}

func (f *fakeJobStatus) LatestJobs(_ context.Context, server string) ([]AsyncJob, error) {
	f.got = server
	return f.jobs, f.err
}

// TestServerJobsHandler covers GET /api/v1/servers/{name}/jobs: the owner sees the
// recorded outcomes, strangers are refused, a nil reader is an honest 503, and a
// reader error surfaces as 500.
func TestServerJobsHandler(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	stranger := &Principal{UserID: "other", Email: "other@example.net", Role: "user"}

	mk := func(reader JobStatusReader) (*API, *fakeJobStatus) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		a := newTestAPI(repo, newFakeCluster())
		a.External = staticExternal{p: owner}
		var fjs *fakeJobStatus
		if reader != nil {
			a.JobStatus = reader
		}
		if fj, ok := reader.(*fakeJobStatus); ok {
			fjs = fj
		}
		return a, fjs
	}

	t.Run("owner sees failed job", func(t *testing.T) {
		fjs := &fakeJobStatus{jobs: []AsyncJob{{Name: "restore-survival", Kind: "restore", State: "failed", Message: "backoff limit exceeded"}}}
		a, _ := mk(fjs)
		w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/jobs", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Server string     `json:"server"`
			Jobs   []AsyncJob `json:"jobs"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if resp.Server != "survival" || len(resp.Jobs) != 1 || resp.Jobs[0].State != "failed" {
			t.Fatalf("unexpected payload %+v", resp)
		}
		if fjs.got != "survival" {
			t.Fatalf("reader asked for %q", fjs.got)
		}
	})

	t.Run("stranger -> 403", func(t *testing.T) {
		fjs := &fakeJobStatus{}
		a, _ := mk(fjs)
		a.External = staticExternal{p: stranger}
		if w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/jobs", "", nil); w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
	})

	t.Run("nil reader -> 503", func(t *testing.T) {
		a, _ := mk(nil)
		if w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/jobs", "", nil); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("code = %d, want 503", w.Code)
		}
	})

	t.Run("reader error -> 500", func(t *testing.T) {
		fjs := &fakeJobStatus{err: errors.New("apiserver down")}
		a, _ := mk(fjs)
		if w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/jobs", "", nil); w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500", w.Code)
		}
	})

	t.Run("unknown server -> 404", func(t *testing.T) {
		a, _ := mk(&fakeJobStatus{})
		if w := do(a.ExternalHandler(), "GET", "/api/v1/servers/ghost/jobs", "", nil); w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})
}

// TestJobToAsyncJob pins the Job→AsyncJob projection: kinds come from managed-by,
// terminal conditions decide state, and unknown owners are dropped.
func TestJobToAsyncJob(t *testing.T) {
	start := metav1.NewTime(time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC))
	done := metav1.NewTime(time.Date(2026, 9, 22, 10, 5, 0, 0, time.UTC))

	failed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "restore-survival", Labels: map[string]string{jobManagedByLabel: jobManagedByRestore}},
		Status: batchv1.JobStatus{
			StartTime: &start,
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
				LastTransitionTime: done,
			}},
		},
	}
	aj, ok := jobToAsyncJob(failed)
	if !ok || aj.Kind != "restore" || aj.State != "failed" || aj.Name != "restore-survival" {
		t.Fatalf("failed job projection = %+v ok=%v", aj, ok)
	}
	if !aj.FinishedAt.Equal(done.Time) || !aj.StartedAt.Equal(start.Time) {
		t.Fatalf("timestamps = %+v", aj)
	}

	complete := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-survival-1", Labels: map[string]string{jobManagedByLabel: jobManagedByBackup}},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: done,
		}}},
	}
	if aj, ok := jobToAsyncJob(complete); !ok || aj.Kind != "backup" || aj.State != "succeeded" {
		t.Fatalf("complete job projection = %+v ok=%v", aj, ok)
	}

	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{jobManagedByLabel: jobManagedByBackup}}}
	if aj, ok := jobToAsyncJob(running); !ok || aj.State != "running" {
		t.Fatalf("running job projection = %+v ok=%v", aj, ok)
	}

	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{jobManagedByLabel: "someone-else"}}}
	if _, ok := jobToAsyncJob(foreign); ok {
		t.Fatal("foreign job must be dropped")
	}
}

// TestLatestJobsExplainsFailures: a failed Job reports the error its container
// exited on (the last line of the terminated message), newest pod first, and
// keeps the condition text when no pod explains it.
func TestLatestJobsExplainsFailures(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	labels := func(job string) map[string]string {
		return map[string]string{jobServerLabel: "survival", jobManagedByLabel: jobManagedByBackup, "job-name": job}
	}
	at := func(min int) metav1.Time { return metav1.NewTime(time.Date(2026, 9, 24, 10, min, 0, 0, time.UTC)) }
	failedJob := func(name string, min int) *batchv1.Job {
		start := at(min)
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft", Labels: labels(name)},
			Status: batchv1.JobStatus{StartTime: &start, Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
			}}},
		}
	}
	pod := func(name, job string, min int, exit int32, msg string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft", Labels: labels(job), CreationTimestamp: at(min)},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "backup", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: msg}},
			}}},
		}
	}
	// Killed for memory mid-walk: the last line it printed says nothing of that.
	oom := pod("c-1", "backup-survival-c", 4, 137, "archiving survival\n")
	oom.Status.ContainerStatuses[0].State.Terminated.Reason = "OOMKilled"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		failedJob("backup-survival-a", 1),
		failedJob("backup-survival-b", 2),
		pod("a-1", "backup-survival-a", 1, 1, "felis backup: first try\n"),
		pod("a-2", "backup-survival-a", 3, 1,
			"archiving survival\nfelis backup: backup: not enough free disk for the archive: the world is 2.0 GiB\n"),
		pod("b-1", "backup-survival-b", 2, 0, "done"),
		failedJob("backup-survival-c", 4), oom,
	).Build()

	jobs, err := NewK8sJobStatus(c, "minecraft").LatestJobs(context.Background(), "survival")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, j := range jobs {
		got[j.Name] = j.Message
	}
	if want := "felis backup: backup: not enough free disk for the archive: the world is 2.0 GiB"; got["backup-survival-a"] != want {
		t.Errorf("a: message = %q, want %q", got["backup-survival-a"], want)
	}
	if want := "Job has reached the specified backoff limit"; got["backup-survival-b"] != want {
		t.Errorf("b: message = %q, want the condition text", got["backup-survival-b"])
	}
	if want := "the job ran out of memory and the system stopped it (OOMKilled)"; got["backup-survival-c"] != want {
		t.Errorf("c: message = %q, want %q", got["backup-survival-c"], want)
	}
}
