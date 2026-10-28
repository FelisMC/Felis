package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	brandingPath       = "/api/v1/settings/branding"
	publicBrandingPath = "/api/v1/branding"
)

func brandingOwner() (*API, *fakeRepo) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}}
	return a, repo
}

// brandingVisitor is another replica; its clock passes the cache lifetime on every read.
func brandingVisitor(repo *fakeRepo) *API {
	a := newTestAPI(repo, newFakeCluster())
	a.External = staticExternal{}
	at := a.now()
	a.Now = func() time.Time {
		at = at.Add(brandingCacheTTL)
		return at
	}
	return a
}

func pngOf(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// webpOf builds a RIFF container holding one lossless chunk.
func webpOf() []byte {
	body := append([]byte("WEBPVP8L\x02\x00\x00\x00"), 0, 0)
	return append(binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(len(body))), body...)
}

func versionOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readBrandingSettings(t *testing.T, a *API) brandingView {
	t.Helper()
	w := do(a.ExternalHandler(), "GET", brandingPath, "", nil)
	var v brandingView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("read settings = %d %s", w.Code, w.Body.String())
	}
	return v
}

func readPublicBranding(t *testing.T, a *API) (publicBrandingView, *httptest.ResponseRecorder) {
	t.Helper()
	w := do(a.ExternalHandler(), "GET", publicBrandingPath, "", nil)
	var v publicBrandingView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("read public = %d %s", w.Code, w.Body.String())
	}
	return v, w
}

func putBrandingSettings(a *API, name, notice string, poweredBy bool, revision string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(map[string]any{"name": name, "notice": notice, "show_powered_by": poweredBy, "revision": revision})
	return do(a.ExternalHandler(), "PUT", brandingPath, string(raw), jsonHeader)
}

func putBrandingImage(a *API, kind, contentType string, data []byte) *httptest.ResponseRecorder {
	return do(a.ExternalHandler(), "PUT", brandingPath+"/"+kind, string(data), ctHeader(contentType))
}

func TestBrandingWritesRequireOwnerAndFreshProof(t *testing.T) {
	type route struct{ method, path string }
	a, repo := brandingOwner()
	writes := []route{
		{"PUT", brandingPath},
		{"PUT", brandingPath + "/logo"}, {"DELETE", brandingPath + "/logo"},
		{"PUT", brandingPath + "/background"}, {"DELETE", brandingPath + "/background"},
	}
	for _, p := range []*Principal{nil, {UserID: "admin", Role: "admin", ViaAdminAccess: true}, {UserID: "owner", Role: "owner"}} {
		a.External = staticExternal{p: p}
		for _, rt := range append([]route{{"GET", brandingPath}}, writes...) {
			if w := do(a.ExternalHandler(), rt.method, rt.path, `{}`, jsonHeader); w.Code != 401 && w.Code != 403 {
				t.Fatalf("%s %s as %+v = %d", rt.method, rt.path, p, w.Code)
			}
		}
	}
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: p.UserID, UserVerified: true}
	a.External = staticExternal{p: p}
	for _, rt := range writes {
		if w := do(a.ExternalHandler(), rt.method, rt.path, `{}`, jsonHeader); w.Code != 403 || decodeErr(t, w) != "reauth_required" {
			t.Fatalf("%s %s without proof = %d %s", rt.method, rt.path, w.Code, w.Body.String())
		}
	}
	if len(repo.settings) != 0 {
		t.Fatalf("a refused write stored %v", repo.settings)
	}
}

