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

// The codes an unzip refuses an archive with. Each names what is wrong with the
// ARCHIVE, so the panel can say "this zip is broken" rather than "your path is
// wrong"; Result.Entry names the entry.
const (
	// CodeArchiveInvalid is a file that is not a zip, a damaged one, one whose
	// entry bytes disagree with their header (size or CRC), or one with nothing in it.
	CodeArchiveInvalid = "archive_invalid"
	// CodeArchiveUnsafe is an entry naming a path outside the folder it is
	// extracted into ("../", "/etc/x", "C:\x"), or a device, pipe or socket.
	CodeArchiveUnsafe = "archive_unsafe"
	// CodeArchiveSymlink is an entry that is a symbolic link. A link extracted into
	// the world could point anywhere, and every later op would have to reason about
	// it, so an archive carrying one is refused whole.
	CodeArchiveSymlink = "archive_symlink"
	// CodeTypeConflict is an archive with a file where the server has a folder, a
	// folder where it has a file, or anything where it has a link. Overwrite never
	// resolves it: replacing a folder with a file would delete the folder.
	CodeTypeConflict = "type_conflict"
)

// MaxConflicts bounds Result.Conflicts, keeping the result line bounded when an
// archive would replace a whole world; ConflictCount still says how many there
// are.
const MaxConflicts = 200

// unzipEntryOverhead is what the space check adds per entry for the inode and
// directory block it takes beyond its bytes.
const unzipEntryOverhead = 4096

// unzipTempPrefix names the working folder an unzip extracts into. It sits in
// the destination folder, so every move out of it is a rename on one volume.
const unzipTempPrefix = ".felis-unzip-"

// unzip extracts the .zip at name into the folder holding it.
//
// It is all or nothing. Every check that can refuse the archive runs before a
// byte is written: entry names, entry types, what is already on the server, and
// the room on the volume. The entries are then extracted into a working folder
// beside the destination, and only once every one of them has been written and
// verified are they renamed into place. A failure at any point before that
// leaves the destination exactly as it was. The renames themselves are journaled
// and undone in reverse if one fails, so a replaced file comes back. Only a Job
// killed in the middle of the renames — a few milliseconds for thousands of
// files — can leave the archive half applied.
//
// An existing file the archive would replace is a conflict: without overwrite the
// unzip lists them (CodeExists, Conflicts) and changes nothing; with it they are
// replaced. Folders merge. A file where the server has a folder, or the reverse,
// is CodeTypeConflict whatever overwrite says.
//
// The only size bound is the volume. Each entry's declared size is summed and
// checked against the free space up front, and archive/zip itself refuses an
// entry whose bytes run past its declared size or fail its CRC, so an archive
// that lies about its sizes (a zip bomb) stops at the first lying entry and
// nothing it wrote survives.
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
	// ErrInsecurePath comes back WITH a usable reader, and only under
	// GODEBUG=zipinsecurepath=0; planUnzip does that check itself, for every
	// entry, whatever the setting.
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
		count := len(list)
		if count > MaxConflicts {
			list = list[:MaxConflicts]
		}
		return Result{Code: CodeExists, Conflicts: list, ConflictCount: count, Error: fmt.Sprintf(
			"%d files in the archive already exist on the server; extract again with overwrite to replace them", count)}
	}

	// A working folder left by an unzip that was killed holds only a copy, and
	// clearing it first gives its room back to the check below.
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
	// After a success tmp holds only the files the archive replaced; after a
	// failure, everything the archive wrote. Either way it goes.
	defer func() { _ = r.RemoveAll(tmp) }()

	if res := extractAll(r, staged, p, progress); res.Code != "" {
		return res
	}
	if res := placeAll(r, dest, staged, old, p, present, replaced); res.Code != "" {
		return res
	}
	return Result{Files: len(p.files), Bytes: p.bytes}
}

// unzipPlan is an archive's entries once every one has passed planUnzip. Names
// are cleaned, slash-separated and relative to the destination folder.
type unzipPlan struct {
	files []zipFile
	// dirs is every folder the archive makes, named in it or implied by a file
	// inside it, sorted so a folder comes before everything in it.
	dirs  []string
	isDir map[string]bool
	bytes int64 // the declared size of every file, summed
}

type zipFile struct {
	f    *zip.File
	name string
	mode fs.FileMode
}

