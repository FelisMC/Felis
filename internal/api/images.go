package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/imagepin"
	"k8s.io/apimachinery/pkg/util/validation"
)

type ImageBuilder interface {
	Submit(ctx context.Context, req build.Request) (*build.Build, error)
	// GetMany omits unknown IDs and reads without reconciliation; listing submissions must not touch the cluster.
	GetMany(ctx context.Context, ids []string) (map[string]build.Build, error)
	// Sync reconciles against the Job and is idempotent for terminal builds.
	Sync(ctx context.Context, id string) (*build.Build, error)
	Cancel(ctx context.Context, id string) (*build.Build, error)
	ListBuilds(ctx context.Context, opts build.ListOpts) ([]build.Build, int, error)
	ListImages(ctx context.Context) ([]build.Image, error)
	AddExternalImage(ctx context.Context, imageRef, addedBy string) (*build.Image, error)
	RemoveImage(ctx context.Context, imageRef string) error
	ImageAdmitted(ctx context.Context, imageRef string) (bool, error)
	// Scan returns build.ErrNotFound when no scan is stored.
	Scan(ctx context.Context, id string) (*build.Scan, error)
}

type buildImageRequest struct {
	ImageRef   string `json:"image_ref"`
	Dockerfile string `json:"dockerfile"`
	ContextRef string `json:"context_ref"`
	BaseImage  string `json:"base_image,omitempty"`
}

type addImageRequest struct {
	ImageRef string `json:"image_ref"`
}

func (a *API) handleBuildImage(w http.ResponseWriter, r *http.Request) {
	if a.Builder == nil {
		writeError(w, r, errBuildUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	var body buildImageRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	bld, err := a.Builder.Submit(r.Context(), build.Request{
		ImageRef:    body.ImageRef,
		Dockerfile:  body.Dockerfile,
		ContextRef:  body.ContextRef,
		BaseImage:   body.BaseImage,
		RequestedBy: p.Email,
	})
	if err != nil {
		writeBuildError(w, r, err)
		return
	}
	a.audit(r, "image.build", bld.ImageRef)
	writeJSON(w, http.StatusAccepted, bld)
}

func (a *API) handleListBuilds(w http.ResponseWriter, r *http.Request) {
	if a.Builder == nil {
		writeError(w, r, errBuildUnavailable)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	builds, total, err := a.Builder.ListBuilds(r.Context(), build.ListOpts{
		Query: q.Get("query"), Limit: limit, Offset: offset,
	})
	if err != nil {
		writeBuildError(w, r, err)
		return
	}
	if builds == nil {
		builds = []build.Build{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"builds": builds, "total": total})
}

func (a *API) handleGetBuild(w http.ResponseWriter, r *http.Request) {
	if a.Builder == nil {
		writeError(w, r, errBuildUnavailable)
		return
	}
	id := r.PathValue("id")
	bld, err := a.Builder.Sync(r.Context(), id)
	if err != nil {
		writeBuildError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bld)
}

// handleBuildLogs validates error conditions before committing SSE headers.
func (a *API) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Validate buildID before using it in a label selector to prevent selector injection.
	if errs := validation.IsValidLabelValue(id); len(errs) > 0 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"build id is not a valid build identifier"))
		return
	}
	if a.BuildLogs == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "build_logs_unavailable",
			"build log streaming is not configured"))
		return
	}
	p := principalFromContext(r.Context())

	// Acquire the per-principal stream slot before opening the upstream log connection.
	release, ok := a.streamGate().acquire(streamKey(p))
	if !ok {
		writeError(w, r, newError(http.StatusTooManyRequests, "too_many_streams",
			"too many open build-log streams; close one and retry"))
		return
	}
	defer release()

	ctx := r.Context()
	if since, ok := logSinceFromRequest(r, a.now()); ok {
		ctx = withLogSince(ctx, since)
	}
	src, err := a.BuildLogs.StreamLogs(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, r, newError(http.StatusNotFound, "not_found",
			"no build pod found for this id (the build has not started yet, or its pod was cleaned up)"))
		return
	case errors.Is(err, ErrConsoleUnavailable):
		writeError(w, r, newError(http.StatusServiceUnavailable, "build_logs_unavailable",
			"build logs are not reachable yet (the build pod may still be pulling its image); retry shortly"))
		return
	case err != nil:
		writeError(w, r, newError(http.StatusInternalServerError, "internal",
			"could not open build logs"))
		return
	}
	a.audit(r, "image.build.logs", id)
	relayLogStream(w, r, src, a.streamRecheck(r, func(_ context.Context, p *Principal) error {
		if !p.IsAdmin() {
			return errForbidden
		}
		return nil
	}), a.streamsClosing())
}

func (a *API) handleCancelBuild(w http.ResponseWriter, r *http.Request) {
	if a.Builder == nil {
		writeError(w, r, errBuildUnavailable)
		return
	}
	id := r.PathValue("id")
	bld, err := a.Builder.Cancel(r.Context(), id)
	if err != nil {
		writeBuildError(w, r, err)
		return
	}
	a.audit(r, "image.build.cancel", bld.ImageRef)
	writeJSON(w, http.StatusOK, bld)
}

