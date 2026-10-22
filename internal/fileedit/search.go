package fileedit

import (
	"io/fs"
	"os"
	"path"
	"strings"
)

const MaxSearchResults = 500
const MaxSearchDepth = 16

func search(r *os.Root, base, query string) Result {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return Result{Code: CodeBadPath, Error: "a filename search needs a query"}
	}
	base = path.Clean(base)
	info, err := r.Stat(base)
	if err != nil {
		return failure(err, base)
	}
	if !info.IsDir() {
		return Result{Code: CodeBadPath, Error: "search starts in a folder"}
	}
	result := Result{Entries: []Entry{}, Avail: -1}
	err = fs.WalkDir(r.FS(), base, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == base {
			return nil
		}
		if InternalPath(name) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		relative := strings.TrimPrefix(name, base+"/")
		if base == "." {
			relative = name
		}
		if strings.Contains(strings.ToLower(entry.Name()), query) {
			if len(result.Entries) == MaxSearchResults {
				result.Truncated = true
				return fs.SkipAll
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			result.Entries = append(result.Entries, Entry{Name: relative, Size: info.Size(), IsDir: info.IsDir(), ModTime: info.ModTime()})
		}
		if entry.IsDir() && (entry.Name() == "region" || entry.Name() == "entities" || entry.Name() == "poi" || strings.Count(relative, "/") >= MaxSearchDepth) {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return failure(err, base)
	}
	return result
}
