package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestLayout writes an OCI layout tar holding one linux/arch image with one
// layer, the way buildx --output type=oci leaves a single-platform build, and
// returns its path with the manifest and config digests.
func writeTestLayout(t *testing.T, dir, arch, layer string) (path, manifestDigest, configDigest string) {
	t.Helper()
	blobs := map[string][]byte{}
	add := func(b []byte) (string, int) {
		sum := sha256.Sum256(b)
		d := "sha256:" + hex.EncodeToString(sum[:])
		blobs[d] = b
		return d, len(b)
	}
	cfgDigest, cfgSize := add([]byte(`{"architecture":"` + arch + `","os":"linux","rootfs":{"type":"layers"}}`))
	layerDigest, layerSize := add([]byte(layer))
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},`+
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`,
		cfgDigest, cfgSize, layerDigest, layerSize)
	mDigest, mSize := add([]byte(manifest))
	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []map[string]any{{
			"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": mDigest, "size": mSize,
		}},
	})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	put := func(name string, b []byte) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	put("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	put("index.json", index)
	for d, b := range blobs {
		put("blobs/sha256/"+strings.TrimPrefix(d, "sha256:"), b)
	}
	tw.Close()
	path = filepath.Join(dir, arch+"-"+layer+".tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, mDigest, cfgDigest
}

func TestImageBundleWritesTheListingTheInstallerReads(t *testing.T) {
	dir := t.TempDir()
	limbo, limboDigest, limboConfig := writeTestLayout(t, dir, "arm64", "limbo")
	lobby, lobbyDigest, lobbyConfig := writeTestLayout(t, dir, "arm64", "lobby")
	out, list := filepath.Join(dir, "images.tar"), filepath.Join(dir, "images.txt")
	var stderr bytes.Buffer
	code := cmdImageBundle([]string{"--platform", "linux/arm64", "--out", out, "--list", list,
		"--layout", "limbo=registry.felis.svc:5000/felis/limbo:demo=" + limbo,
		"--layout", "lobby=registry.felis.svc:5000/felis/lobby:demo=" + lobby,
	}, nil, &stderr)
	if code != 0 {
		t.Fatalf("image-bundle = %d: %s", code, stderr.String())
	}
	// deploy/bootstrap.sh reads this with `read -r role name digest config`.
	got, _ := os.ReadFile(list)
	want := "limbo registry.felis.svc:5000/felis/limbo:demo " + limboDigest + " " + limboConfig + "\n" +
		"lobby registry.felis.svc:5000/felis/lobby:demo " + lobbyDigest + " " + lobbyConfig + "\n"
	if string(got) != want {
		t.Errorf("listing:\n%s\nwant:\n%s", got, want)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Errorf("bundle: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Errorf("left behind %v", leftovers)
	}
}

func TestImageBundleLeavesNothingWhenAnImageIsRefused(t *testing.T) {
	dir := t.TempDir()
	amd, _, _ := writeTestLayout(t, dir, "amd64", "limbo")
	out, list := filepath.Join(dir, "images.tar"), filepath.Join(dir, "images.txt")
	// The pair an earlier run wrote stays as it was: a listing beside a bundle it
	// does not describe would have the installer look for images that are not there.
	os.WriteFile(out, []byte("old bundle"), 0o644)
	os.WriteFile(list, []byte("old listing\n"), 0o644)
	var stderr bytes.Buffer
	code := cmdImageBundle([]string{"--platform", "linux/arm64", "--out", out, "--list", list,
		"--layout", "limbo=registry.felis.svc:5000/felis/limbo:demo=" + amd}, nil, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "is a linux/amd64 image") {
		t.Fatalf("image-bundle = %d: %s", code, stderr.String())
	}
	if got, _ := os.ReadFile(out); string(got) != "old bundle" {
		t.Errorf("bundle was replaced with %d bytes", len(got))
	}
	if got, _ := os.ReadFile(list); string(got) != "old listing\n" {
		t.Errorf("listing was replaced with %q", got)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Errorf("left behind %v", leftovers)
	}
}

func TestPushImageReadsABundleByImageName(t *testing.T) {
	dir := t.TempDir()
	limbo, _, _ := writeTestLayout(t, dir, "arm64", "limbo")
	out, list := filepath.Join(dir, "images.tar"), filepath.Join(dir, "images.txt")
	if code := cmdImageBundle([]string{"--platform", "linux/arm64", "--out", out, "--list", list,
		"--layout", "limbo=registry.felis.svc:5000/felis/limbo:demo=" + limbo}, nil, &bytes.Buffer{}); code != 0 {
		t.Fatal("image-bundle failed")
	}
	t.Setenv("FELIS_REGISTRY_USERNAME", "platform")
	t.Setenv("FELIS_REGISTRY_PASSWORD", "x")
	// The name is looked up in the bundle's index before the registry is contacted,
	// so a name the bundle lacks fails here with what it does hold.
	var stderr bytes.Buffer
	code := cmdPushImage([]string{"--tar", out, "--image", "registry.felis.svc:5000/felis/lobby:demo",
		"--ref", "127.0.0.1:1/felis/lobby:demo"}, &bytes.Buffer{}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "holds no image named registry.felis.svc:5000/felis/lobby:demo (it holds: registry.felis.svc:5000/felis/limbo:demo") {
		t.Fatalf("push-image = %d: %s", code, stderr.String())
	}
}
