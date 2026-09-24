package backup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckRoom(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "level.dat"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	archives := filepath.Join(t.TempDir(), "not", "made", "yet")
	if err := CheckRoom(archives, src, 0); err != nil {
		t.Fatalf("no floor: %v", err)
	}
	// No disk is ever entirely free, so a 100% floor always refuses.
	if err := CheckRoom(archives, src, 1); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("full floor: err = %v, want ErrNoRoom", err)
	}
	if err := CheckRoom(archives, filepath.Join(src, "absent"), 0); err == nil {
		t.Fatal("a missing world measured as empty")
	}
}
