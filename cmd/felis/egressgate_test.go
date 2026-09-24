package main

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

func shrinkEgressGate(t *testing.T) {
	t.Helper()
	dial, poll := egressDialTimeout, egressPollInterval
	egressDialTimeout, egressPollInterval = 200*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { egressDialTimeout, egressPollInterval = dial, poll })
}

// The gate holds while the probe answers and lets the pod go on once the policy
// lands, which the test plays by closing the listener.
func TestEgressGateWaitsForTheLock(t *testing.T) {
	shrinkEgressGate(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 100)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			accepted <- struct{}{}
		}
	}()
	go func() {
		for i := 0; i < 3; i++ {
			<-accepted
		}
		_ = ln.Close()
	}()
	var out, errb bytes.Buffer
	if code := cmdEgressGate([]string{"--probe", ln.Addr().String(), "--wait", "10s"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "egress lock is in effect") {
		t.Errorf("stdout = %q", out.String())
	}
}

// A probe that keeps answering means no policy is enforced: the build must not run.
func TestEgressGateRefusesAnOpenNetwork(t *testing.T) {
	shrinkEgressGate(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	var out, errb bytes.Buffer
	if code := cmdEgressGate([]string{"--probe", ln.Addr().String(), "--wait", "100ms"}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not enforced") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestEgressGateDefaultsToTheKubernetesService(t *testing.T) {
	shrinkEgressGate(t)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	var out, errb bytes.Buffer
	if code := cmdEgressGate(nil, &out, &errb); code != 2 {
		t.Fatalf("exit %d without a probe, want 2", code)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close() // closed: the lock reads as in effect at once
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	out.Reset()
	if code := cmdEgressGate(nil, &out, &errb); code != 0 || !strings.Contains(out.String(), ln.Addr().String()) {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
}
