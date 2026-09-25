package submit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newChunkedManager wires a Manager with an in-memory Blobs, a PartStore on a
// temp dir and 4-byte parts, and files one pending submission for user-1.
func newChunkedManager(t *testing.T) (*Manager, *fakeStore, *fakeBlobs, string) {
	t.Helper()
	m, st, _ := newManager()
	fb := newFakeBlobs()
	m.Blobs = fb
	// A near-zero floor keeps the room check off this machine's own disk usage.
	m.Parts = &PartStore{Dir: t.TempDir(), MinFree: 1e-9}
	m.PartMaxBytes = 4
	sub, err := m.Create(context.Background(), CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	return m, st, fb, sub.ID
}

func sendPart(t *testing.T, m *Manager, id string, offset int64, part string) int64 {
	t.Helper()
	p, err := m.UploadPart(context.Background(), id, "user-1", offset, strings.NewReader(part))
	if err != nil {
		t.Fatalf("UploadPart(offset %d, %q): %v", offset, part, err)
	}
	return p.Received
}

func staged(t *testing.T, m *Manager, id string) int64 {
	t.Helper()
	p, err := m.UploadStatus(context.Background(), id, "user-1")
	if err != nil {
		t.Fatalf("UploadStatus: %v", err)
	}
	return p.Received
}

func TestChunkedUploadAssemblesAndStores(t *testing.T) {
	m, _, fb, id := newChunkedManager(t)
	ctx := context.Background()

	p, err := m.UploadStatus(ctx, id, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if p != (UploadProgress{Received: 0, PartMaxBytes: 4, MaxContextBytes: 1 << 30}) {
		t.Fatalf("status before any part = %+v", p)
	}
	for _, step := range []struct {
		offset int64
		part   string
		want   int64
	}{
		{0, "\x1f\x8b\x08\x00", 4},
		{4, "abcd", 8},
		{8, "efgh", 12},
		{12, "ij", 14},
	} {
		if got := sendPart(t, m, id, step.offset, step.part); got != step.want {
			t.Fatalf("after the part at %d: received %d, want %d", step.offset, got, step.want)
		}
	}
	if _, ok := fb.stored[id]; ok {
		t.Fatal("parts reached the blob store before the upload completed")
	}

	sub, err := m.CompleteUpload(ctx, id, "user-1")
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if got := string(fb.stored[id]); got != "\x1f\x8b\x08\x00abcdefghij" {
		t.Fatalf("stored %q, want the parts in order", got)
	}
	if sub.ContextSHA256 != "df86a051af3d8615de097952ac01bc094781053bcda61aaac848edb98068209d" {
		t.Fatalf("recorded digest %s", sub.ContextSHA256)
	}
	if got := staged(t, m, id); got != 0 {
		t.Fatalf("staged bytes after completion = %d, want the staged copy gone", got)
	}
}

func TestChunkedUploadResumesWhereTheStagedBytesEnd(t *testing.T) {
	m, _, _, id := newChunkedManager(t)
	ctx := context.Background()

	var mismatch *OffsetMismatchError
	_, err := m.UploadPart(ctx, id, "user-1", 4, strings.NewReader("abcd"))
	if !errors.As(err, &mismatch) || mismatch.Received != 0 {
		t.Fatalf("a part past an empty upload = %v, want an offset mismatch at 0", err)
	}
	sendPart(t, m, id, 0, "\x1f\x8b\x08\x00")
	_, err = m.UploadPart(ctx, id, "user-1", 8, strings.NewReader("efgh"))
	if !errors.As(err, &mismatch) || mismatch.Received != 4 {
		t.Fatalf("a part that skips ahead = %v, want an offset mismatch at 4", err)
	}
	if got := sendPart(t, m, id, 4, "abcd"); got != 8 {
		t.Fatalf("the part at the staged length: received %d, want 8", got)
	}
	// Offset 0 starts over.
	if got := sendPart(t, m, id, 0, "\x1f\x8b"); got != 2 {
		t.Fatalf("restart at 0: received %d, want 2", got)
	}
}

type failingReader struct {
	data string
	done bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.done {
		return 0, errors.New("connection reset")
	}
	f.done = true
	return copy(p, f.data), nil
}

func TestChunkedUploadCutsBackAPartThatBrokeOff(t *testing.T) {
	m, _, _, id := newChunkedManager(t)
	sendPart(t, m, id, 0, "\x1f\x8b\x08\x00")

	_, err := m.UploadPart(context.Background(), id, "user-1", 4, &failingReader{data: "ab"})
	if err == nil {
		t.Fatal("a part that broke off was accepted")
	}
	if got := staged(t, m, id); got != 4 {
		t.Fatalf("staged after a broken part = %d, want 4 (the half part cut back off)", got)
	}
}

func TestChunkedUploadFirstPartMustBeGzip(t *testing.T) {
	m, _, _, id := newChunkedManager(t)
	_, err := m.UploadPart(context.Background(), id, "user-1", 0, strings.NewReader("PK\x03\x04"))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a zip first part = %v, want ErrInvalid", err)
	}
	if got := staged(t, m, id); got != 0 {
		t.Fatalf("staged after a refused first part = %d, want 0", got)
	}
}

func TestChunkedUploadPartCap(t *testing.T) {
	m, _, _, id := newChunkedManager(t)
	sendPart(t, m, id, 0, "\x1f\x8b\x08\x00")

	_, err := m.UploadPart(context.Background(), id, "user-1", 4, strings.NewReader("abcde"))
	if !errors.Is(err, ErrPartTooLarge) {
		t.Fatalf("a 5-byte part over a 4-byte cap = %v, want ErrPartTooLarge", err)
	}
	if got := staged(t, m, id); got != 4 {
		t.Fatalf("staged after an oversize part = %d, want 4", got)
	}
}

// The staged total meets the same cap as a whole context, with the same error.
func TestChunkedUploadTotalCappedLikeAWholeContext(t *testing.T) {
	m, _, _, id := newChunkedManager(t)
	m.MaxContextBytes = 10
	sendPart(t, m, id, 0, "\x1f\x8b\x08\x00")
	sendPart(t, m, id, 4, "abcd")

	_, err := m.UploadPart(context.Background(), id, "user-1", 8, strings.NewReader("efg"))
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrPartTooLarge) {
		t.Fatalf("a part past the context cap = %v, want the context-too-large ErrInvalid", err)
	}
	if got := sendPart(t, m, id, 8, "ef"); got != 10 {
		t.Fatalf("a part ending exactly at the cap: received %d, want 10", got)
	}
}

