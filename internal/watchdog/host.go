package watchdog

import (
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	// addressFor is short: nothing reaches the database or the panel while the
	// address is gone, so a DHCP renewal that lands on a new lease is an outage.
	addressFor = 5 * time.Minute
	// clockFor leaves room for an NTP daemon that was just enabled (by the
	// installer, or after a reboot) to reach its first synchronization.
	clockFor = 30 * time.Minute

	// staUnsync is STA_UNSYNC from <linux/timex.h>. The kernel holds it set while
	// no NTP daemon disciplines the clock; chronyd and systemd-timesyncd clear it
	// once they have synchronized. It is what `timedatectl` prints as "System
	// clock synchronized: no".
	staUnsync = 0x0040
)

// AddressFinding reports that this host no longer holds want, the node address
// the installer wrote into the database connection string, pg_hba, the network
// policies, the panel certificate and (by default) the nip.io root domain. held
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
		Summary: fmt.Sprintf("本机已不再持有安装时的地址 %s（现在是：%s）：数据库连接、pg_hba、网络策略和面板证书仍指向旧地址",
			want, current),
		SummaryEN: fmt.Sprintf("this host no longer holds %s, the address the install was made on (it has: %s): the database connection, pg_hba, the network policies and the panel certificate still point at it",
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
