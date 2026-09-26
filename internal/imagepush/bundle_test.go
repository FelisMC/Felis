package imagepush

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/registrygate"
)

// layoutBuilder writes OCI layout tars the way buildx's oci exporter does.
type layoutBuilder struct {
	blobs map[string][]byte
	order []string
}

func newLayoutBuilder() *layoutBuilder { return &layoutBuilder{blobs: map[string][]byte{}} }

func (b *layoutBuilder) add(body []byte) descriptor {
	d := digestOf(body)
	if _, ok := b.blobs[d]; !ok {
		b.order = append(b.order, d)
	}
	b.blobs[d] = body
	return descriptor{MediaType: mediaOCILayerGz, Size: int64(len(body)), Digest: d}
}

// image stores one platform's manifest and returns its descriptor.
func (b *layoutBuilder) image(arch string, layers ...string) layoutDescriptor {
	cfg := b.add([]byte(`{"architecture":"` + arch + `","os":"linux","rootfs":{"type":"layers"}}`))
	cfg.MediaType = mediaOCIConfig
	m := manifest{SchemaVersion: 2, MediaType: mediaOCIManifest, Config: cfg}
	for _, l := range layers {
		m.Layers = append(m.Layers, b.add([]byte(l)))
	}
	body, _ := json.Marshal(m)
	d := b.add(body)
	return layoutDescriptor{MediaType: mediaOCIManifest, Digest: d.Digest, Size: d.Size}
}

// index stores a nested index over the given entries.
func (b *layoutBuilder) index(entries ...layoutDescriptor) layoutDescriptor {
	body, _ := json.Marshal(layoutIndex{SchemaVersion: 2, MediaType: mediaOCIIndex, Manifests: entries})
	d := b.add(body)
	return layoutDescriptor{MediaType: mediaOCIIndex, Digest: d.Digest, Size: d.Size}
}

func (b *layoutBuilder) write(t *testing.T, top ...layoutDescriptor) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, body []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	add(layoutMarkerFile, []byte(layoutMarkerContent))
	idx, _ := json.Marshal(layoutIndex{SchemaVersion: 2, MediaType: mediaOCIIndex, Manifests: top})
	add(layoutIndexFile, idx)
	for _, d := range b.order {
		add(blobPath(d), b.blobs[d])
	}
	tw.Close()
	path := filepath.Join(t.TempDir(), "layout.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// readTar returns every regular file in a tar, and fails on a name written twice.
func readTar(t *testing.T, r io.Reader) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if _, dup := files[h.Name]; dup {
			t.Errorf("%s is written twice", h.Name)
		}
		files[h.Name], _ = io.ReadAll(tr)
	}
}