// Staged bytes count against the storage budget, so parts parked on several
// pending submissions cannot hold more than stored contexts could.
func TestChunkedUploadStagedBytesCountAgainstTheBudget(t *testing.T) {
	m, _, fb, a := newChunkedManager(t)
	ctx := context.Background()
	m.MaxStoredBytesPerUser = 10
	m.PartMaxBytes = 16
	sendPart(t, m, a, 0, "\x1f\x8b\x08\x00abcd")

	b, err := m.Create(ctx, CreateRequest{DisplayName: "B", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.UploadContext(ctx, b.ID, "user-1", strings.NewReader("\x1f\x8b\x08")); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("3 bytes beside 8 staged under a 10-byte budget = %v, want ErrQuotaExceeded", err)
	}
	if _, err := m.UploadContext(ctx, b.ID, "user-1", strings.NewReader("\x1f\x8b")); err != nil {
		t.Fatalf("2 bytes beside 8 staged = %v, want accepted", err)
	}
	// And the other way round: B's stored 2 bytes bound what A may still stage.
	if _, err := m.UploadPart(ctx, a, "user-1", 8, strings.NewReader("e")); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("staging past the budget = %v, want ErrQuotaExceeded", err)
	}
	if _, ok := fb.stored[a]; ok {
		t.Fatal("staging stored a blob")
	}
}

func TestChunkedUploadOwnerAndStatus(t *testing.T) {
	m, st, _, id := newChunkedManager(t)
	ctx := context.Background()
	sendPart(t, m, id, 0, "\x1f\x8b\x08\x00")

	if _, err := m.UploadStatus(ctx, id, "user-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's status = %v, want ErrNotFound", err)
	}
	if _, err := m.UploadPart(ctx, id, "user-2", 4, strings.NewReader("abcd")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's part = %v, want ErrNotFound", err)
	}
	if _, err := m.CompleteUpload(ctx, id, "user-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's completion = %v, want ErrNotFound", err)
	}
	st.subs[id].Status = StatusRejected
	if _, err := m.UploadPart(ctx, id, "user-1", 4, strings.NewReader("abcd")); !errors.Is(err, ErrAlreadyReviewed) {
		t.Fatalf("a part for a reviewed submission = %v, want ErrAlreadyReviewed", err)
	}
	if _, err := m.CompleteUpload(ctx, id, "user-1"); !errors.Is(err, ErrAlreadyReviewed) {
		t.Fatalf("completing a reviewed submission = %v, want ErrAlreadyReviewed", err)
	}
}

