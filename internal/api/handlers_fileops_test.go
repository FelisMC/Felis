package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
)

const (
	sessionsRoute = "/api/v1/servers/survival/files/uploads"
	unzipRoute    = "/api/v1/servers/survival/files/unzip"
	opsRoute      = "/api/v1/servers/survival/files/ops"
	sessionPath   = "world/region/r.0.0.mca"
)

var (
	fileOwner    = &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	fileStranger = &Principal{UserID: "stranger", Email: "stranger@example.net", Role: "user"}
	fileAdmin    = &Principal{UserID: "admin1", Email: "admin1@example.net", Role: "admin", ViaAdminAccess: true}
)

// beginSession begins a session for size bytes at sessionPath and returns it.
func beginSession(t *testing.T, api *API, size int) fileSessionView {
	t.Helper()
	w := do(api.ExternalHandler(), "POST", sessionsRoute+"?path="+sessionPath, `{"size":`+strconv.Itoa(size)+`}`, jsonHeader)
	if w.Code != http.StatusCreated {
		t.Fatalf("begin: code = %d (%s)", w.Code, w.Body.String())
	}
	return sessionAnswer(t, w)
}

// sessionAnswer decodes a session answer, refusing a field the view lacks.
func sessionAnswer(t *testing.T, w *httptest.ResponseRecorder) fileSessionView {
	t.Helper()
	var s fileSessionView
	dec := json.NewDecoder(strings.NewReader(w.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		t.Fatalf("session answer: %v (%s)", err, w.Body.String())
	}
	return s
}

// doPart sends one part with the Content-Length given, whatever the body's own
// length: -1 sends none, and one past the body is a part cut short. Its
// Content-Digest is that of the body.
func doPart(h http.Handler, target, body string, length int64) *httptest.ResponseRecorder {
	return doPartDigest(h, target, body, length, contentDigestOf(body))
}

// doPartDigest is doPart with the Content-Digest given; empty sends none.
func doPartDigest(h http.Handler, target, body string, length int64, digest string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("PUT", target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/octet-stream")
	if digest != "" {
		r.Header.Set("Content-Digest", digest)
	}
	r.ContentLength = length
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	recordContract(r, body, w)
	return w
}

func partAt(id string, offset int) string {
	return sessionsRoute + "/" + id + "?offset=" + strconv.Itoa(offset)
}

func errMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var raw map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("error body not JSON: %v (%s)", err, w.Body.String())
	}
	return raw["error"]["message"]
}

// fetchStaged is the Job's fetch of what a commit handed it.
func fetchStaged(api *API, src fileedit.UploadSource) *httptest.ResponseRecorder {
	at := strings.TrimPrefix(src.URL, api.InternalBaseURL)
	return do(api.InternalHandler(), "GET", at, "", map[string]string{"Authorization": "Bearer " + src.Token})
}

var opStarted = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

