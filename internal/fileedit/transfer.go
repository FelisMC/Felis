package fileedit

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

const MaxSelectedPaths = 500
const MaxSelectedPathBytes = 64 << 10

func selectedPaths(r *os.Root, names []string) ([]string, Result) {
	if len(names) == 0 || len(names) > MaxSelectedPaths {
		return nil, Result{Code: CodeBadPath, Error: "select between 1 and 500 files or folders"}
	}
	out := make([]string, 0, len(names))
	bases := map[string]bool{}
	total := 0
	for _, name := range names {
		total += len(name)
		if len(name) > 4096 || total > MaxSelectedPathBytes {
			return nil, Result{Code: CodeBadPath, Error: "selected paths are too long"}
		}
		name = path.Clean(name)
		if name == "." || !fs.ValidPath(name) || InternalPath(name) {
			return nil, Result{Code: CodeBadPath, Error: "the server root and paths outside it cannot be selected"}
		}
		if res := noLinks(r, name); res.Code != "" {
			return nil, res
		}
		if bases[path.Base(name)] {
			return nil, Result{Code: CodeBadPath, Error: "selected paths must have distinct names"}
		}
		bases[path.Base(name)] = true
		out = append(out, name)
	}
	sort.Strings(out)
	for i, name := range out {
		for _, prior := range out[:i] {
			if strings.HasPrefix(name, prior+"/") {
				return nil, Result{Code: CodeBadPath, Error: "a folder and its contents cannot both be selected"}
			}
		}
	}
	return out, Result{}
}

func noLinks(r *os.Root, name string) Result {
	for cur := name; cur != "."; cur = path.Dir(cur) {
		info, err := r.Lstat(cur)
		if err != nil {
			return failure(err, cur)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return Result{Code: CodeBadPath, Entry: cur, Error: "symbolic links cannot be used in this operation"}
		}
	}
	return Result{}
}

func sourceEntries(r *os.Root, names []string, moving bool) archiveWalk {
	return walkSources(r, names, moving, NewGuard(r))
}

func walkSources(r *os.Root, names []string, moving bool, guard Guard) archiveWalk {
	return func(visit func(archiveEntry) Result) Result {
		var res Result
		for _, name := range names {
			err := fs.WalkDir(r.FS(), name, func(at string, de fs.DirEntry, err error) error {
				if err != nil {
					res = failure(err, at)
					return fs.SkipAll
				}
				if InternalPath(path.Base(name) + at[len(name):]) {
					res = Result{Code: CodeBadPath, Error: "folders with active operation staging cannot be transferred"}
					return fs.SkipAll
				}
				info, err := de.Info()
				if err != nil {
					res = failure(err, at)
					return fs.SkipAll
				}
				if !info.IsDir() && !info.Mode().IsRegular() {
					res = Result{Code: CodeBadPath, Entry: at, Error: "only regular files and folders can be transferred"}
					return fs.SkipAll
				}
				if moving {
					if result := guardMove(r, at, info); result.Code != "" {
						res = result
						return fs.SkipAll
					}
				}
				withheld, redact := guard.Rule(info)
				currentWithheld, currentRedact := NewGuard(r).Rule(info)
				withheld, redact = withheld || currentWithheld, redact || currentRedact
				if withheld {
					res = Result{Code: CodeBadPath, Entry: at, Error: "protected configuration cannot be copied or moved"}
					return fs.SkipAll
				}
				entry := archiveEntry{name: path.Base(name) + at[len(name):], mode: info.Mode(), size: info.Size(), open: func() (io.ReadCloser, error) {
					f, err := r.Open(at)
					if err != nil {
						return nil, err
					}
					current, err := f.Stat()
					if err != nil || !os.SameFile(info, current) {
						f.Close()
						return nil, fmt.Errorf("%s changed during the operation", at)
					}
					return f, nil
				}}
				if redact {
					if moving {
						res = Result{Code: CodeBadPath, Entry: at, Error: "managed properties cannot be moved"}
						return fs.SkipAll
					}
					f, err := r.Open(at)
					if err != nil {
						res = failure(err, at)
						return fs.SkipAll
					}
					current, err := f.Stat()
					if err != nil || !os.SameFile(info, current) {
						f.Close()
						res = Result{Code: CodeConflict, Error: "source changed during the operation"}
						return fs.SkipAll
					}
					body, err := io.ReadAll(io.LimitReader(f, MaxReadBytes+1))
					f.Close()
					if err != nil {
						res = failure(err, at)
						return fs.SkipAll
					}
					if len(body) > MaxReadBytes {
						res = Result{Code: CodeTooLarge, Entry: at, Error: "managed properties are too large to redact"}
						return fs.SkipAll
					}
					body = RedactProps(body)
					entry.size, entry.open = int64(len(body)), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
				}
				res = visit(entry)
				if res.Code != "" {
					return fs.SkipAll
				}
				return nil
			})
			if err != nil {
				return failure(err, name)
			}
			if res.Code != "" {
				return res
			}
		}
		return Result{}
	}
}

