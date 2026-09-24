// Package updates is the pure decision core of Felis's component self-update
// subsystem (a user-directed capability over spec V4.1: Felis itself, k3s, and the
// off-cluster components should be kept current — while Minecraft servers are left
// pinned, "能不动的就别动"). It answers one question deterministically: given each
// tracked component's current version, the latest version discovered upstream, its
// update policy, and the current time, WHAT should happen — nothing, report it as
// pinned, notify a SysAdmin, or apply an update inside a human-set maintenance
// window.
//
// The package is deliberately pure: it performs no I/O. Discovering the latest
// version (GitHub Releases / PaperMC), notifying operators (SMTP / in-game), and
// applying an update (control-plane image bump, k3s upgrade, cloudflared swap) are
// integration seams that live with the caller (see seams.go). Keeping the DECISION
// here — with its load-bearing safety invariants (a pinned component NEVER changes,
// a downgrade is NEVER proposed, a prerelease is NEVER auto-applied, and an apply
// happens ONLY inside the window a SysAdmin explicitly set) — makes those invariants
// unit-testable without a cluster, a mailbox, or the network, mirroring how
// internal/cfsetup splits its pure core from its ExecRunner.
package updates

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a tolerant semantic version. It is tolerant on purpose: the versions
// Felis compares do not come from one clean source. k3s stamps a build suffix
// ("v1.30.2+k3s1"), releases are commonly tagged with a leading "v", cloudflared
// ships calendar versions ("2024.2.1"), and prereleases carry a "-rc.1" tail. Parse
// accepts all of these and Compare orders them by the SemVer 2.0.0 precedence rules
// (build metadata after "+" is ignored for ordering; a prerelease sorts BEFORE its
// corresponding release).
type Version struct {
	Major int
	Minor int
	Patch int
	// Revision is an optional fourth numeric component. Temurin numbers an emergency
	// respin of a JDK update that way ("25.0.4.1+1"), and it orders after Patch.
	Revision int
	// Prerelease is the dot-separated identifier set after "-" (empty for a normal
	// release). Its presence is what IsPrerelease reports and what makes this version
	// sort below the same Major.Minor.Patch without a prerelease.
	Prerelease string
	// raw preserves the original string so String() round-trips what upstream
	// actually published (e.g. the "+k3s1" a human needs to see in a report).
	raw string
}

// Parse reads a tolerant semantic version. It accepts an optional leading "v",
// fills missing minor/patch with 0 (so "v2" and "2.0" parse), takes an optional
// fourth numeric component (the JDK's "25.0.4.1"), strips build
// metadata after "+" for ordering while preserving it in the raw string, and keeps
// any "-prerelease" tail. It fails closed: an unparseable core (non-numeric
// major/minor/patch) returns an error rather than a zero Version, so a garbled feed
// can never masquerade as version 0.0.0 and trigger a spurious "upgrade".
func Parse(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Version{}, fmt.Errorf("updates: empty version string")
	}
	v := Version{raw: raw}

	core := strings.TrimPrefix(raw, "v")
	core = strings.TrimPrefix(core, "V")

	// Split off build metadata ("+k3s1"): ignored for precedence per SemVer §10.
	if i := strings.IndexByte(core, '+'); i >= 0 {
		core = core[:i]
	}
	// Split off the prerelease tail ("-rc.1", "-SNAPSHOT").
	if i := strings.IndexByte(core, '-'); i >= 0 {
		v.Prerelease = core[i+1:]
		core = core[:i]
	}

	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return Version{}, fmt.Errorf("updates: %q is not a dotted version", raw)
	}
	nums := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return Version{}, fmt.Errorf("updates: %q has a non-numeric component %q", raw, p)
		}
		if n < 0 {
			return Version{}, fmt.Errorf("updates: %q has a negative component %q", raw, p)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch, v.Revision = nums[0], nums[1], nums[2], nums[3]
	return v, nil
}

// IsPrerelease reports whether the version carries a prerelease tail. Auto-apply is
// gated on this being false: Felis tracks stable releases and never bumps a live
// component onto an rc/beta/SNAPSHOT on its own.
func (v Version) IsPrerelease() bool { return v.Prerelease != "" }

// String returns the original published string when known (so "+k3s1" survives into
// a report), falling back to the reconstructed core.
func (v Version) String() string {
	if v.raw != "" {
		return v.raw
	}
	base := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Revision != 0 {
		base += fmt.Sprintf(".%d", v.Revision)
	}
	if v.Prerelease != "" {
		return base + "-" + v.Prerelease
	}
	return base
}

// Compare returns -1, 0, or +1 as v sorts before, equal to, or after o, by SemVer
// 2.0.0 precedence: numeric Major.Minor.Patch(.Revision) first, then — for an equal core — a
// version WITH a prerelease sorts below one without, and two prereleases compare by
// their dot-separated identifiers (numeric identifiers numerically, others
// lexically; a numeric identifier always sorts below an alphanumeric one). Build
// metadata is not consulted.
func (v Version) Compare(o Version) int {
	if c := cmpInt(v.Major, o.Major); c != 0 {
		return c
	}
	if c := cmpInt(v.Minor, o.Minor); c != 0 {
		return c
	}
	if c := cmpInt(v.Patch, o.Patch); c != 0 {
		return c
	}
	if c := cmpInt(v.Revision, o.Revision); c != 0 {
		return c
	}
	return comparePrerelease(v.Prerelease, o.Prerelease)
}

// After reports whether v is strictly newer than o. It is the single predicate the
// plan engine uses to decide there is anything to do, so "no downgrade is ever
// proposed" reduces to "we only act when After is true".
func (v Version) After(o Version) bool { return v.Compare(o) > 0 }

// comparePrerelease implements SemVer §11.4: an empty prerelease (a release) has
// HIGHER precedence than any non-empty one.
func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1 // release > prerelease
	}
	if b == "" {
		return -1 // prerelease < release
	}
	ai, bi := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(ai) && i < len(bi); i++ {
		if c := comparePrereleaseIdent(ai[i], bi[i]); c != 0 {
			return c
		}
	}
	// All shared identifiers equal: the longer set has higher precedence (§11.4.4).
	return cmpInt(len(ai), len(bi))
}

// comparePrereleaseIdent compares two prerelease identifiers: both numeric ⇒
// numeric compare; a numeric identifier sorts BELOW an alphanumeric one; otherwise
// ASCII lexical.
func comparePrereleaseIdent(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return cmpInt(an, bn)
	case aerr == nil: // a numeric, b not ⇒ a lower
		return -1
	case berr == nil: // b numeric, a not ⇒ a higher
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
