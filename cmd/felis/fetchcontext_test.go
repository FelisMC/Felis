package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
