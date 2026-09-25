package watchdog

import (
	"net"
	"strings"
	"testing"
)

func TestAddressFinding(t *testing.T) {
	loop := net.ParseIP("127.0.0.1")
	if f := AddressFinding("10.211.55.6", []net.IP{loop, net.IPv4(10, 211, 55, 6)}); f != nil {
		t.Fatalf("still held: got %+v", f)
	}
	// The 4-byte form an interface can report is the same address.
	if f := AddressFinding("10.211.55.6", []net.IP{{10, 211, 55, 6}}); f != nil {
		t.Fatalf("4-byte form: got %+v", f)
	}
	if f := AddressFinding("", []net.IP{loop}); f != nil {
		t.Fatalf("no recorded address: got %+v", f)
	}

	f := AddressFinding("10.211.55.6", []net.IP{loop, net.ParseIP("10.211.55.9"), net.ParseIP("fe80::1c1a:2bff:fe3c:4d5e")})
	if f == nil {
		t.Fatal("moved address: no finding")
	}
	if f.Key != "host-address" || f.Severity != Critical {
		t.Fatalf("moved address: key %q severity %v", f.Key, f.Severity)
	}
	if !strings.Contains(f.SummaryEN, "no longer holds 10.211.55.6") || !strings.Contains(f.SummaryEN, "(it has: 10.211.55.9)") {
		t.Fatalf("moved address: summary %q", f.SummaryEN)
	}

	f = AddressFinding("10.211.55.6", []net.IP{loop})
	if f == nil || !strings.Contains(f.Summary, "（现在是：无 / none）") {
		t.Fatalf("no address at all: got %+v", f)
	}
}

func TestClockFinding(t *testing.T) {
	// Status words as <linux/timex.h> spells them: STA_PLL 0x0001, STA_UNSYNC
	// 0x0040, STA_NANO 0x2000. An unsynced kernel reports 0x0041 with a daemon
	// that has not locked yet, 0x0040 with none; chronyd synced leaves 0x2001.
	if f := ClockFinding(0x2001, true); f != nil {
		t.Fatalf("synchronized: got %+v", f)
	}
	for _, st := range []int32{0x0040, 0x0041} {
		f := ClockFinding(st, true)
		if f == nil || f.Key != "clock" || f.Severity != Warning {
			t.Fatalf("status %#x: got %+v", st, f)
		}
	}
	if f := ClockFinding(0x0040, false); f != nil {
		t.Fatalf("unreadable status: got %+v", f)
	}
}
