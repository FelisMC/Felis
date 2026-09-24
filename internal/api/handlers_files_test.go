package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
)

// fakeFileEditor records what the handlers ask the executor to do and returns
// canned results. The real executor runs a Job and reads its log back; none of
// that is the handlers' business, so the fake collapses it to "what was asked,
// and what came back".
type fakeFileEditor struct {
	err error

	calls      int
	gotServer  string
	gotPath    string
	gotContent []byte

	entries   []fileedit.Entry
	truncated bool
	content   []byte
}

func (f *fakeFileEditor) List(_ context.Context, server, path string) ([]fileedit.Entry, bool, error) {
	f.calls++
	f.gotServer, f.gotPath = server, path
	return f.entries, f.truncated, f.err
}

func (f *fakeFileEditor) Read(_ context.Context, server, path string) ([]byte, error) {
	f.calls++
	f.gotServer, f.gotPath = server, path
	return f.content, f.err
}

func (f *fakeFileEditor) Write(_ context.Context, server, path string, content []byte) error {
	f.calls++
	f.gotServer, f.gotPath, f.gotContent = server, path, content
	return f.err
}

// mkFiles builds an API whose "survival" server is STOPPED and owned by owner1,
// with a wired fakeFileEditor — the state in which every file operation is
// permitted, so each subtest changes exactly the one thing it is about.
func mkFiles() (*API, *fakeRepo, *fakeCluster, *fakeFileEditor) {
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped",
		Ready: false, DesiredState: string(v1alpha1.DesiredStopped)}
	files := &fakeFileEditor{}
	api := newTestAPI(repo, cl)
	api.Files = files
	return api, repo, cl, files
}

