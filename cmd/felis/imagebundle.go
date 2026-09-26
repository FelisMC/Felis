package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"felis.lolicon.best/internal/imagepush"
)

// cmdImageBundle writes a release's image bundle: one OCI layout tar holding every
// image an install runs, for one platform, plus its listing (one "role name
// manifest-digest config-digest" line per image). deploy/build-release-artifacts.sh
// runs it in CI; deploy/bootstrap.sh imports the tar into k3s's containerd and
// pushes it into the platform registry with push-image --image.
//
//	--layout role=name=path   an image buildx wrote with --output type=oci
//	--pull role=ref           a digest-pinned public image, named repository@digest
//
// The tar and the listing are written beside their final paths and renamed into
// place, so a failed run leaves neither behind.
func cmdImageBundle(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("image-bundle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	platform := fs.String("platform", "", "os/arch the bundle is for, e.g. linux/arm64")
	out := fs.String("out", "", "path of the bundle tar to write")
	list := fs.String("list", "", "path of the listing to write")
	var images []imagepush.BundleImage
	fs.Func("layout", "role=name=path of an OCI layout tar (repeatable)", func(v string) error {
		role, rest, ok := strings.Cut(v, "=")
		name, path, ok2 := strings.Cut(rest, "=")
		if !ok || !ok2 || role == "" || name == "" || path == "" {
			return fmt.Errorf("want role=name=path, got %q", v)
		}
		images = append(images, imagepush.BundleImage{Role: role, Name: name, Layout: path})
		return nil
	})
	fs.Func("pull", "role=ref of a digest-pinned public image (repeatable)", func(v string) error {
		role, ref, ok := strings.Cut(v, "=")
		if !ok || role == "" || !strings.Contains(ref, "@sha256:") {
			return fmt.Errorf("want role=ref with ref pinned by digest, got %q", v)
		}
		images = append(images, imagepush.BundleImage{Role: role, Name: imagepush.PinnedName(ref), Source: ref})
		return nil
	})
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *platform == "" || *out == "" || *list == "" || len(images) == 0 {
		fmt.Fprintln(stderr, "felis image-bundle: --platform, --out, --list and at least one --layout or --pull are required")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := writeImageBundle(ctx, &imagepush.Source{Platform: *platform}, images, *out, *list); err != nil {
		fmt.Fprintf(stderr, "felis image-bundle: %v\n", err)
		return 1
	}
	return 0
}

func writeImageBundle(ctx context.Context, s *imagepush.Source, images []imagepush.BundleImage, out, list string) (err error) {
	tmpOut, tmpList := out+".tmp", list+".tmp"
	defer func() {
		if err != nil {
			os.Remove(tmpOut)
			os.Remove(tmpList)
		}
	}()
	f, err := os.Create(tmpOut)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	entries, err := imagepush.WriteBundle(ctx, s, images, w)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s %s %s\n", e.Role, e.Name, e.Digest, e.Config)
	}
	if err := os.WriteFile(tmpList, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpOut, out); err != nil {
		return err
	}
	return os.Rename(tmpList, list)
}
