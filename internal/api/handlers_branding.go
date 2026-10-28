package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	brandingKey           = "branding"
	brandingLogoKey       = "branding_logo"
	brandingBackgroundKey = "branding_background"

	defaultBrandName     = "Felis"
	brandingNameMax      = 40
	brandingNoticeMax    = 1000
	brandingImageMaxSide = 4096
	// brandingCacheTTL bounds how long another replica keeps serving replaced branding.
	brandingCacheTTL = 30 * time.Second
)

// brandingCache spares the public read from loading both images on every page load.
type brandingCache struct {
	mu   sync.Mutex
	view publicBrandingView
	at   time.Time
}

func (c *brandingCache) forget() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

type brandingImageKind struct {
	name     string
	key      string
	action   string
	maxBytes int64
}

var (
	brandingLogo       = brandingImageKind{name: "logo", key: brandingLogoKey, action: "platform.branding_logo", maxBytes: 256 << 10}
	brandingBackground = brandingImageKind{name: "background", key: brandingBackgroundKey, action: "platform.branding_background", maxBytes: 2 << 20}
)

// An empty name stands for the default name.
type brandingSettings struct {
	Name          string `json:"name"`
	Notice        string `json:"notice"`
	ShowPoweredBy bool   `json:"show_powered_by"`
}

type brandingView struct {
	brandingSettings
	HasLogo       bool   `json:"has_logo"`
	HasBackground bool   `json:"has_background"`
	Revision      string `json:"revision"`
}

type publicBrandingView struct {
	Name          string  `json:"name"`
	Notice        string  `json:"notice"`
	ShowPoweredBy bool    `json:"show_powered_by"`
	Logo          *string `json:"logo"`
	Background    *string `json:"background"`
}

type brandingImage struct {
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
	Data   string `json:"data"`
}