// TestFileUploadSession drives a session from begin to the Job's fetch across
// both faces, and each way it can go wrong on the way.
func TestFileUploadSession(t *testing.T) {
	commit := func(api *API, id, body string) *httptest.ResponseRecorder {
		return do(api.ExternalHandler(), "POST", sessionsRoute+"/"+id+"/commit", body, jsonHeader)
	}
	status := func(api *API, id string) *httptest.ResponseRecorder {
		return do(api.ExternalHandler(), "GET", sessionsRoute+"/"+id, "", nil)
	}

	t.Run("begin, parts, commit, and the Job fetches the whole file once", func(t *testing.T) {
		api, repo, cl, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		// Every other internal route wants a service token; the Job has none.
		api.Internal = CallerTokens{CallerVelocity: "s3cr3t"}
		files.op = fileedit.OpState{ID: "op1", Op: fileedit.OpUpload, Path: sessionPath,
			State: fileedit.OpRunning, Started: opStarted}
		h := api.ExternalHandler()

		s := beginSession(t, api, 10)
		// A new session lists its parts as [], which decodes non-nil; null would
		// leave a client resuming with nothing to walk.
		if len(s.ID) != 32 || s.Path != sessionPath || s.Size != 10 || s.Received != 0 || s.PartMaxBytes != fileedit.PartBytes ||
			s.Parts == nil || len(s.Parts) != 0 {
			t.Fatalf("begin = %+v", s)
		}

		if w := doPart(h, partAt(s.ID, 0), "hello", 5); w.Code != http.StatusOK || sessionAnswer(t, w).Received != 5 {
			t.Fatalf("part 1: code = %d (%s)", w.Code, w.Body.String())
		}
		// The same part again, as a client that lost the answer might send it.
		w := doPart(h, partAt(s.ID, 0), "hello", 5)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "upload_offset_mismatch" ||
			errMessage(t, w) != "the upload holds 5 bytes; send the part that starts there" {
			t.Fatalf("replayed part: code = %d (%s)", w.Code, w.Body.String())
		}

		// Too early: refused before the world lock is asked for.
		w = commit(api, s.ID, `{}`)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "upload_incomplete" ||
			files.calls != 0 || len(cl.acquired) != 0 {
			t.Fatalf("early commit: code = %d calls = %d acquired %v (%s)", w.Code, files.calls, cl.acquired, w.Body.String())
		}

		hello := sha256.Sum256([]byte("hello"))
		if w := status(api, s.ID); w.Code != http.StatusOK || !reflect.DeepEqual(sessionAnswer(t, w), fileSessionView{
			ID: s.ID, Path: sessionPath, Size: 10, Received: 5, PartMaxBytes: fileedit.PartBytes,
			Parts: []filePartView{{Size: 5, SHA256: hex.EncodeToString(hello[:])}}}) {
			t.Fatalf("status: code = %d (%s)", w.Code, w.Body.String())
		}
		if w := doPart(h, partAt(s.ID, 5), "world", 5); w.Code != http.StatusOK || sessionAnswer(t, w).Received != 10 {
			t.Fatalf("part 2: code = %d (%s)", w.Code, w.Body.String())
		}
		if w := doPart(h, partAt(s.ID, 10), "!", 1); w.Code != http.StatusRequestEntityTooLarge || decodeErr(t, w) != "part_too_large" {
			t.Fatalf("a part past the size: code = %d (%s)", w.Code, w.Body.String())
		}

		w = commit(api, s.ID, `{}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("commit: code = %d (%s)", w.Code, w.Body.String())
		}
		want := map[string]any{"op": map[string]any{
			"id": "op1", "op": "upload", "path": sessionPath, "state": "running",
			"started_at": "2026-09-28T10:00:00Z", "done": float64(0), "total": float64(0),
		}}
		if got := fileAnswer(t, w); !reflect.DeepEqual(got, want) {
			t.Fatalf("commit answer = %v, want %v", got, want)
		}
		digest := sha256.Sum256([]byte("helloworld"))
		sum := hex.EncodeToString(digest[:])
		src := files.gotSource
		if files.calls != 1 || files.gotOp != fileedit.OpUpload || files.gotServer != "survival" || files.gotPath != sessionPath ||
			src.URL != api.InternalBaseURL+"/api/v1/internal/file-uploads/"+s.ID ||
			src.Size != 10 || src.SHA256 != sum || len(src.Token) != 64 || files.gotOverwrite {
			t.Fatalf("executor saw calls=%d op %q server %q path %q source %+v overwrite %v",
				files.calls, files.gotOp, files.gotServer, files.gotPath, src, files.gotOverwrite)
		}
		if strings.Join(cl.acquired, ",") != "survival:"+maintenance.KindFileWrite || strings.Join(cl.released, ",") != "survival" {
			t.Fatalf("lock acquired %v, released %v", cl.acquired, cl.released)
		}
		onlyAudit(t, repo, "file.upload", "survival:"+sessionPath, `{"overwrite":false,"sha256":"`+sum+`","size_bytes":10}`)

		fetched := fetchStaged(api, src)
		if fetched.Code != http.StatusOK || fetched.Body.String() != "helloworld" || fetched.Header().Get("Content-Length") != "10" {
			t.Fatalf("fetch: code = %d %q Content-Length %q", fetched.Code, fetched.Body.String(), fetched.Header().Get("Content-Length"))
		}
		if again := fetchStaged(api, src); again.Code != http.StatusNotFound {
			t.Fatalf("second fetch: code = %d", again.Code)
		}
		// Served whole, the session stays until the Job says the file landed: a
		// Job that fails after its fetch leaves the upload to be committed again.
		if w := status(api, s.ID); w.Code != http.StatusOK || sessionAnswer(t, w).Received != 10 {
			t.Fatalf("status after the fetch: code = %d (%s)", w.Code, w.Body.String())
		}
		landed := func(token string) *httptest.ResponseRecorder {
			return do(api.InternalHandler(), "DELETE", strings.TrimPrefix(src.URL, api.InternalBaseURL), "",
				map[string]string{"Authorization": "Bearer " + token})
		}
		if w := landed(strings.Repeat("0", len(src.Token))); w.Code != http.StatusNotFound || decodeErr(t, w) != "not_found" {
			t.Fatalf("landed with the wrong token: code = %d (%s)", w.Code, w.Body.String())
		}
		if w := status(api, s.ID); w.Code != http.StatusOK {
			t.Fatalf("a wrong token let go of the session: code = %d (%s)", w.Code, w.Body.String())
		}
		if w := landed(src.Token); w.Code != http.StatusNoContent {
			t.Fatalf("landed: code = %d (%s)", w.Code, w.Body.String())
		}
		if w := status(api, s.ID); w.Code != http.StatusNotFound || decodeErr(t, w) != "upload_not_found" {
			t.Fatalf("status after it landed: code = %d (%s)", w.Code, w.Body.String())
		}
		stageEmpty(t, api)
		if w := landed(src.Token); w.Code != http.StatusNotFound {
			t.Fatalf("landed twice: code = %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("a Job that never fetched is committed again with a fresh token", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		s := beginSession(t, api, 3)
		doPart(api.ExternalHandler(), partAt(s.ID, 0), "abc", 3)
		if w := commit(api, s.ID, `{}`); w.Code != http.StatusAccepted {
			t.Fatalf("first commit: code = %d (%s)", w.Code, w.Body.String())
		}
		first := files.gotSource
		if w := commit(api, s.ID, `{"overwrite":true}`); w.Code != http.StatusAccepted || !files.gotOverwrite {
			t.Fatalf("second commit: code = %d overwrite %v (%s)", w.Code, files.gotOverwrite, w.Body.String())
		}
		second := files.gotSource
		if w := fetchStaged(api, first); w.Code != http.StatusNotFound {
			t.Fatalf("the first commit's token still opens it: code = %d", w.Code)
		}
		if w := fetchStaged(api, second); w.Code != http.StatusOK || w.Body.String() != "abc" {
			t.Fatalf("fetch: code = %d %q", w.Code, w.Body.String())
		}
		if len(repo.audits) != 2 || string(repo.audits[1].Payload) != `{"overwrite":true,"sha256":"`+second.SHA256+`","size_bytes":3}` {
			t.Fatalf("audits = %+v", repo.audits)
		}
	})

	// The running Job's hold on the world refuses the commit before Seal, so the
	// token that Job carries still opens the file.
	t.Run("a commit while the world is held leaves the running Job's token alone", func(t *testing.T) {
		api, _, cl, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		s := beginSession(t, api, 3)
		doPart(api.ExternalHandler(), partAt(s.ID, 0), "abc", 3)
		commit(api, s.ID, `{}`)
		running := files.gotSource
		cl.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindFileWrite}
		w := commit(api, s.ID, `{}`)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "maintenance_in_progress" || files.calls != 1 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
		if w := fetchStaged(api, running); w.Code != http.StatusOK || w.Body.String() != "abc" {
			t.Fatalf("the running Job lost its file: code = %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("a Job that could not start is not audited and lets go of the world", func(t *testing.T) {
		api, repo, cl, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		s := beginSession(t, api, 3)
		doPart(api.ExternalHandler(), partAt(s.ID, 0), "abc", 3)
		files.err = errors.New("the cluster said no")
		w := commit(api, s.ID, `{}`)
		if w.Code != http.StatusInternalServerError || len(repo.audits) != 0 || strings.Join(cl.released, ",") != "survival" {
			t.Fatalf("code = %d audits %+v released %v (%s)", w.Code, repo.audits, cl.released, w.Body.String())
		}
		if w := status(api, s.ID); w.Code != http.StatusOK || sessionAnswer(t, w).Received != 3 {
			t.Fatalf("the session went with the failed start: code = %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("a fetch cut short keeps the session for the next commit", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		s := beginSession(t, api, 3)
		doPart(api.ExternalHandler(), partAt(s.ID, 0), "abc", 3)
		commit(api, s.ID, `{}`)
		src := files.gotSource
		r := httptest.NewRequest("GET", strings.TrimPrefix(src.URL, api.InternalBaseURL), nil)
		r.Header.Set("Authorization", "Bearer "+src.Token)
		cut := &brokenWriter{header: http.Header{}}
		api.InternalHandler().ServeHTTP(cut, r)
		if cut.code != http.StatusOK {
			t.Fatalf("cut fetch: code = %d", cut.code)
		}
		if w := status(api, s.ID); w.Code != http.StatusOK {
			t.Fatalf("status after a cut fetch: code = %d (%s)", w.Code, w.Body.String())
		}
		commit(api, s.ID, `{}`)
		if w := fetchStaged(api, files.gotSource); w.Code != http.StatusOK || w.Body.String() != "abc" {
			t.Fatalf("refetch: code = %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("a part changed on the way is refused and taken back", func(t *testing.T) {
		api, _, _, _ := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		h := api.ExternalHandler()
		s := beginSession(t, api, 10)
		doPart(h, partAt(s.ID, 0), "hello", 5)
		for _, c := range []struct {
			name, digest, errCode string
		}{
			{"another part's digest", contentDigestOf("wor1d"), "digest_mismatch"},
			{"no digest", "", "digest_required"},
			{"a malformed digest", "sha-256=:bm90IGEgc3VtCg==:", "bad_digest"},
		} {
			w := doPartDigest(h, partAt(s.ID, 5), "world", 5, c.digest)
			if w.Code != http.StatusBadRequest || decodeErr(t, w) != c.errCode {
				t.Fatalf("%s: code = %d (%s), want 400 %s", c.name, w.Code, w.Body.String(), c.errCode)
			}
			if got := sessionAnswer(t, status(api, s.ID)); got.Received != 5 || len(got.Parts) != 1 {
				t.Fatalf("%s: session after the refused part = %+v", c.name, got)
			}
		}
		if w := doPart(h, partAt(s.ID, 5), "world", 5); w.Code != http.StatusOK || sessionAnswer(t, w).Received != 10 {
			t.Fatalf("the part sent again: code = %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("a part cut short leaves the session where it was", func(t *testing.T) {
		api, _, _, _ := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		s := beginSession(t, api, 10)
		w := doPart(api.ExternalHandler(), partAt(s.ID, 0), "abc", 5)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "upload_incomplete" {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		if w := status(api, s.ID); sessionAnswer(t, w).Received != 0 {
			t.Fatalf("status = %s", w.Body.String())
		}
	})

	t.Run("while a part arrives, the session takes nothing else", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		h := api.ExternalHandler()
		s := beginSession(t, api, 6)
		pr, pw := io.Pipe()
		arriving := make(chan int)
		go func() {
			r := httptest.NewRequest("PUT", partAt(s.ID, 0), pr)
			r.Header.Set("Content-Type", "application/octet-stream")
			r.Header.Set("Content-Digest", contentDigestOf("abc"))
			r.ContentLength = 3
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			arriving <- w.Code
		}()
		// The write returns once the handler has read the byte, so the part is
		// being appended.
		if _, err := pw.Write([]byte("a")); err != nil {
			t.Fatal(err)
		}
		for name, w := range map[string]*httptest.ResponseRecorder{
			"another part": doPart(h, partAt(s.ID, 0), "xyz", 3),
			"cancel":       do(h, "DELETE", sessionsRoute+"/"+s.ID, "", nil),
		} {
			if w.Code != http.StatusConflict || decodeErr(t, w) != "upload_busy" {
				t.Errorf("%s: code = %d (%s)", name, w.Code, w.Body.String())
			}
		}
		pw.CloseWithError(errors.New("the client went away"))
		if code := <-arriving; code != http.StatusBadRequest {
			t.Fatalf("the broken part answered %d", code)
		}
		if w := status(api, s.ID); w.Code != http.StatusOK || sessionAnswer(t, w).Received != 0 || files.calls != 0 {
			t.Fatalf("status: code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
	})

	t.Run("parts refused before a byte is read", func(t *testing.T) {
		for _, c := range []struct {
			name, target string
			length       int64
			code         int
			errCode      string
		}{
			{"no offset", sessionsRoute + "/%s", 3, http.StatusBadRequest, "bad_request"},
			{"an offset that is no number", sessionsRoute + "/%s?offset=abc", 3, http.StatusBadRequest, "bad_request"},
			{"no Content-Length", sessionsRoute + "/%s?offset=0", -1, http.StatusLengthRequired, "length_required"},
			{"a part over the cap", sessionsRoute + "/%s?offset=0", fileedit.PartBytes + 1, http.StatusRequestEntityTooLarge, "part_too_large"},
			// At the cap it is taken, and the three bytes behind it end short.
			{"a part at the cap", sessionsRoute + "/%s?offset=0", fileedit.PartBytes, http.StatusBadRequest, "upload_incomplete"},
		} {
			t.Run(c.name, func(t *testing.T) {
				api, _, _, _ := mkFiles(t)
				api.External = staticExternal{p: fileOwner}
				s := beginSession(t, api, fileedit.PartBytes+10)
				w := doPart(api.ExternalHandler(), fmt.Sprintf(c.target, s.ID), "abc", c.length)
				if w.Code != c.code || decodeErr(t, w) != c.errCode {
					t.Fatalf("code = %d (%s), want %d %s", w.Code, w.Body.String(), c.code, c.errCode)
				}
				if w := status(api, s.ID); sessionAnswer(t, w).Received != 0 {
					t.Fatalf("status = %s", w.Body.String())
				}
			})
		}
	})

	// The parts need only the account and the server: starting the server midway
	// costs the upload nothing but the commit.
	t.Run("parts, status and cancel go on while the server runs", func(t *testing.T) {
		api, _, cl, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		h := api.ExternalHandler()
		s := beginSession(t, api, 3)
		cl.byName["survival"].Ready = true
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		if w := doPart(h, partAt(s.ID, 0), "abc", 3); w.Code != http.StatusOK {
			t.Fatalf("part: code = %d (%s)", w.Code, w.Body.String())
		}
		if w := status(api, s.ID); w.Code != http.StatusOK {
			t.Fatalf("status: code = %d (%s)", w.Code, w.Body.String())
		}
		if w := commit(api, s.ID, `{}`); w.Code != http.StatusConflict || decodeErr(t, w) != "not_stopped" || files.calls != 0 {
			t.Fatalf("commit: code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
		if w := do(h, "DELETE", sessionsRoute+"/"+s.ID, "", nil); w.Code != http.StatusNoContent {
			t.Fatalf("cancel: code = %d (%s)", w.Code, w.Body.String())
		}
		if w := status(api, s.ID); w.Code != http.StatusNotFound || decodeErr(t, w) != "upload_not_found" {
			t.Fatalf("status after cancel: code = %d (%s)", w.Code, w.Body.String())
		}
		stageEmpty(t, api)
	})

	t.Run("a session answers only the account and server it was begun for", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		repo.byName["creative"] = &ServerRecord{Name: "creative", OwnerID: "owner1"}
		api.External = staticExternal{p: fileOwner}
		s := beginSession(t, api, 3)

		elsewhere := strings.Replace(sessionsRoute, "survival", "creative", 1) + "/" + s.ID
		if w := do(api.ExternalHandler(), "GET", elsewhere, "", nil); w.Code != http.StatusNotFound || decodeErr(t, w) != "upload_not_found" {
			t.Fatalf("another server: code = %d (%s)", w.Code, w.Body.String())
		}

		// Staff may reach the server, and still not someone else's session.
		api.External = staticExternal{p: fileAdmin}
		h := api.ExternalHandler()
		for name, w := range map[string]*httptest.ResponseRecorder{
			"status": status(api, s.ID),
			"part":   doPart(h, partAt(s.ID, 0), "abc", 3),
			"cancel": do(h, "DELETE", sessionsRoute+"/"+s.ID, "", nil),
			"commit": commit(api, s.ID, `{}`),
		} {
			if w.Code != http.StatusNotFound || decodeErr(t, w) != "upload_not_found" {
				t.Errorf("admin %s: code = %d (%s)", name, w.Code, w.Body.String())
			}
		}
		if files.calls != 0 {
			t.Fatal("another account's commit reached the executor")
		}

		api.External = staticExternal{p: fileOwner}
		if w := status(api, s.ID); w.Code != http.StatusOK || sessionAnswer(t, w).Received != 0 {
			t.Fatalf("the owner's session was touched: code = %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("a stranger is refused on every session route", func(t *testing.T) {
		api, _, _, _ := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		s := beginSession(t, api, 3)
		api.External = staticExternal{p: fileStranger}
		h := api.ExternalHandler()
		for name, w := range map[string]*httptest.ResponseRecorder{
			"begin":  do(h, "POST", sessionsRoute+"?path=a.jar", `{"size":3}`, jsonHeader),
			"status": status(api, s.ID),
			"part":   doPart(h, partAt(s.ID, 0), "abc", 3),
			"cancel": do(h, "DELETE", sessionsRoute+"/"+s.ID, "", nil),
			"commit": commit(api, s.ID, `{}`),
		} {
			if w.Code != http.StatusForbidden {
				t.Errorf("%s: code = %d (%s)", name, w.Code, w.Body.String())
			}
		}
	})

	t.Run("begin refused", func(t *testing.T) {
		for _, c := range []struct {
			name, target, body string
			setup              func(*API)
			code               int
			errCode            string
		}{
			{"no size", sessionsRoute + "?path=a.jar", `{}`, nil, http.StatusBadRequest, "bad_request"},
			{"a negative size", sessionsRoute + "?path=a.jar", `{"size":-1}`, nil, http.StatusBadRequest, "bad_request"},
			{"no path", sessionsRoute, `{"size":3}`, nil, http.StatusBadRequest, "bad_request"},
			{"a path leaving the world", sessionsRoute + "?path=../a.jar", `{"size":3}`, nil, http.StatusBadRequest, "bad_path"},
			{"an absolute path", sessionsRoute + "?path=/etc/a.jar", `{"size":3}`, nil, http.StatusBadRequest, "bad_path"},
			{"the world folder itself", sessionsRoute + "?path=plugins/..", `{"size":3}`, nil, http.StatusBadRequest, "bad_path"},
			{"a name longer than a folder entry holds", sessionsRoute + "?path=plugins/" + strings.Repeat("n", fileedit.NameMax-3) + ".jar", `{"size":3}`,
				nil, http.StatusBadRequest, "bad_path"},
			{"a staging disk at its floor", sessionsRoute + "?path=a.jar", `{"size":3}`,
				func(a *API) { a.FileStage.MinFree = 1 }, http.StatusInsufficientStorage, "upload_staging_full"},
			{"no stage", sessionsRoute + "?path=a.jar", `{"size":3}`,
				func(a *API) { a.FileStage = nil }, http.StatusServiceUnavailable, "files_unavailable"},
			{"no internal URL", sessionsRoute + "?path=a.jar", `{"size":3}`,
				func(a *API) { a.InternalBaseURL = "" }, http.StatusServiceUnavailable, "files_unavailable"},
			{"a caller with no account", sessionsRoute + "?path=a.jar", `{"size":3}`,
				func(a *API) { a.External = staticExternal{p: &Principal{Role: "admin", ViaAdminAccess: true}} },
				http.StatusForbidden, "forbidden"},
		} {
			t.Run(c.name, func(t *testing.T) {
				api, _, _, _ := mkFiles(t)
				api.External = staticExternal{p: fileOwner}
				if c.setup != nil {
					c.setup(api)
				}
				w := do(api.ExternalHandler(), "POST", c.target, c.body, jsonHeader)
				if w.Code != c.code || decodeErr(t, w) != c.errCode {
					t.Fatalf("code = %d (%s), want %d %s", w.Code, w.Body.String(), c.code, c.errCode)
				}
				if api.FileStage != nil {
					stageEmpty(t, api)
				}
			})
		}
	})

	t.Run("a name as long as a folder entry holds is taken", func(t *testing.T) {
		api, _, _, _ := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		long := strings.Repeat("n", fileedit.NameMax-4) + ".jar"
		w := do(api.ExternalHandler(), "POST", sessionsRoute+"?path=plugins/"+long, `{"size":3}`, jsonHeader)
		if w.Code != http.StatusCreated {
			t.Fatalf("code = %d (%s), want 201", w.Code, w.Body.String())
		}
		if s := sessionAnswer(t, w); s.Path != "plugins/"+long {
			t.Fatalf("path = %q", s.Path)
		}
	})

	t.Run("a fifth upload at once -> 429", func(t *testing.T) {
		api, _, _, _ := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		for i := 0; i < fileedit.MaxSessionsPerUser; i++ {
			beginSession(t, api, 1)
		}
		w := do(api.ExternalHandler(), "POST", sessionsRoute+"?path=a.jar", `{"size":1}`, jsonHeader)
		if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "too_many_uploads" {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("a commit or cancel of no session -> 404", func(t *testing.T) {
		api, _, cl, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		const id = "00112233445566778899aabbccddeeff"
		for name, w := range map[string]*httptest.ResponseRecorder{
			"commit": commit(api, id, `{}`),
			"cancel": do(api.ExternalHandler(), "DELETE", sessionsRoute+"/"+id, "", nil),
			"part":   doPart(api.ExternalHandler(), partAt(id, 0), "abc", 3),
		} {
			if w.Code != http.StatusNotFound || decodeErr(t, w) != "upload_not_found" {
				t.Errorf("%s: code = %d (%s)", name, w.Code, w.Body.String())
			}
		}
		if files.calls != 0 || len(cl.acquired) != 0 {
			t.Fatalf("calls = %d acquired %v", files.calls, cl.acquired)
		}
	})

	t.Run("a commit needs a body", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		s := beginSession(t, api, 0)
		if w := commit(api, s.ID, ""); w.Code != http.StatusBadRequest || files.calls != 0 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
	})
}

// brokenWriter takes the headers and fails every write, as a connection that
// dropped once the answer began does.
type brokenWriter struct {
	header http.Header
	code   int
}

func (b *brokenWriter) Header() http.Header         { return b.header }
func (b *brokenWriter) WriteHeader(code int)        { b.code = code }
func (b *brokenWriter) Write(p []byte) (int, error) { return 0, errors.New("connection reset") }

// TestFileOpsWorldGates pins the stopped and world-volume gates on the routes
// that begin or start a background op: each refuses before a byte is staged, a
// lock is asked for, or a Job is created.
func TestFileOpsWorldGates(t *testing.T) {
	routes := []struct{ name, target, body string }{
		{"begin", sessionsRoute + "?path=a.jar", `{"size":3}`},
		{"commit", sessionsRoute + "/00112233445566778899aabbccddeeff/commit", `{}`},
		{"unzip", unzipRoute + "?path=maps/a.zip", `{}`},
	}
	gates := []struct {
		name, code string
		set        func(*fakeCluster)
	}{
		{"running", "not_stopped", func(c *fakeCluster) {
			c.byName["survival"].Ready = true
			c.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		}},
		{"starting", "not_stopped", func(c *fakeCluster) {
			c.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		}},
		{"no world volume", "no_world_volume", func(c *fakeCluster) { c.noWorld["survival"] = true }},
	}
	for _, rt := range routes {
		for _, g := range gates {
			t.Run(rt.name+" on a "+g.name+" server", func(t *testing.T) {
				api, _, cl, files := mkFiles(t)
				api.External = staticExternal{p: fileOwner}
				g.set(cl)
				w := do(api.ExternalHandler(), "POST", rt.target, rt.body, jsonHeader)
				if w.Code != http.StatusConflict || decodeErr(t, w) != g.code {
					t.Fatalf("code = %d (%s), want 409 %s", w.Code, w.Body.String(), g.code)
				}
				if files.calls != 0 || len(cl.acquired) != 0 {
					t.Fatalf("calls = %d acquired %v", files.calls, cl.acquired)
				}
				stageEmpty(t, api)
			})
		}
	}
}

func TestFileUnzip(t *testing.T) {
	unzip := func(api *API, path, body string) *httptest.ResponseRecorder {
		return do(api.ExternalHandler(), "POST", unzipRoute+"?path="+path, body, jsonHeader)
	}

	t.Run("starts the Job under the world lock and audits it", func(t *testing.T) {
		api, repo, cl, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		files.op = fileedit.OpState{ID: "op2", Op: fileedit.OpUnzip, Path: "maps/a.zip",
			State: fileedit.OpRunning, Started: opStarted}
		w := unzip(api, "maps/a.zip", `{}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
		}
		want := map[string]any{"op": map[string]any{
			"id": "op2", "op": "unzip", "path": "maps/a.zip", "state": "running",
			"started_at": "2026-09-28T10:00:00Z", "done": float64(0), "total": float64(0),
		}}
		if got := fileAnswer(t, w); !reflect.DeepEqual(got, want) {
			t.Fatalf("answer = %v, want %v", got, want)
		}
		if files.calls != 1 || files.gotOp != fileedit.OpUnzip || files.gotServer != "survival" ||
			files.gotPath != "maps/a.zip" || files.gotOverwrite {
			t.Fatalf("executor saw calls=%d op %q server %q path %q overwrite %v",
				files.calls, files.gotOp, files.gotServer, files.gotPath, files.gotOverwrite)
		}
		if strings.Join(cl.acquired, ",") != "survival:"+maintenance.KindFileWrite || strings.Join(cl.released, ",") != "survival" {
			t.Fatalf("lock acquired %v, released %v", cl.acquired, cl.released)
		}
		onlyAudit(t, repo, "file.unzip", "survival:maps/a.zip", `{"overwrite":false}`)
	})

	t.Run("overwrite reaches the executor, and the suffix is any case", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		if w := unzip(api, "maps/A.ZIP", `{"overwrite":true}`); w.Code != http.StatusAccepted || !files.gotOverwrite || files.gotPath != "maps/A.ZIP" {
			t.Fatalf("code = %d overwrite %v path %q (%s)", w.Code, files.gotOverwrite, files.gotPath, w.Body.String())
		}
		onlyAudit(t, repo, "file.unzip", "survival:maps/A.ZIP", `{"overwrite":true}`)
	})

	t.Run("refused before the lock", func(t *testing.T) {
		for _, c := range []struct {
			name, path, body string
			code             int
			errCode          string
		}{
			{"not a zip", "maps/a.tar.gz", `{}`, http.StatusBadRequest, "bad_path"},
			{"zip only inside the name", "maps/a.zip.bak", `{}`, http.StatusBadRequest, "bad_path"},
			{"no path", "", `{}`, http.StatusBadRequest, "bad_request"},
			{"no body", "maps/a.zip", "", http.StatusBadRequest, "bad_request"},
		} {
			t.Run(c.name, func(t *testing.T) {
				api, repo, cl, files := mkFiles(t)
				api.External = staticExternal{p: fileOwner}
				w := unzip(api, c.path, c.body)
				if w.Code != c.code || decodeErr(t, w) != c.errCode {
					t.Fatalf("code = %d (%s), want %d %s", w.Code, w.Body.String(), c.code, c.errCode)
				}
				if files.calls != 0 || len(cl.acquired) != 0 || len(repo.audits) != 0 {
					t.Fatalf("calls = %d acquired %v audits %+v", files.calls, cl.acquired, repo.audits)
				}
			})
		}
	})

	t.Run("a held world -> 409, no Job", func(t *testing.T) {
		api, _, cl, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		cl.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindBackup}
		if w := unzip(api, "maps/a.zip", `{}`); w.Code != http.StatusConflict || decodeErr(t, w) != "maintenance_in_progress" || files.calls != 0 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
	})

	t.Run("a Job that could not start is not audited and lets go of the world", func(t *testing.T) {
		api, repo, cl, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		files.err = errors.New("the cluster said no")
		w := unzip(api, "maps/a.zip", `{}`)
		if w.Code != http.StatusInternalServerError || len(repo.audits) != 0 || strings.Join(cl.released, ",") != "survival" {
			t.Fatalf("code = %d audits %+v released %v (%s)", w.Code, repo.audits, cl.released, w.Body.String())
		}
	})

	t.Run("a stranger -> 403, an admin may", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: fileStranger}
		if w := unzip(api, "maps/a.zip", `{}`); w.Code != http.StatusForbidden || files.calls != 0 {
			t.Fatalf("stranger: code = %d calls = %d", w.Code, files.calls)
		}
		api.External = staticExternal{p: fileAdmin}
		if w := unzip(api, "maps/a.zip", `{}`); w.Code != http.StatusAccepted || files.calls != 1 {
			t.Fatalf("admin: code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
	})
}

