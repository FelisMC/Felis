// Package updates plans component updates without I/O; execution and discovery are supplied by the caller.
package updates

import (
	"fmt"
	"strconv"
	"strings"
)

// Version compares semantic-version precedence while tolerating v prefixes, calendar versions and build metadata.
type Version struct {
	Major int
	Minor int
	Patch int
	// Revision is a fourth numeric component for JDK respins, ordered after Patch.
	Revision   int
	Prerelease string
	raw        string
}

// Parse defaults omitted minor or patch values to zero, accepts a fourth revision and preserves raw metadata without ordering by it.
func Parse(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Version{}, fmt.Errorf("updates: empty version string")
	}
	v := Version{raw: raw}

	core := strings.TrimPrefix(raw, "v")
	core = strings.TrimPrefix(core, "V")

	if i := strings.IndexByte(core, '+'); i >= 0 {
		core = core[:i]
	}
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

func (v Version) IsPrerelease() bool { return v.Prerelease != "" }

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

// Compare orders core and prerelease identifiers numerically or lexically as required by SemVer, ignoring build metadata.
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

func (v Version) After(o Version) bool { return v.Compare(o) > 0 }

func comparePrerelease(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	ai, bi := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(ai) && i < len(bi); i++ {
		if c := comparePrereleaseIdent(ai[i], bi[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(ai), len(bi))
}

func comparePrereleaseIdent(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return cmpInt(an, bn)
	case aerr == nil:
		return -1
	case berr == nil:
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