func transfer(r *os.Root, rootPath string, req Request) Result {
	names, res := selectedPaths(r, req.Paths)
	if res.Code != "" {
		return res
	}
	dest := path.Clean(req.To)
	if dest != "." && !fs.ValidPath(dest) {
		return Result{Code: CodeBadPath, Error: "destination must be a folder inside the server"}
	}
	if res := noLinks(r, dest); res.Code != "" {
		return res
	}
	info, err := r.Stat(dest)
	if err != nil {
		return failure(err, dest)
	}
	if !info.IsDir() {
		return typeConflict(dest, "is not a folder")
	}
	for _, name := range names {
		if dest == name || strings.HasPrefix(dest, name+"/") || path.Join(dest, path.Base(name)) == name {
			return Result{Code: CodeBadPath, Error: "a file or folder cannot be transferred into itself"}
		}
	}
	p, res := planEntries(sourceEntries(r, names, req.Op == OpMove))
	if res.Code != "" {
		return res
	}
	if req.Op == OpCopy {
		return applyPlan(r, rootPath, dest, p, req.Overwrite, req.Progress)
	}
	return moveSelected(r, dest, names, p, req.Overwrite, req.Progress)
}

// moveSelected renames source inodes, keeping writes made during the move.
func moveSelected(r *os.Root, dest string, names []string, p unzipPlan, overwrite bool, progress func(int64, int64)) Result {
	present, replaced, res := checkTargets(r, dest, p)
	if res.Code != "" {
		return res
	}
	if len(replaced) > 0 && !overwrite {
		return conflictResult(dest, replaced)
	}
	suffix, err := randomHex(6)
	if err != nil {
		return failure(err, dest)
	}
	tmp := path.Join(dest, ".felis-move-"+suffix)
	staged, old := path.Join(tmp, "new"), path.Join(tmp, "old")
	for _, name := range []string{tmp, staged, old} {
		if err := r.Mkdir(name, 0o700); err != nil {
			_ = r.RemoveAll(tmp)
			return writeFailure(err, name)
		}
	}
	guard := NewGuard(r)
	frozen := []string{}
	restore := func() bool {
		restored := true
		for i := len(frozen) - 1; i >= 0; i-- {
			name := frozen[i]
			if _, err := r.Lstat(name); !os.IsNotExist(err) {
				restored = false
				continue
			}
			if err := renameNoReplace(r, path.Join(staged, path.Base(name)), name); err != nil {
				restored = false
			}
		}
		return restored
	}
	for _, name := range names {
		if err := renameNoReplace(r, name, path.Join(staged, path.Base(name))); err != nil {
			result := writeFailure(err, name)
			if restore() {
				_ = r.RemoveAll(tmp)
			} else {
				result = preservePlacement(r, dest, tmp, staged, old, suffix, "move-recovery-", guard, result)
			}
			return result
		}
		frozen = append(frozen, name)
	}
	frozenNames := make([]string, 0, len(names))
	for _, name := range names {
		frozenNames = append(frozenNames, path.Join(staged, path.Base(name)))
	}
	p, res = planEntries(walkSources(r, frozenNames, true, guard))
	if res.Code == "" {
		present, replaced, res = checkTargets(r, dest, p)
	}
	if res.Code == "" && len(replaced) > 0 && !overwrite {
		res = conflictResult(dest, replaced)
	}
	rolledBack := true
	if res.Code == "" {
		res, rolledBack = placeAll(r, dest, staged, old, p, present, replaced)
	}
	if res.Code != "" {
		restored := restore()
		if restored && rolledBack {
			_ = r.RemoveAll(tmp)
		} else {
			res = preservePlacement(r, dest, tmp, staged, old, suffix, "move-recovery-", guard, res)
		}
		return res
	}
	_ = r.RemoveAll(old)
	pruneEmpty(r, staged)
	if err := r.Remove(tmp); err != nil {
		return preservePlacement(r, dest, tmp, staged, old, suffix, "move-recovery-", guard, Result{Code: CodeConflict, Error: "files changed during the move"})
	}
	for _, name := range names {
		syncDir(r, path.Dir(name))
	}
	if progress != nil {
		progress(p.bytes, p.bytes)
	}
	return Result{Files: len(p.files), Bytes: p.bytes}
}

func pruneEmpty(r *os.Root, name string) {
	f, err := r.Open(name)
	if err != nil {
		return
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			pruneEmpty(r, path.Join(name, entry.Name()))
		}
	}
	_ = r.Remove(name)
}
