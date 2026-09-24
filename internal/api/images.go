package api

import (
	"context"
	"errors"
	"net/http"

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
	// Get reads one build row without reconciling it — the read-only lookup the
	// submission views use to surface a build's outcome to its submitter (the
	// /images/build routes are admin-tier). State advance belongs to the
	// reconcile loop (Sync/SyncAll), so a list render never touches the cluster.
	Get(ctx context.Context, id string) (*build.Build, error)
	// Sync reconciles a build against its Job and returns the current view, so a
	// GET doubles as the reconcile tick (idempotent on terminal builds).
	Sync(ctx context.Context, id string) (*build.Build, error)
	Cancel(ctx context.Context, id string) (*build.Build, error)
	ListImages(ctx context.Context) ([]build.Image, error)
	AddExternalImage(ctx context.Context, imageRef, addedBy string) (*build.Image, error)
	RemoveImage(ctx context.Context, imageRef string) error
	// ImageAdmitted reports whether a concrete image ref is whitelisted and
	// enabled — the §15 create-server form gate (admission is data-driven, never
	// a free image string from the body).
	ImageAdmitted(ctx context.Context, imageRef string) (bool, error)
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

	src, err := a.BuildLogs.StreamLogs(r.Context(), id)
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
	relayLogStream(w, r, src)
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
