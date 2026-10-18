package main

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"felis.lolicon.best/internal/platform"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLeadLoopsRunsTheLoopsOnlyWithTheLease(t *testing.T) {
	apiLeaseDuration, apiLeaseRenew, apiLeaseRetry = 1500*time.Millisecond, time.Second, 200*time.Millisecond
	t.Cleanup(func() { apiLeaseDuration, apiLeaseRenew, apiLeaseRetry = 15*time.Second, 10*time.Second, 2*time.Second })

	// The record's duration decides when a candidate may take over; 3s is fifteen renewals,
	// so a renewal running late under a loaded test run still keeps the lease.
	holder, seconds := "old-pod", int32(3)
	now := metav1.NewMicroTime(time.Now())
	cs := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: platform.LeaseAPILoops, Namespace: "felis"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, AcquireTime: &now, RenewTime: &now},
	})
	// The old holder renews every 200ms until told to stop.
	renewing, stopRenewing := context.WithCancel(context.Background())
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		for renewing.Err() == nil {
			lease, err := cs.CoordinationV1().Leases("felis").Get(context.Background(), platform.LeaseAPILoops, metav1.GetOptions{})
			if err == nil {
				t := metav1.NewMicroTime(time.Now())
				lease.Spec.RenewTime = &t
				_, _ = cs.CoordinationV1().Leases("felis").Update(context.Background(), lease, metav1.UpdateOptions{})
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	var mu sync.Mutex
	started := 0
	count := func() int { mu.Lock(); defer mu.Unlock(); return started }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		leadLoops(ctx, cs, "felis", func(context.Context) { mu.Lock(); started++; mu.Unlock() }, io.Discard)
		close(done)
	}()
	time.Sleep(3 * time.Second)
	if count() != 0 {
		t.Fatal("the loops started while another felis-api renewed the lease")
	}

	stopRenewing()
	<-renewed
	deadline := time.Now().Add(10 * time.Second)
	for count() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the loops did not start once the old holder stopped renewing")
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done
	lease, err := cs.CoordinationV1().Leases("felis").Get(context.Background(), platform.LeaseAPILoops, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
		t.Errorf("a stopped felis-api kept the lease: %q", *lease.Spec.HolderIdentity)
	}
}
