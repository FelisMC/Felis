package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/platform"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Lease expiry bounds takeover latency; clean shutdown releases immediately.
var (
	apiLeaseDuration = 15 * time.Second
	apiLeaseRenew    = 10 * time.Second
	apiLeaseRetry    = 2 * time.Second
)

// leadLoops stops background work when its leadership lease is lost.
func leadLoops(ctx context.Context, cs kubernetes.Interface, namespace string, loops func(context.Context), stderr io.Writer) {
	identity := leaseIdentity()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: platform.LeaseAPILoops, Namespace: namespace},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	for ctx.Err() == nil {
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			LeaseDuration:   apiLeaseDuration,
			RenewDeadline:   apiLeaseRenew,
			RetryPeriod:     apiLeaseRetry,
			ReleaseOnCancel: true,
			Name:            platform.LeaseAPILoops,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					fmt.Fprintf(stderr, "felis api: holding %s as %s; the background loops run here\n", platform.LeaseAPILoops, identity)
					loops(ctx)
				},
				OnStoppedLeading: func() {
					fmt.Fprintf(stderr, "felis api: %s released; the background loops stopped here\n", platform.LeaseAPILoops)
				},
			},
		})
		if err != nil {
			fmt.Fprintf(stderr, "felis api: background-loop lease: %v\n", err)
			return
		}
		elector.Run(ctx)
	}
}

// leaseIdentity names this process in the lease: its pod and a random tag, so a pod that
// restarts in place is a new holder.
func leaseIdentity() string {
	host, _ := os.Hostname()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return host + "-" + hex.EncodeToString(b[:])
}

// podNamespace is the namespace this pod runs in, read from its service account mount,
// else the control namespace.
func podNamespace() string {
	raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if ns := strings.TrimSpace(string(raw)); err == nil && ns != "" {
		return ns
	}
	return platform.DefaultControlNamespace
}
