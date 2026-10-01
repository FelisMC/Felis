package archivetransfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/worldexport"
)

func archiveBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "world/level.dat", Mode: 0644, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func upload(t *testing.T, url, token string, body []byte, digest bool) int {
	t.Helper()
	r, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if digest {
		sum := sha256.Sum256(body)
		r.ContentLength = -1
		r.Trailer = http.Header{worldexport.DigestTrailer: {"sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"}}
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Log(string(response))
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestDurableTransferAndRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	key := strings.Repeat("k", 32)
	s := &Server{Root: root, Key: key, Limit: 1 << 20, freeBytes: func() (int64, error) { return 1 << 30, nil }}
	ts := httptest.NewServer(s)
	defer ts.Close()
	c := Client{Root: root, Key: key, URL: ts.URL, Limit: 1 << 20}
	ticket, url, token, err := c.Issue("alice", "PUT", "", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := archiveBytes(t)
	if code := upload(t, url, token, body, true); code != 204 {
		t.Fatalf("upload %d", code)
	}
	if code := upload(t, url, token, body, true); code != 409 {
		t.Fatalf("replay %d", code)
	}
	rec, err := c.Receipt(ctx, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if rec.Size != int64(len(body)) || rec.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("receipt %+v", rec)
	}
	// Simulate A losing the response and crashing between archive commit and receipt write.
	if err := os.Remove(filepath.Join(root, ".transfers", ticket.ID+".json")); err != nil {
		t.Fatal(err)
	}
	restarted := &Server{Root: root, Key: key, Limit: 1 << 20, freeBytes: func() (int64, error) { return 1 << 30, nil }}
	if recovered, err := restarted.receipt(ctx, ticket.ID); err != nil || recovered != rec {
		t.Fatalf("recovery %+v: %v", recovered, err)
	}
	if _, _, _, err := c.Issue("bob", "GET", rec.Ref, rec.SHA256, time.Minute); err == nil {
		t.Fatal("cross-server download ticket accepted")
	}
	_, getURL, getToken, err := c.Issue("alice", "GET", rec.Ref, rec.SHA256, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "download.tar.gz")
	if err := Fetch(ctx, getURL, getToken, dest, rec.SHA256, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(ctx, getURL, getToken, filepath.Join(t.TempDir(), "replay"), rec.SHA256, 1<<20); err == nil {
		t.Fatal("download replay accepted")
	}
	world := t.TempDir()
	a := &backup.TarLocal{BackupRoot: filepath.Dir(dest), Resolve: func(string) (string, error) { return world, nil }}
	if err := a.Restore(ctx, backup.ArchiveRef(dest), "alice"); err != nil {
		t.Fatal(err)
	}
	if err := backup.VerifyRestored(ctx, dest, world); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "world", "level.dat"), []byte("wrong"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := backup.VerifyRestored(ctx, dest, world); err == nil {
		t.Fatal("read-back corruption accepted")
	}
}

func TestRejectedTransfersNeverCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int64
		body   []byte
		digest bool
	}{
		{"missing checksum", 1 << 20, archiveBytes(t), false},
		{"invalid tar", 1 << 20, []byte("corrupt archive"), true},
		{"size limit", 10, archiveBytes(t), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			key := strings.Repeat("k", 32)
			ts := httptest.NewServer(&Server{Root: root, Key: key, Limit: tc.limit, freeBytes: func() (int64, error) { return 1 << 30, nil }})
			defer ts.Close()
			c := Client{Root: root, Key: key, URL: ts.URL, Limit: tc.limit}
			ticket, url, token, err := c.Issue("alice", "PUT", "", "", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if code := upload(t, url, token, tc.body, tc.digest); code != 422 {
				t.Fatalf("status %d", code)
			}
			if _, err := os.Stat(ticket.Ref); !os.IsNotExist(err) {
				t.Fatalf("archive committed: %v", err)
			}
			if _, err := c.Receipt(context.Background(), ticket.ID); err == nil {
				t.Fatal("failed upload has receipt")
			}
			if code := upload(t, url, token, tc.body, true); code != 409 {
				t.Fatalf("failed upload replay %d", code)
			}
		})
	}
}

func signTicket(t Ticket, key string) string {
	raw, _ := json.Marshal(t)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func TestExpiredWrongMethodAndPath(t *testing.T) {
	root := t.TempDir()
	key := strings.Repeat("k", 32)
	s := &Server{Root: root, Key: key}
	c := Client{Root: root, Key: key, URL: "http://archive"}
	ticket, _, token, err := c.Issue("alice", "PUT", "", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, token string }{
		{"GET", "/transfers/" + ticket.ID, token},
		{"PUT", "/transfers/" + ID(), token},
		{"PUT", "/transfers/" + ticket.ID, token + "broken"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 && w.Code != 401 {
			t.Fatalf("scope accepted: %d", w.Code)
		}
	}
	ticket.Expires = time.Now().Add(-time.Minute)
	r := httptest.NewRequest("PUT", "/transfers/"+ticket.ID, nil)
	r.Header.Set("Authorization", "Bearer "+signTicket(ticket, key))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("expired %d", w.Code)
	}
}

func TestFullDiskDoesNotCommit(t *testing.T) {
	root := t.TempDir()
	key := strings.Repeat("k", 32)
	ts := httptest.NewServer(&Server{Root: root, Key: key, freeBytes: func() (int64, error) { return 0, nil }})
	defer ts.Close()
	c := Client{Root: root, Key: key, URL: ts.URL}
	ticket, url, token, err := c.Issue("alice", "PUT", "", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if code := upload(t, url, token, archiveBytes(t), true); code != 422 {
		t.Fatal("disk full accepted", code)
	}
	if _, err := os.Stat(ticket.Ref); !os.IsNotExist(err) {
		t.Fatal("full disk committed archive", err)
	}
}
