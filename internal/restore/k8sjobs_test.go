package restore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/archivetransfer"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
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

func TestCreateRestoreJobUsesSecretKeyRefForRemoteToken(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if j, ok := obj.(*batchv1.Job); ok && j.UID == "" {
				j.UID = types.UID("uid-" + j.Name)
			}
			return cl.Create(ctx, obj, opts...)
		},
	}).Build()
	p := testParams()
	p.BackupPVC = ""
	p.SourceURL = "http://archive.felis.svc:8081/t"
	p.Token = "secret-transfer-token"
	p.SHA256 = strings.Repeat("a", 64)
	p.MaxBytes = 1024

	if err := NewK8sJobs(c).CreateRestoreJob(context.Background(), p); err != nil {
		t.Fatalf("CreateRestoreJob: %v", err)
	}
	var got batchv1.Job
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: "restore-survival"}, &got); err != nil {
		t.Fatalf("job missing: %v", err)
	}
	var secretName string
	for _, e := range got.Spec.Template.Spec.Containers[0].Env {
		if e.Name == archivetransfer.TokenEnv {
			if e.Value != "" {
				t.Fatalf("token leaked in plaintext env: %q", e.Value)
			}
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("token env missing SecretKeyRef: %+v", e)
			}
			secretName = e.ValueFrom.SecretKeyRef.Name
		}
	}
	if secretName == "" {
		t.Fatal("missing FELIS_ARCHIVE_TOKEN env")
	}
	var sec corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "minecraft", Name: secretName}, &sec); err != nil {
		t.Fatalf("token secret %s missing: %v", secretName, err)
	}
	if string(sec.Data[TokenSecretKey]) != p.Token {
		t.Fatalf("secret token = %q, want %q", string(sec.Data[TokenSecretKey]), p.Token)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != got.Name {
		t.Fatalf("secret owners = %+v, want %s", sec.OwnerReferences, got.Name)
	}
}
