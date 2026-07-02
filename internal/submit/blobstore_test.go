package submit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalContextStorePutAndExists(t *testing.T) {
	base := t.TempDir()
	s := &LocalContextStore{Base: base}
	ctx := context.Background()

	if ok, err := s.Exists(ctx, "sub-abc"); err != nil || ok {
		t.Fatalf("Exists before Put = (%v, %v), want (false, nil)", ok, err)
	}

	payload := "\x1f\x8b\x08\x00the modpack context"
	n, err := s.Put(ctx, "sub-abc", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("Put returned %d bytes, want %d", n, len(payload))
	}

	// The blob lands at exactly {base}/{id}/context.tar.gz — where deriveContextRef
	// points Kaniko's --context.
	dest := filepath.Join(base, "sub-abc", contextBlobName)
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read stored blob: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("stored %q, want %q", got, payload)
	}
	if ok, err := s.Exists(ctx, "sub-abc"); err != nil || !ok {
		t.Fatalf("Exists after Put = (%v, %v), want (true, nil)", ok, err)
	}

	// The write is atomic: no leftover temp files beside the committed blob.
	entries, err := os.ReadDir(filepath.Join(base, "sub-abc"))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != contextBlobName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir entries = %v, want only %q (no temp files)", names, contextBlobName)
	}
}

func TestLocalContextStorePutOverwrites(t *testing.T) {
	base := t.TempDir()
	s := &LocalContextStore{Base: base}
	ctx := context.Background()

	if _, err := s.Put(ctx, "sub-1", strings.NewReader("\x1f\x8bfirst")); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if _, err := s.Put(ctx, "sub-1", strings.NewReader("\x1f\x8bsecond upload")); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(base, "sub-1", contextBlobName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "\x1f\x8bsecond upload" {
		t.Fatalf("stored %q, want the second upload (a re-upload supersedes)", got)
	}
}

func TestLocalContextStoreRejectsUnsafeID(t *testing.T) {
	base := t.TempDir()
	s := &LocalContextStore{Base: base}
	ctx := context.Background()

	for _, id := range []string{"../evil", "sub/../../etc", "SUB-UPPER", "has space", "", "a/b"} {
		if _, err := s.Put(ctx, id, strings.NewReader("\x1f\x8bx")); err == nil {
			t.Errorf("Put(%q) succeeded, want rejection", id)
		}
		if _, err := s.Exists(ctx, id); err == nil {
			t.Errorf("Exists(%q) succeeded, want rejection", id)
		}
	}
	// Nothing escaped the base directory.
	if _, err := os.Stat(filepath.Join(filepath.Dir(base), "evil")); !os.IsNotExist(err) {
		t.Fatal("an unsafe id wrote outside Base")
	}
}
