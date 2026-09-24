package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type tarEntry struct {
	name     string
	body     string
	mode     int64
	typ      byte
	linkname string
}

// tgzBody builds an in-memory .tar.gz from entries, preserving each entry's type
// and mode so the tests can exercise the guards with exactly the bytes an
// attacker could upload.
func tgzBody(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Size: int64(len(e.body))}
		if typ == tar.TypeSymlink {
			hdr.Linkname = e.linkname
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// A normal context extracts with its tree intact, and the executable bit that
// modpack entrypoints rely on survives.
func TestExtractTarGzRoundTrip(t *testing.T) {
	dir := t.TempDir()
	body := tgzBody(t,
		tarEntry{name: "Dockerfile", body: "FROM scratch\n"},
		tarEntry{name: "mods/example.jar", body: "jar-bytes"},
		tarEntry{name: "start.sh", body: "#!/bin/sh\n", mode: 0o755},
		tarEntry{name: "mods/", typ: tar.TypeDir, mode: 0o755},
	)
	if err := extractTarGz(bytes.NewReader(body), dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for name, want := range map[string]string{
		"Dockerfile":       "FROM scratch\n",
		"mods/example.jar": "jar-bytes",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = (%q, %v), want %q", name, got, err, want)
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "start.sh"))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("entrypoint script lost its exec bit: %v (%v)", fi, err)
	}
}

// The guards: "..", absolute paths, symlinks, and special files are refused whole
// — nothing escapes, and nothing is silently skipped.
func TestExtractTarGzRefusesEscapes(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"dotdot", []tarEntry{{name: "../outside", body: "x"}}},
		{"nested dotdot", []tarEntry{{name: "a/../../outside", body: "x"}}},
		{"absolute", []tarEntry{{name: "/etc/outside", body: "x"}}},
		{"symlink", []tarEntry{{name: "link", typ: tar.TypeSymlink, linkname: "/etc"}}},
		{"hardlink", []tarEntry{{name: "hard", typ: tar.TypeLink, linkname: "somewhere"}}},
		{"device", []tarEntry{{name: "dev", typ: tar.TypeChar}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := extractTarGz(bytes.NewReader(tgzBody(t, tc.entries...)), dir); err == nil {
				t.Fatal("extract accepted a hostile entry, want an error")
			}
			// Nothing may have been written outside the target (or at all).
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("hostile archive left %d entries behind", len(entries))
			}
		})
	}
}

// A context that expands past the byte or entry cap is refused, however small
// it was compressed: gzip bombs and inode floods stop at the cap.
func TestExtractTarGzCapsExpansion(t *testing.T) {
	bytesCap, entriesCap := maxContextBytes, maxContextEntries
	t.Cleanup(func() { maxContextBytes, maxContextEntries = bytesCap, entriesCap })
	maxContextBytes, maxContextEntries = 1000, 5

	fits := tgzBody(t, tarEntry{name: "a", body: strings.Repeat("x", 600)}, tarEntry{name: "b", body: strings.Repeat("y", 400)})
	if err := extractTarGz(bytes.NewReader(fits), t.TempDir()); err != nil {
		t.Fatalf("a context exactly at the byte cap: %v", err)
	}
	big := tgzBody(t, tarEntry{name: "a", body: strings.Repeat("x", 600)}, tarEntry{name: "b", body: strings.Repeat("y", 401)})
	if err := extractTarGz(bytes.NewReader(big), t.TempDir()); err == nil || !strings.Contains(err.Error(), "expands past") {
		t.Fatalf("one byte over the cap: err = %v", err)
	}
	var many []tarEntry
	for i := 0; i < 6; i++ {
		many = append(many, tarEntry{name: "d" + string(rune('0'+i)) + "/", typ: tar.TypeDir})
	}
	if err := extractTarGz(bytes.NewReader(tgzBody(t, many...)), t.TempDir()); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("six entries over a cap of five: err = %v", err)
	}
}

