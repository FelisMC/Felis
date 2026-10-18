// Package software resolves, verifies and installs server software on world volumes for Java runtime images.
package software

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

type Type string

const (
	Paper    Type = "paper"
	Purpur   Type = "purpur"
	Fabric   Type = "fabric"
	Quilt    Type = "quilt"
	Forge    Type = "forge"
	NeoForge Type = "neoforge"
	Vanilla  Type = "vanilla"
	Custom   Type = "custom"
)

var Types = []Type{Paper, Purpur, Fabric, Quilt, Forge, NeoForge, Vanilla, Custom}

func (t Type) Valid() bool {
	for _, v := range Types {
		if t == v {
			return true
		}
	}
	return false
}

func (t Type) HasBuilds() bool {
	switch t {
	case Paper, Purpur, Fabric, Quilt, Forge, NeoForge:
		return true
	}
	return false
}

const (
	ForwardingVelocity = "velocity"
	ForwardingNone     = "none"
)

// Spec mirrors the CRD software selection without a Kubernetes dependency and travels to init containers as JSON.
type Spec struct {
	Type             Type   `json:"type"`
	MinecraftVersion string `json:"minecraftVersion,omitempty"`
	Build            string `json:"build,omitempty"`
	Java             int    `json:"java"`
	Jar              string `json:"jar,omitempty"`
	Forwarding       string `json:"forwarding,omitempty"`
}

func (s Spec) Key() string {
	return strings.Join([]string{string(s.Type), s.MinecraftVersion, s.Build, s.Jar, s.Forwarding}, "|")
}

var Runtimes = []int{8, 17, 21, 25}

func RuntimeFor(major int) (int, error) {
	if major <= 0 {
		major = 8
	}
	for _, r := range Runtimes {
		if r >= major {
			return r, nil
		}
	}
	return 0, fmt.Errorf("software: Java %d is newer than any runtime the platform ships (%v)", major, Runtimes)
}

func ValidRuntime(java int) bool {
	for _, r := range Runtimes {
		if r == java {
			return true
		}
	}
	return false
}

var ErrInvalid = errors.New("invalid software")

// Validate checks local shape only; the resolver separately checks whether upstream versions and builds exist.
func (s Spec) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	if !s.Type.Valid() {
		return bad("unknown type %q", s.Type)
	}
	if !ValidRuntime(s.Java) {
		return bad("java must be one of %v", Runtimes)
	}
	switch s.Forwarding {
	case "", ForwardingNone, ForwardingVelocity:
	default:
		return bad("unknown forwarding %q", s.Forwarding)
	}
	if s.Type == Custom {
		if s.MinecraftVersion != "" || s.Build != "" {
			return bad("a custom jar takes no version or build")
		}
		if err := ValidateJarPath(s.Jar); err != nil {
			return bad("%v", err)
		}
		if s.Forwarding == ForwardingVelocity {
			return bad("a custom jar cannot take managed forwarding")
		}
		return nil
	}
	if s.Jar != "" {
		return bad("only a custom server names a jar")
	}
	if !versionPattern(s.MinecraftVersion) {
		return bad("invalid minecraft version %q", s.MinecraftVersion)
	}
	if s.Type.HasBuilds() {
		if !versionPattern(s.Build) {
			return bad("invalid build %q", s.Build)
		}
	} else if s.Build != "" {
		return bad("%s takes no build", s.Type)
	}
	return nil
}

func ValidateJarPath(p string) error {
	if p == "" {
		return errors.New("a custom server needs the path of its jar")
	}
	if len(p) > 255 {
		return errors.New("the jar path is too long")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return errors.New("the jar path must be relative to the server directory")
	}
	clean := path.Clean(p)
	if clean != p || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return errors.New("the jar path must stay inside the server directory")
	}
	if strings.HasPrefix(clean, ".felis/") {
		return errors.New("the jar path cannot be inside .felis, which the platform manages")
	}
	if !strings.HasSuffix(strings.ToLower(clean), ".jar") {
		return errors.New("the jar path must end in .jar")
	}
	for _, r := range clean {
		if r < 0x20 || r == 0x7f {
			return errors.New("the jar path contains a control character")
		}
	}
	return nil
}

// Version strings are used in URLs and paths, so validate their charset and length before either use.
func versionPattern(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for i, r := range v {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r == '.' || r == '-' || r == '+' || r == '_'):
		default:
			return false
		}
	}
	return true
}

type mcVersion struct {
	parts []int
	pre   bool
}

// parseMC rejects non-release-shaped snapshot IDs such as 24w14a.
func parseMC(v string) (mcVersion, bool) {
	base, suffix, _ := strings.Cut(v, "-")
	fields := strings.Split(base, ".")
	if len(fields) < 2 || len(fields) > 4 {
		return mcVersion{}, false
	}
	out := mcVersion{pre: suffix != ""}
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return mcVersion{}, false
		}
		out.parts = append(out.parts, n)
	}
	return out, true
}

// cmp treats missing numeric components as zero and ignores prerelease state.
func (a mcVersion) cmp(b mcVersion) int {
	for i := 0; i < len(a.parts) || i < len(b.parts); i++ {
		var x, y int
		if i < len(a.parts) {
			x = a.parts[i]
		}
		if i < len(b.parts) {
			y = b.parts[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func AtLeast(v, min string) bool {
	a, ok := parseMC(v)
	if !ok {
		return false
	}
	b, ok := parseMC(min)
	if !ok {
		return false
	}
	return a.cmp(b) >= 0
}

func Between(v, lo, hi string) bool {
	return AtLeast(v, lo) && AtLeast(hi, v)
}

func stableVersion(v string) bool {
	m, ok := parseMC(v)
	return ok && !m.pre
}
