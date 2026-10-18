// Package netguard restricts outbound connections configured through owner settings.
package netguard

import (
	"fmt"
	"net"
	"syscall"
)

// RefuseLinkLocal checks the resolved dial address to block link-local, unspecified, and multicast destinations.
func RefuseLinkLocal(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("refusing to dial %s: not a routable upstream address", host)
	}
	return nil
}