type buildScanView struct {
	BuildID   string            `json:"build_id"`
	ScannedAt time.Time         `json:"scanned_at"`
	Summary   build.ScanSummary `json:"summary"`
	HasReport bool              `json:"has_report"`
	HasSBOM   bool              `json:"has_sbom"`
}

// handleBuildScan returns the recorded verdict; later policy changes do not reinterpret past scans.
func (a *API) handleBuildScan(w http.ResponseWriter, r *http.Request) {
	scan, ok := a.buildScan(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, buildScanView{
		BuildID: scan.BuildID, ScannedAt: scan.ScannedAt, Summary: scan.Summary,
		HasReport: len(scan.ReportGz) > 0, HasSBOM: len(scan.SBOMGz) > 0,
	})
}

func (a *API) handleBuildScanReport(w http.ResponseWriter, r *http.Request) {
	a.serveScanDocument(w, r, "report", func(s *build.Scan) []byte { return s.ReportGz },
		"application/json", "-trivy.json", "image.build.scan.report")
}

func (a *API) handleBuildSBOM(w http.ResponseWriter, r *http.Request) {
	a.serveScanDocument(w, r, "SBOM", func(s *build.Scan) []byte { return s.SBOMGz },
		"application/vnd.cyclonedx+json", ".cdx.json", "image.build.sbom")
}

func (a *API) buildScan(w http.ResponseWriter, r *http.Request) (*build.Scan, bool) {
	if a.Builder == nil {
		writeError(w, r, errBuildUnavailable)
		return nil, false
	}
	scan, err := a.Builder.Scan(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, build.ErrNotFound):
		writeError(w, r, newError(http.StatusNotFound, "scan_not_found",
			"this build has no scan: it has not reached the scan step, or it ran before builds kept their scans"))
		return nil, false
	case err != nil:
		writeBuildError(w, r, err)
		return nil, false
	}
	return scan, true
}

// serveScanDocument forces download because report fields contain untrusted image content.
func (a *API) serveScanDocument(w http.ResponseWriter, r *http.Request, what string,
	doc func(*build.Scan) []byte, contentType, suffix, action string) {
	scan, ok := a.buildScan(w, r)
	if !ok {
		return
	}
	gz := doc(scan)
	if len(gz) == 0 {
		writeError(w, r, newError(http.StatusNotFound, "scan_document_not_kept",
			"this build's scan kept no %s: it was too large to keep, or the step that writes it failed", what))
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer zr.Close()
	a.audit(r, action, scan.BuildID)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+scan.BuildID+suffix+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, zr)
}

func (a *API) handleListImages(w http.ResponseWriter, r *http.Request) {
	if a.Builder == nil {
		writeError(w, r, errBuildUnavailable)
		return
	}
	images, err := a.Builder.ListImages(r.Context())
	if err != nil {
		writeBuildError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": images})
}

func (a *API) handleAddImage(w http.ResponseWriter, r *http.Request) {
	if a.Builder == nil {
		writeError(w, r, errBuildUnavailable)
		return
	}
	p := principalFromContext(r.Context())
	var body addImageRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	img, err := a.Builder.AddExternalImage(r.Context(), body.ImageRef, p.Email)
	if err != nil {
		writeBuildError(w, r, err)
		return
	}
	a.audit(r, "image.admit", img.ImageRef)
	writeJSON(w, http.StatusCreated, img)
}

func (a *API) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	if a.Builder == nil {
		writeError(w, r, errBuildUnavailable)
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"the ?ref= query parameter is required"))
		return
	}
	if err := a.Builder.RemoveImage(r.Context(), ref); err != nil {
		writeBuildError(w, r, err)
		return
	}
	a.audit(r, "image.remove", ref)
	w.WriteHeader(http.StatusNoContent)
}

var errBuildUnavailable = newError(http.StatusServiceUnavailable, "build_unavailable",
	"image build subsystem is not configured")

func writeBuildError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, build.ErrInvalid):
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "%s", err.Error()))
	case errors.Is(err, build.ErrNotFound):
		writeError(w, r, newError(http.StatusNotFound, "not_found", "build or image not found"))
	case errors.Is(err, build.ErrAlreadyTerminal):
		writeError(w, r, newError(http.StatusConflict, "already_terminal",
			"build has already finished"))
	default:
		writeError(w, r, err)
	}
}

// ImagePinner leaves unmanaged references unchanged.
type ImagePinner interface {
	Pin(ctx context.Context, ref string) (string, error)
}

// pinImage refuses unresolved managed tags so later pushes cannot silently change a server's image.
func (a *API) pinImage(ctx context.Context, ref string) (string, error) {
	if a.Images == nil {
		return ref, nil
	}
	pinned, err := a.Images.Pin(ctx, ref)
	switch {
	case errors.Is(err, imagepin.ErrNotFound) && imagepin.Pinned(ref):
		return "", newError(http.StatusBadRequest, "image_not_in_registry",
			"the registry no longer holds build %q (nothing referenced it, so it was pruned); pick a current tag", ref)
	case errors.Is(err, imagepin.ErrNotFound):
		return "", newError(http.StatusBadRequest, "image_not_in_registry",
			"image %q is whitelisted but the registry does not hold it; build or push it first", ref)
	case err != nil:
		return "", newError(http.StatusServiceUnavailable, "registry_unavailable",
			"could not resolve image %q to a digest: %v", ref, err)
	}
	return pinned, nil
}