func TestBrandingSettingsSaveRules(t *testing.T) {
	a, repo := brandingOwner()
	initial := readBrandingSettings(t, a)
	if initial.Name != "" || initial.Notice != "" || !initial.ShowPoweredBy || initial.HasLogo || initial.HasBackground {
		t.Fatalf("unset settings = %+v", initial)
	}
	for _, c := range []struct{ label, name, notice string }{
		{"name over 40 characters", strings.Repeat("名", 41), ""},
		{"notice over 1000 characters", "", strings.Repeat("a", 1001)},
		{"control character in notice", "", "a\x07b"},
		{"line break in name", "a\nb", ""},
	} {
		if w := putBrandingSettings(a, c.name, c.notice, true, initial.Revision); w.Code != 400 || decodeErr(t, w) != "bad_request" {
			t.Errorf("%s = %d %s", c.label, w.Code, w.Body.String())
		}
	}
	if len(repo.settings) != 0 {
		t.Fatalf("a rejected save stored %v", repo.settings)
	}

	name, notice := strings.Repeat("名", 40), "<b>first</b>\r\nsecond"+strings.Repeat("x", 980)
	if w := putBrandingSettings(a, "  "+name+" ", " "+notice+"\n", false, initial.Revision); w.Code != 200 {
		t.Fatalf("save at the limits = %d %s", w.Code, w.Body.String())
	}
	saved := readBrandingSettings(t, a)
	if saved.Name != name || saved.Notice != notice || saved.ShowPoweredBy || saved.Revision == initial.Revision {
		t.Fatalf("saved = %+v", saved)
	}
	if w := putBrandingSettings(a, "Other", "", true, initial.Revision); w.Code != 409 || decodeErr(t, w) != "conflict" {
		t.Fatalf("stale revision = %d %s", w.Code, w.Body.String())
	}
	if got := readBrandingSettings(t, a); got.Name != name {
		t.Fatalf("a stale save changed the name to %q", got.Name)
	}
	if !hasAudit(repo, "platform.branding") {
		t.Error("the save was not audited")
	}
}

func TestBrandingImagesUploadServeAndRemove(t *testing.T) {
	a, repo := brandingOwner()
	visitor := brandingVisitor(repo)
	logo, background := pngOf(t, 1, 1), webpOf()

	for _, c := range []struct {
		label, kind, contentType string
		body                     []byte
		status                   int
	}{
		{"logo of 257 KiB", "logo", "image/png", bytes.Repeat([]byte{'x'}, 257<<10), 413},
		{"background over 2 MiB", "background", "image/png", bytes.Repeat([]byte{'x'}, 2<<20+1), 413},
		{"empty body", "logo", "image/png", nil, 400},
		{"WebP declared as PNG", "logo", "image/png", background, 400},
		{"PNG declared as WebP", "logo", "image/webp", logo, 400},
		{"PNG wider than 4096", "logo", "image/png", pngOf(t, 4097, 1), 400},
		{"WebP with a wrong length field", "background", "image/webp", background[:len(background)-1], 400},
		{"SVG", "logo", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 415},
		{"plain text", "logo", "text/plain", logo, 415},
	} {
		w := putBrandingImage(a, c.kind, c.contentType, c.body)
		if w.Code != c.status || (c.status == 400 && decodeErr(t, w) != "bad_image") {
			t.Errorf("%s = %d %s, want %d", c.label, w.Code, w.Body.String(), c.status)
		}
	}
	if len(repo.settings) != 0 {
		t.Fatalf("a rejected upload stored %v", repo.settings)
	}

	if w := putBrandingImage(a, "logo", "image/png", logo); w.Code != 200 {
		t.Fatalf("logo upload = %d %s", w.Code, w.Body.String())
	}
	if w := putBrandingImage(a, "background", "image/webp", background); w.Code != 200 {
		t.Fatalf("background upload = %d %s", w.Code, w.Body.String())
	}
	if v := readBrandingSettings(t, a); !v.HasLogo || !v.HasBackground {
		t.Fatalf("settings after upload = %+v", v)
	}
	public, _ := readPublicBranding(t, visitor)
	if public.Logo == nil || *public.Logo != "/api/v1/branding/logo?v="+versionOf(logo)[:16] ||
		public.Background == nil || *public.Background != "/api/v1/branding/background?v="+versionOf(background)[:16] {
		t.Fatalf("public image URLs = %v %v", public.Logo, public.Background)
	}

	w := do(visitor.ExternalHandler(), "GET", "/api/v1/branding/logo", "", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), logo) || w.Header().Get("Content-Type") != "image/png" ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("logo = %d %v", w.Code, w.Header())
	}
	etag := w.Header().Get("ETag")
	if etag != `"`+versionOf(logo)+`"` {
		t.Fatalf("logo ETag = %s", etag)
	}
	if w := do(visitor.ExternalHandler(), "GET", "/api/v1/branding/logo", "", map[string]string{"If-None-Match": etag}); w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("revalidated logo = %d with %d bytes", w.Code, w.Body.Len())
	}
	if w := do(visitor.ExternalHandler(), "GET", "/api/v1/branding/background", "", nil); w.Code != 200 || w.Header().Get("Content-Type") != "image/webp" || !bytes.Equal(w.Body.Bytes(), background) {
		t.Fatalf("background = %d %v", w.Code, w.Header())
	}

	if w := putBrandingImage(a, "logo", "image/png", pngOf(t, 2, 2)); w.Code != 200 {
		t.Fatalf("logo replace = %d", w.Code)
	}
	if replaced, _ := readPublicBranding(t, visitor); *replaced.Logo == *public.Logo {
		t.Fatal("a replaced logo kept its URL")
	}

	for _, kind := range []string{"logo", "background"} {
		if w := do(a.ExternalHandler(), "DELETE", brandingPath+"/"+kind, "", nil); w.Code != 200 {
			t.Fatalf("remove %s = %d", kind, w.Code)
		}
		if w := do(visitor.ExternalHandler(), "GET", "/api/v1/branding/"+kind, "", nil); w.Code != 404 || decodeErr(t, w) != "not_found" {
			t.Fatalf("%s after removal = %d", kind, w.Code)
		}
	}
	if v := readBrandingSettings(t, a); v.HasLogo || v.HasBackground {
		t.Fatalf("settings after removal = %+v", v)
	}
	if after, _ := readPublicBranding(t, visitor); after.Logo != nil || after.Background != nil {
		t.Fatalf("public after removal = %+v", after)
	}
	for _, action := range []string{"platform.branding_logo", "platform.branding_logo.clear", "platform.branding_background", "platform.branding_background.clear"} {
		if !hasAudit(repo, action) {
			t.Errorf("%s was not audited", action)
		}
	}
}

