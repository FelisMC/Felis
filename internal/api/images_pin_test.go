package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"felis.lolicon.best/internal/imagepin"
)

const pinnedDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// fakePinner preserves already-pinned refs and resolves other refs to pinnedDigest unless err is set.
type fakePinner struct {
	err  error
	seen []string
}

func (f *fakePinner) Pin(_ context.Context, ref string) (string, error) {
	f.seen = append(f.seen, ref)
	if f.err != nil {
		return "", f.err
	}
	if imagepin.Pinned(ref) {
		return ref, nil
	}
	return ref + "@" + pinnedDigest, nil
}

func TestCreateServerStoresPinnedImage(t *testing.T) {
	api, _, cl, _ := newCreateAPI()
	pin := &fakePinner{}
	api.Images = pin

	w := do(api.ExternalHandler(), "POST", "/api/v1/servers", validCreateBody, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	if got, want := cl.created["survival"].Image, admittedImage+"@"+pinnedDigest; got != want {
		t.Errorf("created image = %q, want %q", got, want)
	}
	if len(pin.seen) != 1 || pin.seen[0] != admittedImage {
		t.Errorf("pinned refs = %v, want the admitted ref once", pin.seen)
	}
}

func TestCreateServerPinErrors(t *testing.T) {
	for _, tc := range []struct {
		err      error
		code     int
		wantCode string
	}{
		{fmt.Errorf("resolve: %w", imagepin.ErrNotFound), http.StatusBadRequest, "image_not_in_registry"},
		{errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable, "registry_unavailable"},
	} {
		api, _, cl, _ := newCreateAPI()
		api.Images = &fakePinner{err: tc.err}

		w := do(api.ExternalHandler(), "POST", "/api/v1/servers", validCreateBody, nil)
		if w.Code != tc.code || decodeErr(t, w) != tc.wantCode {
			t.Errorf("%v: got %d %s, want %d %s", tc.err, w.Code, w.Body.String(), tc.code, tc.wantCode)
		}
		if _, ok := cl.created["survival"]; ok {
			t.Errorf("%v: server created despite the pin failure", tc.err)
		}
	}
}

func TestPatchServerImageChangeUnconfirmed(t *testing.T) {
	api, repo, cl, _ := newPatchAPI()
	cl.byName["survival"].Image = "registry.felis.svc:5000/mc:0@" + pinnedDigest
	api.Images = &fakePinner{}

	w := patchSurvival(api, `{"image":"`+admittedImage+`"}`)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "image_change_unconfirmed" {
		t.Fatalf("got %d %s, want 409 image_change_unconfirmed", w.Code, w.Body.String())
	}
	if _, ok := cl.patched["survival"]; ok {
		t.Error("spec patched without confirmation")
	}
	if len(repo.audits) != 0 {
		t.Errorf("refused change audited: %+v", repo.audits)
	}
}

func TestPatchServerImageConfirmedAudited(t *testing.T) {
	api, repo, cl, _ := newPatchAPI()
	from := "registry.felis.svc:5000/mc:0@" + pinnedDigest
	cl.byName["survival"].Image = from
	api.Images = &fakePinner{}

	w := patchSurvival(api, `{"image":"`+admittedImage+`","confirmImageChange":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	to := admittedImage + "@" + pinnedDigest
	if p := cl.patched["survival"]; p.Image == nil || *p.Image != to {
		t.Fatalf("patched image = %v, want %q", p.Image, to)
	}
	if len(repo.audits) != 1 || repo.audits[0].Action != "server.patch" {
		t.Fatalf("audits = %+v, want one server.patch", repo.audits)
	}
	var payload map[string]string
	if err := json.Unmarshal(repo.audits[0].Payload, &payload); err != nil {
		t.Fatalf("audit payload %q: %v", repo.audits[0].Payload, err)
	}
	if payload["image_from"] != from || payload["image_to"] != to {
		t.Errorf("audit payload = %v, want image_from %q image_to %q", payload, from, to)
	}
}

func TestPatchServerImageSamePinIsNoChange(t *testing.T) {
	api, repo, cl, _ := newPatchAPI()
	cl.byName["survival"].Image = admittedImage + "@" + pinnedDigest
	api.Images = &fakePinner{}

	w := patchSurvival(api, `{"image":"`+admittedImage+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if p := cl.patched["survival"]; p.Image != nil {
		t.Errorf("patched image = %q, want no image change", *p.Image)
	}
	if !strings.Contains(w.Body.String(), `"patched":[]`) {
		t.Errorf("body = %s, want an empty patched list", w.Body.String())
	}
	if len(repo.audits) != 1 || repo.audits[0].Payload != nil {
		t.Errorf("audits = %+v, want a plain server.patch", repo.audits)
	}
}

func TestPatchServerImageRestoresPinnedBuild(t *testing.T) {
	api, _, cl, fb := newPatchAPI()
	cl.byName["survival"].Image = admittedImage + "@" + pinnedDigest
	pin := &fakePinner{}
	api.Images = pin
	old := admittedImage + "@sha256:" + fmt.Sprintf("%064d", 0)
	fb.admitted[old] = true

	w := patchSurvival(api, `{"image":"`+old+`","confirmImageChange":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if p := cl.patched["survival"]; p.Image == nil || *p.Image != old {
		t.Errorf("patched image = %v, want %q", p.Image, old)
	}
}
