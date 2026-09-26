package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/offsite"
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

// TestMailHold: mail waits through the installer's quiet window, and on a host
// standing by for another host's off-site bucket while that host writes it.
func TestMailHold(t *testing.T) {
	dir := t.TempDir()
	quiet, status := filepath.Join(dir, "quiet"), filepath.Join(dir, "status.json")
	now := time.Now()
	if got := mailHold(quiet, true, status, now); got != "" {
		t.Errorf("no quiet file, no status: %q", got)
	}
	writeTestFile(t, quiet, fmt.Sprintf("%d\n", now.Add(time.Hour).Unix()), 0o644)
	if got := mailHold(quiet, false, status, now); !strings.Contains(got, "quiet until") {
		t.Errorf("inside the quiet window: %q", got)
	}
	os.Remove(quiet)

	w := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: now.Add(-40 * time.Minute)}
	for _, tc := range []struct {
		what      string
		st        offsite.Status
		offsiteOn bool
		held      bool
	}{
		{"standing by for a live writer", offsite.Status{Standby: true, Writer: w}, true, true},
		{"standing by, [offsite] since removed", offsite.Status{Standby: true, Writer: w}, false, false},
		{"standing by for a writer gone quiet", offsite.Status{Standby: true, Writer: &offsite.Writer{HostID: w.HostID, Host: w.Host, At: now.Add(-offsite.WriterLive - time.Minute)}}, true, false},
		{"standing by, no writer named", offsite.Status{Standby: true}, true, false},
		{"displaced", offsite.Status{Displaced: true, Writer: w}, true, false},
		{"the writer itself", offsite.Status{LastSuccess: now}, true, false},
	} {
		if err := offsite.WriteStatus(status, tc.st); err != nil {
			t.Fatal(err)
		}
		got := mailHold(quiet, tc.offsiteOn, status, now)
		if (got != "") != tc.held || (tc.held && !strings.Contains(got, "stands by for host prod-1 (id bbbbbbbbbbbbbbbb)")) {
			t.Errorf("%s: hold = %q, want held %v", tc.what, got, tc.held)
		}
	}
	writeTestFile(t, status, "{", 0o600)
	if got := mailHold(quiet, true, status, now); got != "" {
		t.Errorf("an unreadable status held the mail: %q", got)
	}
}
