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

// Filter runs on exported data only; info=nil means an archive entry without live file identity, and backups remain complete on-platform copies.
type Filter func(name string, info fs.FileInfo) (withhold bool, rewrite func([]byte) []byte)

// Files exceeding maxRewrite are withheld rather than emitted without secret redaction.
const maxRewrite = 1 << 20

func WriteTarGz(ctx context.Context, w io.Writer, srcDir string, filter Filter) (skipped, withheld []string, err error) {
	st, err := writeTarGz(ctx, w, srcDir, filter)
	return st.skipped, st.withheld, err
}

// FilterTarGz consumes through source EOF before closing output so digest or trailer failures leave an incomplete download.
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
