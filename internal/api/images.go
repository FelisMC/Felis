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

// ImageBuilder is the build-subsystem surface the API depends on (spec §16). It
// is an interface so the image handlers are unit-tested against a fake; the
// production implementation is *build.Builder. All build operations are
// admin-tier (Zero Trust), enforced by adminOnly before these handlers run.
type ImageBuilder interface {
	Submit(ctx context.Context, req build.Request) (*build.Build, error)
	// GetMany reads the build rows among ids without reconciling them, keyed by
	// id — the one read-only lookup a submission list makes to surface each
	// build's outcome to its submitter (the /images/build routes are admin-tier).
	// State advance belongs to the reconcile loop (Sync/SyncAll), so a list render
	// never touches the cluster. An id with no row is absent from the map.
	GetMany(ctx context.Context, ids []string) (map[string]build.Build, error)
	// Sync reconciles a build against its Job and returns the current view, so a
	// GET doubles as the reconcile tick (idempotent on terminal builds).
	Sync(ctx context.Context, id string) (*build.Build, error)
	Cancel(ctx context.Context, id string) (*build.Build, error)
	// ListBuilds pages the build history, newest first, with the match total.
	ListBuilds(ctx context.Context, opts build.ListOpts) ([]build.Build, int, error)
	ListImages(ctx context.Context) ([]build.Image, error)
	AddExternalImage(ctx context.Context, imageRef, addedBy string) (*build.Image, error)
	RemoveImage(ctx context.Context, imageRef string) error
	// ImageAdmitted reports whether a concrete image ref is whitelisted and
	// enabled — the §15 create-server form gate (admission is data-driven, never
	// a free image string from the body).
	ImageAdmitted(ctx context.Context, imageRef string) (bool, error)
	// Scan reads what the build's scan gate kept: the verdict, the listed
	// findings, and the gzipped Trivy report and CycloneDX SBOM. A build with no
	// scan is build.ErrNotFound.
	Scan(ctx context.Context, id string) (*build.Scan, error)
}

// buildImageRequest is the POST /images/build body (spec §16). The push target,
// the uploaded Dockerfile, and the context reference are required; base_image is
// recorded for audit only. The requester identity comes from the Access
// principal, never the body.
type buildImageRequest struct {
	ImageRef   string `json:"image_ref"`
	Dockerfile string `json:"dockerfile"`
	ContextRef string `json:"context_ref"`
	BaseImage  string `json:"base_image,omitempty"`
}

// addImageRequest is the POST /images body for external admission (spec §15).
type addImageRequest struct {
	ImageRef string `json:"image_ref"`
}

// handleBuildImage starts a build (admin-tier). It maps validation failures to
// 400 and everything else to the standard envelope.
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

// handleListBuilds pages the build history (admin-tier), newest first. The
// panel lists from here, so a build started from another browser, or by another
// admin, is still there to follow and to cancel.
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

// handleGetBuild returns a build, reconciling it against its Job first so
// polling drives the scan-gate translation without a separate background loop.
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

// handleBuildLogs (spec §16, §416 日志流复用 §8) streams the build Pod's log to the
// admin as Server-Sent Events. It funnels through relayLogStream — the very same
// §8 read-side relay the server console uses — after a.BuildLogs selects the build
// Job's Pod in the build namespace by build-id label and follows its kaniko
// container. It is admin-tier (adminOnly gates the route); the relay never dials
// RCON and never resolves a secret, it only reads pod logs.
//
// Every error path is resolved BEFORE the relay writes the first SSE byte — once
// the stream headers ship the status code is fixed and no error envelope can
// follow. The buildID becomes a label-selector value (build-id=<id>), so a
// malformed id is rejected up front (400) rather than concatenated into a
// selector, mirroring how handleServerConsole validates the server name before
// touching the streamer.
func (a *API) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Reject anything that is not a valid label value before it reaches the
	// selector: it cannot match a real build Pod (whose build-id label is itself a
	// valid label value) and, left unchecked, a crafted id (e.g. one containing a
	// comma) could inject a second selector requirement. 400 is the honest answer.
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

	// Bound concurrent SSE streams per principal (shared with the console relay): a
	// stalled reader pins this relay and its upstream build-pod follow, so cap how many
	// one principal may hold at once. Acquired before opening the stream and released on
	// every return path. See streamGate — this bounds blast radius, not the leak itself.
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

// handleCancelBuild cancels an in-flight build (admin-tier). A build that has
// already finished is 409.
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

// buildScanView is GET /images/build/{id}/scan: the scan gate's verdict and
// listed findings, and which full documents the build keeps for download.
type buildScanView struct {
	BuildID   string            `json:"build_id"`
	ScannedAt time.Time         `json:"scanned_at"`
	Summary   build.ScanSummary `json:"summary"`
	HasReport bool              `json:"has_report"`
	HasSBOM   bool              `json:"has_sbom"`
}

// handleBuildScan returns the scan a build's scan gate kept (admin-tier). The
// verdict is the gate's own, under the policy recorded with it, so a later
// change to [registry] scan_fail_on never rewrites why an old build failed.
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

// handleBuildScanReport downloads the build's full Trivy JSON report.
func (a *API) handleBuildScanReport(w http.ResponseWriter, r *http.Request) {
	a.serveScanDocument(w, r, "report", func(s *build.Scan) []byte { return s.ReportGz },
		"application/json", "-trivy.json", "image.build.scan.report")
}

// handleBuildSBOM downloads the build's CycloneDX SBOM.
func (a *API) handleBuildSBOM(w http.ResponseWriter, r *http.Request) {
	a.serveScanDocument(w, r, "SBOM", func(s *build.Scan) []byte { return s.SBOMGz },
		"application/vnd.cyclonedx+json", ".cdx.json", "image.build.sbom")
}

// buildScan reads the scan named by the request path. On failure the error
// response is already written.
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

// serveScanDocument sends one gzipped document of a build's scan as a
// download. The bytes are the image's own (package names, file paths), so the
// attachment disposition, with the nosniff every response carries, keeps a
// browser from rendering them.
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

// handleListImages returns the image whitelist (spec §15: the create-server form
// source).
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

// handleAddImage admits an externally-built image (spec §15). It is admin-tier
// and enabled immediately, recorded with added_by for audit.
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

// handleRemoveImage withdraws an image from the whitelist (spec §22). The ref is
// taken from the ?ref= query parameter because image references contain '/' and
// ':' that do not round-trip cleanly through a path segment.
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

// errBuildUnavailable is returned when the build subsystem is not configured on
// this api instance.
var errBuildUnavailable = newError(http.StatusServiceUnavailable, "build_unavailable",
	"image build subsystem is not configured")

// writeBuildError maps build-package errors onto HTTP status codes: a validation
// failure is 400, a missing build/image is 404, an already-terminal build is
// 409, and anything else collapses to a 500 by writeError.
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

// ImagePinner resolves an image ref to the immutable form a server's spec keeps
// (imagepin.Resolver). A ref it does not manage comes back unchanged.
type ImagePinner interface {
	Pin(ctx context.Context, ref string) (string, error)
}

// pinImage pins an admitted ref for a server spec. A tag the registry does not
// hold is the caller's to fix (build or push it first); any other failure is the
// registry being unreachable, and the server is not created or changed without a
// pin, since an unpinned ref is exactly what lets a later push move its world.
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
