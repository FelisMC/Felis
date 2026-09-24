package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// Vars so tests can shrink them. A dial that neither connects nor is refused
// within egressDialTimeout counts as blocked: a policy that drops packets looks
// exactly like that.
var (
	egressDialTimeout  = 500 * time.Millisecond
	egressPollInterval = 200 * time.Millisecond
)

// cmdEgressGate is the first initContainer of every build pod. The pod's
// NetworkPolicy is programmed asynchronously after the pod starts (live on k3s:
// a build-labelled pod reached the internet and the Kubernetes API for its first
// ~0.7 s), so the gate dials a destination the policy denies until it stops
// answering, and only then lets the pod's next container, eventually the
// untrusted Dockerfile, start.
//
// The default probe is the Kubernetes API Service, which the kubelet names in
// every pod's environment and the build policy never admits. A probe that still
// answers after --wait means the policy is not enforced at all (a CNI without
// NetworkPolicy support, or k3s run with --disable-network-policy), and the
// build fails closed.
func cmdEgressGate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("egress-gate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	probe := fs.String("probe", "", "host:port the build NetworkPolicy denies (default: the Kubernetes API Service from KUBERNETES_SERVICE_HOST/PORT)")
	wait := fs.Duration("wait", 2*time.Minute, "how long the probe may keep answering before the build is refused")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *probe == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			fmt.Fprintln(stderr, "felis egress-gate: no --probe and no KUBERNETES_SERVICE_HOST/PORT to default to")
			return 2
		}
		*probe = net.JoinHostPort(host, port)
	}

	start := time.Now()
	for {
		conn, err := net.DialTimeout("tcp", *probe, egressDialTimeout)
		if err != nil {
			fmt.Fprintf(stdout, "felis egress-gate: %s is unreachable after %s (%v); the egress lock is in effect\n",
				*probe, time.Since(start).Round(time.Millisecond), err)
			return 0
		}
		_ = conn.Close()
		if time.Since(start) >= *wait {
			fmt.Fprintf(stderr, "felis egress-gate: %s still answers after %s: the build namespace's NetworkPolicy is not enforced "+
				"(a CNI without NetworkPolicy support, or k3s started with --disable-network-policy); refusing to run the build\n",
				*probe, *wait)
			return 1
		}
		time.Sleep(egressPollInterval)
	}
}
