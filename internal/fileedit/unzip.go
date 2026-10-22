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
	"time"
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
	if withheld, _ := NewGuard(r).Rule(info); withheld {
		return Result{Code: CodeBadPath, Error: "protected configuration cannot be opened as an archive"}
	}
	walk, res := openArchive(f, name, info.Size())
	if res.Code != "" {
		return res
	}
	p, res := planEntries(walk)
	if res.Code != "" {
		return res
	}

	return applyPlan(r, rootPath, path.Dir(name), p, overwrite, progress)
}

func applyPlan(r *os.Root, rootPath, dest string, p unzipPlan, overwrite bool, progress func(done, total int64)) Result {
	present, replaced, res := checkTargets(r, dest, p)
	if res.Code != "" {
		return res
	}
	if len(replaced) > 0 && !overwrite {
		return conflictResult(dest, replaced)
	}

	// Clear abandoned staging copies before measuring free space.
	sweepUnzipTemps(r, dest)
	overhead := int64(len(p.files)+len(p.dirs)) * unzipEntryOverhead
	if p.bytes > math.MaxInt64-overhead {
		return Result{Code: CodeArchiveInvalid, Error: "source size is too large"}
	}
	need := p.bytes + overhead
	if avail, _, err := statfs(rootPath); err == nil && uint64(need) > avail {
		free := int64(math.MaxInt64)
		if avail < math.MaxInt64 {
			free = int64(avail)
		}
		return Result{Code: CodeNoSpace, Need: need, Avail: free, Error: fmt.Sprintf(
			"the operation needs %d bytes and the server's volume has %d free; nothing was changed", need, free)}
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
	cleanup := true
	defer func() {
		if cleanup {
			_ = r.RemoveAll(tmp)
		}
	}()

	if res := extractAll(r, staged, p, progress); res.Code != "" {
		return res
	}
	guard := NewGuard(r)
	if res, rolledBack := placeAll(r, dest, staged, old, p, present, replaced); res.Code != "" {
		if !rolledBack {
			cleanup = false
			return preservePlacement(r, dest, tmp, staged, old, hex.EncodeToString(suffix[:]), "file-recovery-", guard, res)
		}
		return res
	}
	return Result{Files: len(p.files), Bytes: p.bytes}
}

func conflictResult(dest string, replaced map[string]bool) Result {
	list := make([]string, 0, len(replaced))
	for name := range replaced {
		list = append(list, path.Join(dest, name))
	}
	sort.Strings(list)
	count, n, size := len(list), 0, 0
	for n < count && n < MaxConflicts && size+len(list[n]) <= maxConflictBytes {
		size += len(list[n])
		n++
	}
	return Result{Code: CodeExists, Conflicts: list[:n], ConflictCount: count, Error: fmt.Sprintf("%d target files already exist; confirm overwrite to replace them", count)}
}

type unzipPlan struct {
	files []zipFile
	walk  archiveWalk
	// dirs sorts parents before their contents, including implied directories.
	dirs  []string
	isDir map[string]bool
	bytes int64
}

type zipFile struct {
	name string
	mode fs.FileMode
	size int64
}

func planUnzip(entries []*zip.File) (unzipPlan, Result) {
	return planEntries(zipEntries(entries))
}

func planEntries(walk archiveWalk) (unzipPlan, Result) {
	p := unzipPlan{isDir: map[string]bool{}, walk: walk}
	res := walk(func(entry archiveEntry) Result {
		raw := entry.name
		name, ok := cleanEntry(raw)
		if !ok {
			return Result{Code: CodeArchiveUnsafe, Entry: raw, Error: fmt.Sprintf(
				"%s leads outside the folder it would be extracted into", raw)}
		}
		if entry.archive && (name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/")) {
			return Result{}
		}
		mode := entry.mode
		isDir := mode.IsDir() || strings.HasSuffix(raw, "/")
		switch {
		case mode&fs.ModeSymlink != 0:
			return Result{Code: CodeArchiveSymlink, Entry: raw, Error: fmt.Sprintf(
				"%s is a symbolic link; an archive containing links is not extracted", raw)}
		case isDir:
			if name != "." {
				p.isDir[name] = true
			}
			return Result{}
		case !mode.IsRegular():
			return Result{Code: CodeArchiveUnsafe, Entry: raw, Error: fmt.Sprintf(
				"%s is not a regular file", raw)}
		case name == ".":
			return Result{Code: CodeArchiveUnsafe, Entry: raw, Error: fmt.Sprintf(
				"%s names the destination folder itself", raw)}
		case entry.size < 0 || entry.size > math.MaxInt64-p.bytes:
			return Result{Code: CodeArchiveInvalid, Entry: raw, Error: fmt.Sprintf(
				"%s declares an impossible size", raw)}
		}
		p.bytes += entry.size
		// Preserve executable bits while using normal server permissions for other modes.
		perm := fs.FileMode(0o644)
		if mode.Perm()&0o111 != 0 {
			perm = 0o755
		}
		p.files = append(p.files, zipFile{name: name, mode: perm, size: entry.size})
		return Result{}
	})
	if res.Code != "" {
		return p, res
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
	expected := make(map[string]zipFile, len(p.files))
	for _, file := range p.files {
		expected[file.name] = file
	}
	res := p.walk(func(entry archiveEntry) Result {
		name, ok := cleanEntry(entry.name)
		if !ok {
			return Result{Code: CodeArchiveUnsafe, Entry: entry.name, Error: "source entry leads outside the destination"}
		}
		if entry.mode.IsDir() || entry.archive && (name == "__MACOSX" || strings.HasPrefix(name, "__MACOSX/")) {
			return Result{}
		}
		file, ok := expected[name]
		if !ok || entry.size != file.size {
			return Result{Code: CodeConflict, Entry: name, Error: "source changed during the operation"}
		}
		delete(expected, name)
		return extractOne(r, path.Join(staged, name), file, entry.open, count)
	})
	if res.Code != "" {
		return res
	}
	if len(expected) != 0 {
		return Result{Code: CodeConflict, Error: "source changed during the operation"}
	}
	for _, d := range p.dirs {
		syncDir(r, path.Join(staged, d))
	}
	syncDir(r, staged)
	return Result{}
}

func extractOne(r *os.Root, at string, zf zipFile, open func() (io.ReadCloser, error), count func(int)) Result {
	invalid := func(err error) Result {
		return Result{Code: CodeArchiveInvalid, Entry: zf.name, Error: fmt.Sprintf(
			"%s in the archive is damaged: %v; nothing was changed", zf.name, err)}
	}
	src, err := open()
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
	n, err := io.Copy(countingWriter{w, count}, archiveReader{io.LimitReader(src, zf.size+1)})
	if err != nil {
		w.Close()
		var ae *archiveError
		if errors.As(err, &ae) {
			return invalid(ae.err)
		}
		return unzipWriteFailure(err, zf.name)
	}
	if n != zf.size {
		w.Close()
		return invalid(fmt.Errorf("read %d bytes, expected %d", n, zf.size))
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
func placeAll(r *os.Root, dest, staged, old string, p unzipPlan, present, replaced map[string]bool) (Result, bool) {
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

	type move struct {
		from, to string
		info     fs.FileInfo
	}
	var journal []move
	mv := func(from, to string) error {
		info, err := r.Lstat(from)
		if err != nil {
			return err
		}
		if err := renameEntry(r, from, to); err != nil {
			return err
		}
		journal = append(journal, move{from: from, to: to, info: info})
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
		rolledBack := true
		for i := len(journal) - 1; i >= 0; i-- {
			entry := journal[i]
			info, statErr := r.Lstat(entry.to)
			if statErr != nil || !os.SameFile(entry.info, info) {
				rolledBack = false
				continue
			}
			if err := renameNoReplace(r, entry.to, entry.from); err != nil {
				rolledBack = false
			}
		}
		result := unzipWriteFailure(err, dest)
		if !rolledBack {
			result.Code = CodeRecovery
			result.Error = fmt.Sprintf("the operation failed (%v) and could not be fully rolled back", err)
		}
		return result, rolledBack
	}

	syncDir(r, dest)
	for d := range present {
		syncDir(r, path.Join(dest, d))
	}
	return Result{}, true
}

var renameEntry = renameNoReplace

func sweepUnzipTemps(r *os.Root, dir string) {
	d, err := r.Open(dir)
	if err != nil {
		return
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	for _, n := range names {
		if isUnzipTemp(n) && !hasEntries(r, path.Join(dir, n, "old")) {
			if _, err := r.Lstat(path.Join(dir, n, recoveryMarker)); !errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if info, err := r.Stat(path.Join(dir, n)); err == nil && time.Since(info.ModTime()) > defaultAsyncDeadline+time.Hour {
				_ = r.RemoveAll(path.Join(dir, n))
			}
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
