package fileedit

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

func packZip(r *os.Root, rootPath string, req Request) Result {
	names, res := selectedPaths(r, req.Paths)
	if res.Code != "" {
		return res
	}
	target := path.Clean(req.To)
	if !fs.ValidPath(target) || target == "." || !strings.HasSuffix(strings.ToLower(target), ".zip") || InternalPath(target) {
		return Result{Code: CodeBadPath, Error: "the archive destination must be a zip file inside the server"}
	}
	if res := noLinks(r, path.Dir(target)); res.Code != "" {
		return res
	}
	for _, name := range names {
		if target == name || strings.HasPrefix(target, name+"/") {
			return Result{Code: CodeBadPath, Error: "an archive cannot be created inside its selected source"}
		}
	}
	if info, err := r.Lstat(target); err == nil {
		if !info.Mode().IsRegular() {
			return typeConflict(target, "is not a regular file")
		}
		if !req.Overwrite {
			return conflictResult(".", map[string]bool{target: true})
		}
	} else if !os.IsNotExist(err) {
		return failure(err, target)
	}
	d, err := OpenSelectedDownload(rootPath, names)
	if err != nil {
		return Result{Code: CodeBadPath, Error: err.Error()}
	}
	defer d.Close()
	var done int64
	result, err := land(r, target, target, 0o644, func(w io.Writer) error {
		out := countingWriter{w, func(n int) {
			done += int64(n)
			if req.Progress != nil {
				req.Progress(done, 0)
			}
		}}
		return d.WriteTo(context.Background(), out)
	})
	if err != nil {
		return Result{Code: CodeBadPath, Error: fmt.Sprintf("pack zip: %v", err)}
	}
	if result.Code == "" {
		result.Bytes = done
		if req.Progress != nil {
			req.Progress(done, done)
		}
	}
	return result
}
