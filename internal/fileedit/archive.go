package fileedit

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"strings"
)

type archiveEntry struct {
	name    string
	archive bool
	mode    fs.FileMode
	size    int64
	open    func() (io.ReadCloser, error)
}

type archiveWalk func(func(archiveEntry) Result) Result

func zipEntries(files []*zip.File) archiveWalk {
	return func(visit func(archiveEntry) Result) Result {
		for _, f := range files {
			if f.UncompressedSize64 > math.MaxInt64 {
				return Result{Code: CodeArchiveInvalid, Entry: f.Name, Error: "archive entry declares an impossible size"}
			}
			entry := archiveEntry{name: entryName(f), archive: true, mode: f.Mode(), size: int64(f.UncompressedSize64), open: f.Open}
			if strings.HasSuffix(entry.name, "/") {
				entry.mode |= fs.ModeDir
			}
			if res := visit(entry); res.Code != "" {
				return res
			}
		}
		return Result{}
	}
}

func openArchive(f *os.File, name string, size int64) (archiveWalk, Result) {
	name = strings.ToLower(name)
	if strings.HasSuffix(name, ".zip") {
		reader, err := zip.NewReader(f, size)
		if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
			return nil, Result{Code: CodeArchiveInvalid, Error: err.Error()}
		}
		return zipEntries(reader.File), Result{}
	}
	if !strings.HasSuffix(name, ".tar") && !strings.HasSuffix(name, ".tar.gz") && !strings.HasSuffix(name, ".tgz") {
		return nil, Result{Code: CodeBadPath, Error: "supported archives are zip, tar, tar.gz and tgz"}
	}
	return func(visit func(archiveEntry) Result) Result {
		invalid := func(err error) Result {
			return Result{Code: CodeArchiveInvalid, Error: fmt.Sprintf("cannot read archive: %v", err)}
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return invalid(err)
		}
		var source io.Reader = f
		var compressed *gzip.Reader
		if !strings.HasSuffix(name, ".tar") {
			var err error
			compressed, err = gzip.NewReader(f)
			if err != nil {
				return invalid(err)
			}
			defer compressed.Close()
			source = compressed
		}
		reader := tar.NewReader(source)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				if compressed != nil {
					if _, err := io.Copy(io.Discard, compressed); err != nil {
						return invalid(err)
					}
				}
				return Result{}
			}
			if err != nil {
				return invalid(err)
			}
			if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
				return Result{Code: CodeArchiveSymlink, Entry: header.Name, Error: "archives containing links are not extracted"}
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
				return Result{Code: CodeArchiveUnsafe, Entry: header.Name, Error: "archive contains a special file"}
			}
			entry := archiveEntry{name: strings.ReplaceAll(header.Name, `\`, "/"), archive: true, mode: header.FileInfo().Mode(), size: header.Size, open: func() (io.ReadCloser, error) { return io.NopCloser(reader), nil }}
			if res := visit(entry); res.Code != "" {
				return res
			}
		}
	}, Result{}
}
