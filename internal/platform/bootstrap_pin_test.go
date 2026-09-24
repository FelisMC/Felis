package platform

import (
	"os"
	"regexp"
	"testing"
)

// The renderer puts defaultRegistryImage in the registry Deployment, and bootstrap
// caches and GC-pins REGISTRY_IMAGE in containerd. If the two drift, kubelet looks
// for an image nobody cached or pinned, and an air-gapped box cannot start its
// registry at all.
func TestBootstrapPinsTheRegistryImage(t *testing.T) {
	b, err := os.ReadFile("../../deploy/bootstrap.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^REGISTRY_IMAGE="([^"]+)"$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("deploy/bootstrap.sh has no REGISTRY_IMAGE=\"...\" line")
	}
	if got := string(m[1]); got != defaultRegistryImage {
		t.Fatalf("bootstrap REGISTRY_IMAGE = %q, renderer default = %q", got, defaultRegistryImage)
	}
	if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(defaultRegistryImage) {
		t.Fatalf("defaultRegistryImage %q is not pinned by digest", defaultRegistryImage)
	}
}
