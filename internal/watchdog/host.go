package watchdog

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// addressFor is short: the cluster and the panel are down while the address
	// is gone, so a DHCP renewal that lands on a new lease is an outage.
	addressFor = 5 * time.Minute
	// clockFor leaves room for an NTP daemon that was just enabled (by the
	// installer, or after a reboot) to reach its first synchronization.
	clockFor = 30 * time.Minute
	// certFor is zero: an expiry date does not heal itself while it waits.
	certFor = 0

	// certWarnWithin gives a month to find a moment to restart k3s;
	// certCriticalWithin is the last week.
	certWarnWithin     = 30 * 24 * time.Hour
	certCriticalWithin = 7 * 24 * time.Hour

	// staUnsync is STA_UNSYNC from <linux/timex.h>. The kernel holds it set while
	// no NTP daemon disciplines the clock; chronyd and systemd-timesyncd clear it
	// once they have synchronized. It is what `timedatectl` prints as "System
	// clock synchronized: no".
	staUnsync = 0x0040
)

// AddressFinding reports that this host no longer holds want, the node address
// the installer gave k3s and wrote into the network policies, the panel
// certificate and (by default) the nip.io root domain. held
// is every address on the host's interfaces. An empty or unparsable want skips
// the check.
func AddressFinding(want string, held []net.IP) *Finding {
	ip := net.ParseIP(want)
	if ip == nil {
		return nil
	}
	var now []string
	for _, h := range held {
		if h.Equal(ip) {
			return nil
		}
		if !h.IsLoopback() && !h.IsLinkLocalUnicast() {
			now = append(now, h.String())
		}
	}
	current := strings.Join(now, ", ")
	if current == "" {
		current = "无 / none"
	}
	return &Finding{
		Key: "host-address", Severity: Critical, For: addressFor,
		Summary: fmt.Sprintf("本机已不再持有安装时的地址 %s（现在是：%s）：k3s 节点、网络策略和面板证书仍指向旧地址",
			want, current),
		SummaryEN: fmt.Sprintf("this host no longer holds %s, the address the install was made on (it has: %s): the k3s node, the network policies and the panel certificate still point at it",
			want, current),
		Hint: "give the host its old address back (a DHCP reservation or a static address); docs/troubleshooting.md §13c",
	}
}

// HostAddresses lists the addresses on this host's interfaces.
func HostAddresses() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n.IP)
		}
	}
	return out, nil
}

// ClockFinding reports a clock no NTP daemon has synchronized, from the kernel's
// adjtimex status word. ok is false where the status cannot be read (not Linux),
// which skips the check.
func ClockFinding(status int32, ok bool) *Finding {
	if !ok || status&staUnsync == 0 {
		return nil
	}
	return &Finding{
		Key: "clock", Severity: Warning, For: clockFor,
		Summary:   "系统时钟没有经 NTP 同步：登录验证码与会话的过期、异地备份上传（S3 拒收偏差超过 15 分钟的请求）和证书校验都依赖准确时间",
		SummaryEN: "the system clock is not synchronized by NTP: sign-in code and session expiry, off-site uploads (S3 refuses requests more than 15 minutes off) and certificate checks all depend on it",
		Hint:      "timedatectl; sudo timedatectl set-ntp true (docs/troubleshooting.md §13c)",
	}
}

// K3sCertDirs are where k3s keeps the certificates it issues itself, the set
// `k3s certificate check` reads: the API server's serving and client
// certificates, the component and admin client certificates, etcd's, and the
// agent's (kubelet, kube-proxy). temporary-certs is left out: the API server
// makes its loopback certificate there on each start, for a hundred years.
var K3sCertDirs = []string{
	"/var/lib/rancher/k3s/server/tls",
	"/var/lib/rancher/k3s/server/tls/etcd",
	"/var/lib/rancher/k3s/server/tls/kube-controller-manager",
	"/var/lib/rancher/k3s/server/tls/kube-scheduler",
	"/var/lib/rancher/k3s/agent",
}

// CertFinding reports the certificate in the *.crt files under dirs that
// expires first, once it is within certWarnWithin of expiring (certCriticalWithin,
// or past it: critical). k3s issues its client and serving certificates for a
// year and renews the ones close to expiry only as it starts, so a host that
// runs a year without restarting k3s loses its API server and its kubelet the
// day they lapse. Its CA certificates last ten years and no restart renews
// them. Only the public .crt files are read, never the keys beside them; a
// directory that does not exist (a host without k3s) reports nothing, and a
// file that does not parse is skipped.
func CertFinding(dirs []string, now time.Time) *Finding {
	var soonest *x509.Certificate
	var file string
	for _, dir := range dirs {
		// A directory or file that cannot be read reads as empty.
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".crt") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, _ := os.ReadFile(path)
			// k3s writes a leaf followed by the CA that signed it.
			for {
				var block *pem.Block
				if block, data = pem.Decode(data); block == nil {
					break
				}
				c, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					continue
				}
				if soonest == nil || c.NotAfter.Before(soonest.NotAfter) {
					soonest, file = c, path
				}
			}
		}
	}
	if soonest == nil {
		return nil
	}
	left := soonest.NotAfter.Sub(now)
	if left >= certWarnWithin {
		return nil
	}
	sev := Warning
	if left < certCriticalWithin {
		sev = Critical
	}
	when := soonest.NotAfter.UTC().Format("2006-01-02 15:04 UTC")
	f := &Finding{Key: "k3s-certs", Severity: sev, For: certFor}
	// x509 holds a certificate valid through the instant of NotAfter.
	if left < 0 {
		f.Summary = fmt.Sprintf("k3s 证书 %s（%s）已于 %s 过期：k3s 组件之间无法再认证，集群 API、节点和面板对服务器的管理都会中断",
			file, soonest.Subject.CommonName, when)
		f.SummaryEN = fmt.Sprintf("the k3s certificate %s (%s) expired at %s: the k3s components can no longer authenticate to each other, so the cluster API, the node and the panel's server management stop working",
			file, soonest.Subject.CommonName, when)
	} else {
		days := int(left / (24 * time.Hour))
		f.Summary = fmt.Sprintf("k3s 证书 %s（%s）将于 %s 过期（还剩 %d 天）：过期后 k3s 组件之间无法认证，集群 API、节点和面板对服务器的管理都会中断",
			file, soonest.Subject.CommonName, when, days)
		f.SummaryEN = fmt.Sprintf("the k3s certificate %s (%s) expires at %s (%d days left): once it does, the k3s components can no longer authenticate to each other, so the cluster API, the node and the panel's server management stop working",
			file, soonest.Subject.CommonName, when, days)
	}
	if soonest.IsCA {
		f.Hint = "a k3s CA certificate, which no restart renews: rotate it with `k3s certificate rotate-ca` (docs/troubleshooting.md §13d)"
	} else {
		f.Hint = "sudo systemctl restart k3s: k3s renews its certificates near expiry as it starts, and running game servers keep running; check with sudo k3s certificate check (docs/troubleshooting.md §13d)"
	}
	return f
}