// The command end to end: it dials the URL with the bearer token from the
// environment, and refuses to run without it (the internal face would 401
// anyway; failing at parse time is the honest earlier error).
func TestCmdFetchContextFetchAndExtract(t *testing.T) {
	body := tgzBody(t, tarEntry{name: "Dockerfile", body: "FROM scratch\n"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("FELIS_SERVICE_TOKEN", "test-token")
	if code := cmdFetchContext([]string{"--url=" + srv.URL + "/sub-1/context", "--out=" + dir}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("cmdFetchContext exit = %d, want 0", code)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil || string(got) != "FROM scratch\n" {
		t.Fatalf("extracted Dockerfile = (%q, %v)", got, err)
	}

	// No token: refuse before dialing.
	t.Setenv("FELIS_SERVICE_TOKEN", "")
	var stderr bytes.Buffer
	if code := cmdFetchContext([]string{"--url=" + srv.URL + "/sub-1/context", "--out=" + t.TempDir()}, io.Discard, &stderr); code != 2 {
		t.Fatalf("missing token exit = %d, want 2 (stderr %q)", code, stderr.String())
	}

	// A non-200 answer (e.g. the route's 404 for a never-uploaded context) fails.
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv404.Close()
	t.Setenv("FELIS_SERVICE_TOKEN", "test-token")
	if code := cmdFetchContext([]string{"--url=" + srv404.URL + "/sub-1/context", "--out=" + t.TempDir()}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("404 exit = %d, want 1", code)
	}
}

// A body that is not a gzip tarball must fail the extraction rather than produce
// an empty (or partial) context Kaniko would then try to build.
func TestExtractTarGzRejectsNonGzip(t *testing.T) {
	dir := t.TempDir()
	err := extractTarGz(strings.NewReader("not a tarball"), dir)
	if err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("err = %v, want a gzip complaint", err)
	}
}

// shrinkFetchWindow swaps the retry knobs for a faster test and restores them
// afterwards, so no test leaks a tiny window into another.
func shrinkFetchWindow(t *testing.T, interval, window time.Duration) {
	t.Helper()
	oldInterval, oldWindow := fetchRetryInterval, fetchRetryWindow
	fetchRetryInterval, fetchRetryWindow = interval, window
	t.Cleanup(func() { fetchRetryInterval, fetchRetryWindow = oldInterval, oldWindow })
}

// A control-plane blip mid-fetch is survived: a 5xx on the first attempt is
// retried and the second attempt's tarball extracts. This walks back the live
// drill's failure, where the api pod rolled mid-fetch and the single attempt
// died, failing the build Job.
func TestFetchContextRetriesThroughBlip(t *testing.T) {
	shrinkFetchWindow(t, 10*time.Millisecond, time.Second)
	body := tgzBody(t, tarEntry{name: "Dockerfile", body: "FROM scratch\n"})
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway) // the port is up, the API is not
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("FELIS_SERVICE_TOKEN", "test-token")
	var stderr bytes.Buffer
	if code := cmdFetchContext([]string{"--url=" + srv.URL + "/sub-1/context", "--out=" + dir}, io.Discard, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil || string(got) != "FROM scratch\n" {
		t.Fatalf("extracted Dockerfile = (%q, %v)", got, err)
	}
	if !strings.Contains(stderr.String(), "retrying") {
		t.Fatalf("stderr %q does not mention the retry", stderr.String())
	}
}

// The live drill's exact shape: the dial itself is refused (the api pod is
// gone and no endpoint answers). A refused dial is retried like any other
// transport failure, and once the face is back the fetch completes.
func TestFetchContextRetriesRefusedDial(t *testing.T) {
	shrinkFetchWindow(t, 10*time.Millisecond, 5*time.Second)
	body := tgzBody(t, tarEntry{name: "Dockerfile", body: "FROM scratch\n"})

	// Borrow a listen address, then close it: the first attempts dial into a
	// refused connection, exactly like a restarting control plane.
	probe := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(probe.URL, "http://")
	probe.Close()

	dir := t.TempDir()
	t.Setenv("FELIS_SERVICE_TOKEN", "test-token")
	var stderr bytes.Buffer
	// Start the fetch; while the retry loop burns refused dials, bring the same
	// address back.
	result := make(chan int, 1)
	go func() {
		result <- cmdFetchContext([]string{"--url=http://" + addr + "/sub-1/context", "--out=" + dir}, io.Discard, &stderr)
	}()
	time.Sleep(100 * time.Millisecond) // let a handful of dials be refused
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("rebind %s: %v", addr, err)
	}
	back := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(body)
	})}
	defer back.Close()
	go func() { _ = back.Serve(ln) }()

	code := <-result
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if got, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil || string(got) != "FROM scratch\n" {
		t.Fatalf("extracted Dockerfile = (%q, %v)", got, err)
	}
	if !strings.Contains(stderr.String(), "retrying") {
		t.Fatalf("stderr %q does not mention the retry", stderr.String())
	}
}

// A 4xx is an answer, not a blip: a missing/never-uploaded context fails
// immediately — no retry loop burns the build's deadline on a terminal error.
func TestFetchContextDoesNotRetry4xx(t *testing.T) {
	shrinkFetchWindow(t, 5*time.Millisecond, 200*time.Millisecond)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("FELIS_SERVICE_TOKEN", "test-token")
	var stderr bytes.Buffer
	if code := cmdFetchContext([]string{"--url=" + srv.URL + "/sub-1/context", "--out=" + t.TempDir()}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr.String())
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("server saw %d attempts, want exactly 1", got)
	}
	if strings.Contains(stderr.String(), "retrying") {
		t.Fatalf("stderr %q mentions a retry for a terminal 4xx", stderr.String())
	}
}

// The retry is bounded: an internal face that stays down does not hang the
// build pod; the window runs out and the fetch reports the exhausted retries.
func TestFetchContextGivesUpAfterWindow(t *testing.T) {
	shrinkFetchWindow(t, 5*time.Millisecond, 60*time.Millisecond)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close() // the face is up but never healthy: 503 forever

	t.Setenv("FELIS_SERVICE_TOKEN", "test-token")
	var stderr bytes.Buffer
	start := time.Now()
	if code := cmdFetchContext([]string{"--url=" + srv.URL + "/sub-1/context", "--out=" + t.TempDir()}, io.Discard, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr.String())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("gave up after %v; the window is supposed to bound it", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("server saw %d attempts, want at least one retry", got)
	}
	if !strings.Contains(stderr.String(), "retried for") {
		t.Fatalf("stderr %q does not report the exhausted retry window", stderr.String())
	}
}
