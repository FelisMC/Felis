package build

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"felis.lolicon.best/internal/registrygate"
)

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

// imageNameRE matches the path+tag under the registry host; strict so no shell metacharacters reach the kaniko/trivy argv.
var imageNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?(:[a-zA-Z0-9._-]+)?$`)

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
	if IsHTTPContextRef(req.ContextRef) {
		if err := validateContextURL(req.ContextRef, cfg.ContextOrigin); err != nil {
			return err
		}
	}
	if req.ContextDigest != "" {
		if !IsSHA256Hex(req.ContextDigest) {
			return invalidf("context digest %q is not a lowercase hex sha256", req.ContextDigest)
		}
		if !IsHTTPContextRef(req.ContextRef) {
			return invalidf("a context digest needs an http(s) context reference, whose fetch step checks it")
		}
	}
	return nil
}

var internalContextPathRE = regexp.MustCompile(`^/api/v1/internal/submissions/[A-Za-z0-9_-]+/context$`)

// validateContextURL admits only an uploaded submission on the internal face: the fetch step sends the service token to whatever host the URL names.
func validateContextURL(ref, origin string) error {
	if origin == "" {
		return invalidf("an http(s) context reference is fetched from the platform's internal API, which this builder is not configured with")
	}
	u, err := url.Parse(ref)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		URLOrigin(ref) != URLOrigin(origin) || !internalContextPathRE.MatchString(u.Path) {
		return invalidf("an http(s) context reference must be an uploaded submission on the internal API (%s/api/v1/internal/submissions/<id>/context)",
			strings.TrimRight(origin, "/"))
	}
	return nil
}

// URLOrigin returns the lowercased scheme://host[:port] of raw, or "" when raw is not an absolute http(s) URL.
func URLOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func IsSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidateImageRef checks a bare image reference. A trailing ":*" tag wildcard is stripped first; it is only
// compared by imageMatches and never reaches a kaniko/trivy argv.
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

// splitTag only treats a colon after the final path separator as the tag separator, so a registry port is not a tag.
func splitTag(ref string) (repo, tag string) {
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

// imageMatches reports whether ref equals pattern or pattern is "repo:*" and ref has a non-empty tag on that repo.
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

// validateRegistryTarget requires a build to push to the configured registry host; an empty registryURL skips the host check.
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
	// The registry gate refuses these repositories anyway; refusing here fails fast with a 400.
	root, _, _ := strings.Cut(rest, "/")
	for _, reserved := range registrygate.ReservedRepoRoots {
		if root == reserved || strings.HasPrefix(root, reserved+":") {
			return invalidf("image reference %q is in %s/, which is reserved for the platform's own images", ref, reserved)
		}
	}
	return nil
}

// splitRegistryHost treats the first segment as the host only if it contains '.' or ':' (containerd's heuristic), so "foo/bar:1" is rejected.
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