func TestBundleCarriesOnePlatformAndPushesByteForByte(t *testing.T) {
	// The control-plane image the way buildx leaves it with provenance on: an index
	// over the image and an attestation manifest, beside another architecture.
	lb := newLayoutBuilder()
	arm := lb.image("arm64", "shared base", "felis binary")
	amd := lb.image("amd64", "amd64 base", "amd64 binary")
	att := lb.image("unknown", "provenance")
	att.Annotations = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
	att.Platform = &platformSpec{OS: "unknown", Architecture: "unknown"}
	arm.Platform = &platformSpec{OS: "linux", Architecture: "arm64"}
	amd.Platform = &platformSpec{OS: "linux", Architecture: "amd64"}
	felisLayout := lb.write(t, lb.index(amd, att, arm))

	// A game image built alone: one manifest, no platform field, sharing a base layer.
	lb2 := newLayoutBuilder()
	limbo := lb2.image("arm64", "shared base", "limbo jar")
	limboLayout := lb2.write(t, limbo)

	src := newFakeSource(t)
	idx, srcArm := src.addIndex("2.8.3")
	pinned := src.host() + "/tools/thing:2.8.3@" + idx

	var out bytes.Buffer
	s := &Source{Scheme: "http", Platform: "linux/arm64"}
	entries, err := WriteBundle(context.Background(), s, []BundleImage{
		{Role: "felis", Name: "registry.felis.svc:5000/felis/felis:v1.2.3", Layout: felisLayout},
		{Role: "limbo", Name: "registry.felis.svc:5000/felis/limbo:demo", Layout: limboLayout},
		{Role: "registry", Name: PinnedName(pinned), Source: pinned},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{"felis": arm.Digest, "limbo": limbo.Digest, "registry": srcArm}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	for _, e := range entries {
		if e.Digest != want[e.Role] {
			t.Errorf("%s: digest %s, want the arm64 manifest %s", e.Role, e.Digest, want[e.Role])
		}
	}
	if entries[2].Name != src.host()+"/tools/thing@"+idx {
		t.Errorf("the pulled image is named %s, want repository@index-digest, the name CRI looks the pinned ref up by", entries[2].Name)
	}

	files := readTar(t, bytes.NewReader(out.Bytes()))
	if string(files[layoutMarkerFile]) != layoutMarkerContent {
		t.Errorf("oci-layout = %q", files[layoutMarkerFile])
	}
	var index layoutIndex
	if err := json.Unmarshal(files[layoutIndexFile], &index); err != nil {
		t.Fatal(err)
	}
	names := map[string]layoutDescriptor{}
	for _, m := range index.Manifests {
		if m.Platform != nil || m.MediaType != mediaOCIManifest {
			t.Errorf("index entry %+v must point straight at a manifest, with no platform", m)
		}
		names[m.Annotations[annotationImageName]] = m
	}
	for name, digest := range map[string]string{
		"registry.felis.svc:5000/felis/felis:v1.2.3":          arm.Digest,
		"registry.felis.svc:5000/felis/felis@" + arm.Digest:   arm.Digest,
		"registry.felis.svc:5000/felis/limbo:demo":            limbo.Digest,
		"registry.felis.svc:5000/felis/limbo@" + limbo.Digest: limbo.Digest,
		src.host() + "/tools/thing@" + idx:                    srcArm,
	} {
		if names[name].Digest != digest {
			t.Errorf("%s -> %q, want %s", name, names[name].Digest, digest)
		}
	}
	if len(names) != 5 {
		t.Errorf("index names %d images, want 5: %v", len(names), names)
	}
	if got := names["registry.felis.svc:5000/felis/limbo:demo"].Annotations[annotationRefName]; got != "demo" {
		t.Errorf("ref.name = %q, want the tag", got)
	}

	// Every blob an entry needs is in the tar and hashes to its name; nothing from
	// the other architecture or the attestation came along.
	need := map[string]bool{}
	for _, m := range index.Manifests {
		body := files[blobPath(m.Digest)]
		if digestOf(body) != m.Digest {
			t.Fatalf("manifest %s missing or corrupt", m.Digest)
		}
		var mf manifest
		json.Unmarshal(body, &mf)
		need[m.Digest] = true
		for _, d := range append([]descriptor{mf.Config}, mf.Layers...) {
			need[d.Digest] = true
			if digestOf(files[blobPath(d.Digest)]) != d.Digest {
				t.Errorf("blob %s missing or corrupt", d.Digest)
			}
		}
	}
	for name := range files {
		if strings.HasPrefix(name, "blobs/") && !need["sha256:"+strings.TrimPrefix(name, "blobs/sha256/")] {
			t.Errorf("%s is in the bundle but no image uses it", name)
		}
	}

	// The same bytes again give the same bundle.
	var again bytes.Buffer
	if _, err := WriteBundle(context.Background(), s, []BundleImage{
		{Role: "felis", Name: "registry.felis.svc:5000/felis/felis:v1.2.3", Layout: felisLayout},
		{Role: "limbo", Name: "registry.felis.svc:5000/felis/limbo:demo", Layout: limboLayout},
		{Role: "registry", Name: PinnedName(pinned), Source: pinned},
	}, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), again.Bytes()) {
		t.Error("two runs over the same images wrote different bundles")
	}
	// Both runs above fall in the same second; a rebuild of the release a day later
	// must write the same bytes too, so no header carries the time it was written.
	tr := tar.NewReader(bytes.NewReader(out.Bytes()))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if !h.ModTime.Equal(time.Unix(0, 0)) {
			t.Errorf("%s is stamped %v", h.Name, h.ModTime)
		}
	}

	// Pushed from the bundle, the registry records the manifest containerd imported.
	bundle := filepath.Join(t.TempDir(), "bundle.tar")
	os.WriteFile(bundle, out.Bytes(), 0o644)
	reg, host := startStack(t)
	p := &Pusher{Scheme: "http", Username: registrygate.PrincipalPlatform, Password: "plat-secret", Attempts: 1}
	for _, tc := range []struct{ name, repo, digest string }{
		{"registry.felis.svc:5000/felis/limbo:demo", "felis/limbo", limbo.Digest},
		{src.host() + "/tools/thing@" + idx, "felis/thing", srcArm},
	} {
		got, err := p.PushLayout(context.Background(), bundle, tc.name, host+"/"+tc.repo+":demo")
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.digest {
			t.Errorf("pushed %s as %s, want %s", tc.name, got, tc.digest)
		}
		if !bytes.Equal(reg.manifests[tc.repo+":demo"], files[blobPath(tc.digest)]) {
			t.Errorf("%s: the registry's manifest differs from the bundle's bytes", tc.repo)
		}
		if reg.types[tc.repo+":demo"] != mediaOCIManifest {
			t.Errorf("%s: pushed as %q", tc.repo, reg.types[tc.repo+":demo"])
		}
	}
	puts := reg.puts
	if _, err := p.PushLayout(context.Background(), bundle, "registry.felis.svc:5000/felis/limbo:demo", host+"/felis/limbo:demo"); err != nil {
		t.Fatal(err)
	}
	if reg.puts != puts {
		t.Errorf("a second push uploaded %d blobs the registry already held", reg.puts-puts)
	}
}

