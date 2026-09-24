package updater

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeCmd is a fake commandRunner: it returns canned `--version` output keyed by the
// binary name, or a fixed error, so the CLI extraction path is proven without k3s or
// cloudflared installed.
type fakeCmd struct {
	out map[string][]byte
	err error
}

func (f fakeCmd) output(_ context.Context, name string, _ ...string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.out[name]
	if !ok {
		return nil, fmt.Errorf("no canned output for %q", name)
	}
	return b, nil
}

// Real `--version` banners (captured from the tools' known output shape). The k3s
// banner is the important one: the binary prints the '+k3s1' build form, which must
// parse STABLE — contrast versionFromImageRef, which has to repair the '-k3s1' the
// registry forces.
const (
	k3sVersionBanner         = "k3s version v1.36.2+k3s1 (a1b2c3d4)\ngo version go1.24.0\n"
	cloudflaredVersionBanner = "cloudflared version 2026.6.1 (built 2026-06-20-1057 UTC)\n"
)

func TestVersionFromCLI(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantCore [3]int
		wantStr  string
	}{
		{"k3s keeps +build stable", k3sVersionBanner, [3]int{1, 36, 2}, "v1.36.2+k3s1"},
		{"cloudflared calver", cloudflaredVersionBanner, [3]int{2026, 6, 1}, "2026.6.1"},
		{"version after a label", "Version: 1.2.3", [3]int{1, 2, 3}, "1.2.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := versionFromCLI(tc.raw)
			if err != nil {
				t.Fatalf("versionFromCLI(%q): %v", tc.raw, err)
			}
			if got := [3]int{v.Major, v.Minor, v.Patch}; got != tc.wantCore {
				t.Errorf("core = %v, want %v", got, tc.wantCore)
			}
			if v.IsPrerelease() {
				t.Errorf("%q parsed as prerelease", tc.raw)
			}
			if v.String() != tc.wantStr {
				t.Errorf("String() = %q, want %q", v.String(), tc.wantStr)
			}
		})
	}

	for _, bad := range []string{"", "   ", "no version in here at all", "k3s\ngo version go1.24"} {
		if v, err := versionFromCLI(bad); err == nil {
			t.Errorf("versionFromCLI(%q) = %q, want error", bad, v.String())
		}
	}
}

func TestVersionFromImageRef(t *testing.T) {
	cases := []struct {
		name     string
		ref      string
		wantCore [3]int
	}{
		{"k3s docker tag restored to stable", "rancher/k3s:v1.36.2-k3s1", [3]int{1, 36, 2}},
		{"rke2 docker tag restored to stable", "rancher/rke2-runtime:v1.31.4-rke2r1", [3]int{1, 31, 4}},
		{"clean semver with registry", "ghcr.io/acme/felis-api:1.4.0", [3]int{1, 4, 0}},
		{"registry port kept, digest stripped", "localhost:5000/felis-api:1.4.0@sha256:deadbeef", [3]int{1, 4, 0}},
		{"cloudflared calver image", "cloudflare/cloudflared:2026.6.1", [3]int{2026, 6, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := versionFromImageRef(tc.ref)
			if err != nil {
				t.Fatalf("versionFromImageRef(%q): %v", tc.ref, err)
			}
			if got := [3]int{v.Major, v.Minor, v.Patch}; got != tc.wantCore {
				t.Errorf("core = %v, want %v", got, tc.wantCore)
			}
			if v.IsPrerelease() {
				t.Errorf("%q parsed as prerelease — the '-k3s1'/'-rke2r1' build suffix was not restored to '+'", tc.ref)
			}
		})
	}

	// No tag, empty tag, non-version tag, and empty ref all fail closed.
	for _, bad := range []string{"", "ubuntu", "ghcr.io/acme/felis-api", "repo:latest", "repo:", "repo:latest@sha256:abc"} {
		if v, err := versionFromImageRef(bad); err == nil {
			t.Errorf("versionFromImageRef(%q) = %q, want error", bad, v.String())
		}
	}
}

