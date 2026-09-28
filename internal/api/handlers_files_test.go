package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
)

// fakeFileEditor records what the handlers ask the executor to do and returns
// canned results. The real executor runs a Job and reads its log back; none of
// that is the handlers' business, so the fake collapses it to "what was asked,
// and what came back".
type fakeFileEditor struct {
	err error

	calls         int
	gotOp         string
	gotServer     string
	gotPath       string
	gotContent    []byte
	gotExpect     string
	gotCreateOnly bool
	gotTo         string
	gotSource     fileedit.UploadSource
	gotOverwrite  bool
	// onUpload, when set, runs inside Upload the way the real Job fetches the
	// staged bytes while the handler waits.
	onUpload func(fileedit.UploadSource)

	entries   []fileedit.Entry
	truncated bool
	free      int64
	content   []byte
	sum       string

	// op is what StartUpload and StartUnzip answer (started), ops what Ops does.
	op  fileedit.OpState
	ops []fileedit.OpState
}

func (f *fakeFileEditor) List(_ context.Context, server, path string) (fileedit.Listing, error) {
	f.calls++
	f.gotServer, f.gotPath = server, path
	return fileedit.Listing{Entries: f.entries, Truncated: f.truncated, Free: f.free}, f.err
}

func (f *fakeFileEditor) Read(_ context.Context, server, path string) ([]byte, string, error) {
	f.calls++
	f.gotServer, f.gotPath = server, path
	return f.content, f.sum, f.err
}

func (f *fakeFileEditor) Write(_ context.Context, server, path string, content []byte, expect string, createOnly bool) (string, error) {
	f.calls++
	f.gotOp = fileedit.OpWrite
	f.gotServer, f.gotPath, f.gotContent, f.gotExpect, f.gotCreateOnly = server, path, content, expect, createOnly
	return f.sum, f.err
}

func (f *fakeFileEditor) Mkdir(_ context.Context, server, path string) error {
	f.calls++
	f.gotOp, f.gotServer, f.gotPath = fileedit.OpMkdir, server, path
	return f.err
}

func (f *fakeFileEditor) Delete(_ context.Context, server, path string) error {
	f.calls++
	f.gotOp, f.gotServer, f.gotPath = fileedit.OpDelete, server, path
	return f.err
}

func (f *fakeFileEditor) Rename(_ context.Context, server, path, to string) error {
	f.calls++
	f.gotOp, f.gotServer, f.gotPath, f.gotTo = fileedit.OpRename, server, path, to
	return f.err
}

func (f *fakeFileEditor) Upload(_ context.Context, server, path string, src fileedit.UploadSource, overwrite bool) error {
	f.calls++
	f.gotOp, f.gotServer, f.gotPath, f.gotSource, f.gotOverwrite = fileedit.OpUpload, server, path, src, overwrite
	if f.onUpload != nil {
		f.onUpload(src)
	}
	return f.err
}

func (f *fakeFileEditor) StartUpload(_ context.Context, server, path string, src fileedit.UploadSource, overwrite bool) (fileedit.OpState, error) {
	f.calls++
	f.gotOp, f.gotServer, f.gotPath, f.gotSource, f.gotOverwrite = fileedit.OpUpload, server, path, src, overwrite
	return f.started(fileedit.OpUpload, path), f.err
}

func (f *fakeFileEditor) StartUnzip(_ context.Context, server, path string, overwrite bool) (fileedit.OpState, error) {
	f.calls++
	f.gotOp, f.gotServer, f.gotPath, f.gotOverwrite = fileedit.OpUnzip, server, path, overwrite
	return f.started(fileedit.OpUnzip, path), f.err
}

// started is the op StartUpload and StartUnzip answer: f.op when a test set
// one, else a running op as the real Editor answers it.
func (f *fakeFileEditor) started(op, path string) fileedit.OpState {
	if f.op.ID != "" {
		return f.op
	}
	return fileedit.OpState{ID: "op" + strconv.Itoa(f.calls), Op: op, Path: path, State: fileedit.OpRunning, Started: time.Now()}
}

func (f *fakeFileEditor) Ops(_ context.Context, server string) ([]fileedit.OpState, error) {
	f.calls++
	f.gotServer = server
	return f.ops, f.err
}

// fileRouteHeader is the Content-Type a file route's body goes with: raw bytes
// for an upload, JSON for any other body.
func fileRouteHeader(name, body string) map[string]string {
	switch {
	case name == "upload":
		return ctHeader("application/octet-stream")
	case body != "":
		return jsonHeader
	}
	return nil
}

