package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/maintenance"
)

type paperGlobalPatchRequest struct {
	Values       map[string]any `json:"values"`
	ExpectSHA256 string         `json:"expect_sha256"`
}

func (a *API) handleReadPaperGlobal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	config, err := a.Files.ReadPaperGlobal(r.Context(), name)
	if err != nil {
		writePaperGlobalError(w, r, err)
		return
	}
	writePaperGlobalConfig(w, config)
}

func (a *API) handlePatchPaperGlobal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name, ok := a.authorizeFileOp(w, r)
	if !ok {
		return
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, fileedit.MaxPaperGlobalPatchBytes+1))
	if err != nil {
		writeError(w, r, errBadRequest)
		return
	}
	if len(content) > fileedit.MaxPaperGlobalPatchBytes {
		writeError(w, r, newError(http.StatusRequestEntityTooLarge, "too_large",
			"the protected configuration patch must fit within 64 KiB"))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(content))
	var body paperGlobalPatchRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "invalid protected configuration patch"))
		return
	}
	if body.Values == nil || !sha256Hex.MatchString(body.ExpectSHA256) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"values and the 64-digit lowercase expect_sha256 returned by a read are required"))
		return
	}
	if err := fileedit.ValidatePaperGlobalPatch(body.Values); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"values must contain only supported paper-global fields with valid scalar values"))
		return
	}
	release, ok := a.acquireFileWorld(w, r, name, maintenance.KindFileWrite,
		"wait for the server to stop or finish starting before editing its configuration")
	if !ok {
		return
	}
	defer release()

	config, err := a.Files.PatchPaperGlobal(r.Context(), name, body.Values, body.ExpectSHA256)
	if err != nil {
		writePaperGlobalError(w, r, err)
		return
	}
	a.auditFile(r, "config.paper-global.patch", name, "config/paper-global.yml", nil)
	writePaperGlobalConfig(w, config)
}

func writePaperGlobalConfig(w http.ResponseWriter, config fileedit.PaperGlobalConfig) {
	if config.Values == nil {
		config.Values = map[string]any{}
	}
	writeJSON(w, http.StatusOK, config)
}

// YAML errors may echo stored values, so only sentinel errors reach the client.
func writePaperGlobalError(w http.ResponseWriter, r *http.Request, err error) {
	for _, safe := range []error{
		fileedit.ErrNotFound, fileedit.ErrBadPath, fileedit.ErrTooLarge,
		fileedit.ErrConflict, fileedit.ErrNoSpace, fileedit.ErrDigestMismatch,
		fileedit.ErrReadDamaged, context.DeadlineExceeded,
	} {
		if errors.Is(err, safe) {
			writeFileEditError(w, r, safe)
			return
		}
	}
	writeError(w, r, newError(http.StatusInternalServerError, "internal",
		"the protected configuration could not be read or saved"))
}
