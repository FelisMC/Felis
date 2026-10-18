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

// Wait for NetworkPolicy enforcement before starting untrusted workloads.
// Builds fail closed; servers allow the probe because configured CIDRs may permit the same node.
func cmdEgressGate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("egress-gate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	probe := fs.String("probe", "", "host:port the pod's NetworkPolicy denies (default: the Kubernetes API Service from KUBERNETES_SERVICE_HOST/PORT)")
	positive := fs.String("positive-probe", "", "allowed host:port that must remain reachable during denial checks")
	wait := fs.Duration("wait", 2*time.Minute, "how long the probe may keep answering before the gate gives up")
	failOpen := fs.Bool("fail-open", false, "when --wait runs out, warn and let the pod go on instead of refusing it")
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
		if *positive != "" {
			allowed, err := net.DialTimeout("tcp", *positive, egressDialTimeout)
			if err != nil {
				if time.Since(start) >= *wait {
					fmt.Fprintln(stderr, "felis egress-gate: positive probe unavailable; refusing to start", err)
					return 1
				}
				time.Sleep(egressPollInterval)
				continue
			}
			allowed.Close()
		}
		conn, err := net.DialTimeout("tcp", *probe, egressDialTimeout)
		if err != nil {
			fmt.Fprintf(stdout, "felis egress-gate: %s is unreachable after %s (%v); the egress lock is in effect\n",
				*probe, time.Since(start).Round(time.Millisecond), err)
			return 0
		}
		_ = conn.Close()
		if time.Since(start) >= *wait {
			if *failOpen {
				fmt.Fprintf(stderr, "felis egress-gate: %s still answers after %s; starting anyway. Either this namespace's "+
					"NetworkPolicy is not enforced (a CNI without NetworkPolicy support, or k3s started with "+
					"--disable-network-policy), or an allowed CIDR admits the address behind it\n", *probe, *wait)
				return 0
			}
			fmt.Fprintf(stderr, "felis egress-gate: %s still answers after %s: the build namespace's NetworkPolicy is not enforced "+
				"(a CNI without NetworkPolicy support, or k3s started with --disable-network-policy); refusing to run the build\n",
				*probe, *wait)
			return 1
		}
		time.Sleep(egressPollInterval)
	}
}