// testSum is a well-formed sha256 hex digest for the fake to hand out.
var testSum = strings.Repeat("a", 64)

// mkFiles builds an API whose "survival" server is STOPPED and owned by owner1,
// with a wired fakeFileEditor — the state in which every file operation is
// permitted, so each subtest changes exactly the one thing it is about.
//
// Uploads stage in a per-test directory. MinFree is near zero because the
// staging floor is fileedit's to test, and the machine running the tests may
// well have less than 10% of its disk free.
func mkFiles(t *testing.T) (*API, *fakeRepo, *fakeCluster, *fakeFileEditor) {
	t.Helper()
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped",
		Ready: false, DesiredState: string(v1alpha1.DesiredStopped)}
	files := &fakeFileEditor{}
	api := newTestAPI(repo, cl)
	api.Files = files
	api.FileStage = &fileedit.Stage{Dir: t.TempDir(), MinFree: 1e-9}
	api.InternalBaseURL = "http://felis-api-internal.felis.svc.cluster.local:8081"
	return api, repo, cl, files
}

// TestFileEditorStoppedGate is the gate this whole subsystem hinges on. The world
// PVC is ReadWriteOnce, but RWO is per node: on a single node a file Job mounts it
// right beside a running server, and a write lands under a live world that the
// server's next save overwrites or tears. Every route
// must therefore refuse a non-stopped server with 409 not_stopped BEFORE reaching
// the executor, which is why each asserts calls == 0 as well as the status.
func TestFileEditorStoppedGate(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	routes := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list", "GET", "/api/v1/servers/survival/files?path=config", ""},
		{"read", "GET", "/api/v1/servers/survival/file?path=server.properties", ""},
		{"write", "PUT", "/api/v1/servers/survival/file?path=server.properties", `{"content":"aGk="}`},
		{"mkdir", "POST", "/api/v1/servers/survival/files/mkdir?path=plugins", ""},
		{"delete", "DELETE", "/api/v1/servers/survival/file?path=old.jar", ""},
		{"rename", "POST", "/api/v1/servers/survival/files/rename?path=a.txt", `{"to":"b.txt"}`},
		{"upload", "PUT", "/api/v1/servers/survival/files/upload?path=plugins/x.jar", "PK-jar-bytes"},
	}

	// Both non-stopped shapes matter and they are different states: a server that is
	// UP (Ready) plainly holds the volume, but so does one that is merely coming up
	// (desiredState=Running, not yet Ready) — the gate keys on intent as well as
	// readiness, exactly as the backup/restore gates do.
	states := []struct {
		name         string
		ready        bool
		desiredState v1alpha1.DesiredState
	}{
		{"running", true, v1alpha1.DesiredRunning},
		{"starting", false, v1alpha1.DesiredRunning},
	}

	for _, rt := range routes {
		for _, st := range states {
			t.Run(fmt.Sprintf("%s on a %s server -> 409 not_stopped", rt.name, st.name), func(t *testing.T) {
				api, _, cl, files := mkFiles(t)
				cl.byName["survival"].Ready = st.ready
				cl.byName["survival"].DesiredState = string(st.desiredState)
				api.External = staticExternal{p: owner}

				w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, fileRouteHeader(rt.name, rt.body))
				if w.Code != http.StatusConflict || decodeErr(t, w) != "not_stopped" {
					t.Fatalf("code = %d body %s", w.Code, w.Body.String())
				}
				if files.calls != 0 {
					t.Fatal("a running server holds the RWO world PVC — the file Job must never be created")
				}
			})
		}
	}
}

// TestFileEditorWorldVolumeGate pins the second physical gate: a server with no
// world PVC (never started, or already reaped) has no claim for the Job to mount,
// so its Pod would sit Pending until the executor's wait timed out — a 90s hang
// and a misleading 504 files_timeout for a request that is knowably impossible.
// Every route must refuse BEFORE creating a Job, with the same specific 409
// the backup/restore faces use.
func TestFileEditorWorldVolumeGate(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	routes := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list", "GET", "/api/v1/servers/survival/files?path=config", ""},
		{"read", "GET", "/api/v1/servers/survival/file?path=server.properties", ""},
		{"write", "PUT", "/api/v1/servers/survival/file?path=server.properties", `{"content":"aGk="}`},
		{"mkdir", "POST", "/api/v1/servers/survival/files/mkdir?path=plugins", ""},
		{"delete", "DELETE", "/api/v1/servers/survival/file?path=old.jar", ""},
		{"rename", "POST", "/api/v1/servers/survival/files/rename?path=a.txt", `{"to":"b.txt"}`},
		{"upload", "PUT", "/api/v1/servers/survival/files/upload?path=plugins/x.jar", "PK-jar-bytes"},
	}

	for _, rt := range routes {
		t.Run(rt.name+" without a world volume -> 409 no_world_volume", func(t *testing.T) {
			api, _, cl, files := mkFiles(t)
			cl.noWorld["survival"] = true
			api.External = staticExternal{p: owner}

			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, fileRouteHeader(rt.name, rt.body))
			if w.Code != http.StatusConflict || decodeErr(t, w) != "no_world_volume" {
				t.Fatalf("code = %d body %s", w.Code, w.Body.String())
			}
			if files.calls != 0 {
				t.Fatal("no claim to mount — the file Job must never be created")
			}
		})
	}
}

