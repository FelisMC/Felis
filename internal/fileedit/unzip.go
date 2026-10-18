package fileedit

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	CodeArchiveInvalid = "archive_invalid"
	CodeArchiveUnsafe  = "archive_unsafe"
	// CodeArchiveSymlink rejects the whole archive rather than extracting links with unverifiable future targets.
	CodeArchiveSymlink = "archive_symlink"
	// CodeTypeConflict cannot be overridden because replacing a folder or link could destroy unrelated data.
	CodeTypeConflict = "type_conflict"
)

// Bound conflict count and name bytes so runtime log splitting cannot push ResultPrefix outside the retained log tail.
const (
	MaxConflicts     = 200
	maxConflictBytes = 8 << 10
)

// unzipEntryOverhead estimates inode and directory-block space beyond payload bytes.
const unzipEntryOverhead = 4096

// unzipTempPrefix stays beside the destination so final moves remain same-volume renames.
const unzipTempPrefix = ".felis-unzip-"

// unzip stages and verifies before journaled renames; failures roll back, but termination during placement may leave a partial application.
func unzip(r *os.Root, rootPath, name string, overwrite bool, progress func(done, total int64)) Result {
	name = path.Clean(name)
	if !strings.EqualFold(path.Ext(name), ".zip") {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf("%s is not a .zip archive", name)}
	}
	f, err := r.Open(name)
	if err != nil {
		return failure(err, name)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return failure(err, name)
	}
	if !info.Mode().IsRegular() {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf("%s is not a file", name)}
	}
	// ErrInsecurePath can include a usable reader; planUnzip enforces path checks independently of GODEBUG.
	zr, err := zip.NewReader(f, info.Size())
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return Result{Code: CodeArchiveInvalid, Error: fmt.Sprintf("%s is not a readable zip archive: %v", name, err)}
	}
	p, res := planUnzip(zr.File)
	if res.Code != "" {
		return res
	}

	dest := path.Dir(name)
	present, replaced, res := checkTargets(r, dest, p)
	if res.Code != "" {
		return res
	}
	if len(replaced) > 0 && !overwrite {
		list := make([]string, 0, len(replaced))
		for n := range replaced {
			list = append(list, path.Join(dest, n))
		}
		sort.Strings(list)
		count, n, size := len(list), 0, 0
		for n < count && n < MaxConflicts && size+len(list[n]) <= maxConflictBytes {
			size += len(list[n])
			n++
		}
		list = list[:n]
		return Result{Code: CodeExists, Conflicts: list, ConflictCount: count, Error: fmt.Sprintf(
			"%d files in the archive already exist on the server; extract again with overwrite to replace them", count)}
	}

	// Clear abandoned staging copies before measuring free space.
	sweepUnzipTemps(r, dest)
	need := p.bytes + int64(len(p.files)+len(p.dirs))*unzipEntryOverhead
	if avail, _, err := statfs(rootPath); err == nil && uint64(need) > avail {
		free := int64(math.MaxInt64)
		if avail < math.MaxInt64 {
			free = int64(avail)
		}
		return Result{Code: CodeNoSpace, Need: need, Avail: free, Error: fmt.Sprintf(
			"extracting %s needs %d bytes and the server's volume has %d free; nothing was changed", name, need, free)}
	}

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf("generate a temporary name: %v", err)}
	}
	tmp := path.Join(dest, unzipTempPrefix+hex.EncodeToString(suffix[:]))
	staged, old := path.Join(tmp, "new"), path.Join(tmp, "old")
	for _, d := range []string{tmp, staged, old} {
		if err := r.Mkdir(d, 0o700); err != nil {
			_ = r.RemoveAll(tmp)
			return unzipWriteFailure(err, dest)
		}
	}
	defer func() { _ = r.RemoveAll(tmp) }()

	if res := extractAll(r, staged, p, progress); res.Code != "" {
		return res
	}
	if res := placeAll(r, dest, staged, old, p, present, replaced); res.Code != "" {
		return res
	}
	return Result{Files: len(p.files), Bytes: p.bytes}
}

type unzipPlan struct {
	files []zipFile
	// dirs sorts parents before their contents, including implied directories.
	dirs  []string
	isDir map[string]bool
	bytes int64
}

type zipFile struct {
	f    *zip.File
	name string
	mode fs.FileMode
}

