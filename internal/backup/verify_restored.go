package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

// VerifyRestored reads target files back and compares them with every regular archive entry.
// OpenRoot keeps a malicious path or existing symlink inside the world volume.
func VerifyRestored(ctx context.Context, ref, world string) error {
	root, err := os.OpenRoot(world)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := os.Open(ref)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			continue
		}
		dst, err := root.Open(h.Name)
		if err != nil {
			return err
		}
		st, err := dst.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Size() != h.Size {
			dst.Close()
			return fmt.Errorf("restored entry %q has the wrong type or size", h.Name)
		}
		sourceHash, targetHash := sha256.New(), sha256.New()
		_, err = io.Copy(sourceHash, tr)
		if err == nil {
			_, err = io.Copy(targetHash, dst)
		}
		dst.Close()
		if err != nil {
			return err
		}
		if string(sourceHash.Sum(nil)) != string(targetHash.Sum(nil)) {
			return fmt.Errorf("restored entry %q failed SHA-256 read-back", h.Name)
		}
	}
}