// TestFileEditorAuthorization pins who may touch a world's files. It is the same
// owner-or-admin rule the backup routes enforce, and it must hold on every
// route — a read-only route leaking another owner's config (an RCON password
// lives in server.properties) would be as bad as an unauthorized write.
func TestFileEditorAuthorization(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	stranger := &Principal{UserID: "stranger", Email: "stranger@example.net", Role: "user"}
	admin := &Principal{UserID: "admin1", Email: "admin1@example.net", Role: "admin", ViaAdminAccess: true}

	routes := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list", "GET", "/api/v1/servers/survival/files", ""},
		{"read", "GET", "/api/v1/servers/survival/file?path=server.properties", ""},
		{"write", "PUT", "/api/v1/servers/survival/file?path=server.properties", `{"content":"aGk="}`},
		{"mkdir", "POST", "/api/v1/servers/survival/files/mkdir?path=plugins", ""},
		{"delete", "DELETE", "/api/v1/servers/survival/file?path=old.jar", ""},
		{"rename", "POST", "/api/v1/servers/survival/files/rename?path=a.txt", `{"to":"b.txt"}`},
		{"upload", "PUT", "/api/v1/servers/survival/files/upload?path=plugins/x.jar", "PK-jar-bytes"},
	}

	for _, rt := range routes {
		t.Run(rt.name+": non-owner -> 403, executor untouched", func(t *testing.T) {
			api, _, _, files := mkFiles(t)
			api.External = staticExternal{p: stranger}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, fileRouteHeader(rt.name, rt.body))
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
			}
			if files.calls != 0 {
				t.Fatal("a forbidden caller must not reach the file executor")
			}
		})

		t.Run(rt.name+": owner -> allowed", func(t *testing.T) {
			api, _, _, files := mkFiles(t)
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, fileRouteHeader(rt.name, rt.body))
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
			}
			if files.calls != 1 || files.gotServer != "survival" {
				t.Fatalf("executor saw (calls=%d, server=%q)", files.calls, files.gotServer)
			}
		})

		t.Run(rt.name+": admin on someone else's server -> allowed", func(t *testing.T) {
			api, repo, _, files := mkFiles(t)
			repo.byName["survival"].OwnerID = "someone-else"
			api.External = staticExternal{p: admin}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, fileRouteHeader(rt.name, rt.body))
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
			}
			if files.calls != 1 {
				t.Fatal("admin should reach the executor")
			}
		})

		t.Run(rt.name+": unowned server -> 403 for a plain user", func(t *testing.T) {
			api, repo, _, _ := mkFiles(t)
			repo.byName["survival"].OwnerID = "" // released world
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, fileRouteHeader(rt.name, rt.body))
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
			}
		})

		t.Run(rt.name+": unknown server -> 404", func(t *testing.T) {
			api, _, _, _ := mkFiles(t)
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method,
				strings.Replace(rt.path, "survival", "missing", 1), rt.body, fileRouteHeader(rt.name, rt.body))
			if w.Code != http.StatusNotFound {
				t.Fatalf("code = %d, want 404 (%s)", w.Code, w.Body.String())
			}
		})

		t.Run(rt.name+": invalid server name -> 400 bad_name", func(t *testing.T) {
			api, _, _, _ := mkFiles(t)
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method,
				strings.Replace(rt.path, "survival", "X", 1), rt.body, fileRouteHeader(rt.name, rt.body))
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_name" {
				t.Fatalf("code = %d body %s", w.Code, w.Body.String())
			}
		})

		t.Run(rt.name+": nil FileEditor -> 503 files_unavailable", func(t *testing.T) {
			api, _, _, _ := mkFiles(t)
			api.Files = nil
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, fileRouteHeader(rt.name, rt.body))
			if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "files_unavailable" {
				t.Fatalf("code = %d body %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestFileEditorHandlers covers the per-route behaviour the shared gate does not:
// what is passed through to the executor and what comes back.
func TestFileEditorHandlers(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	t.Run("list passes the path through and returns entries", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		files.entries = []fileedit.Entry{{Name: "paper.yml", Size: 12}, {Name: "sub", IsDir: true}}
		files.truncated = true
		files.free = 5 << 30
		api.External = staticExternal{p: owner}

		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/files?path=config", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Path      string           `json:"path"`
			Entries   []fileedit.Entry `json:"entries"`
			Truncated bool             `json:"truncated"`
			FreeBytes int64            `json:"free_bytes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if files.gotPath != "config" {
			t.Fatalf("executor saw path %q, want the query value verbatim", files.gotPath)
		}
		if resp.Path != "config" || len(resp.Entries) != 2 || !resp.Truncated || resp.FreeBytes != 5<<30 {
			t.Fatalf("unexpected response %+v", resp)
		}
	})

	t.Run("list without a path lists the world root", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/files", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if files.gotPath != "" {
			t.Fatalf("path = %q, want empty (the root)", files.gotPath)
		}
	})

	t.Run("read returns base64 content and its hash", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		files.content = []byte("motd=hello\n")
		files.sum = testSum
		api.External = staticExternal{p: owner}

		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/file?path=server.properties", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
			SHA256  string `json:"sha256"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if resp.Path != "server.properties" || string(resp.Content) != "motd=hello\n" || resp.SHA256 != testSum {
			t.Fatalf("unexpected response %+v (%q)", resp, resp.Content)
		}
	})

	t.Run("read without a path -> 400", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/file", "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
		if files.calls != 0 {
			t.Fatal("a pathless read must not reach the executor")
		}
	})

	t.Run("write decodes content, audits, and answers 200", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}

		w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=server.properties",
			`{"content":"bW90ZD1jaGFuZ2VkCg=="}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if string(files.gotContent) != "motd=changed\n" {
			t.Fatalf("executor got content %q, want the decoded bytes", files.gotContent)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "file.write" ||
			repo.audits[0].Actor != "owner1@example.net" {
			t.Fatalf("write not audited as expected: %+v", repo.audits)
		}
	})

	t.Run("write passes the expected hash through and returns the new one", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		files.sum = strings.Repeat("b", 64)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=server.properties",
			`{"content":"aGk=","expect_sha256":"`+testSum+`"}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if files.gotExpect != testSum {
			t.Fatalf("executor got expect %q, want %q", files.gotExpect, testSum)
		}
		var resp struct {
			SHA256 string `json:"sha256"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.SHA256 != files.sum {
			t.Fatalf("response sha256 = %q (%v), want %q", resp.SHA256, err, files.sum)
		}
	})

	t.Run("a malformed expected hash -> 400 before the executor", func(t *testing.T) {
		for _, bad := range []string{"abc", strings.Repeat("A", 64), strings.Repeat("a", 63) + " ", "--op=list"} {
			api, _, _, files := mkFiles(t)
			api.External = staticExternal{p: owner}
			body, _ := json.Marshal(map[string]any{"content": []byte("hi"), "expect_sha256": bad})
			w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=server.properties",
				string(body), jsonHeader)
			if w.Code != http.StatusBadRequest || files.calls != 0 {
				t.Fatalf("expect %q: code = %d calls = %d, want 400 and no Job", bad, w.Code, files.calls)
			}
		}
	})

	t.Run("a stale write -> 409 file_changed, not audited", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		files.err = fmt.Errorf("%w: server.properties has changed", fileedit.ErrConflict)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=server.properties",
			`{"content":"aGk=","expect_sha256":"`+testSum+`"}`, jsonHeader)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "file_changed" {
			t.Fatalf("code = %d body %s, want 409 file_changed", w.Code, w.Body.String())
		}
		if len(repo.audits) != 0 {
			t.Fatalf("a refused write was audited: %+v", repo.audits)
		}
	})

	t.Run("reads are not audited", func(t *testing.T) {
		api, repo, _, _ := mkFiles(t)
		api.External = staticExternal{p: owner}
		do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/files", "", nil)
		do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/file?path=x", "", nil)
		if len(repo.audits) != 0 {
			t.Fatalf("reads should not write audit rows: %+v", repo.audits)
		}
	})

	t.Run("oversized write -> 413 before the executor", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		// base64 of MaxWriteBytes+1 zero bytes, built as a JSON body.
		body, err := json.Marshal(writeFileRequest{Content: bytesPtr(make([]byte, fileedit.MaxWriteBytes+1))})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=big.txt",
			string(body), jsonHeader)
		if w.Code != http.StatusRequestEntityTooLarge || decodeErr(t, w) != "too_large" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if files.calls != 0 {
			t.Fatal("an oversized write must be refused before a Job is rendered")
		}
	})

	// A body that omits "content" must be refused, not treated as empty. Before
	// Content became a *[]byte, {} decoded to nil and travelled all the way to
	// O_TRUNC — so a client serialisation bug answered 200 while zeroing the very
	// config the caller opened the editor to repair. An explicit "" stays legal,
	// because a deliberate truncate is a real edit; only the OMISSION is refused.
	t.Run("a write with no content field -> 400, never a truncate", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"content":null}`} {
			api, _, _, files := mkFiles(t)
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=server.properties",
				body, jsonHeader)
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
				t.Fatalf("body %s: code = %d, want 400 bad_request (%s)", body, w.Code, w.Body.String())
			}
			if files.calls != 0 {
				t.Fatalf("body %s: reached the executor; an omitted content field must never truncate", body)
			}
		}
	})

	t.Run("an explicit empty content is a legitimate truncate", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=server.properties",
			`{"content":""}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		if files.calls != 1 || len(files.gotContent) != 0 {
			t.Fatalf("calls = %d, content = %d bytes; want one call writing 0 bytes",
				files.calls, len(files.gotContent))
		}
	})

	t.Run("a write at exactly the limit is allowed", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		body, err := json.Marshal(writeFileRequest{Content: bytesPtr(make([]byte, fileedit.MaxWriteBytes))})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=big.txt",
			string(body), jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 — the limit is inclusive (%s)", w.Code, w.Body.String())
		}
		if len(files.gotContent) != fileedit.MaxWriteBytes {
			t.Fatalf("executor got %d bytes, want %d", len(files.gotContent), fileedit.MaxWriteBytes)
		}
	})
}

// fileAnswer decodes a file route's JSON answer for an exact comparison.
func fileAnswer(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
	}
	return m
}

// onlyAudit asserts the request wrote exactly one audit row, by owner1, with
// this action, target and payload ("" for none).
func onlyAudit(t *testing.T, repo *fakeRepo, action, target, payload string) {
	t.Helper()
	if len(repo.audits) != 1 {
		t.Fatalf("audits = %+v, want exactly one %s", repo.audits, action)
	}
	a := repo.audits[0]
	if a.Action != action || a.ServerName != target || a.Actor != "owner1@example.net" ||
		a.ActorUserID != "owner1" || string(a.Payload) != payload {
		t.Fatalf("audit = %+v (payload %s), want %s on %s with payload %q", a, a.Payload, action, target, payload)
	}
}

// TestFileManagerHandlers covers the routes that change a world's file tree
// beyond a save: what each hands the executor, what it answers, what it audits.
func TestFileManagerHandlers(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	t.Run("mkdir makes the folder and audits it", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/files/mkdir?path=plugins/Essentials", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if files.calls != 1 || files.gotOp != fileedit.OpMkdir || files.gotPath != "plugins/Essentials" {
			t.Fatalf("executor saw %d calls, op %q, path %q", files.calls, files.gotOp, files.gotPath)
		}
		if got, want := fileAnswer(t, w), (map[string]any{"path": "plugins/Essentials", "status": "created"}); !reflect.DeepEqual(got, want) {
			t.Fatalf("answer = %v, want %v", got, want)
		}
		onlyAudit(t, repo, "file.mkdir", "survival:plugins/Essentials", "")
	})

	t.Run("delete removes the path and audits it", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "DELETE", "/api/v1/servers/survival/file?path=plugins/old.jar", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if files.calls != 1 || files.gotOp != fileedit.OpDelete || files.gotPath != "plugins/old.jar" {
			t.Fatalf("executor saw %d calls, op %q, path %q", files.calls, files.gotOp, files.gotPath)
		}
		if got, want := fileAnswer(t, w), (map[string]any{"path": "plugins/old.jar", "status": "deleted"}); !reflect.DeepEqual(got, want) {
			t.Fatalf("answer = %v, want %v", got, want)
		}
		onlyAudit(t, repo, "file.delete", "survival:plugins/old.jar", "")
	})

	t.Run("rename passes the destination and audits both names", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/files/rename?path=plugins/a.jar",
			`{"to":"plugins/disabled/a.jar"}`, jsonHeader)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if files.calls != 1 || files.gotOp != fileedit.OpRename || files.gotPath != "plugins/a.jar" || files.gotTo != "plugins/disabled/a.jar" {
			t.Fatalf("executor saw %d calls, op %q, %q -> %q", files.calls, files.gotOp, files.gotPath, files.gotTo)
		}
		want := map[string]any{"path": "plugins/a.jar", "to": "plugins/disabled/a.jar", "status": "renamed"}
		if got := fileAnswer(t, w); !reflect.DeepEqual(got, want) {
			t.Fatalf("answer = %v, want %v", got, want)
		}
		onlyAudit(t, repo, "file.rename", "survival:plugins/a.jar", `{"to":"plugins/disabled/a.jar"}`)
	})

	t.Run("rename without a destination -> 400 before the executor", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"to":""}`} {
			api, _, _, files := mkFiles(t)
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/files/rename?path=a.txt", body, jsonHeader)
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" || files.calls != 0 {
				t.Fatalf("body %s: code = %d calls = %d (%s), want 400 and no Job", body, w.Code, files.calls, w.Body.String())
			}
		}
	})

	t.Run("a route that changes a path needs the path", func(t *testing.T) {
		for _, rt := range []struct{ method, path, body string }{
			{"POST", "/api/v1/servers/survival/files/mkdir", ""},
			{"DELETE", "/api/v1/servers/survival/file", ""},
			{"POST", "/api/v1/servers/survival/files/rename", `{"to":"b.txt"}`},
			{"PUT", "/api/v1/servers/survival/files/upload", "bytes"},
		} {
			api, _, _, files := mkFiles(t)
			api.External = staticExternal{p: owner}
			hdr := jsonHeader
			if rt.method == "PUT" {
				hdr = ctHeader("application/octet-stream")
			}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, hdr)
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" || files.calls != 0 {
				t.Fatalf("%s %s: code = %d calls = %d (%s), want 400 and no Job", rt.method, rt.path, w.Code, files.calls, w.Body.String())
			}
		}
	})

	t.Run("a refused change is not audited", func(t *testing.T) {
		for _, rt := range []struct{ method, path, body string }{
			{"POST", "/api/v1/servers/survival/files/mkdir?path=plugins", ""},
			{"DELETE", "/api/v1/servers/survival/file?path=plugins", ""},
			{"POST", "/api/v1/servers/survival/files/rename?path=a.txt", `{"to":"b.txt"}`},
		} {
			api, repo, _, files := mkFiles(t)
			files.err = fmt.Errorf("%w: plugins already exists", fileedit.ErrExists)
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, jsonHeader)
			if w.Code != http.StatusConflict || decodeErr(t, w) != "file_exists" {
				t.Fatalf("%s %s: code = %d (%s), want 409 file_exists", rt.method, rt.path, w.Code, w.Body.String())
			}
			if len(repo.audits) != 0 {
				t.Fatalf("%s %s: a refused change was audited: %+v", rt.method, rt.path, repo.audits)
			}
		}
	})

	t.Run("create_only reaches the executor", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=plugins/new.yml",
			`{"content":"","create_only":true}`, jsonHeader)
		if w.Code != http.StatusOK || !files.gotCreateOnly || files.gotExpect != "" {
			t.Fatalf("code = %d, createOnly = %v, expect = %q (%s)", w.Code, files.gotCreateOnly, files.gotExpect, w.Body.String())
		}
		w = do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=server.properties",
			`{"content":"aGk="}`, jsonHeader)
		if w.Code != http.StatusOK || files.gotCreateOnly {
			t.Fatalf("a plain save: code = %d, createOnly = %v", w.Code, files.gotCreateOnly)
		}
	})

	t.Run("create_only with an expected hash -> 400", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/survival/file?path=a.yml",
			`{"content":"","create_only":true,"expect_sha256":"`+testSum+`"}`, jsonHeader)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" || files.calls != 0 {
			t.Fatalf("code = %d calls = %d (%s), want 400 and no Job", w.Code, files.calls, w.Body.String())
		}
	})
}

