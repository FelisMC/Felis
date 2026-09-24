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

// The JRE answers from its release file. Temurin's SEMANTIC_VERSION keeps the respin
// component; JAVA_RUNTIME_VERSION's "-LTS" tail would read as a prerelease.
func TestJREReleaseVersion(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"JAVA_RUNTIME_VERSION=\"25.0.4.1+1-LTS\"\nJAVA_VERSION=\"25.0.4.1\"\nSEMANTIC_VERSION=\"25.0.4.1+1\"\n": "25.0.4.1+1",
		"JAVA_VERSION=\"21.0.8\"\n": "21.0.8",
	}
	for body, want := range cases {
		path := filepath.Join(dir, "release")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		v, err := jreReleaseVersion(path)
		if err != nil {
			t.Fatalf("jreReleaseVersion(%q): %v", body, err)
		}
		if v.String() != want || v.IsPrerelease() {
			t.Errorf("jreReleaseVersion(%q) = %s, want stable %s", body, v, want)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), []byte("IMPLEMENTOR=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, err := jreReleaseVersion(filepath.Join(dir, "release")); err == nil {
		t.Errorf("a release file without a version = %s, want an error", v)
	}
	if _, err := jreReleaseVersion(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing release file must be an error")
	}
}

// PostgreSQL answers from the server binary where it is on PATH, else from the client.
func TestHostGathererPostgres(t *testing.T) {
	for name, out := range map[string]map[string][]byte{
		"server on PATH": {"postgres": []byte("postgres (PostgreSQL) 13.23\n"), "psql": []byte("psql (PostgreSQL) 12.1\n")},
		"client only":    {"psql": []byte("psql (PostgreSQL) 13.23 (Ubuntu 13.23-1.pgdg24.04+1)\n")},
	} {
		g := hostGatherer{sys: sysGatherer{run: fakeCmd{out: out}}}
		v, err := g.Current(context.Background(), Spec{Name: "postgresql"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.String() != "13.23" {
			t.Errorf("%s: version = %s, want 13.23", name, v)
		}
	}
	g := hostGatherer{sys: sysGatherer{run: fakeCmd{out: map[string][]byte{}}}}
	if _, err := g.Current(context.Background(), Spec{Name: "postgresql"}); err == nil {
		t.Error("no postgres and no psql must be an error")
	}
}