// TestFileEditorStoppedGate is the gate this whole subsystem hinges on. The world
// PVC is ReadWriteOnce, but RWO is per node: on a single node a file Job mounts it
// right beside a running server, and a write lands under a live world that the
// server's next save overwrites or tears. Every one of the three routes
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
				api, _, cl, files := mkFiles()
				cl.byName["survival"].Ready = st.ready
				cl.byName["survival"].DesiredState = string(st.desiredState)
				api.External = staticExternal{p: owner}

				var hdr map[string]string
				if rt.body != "" {
					hdr = jsonHeader
				}
				w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, hdr)
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
// All three routes must refuse BEFORE creating a Job, with the same specific 409
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
	}

	for _, rt := range routes {
		t.Run(rt.name+" without a world volume -> 409 no_world_volume", func(t *testing.T) {
			api, _, cl, files := mkFiles()
			cl.noWorld["survival"] = true
			api.External = staticExternal{p: owner}

			var hdr map[string]string
			if rt.body != "" {
				hdr = jsonHeader
			}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, hdr)
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
// owner-or-admin rule the backup routes enforce, and it must hold on all three
// routes — a read-only route leaking another owner's config (an RCON password
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
	}

	hdrFor := func(body string) map[string]string {
		if body != "" {
			return jsonHeader
		}
		return nil
	}

	for _, rt := range routes {
		t.Run(rt.name+": non-owner -> 403, executor untouched", func(t *testing.T) {
			api, _, _, files := mkFiles()
			api.External = staticExternal{p: stranger}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, hdrFor(rt.body))
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
			}
			if files.calls != 0 {
				t.Fatal("a forbidden caller must not reach the file executor")
			}
		})

		t.Run(rt.name+": owner -> allowed", func(t *testing.T) {
			api, _, _, files := mkFiles()
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, hdrFor(rt.body))
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
			}
			if files.calls != 1 || files.gotServer != "survival" {
				t.Fatalf("executor saw (calls=%d, server=%q)", files.calls, files.gotServer)
			}
		})

		t.Run(rt.name+": admin on someone else's server -> allowed", func(t *testing.T) {
			api, repo, _, files := mkFiles()
			repo.byName["survival"].OwnerID = "someone-else"
			api.External = staticExternal{p: admin}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, hdrFor(rt.body))
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
			}
			if files.calls != 1 {
				t.Fatal("admin should reach the executor")
			}
		})

		t.Run(rt.name+": unowned server -> 403 for a plain user", func(t *testing.T) {
			api, repo, _, _ := mkFiles()
			repo.byName["survival"].OwnerID = "" // released world
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, hdrFor(rt.body))
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 (%s)", w.Code, w.Body.String())
			}
		})

		t.Run(rt.name+": unknown server -> 404", func(t *testing.T) {
			api, _, _, _ := mkFiles()
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method,
				strings.Replace(rt.path, "survival", "missing", 1), rt.body, hdrFor(rt.body))
			if w.Code != http.StatusNotFound {
				t.Fatalf("code = %d, want 404 (%s)", w.Code, w.Body.String())
			}
		})

		t.Run(rt.name+": invalid server name -> 400 bad_name", func(t *testing.T) {
			api, _, _, _ := mkFiles()
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method,
				strings.Replace(rt.path, "survival", "X", 1), rt.body, hdrFor(rt.body))
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_name" {
				t.Fatalf("code = %d body %s", w.Code, w.Body.String())
			}
		})

		t.Run(rt.name+": nil FileEditor -> 503 files_unavailable", func(t *testing.T) {
			api, _, _, _ := mkFiles()
			api.Files = nil
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), rt.method, rt.path, rt.body, hdrFor(rt.body))
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
		api, _, _, files := mkFiles()
		files.entries = []fileedit.Entry{{Name: "paper.yml", Size: 12}, {Name: "sub", IsDir: true}}
		files.truncated = true
		api.External = staticExternal{p: owner}

		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/files?path=config", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Path      string           `json:"path"`
			Entries   []fileedit.Entry `json:"entries"`
			Truncated bool             `json:"truncated"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if files.gotPath != "config" {
			t.Fatalf("executor saw path %q, want the query value verbatim", files.gotPath)
		}
		if resp.Path != "config" || len(resp.Entries) != 2 || !resp.Truncated {
			t.Fatalf("unexpected response %+v", resp)
		}
	})

	t.Run("list without a path lists the world root", func(t *testing.T) {
		api, _, _, files := mkFiles()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/files", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if files.gotPath != "" {
			t.Fatalf("path = %q, want empty (the root)", files.gotPath)
		}
	})

	t.Run("read returns base64 content", func(t *testing.T) {
		api, _, _, files := mkFiles()
		files.content = []byte("motd=hello\n")
		api.External = staticExternal{p: owner}

		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/file?path=server.properties", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Path    string `json:"path"`
			Content []byte `json:"content"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if resp.Path != "server.properties" || string(resp.Content) != "motd=hello\n" {
			t.Fatalf("unexpected response %+v (%q)", resp, resp.Content)
		}
	})

	t.Run("read without a path -> 400", func(t *testing.T) {
		api, _, _, files := mkFiles()
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
		api, repo, _, files := mkFiles()
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

	t.Run("reads are not audited", func(t *testing.T) {
		api, repo, _, _ := mkFiles()
		api.External = staticExternal{p: owner}
		do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/files", "", nil)
		do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/file?path=x", "", nil)
		if len(repo.audits) != 0 {
			t.Fatalf("reads should not write audit rows: %+v", repo.audits)
		}
	})

	t.Run("oversized write -> 413 before the executor", func(t *testing.T) {
		api, _, _, files := mkFiles()
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
			api, _, _, files := mkFiles()
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
		api, _, _, files := mkFiles()
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
		api, _, _, files := mkFiles()
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
		{"timeout", fmt.Errorf("waiting: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, "files_timeout"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, _, _, files := mkFiles()
			files.err = tc.err
			api.External = staticExternal{p: owner}
			w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/file?path=x", "", nil)
			if w.Code != tc.wantCode || decodeErr(t, w) != tc.wantErr {
				t.Fatalf("code = %d body %s, want %d/%s", w.Code, w.Body.String(), tc.wantCode, tc.wantErr)
			}
		})
	}

	t.Run("an unrecognised executor failure -> 500", func(t *testing.T) {
		api, _, _, files := mkFiles()
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
