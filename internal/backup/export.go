package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Filter decides, for one regular file on its way out of a world into an
// export, whether it is left out (withhold) or has its bytes replaced (rewrite
// non-nil). name is the archive path; info is the file's, or nil when the file
// is an entry of a stored archive, which has only names. A backup is not
// filtered: it stays on the platform, and a restore must bring the world back
// whole.
type Filter func(name string, info fs.FileInfo) (withhold bool, rewrite func([]byte) []byte)

// maxRewrite bounds a file a Filter rewrites, which is read whole into memory.
// The files it rewrites are small configs; one larger than this is withheld
// rather than sent unrewritten.
const maxRewrite = 1 << 20

// WriteTarGz archives srcDir into w laid out exactly as Archive lays out a
// backup, so an exported world restores like any other archive. It returns the
// entries a tar cannot hold and the files filter withheld. The world export Job
// streams it straight into its upload.
func WriteTarGz(ctx context.Context, w io.Writer, srcDir string, filter Filter) (skipped, withheld []string, err error) {
	st, err := writeTarGz(ctx, w, srcDir, filter)
	return st.skipped, st.withheld, err
}

// FilterTarGz copies the gzip+tar archive read from r into w, passing every
// regular file through filter by name. It is how a stored backup leaves the
// platform: the archive is the world as it was, secrets included, so it is
// re-written on the way out rather than handed over as stored.
//
// r is read to its very end, past the tar trailer, before w's archive is
// closed. A reader that checks a digest when it reaches EOF therefore fails the
// copy while w still lacks the end of its archive, and a receiver never holds a
// complete-looking copy of a corrupt backup.
func FilterTarGz(ctx context.Context, w io.Writer, r io.Reader, filter Filter) (withheld []string, err error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("backup: open archive: %w", err)
	}
	tr := tar.NewReader(zr)
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	for {
		if err := ctx.Err(); err != nil {
			return withheld, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return withheld, fmt.Errorf("backup: read archive: %w", err)
		}
		if !hdr.FileInfo().Mode().IsRegular() {
			if err := tw.WriteHeader(hdr); err != nil {
				return withheld, err
			}
			continue
		}
		withhold, rewrite := filter(hdr.Name, nil)
		if withhold {
			withheld = append(withheld, hdr.Name)
			continue
		}
		if rewrite == nil {
			if err := tw.WriteHeader(hdr); err != nil {
				return withheld, err
			}
			if _, err := io.Copy(tw, tr); err != nil {
				return withheld, fmt.Errorf("backup: copy %s: %w", hdr.Name, err)
			}
			continue
		}
		content, err := io.ReadAll(io.LimitReader(tr, maxRewrite+1))
		if err != nil {
			return withheld, fmt.Errorf("backup: read %s: %w", hdr.Name, err)
		}
		if len(content) > maxRewrite {
			withheld = append(withheld, hdr.Name)
			continue
		}
		content = rewrite(content)
		hdr.Size = int64(len(content))
		if err := tw.WriteHeader(hdr); err != nil {
			return withheld, err
		}
		if _, err := tw.Write(content); err != nil {
			return withheld, err
		}
	}
	// Through the gzip trailer to r's EOF (see above).
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return withheld, fmt.Errorf("backup: read archive: %w", err)
	}
	if err := tw.Close(); err != nil {
		return withheld, fmt.Errorf("backup: close tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return withheld, fmt.Errorf("backup: close gzip: %w", err)
	}
	return withheld, nil
}

// readRewritable reads a file a Filter rewrites, reporting false when it is
// larger than maxRewrite.
func readRewritable(path string) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRewrite+1))
	if err != nil {
		return nil, false, err
	}
	return b, len(b) <= maxRewrite, nil
}
