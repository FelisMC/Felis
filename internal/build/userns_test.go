package build

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// "auto" follows the probe through every copy of the Config; "on" and "off"
// ignore it.
func TestUserNamespacesMode(t *testing.T) {
	probe := new(atomic.Bool)
	for _, tc := range []struct {
		mode  string
		probe bool
		want  bool
	}{
		{"", false, false}, {"", true, true}, {UserNamespacesAuto, true, true},
		{UserNamespacesOn, false, true}, {UserNamespacesOff, true, false},
	} {
		b, _, jb := newBuilder()
		b.Config.UserNamespaces = tc.mode
		b.Config.UserNamespacesProbe = probe
		probe.Store(tc.probe)
		if _, err := b.Submit(context.Background(), goodRequest()); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if got := jb.created[0].UserNamespaces; got != tc.want {
			t.Errorf("mode %q, probe %v: UserNamespaces = %v, want %v", tc.mode, tc.probe, got, tc.want)
		}
	}
	b, _, jb := newBuilder()
	if _, err := b.Submit(context.Background(), goodRequest()); err != nil || jb.created[0].UserNamespaces {
		t.Errorf("no probe wired: err %v, UserNamespaces %v", err, jb.created[0].UserNamespaces)
	}
}

// The probe pod has the shape that needs user-namespace support, and runs
// nothing that matters.
func TestUsernsProbeJobShape(t *testing.T) {
	job := UsernsProbeJob("felis-build", "felis-build", "felis:test", "userns-probe-x")
	spec := job.Spec.Template.Spec
	if spec.HostUsers == nil || *spec.HostUsers {
		t.Fatal("the probe must ask for hostUsers: false")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken || spec.ServiceAccountName != "felis-build" {
		t.Error("the probe must run as the bare build SA with no token")
	}
	c := spec.Containers[0]
	if c.Image != "felis:test" || len(c.Args) != 1 || c.Args[0] != "version" {
		t.Errorf("probe container runs %s %v", c.Image, c.Args)
	}
	if c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 || len(c.SecurityContext.Capabilities.Add) != 3 {
		t.Error("the probe must run as root with kaniko's capabilities, the case user namespaces must carry")
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].EmptyDir == nil {
		t.Error("the probe must mount an emptyDir, as build pods do")
	}
	if job.Labels[LabelManagedBy] != managedByValue {
		t.Error("the probe must sit under the build egress policy")
	}
}

func TestProbeUserNamespaces(t *testing.T) {
	timeout, poll := usernsProbeTimeout, usernsProbePoll
	t.Cleanup(func() { usernsProbeTimeout, usernsProbePoll = timeout, poll })
	usernsProbePoll = 5 * time.Millisecond

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	for _, tc := range []struct {
		name string
		cond batchv1.JobConditionType
		want bool
	}{
		{"complete", batchv1.JobComplete, true},
		{"failed", batchv1.JobFailed, false},
		{"never finishes", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A finished probe must answer long before the timeout would.
			usernsProbeTimeout = 5 * time.Second
			if tc.cond == "" {
				usernsProbeTimeout = 100 * time.Millisecond
			}
			start := time.Now()
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&batchv1.Job{}).Build()
			jobs := NewK8sJobs(cl, Config{})
			type result struct {
				ok  bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				ok, err := jobs.ProbeUserNamespaces(context.Background(), "felis:test")
				done <- result{ok, err}
			}()
			if tc.cond != "" {
				finishProbe(t, cl, tc.cond)
			}
			r := <-done
			if r.err != nil || r.ok != tc.want {
				t.Fatalf("ProbeUserNamespaces = (%v, %v), want (%v, nil)", r.ok, r.err, tc.want)
			}
			if tc.cond != "" && time.Since(start) > 2*time.Second {
				t.Fatalf("the probe answered after %s: it waited out the timeout instead of reading the verdict", time.Since(start))
			}
			var left batchv1.JobList
			if err := cl.List(context.Background(), &left, client.InNamespace(defaultNamespace)); err != nil || len(left.Items) != 0 {
				t.Fatalf("probe jobs left behind: %d (%v)", len(left.Items), err)
			}
		})
	}
}

// finishProbe waits for the probe Job to appear and marks it finished.
func finishProbe(t *testing.T, cl client.Client, cond batchv1.JobConditionType) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var jobs batchv1.JobList
		if err := cl.List(context.Background(), &jobs, client.InNamespace(defaultNamespace)); err != nil {
			t.Fatal(err)
		}
		if len(jobs.Items) == 1 {
			job := jobs.Items[0]
			job.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue}}
			if err := cl.Status().Update(context.Background(), &job); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the probe job never appeared")
}
