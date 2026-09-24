// Package imagepin fixes a server's image to the exact build it was created
// with. The platform's own game images are published under mutable tags
// (registry.felis.svc:5000/felis/paper:demo): every installer run rebuilds them
// against the newest Paper/Limbo release and pushes over the same tag. A server
// whose spec.image names that tag would boot whatever the tag points at on its
// next wake, so re-running the installer would silently move a sleeping world to
// a newer Minecraft version. Chunk upgrades are one-way, so that move can never
// be taken back.
//
// Pin resolves such a tag to the manifest digest it names right now and appends
// it (name:tag@sha256:…). Kubernetes pulls a reference carrying a digest by the
// digest alone, so the tag stays only as a readable label of where the build
// came from. A pinned server changes image only when an admin changes
// spec.image, which the API makes an explicit, confirmed step.
//
// Only refs in the platform registry are resolved. That registry is reachable
// anonymously for reads from inside the cluster; a public registry would need a
// token exchange per vendor and egress felis-api does not otherwise have, and an
// external image is one an admin whitelisted by an exact tag of their choosing.
package imagepin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ErrNotFound means the registry answered and does not hold the tag: the image
// was whitelisted but never pushed, or has been deleted since.
var ErrNotFound = errors.New("imagepin: tag not found in the registry")

// manifestAccept lists every manifest shape the registry may hold for a tag. A
// registry asked without an Accept it can satisfy answers with a converted
// schema-1 manifest, whose digest is not the one kubelet would pull.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// maxManifestBytes bounds the body read when the registry sends no digest
// header; a manifest or index is a few KiB.
const maxManifestBytes = 4 << 20

// Pinned reports whether ref already names a digest.
func Pinned(ref string) bool { return strings.Contains(ref, "@") }

// Resolver pins refs that live in one registry.
type Resolver struct {
	// Registry is the host[:port] the refs spell, the [registry] url
	// (registry.felis.svc:5000). Refs under any other host are left as they are.
	Registry string
	// Endpoint is the host[:port] to dial for it. Empty means Registry, which is
	// right inside the cluster; a command on the node reaches the same registry
	// through its loopback hostPort instead.
	Endpoint string
	// Client makes the request. Nil uses a client with a 10s timeout.
	Client *http.Client
}

// Covers reports whether ref lives in the resolver's registry.
func (r Resolver) Covers(ref string) bool {
	return r.Registry != "" && strings.HasPrefix(ref, r.Registry+"/")
}

// Pin returns ref with the digest its tag names now appended. A ref outside the
// registry comes back unchanged. A ref that already carries a digest comes back
// unchanged once the registry confirms it still holds that manifest: the registry
// pruner deletes builds nothing references, and a server set back to one of them
// would otherwise sit in ImagePullBackOff.
func (r Resolver) Pin(ctx context.Context, ref string) (string, error) {
	if !r.Covers(ref) {
		return ref, nil
	}
	if name, pinned, ok := strings.Cut(ref, "@"); ok {
		repo, _ := splitTag(strings.TrimPrefix(name, r.Registry+"/"))
		if repo == "" || !digestRE.MatchString(pinned) {
			return "", fmt.Errorf("imagepin: %q is not a valid pinned reference", ref)
		}
		if _, err := r.digest(ctx, repo, pinned); err != nil {
			return "", fmt.Errorf("imagepin: resolve %s: %w", ref, err)
		}
		return ref, nil
	}
	repo, tag := splitTag(strings.TrimPrefix(ref, r.Registry+"/"))
	if repo == "" {
		return "", fmt.Errorf("imagepin: %q names no repository", ref)
	}
	digest, err := r.digest(ctx, repo, tag)
	if err != nil {
		return "", fmt.Errorf("imagepin: resolve %s: %w", ref, err)
	}
	if !strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":") {
		ref += ":" + tag // spell the implied tag out, so the label reads as what was pinned
	}
	return ref + "@" + digest, nil
}

// splitTag splits "felis/paper:demo" into ("felis/paper", "demo"); a path with no
// tag means "latest", as it does for every image client.
func splitTag(path string) (repo, tag string) {
	slash := strings.LastIndex(path, "/")
	if colon := strings.LastIndex(path, ":"); colon > slash {
		return path[:colon], path[colon+1:]
	}
	return path, "latest"
}

func (r Resolver) digest(ctx context.Context, repo, tag string) (string, error) {
	endpoint := r.Endpoint
	if endpoint == "" {
		endpoint = r.Registry
	}
	// Plain HTTP: the platform registry is an in-cluster Service and the node's
	// loopback hostPort, and containerd's mirror for it is configured the same way.
	url := "http://" + endpoint + "/v2/" + repo + "/manifests/" + tag
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// HEAD first: the registry answers it with Docker-Content-Digest and no body.
	// GET covers a registry that leaves the header off, by hashing the manifest.
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", manifestAccept)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		d, err := readDigest(resp, method == http.MethodGet)
		resp.Body.Close()
		if err != nil || d != "" {
			return d, err
		}
	}
	return "", errors.New("registry sent neither a digest header nor a manifest")
}

// readDigest takes the digest from a manifest response, hashing the body when the
// header is missing and hash is set. An empty digest with a nil error means "try
// the next method".
func readDigest(resp *http.Response, hash bool) (string, error) {
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("registry answered %s", resp.Status)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		if !digestRE.MatchString(d) {
			return "", fmt.Errorf("registry sent a malformed digest %q", d)
		}
		return d, nil
	}
	if !hash {
		return "", nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxManifestBytes {
		return "", errors.New("manifest is implausibly large")
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
