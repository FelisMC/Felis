package fileedit

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

const recoveryMarker = ".keep"

func preservePlacement(r *os.Root, dest, tmp, staged, old, suffix, prefix string, guard Guard, res Result) Result {
	res.Code = CodeRecovery
	if marker, err := r.OpenFile(path.Join(tmp, recoveryMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600); err == nil {
		_ = marker.Close()
	}
	retained := path.Join(dest, ".felis-preserved-"+suffix)
	if err := renameNoReplace(r, tmp, retained); err == nil {
		tmp = retained
		staged, old = path.Join(tmp, "new"), path.Join(tmp, "old")
	}
	res.RecoveryPaths = []string{tmp}
	recovery := path.Join(dest, prefix+suffix)
	if err := r.Mkdir(staged, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		res.Error += "; remaining files are preserved in " + tmp + " for administrator recovery"
		return res
	}
	if err := renameNoReplace(r, staged, recovery); err != nil {
		res.Error += "; remaining files are preserved in " + tmp + " for administrator recovery"
		return res
	}
	if hasEntries(r, old) {
		previous := path.Join(recovery, "previous")
		if err := r.Mkdir(previous, 0700); err == nil {
			_ = fs.WalkDir(r.FS(), old, func(at string, de fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if at == old {
					return nil
				}
				rel := strings.TrimPrefix(at, old+"/")
				to := path.Join(previous, rel)
				if de.IsDir() {
					return r.Mkdir(to, 0755)
				}
				info, err := de.Info()
				if err != nil {
					return err
				}
				withheld, redact := guard.Rule(info)
				currentWithheld, currentRedact := NewGuard(r).Rule(info)
				pathWithheld, pathRedact := ArchiveRule(path.Join(dest, rel))
				if !info.Mode().IsRegular() || withheld || redact || currentWithheld || currentRedact || pathWithheld || pathRedact {
					return nil
				}
				return renameNoReplace(r, at, to)
			})
		}
	}
	pruneEmpty(r, old)
	_ = r.Remove(path.Join(tmp, recoveryMarker))
	_ = r.Remove(tmp)
	res.RecoveryPaths = []string{recovery}
	res.Error += "; remaining source files and ordinary backups are preserved in " + recovery
	if hasEntries(r, tmp) {
		res.RecoveryPaths = append(res.RecoveryPaths, tmp)
		res.Error += "; protected or unmovable backups remain in " + tmp + " for administrator recovery"
	}
	return res
}

func hasEntries(r *os.Root, name string) bool {
	f, err := r.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer f.Close()
	entries, err := f.ReadDir(1)
	return len(entries) > 0 || err != nil && !errors.Is(err, io.EOF)
}
