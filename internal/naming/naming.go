// Package naming enforces the portability and admission rules (spec §2, §22):
// a server name matches ^[a-z0-9-]{3,32}$ and is non-reserved, and every
// hostname must be a single label under the configured root_domain. The root
// domain is never hardcoded — it is always supplied by config — so this package
// stays free of any deployment-specific domain.
package naming

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	serverNameRE = regexp.MustCompile(`^[a-z0-9-]{3,32}$`)
	dnsLabelRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// reserved subdomains/server names that users may not claim: proxy/lobby and
// the platform's own faces.
var reserved = map[string]struct{}{
	"lobby":    {},
	"admin":    {},
	"panel":    {},
	"api":      {},
	"felis":    {},
	"velocity": {},
	"registry": {},
	"internal": {},
	"www":      {},
}

// ValidateServerName checks the §22 name rule and reservation list.
func ValidateServerName(name string) error {
	if !serverNameRE.MatchString(name) {
		return fmt.Errorf("naming: invalid server name %q: must match ^[a-z0-9-]{3,32}$", name)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("naming: server name %q must not start or end with '-'", name)
	}
	if _, ok := reserved[name]; ok {
		return fmt.Errorf("naming: server name %q is reserved", name)
	}
	return nil
}

// IsReserved reports whether label is on the reserved list.
func IsReserved(label string) bool {
	_, ok := reserved[label]
	return ok
}

// worldVolumeName mirrors operator.dataVolumeName: the per-server StatefulSet's
// volumeClaimTemplate is named "world", so a single-replica server's world PVC
// is "world-<name>-0". This is the one naming convention shared by the operator
// (which creates the PVC), the reaper (which deletes it), and restore (which
// mounts it), so it lives here rather than being duplicated per subsystem.
const worldVolumeName = "world"

// WorldPVCName returns the world PersistentVolumeClaim name for a server,
// matching the operator's StatefulSet volumeClaimTemplate naming
// ("world-<name>-0" for the sole replica).
func WorldPVCName(server string) string {
	return worldVolumeName + "-" + server + "-0"
}

// Hostname composes subdomain.rootDomain after validating the subdomain.
func Hostname(subdomain, rootDomain string) (string, error) {
	if err := ValidateServerName(subdomain); err != nil {
		return "", err
	}
	if rootDomain == "" {
		return "", fmt.Errorf("naming: root domain is empty")
	}
	return subdomain + "." + rootDomain, nil
}

// ValidateHostname enforces the §2 invariant that host is a single label
// directly under rootDomain.
func ValidateHostname(host, rootDomain string) error {
	if rootDomain == "" {
		return fmt.Errorf("naming: root domain is empty")
	}
	suffix := "." + rootDomain
	if !strings.HasSuffix(host, suffix) {
		return fmt.Errorf("naming: hostname %q must be under %q", host, rootDomain)
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return fmt.Errorf("naming: hostname %q must be a single label under %q", host, rootDomain)
	}
	if !dnsLabelRE.MatchString(label) {
		return fmt.Errorf("naming: invalid hostname label %q", label)
	}
	return nil
}