func TestBundleRefusesAnotherArchitecturesImage(t *testing.T) {
	lb := newLayoutBuilder()
	layout := lb.write(t, lb.image("amd64", "layer"))
	_, err := WriteBundle(context.Background(), &Source{Platform: "linux/arm64"},
		[]BundleImage{{Role: "limbo", Name: "r.example:5000/felis/limbo:demo", Layout: layout}}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is a linux/amd64 image, the bundle is for linux/arm64") {
		t.Fatalf("WriteBundle = %v, want the architecture refused", err)
	}
}

func TestBundleRefusesANonImage(t *testing.T) {
	// An OCI artifact manifest has an image manifest's shape but is no image: the
	// installer's push would refuse it, so the release build must refuse it first.
	lb := newLayoutBuilder()
	img := lb.image("arm64", "layer")
	var mf manifest
	json.Unmarshal(lb.blobs[img.Digest], &mf)
	mf.MediaType = ""
	body, _ := json.Marshal(mf)
	d := lb.add(body)
	layout := lb.write(t, layoutDescriptor{MediaType: "application/vnd.oci.artifact.manifest.v1+json", Digest: d.Digest, Size: d.Size})
	_, err := WriteBundle(context.Background(), &Source{Platform: "linux/arm64"},
		[]BundleImage{{Role: "limbo", Name: "r.example:5000/felis/limbo:demo", Layout: layout}}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "want an image") {
		t.Fatalf("WriteBundle = %v, want the artifact refused", err)
	}
}

func TestBundleRefusesATamperedBlob(t *testing.T) {
	for swapped, want := range map[string]string{
		"the fake layer":           "holds sha256:",
		"the real layer, and more": "is longer than its 14 bytes",
		"the real":                 "(8 bytes)",
	} {
		lb := newLayoutBuilder()
		img := lb.image("arm64", "the real layer")
		var mf manifest
		json.Unmarshal(lb.blobs[img.Digest], &mf)
		lb.blobs[mf.Layers[0].Digest] = []byte(swapped)
		layout := lb.write(t, img)
		_, err := WriteBundle(context.Background(), &Source{Platform: "linux/arm64"},
			[]BundleImage{{Role: "limbo", Name: "r.example:5000/felis/limbo:demo", Layout: layout}}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("layer swapped for %q: WriteBundle = %v, want %q", swapped, err, want)
		}
	}
}

func TestBundleRefusesAnUnpinnedSource(t *testing.T) {
	src := newFakeSource(t)
	src.addIndex("2.8.3")
	_, err := WriteBundle(context.Background(), &Source{Scheme: "http", Platform: "linux/arm64"},
		[]BundleImage{{Role: "registry", Name: "x", Source: src.host() + "/tools/thing:2.8.3"}}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not pinned by digest") {
		t.Fatalf("WriteBundle = %v, want a tag-only source refused", err)
	}
}

func TestPushLayoutNamesWhatTheBundleHolds(t *testing.T) {
	lb := newLayoutBuilder()
	img := lb.image("arm64", "layer")
	img.Annotations = map[string]string{annotationImageName: "r.example:5000/felis/limbo:demo"}
	layout := lb.write(t, img)
	_, host := startStack(t)
	p := &Pusher{Scheme: "http", Username: registrygate.PrincipalPlatform, Password: "plat-secret", Attempts: 1}
	_, err := p.PushLayout(context.Background(), layout, "r.example:5000/felis/lobby:demo", host+"/felis/lobby:demo")
	if err == nil || !strings.Contains(err.Error(), "holds no image named r.example:5000/felis/lobby:demo (it holds: r.example:5000/felis/limbo:demo)") {
		t.Fatalf("PushLayout = %v, want the missing name reported with what is there", err)
	}
}

func TestPinnedName(t *testing.T) {
	for in, want := range map[string]string{
		"docker.io/library/registry:2.8.3@sha256:ab": "docker.io/library/registry@sha256:ab",
		"host:5000/a/b:tag@sha256:cd":                "host:5000/a/b@sha256:cd",
		"host:5000/a/b@sha256:ef":                    "host:5000/a/b@sha256:ef",
	} {
		if got := PinnedName(in); got != want {
			t.Errorf("PinnedName(%q) = %q, want %q", in, got, want)
		}
	}
}
