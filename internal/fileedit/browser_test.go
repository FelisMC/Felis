package fileedit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserReusesWorkerAndReadsCurrentBytes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config.yml"), []byte("enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	b := &Browser{}
	srv := httptest.NewServer(b)
	b.BaseURL = srv.URL
	defer srv.Close()
	defer cancel()
	var workers atomic.Int32
	start := func(_ context.Context, p JobParams) error {
		workers.Add(1)
		job, err := FilesJob(p)
		if err != nil {
			return err
		}
		pod := job.Spec.Template.Spec
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || len(pod.Volumes) != 1 || !pod.Containers[0].VolumeMounts[0].ReadOnly {
			t.Error("browser weakened the file Job's isolation")
		}
		go func() {
			if err := Browse(ctx, root, p.BrowserURL, p.BrowserToken); err != nil && ctx.Err() == nil {
				t.Errorf("worker: %v", err)
			}
		}()
		return nil
	}
	read := func(op, path string) Result {
		p := testParams(op)
		p.Path = path
		p.OpID, _ = newOpID()
		payload, err := b.Run(ctx, p, start)
		if err != nil {
			t.Fatal(err)
		}
		var result Result
		if err := json.Unmarshal(payload, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got := read(OpRead, "config.yml"); string(got.Content) != "enabled: true\n" {
		t.Fatalf("first read: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yml"), []byte("enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := read(OpRead, "config.yml"); string(got.Content) != "enabled: false\n" {
		t.Fatalf("stale read: %+v", got)
	}
	if got := read(OpList, ""); len(got.Entries) != 1 {
		t.Fatalf("listing: %+v", got)
	}
	if got := read(OpRead, "../outside"); got.Code != CodeBadPath {
		t.Fatalf("containment: %+v", got)
	}
	if workers.Load() != 1 {
		t.Fatalf("started %d workers for repeated reads", workers.Load())
	}

	// Simultaneous readers still receive their own result, never the other path's.
	var wg sync.WaitGroup
	for _, path := range []string{"config.yml", "missing.yml"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := testParams(OpRead)
			p.Path = path
			p.OpID, _ = newOpID()
			payload, err := b.Run(ctx, p, start)
			if err != nil {
				t.Error(err)
				return
			}
			var got Result
			json.Unmarshal(payload, &got)
			if path == "missing.yml" && got.Code != CodeNotFound || path == "config.yml" && string(got.Content) != "enabled: false\n" {
				t.Errorf("%s: %+v", path, got)
			}
		}()
	}
	wg.Wait()
}

func TestBrowserCancellationRevokesToken(t *testing.T) {
	b := &Browser{BaseURL: "http://internal"}
	var started JobParams
	ctx, cancel := context.WithCancel(context.Background())
	p := testParams(OpRead)
	_, err := b.Run(ctx, p, func(_ context.Context, p JobParams) error { started = p; cancel(); return nil })
	if err == nil {
		t.Fatal("cancelled read succeeded")
	}
	r := httptest.NewRequest(http.MethodPost, started.BrowserURL, strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+started.BrowserToken)
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cancelled token returned %d", w.Code)
	}
}

func TestBrowserRejectsWrongTokenAndStaleResult(t *testing.T) {
	b := &Browser{BaseURL: "http://internal"}
	p := testParams(OpRead)
	var started JobParams
	s, err := b.session(context.Background(), p, func(_ context.Context, p JobParams) error { started = p; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer b.close(s)
	for _, header := range []string{"", "Bearer wrong", started.BrowserToken} {
		r := httptest.NewRequest(http.MethodPost, started.BrowserURL, strings.NewReader(`{}`))
		r.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("wrong token returned %d", w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, started.BrowserURL, strings.NewReader(`{"id":"another-read","result":{}}`))
	r.Header.Set("Authorization", "Bearer "+started.BrowserToken)
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale result returned %d", w.Code)
	}
}

func TestBrowserCapacityReleasedAfterClosingSession(t *testing.T) {
	b := &Browser{BaseURL: "http://internal"}
	defer func() {
		for _, s := range b.sessions {
			b.close(s)
		}
	}()
	started := 0
	start := func(context.Context, JobParams) error { started++; return nil }
	for i := 0; i < maxBrowsers; i++ {
		p := testParams(OpList)
		p.Server = string(rune('a' + i))
		p.OpID = p.Server
		if _, err := b.session(context.Background(), p, start); err != nil {
			t.Fatal(err)
		}
	}
	p := testParams(OpRead)
	if _, err := b.session(context.Background(), p, start); err != errBrowserFull || started != maxBrowsers {
		t.Fatalf("capacity: err=%v, started=%d", err, started)
	}
	for _, s := range b.sessions {
		b.close(s)
		break
	}
	if _, err := b.session(context.Background(), p, start); err != nil || started != maxBrowsers+1 {
		t.Fatalf("reopen: err=%v, started=%d", err, started)
	}
}

func TestBrowseRefusesMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "keep.txt")
	os.WriteFile(path, []byte("keep"), 0600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(BrowseCommand{ID: "x", Op: OpDelete, Path: "keep.txt"})
	}))
	defer srv.Close()
	if err := Browse(context.Background(), root, srv.URL, "token"); err == nil {
		t.Fatal("worker accepted a write command")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "keep" {
		t.Fatalf("file changed: %q, %v", got, err)
	}
	p := testParams(OpWrite)
	p.BrowserURL = srv.URL
	p.BrowserToken = "token"
	if _, err := FilesJob(p); err == nil {
		t.Fatal("writable browser Job was allowed")
	}
}
