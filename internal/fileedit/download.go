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

// Content types of a download: a file goes out as its bytes, a folder as a zip.
const (
	DownloadFileType = "application/octet-stream"
	DownloadZipType  = "application/zip"
)

// Download is one file or folder of a world on its way to the owner's browser,
// run by the export Job (cmd/felis export --mode files). It passes the same
// guards a read does (Guard): the forwarding-secret file never leaves, and
// server.properties leaves with its RCON password redacted.
type Download struct {
	// Size is a file download's exact length, or -1 for a folder, whose zip is
	// written as it streams.
	Size        int64
	ContentType string
	// Skipped and Withheld count, once WriteTo has run, the entries a folder
	// download left out: links, devices and sockets, and guarded files.
	Skipped, Withheld int

	root  *os.Root
	name  string
	file  *os.File // a file download
	body  []byte   // a redacted file download
	guard Guard
}

// OpenDownload opens name under rootPath for download. dir is what the caller
// saw at name when it asked (the panel's listing): a download of a file that has
// since become a folder, or the reverse, is refused rather than sent as the
// other thing. The world root itself is refused; the world export sends that.
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

// Close releases what OpenDownload opened.
func (d *Download) Close() error {
	if d.file != nil {
		d.file.Close()
	}
	return d.root.Close()
}

// WriteTo writes the download to w: exactly Size bytes of a file, or a zip of a
// folder whose entries sit under the folder's own name, so unpacking it makes
// that one folder. A file that shrank since it was opened is an error, never a
// short download passed off as whole.
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

// writeZip streams the folder as a zip. Everything is deflated at the fastest
// level: the Job has one CPU and the owner's connection is the slower end, and
// already-compressed files (jars, region files) come out as stored blocks
// without costing much. Links, devices and sockets are left out, like a world
// export leaves them out.
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

// ctxReader stops a long copy once ctx is done.
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
