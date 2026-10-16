// Package software resolves the server software a Minecraft server runs when it
// is built from the platform's runtime image instead of a whitelisted server
// image. The runtime image carries only a Java runtime; the server itself (Paper,
// Fabric, Forge, a vanilla jar, or a jar the owner uploaded) lives on the
// server's world volume, installed there by `felis install-server` before every
// start and launched by `felis launch`.
//
// Everything that is downloaded is named by an upstream metadata source and
// checked against the digest that source publishes, before it is used: Mojang's
// version manifests (sha1), PaperMC Fill (sha256), Purpur (md5), the Fabric,
// Quilt, Forge and NeoForge Maven repositories (the .sha256 or .sha1 files they
// serve beside every artifact), and Modrinth (sha512) for the identity
// forwarding mods. The cluster's artifact cache (internal/artifactcache) holds
// one copy per digest, so the same version is fetched from upstream once.
package software

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Type names a kind of server software.
type Type string

// The supported software types.
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

// Types lists every type in the order the panel offers them.
var Types = []Type{Paper, Purpur, Fabric, Quilt, Forge, NeoForge, Vanilla, Custom}

// Valid reports whether t is a supported type.
func (t Type) Valid() bool {
	for _, v := range Types {
		if t == v {
			return true
		}
	}
	return false
}

// HasBuilds reports whether a type picks a build or loader version on top of the
// Minecraft version.
func (t Type) HasBuilds() bool {
	switch t {
	case Paper, Purpur, Fabric, Quilt, Forge, NeoForge:
		return true
	}
	return false
}

// Forwarding modes: how the proxy's verified player identity reaches the server.
const (
	// ForwardingVelocity means the server takes Velocity modern forwarding: the
	// Paper family through its own config, the modded loaders through a
	// forwarding mod the installer adds.
	ForwardingVelocity = "velocity"
	// ForwardingNone means the server sees players under offline-mode UUIDs: a
	// vanilla server, a custom jar, or a version no forwarding mod covers.
	ForwardingNone = "none"
)

// Spec is the software a server runs. It mirrors v1alpha1.SoftwareSpec, kept
// separate so this package has no Kubernetes dependency and the init containers
// can read it from one JSON environment variable.
type Spec struct {
	Type             Type   `json:"type"`
	MinecraftVersion string `json:"minecraftVersion,omitempty"`
	// Build is the Paper/Purpur build or the Fabric/Quilt/Forge/NeoForge loader
	// version.
	Build string `json:"build,omitempty"`
	// Java is the major version of the runtime the server starts on.
	Java int `json:"java"`
	// Jar is a custom server's jar, relative to the data directory.
	Jar string `json:"jar,omitempty"`
	// Forwarding is ForwardingVelocity or ForwardingNone, decided when the
	// software is chosen (see ForwardingFor).
	Forwarding string `json:"forwarding,omitempty"`
}

// Key identifies what an installation was made for. A plan cached on the volume
// is reused only while the key stays the same.
func (s Spec) Key() string {
	return strings.Join([]string{string(s.Type), s.MinecraftVersion, s.Build, s.Jar, s.Forwarding}, "|")
}

// Runtimes are the Java major versions the platform ships a runtime image for.
var Runtimes = []int{8, 17, 21, 25}

// RuntimeFor returns the smallest shipped runtime that runs software requiring
// major, or an error when none is new enough.
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

// ValidRuntime reports whether java is one of Runtimes.
func ValidRuntime(java int) bool {
	for _, r := range Runtimes {
		if r == java {
			return true
		}
	}
	return false
}

// ErrInvalid wraps every Spec validation failure.
var ErrInvalid = errors.New("invalid software")

// Validate checks the fields a Spec must carry for its type. It does not reach
// upstream: whether the version and build exist is the resolver's question.
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

// ValidateJarPath accepts a relative path inside the data directory ending in
// .jar. The launcher runs it from the data directory, so an absolute path or one
// climbing out with ".." would name a file outside the server's volume.
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

// versionPattern accepts the version and build strings upstream publishes
// ("1.21.4", "26.3-rc-3", "21.4.150-beta", "0.16.10", "1.12.2-14.23.5.2864",
// "2416"): letters, digits and . - + _, starting with an alphanumeric, bounded in
// length. They end up in URLs and file paths, so anything else is refused before
// it reaches either.
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

// mcVersion is a Minecraft release version split into its numeric parts:
// "1.20.1" is [1 20 1], "26.3" is [26 3]. Pre-releases and release candidates
// ("1.21.11-rc3", "26.3-rc-3") parse to their release's numbers with pre set.
type mcVersion struct {
	parts []int
	pre   bool
}

// parseMC parses a release-shaped version. Snapshot ids ("26.4-snapshot-4" is
// release-shaped and parses; "24w14a" does not) report ok=false.
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

// cmp orders two versions by their numeric parts; a missing part is 0, so
// "1.21" equals "1.21.0". Pre-release state is ignored.
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

// AtLeast reports whether release-shaped version v is at least min. A version
// that does not parse is never at least anything.
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

// Between reports lo <= v <= hi for release-shaped versions.
func Between(v, lo, hi string) bool {
	return AtLeast(v, lo) && AtLeast(hi, v)
}

// stableVersion reports whether a version id is a plain release ("1.21.4",
// "26.3"), not a pre-release, candidate or snapshot.
func stableVersion(v string) bool {
	m, ok := parseMC(v)
	return ok && !m.pre
}
