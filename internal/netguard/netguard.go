// Package netguard holds the dial checks for connections an Owner's settings aim: an
// upstream Yggdrasil root, a mail relay. felis-api makes them from inside the cluster, so
// whatever address a setting names is dialled with the pod's reach.
package netguard

import (
	"fmt"
	"net"
	"syscall"
)

// RefuseLinkLocal is a net.Dialer Control that keeps a connection off link-local,
// unspecified and multicast addresses. A cloud host serves its metadata, with the
// instance's credentials, at 169.254.169.254; no relay or auth root lives there, so a
// setting pointing there is only a way to read them through felis-api. It checks the
// address actually dialled, after DNS, so a name that resolves there is refused too.
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
