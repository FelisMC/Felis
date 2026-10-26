package build

import (
	"strings"
	"testing"
)

func TestToolRefsDefaultToTheRegistryCopies(t *testing.T) {
	for _, ref := range (Config{RegistryURL: "registry.felis.svc:5000"}).ToolRefs() {
		if !strings.HasPrefix(ref, "registry.felis.svc:5000/mirror/") {
			t.Errorf("default tool ref %q is not the registry copy", ref)
		}
	}

	explicit := Config{RegistryURL: "r:5000", KanikoImage: "r:5000/mirror/kaniko-fork:v2", TrivyDBRepository: "r:5000/mirror/db:9"}.ToolRefs()
	if explicit[0] != "r:5000/mirror/kaniko-fork:v2" || explicit[2] != "r:5000/mirror/db:9" {
		t.Errorf("explicit values lost: %v", explicit)
	}

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
