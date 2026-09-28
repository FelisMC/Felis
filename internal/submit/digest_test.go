package submit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sumOf is the SHA-256 a client sends with body.
func sumOf(body string) []byte {
	s := sha256.Sum256([]byte(body))
	return s[:]
}

func TestVerifyDigest(t *testing.T) {
	t.Run("bytes that hash to the digest end cleanly", func(t *testing.T) {
		b, err := io.ReadAll(VerifyDigest(strings.NewReader("modpack"), sumOf("modpack")))
		if err != nil || string(b) != "modpack" {
			t.Fatalf("ReadAll = (%q, %v), want (modpack, nil)", b, err)
		}
	})
	t.Run("bytes that do not end in ErrDigestMismatch", func(t *testing.T) {
		b, err := io.ReadAll(VerifyDigest(strings.NewReader("modpacX"), sumOf("modpack")))
		if !errors.Is(err, ErrDigestMismatch) {
			t.Fatalf("err = %v, want ErrDigestMismatch", err)
		}
		if string(b) != "modpacX" {
			t.Fatalf("passed on %q, want every byte (the store decides what to keep)", b)
		}
	})
	t.Run("a read that breaks off is passed on as it is", func(t *testing.T) {
		_, err := io.ReadAll(VerifyDigest(&failingReader{data: "mod"}, sumOf("mod")))
		if err == nil || errors.Is(err, ErrDigestMismatch) || err.Error() != "connection reset" {
			t.Fatalf("err = %v, want the connection reset itself", err)
		}
	})
}

// A part that changed on the way is cut back off like one that broke off, below
// the part cap and exactly at it (where the cap reads one byte past the part).
func TestChunkedUploadCutsBackAPartChangedOnTheWay(t *testing.T) {
	for _, tc := range []struct{ name, sent, meant string }{
		{"under the part cap", "abX", "abc"},
		{"at the part cap", "abcX", "abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _, id := newChunkedManager(t)
			sendPart(t, m, id, 0, "\x1f\x8b\x08\x00")

			_, err := m.UploadPart(context.Background(), id, "user-1", 4, VerifyDigest(strings.NewReader(tc.sent), sumOf(tc.meant)))
			if !errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("err = %v, want ErrDigestMismatch", err)
			}
			if got := staged(t, m, id); got != 4 {
				t.Fatalf("staged after a changed part = %d, want 4", got)
			}
			p, err := m.UploadPart(context.Background(), id, "user-1", 4, VerifyDigest(strings.NewReader(tc.meant), sumOf(tc.meant)))
			if err != nil || p.Received != int64(4+len(tc.meant)) {
				t.Fatalf("the part sent again = (%d, %v), want (%d, nil)", p.Received, err, 4+len(tc.meant))
			}
		})
	}
}

// A context that changed on the way leaves the one before it stored, and its
// digest recorded.
func TestUploadContextChangedOnTheWayKeepsThePreviousContext(t *testing.T) {
	m, st, _ := newManager()
	base := t.TempDir()
	m.Blobs = &LocalContextStore{Base: base, MinFree: 1e-9}
	ctx := context.Background()
	seed, _ := m.Create(ctx, CreateRequest{DisplayName: "Pack", SubmittedBy: "user-1"})
	first, second := gzBody("first"), gzBody("second")
	if _, err := m.UploadContext(ctx, seed.ID, "user-1", VerifyDigest(strings.NewReader(first), sumOf(first))); err != nil {
		t.Fatalf("first upload: %v", err)
	}

	_, err := m.UploadContext(ctx, seed.ID, "user-1", VerifyDigest(strings.NewReader(second+"X"), sumOf(second)))

	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	if got, _ := os.ReadFile(filepath.Join(base, seed.ID, contextBlobName)); string(got) != first {
		t.Fatalf("stored %q, want the first upload %q", got, first)
	}
	if got := st.subs[seed.ID].ContextSHA256; got != hex.EncodeToString(sumOf(first)) {
		t.Fatalf("recorded digest %s, want the first upload's", got)
	}
}

// Through minio-go, a mismatch found at the end of the stream aborts the
// multipart upload, whether the last part is short or the stream ends right at a
// part boundary.
func TestS3ContextStorePutKeepsNothingChangedOnTheWay(t *testing.T) {
	for _, size := range []int{2*s3PartSize + 5, 2 * s3PartSize} {
		fake := &multipartS3{objects: map[string]string{}}
		ts := httptest.NewServer(fake)
		s, err := NewS3ContextStore(S3StoreConfig{Base: "s3://felis-uploads/builds", Endpoint: ts.URL, Region: "us-east-1", AccessKey: "a", SecretKey: "b"})
		if err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, size)
		rand.New(rand.NewSource(2)).Read(payload)
		meant := sumOf(string(payload))
		payload[size-1] ^= 1

		_, err = s.Put(context.Background(), "sub-big", VerifyDigest(bytes.NewReader(payload), meant))
		ts.Close()

		if !errors.Is(err, ErrDigestMismatch) {
			t.Errorf("%d bytes: err = %v, want ErrDigestMismatch", size, err)
		}
		if len(fake.objects) != 0 {
			t.Errorf("%d bytes: an object was completed: %v", size, fake.objects)
		}
	}
}
