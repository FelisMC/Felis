package platform

import (
	"fmt"
	"os"
	"regexp"
	"testing"
)

func readBootstrap(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../deploy/bootstrap.sh")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// bootstrapVar returns the value of a top-level NAME=value or NAME="value" line.
func bootstrapVar(t *testing.T, b []byte, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + name + `=("([^"]*)"|(\S+))$`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("deploy/bootstrap.sh has no top-level %s= line", name)
	}
	if m[2] != nil {
		return string(m[2])
	}
	return string(m[3])
}

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

// The renderer puts defaultPostgresImage in the felis-postgres Deployment, and
// bootstrap pulls and GC-pins POSTGRES_IMAGE and reads the cluster directory's
// major version off its tag. If the two drift, the pod runs an image bootstrap
// never checked against the data directory.
func TestBootstrapPinsThePostgresImage(t *testing.T) {
	if got := bootstrapVar(t, readBootstrap(t), "POSTGRES_IMAGE"); got != defaultPostgresImage {
		t.Fatalf("bootstrap POSTGRES_IMAGE = %q, renderer default = %q", got, defaultPostgresImage)
	}
	if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(defaultPostgresImage) {
		t.Fatalf("defaultPostgresImage %q is not pinned by digest", defaultPostgresImage)
	}
}

// Bootstrap creates the data directory, the superuser Secret and the host config
// that point at what PostgresObjects renders, and execs into its container by name.
func TestBootstrapAgreesWithThePostgresConstants(t *testing.T) {
	b := readBootstrap(t)
	for _, c := range []struct{ name, want string }{
		{"PG_DEPLOYMENT", PostgresName},
		{"PG_CONTAINER", PostgresContainer},
		{"PG_SECRET", PostgresSuperuserSecret},
		{"PG_SECRET_KEY", PostgresSuperuserSecretKey},
		{"PG_DATA_DIR", PostgresDataHostPath},
		{"PG_UID", fmt.Sprint(postgresUID)},
		{"PG_HOST_PORT", fmt.Sprint(PostgresHostPort)},
		{"CONTROL_NS", DefaultControlNamespace},
		{"PG_SERVICE_ADDR", fmt.Sprintf("${PG_DEPLOYMENT}.${CONTROL_NS}.svc:%d", PostgresPort)},
	} {
		if got := bootstrapVar(t, b, c.name); got != c.want {
			t.Errorf("bootstrap %s = %q, platform = %q", c.name, got, c.want)
		}
	}
}