// doUpload sends an upload whose Content-Length is declared, not measured, the
// way a client that streams or lies would send it.
func doUpload(h http.Handler, body string, length int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest("PUT", "/api/v1/servers/survival/files/upload?path=plugins/x.jar", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/octet-stream")
	r.ContentLength = length
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	recordContract(r, body, w)
	return w
}

// stageEmpty asserts no staged upload is left on disk.
func stageEmpty(t *testing.T, api *API) {
	t.Helper()
	left, err := os.ReadDir(api.FileStage.Dir)
	if err != nil {
		t.Fatalf("read the stage: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("staged files left behind: %v", left)
	}
}

// TestFileUpload drives an upload across both faces: the external PUT stages the
// body, and the executor, standing in for the Job, fetches it from the internal
// face with the token it was handed, as cmd/felis files does.
func TestFileUpload(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	const body = "PK\x03\x04 a plugin jar"
	digest := sha256.Sum256([]byte(body))
	sum := hex.EncodeToString(digest[:])
	const route = "/api/v1/servers/survival/files/upload?path=plugins/x.jar"
	octet := ctHeader("application/octet-stream")

	t.Run("the Job fetches the body once, with its token alone", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		// Every other internal route wants a service token; the Job has none.
		api.Internal = CallerTokens{CallerVelocity: "s3cr3t"}
		prefix := api.InternalBaseURL + "/api/v1/internal/file-uploads/"
		var bare, wrong, unschemed, fetched, again *httptest.ResponseRecorder
		files.onUpload = func(src fileedit.UploadSource) {
			id, ok := strings.CutPrefix(src.URL, prefix)
			if !ok || len(id) != 32 {
				t.Errorf("source URL %q is not one id under %q", src.URL, prefix)
				return
			}
			h := api.InternalHandler()
			at := "/api/v1/internal/file-uploads/" + id
			bare = do(h, "GET", at, "", nil)
			wrong = do(h, "GET", at, "", map[string]string{"Authorization": "Bearer " + strings.Repeat("0", len(src.Token))})
			unschemed = do(h, "GET", at, "", map[string]string{"Authorization": src.Token})
			fetched = do(h, "GET", at, "", map[string]string{"Authorization": "Bearer " + src.Token})
			again = do(h, "GET", at, "", map[string]string{"Authorization": "Bearer " + src.Token})
		}

		w := do(api.ExternalHandler(), "PUT", route, body, octet)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if fetched == nil {
			t.Fatal("the executor never fetched the upload")
		}
		for name, r := range map[string]*httptest.ResponseRecorder{
			"no token": bare, "wrong token": wrong, "the token without Bearer": unschemed, "second fetch": again,
		} {
			if r.Code != http.StatusNotFound || decodeErr(t, r) != "not_found" {
				t.Errorf("%s: code = %d (%s), want 404 not_found", name, r.Code, r.Body.String())
			}
		}
		if fetched.Code != http.StatusOK || fetched.Body.String() != body ||
			fetched.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
			t.Fatalf("fetch: code = %d, %q, Content-Length %q", fetched.Code, fetched.Body.String(), fetched.Header().Get("Content-Length"))
		}
		if files.gotPath != "plugins/x.jar" || files.gotSource.Size != int64(len(body)) ||
			files.gotSource.SHA256 != sum || files.gotOverwrite {
			t.Fatalf("executor saw path %q, source %+v, overwrite %v", files.gotPath, files.gotSource, files.gotOverwrite)
		}
		want := map[string]any{"path": "plugins/x.jar", "status": "uploaded", "sha256": sum, "size": float64(len(body))}
		if got := fileAnswer(t, w); !reflect.DeepEqual(got, want) {
			t.Fatalf("answer = %v, want %v", got, want)
		}
		onlyAudit(t, repo, "file.upload", "survival:plugins/x.jar",
			`{"overwrite":false,"sha256":"`+sum+`","size_bytes":`+strconv.Itoa(len(body))+`}`)
		stageEmpty(t, api)
	})

	t.Run("overwrite=true reaches the executor", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "PUT", route+"&overwrite=true", body, octet)
		if w.Code != http.StatusOK || !files.gotOverwrite {
			t.Fatalf("code = %d, overwrite = %v (%s)", w.Code, files.gotOverwrite, w.Body.String())
		}
		onlyAudit(t, repo, "file.upload", "survival:plugins/x.jar",
			`{"overwrite":true,"sha256":"`+sum+`","size_bytes":`+strconv.Itoa(len(body))+`}`)
	})

	t.Run("a refused upload is not audited and its bytes are dropped", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		files.err = fmt.Errorf("%w: plugins/x.jar already exists", fileedit.ErrExists)
		w := do(api.ExternalHandler(), "PUT", route, body, octet)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "file_exists" {
			t.Fatalf("code = %d (%s), want 409 file_exists", w.Code, w.Body.String())
		}
		if len(repo.audits) != 0 {
			t.Fatalf("a refused upload was audited: %+v", repo.audits)
		}
		stageEmpty(t, api)
	})

	t.Run("a lock that cannot be taken drops the staged bytes", func(t *testing.T) {
		api, _, cl, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		cl.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindBackup}
		w := do(api.ExternalHandler(), "PUT", route, body, octet)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "maintenance_in_progress" || files.calls != 0 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
		stageEmpty(t, api)
	})

	t.Run("no Content-Length -> 411", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := doUpload(api.ExternalHandler(), body, -1)
		if w.Code != http.StatusLengthRequired || decodeErr(t, w) != "length_required" || files.calls != 0 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
	})

	t.Run("a declared size over the cap -> 413 before a byte is staged", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := doUpload(api.ExternalHandler(), body, fileedit.MaxUploadBytes+1)
		if w.Code != http.StatusRequestEntityTooLarge || decodeErr(t, w) != "too_large" || files.calls != 0 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
		stageEmpty(t, api)
	})

	// Staged, and so short: the body is a few bytes of a declared 64 MiB.
	t.Run("a declared size at the cap is taken", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := doUpload(api.ExternalHandler(), body, fileedit.MaxUploadBytes)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "upload_incomplete" || files.calls != 0 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
	})

	t.Run("a body shorter than its Content-Length -> 400 upload_incomplete", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		w := doUpload(api.ExternalHandler(), body, int64(len(body))+1)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "upload_incomplete" || files.calls != 0 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
		stageEmpty(t, api)
	})

	t.Run("a staging disk at its floor -> 507", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: owner}
		api.FileStage.MinFree = 1
		w := do(api.ExternalHandler(), "PUT", route, body, octet)
		if w.Code != http.StatusInsufficientStorage || decodeErr(t, w) != "upload_staging_full" || files.calls != 0 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
	})

	t.Run("uploads not wired -> 503", func(t *testing.T) {
		for name, unwire := range map[string]func(*API){
			"no stage":        func(a *API) { a.FileStage = nil },
			"no internal URL": func(a *API) { a.InternalBaseURL = "" },
		} {
			api, _, _, files := mkFiles(t)
			api.External = staticExternal{p: owner}
			unwire(api)
			w := do(api.ExternalHandler(), "PUT", route, body, octet)
			if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "files_unavailable" || files.calls != 0 {
				t.Fatalf("%s: code = %d calls = %d (%s)", name, w.Code, files.calls, w.Body.String())
			}
		}
	})

	t.Run("the internal route without a stage -> 404", func(t *testing.T) {
		api, _, _, _ := mkFiles(t)
		api.FileStage = nil
		w := do(api.InternalHandler(), "GET", "/api/v1/internal/file-uploads/00112233445566778899aabbccddeeff", "",
			map[string]string{"Authorization": "Bearer t"})
		if w.Code != http.StatusNotFound || decodeErr(t, w) != "not_found" {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
	})
}

