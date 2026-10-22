package fileedit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchNamesAndBounds(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"plugins/WorldEdit/Config.yml", "world/region/r.0.0.mca", "world/entities/r.0.0.mca", "world/poi/r.0.0.mca", "logs/latest.log"} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Execute(root, Request{Op: OpSearch, Query: "config"})
	if err != nil || result.Code != "" || len(result.Entries) != 1 || result.Entries[0].Name != "plugins/WorldEdit/Config.yml" {
		t.Fatalf("recursive filename search: %+v %v", result, err)
	}
	result, err = Execute(root, Request{Op: OpSearch, Query: ".mca"})
	if err != nil || len(result.Entries) != 0 {
		t.Fatal("search entered chunk directories", result, err)
	}
	for i := range 510 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("match-%03d", i)), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	result, err = Execute(root, Request{Op: OpSearch, Query: "match-"})
	if err != nil || len(result.Entries) != 500 || !result.Truncated {
		t.Fatalf("result bound: %d truncated=%v err=%v", len(result.Entries), result.Truncated, err)
	}
	result, err = Execute(root, Request{Op: OpSearch, Path: "../", Query: "match"})
	if err != nil || result.Code != CodeBadPath {
		t.Fatalf("search escaped the world: %+v %v", result, err)
	}
}

func TestSearchDepthBoundary(t *testing.T) {
	root := t.TempDir()
	depth16 := strings.Repeat("nested/", 16) + "match-at-limit.txt"
	depth17 := strings.Repeat("nested/", 17) + "match-beyond-limit.txt"
	for _, name := range []string{depth16, depth17} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Execute(root, Request{Op: OpSearch, Query: "match-"})
	if err != nil || result.Code != "" || len(result.Entries) != 1 || result.Entries[0].Name != depth16 || result.Avail != -1 {
		t.Fatalf("depth bound: %+v %v", result, err)
	}
}
