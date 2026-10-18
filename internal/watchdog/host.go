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
	addressFor = 5 * time.Minute
	// Allow newly enabled NTP daemons time to synchronize.
	clockFor = 30 * time.Minute
	certFor  = 0

	certWarnWithin     = 30 * 24 * time.Hour
	certCriticalWithin = 7 * 24 * time.Hour

	// staUnsync is STA_UNSYNC from <linux/timex.h>, cleared once NTP synchronizes.
	staUnsync = 0x0040
)

// AddressFinding skips empty or invalid expected addresses.
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
		Hint: "give the host its old address back (a DHCP reservation or a static address); https://docs.felismc.com/en/operations/troubleshooting#host-address-changed",
	}
}

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

// ClockFinding skips checks when kernel clock status is unavailable.
func ClockFinding(status int32, ok bool) *Finding {
	if !ok || status&staUnsync == 0 {
		return nil
	}
	return &Finding{
		Key: "clock", Severity: Warning, For: clockFor,
		Summary:   "系统时钟没有经 NTP 同步：登录验证码与会话的过期、异地备份上传（S3 拒收偏差超过 15 分钟的请求）和证书校验都依赖准确时间",
		SummaryEN: "the system clock is not synchronized by NTP: sign-in code and session expiry, off-site uploads (S3 refuses requests more than 15 minutes off) and certificate checks all depend on it",
		Hint:      "timedatectl; sudo timedatectl set-ntp true (https://docs.felismc.com/en/operations/troubleshooting#clock)",
	}
}

// K3sCertDirs excludes temporary loopback certificates regenerated on startup.
var K3sCertDirs = []string{
	"/var/lib/rancher/k3s/server/tls",
	"/var/lib/rancher/k3s/server/tls/etcd",
	"/var/lib/rancher/k3s/server/tls/kube-controller-manager",
	"/var/lib/rancher/k3s/server/tls/kube-scheduler",
	"/var/lib/rancher/k3s/agent",
}

// CertFinding reports the earliest expiry, skipping absent directories and unreadable or invalid certificates.
func CertFinding(dirs []string, now time.Time) *Finding {
	var soonest *x509.Certificate
	var file string
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".crt") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, _ := os.ReadFile(path)
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
	// Certificates remain valid through NotAfter.
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
		f.Hint = "a k3s CA certificate, which no restart renews: rotate it with `k3s certificate rotate-ca` (https://docs.felismc.com/en/operations/troubleshooting#k3s-certificates)"
	} else {
		f.Hint = "sudo systemctl restart k3s: k3s renews its certificates near expiry as it starts, and running game servers keep running; check with sudo k3s certificate check (https://docs.felismc.com/en/operations/troubleshooting#k3s-certificates)"
	}
	return f
}
