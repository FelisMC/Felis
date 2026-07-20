package updater

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"felis.lolicon.best/internal/updates"
)

// writeJar builds a minimal jar (a zip) at path. A nil manifest writes no
// META-INF/MANIFEST.MF at all, which is the "unreadable manifest" case.
func writeJar(t *testing.T, path string, manifest []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create jar: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	if manifest != nil {
		w, err := zw.Create("META-INF/MANIFEST.MF")
		if err != nil {
			t.Fatalf("create manifest entry: %v", err)
		}
		if _, err := w.Write(manifest); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close jar: %v", err)
	}
}

// The manifest read is the whole reason this seam can answer at all: bootstrap
// installs the proxy as a fixed "velocity.jar", so the filename carries no version
// and versionFromJarName alone would always fail on a real Felis node.
func TestVelocityJarVersionReadsManifest(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "velocity.jar")
	writeJar(t, jar, []byte("Manifest-Version: 1.0\r\nImplementation-Version: 3.5.1\r\nMain-Class: com.velocitypowered.proxy.Velocity\r\n\r\n"))

	got, err := velocityJarVersion(jar)
	if err != nil {
		t.Fatalf("velocityJarVersion: %v", err)
	}
	if got.String() != "3.5.1" {
		t.Fatalf("version = %q, want 3.5.1", got.String())
	}
}

// A jar with no usable manifest still answers when the operator hand-placed a
// versioned filename — the fallback path.
func TestVelocityJarVersionFallsBackToFilename(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "velocity-3.4.0.jar")
	writeJar(t, jar, nil)

	got, err := velocityJarVersion(jar)
	if err != nil {
		t.Fatalf("velocityJarVersion: %v", err)
	}
	if got.String() != "3.4.0" {
		t.Fatalf("version = %q, want 3.4.0", got.String())
	}
}

// Fails closed: neither source carries a version, so this must error rather than
// report a zero version that would make every upstream release look like an upgrade.
func TestVelocityJarVersionFailsClosed(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "velocity.jar")
	writeJar(t, jar, []byte("Manifest-Version: 1.0\r\n\r\n"))

	if _, err := velocityJarVersion(jar); err == nil {
		t.Fatal("want an error when neither the manifest nor the filename carries a version")
	}
	if _, err := velocityJarVersion(filepath.Join(dir, "absent.jar")); err == nil {
		t.Fatal("want an error for a missing jar")
	}
}

// An unstamped `go build` reports "dev". That must be a loud gather failure, not a
// silent 0.0.0 — a zero current would make every release upstream a false "upgrade".
func TestHostGathererRejectsUnstampedBuild(t *testing.T) {
	g := NewHostGatherer("dev", filepath.Join(t.TempDir(), "velocity.jar"))
	if _, err := g.Current(context.Background(), Spec{Name: "felis-api"}); err == nil {
		t.Fatal("want an error for an unstamped build stamp")
	}
}

// The felis binary's own stamp answers for felis-api: bootstrap builds the image and
// the host binary from one checkout with one `git describe`, so they are the same
// artifact. A release stamp must round-trip into a comparable version.
func TestHostGathererUsesOwnBuildStamp(t *testing.T) {
	g := NewHostGatherer("v1.2.3", filepath.Join(t.TempDir(), "velocity.jar"))
	got, err := g.Current(context.Background(), Spec{Name: "felis-api"})
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if want := (updates.Version{Major: 1, Minor: 2, Patch: 3}); got.Compare(want) != 0 {
		t.Fatalf("version = %s, want 1.2.3", got)
	}
}