func planUnzip(entries []*zip.File) (unzipPlan, Result) {
	p := unzipPlan{isDir: map[string]bool{}}
	for _, f := range entries {
		raw := entryName(f)
		name, ok := cleanEntry(raw)
		if !ok {
			return p, Result{Code: CodeArchiveUnsafe, Entry: raw, Error: fmt.Sprintf(
				"%s leads outside the folder it would be extracted into", raw)}
		}
		if name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		mode := f.Mode()
		isDir := mode.IsDir() || strings.HasSuffix(raw, "/")
		switch {
		case mode&fs.ModeSymlink != 0:
			return p, Result{Code: CodeArchiveSymlink, Entry: raw, Error: fmt.Sprintf(
				"%s is a symbolic link; an archive containing links is not extracted", raw)}
		case isDir:
			if name != "." {
				p.isDir[name] = true
			}
			continue
		case !mode.IsRegular():
			return p, Result{Code: CodeArchiveUnsafe, Entry: raw, Error: fmt.Sprintf(
				"%s is not a regular file", raw)}
		case name == ".":
			return p, Result{Code: CodeArchiveUnsafe, Entry: raw, Error: fmt.Sprintf(
				"%s names the destination folder itself", raw)}
		case f.UncompressedSize64 > uint64(math.MaxInt64-p.bytes):
			return p, Result{Code: CodeArchiveInvalid, Entry: raw, Error: fmt.Sprintf(
				"%s declares an impossible size", raw)}
		}
		p.bytes += int64(f.UncompressedSize64)
		// Preserve executable bits while using normal server permissions for other modes.
		perm := fs.FileMode(0o644)
		if mode.Perm()&0o111 != 0 {
			perm = 0o755
		}
		p.files = append(p.files, zipFile{f: f, name: name, mode: perm})
	}
	// Sorted duplicates are adjacent, avoiding a second entry-sized set.
	sort.Slice(p.files, func(i, j int) bool { return p.files[i].name < p.files[j].name })
	for i := 1; i < len(p.files); i++ {
		if p.files[i].name == p.files[i-1].name {
			return p, Result{Code: CodeArchiveInvalid, Entry: p.files[i].name, Error: fmt.Sprintf(
				"%s appears in the archive twice", p.files[i].name)}
		}
	}
	for _, zf := range p.files {
		for d := path.Dir(zf.name); d != "." && d != "/"; d = path.Dir(d) {
			p.isDir[d] = true
		}
	}
	for _, zf := range p.files {
		if p.isDir[zf.name] {
			return p, Result{Code: CodeArchiveInvalid, Entry: zf.name, Error: fmt.Sprintf(
				"%s is both a file and a folder in the archive", zf.name)}
		}
	}
	if len(p.files) == 0 && len(p.isDir) == 0 {
		return p, Result{Code: CodeArchiveInvalid, Error: "the archive has nothing to extract"}
	}
	for d := range p.isDir {
		p.dirs = append(p.dirs, d)
	}
	sort.Strings(p.dirs)
	return p, Result{}
}