// TestFileEditorErrorMapping proves each executor sentinel reaches the caller as the
// right status. The containment refusal mapping to 400 (not 403) is the one worth
// stating: an escaping path is a malformed request, not a permission a caller might
// be granted.
func TestFileEditorErrorMapping(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	cases := []struct {
		name     string
		err      error
		wantCode int
		wantErr  string
	}{
		{"escaping path", fmt.Errorf("%w: nope", fileedit.ErrBadPath), http.StatusBadRequest, "bad_path"},
		{"missing file", fmt.Errorf("%w: nope", fileedit.ErrNotFound), http.StatusNotFound, "not_found"},
		{"oversized file", fmt.Errorf("%w: nope", fileedit.ErrTooLarge), http.StatusRequestEntityTooLarge, "too_large"},
		{"changed since read", fmt.Errorf("%w: nope", fileedit.ErrConflict), http.StatusConflict, "file_changed"},
		{"volume full", fmt.Errorf("%w: nope", fileedit.ErrNoSpace), http.StatusInsufficientStorage, "volume_full"},
		{"already there", fmt.Errorf("%w: nope", fileedit.ErrExists), http.StatusConflict, "file_exists"},
		{"timeout", fmt.Errorf("waiting: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, "files_timeout"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, _, _, files := mkFiles(t)
			files.err = tc.err
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/file?path=x", "", nil)
			if w.Code != tc.wantCode || decodeErr(t, w) != tc.wantErr {
				t.Fatalf("code = %d body %s, want %d/%s", w.Code, w.Body.String(), tc.wantCode, tc.wantErr)
			}
		})
	}

	t.Run("an unrecognised executor failure -> 500", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		files.err = fmt.Errorf("the job pod exploded")
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/file?path=x", "", nil)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500 (%s)", w.Code, w.Body.String())
		}
	})
}

// bytesPtr builds the *[]byte writeFileRequest.Content wants. The pointer is what
// lets an omitted field be distinguished from an empty one; see the type's comment.
func bytesPtr(b []byte) *[]byte { return &b }
