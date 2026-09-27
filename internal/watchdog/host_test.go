package watchdog

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// certPEM makes a self-signed certificate that expires at notAfter. Only the
// dates, the name and the CA bit matter to CertFinding.
func certPEM(t *testing.T, cn string, notAfter time.Time, ca bool) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:              notAfter,
		IsCA:                  ca,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeCert(t *testing.T, path string, pems ...[]byte) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Join(pems, nil), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCertFinding(t *testing.T) {
	now := time.Date(2027, 8, 1, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	ca := certPEM(t, "k3s-client-ca@1790445013", now.Add(3650*day), true)

	// A fresh install: leaves for a year, the CA for ten. Nothing to say, and
	// a missing directory (a host without k3s) is no finding either.
	fresh := t.TempDir()
	writeCert(t, filepath.Join(fresh, "client-admin.crt"), certPEM(t, "system:admin", now.Add(300*day), false), ca)
	if f := CertFinding([]string{fresh, filepath.Join(fresh, "missing")}, now); f != nil {
		t.Fatalf("fresh certificates: got %+v", f)
	}
	if f := CertFinding(nil, now); f != nil {
		t.Fatalf("no directories: got %+v", f)
	}

	// The soonest certificate wins, across directories and within a chained
	// file; a key file, garbage and a directory named like a certificate are
	// passed over.
	server, agent := t.TempDir(), t.TempDir()
	writeCert(t, filepath.Join(server, "serving-kube-apiserver.crt"), certPEM(t, "kube-apiserver", now.Add(25*day), false), ca)
	writeCert(t, filepath.Join(agent, "client-kubelet.crt"), ca, certPEM(t, "system:node:felis", now.Add(12*day+time.Hour), false))
	writeCert(t, filepath.Join(agent, "client-kubelet.key"), certPEM(t, "in a key file", now.Add(time.Hour), false))
	writeCert(t, filepath.Join(agent, "garbage.crt"), []byte("-----BEGIN CERTIFICATE-----\nbm90IGEgY2VydA==\n-----END CERTIFICATE-----\n"))
	if err := os.Mkdir(filepath.Join(agent, "old.crt"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := CertFinding([]string{server, agent}, now)
	if f == nil {
		t.Fatal("certificate 12 days from expiry: no finding")
	}
	if f.Key != "k3s-certs" || f.Severity != Warning || f.For != 0 {
		t.Fatalf("12 days: key %q severity %v for %v", f.Key, f.Severity, f.For)
	}
	want := "the k3s certificate " + filepath.Join(agent, "client-kubelet.crt") + " (system:node:felis) expires at 2027-08-13 13:00 UTC (12 days left)"
	if !strings.HasPrefix(f.SummaryEN, want+": ") {
		t.Fatalf("12 days: summary %q, want it to start %q", f.SummaryEN, want)
	}
	if !strings.Contains(f.Summary, "（还剩 12 天）") {
		t.Fatalf("12 days: 中文摘要 %q", f.Summary)
	}
	if !strings.HasPrefix(f.Hint, "sudo systemctl restart k3s") {
		t.Fatalf("12 days: hint %q", f.Hint)
	}

	// The thresholds: a month out is still quiet, the last week is critical,
	// and a lapsed certificate says so.
	edge := t.TempDir()
	for _, tc := range []struct {
		left time.Duration
		sev  Severity
	}{
		{30 * day, ""},
		{30*day - time.Second, Warning},
		{7 * day, Warning},
		{7*day - time.Second, Critical},
		{-time.Hour, Critical},
	} {
		writeCert(t, filepath.Join(edge, "client-scheduler.crt"), certPEM(t, "system:kube-scheduler", now.Add(tc.left), false))
		f := CertFinding([]string{edge}, now)
		var got Severity
		if f != nil {
			got = f.Severity
		}
		if got != tc.sev {
			t.Fatalf("%v left: severity %q, want %q", tc.left, got, tc.sev)
		}
	}
	f = CertFinding([]string{edge}, now)
	if !strings.Contains(f.SummaryEN, "(system:kube-scheduler) expired at 2027-08-01 11:00 UTC: ") || strings.Contains(f.SummaryEN, "days left") {
		t.Fatalf("expired: summary %q", f.SummaryEN)
	}
	if !strings.Contains(f.Summary, "已于 2027-08-01 11:00 UTC 过期") {
		t.Fatalf("expired: 中文摘要 %q", f.Summary)
	}

	writeCert(t, filepath.Join(edge, "client-scheduler.crt"), certPEM(t, "system:kube-scheduler", now, false))
	if f := CertFinding([]string{edge}, now); f == nil || !strings.Contains(f.SummaryEN, "expires at 2027-08-01 12:00 UTC (0 days left)") {
		t.Fatalf("expiring this instant: got %+v", f)
	}

	// A CA near its end needs a rotation, which a restart does not do.
	old := t.TempDir()
	writeCert(t, filepath.Join(old, "server-ca.crt"), certPEM(t, "k3s-server-ca@1", now.Add(20*day), true))
	f = CertFinding([]string{old}, now)
	if f == nil || !strings.Contains(f.Hint, "k3s certificate rotate-ca") || strings.Contains(f.Hint, "systemctl restart") {
		t.Fatalf("CA near expiry: got %+v", f)
	}
}
