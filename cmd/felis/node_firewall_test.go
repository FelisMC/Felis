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