func (b brandingSettings) check() error {
	switch {
	case utf8.RuneCountInString(b.Name) > brandingNameMax:
		return newError(http.StatusBadRequest, "bad_request", "name must be at most %d characters", brandingNameMax)
	case strings.ContainsFunc(b.Name, unicode.IsControl):
		return newError(http.StatusBadRequest, "bad_request", "name must not contain control characters")
	case utf8.RuneCountInString(b.Notice) > brandingNoticeMax:
		return newError(http.StatusBadRequest, "bad_request", "notice must be at most %d characters", brandingNoticeMax)
	case strings.ContainsFunc(b.Notice, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' }):
		return newError(http.StatusBadRequest, "bad_request", "notice may contain line breaks but no other control characters")
	}
	return nil
}

func (i *brandingImage) valid() bool {
	return (i.Type == "image/png" || i.Type == "image/webp") && len(i.SHA256) == sha256.Size*2
}

// url carries a content version so a replaced image is fetched anew despite the long cache lifetime.
func (i *brandingImage) url(kind brandingImageKind) *string {
	if i == nil {
		return nil
	}
	u := "/api/v1/branding/" + kind.name + "?v=" + i.SHA256[:16]
	return &u
}

// readBranding leaves raw nil until the settings are first saved.
func (a *API) readBranding(ctx context.Context) (brandingView, []byte, error) {
	view := brandingView{brandingSettings: brandingSettings{ShowPoweredBy: true}}
	raw, err := a.Repo.GetSetting(ctx, brandingKey)
	switch {
	case errors.Is(err, ErrNotFound):
		raw = nil
	case err != nil:
		return view, nil, err
	default:
		if err := json.Unmarshal(raw, &view.brandingSettings); err != nil {
			return view, nil, err
		}
		if view.brandingSettings.check() != nil {
			return view, nil, errors.New("invalid persisted branding")
		}
	}
	canonical, _ := json.Marshal(view.brandingSettings)
	sum := sha256.Sum256(canonical)
	view.Revision = hex.EncodeToString(sum[:])
	return view, raw, nil
}

// readBrandingImage returns nil when no image is set.
func (a *API) readBrandingImage(ctx context.Context, kind brandingImageKind) (*brandingImage, error) {
	raw, err := a.Repo.GetSetting(ctx, kind.key)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var img *brandingImage
	if err := json.Unmarshal(raw, &img); err != nil {
		return nil, err
	}
	if img != nil && !img.valid() {
		return nil, errors.New("invalid persisted " + kind.name)
	}
	return img, nil
}

func (a *API) handleGetBranding(w http.ResponseWriter, r *http.Request) {
	view, _, err := a.readBranding(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	logo, err := a.readBrandingImage(r.Context(), brandingLogo)
	if err != nil {
		writeError(w, r, err)
		return
	}
	background, err := a.readBrandingImage(r.Context(), brandingBackground)
	if err != nil {
		writeError(w, r, err)
		return
	}
	view.HasLogo, view.HasBackground = logo != nil, background != nil
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleSetBranding(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		brandingSettings
		Revision string `json:"revision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Notice = strings.TrimSpace(body.Notice)
	if err := body.brandingSettings.check(); err != nil {
		writeError(w, r, err)
		return
	}
	current, expected, err := a.readBranding(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.Revision != current.Revision {
		writeError(w, r, newError(http.StatusConflict, "conflict", "branding changed; reload before saving"))
		return
	}
	raw, _ := json.Marshal(body.brandingSettings)
	if err := a.Repo.CompareAndSetSetting(r.Context(), brandingKey, expected, raw); err != nil {
		writeError(w, r, err)
		return
	}
	a.brandingCache.forget()
	a.audit(r, "platform.branding", "platform")
	a.handleGetBranding(w, r)
}

func (a *API) handleSetBrandingImage(kind brandingImageKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.requireReauth(w, r, principalFromContext(r.Context())) {
			return
		}
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || (contentType != "image/png" && contentType != "image/webp") {
			writeError(w, r, newError(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be image/png or image/webp"))
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, kind.maxBytes))
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, r, newError(http.StatusRequestEntityTooLarge, "too_large", "the %s may be at most %d KiB", kind.name, kind.maxBytes>>10))
			return
		}
		if err != nil {
			writeError(w, r, newError(http.StatusBadRequest, "bad_request", "the image could not be read: %v", err))
			return
		}
		if err := checkBrandingImage(contentType, data); err != nil {
			writeError(w, r, err)
			return
		}
		sum := sha256.Sum256(data)
		raw, _ := json.Marshal(brandingImage{Type: contentType, SHA256: hex.EncodeToString(sum[:]), Data: base64.StdEncoding.EncodeToString(data)})
		if err := a.Repo.SetSetting(r.Context(), kind.key, raw); err != nil {
			writeError(w, r, err)
			return
		}
		a.brandingCache.forget()
		a.audit(r, kind.action, "platform")
		a.handleGetBranding(w, r)
	}
}

func (a *API) handleClearBrandingImage(kind brandingImageKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.requireReauth(w, r, principalFromContext(r.Context())) {
			return
		}
		if err := a.Repo.SetSetting(r.Context(), kind.key, []byte("null")); err != nil {
			writeError(w, r, err)
			return
		}
		a.brandingCache.forget()
		a.audit(r, kind.action+".clear", "platform")
		a.handleGetBranding(w, r)
	}
}

// checkBrandingImage inspects only the headers; the server never decodes pixels.
func checkBrandingImage(contentType string, data []byte) error {
	switch contentType {
	case "image/png":
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width > brandingImageMaxSide || cfg.Height > brandingImageMaxSide {
			return newError(http.StatusBadRequest, "bad_image", "the file is not a PNG image of at most %d×%d pixels", brandingImageMaxSide, brandingImageMaxSide)
		}
	case "image/webp":
		if !isWebP(data) {
			return newError(http.StatusBadRequest, "bad_image", "the file is not a WebP image")
		}
	}
	return nil
}

// isWebP checks the RIFF header, its length field and the first chunk type.
func isWebP(data []byte) bool {
	if len(data) < 20 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return false
	}
	if int64(binary.LittleEndian.Uint32(data[4:8])) != int64(len(data))-8 {
		return false
	}
	switch string(data[12:16]) {
	case "VP8 ", "VP8L", "VP8X":
		return true
	}
	return false
}

func defaultPublicBranding() publicBrandingView {
	return publicBrandingView{Name: defaultBrandName, ShowPoweredBy: true}
}

func (a *API) cachedPublicBranding(ctx context.Context) (publicBrandingView, error) {
	c := &a.brandingCache
	c.mu.Lock()
	defer c.mu.Unlock()
	now := a.now()
	if !c.at.IsZero() && now.Sub(c.at) < brandingCacheTTL {
		return c.view, nil
	}
	view, err := a.readPublicBranding(ctx)
	if err != nil {
		return view, err
	}
	c.view, c.at = view, now
	return view, nil
}

func (a *API) readPublicBranding(ctx context.Context) (publicBrandingView, error) {
	settings, _, err := a.readBranding(ctx)
	if err != nil {
		return publicBrandingView{}, err
	}
	logo, err := a.readBrandingImage(ctx, brandingLogo)
	if err != nil {
		return publicBrandingView{}, err
	}
	background, err := a.readBrandingImage(ctx, brandingBackground)
	if err != nil {
		return publicBrandingView{}, err
	}
	return publicBrandingView{
		Name:          cmp.Or(settings.Name, defaultBrandName),
		Notice:        settings.Notice,
		ShowPoweredBy: settings.ShowPoweredBy,
		Logo:          logo.url(brandingLogo),
		Background:    background.url(brandingBackground),
	}, nil
}

// handlePublicBranding answers 200 with the defaults on store failure so the panel still renders.
func (a *API) handlePublicBranding(w http.ResponseWriter, r *http.Request) {
	view, err := a.cachedPublicBranding(r.Context())
	if err != nil {
		log.Printf("branding: %v", err)
		view = defaultPublicBranding()
	}
	body, _ := json.Marshal(view)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

func (a *API) handleBrandingImage(kind brandingImageKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		img, err := a.readBrandingImage(r.Context(), kind)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if img == nil {
			writeError(w, r, newError(http.StatusNotFound, "not_found", "no %s is set", kind.name))
			return
		}
		data, err := base64.StdEncoding.DecodeString(img.Data)
		if err != nil {
			writeError(w, r, err)
			return
		}
		etag := `"` + img.SHA256 + `"`
		h := w.Header()
		h.Set("Content-Type", img.Type)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("ETag", etag)
		h.Set("Cache-Control", "public, max-age=86400")
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}
}

// etagMatches applies the weak comparison If-None-Match uses.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