// entryName decodes invalid UTF-8 as GB18030 before converting backslashes because GBK trail bytes can be 0x5C.
func entryName(f *zip.File) string {
	name := f.Name
	if !utf8.ValidString(name) {
		if s, err := simplifiedchinese.GB18030.NewDecoder().String(name); err == nil {
			name = s
		}
	}
	return strings.ReplaceAll(name, `\`, "/")
}

// cleanEntry rejects textual escapes; extraction additionally enforces containment through os.Root.
func cleanEntry(raw string) (string, bool) {
	if raw == "" || strings.ContainsRune(raw, 0) || strings.HasPrefix(raw, "/") {
		return "", false
	}
	if len(raw) >= 2 && raw[1] == ':' && (raw[0]|0x20) >= 'a' && (raw[0]|0x20) <= 'z' {
		return "", false
	}
	name := path.Clean(raw)
	if name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, true
}

func checkTargets(r *os.Root, dest string, p unzipPlan) (present, replaced map[string]bool, res Result) {
	present, replaced = map[string]bool{}, map[string]bool{}
	absent := func(name string) bool {
		parent := path.Dir(name)
		return parent != "." && !present[parent]
	}
	for _, d := range p.dirs {
		if absent(d) {
			continue
		}
		at := path.Join(dest, d)
		info, err := r.Lstat(at)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, nil, failure(err, at)
		case info.Mode()&fs.ModeSymlink != 0:
			return nil, nil, typeConflict(at, "is a link on the server, and the archive has a folder there")
		case info.IsDir():
			present[d] = true
		default:
			return nil, nil, typeConflict(at, "is a file on the server, and the archive has a folder there")
		}
	}
	for _, zf := range p.files {
		if absent(zf.name) {
			continue
		}
		at := path.Join(dest, zf.name)
		info, err := r.Lstat(at)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, nil, failure(err, at)
		case info.Mode().IsRegular():
			replaced[zf.name] = true
		case info.IsDir():
			return nil, nil, typeConflict(at, "is a folder on the server, and the archive has a file there")
		default:
			return nil, nil, typeConflict(at, "is a link or special file on the server, and the archive has a file there")
		}
	}
	return present, replaced, Result{}
}

func typeConflict(at, why string) Result {
	return Result{Code: CodeTypeConflict, Entry: at, Error: fmt.Sprintf("%s %s; nothing was changed", at, why)}
}

func extractAll(r *os.Root, staged string, p unzipPlan, progress func(done, total int64)) Result {
	for _, d := range p.dirs {
		at := path.Join(staged, d)
		if err := r.Mkdir(at, 0o755); err != nil {
			return unzipWriteFailure(err, d)
		}
		_ = ownWritten(r, at)
	}
	var done int64
	count := func(n int) {
		done += int64(n)
		if progress != nil {
			progress(done, p.bytes)
		}
	}
	for _, zf := range p.files {
		if res := extractOne(r, path.Join(staged, zf.name), zf, count); res.Code != "" {
			return res
		}
	}
	for _, d := range p.dirs {
		syncDir(r, path.Join(staged, d))
	}
	syncDir(r, staged)
	return Result{}
}

func extractOne(r *os.Root, at string, zf zipFile, count func(int)) Result {
	invalid := func(err error) Result {
		return Result{Code: CodeArchiveInvalid, Entry: zf.name, Error: fmt.Sprintf(
			"%s in the archive is damaged: %v; nothing was changed", zf.name, err)}
	}
	src, err := zf.f.Open()
	if err != nil {
		return invalid(err)
	}
	defer src.Close()
	w, err := r.OpenFile(at, os.O_WRONLY|os.O_CREATE|os.O_EXCL, zf.mode)
	if err != nil {
		return unzipWriteFailure(err, zf.name)
	}
	if err := w.Chmod(zf.mode); err != nil {
		w.Close()
		return unzipWriteFailure(err, zf.name)
	}
	if _, err := io.Copy(countingWriter{w, count}, archiveReader{src}); err != nil {
		w.Close()
		var ae *archiveError
		if errors.As(err, &ae) {
			return invalid(ae.err)
		}
		return unzipWriteFailure(err, zf.name)
	}
	if err := syncWritten(w); err != nil {
		w.Close()
		return unzipWriteFailure(err, zf.name)
	}
	if err := w.Close(); err != nil {
		return unzipWriteFailure(err, zf.name)
	}
	_ = ownWritten(r, at)
	return Result{}
}

// placeAll moves existing files aside before replacing them so rollback can restore them.
func placeAll(r *os.Root, dest, staged, old string, p unzipPlan, present, replaced map[string]bool) Result {
	kids := map[string][]string{}
	add := func(name string) {
		if parent := path.Dir(name); parent == "." || present[parent] {
			kids[parent] = append(kids[parent], name)
		}
	}
	for _, d := range p.dirs {
		add(d)
	}
	for _, zf := range p.files {
		add(zf.name)
	}

	type move struct{ from, to string }
	var journal []move
	mv := func(from, to string) error {
		if err := renameEntry(r, from, to); err != nil {
			return err
		}
		journal = append(journal, move{from, to})
		return nil
	}
	var place func(dir string) error
	place = func(dir string) error {
		for _, c := range kids[dir] {
			src, dst := path.Join(staged, c), path.Join(dest, c)
			switch {
			case p.isDir[c] && present[c]:
				if err := place(c); err != nil {
					return err
				}
			case replaced[c]:
				aside := path.Join(old, c)
				if err := r.MkdirAll(path.Dir(aside), 0o700); err != nil {
					return err
				}
				if err := mv(dst, aside); err != nil {
					return err
				}
				if err := mv(src, dst); err != nil {
					return err
				}
			default:
				if err := mv(src, dst); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := place("."); err != nil {
		for i := len(journal) - 1; i >= 0; i-- {
			_ = r.Rename(journal[i].to, journal[i].from)
		}
		return unzipWriteFailure(err, dest)
	}
	syncDir(r, dest)
	for d := range present {
		syncDir(r, path.Join(dest, d))
	}
	return Result{}
}

var renameEntry = func(r *os.Root, from, to string) error { return r.Rename(from, to) }

func sweepUnzipTemps(r *os.Root, dir string) {
	d, err := r.Open(dir)
	if err != nil {
		return
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	for _, n := range names {
		if isUnzipTemp(n) {
			_ = r.RemoveAll(path.Join(dir, n))
		}
	}
}

func isUnzipTemp(name string) bool {
	suffix, ok := strings.CutPrefix(name, unzipTempPrefix)
	if !ok || len(suffix) != 12 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func unzipWriteFailure(err error, name string) Result {
	res := writeFailure(err, name)
	if res.Code == CodeNoSpace {
		res.Error = "the server's volume filled up while extracting; nothing was changed"
	}
	return res
}

// archiveError distinguishes corrupt source bytes from destination-volume failures.
type archiveError struct{ err error }

func (e *archiveError) Error() string { return "read archive: " + e.err.Error() }
func (e *archiveError) Unwrap() error { return e.err }

type archiveReader struct{ r io.Reader }

func (a archiveReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if err != nil && err != io.EOF {
		err = &archiveError{err}
	}
	return n, err
}

type countingWriter struct {
	w   io.Writer
	add func(int)
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.add(n)
	return n, err
}