func TestVersionFromJarName(t *testing.T) {
	cases := []struct {
		name     string
		jar      string
		wantCore [3]int
	}{
		{"snapshot build, qualifier dropped", "velocity-3.4.0-SNAPSHOT-461.jar", [3]int{3, 4, 0}},
		{"plain release jar", "velocity-3.4.0.jar", [3]int{3, 4, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := versionFromJarName(tc.jar)
			if err != nil {
				t.Fatalf("versionFromJarName(%q): %v", tc.jar, err)
			}
			if got := [3]int{v.Major, v.Minor, v.Patch}; got != tc.wantCore {
				t.Errorf("core = %v, want %v", got, tc.wantCore)
			}
			if v.IsPrerelease() {
				t.Errorf("%q parsed as prerelease — the numeric core should be taken bare", tc.jar)
			}
		})
	}

	for _, bad := range []string{"", "velocity.jar", "proxy-latest.jar"} {
		if v, err := versionFromJarName(bad); err == nil {
			t.Errorf("versionFromJarName(%q) = %q, want error", bad, v.String())
		}
	}
}

// fullFakeGatherer wires every seam with a fake so Current() can be driven for all four
// real Topology components without a node, a cluster, or the proxy host.
func fullFakeGatherer() sysGatherer {
	return sysGatherer{
		run: fakeCmd{out: map[string][]byte{
			"k3s":         []byte(k3sVersionBanner),
			"cloudflared": []byte(cloudflaredVersionBanner),
		}},
		imageForSpec: func(_ context.Context, _ Spec) (string, error) { return "ghcr.io/acme/felis-api:1.4.0", nil },
		jarForSpec:   func(_ context.Context, _ Spec) (string, error) { return "velocity-3.4.0-SNAPSHOT-461.jar", nil },
	}
}

// TestSysGathererCurrentDispatch drives Current() for every component the real Topology
// tracks, proving each name routes to the right seam+extractor and the discovered
// current version (including the raw tag a report needs) comes back correctly.
func TestSysGathererCurrentDispatch(t *testing.T) {
	g := fullFakeGatherer()
	want := map[string]struct {
		core [3]int
		str  string
	}{
		"k3s":         {[3]int{1, 36, 2}, "v1.36.2+k3s1"},
		"cloudflared": {[3]int{2026, 6, 1}, "2026.6.1"},
		"felis-api":   {[3]int{1, 4, 0}, "1.4.0"},
		"velocity":    {[3]int{3, 4, 0}, "3.4.0"},
	}
	// The JRE and PostgreSQL live on the host alone; hostGatherer answers them
	// (gatherer_host_test.go), and here they must fail closed.
	hostOnly := map[string]bool{"jre": true, "postgresql": true}
	for _, spec := range Topology() {
		if hostOnly[spec.Name] {
			if v, err := g.Current(context.Background(), spec); err == nil {
				t.Errorf("Current(%s) = %s from the system gatherer, want an error", spec.Name, v)
			}
			continue
		}
		w, ok := want[spec.Name]
		if !ok {
			t.Fatalf("Topology grew a component %q with no gather expectation — update this test", spec.Name)
		}
		v, err := g.Current(context.Background(), spec)
		if err != nil {
			t.Errorf("Current(%s): %v", spec.Name, err)
			continue
		}
		if got := [3]int{v.Major, v.Minor, v.Patch}; got != w.core {
			t.Errorf("Current(%s) core = %v, want %v", spec.Name, got, w.core)
		}
		if v.String() != w.str {
			t.Errorf("Current(%s) String() = %q, want %q", spec.Name, v.String(), w.str)
		}
	}
}

// TestSysGathererFailsClosed covers every way a gather can fail: a runner error, a seam
// that is not wired (the production default for felis-api/velocity), and an unknown
// component. None returns a usable zero version.
func TestSysGathererFailsClosed(t *testing.T) {
	k3s := Spec{Name: "k3s"}
	felis := Spec{Name: "felis-api"}
	velo := Spec{Name: "velocity"}

	// Runner error propagates.
	boom := sysGatherer{run: fakeCmd{err: errors.New("exec: k3s not found")}}
	if _, err := boom.Current(context.Background(), k3s); err == nil {
		t.Error("a runner error should fail the gather")
	}

	// The production constructor leaves the image/jar seams nil: those components must
	// report an explicit not-wired error, not a wrong version.
	prod := NewSysGatherer()
	if _, err := prod.Current(context.Background(), felis); err == nil {
		t.Error("felis-api gather should error while the image seam is unwired")
	}
	if _, err := prod.Current(context.Background(), velo); err == nil {
		t.Error("velocity gather should error while the jar seam is unwired")
	}

	// An unknown component is a loud error, never a silent skip.
	if _, err := prod.Current(context.Background(), Spec{Name: "postgres"}); err == nil {
		t.Error("an unrecognized component should error")
	}
}
