package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func nodeFirewall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("node firewall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	peers := fs.String("peers", "", "comma-separated exact node IP CIDRs")
	controller := fs.String("controller-ip", "", "controller address (optionally :6443)")
	main := fs.Bool("controller", false, "A hosts the public API NodePort")
	pod := fs.String("pod-cidr", "10.42.0.0/16", "cluster Pod CIDR")
	dryRun := fs.Bool("dry-run", false, "print firewall scripts without installing")
	controlNS := fs.String("control-namespace", "felis", "controller namespace")
	minecraftNS := fs.String("namespace", "minecraft", "game namespace")
	apiIP := fs.String("api-service-ip", "10.43.0.1", "Kubernetes API Service IP")
	port := fs.Int("node-port", 30443, "API NodePort to block on workers before DNAT")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if host, _, err := net.SplitHostPort(*controller); err == nil {
		*controller = host
	}
	if net.ParseIP(*apiIP) == nil || net.ParseIP(*controller) == nil || *port < 30000 || *port > 32767 {
		fmt.Fprintln(stderr, "node firewall: controller IP and NodePort required")
		return 2
	}
	if _, _, err := net.ParseCIDR(*pod); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var v4, v6 []string
	for _, p := range strings.Split(*peers, ",") {
		ip, n, err := net.ParseCIDR(p)
		if err != nil {
			fmt.Fprintln(stderr, "node firewall: invalid peer CIDR")
			return 2
		}
		ones, bits := n.Mask.Size()
		if ones != bits {
			fmt.Fprintln(stderr, "node firewall: only exact peer addresses accepted")
			return 2
		}
		if ip.To4() != nil {
			v4 = append(v4, n.String())
		} else {
			v6 = append(v6, n.String())
		}
	}
	script := "#!/bin/bash\nset -euo pipefail\n"
	for _, f := range []struct {
		bin      string
		peers    []string
		metadata string
	}{{"iptables", v4, "169.254.0.0/16"}, {"ip6tables", v6, "fe80::/10"}} {
		if f.bin == "ip6tables" && len(f.peers) == 0 {
			continue
		}
		b := f.bin + " -w 10"
		script += b + " -N FELIS-HOST 2>/dev/null || true\n" + b + " -F FELIS-HOST\n"
		script += b + " -N FELIS-FORWARD 2>/dev/null || true\n" + b + " -F FELIS-FORWARD\n"
		script += b + " -t raw -N FELIS-NODEPORT 2>/dev/null || true\n" + b + " -t raw -F FELIS-NODEPORT\n"
		for _, c := range []struct{ table, parent, chain string }{{"filter", "INPUT", "FELIS-HOST"}, {"filter", "FORWARD", "FELIS-FORWARD"}, {"raw", "PREROUTING", "FELIS-NODEPORT"}} {
			script += "while " + b + " -t " + c.table + " -D " + c.parent + " -j " + c.chain + " 2>/dev/null; do :; done\n" + b + " -t " + c.table + " -I " + c.parent + " 1 -j " + c.chain + "\n"
		}
		script += b + " -A FELIS-HOST -i lo -j RETURN\n"
		if *main {
			script += b + " -N FELIS-CONTROL 2>/dev/null || true\n" + b + " -A FELIS-HOST -j FELIS-CONTROL\n"
			script += b + " -t raw -N FELIS-CONTROL 2>/dev/null || true\n" + b + " -t raw -A FELIS-NODEPORT -j FELIS-CONTROL\n"
		}
		script += b + " -A FELIS-HOST -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN\n" + b + " -A FELIS-FORWARD -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN\n"
		family := 4
		if f.bin == "ip6tables" {
			family = 6
		}
		podIP, _, _ := net.ParseCIDR(*pod)
		podFamily := 6
		if podIP.To4() != nil {
			podFamily = 4
		}
		if family == podFamily {
			script += b + " -A FELIS-HOST -s " + *pod + " -j DROP\n"
			// raw precedes DNAT and kube-router's filter ACCEPT rules. Block new
			// host connections while preserving established Velocity/RCON replies.
			script += b + " -t raw -A FELIS-NODEPORT -s " + *pod + " -m addrtype --dst-type LOCAL -p tcp --syn -j DROP\n"
			script += b + " -t raw -A FELIS-NODEPORT -s " + *pod + " -m addrtype --dst-type LOCAL -p udp -j DROP\n"
			if (net.ParseIP(*apiIP).To4() != nil) == (family == 4) {
				script += b + " -t raw -A FELIS-NODEPORT -s " + *pod + " -d " + *apiIP + " -p tcp --dport 443 --syn -j DROP\n"
			}

			script += b + " -t raw -A FELIS-NODEPORT -s " + *pod + " -m addrtype --dst-type LOCAL -p tcp --dport " + strconv.Itoa(*port) + " -j DROP\n"
			script += b + " -A FELIS-FORWARD -s " + *pod + " -d " + f.metadata + " -j DROP\n"
			script += b + " -t raw -A FELIS-NODEPORT -s " + *pod + " -d " + f.metadata + " -j DROP\n"
			for _, p := range f.peers {
				script += b + " -A FELIS-FORWARD -s " + *pod + " -d " + p + " -j DROP\n"
				script += b + " -t raw -A FELIS-NODEPORT -s " + *pod + " -d " + p + " -p tcp --syn -j DROP\n"
				script += b + " -t raw -A FELIS-NODEPORT -s " + *pod + " -d " + p + " -p udp -j DROP\n"
			}
		}
		if !*main {
			script += b + " -t raw -A FELIS-NODEPORT -m addrtype --dst-type LOCAL -p tcp --dport " + strconv.Itoa(*port) + " -j DROP\n"
		}
		for _, p := range f.peers {
			script += b + " -A FELIS-HOST -s " + p + " -p udp --dport 51820:51821 -j RETURN\n"
		}
		script += b + " -A FELIS-HOST -p udp --dport 51820:51821 -j DROP\n"
		ctrlIP := net.ParseIP(*controller)
		ctrlFamily := 6
		if ctrlIP.To4() != nil {
			ctrlFamily = 4
		}
		if *main {
			for _, p := range f.peers {
				script += b + " -A FELIS-HOST -s " + p + " -p tcp --dport 6443 -j RETURN\n"
			}
		}
		if ctrlFamily == family {
			script += b + " -A FELIS-HOST -s " + ctrlIP.String() + " -p tcp --dport 10250 -j RETURN\n"
		}
		script += b + " -A FELIS-HOST -p tcp -m multiport --dports 6443,6444,10250,10255,2379,2380,5000,5001,15432 -j DROP\n"
	}
	if *dryRun {
		fmt.Fprint(stdout, script)
		if *main {
			fmt.Fprint(stdout, controlFirewallScript(*controlNS, *minecraftNS))
		}
		return 0
	}
	if err := os.MkdirAll("/etc/felis", 0700); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := os.WriteFile("/etc/felis/node-firewall.sh", []byte(script), 0700); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	unit := "[Unit]\nDescription=Felis host and NodePort isolation\nAfter=network-online.target firewalld.service ufw.service\nBefore=k3s.service k3s-agent.service\n[Service]\nType=oneshot\nExecStart=/etc/felis/node-firewall.sh\nRemainAfterExit=yes\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile("/etc/systemd/system/felis-node-firewall.service", []byte(unit), 0644); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *main {
		refresh := controlFirewallScript(*controlNS, *minecraftNS)
		if err := os.WriteFile("/etc/felis/control-firewall.sh", []byte(refresh), 0700); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		svc := "[Unit]\nDescription=Refresh exact controller Pod access to the apiserver\nAfter=k3s.service\n[Service]\nType=oneshot\nExecStart=/etc/felis/control-firewall.sh\n"
		timer := "[Unit]\nDescription=Track trusted controller Pods after rescheduling\n[Timer]\nOnBootSec=5s\nOnUnitActiveSec=5s\n[Install]\nWantedBy=timers.target\n"
		if err := os.WriteFile("/etc/systemd/system/felis-control-firewall.service", []byte(svc), 0644); err != nil {
			return 1
		}
		if err := os.WriteFile("/etc/systemd/system/felis-control-firewall.timer", []byte(timer), 0644); err != nil {
			return 1
		}
	}
	for _, cmd := range []*exec.Cmd{exec.Command("systemctl", "daemon-reload"), exec.Command("systemctl", "enable", "felis-node-firewall.service"), exec.Command("bash", "/etc/felis/node-firewall.sh")} {
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if *main {
		cmd := exec.Command("systemctl", "enable", "--now", "felis-control-firewall.timer")
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}

func controlFirewallScript(controlNS, minecraftNS string) string {
	return "#!/bin/bash\nset -euo pipefail\niptables -w 10 -F FELIS-CONTROL\niptables -w 10 -t raw -F FELIS-CONTROL\n/usr/local/bin/k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}{\" \"}{.spec.serviceAccountName}{\" \"}{.status.podIP}{\"\\n\"}{end}' | while read -r ns sa ip; do\ncase \"$ns/$sa\" in " + shellQuote(controlNS+"/felis-api") + "|" + shellQuote(controlNS+"/felis-operator") + "|" + shellQuote(minecraftNS+"/felis-reaper") + "|kube-system/*) ;; *) continue;; esac\n[[ $ip =~ ^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$ ]] || continue\niptables -w 10 -A FELIS-CONTROL -s \"$ip/32\" -p tcp --dport 6443 -j ACCEPT\niptables -w 10 -t raw -A FELIS-CONTROL -s \"$ip/32\" -p tcp -m multiport --dports 443,6443 -j ACCEPT\niptables -w 10 -t raw -A FELIS-CONTROL -s \"$ip/32\" -p udp --dport 53 -j ACCEPT\niptables -w 10 -A FELIS-CONTROL -s \"$ip/32\" -p udp --dport 53 -j ACCEPT\ndone\n"
}
