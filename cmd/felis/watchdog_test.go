package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

// TestProxyFinding: a listening proxy is healthy; a closed port is the critical
// "players cannot reach any server" finding.
func TestProxyFinding(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if f := proxyFinding(context.Background(), addr); f != nil {
		t.Fatalf("listening proxy reported: %+v", f)
	}
	ln.Close()
	f := proxyFinding(context.Background(), addr)
	if f == nil || f.Key != "proxy" || !strings.Contains(f.SummaryEN, addr) {
		t.Fatalf("closed proxy = %+v, want the proxy finding", f)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" /, /var/lib/felis ,,")
	if strings.Join(got, "|") != "/|/var/lib/felis" {
		t.Fatalf("splitList = %q", got)
	}
}
