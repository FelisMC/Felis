package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestDistributedFirewallPathsAndSyntax(t *testing.T) {
	for _, controller := range []bool{false, true} {
		args := []string{"--dry-run", "--peers", "192.0.2.1/32,192.0.2.2/32", "--controller-ip", "192.0.2.1"}
		if controller {
			args = append(args, "--controller")
		}
		var out, err bytes.Buffer
		if code := nodeFirewall(args, &out, &err); code != 0 {
			t.Fatal(code, err.String())
		}
		script := out.String()
		for _, must := range []string{"-I INPUT 1 -j FELIS-HOST", "-I FORWARD 1 -j FELIS-FORWARD", "-t raw -I PREROUTING 1 -j FELIS-NODEPORT", "-A FELIS-HOST -s 10.42.0.0/16 -j DROP", "--dst-type LOCAL -p tcp --dport 30443 -j DROP", "-d 169.254.0.0/16 -j DROP", "--dst-type LOCAL -p tcp --syn -j DROP", "-d 10.43.0.1 -p tcp --dport 443 --syn -j DROP"} {
			if !strings.Contains(script, must) {
				t.Fatal("missing protection", must)
			}
		}
		if !controller && strings.Contains(script, " -p tcp --dport 6443 -j RETURN") {
			t.Fatal("worker exposed apiserver")
		}
		check := exec.Command("bash", "-n")
		check.Stdin = strings.NewReader(script)
		if result, e := check.CombinedOutput(); e != nil {
			t.Fatalf("invalid firewall script: %s %v", result, e)
		}
	}
	var out, err bytes.Buffer
	if code := nodeFirewall([]string{"--dry-run", "--peers", "10.0.0.0/8", "--controller-ip", "10.0.0.1"}, &out, &err); code != 2 {
		t.Fatal("broad node range accepted")
	}
}

// TestControllerPeersFirewall pins what controller high availability opens: every
// controller reaches each node's kubelet, and on a controller the others' etcd and
// supervisor ports, ahead of the drop; a worker never opens etcd; a broad range is refused.
func TestControllerPeersFirewall(t *testing.T) {
	for _, controller := range []bool{false, true} {
		args := []string{"--dry-run", "--peers", "192.0.2.1/32,192.0.2.2/32,192.0.2.3/32", "--controller-ip", "192.0.2.1", "--controller-peers", "192.0.2.1/32,192.0.2.3/32"}
		if controller {
			args = append(args, "--controller")
		}
		var out, errb bytes.Buffer
		if code := nodeFirewall(args, &out, &errb); code != 0 {
			t.Fatal(code, errb.String())
		}
		script := out.String()
		drop := strings.Index(script, "--dports 6443,6444,10250,10255,2379,2380,5000,5001,15432 -j DROP")
		for _, c := range []string{"192.0.2.1/32", "192.0.2.3/32"} {
			kubelet := strings.Index(script, "-s "+c+" -p tcp --dport 10250 -j RETURN")
			if kubelet < 0 || kubelet > drop {
				t.Errorf("controller=%v: kubelet from %s not opened before the drop", controller, c)
			}
			etcd := strings.Index(script, "-s "+c+" -p tcp -m multiport --dports 2379,2380,6444 -j RETURN")
			if controller && (etcd < 0 || etcd > drop) {
				t.Errorf("etcd from %s not opened on a controller", c)
			}
			if !controller && etcd >= 0 {
				t.Errorf("a worker opened etcd to %s", c)
			}
		}
		if strings.Contains(script, "-s 192.0.2.2/32 -p tcp -m multiport --dports 2379") {
			t.Error("a worker address reached etcd")
		}
	}
	var out, errb bytes.Buffer
	if code := nodeFirewall([]string{"--dry-run", "--peers", "192.0.2.1/32", "--controller-ip", "192.0.2.1", "--controller-peers", "192.0.2.0/24"}, &out, &errb); code != 2 {
		t.Error("a broad controller range was accepted")
	}
}