// planUnzip checks every entry's name and type and works out what the archive
// makes. Nothing about the server is consulted yet.
func planUnzip(entries []*zip.File) (unzipPlan, Result) {
	p := unzipPlan{isDir: map[string]bool{}}
	byName := map[string]bool{}
	for _, f := range entries {
		raw := entryName(f)
		name, ok := cleanEntry(raw)
		if !ok {
			return p, Result{Code: CodeArchiveUnsafe, Entry: raw, Error: fmt.Sprintf(
				"%s leads outside the folder it would be extracted into", raw)}
		}
		// macOS's Finder adds __MACOSX/ to every zip it makes: resource forks that
		// mean nothing on the server.
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
		case byName[name]:
			return p, Result{Code: CodeArchiveInvalid, Entry: raw, Error: fmt.Sprintf(
				"%s appears in the archive twice", raw)}
		case f.UncompressedSize64 > uint64(math.MaxInt64-p.bytes):
			return p, Result{Code: CodeArchiveInvalid, Entry: raw, Error: fmt.Sprintf(
				"%s declares an impossible size", raw)}
		}
		byName[name] = true
		p.bytes += int64(f.UncompressedSize64)
		// Anything the archive marks executable (a start.sh) stays executable;
		// every other permission is the server's usual.
		perm := fs.FileMode(0o644)
		if mode.Perm()&0o111 != 0 {
			perm = 0o755
		}
		p.files = append(p.files, zipFile{f: f, name: name, mode: perm})
	}
	for _, zf := range p.files {
		// cleanEntry already refused a rooted name; stopping at "/" as well keeps
		// this loop finite should that check ever move.
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
	// A folder's name is a prefix of everything in it, and a prefix sorts first.
	sort.Strings(p.dirs)
	sort.Slice(p.files, func(i, j int) bool { return p.files[i].name < p.files[j].name })
	return p, Result{}
}

// entryName is an entry's name as its maker meant it. A zip made on Chinese
// Windows stores names in the system code page (GBK) without the UTF-8 flag; a
// name that is not valid UTF-8 is decoded as GB18030, GBK's superset. The flag
// alone is no guide: archive/zip reports NonUTF8 for every name made without it,
// which includes plain ASCII and macOS's UTF-8. Decoding comes before the
// backslash below because a GBK trail byte can be 0x5C.
func entryName(f *zip.File) string {
	name := f.Name
	if !utf8.ValidString(name) {
		if s, err := simplifiedchinese.GB18030.NewDecoder().String(name); err == nil {
			name = s
		}
	}
	return strings.ReplaceAll(name, `\`, "/")
}

// cleanEntry cleans an entry name and reports false for one that must not be
// extracted: a NUL, an absolute path, a drive letter, or a climb out of the
// destination. It judges the name as TEXT, which is sound here because nothing
// is resolved through it until checkTargets and extraction, and those go through
// os.Root, which would refuse an escape anyway.
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

// checkTargets looks at what the server already has where the archive lands.
// present is the archive's folders that exist on the server (they merge);
// replaced is its files that do (conflicts). A folder the server lacks cannot
// hold anything, so its contents are not looked up.
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

// extractAll writes every folder and file of p under staged, handed to the game
// uid, and syncs each file.
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

// placeAll renames the extracted tree into dest. A folder the server lacks moves
// whole; one it has is descended into. A file it has is first moved aside into
// old, so undoing the journal puts it back.
func placeAll(r *os.Root, dest, staged, old string, p unzipPlan, present, replaced map[string]bool) Result {
	kids := map[string][]string{}
	for _, d := range p.dirs {
		kids[path.Dir(d)] = append(kids[path.Dir(d)], d)
	}
	for _, zf := range p.files {
		kids[path.Dir(zf.name)] = append(kids[path.Dir(zf.name)], zf.name)
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

// renameEntry is the rename placeAll moves with. A var so a test can fail one
// part-way through and watch the journal undo the rest.
var renameEntry = func(r *os.Root, from, to string) error { return r.Rename(from, to) }

// sweepUnzipTemps removes working folders a killed unzip left in dir.
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

// unzipWriteFailure is writeFailure worded for an unzip, which by then has
// changed nothing whatever the step that failed.
func unzipWriteFailure(err error, name string) Result {
	res := writeFailure(err, name)
	if res.Code == CodeNoSpace {
		res.Error = "the server's volume filled up while extracting; nothing was changed"
	}
	return res
}

// archiveError marks a failure reading an entry's bytes out of the archive, so
// extractOne can tell a damaged archive from the volume failing underneath.
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

// countingWriter reports each write's length to add.
type countingWriter struct {
	w   io.Writer
	add func(int)
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.add(n)
	return n, err
}
