package submit

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countingBlobs counts the Size reads per id.
type countingBlobs struct {
	*fakeBlobs
	sizes map[string]int
}

func (c *countingBlobs) Size(ctx context.Context, id string) (int64, bool, error) {
	c.sizes[id]++
	return c.fakeBlobs.Size(ctx, id)
}

// putThenFail stores the bytes and still fails, like an object-store upload that
// completed but whose answer was lost.
type putThenFail struct{ *fakeBlobs }

func (p putThenFail) Put(ctx context.Context, id string, r io.Reader) (int64, error) {
	if _, err := p.fakeBlobs.Put(ctx, id, r); err != nil {
		return 0, err
	}
	return 0, errors.New("put: connection reset")
}

// The budget check ahead of each part reads every other blob's size once per
// blobSizeTTL; the one on completion reads them all from the store.
func TestPartBudgetRemembersBlobSizes(t *testing.T) {
	m, _, fb, a := newChunkedManager(t)
	ctx := context.Background()
	clock := testNow
	m.Now = func() time.Time { return clock }
	b, err := m.Create(ctx, CreateRequest{DisplayName: "B", SubmittedBy: "user-2"})
	if err != nil {
		t.Fatal(err)
	}
	fb.stored[b.ID] = []byte("xyz")
	cb := &countingBlobs{fakeBlobs: fb, sizes: map[string]int{}}
	m.Blobs = cb

	sendPart(t, m, a, 0, "\x1f\x8b\x08\x00")
	sendPart(t, m, a, 4, "abcd")
	if n := cb.sizes[b.ID]; n != 1 {
		t.Fatalf("two parts read the other blob's size %d times, want once", n)
	}
	clock = clock.Add(blobSizeTTL)
	sendPart(t, m, a, 8, "ef")
	if n := cb.sizes[b.ID]; n != 2 {
		t.Fatalf("a part once the TTL ran out: %d reads in all, want 2", n)
	}
	if _, err := m.CompleteUpload(ctx, a, "user-1"); err != nil {
		t.Fatal(err)
	}
	if n := cb.sizes[b.ID]; n != 3 {
		t.Fatalf("completion within the TTL: %d reads in all, want 3 (it reads the store)", n)
	}
}

// What this Manager stores or deletes counts in the next part's budget at once,
// without waiting out the TTL.
func TestPartBudgetSeesThisManagersOwnWritesAtOnce(t *testing.T) {
	m, _, _, a := newChunkedManager(t)
	ctx := context.Background()
	m.MaxStoredBytesPerUser = 10
	m.PartMaxBytes = 16
	b, err := m.Create(ctx, CreateRequest{DisplayName: "B", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	// The first part remembers B as holding nothing.
	sendPart(t, m, a, 0, "\x1f\x8b")
	if _, err := m.UploadContext(ctx, b.ID, "user-1", strings.NewReader("\x1f\x8b\x08\x00ab")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UploadPart(ctx, a, "user-1", 2, strings.NewReader("cde")); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("5 bytes staged beside B's 6 under a 10-byte budget = %v, want ErrQuotaExceeded", err)
	}
	if _, err := m.Withdraw(ctx, b.ID, "user-1"); err != nil {
		t.Fatal(err)
	}
	// Its row is gone, so nothing counts it; nor is its size kept, or the
	// remembered sizes would grow with every submission ever deleted.
	if _, kept := m.sizes[b.ID]; kept {
		t.Fatal("a withdrawn submission's blob size is still remembered")
	}
	if got := sendPart(t, m, a, 2, "cdefgh"); got != 8 {
		t.Fatalf("staged %d after B was withdrawn, want 8", got)
	}
}

func TestPartBudgetSeesAReapedBlobAtOnce(t *testing.T) {
	m, st, _, a := newChunkedManager(t)
	ctx := context.Background()
	m.MaxStoredBytesPerUser = 10
	m.PartMaxBytes = 16
	b, err := m.Create(ctx, CreateRequest{DisplayName: "B", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.UploadContext(ctx, b.ID, "user-1", strings.NewReader("\x1f\x8b\x08\x00ab")); err != nil {
		t.Fatal(err)
	}
	reviewed := testNow.Add(-48 * time.Hour)
	st.subs[b.ID].Status = StatusRejected
	st.subs[b.ID].ReviewedAt = &reviewed
	if n, err := m.ReapRejected(ctx, 24*time.Hour); n != 1 || err != nil {
		t.Fatalf("ReapRejected = %d, %v; want 1, nil", n, err)
	}
	if got := sendPart(t, m, a, 0, "\x1f\x8b\x08\x00abcd"); got != 8 {
		t.Fatalf("staged %d after B's blob was reaped, want 8", got)
	}
}

// A Put that failed may still have replaced the blob, so its size is read again.
func TestPartBudgetRereadsABlobAfterAFailedPut(t *testing.T) {
	m, _, fb, a := newChunkedManager(t)
	ctx := context.Background()
	m.MaxStoredBytesPerUser = 10
	m.PartMaxBytes = 16
	b, err := m.Create(ctx, CreateRequest{DisplayName: "B", SubmittedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.UploadContext(ctx, b.ID, "user-1", strings.NewReader("\x1f\x8b\x08\x00ab")); err != nil {
		t.Fatal(err)
	}
	m.Blobs = putThenFail{fb}
	if _, err := m.UploadContext(ctx, b.ID, "user-1", strings.NewReader("\x1f\x8b")); err == nil {
		t.Fatal("setup: the failing Put succeeded")
	}
	m.Blobs = fb
	if got := sendPart(t, m, a, 0, "\x1f\x8b\x08\x00abcd"); got != 8 {
		t.Fatalf("staged %d beside B's 2 stored bytes, want 8", got)
	}
}

// A size that cannot be read fails the check as ErrStoreUnavailable, which the
// API answers 503 for the client to send again, never as a bare error (500).
func TestBudgetReadFailuresAreRetryable(t *testing.T) {
	m, _, fb, a := newChunkedManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, CreateRequest{DisplayName: "B", SubmittedBy: "user-2"}); err != nil {
		t.Fatal(err)
	}
	fb.sizeErr = errors.New("dial tcp: i/o timeout")
	if _, err := m.UploadPart(ctx, a, "user-1", 0, strings.NewReader("\x1f\x8b\x08\x00")); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("a part with the blob store down = %v, want ErrStoreUnavailable", err)
	}
	if _, err := m.UploadContext(ctx, a, "user-1", strings.NewReader("\x1f\x8b\x08\x00")); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("an upload with the blob store down = %v, want ErrStoreUnavailable", err)
	}
	if _, ok := fb.stored[a]; ok {
		t.Fatal("an upload whose budget could not be checked was stored")
	}

	fb.sizeErr = nil
	notDir := filepath.Join(t.TempDir(), "parts")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m.Parts = &PartStore{Dir: notDir}
	if _, err := m.UploadContext(ctx, a, "user-1", strings.NewReader("\x1f\x8b\x08\x00")); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("an upload with the staging directory unreadable = %v, want ErrStoreUnavailable", err)
	}
}
