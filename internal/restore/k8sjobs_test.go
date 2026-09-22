package restore

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	}
	for _, tc := range cases {
		if got := restoreJobFinished(&tc.job); got != tc.want {
			t.Errorf("%s: restoreJobFinished = %v, want %v", tc.name, got, tc.want)
		}
	}
}