func TestPublicBrandingDefaultsAndRevalidation(t *testing.T) {
	owner, repo := brandingOwner()
	visitor := brandingVisitor(repo)

	view, w := readPublicBranding(t, visitor)
	if view.Name != "Felis" || view.Notice != "" || !view.ShowPoweredBy || view.Logo != nil || view.Background != nil {
		t.Fatalf("defaults = %+v", view)
	}
	if w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
	etag := w.Header().Get("ETag")
	if got := do(visitor.ExternalHandler(), "GET", publicBrandingPath, "", map[string]string{"If-None-Match": etag}); got.Code != 304 || got.Body.Len() != 0 {
		t.Fatalf("unchanged branding = %d with %d bytes", got.Code, got.Body.Len())
	}

	initial := readBrandingSettings(t, owner)
	notice := "<b>Maintenance</b> tonight\nback by 9"
	if w := putBrandingSettings(owner, "Acme", notice, false, initial.Revision); w.Code != 200 {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	changed, w := readPublicBranding(t, visitor)
	if changed.Name != "Acme" || changed.Notice != notice || changed.ShowPoweredBy {
		t.Fatalf("saved branding = %+v", changed)
	}
	if w.Header().Get("ETag") == etag {
		t.Fatal("the ETag survived a change")
	}
	if got := do(visitor.ExternalHandler(), "GET", publicBrandingPath, "", map[string]string{"If-None-Match": etag}); got.Code != 200 {
		t.Fatalf("stale ETag = %d", got.Code)
	}

	repo.failGetSetting = errors.New("database down")
	if outage, _ := readPublicBranding(t, visitor); outage.Name != "Felis" || outage.Notice != "" || !outage.ShowPoweredBy || outage.Logo != nil {
		t.Fatalf("store outage = %+v", outage)
	}
}
