package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/submit"
)

func TestContextUploadPartForwardsOffsetBodyAndPrincipal(t *testing.T) {
	fs := &fakeSubmissions{progress: submit.UploadProgress{Received: 8, PartMaxBytes: 33554432, MaxContextBytes: 1073741824}}
	api := appSubAPI(fs)
	w := do(api.ExternalHandler(), "PUT", "/api/v1/me/submissions/sub-9/context/upload?offset=4", "abcd",
		map[string]string{"Content-Type": "application/octet-stream", "Content-Digest": contentDigestOf("abcd")})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != `{"received":8,"part_max_bytes":33554432,"max_context_bytes":1073741824}`+"\n" {
		t.Fatalf("body = %s", got)
	}
	if fs.chunkID != "sub-9" || fs.chunkBy != "user-7" || fs.partOffset != 4 || fs.partBody != "abcd" {
		t.Fatalf("forwarded id=%q by=%q offset=%d body=%q, want sub-9 user-7 4 abcd", fs.chunkID, fs.chunkBy, fs.partOffset, fs.partBody)
	}
}

func TestContextUploadPartNeedsAnOffset(t *testing.T) {
	for _, target := range []string{
		"/api/v1/me/submissions/sub-9/context/upload",
		"/api/v1/me/submissions/sub-9/context/upload?offset=four",
	} {
		fs := &fakeSubmissions{}
		w := do(appSubAPI(fs).ExternalHandler(), "PUT", target, "abcd", nil)
		if w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("%s: code = %d (%s), want 400 bad_request", target, w.Code, w.Body.String())
		}
		if fs.chunkID != "" {
			t.Fatalf("%s: the part reached the lane", target)
		}
	}
}

func TestContextUploadStatusReportsTheStagedLength(t *testing.T) {
	fs := &fakeSubmissions{progress: submit.UploadProgress{Received: 50331648, PartMaxBytes: 33554432, MaxContextBytes: 1073741824}}
	w := do(appSubAPI(fs).ExternalHandler(), "GET", "/api/v1/me/submissions/sub-9/context/upload", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != `{"received":50331648,"part_max_bytes":33554432,"max_context_bytes":1073741824}`+"\n" {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	if fs.chunkID != "sub-9" || fs.chunkBy != "user-7" {
		t.Fatalf("asked for id=%q by=%q, want sub-9 user-7", fs.chunkID, fs.chunkBy)
	}
}

func TestContextUploadErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
		want string
		msg  string
		// retry is the Retry-After the answer must carry, if any.
		retry string
	}{
		{"offset mismatch", &submit.OffsetMismatchError{Received: 12}, 409, "upload_offset_mismatch", "the upload holds 12 bytes", ""},
		{"busy", submit.ErrUploadBusy, 409, "upload_busy", "", ""},
		{"part too large", fmt.Errorf("submit: write upload part: %w", submit.ErrPartTooLarge), 413, "part_too_large", "", ""},
		{"not owned", submit.ErrNotFound, 404, "not_found", "", ""},
		{"no part store", submit.ErrUploadsUnavailable, 503, "uploads_unavailable", "", ""},
		{"store did not answer", fmt.Errorf("%w: size of the context of sub-3: dial tcp: i/o timeout", submit.ErrStoreUnavailable),
			503, "uploads_store_unavailable", "send the request again", "5"},
	} {
		fs := &fakeSubmissions{chunkErr: tc.err}
		w := do(appSubAPI(fs).ExternalHandler(), "PUT", "/api/v1/me/submissions/sub-9/context/upload?offset=12", "abcd",
			map[string]string{"Content-Digest": contentDigestOf("abcd")})
		if w.Code != tc.code || decodeErr(t, w) != tc.want {
			t.Errorf("%s: %d %s, want %d %s", tc.name, w.Code, w.Body.String(), tc.code, tc.want)
		}
		if tc.msg != "" && !strings.Contains(w.Body.String(), tc.msg) {
			t.Errorf("%s: body %s does not say %q", tc.name, w.Body.String(), tc.msg)
		}
		if got := w.Header().Get("Retry-After"); got != tc.retry {
			t.Errorf("%s: Retry-After = %q, want %q", tc.name, got, tc.retry)
		}
	}
}

