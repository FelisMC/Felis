package fileedit

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
)

const (
	DownloadFileType = "application/octet-stream"
	DownloadZipType  = "application/zip"
)

// Download uses the same secret-file denial and properties redaction as reads.
type Download struct {
	// Size=-1 denotes a folder whose zip length is unknown until streamed.
	Size        int64
	ContentType string
	// Skipped and Withheld are populated only after WriteTo completes.
	Skipped, Withheld int

	root  *os.Root
	name  string
	file  *os.File
	body  []byte
	guard Guard
}

// OpenDownload refuses changed file-versus-folder types and the world root rather than silently downloading different content.
func OpenDownload(rootPath, name string, dir bool) (*Download, error) {
	name = path.Clean(name)
	if name == "." || name == "/" || !fs.ValidPath(name) {
		return nil, fmt.Errorf("%s is not a file or folder inside the world", name)
	}
	r, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open the world root: %w", err)
	}
	d := &Download{root: r, name: name, guard: NewGuard(r)}
	if err := d.open(dir); err != nil {
		r.Close()
		return nil, err
	}
	return d, nil
}

func (d *Download) open(dir bool) error {
	info, err := d.root.Stat(d.name)
	if err != nil {
		return err
	}
	if info.IsDir() != dir {
		if info.IsDir() {
			return fmt.Errorf("%s is a folder now; reload the file list and download it again", d.name)
		}
		return fmt.Errorf("%s is not a folder now; reload the file list and download it again", d.name)
	}
	if dir {
		d.Size, d.ContentType = -1, DownloadZipType
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", d.name)
	}
	withhold, redact := d.guard.Rule(info)
	if withhold {
		return fmt.Errorf("%s is the file holding the proxy forwarding secret, which is shared cluster-wide, and cannot be downloaded", d.name)
	}
	f, err := d.root.Open(d.name)
	if err != nil {
		return err
	}
	d.ContentType = DownloadFileType
	if !redact {
		d.file, d.Size = f, info.Size()
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxReadBytes+1))
	if err != nil {
		return err
	}
	if len(b) > MaxReadBytes {
		return fmt.Errorf("%s is over %d bytes and cannot be redacted for download", d.name, MaxReadBytes)
	}
	d.body = RedactProps(b)
	d.Size = int64(len(d.body))
	return nil
}

func (d *Download) Close() error {
	if d.file != nil {
		d.file.Close()
	}
	return d.root.Close()
}

// WriteTo treats shrinking files as errors; folder entries remain under one top-level folder.
func (d *Download) WriteTo(ctx context.Context, w io.Writer) error {
	switch {
	case d.body != nil:
		_, err := w.Write(d.body)
		return err
	case d.file != nil:
		_, err := io.CopyN(w, ctxReader{ctx, d.file}, d.Size)
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s shrank while it was being downloaded", d.name)
		}
		return err
	}
	return d.writeZip(ctx, w)
}

// writeZip uses fast deflate and excludes links, devices and sockets.
func (d *Download) writeZip(ctx context.Context, w io.Writer) error {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	base := path.Base(d.name)
	err := fs.WalkDir(d.root.FS(), d.name, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := base + p[len(d.name):]
		info, err := de.Info()
		if err != nil {
			return err
		}
		switch {
		case de.IsDir():
			hdr := &zip.FileHeader{Name: entry + "/", Modified: info.ModTime()}
			hdr.SetMode(info.Mode().Perm() | fs.ModeDir)
			_, err := zw.CreateHeader(hdr)
			return err
		case !de.Type().IsRegular():
			d.Skipped++
			return nil
		}
		withhold, redact := d.guard.Rule(info)
		if withhold {
			d.Withheld++
			return nil
		}
		f, err := d.root.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		var src io.Reader = ctxReader{ctx, f}
		if redact {
			b, err := io.ReadAll(io.LimitReader(f, MaxReadBytes+1))
			if err != nil {
				return err
			}
			if len(b) > MaxReadBytes {
				d.Withheld++
				return nil
			}
			src = bytes.NewReader(RedactProps(b))
		}
		hdr := &zip.FileHeader{Name: entry, Method: zip.Deflate, Modified: info.ModTime()}
		hdr.SetMode(info.Mode().Perm())
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, src)
		return err
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
