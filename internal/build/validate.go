package build

import (
	"fmt"
	"regexp"
	"strings"

	"felis.lolicon.best/internal/registrygate"
)

// invalidf builds a validation error wrapping ErrInvalid so the API layer maps
// every malformed-request case to a single 400 path.
func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

// imageNameRE matches the path+tag of an image reference under the registry
// host, e.g. "foo/bar:1.0" or "mc-paper:latest". It is intentionally strict:
// lowercase path segments, an optional tag of the same alphabet, no digests, no
// shell metacharacters that could escape into the Kaniko/Trivy argv.
var imageNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?(:[a-zA-Z0-9._-]+)?$`)

// Validate enforces the §16 admission rules on a build request: the target must
// address the internal registry, the Dockerfile must be present and within the
// size cap, and a context reference is required (Kaniko pulls it, spec §17).
func Validate(req Request, cfg Config) error {
	cfg = cfg.withDefaults()
	if err := validateRegistryTarget(req.ImageRef, cfg.RegistryURL); err != nil {
		return err
	}
	if strings.TrimSpace(req.Dockerfile) == "" {
		return invalidf("dockerfile is required")
	}
	if len(req.Dockerfile) > cfg.MaxDockerfileBytes {
		return invalidf("dockerfile exceeds %d bytes", cfg.MaxDockerfileBytes)
	}
	if strings.TrimSpace(req.ContextRef) == "" {
		return invalidf("context reference is required")
	}
	return nil
}

// ValidateImageRef checks a bare image reference (used by external admission,
// where there is no registry-target constraint beyond well-formedness). A
// whitelist entry may be a tag wildcard ("registry/foo:*", a legitimate
// image_whitelist value per spec §15): the trailing ":*" is stripped before the
// path is validated. This is safe because a wildcard is only ever compared
// against concrete refs by imageMatches — it never reaches a Kaniko/Trivy argv,
// unlike a build push target (validateRegistryTarget stays strictly concrete).
func ValidateImageRef(ref string) error {
	if ref == "" {
		return invalidf("image reference is required")
	}
	host, rest, ok := splitRegistryHost(ref)
	if !ok {
		return invalidf("image reference %q must be host-qualified (host/path:tag)", ref)
	}
	if repo, isWildcard := strings.CutSuffix(rest, ":*"); isWildcard {
		rest = repo
	}
	if !imageNameRE.MatchString(rest) {
		return invalidf("invalid image path/tag %q", rest)
	}
	_ = host
	return nil
}

// splitTag splits an image reference's path from its tag. It is registry-port
// safe: only a colon *after* the final path separator is a tag separator, so
// "registry:5000/foo" splits to ("registry:5000/foo", ""), never a bogus tag.
func splitTag(ref string) (repo, tag string) {
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

// imageMatches reports whether a concrete image ref is admitted by a whitelist
// pattern. A pattern is either exact ("registry/foo:1.0") or a tag wildcard
// ("registry/foo:*", spec §15) that matches any non-empty tag on the same repo.
// A wildcard never matches an untagged ref — admission is always to a concrete
// tag.
func imageMatches(ref, pattern string) bool {
	if ref == pattern {
		return true
	}
	prefix, ok := strings.CutSuffix(pattern, ":*")
	if !ok {
		return false
	}
	repo, tag := splitTag(ref)
	return repo == prefix && tag != ""
}

// validateRegistryTarget enforces that a build pushes only to the configured
// internal registry — never an arbitrary external host (spec §16: the build can
// never push elsewhere; the registry is not a public ingress). When RegistryURL
// is unset (tests / not configured) the host constraint is skipped but the
// path/tag are still validated.
func validateRegistryTarget(ref, registryURL string) error {
	if ref == "" {
		return invalidf("image reference is required")
	}
	host, rest, ok := splitRegistryHost(ref)
	if !ok {
		return invalidf("image reference %q must target the internal registry (host/path:tag)", ref)
	}
	if !imageNameRE.MatchString(rest) {
		return invalidf("invalid image path/tag %q", rest)
	}
	if registryURL != "" && host != registryHost(registryURL) {
		return invalidf("image reference %q must target the internal registry %q, not %q",
			ref, registryHost(registryURL), host)
	}
	// The registry gate refuses the build principal these repositories anyway
	// (they hold the platform's own images and the scanner's DB mirrors); refusing
	// here turns a build that would fail at its last step into a 400 up front.
	root, _, _ := strings.Cut(rest, "/")
	for _, reserved := range registrygate.ReservedRepoRoots {
		if root == reserved || strings.HasPrefix(root, reserved+":") {
			return invalidf("image reference %q is in %s/, which is reserved for the platform's own images", ref, reserved)
		}
	}
	return nil
}

// splitRegistryHost separates the registry host from the remaining path+tag. A
// reference is host-qualified only if the first segment looks like a registry
// host — it contains a '.' or ':' (port), matching containerd's heuristic.
// "foo/bar:1" (Docker Hub shorthand) is rejected: builds must be explicit about
// the internal registry.
func splitRegistryHost(ref string) (host, rest string, ok bool) {
	slash := strings.IndexByte(ref, '/')
	if slash < 0 {
		return "", "", false
	}
	host = ref[:slash]
	rest = ref[slash+1:]
	if !strings.ContainsAny(host, ".:") {
		return "", "", false
	}
	if rest == "" {
		return "", "", false
	}
	return host, rest, true
}

// registryHost strips any scheme and path from a configured registry URL,
// leaving the host[:port] that an image reference must match.
func registryHost(registryURL string) string {
	h := registryURL
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	return h
}