// Completion is where a context lands: it holds the per-user upload cooldown and
// writes the same audit event as the single upload, and a failed completion
// gives the cooldown back.
func TestContextUploadCompleteHoldsTheCooldownAndAudits(t *testing.T) {
	repo := newFakeRepo()
	fs := &fakeSubmissions{}
	api := appSubAPI(fs)
	api.Repo = repo
	api.SubmitUploadCooldown = time.Minute
	eh := api.ExternalHandler()

	fs.completeErr = submit.ErrUploadsUnavailable
	if w := do(eh, "POST", "/api/v1/me/submissions/sub-9/context/upload/complete", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed completion: code = %d, want 503 (%s)", w.Code, w.Body.String())
	}
	fs.completeErr = nil
	w := do(eh, "POST", "/api/v1/me/submissions/sub-9/context/upload/complete", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("completion right after a failed one: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if fs.chunkID != "sub-9" || fs.chunkBy != "user-7" {
		t.Fatalf("completed id=%q by=%q, want sub-9 user-7", fs.chunkID, fs.chunkBy)
	}
	var audited int
	for _, e := range repo.audits {
		if e.Action == "submission.upload" {
			audited++
		}
	}
	if audited != 1 {
		t.Fatalf("submission.upload audits = %d, want 1 (only the completion that stored): %+v", audited, repo.audits)
	}
	w = do(eh, "POST", "/api/v1/me/submissions/sub-9/context/upload/complete", "", nil)
	if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "submission_cooldown" {
		t.Fatalf("second completion in the window: %d %s, want 429 submission_cooldown", w.Code, w.Body.String())
	}
	// A part never waits on the cooldown.
	if w := do(eh, "PUT", "/api/v1/me/submissions/sub-9/context/upload?offset=0", "\x1f\x8b",
		map[string]string{"Content-Digest": contentDigestOf("\x1f\x8b")}); w.Code != http.StatusOK {
		t.Fatalf("part inside the cooldown: code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

func TestContextUploadWithoutServiceIs503(t *testing.T) {
	api := appSubAPI(&fakeSubmissions{})
	api.Submissions = nil
	eh := api.ExternalHandler()
	for _, rq := range [][2]string{
		{"GET", "/api/v1/me/submissions/sub-9/context/upload"},
		{"PUT", "/api/v1/me/submissions/sub-9/context/upload?offset=0"},
		{"POST", "/api/v1/me/submissions/sub-9/context/upload/complete"},
	} {
		if w := do(eh, rq[0], rq[1], "", nil); w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", rq[0], rq[1], w.Code)
		}
	}
}

// A context upload, whole or in parts, carries the SHA-256 of its body: one
// without it is refused before the lane sees it, and bytes that do not hash to
// it are refused as changed on the way (the lane keeps none of them), so the
// client sends them again.
func TestContextUploadsCheckTheBodyDigest(t *testing.T) {
	body := "\x1f\x8b\x08\x00 the modpack bytes"
	for _, rq := range []struct{ name, method, target string }{
		{"a part", "PUT", "/api/v1/me/submissions/sub-9/context/upload?offset=0"},
		{"a whole context", "POST", "/api/v1/me/submissions/sub-9/context"},
	} {
		for _, tc := range []struct {
			name    string
			sent    string
			headers map[string]string
			code    int
			want    string
		}{
			{"without a digest", body, nil, http.StatusBadRequest, "digest_required"},
			{"changed on the way", body[:len(body)-1] + "X", map[string]string{"Content-Digest": contentDigestOf(body)}, http.StatusBadRequest, "digest_mismatch"},
			{"as sent", body, map[string]string{"Content-Digest": contentDigestOf(body)}, http.StatusOK, ""},
		} {
			t.Run(rq.name+" "+tc.name, func(t *testing.T) {
				fs := &fakeSubmissions{}
				w := do(appSubAPI(fs).ExternalHandler(), rq.method, rq.target, tc.sent, tc.headers)
				if w.Code != tc.code {
					t.Fatalf("code = %d (%s), want %d", w.Code, w.Body.String(), tc.code)
				}
				if tc.want != "" && decodeErr(t, w) != tc.want {
					t.Fatalf("error = %s, want %s", w.Body.String(), tc.want)
				}
				reached := fs.chunkID != "" || fs.uploadedID != ""
				if reached != (tc.headers != nil) {
					t.Fatalf("the body reached the lane: %v, want %v", reached, tc.headers != nil)
				}
			})
		}
	}
}
