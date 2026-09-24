package build

import (
	"strings"
	"testing"
)

// A build reads every tool from the platform registry's copy by default, and
// each copy's repository lives under mirror/, which only the platform principal
// may write and the pruner keeps.
func TestToolRefsDefaultToTheRegistryCopies(t *testing.T) {
	got := Config{RegistryURL: "registry.felis.svc:5000"}.ToolRefs()
	want := []string{
		"registry.felis.svc:5000/mirror/kaniko-executor:v1.24.0",
		"registry.felis.svc:5000/mirror/trivy:0.74.0",
		"registry.felis.svc:5000/mirror/trivy-db:2",
		"registry.felis.svc:5000/mirror/trivy-java-db:1",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("ToolRefs = %v, want %v", got, want)
	}

	explicit := Config{RegistryURL: "r:5000", KanikoImage: "r:5000/mirror/kaniko-fork:v2", TrivyDBRepository: "r:5000/mirror/db:9"}.ToolRefs()
	if explicit[0] != "r:5000/mirror/kaniko-fork:v2" || explicit[2] != "r:5000/mirror/db:9" {
		t.Errorf("explicit values lost: %v", explicit)
	}

	// Without a registry the images are the pinned upstream sources and Trivy
	// keeps its own DB default.
	bare := Config{}.ToolRefs()
	if !strings.Contains(bare[0], "@sha256:") || !strings.Contains(bare[1], "@sha256:") || bare[2] != "" || bare[3] != "" {
		t.Errorf("bare ToolRefs = %v", bare)
	}
}

func TestToolsArePinnedAndMirroredUnderMirror(t *testing.T) {
	for _, tl := range Tools {
		if !strings.HasPrefix(tl.Mirror, "mirror/") || !strings.Contains(tl.Mirror, ":") {
			t.Errorf("%s: mirror %q must be mirror/<repo>:<tag>", tl.Name, tl.Mirror)
		}
		isDB := strings.HasSuffix(tl.Name, "-db")
		if pinned := strings.Contains(tl.Source, "@sha256:"); pinned == isDB {
			t.Errorf("%s: source %q: executor images must be pinned by digest, DBs follow their tag", tl.Name, tl.Source)
		}
	}
}
