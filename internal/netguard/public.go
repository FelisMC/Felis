package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

var ErrNotPublic = errors.New("not a public upstream address")

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

// RefusePrivate checks the actual dial address of public downloads.
func RefusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("refusing to dial %s: %w", host, ErrNotPublic)
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.Zone() != "" || (ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip)) {
		return fmt.Errorf("refusing to dial %s: %w", host, ErrNotPublic)
	}
	for _, prefix := range nonPublic {
		if prefix.Contains(ip) {
			return fmt.Errorf("refusing to dial %s: %w", host, ErrNotPublic)
		}
	}
	return nil
}