func TestFileOps(t *testing.T) {
	ended := opStarted.Add(3 * time.Minute)
	list := func(api *API) *httptest.ResponseRecorder {
		return do(api.ExternalHandler(), "GET", opsRoute, "", nil)
	}

	t.Run("each state and failure as the API shows it", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		base := func(id, op, state string) fileedit.OpState {
			s := fileedit.OpState{ID: id, Op: op, Path: "maps/a.zip", State: state, Started: opStarted}
			if state != fileedit.OpRunning {
				s.Finished = ended
			}
			return s
		}
		running := base("a", fileedit.OpUnzip, fileedit.OpRunning)
		running.Done, running.Total = 40, 100
		unzipped := base("b", fileedit.OpUnzip, fileedit.OpSucceeded)
		unzipped.Done, unzipped.Total = 100, 100
		unzipped.Result = &fileedit.Result{Files: 7, Bytes: 100}
		uploaded := base("c", fileedit.OpUpload, fileedit.OpSucceeded)
		uploaded.Result = &fileedit.Result{SHA256: testSum}
		exists := base("d", fileedit.OpUnzip, fileedit.OpFailed)
		exists.Result = &fileedit.Result{Code: fileedit.CodeExists, Error: "2 files are already there",
			Conflicts: []string{"maps/level.dat", "maps/r.0.0.mca"}, ConflictCount: 2}
		full := base("e", fileedit.OpUpload, fileedit.OpFailed)
		full.Result = &fileedit.Result{Code: fileedit.CodeNoSpace, Error: "no room", Need: 900, Avail: 100}
		changed := base("f", fileedit.OpUpload, fileedit.OpFailed)
		changed.Result = &fileedit.Result{Code: fileedit.CodeConflict, Error: "the bytes changed"}
		unsafe := base("g", fileedit.OpUnzip, fileedit.OpFailed)
		unsafe.Result = &fileedit.Result{Code: fileedit.CodeArchiveUnsafe, Error: "leaves the folder", Entry: "../x",
			Files: 3, Bytes: 9}
		deadline := base("h", fileedit.OpUnzip, fileedit.OpFailed)
		deadline.Reason = "DeadlineExceeded"
		killed := base("i", fileedit.OpUpload, fileedit.OpFailed)
		killed.Reason = "BackoffLimitExceeded"
		oom := base("j", fileedit.OpUnzip, fileedit.OpFailed)
		oom.Reason = "OOMKilled"
		files.ops = []fileedit.OpState{running, unzipped, uploaded, exists, full, changed, unsafe, deadline, killed, oom}

		w := list(api)
		if w.Code != http.StatusOK || files.gotServer != "survival" {
			t.Fatalf("code = %d server %q (%s)", w.Code, files.gotServer, w.Body.String())
		}
		op := func(id, kind, state string, extra map[string]any) map[string]any {
			m := map[string]any{"id": id, "op": kind, "path": "maps/a.zip", "state": state,
				"started_at": "2026-09-28T10:00:00Z", "done": float64(0), "total": float64(0)}
			if state != "running" {
				m["finished_at"] = "2026-09-28T10:03:00Z"
			}
			for k, v := range extra {
				m[k] = v
			}
			return m
		}
		failure := func(code, msg string, extra map[string]any) map[string]any {
			m := map[string]any{"code": code, "message": msg}
			for k, v := range extra {
				m[k] = v
			}
			return map[string]any{"error": m}
		}
		want := map[string]any{"ops": []any{
			op("a", "unzip", "running", map[string]any{"done": float64(40), "total": float64(100)}),
			op("b", "unzip", "succeeded", map[string]any{"done": float64(100), "total": float64(100),
				"files": float64(7), "bytes": float64(100)}),
			op("c", "upload", "succeeded", nil),
			op("d", "unzip", "failed", failure("file_exists", "2 files are already there", map[string]any{
				"conflicts": []any{"maps/level.dat", "maps/r.0.0.mca"}, "conflict_count": float64(2)})),
			op("e", "upload", "failed", failure("volume_full", "no room", map[string]any{
				"need": float64(900), "avail": float64(100)})),
			op("f", "upload", "failed", failure("file_changed", "the bytes changed", nil)),
			op("g", "unzip", "failed", failure("archive_unsafe", "leaves the folder", map[string]any{"entry": "../x"})),
			op("h", "unzip", "failed", failure("job_failed",
				"the file operation ran out of time (DeadlineExceeded); run it again", nil)),
			op("i", "upload", "failed", failure("job_failed",
				"the file operation stopped before it could report how it went (BackoffLimitExceeded); run it again", nil)),
			op("j", "unzip", "failed", failure("job_failed",
				"the file operation ran out of memory (OOMKilled); an archive of this many files has to be split into smaller ones", nil)),
		}}
		if got := fileAnswer(t, w); !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.MarshalIndent(got, "", " ")
			wantJSON, _ := json.MarshalIndent(want, "", " ")
			t.Fatalf("ops =\n%s\nwant\n%s", gotJSON, wantJSON)
		}
	})

	t.Run("none is an empty list", func(t *testing.T) {
		api, _, _, _ := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		if w := list(api); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"ops":[]}` {
			t.Fatalf("code = %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("answers while the server runs", func(t *testing.T) {
		api, _, cl, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		cl.byName["survival"].Ready = true
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		cl.noWorld["survival"] = true
		if w := list(api); w.Code != http.StatusOK || files.calls != 1 {
			t.Fatalf("code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
	})

	t.Run("who may look", func(t *testing.T) {
		api, repo, _, files := mkFiles(t)
		api.External = staticExternal{p: fileStranger}
		if w := list(api); w.Code != http.StatusForbidden || files.calls != 0 {
			t.Fatalf("stranger: code = %d calls = %d", w.Code, files.calls)
		}
		repo.byName["survival"].OwnerID = "someone-else"
		api.External = staticExternal{p: fileAdmin}
		if w := list(api); w.Code != http.StatusOK || files.calls != 1 {
			t.Fatalf("admin: code = %d calls = %d (%s)", w.Code, files.calls, w.Body.String())
		}
		api.External = staticExternal{p: fileOwner}
		if w := do(api.ExternalHandler(), "GET", strings.Replace(opsRoute, "survival", "missing", 1), "", nil); w.Code != http.StatusNotFound {
			t.Fatalf("unknown server: code = %d", w.Code)
		}
		if w := do(api.ExternalHandler(), "GET", strings.Replace(opsRoute, "survival", "X", 1), "", nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_name" {
			t.Fatalf("bad name: code = %d (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("no executor -> 503, a failing one -> 500", func(t *testing.T) {
		api, _, _, files := mkFiles(t)
		api.External = staticExternal{p: fileOwner}
		files.err = errors.New("the cluster said no")
		if w := list(api); w.Code != http.StatusInternalServerError {
			t.Fatalf("failing: code = %d (%s)", w.Code, w.Body.String())
		}
		api.Files = nil
		if w := list(api); w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "files_unavailable" {
			t.Fatalf("nil: code = %d (%s)", w.Code, w.Body.String())
		}
	})
}