func TestChunkedUploadWithoutPartStoreIsUnavailable(t *testing.T) {
	m, _, _, id := newChunkedManager(t)
	m.Parts = nil
	if _, err := m.UploadStatus(context.Background(), id, "user-1"); !errors.Is(err, ErrUploadsUnavailable) {
		t.Fatalf("status without a part store = %v, want ErrUploadsUnavailable", err)
	}
	if _, err := m.UploadPart(context.Background(), id, "user-1", 0, strings.NewReader("\x1f\x8b")); !errors.Is(err, ErrUploadsUnavailable) {
		t.Fatalf("part without a part store = %v, want ErrUploadsUnavailable", err)
	}
}

func TestCompleteUploadWithNothingStagedIsInvalid(t *testing.T) {
	m, _, fb, id := newChunkedManager(t)
	if _, err := m.CompleteUpload(context.Background(), id, "user-1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("completing an empty upload = %v, want ErrInvalid", err)
	}
	if len(fb.stored) != 0 {
		t.Fatal("an empty completion stored a blob")
	}
}

func TestCompleteUploadFailureKeepsTheStagedBytes(t *testing.T) {
	m, _, fb, id := newChunkedManager(t)
	sendPart(t, m, id, 0, "\x1f\x8b\x08\x00")
	sendPart(t, m, id, 4, "ab")
	fb.putErr = errors.New("object store unreachable")

	if _, err := m.CompleteUpload(context.Background(), id, "user-1"); err == nil {
		t.Fatal("completion succeeded with the blob store down")
	}
	if got := staged(t, m, id); got != 6 {
		t.Fatalf("staged after a failed completion = %d, want 6 kept for the retry", got)
	}
	fb.putErr = nil
	if _, err := m.CompleteUpload(context.Background(), id, "user-1"); err != nil {
		t.Fatalf("retried completion: %v", err)
	}
	if got := string(fb.stored[id]); got != "\x1f\x8b\x08\x00ab" {
		t.Fatalf("stored %q after the retry", got)
	}
}

func TestChunkedUploadOneRequestAtATime(t *testing.T) {
	m, _, _, id := newChunkedManager(t)
	sendPart(t, m, id, 0, "\x1f\x8b\x08\x00")
	release, err := m.Parts.hold(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.UploadPart(context.Background(), id, "user-1", 4, strings.NewReader("ab")); !errors.Is(err, ErrUploadBusy) {
		t.Fatalf("a part while another is writing = %v, want ErrUploadBusy", err)
	}
	if _, err := m.CompleteUpload(context.Background(), id, "user-1"); !errors.Is(err, ErrUploadBusy) {
		t.Fatalf("completing while a part is writing = %v, want ErrUploadBusy", err)
	}
	release()
	if got := sendPart(t, m, id, 4, "ab"); got != 6 {
		t.Fatalf("the part after release: received %d, want 6", got)
	}
}

func TestWithdrawDeletesTheStagedUpload(t *testing.T) {
	m, _, _, id := newChunkedManager(t)
	sendPart(t, m, id, 0, "\x1f\x8b\x08\x00")
	if _, err := m.Withdraw(context.Background(), id, "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Parts.Dir, id+".part")); !os.IsNotExist(err) {
		t.Fatalf("staged upload after withdraw: stat err = %v, want it gone", err)
	}
}

func TestReapStaleParts(t *testing.T) {
	m, _, _, oldID := newChunkedManager(t)
	ctx := context.Background()
	fresh, err := m.Create(ctx, CreateRequest{DisplayName: "Fresh", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	sendPart(t, m, oldID, 0, "\x1f\x8b\x08\x00")
	sendPart(t, m, fresh.ID, 0, "\x1f\x8b\x08\x00")
	if err := os.Chtimes(filepath.Join(m.Parts.Dir, oldID+".part"), testNow.Add(-25*time.Hour), testNow.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(m.Parts.Dir, fresh.ID+".part"), testNow.Add(-23*time.Hour), testNow.Add(-23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A stray file that is not a staged upload is left alone.
	if err := os.WriteFile(filepath.Join(m.Parts.Dir, "notes"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(filepath.Join(m.Parts.Dir, "notes"), testNow.Add(-48*time.Hour), testNow.Add(-48*time.Hour))

	n, err := m.ReapStaleParts(StalePartRetention)
	if err != nil || n != 1 {
		t.Fatalf("ReapStaleParts = %d, %v; want 1", n, err)
	}
	if got := staged(t, m, oldID); got != 0 {
		t.Fatalf("the 25-hour-old upload still holds %d bytes", got)
	}
	if got := staged(t, m, fresh.ID); got != 4 {
		t.Fatalf("the 23-hour-old upload holds %d bytes, want 4 kept", got)
	}
	if _, err := os.Stat(filepath.Join(m.Parts.Dir, "notes")); err != nil {
		t.Fatalf("the reaper touched a file that is not a staged upload: %v", err)
	}
}
