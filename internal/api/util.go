package api

import (
	"encoding/json"
	"net/http"
)

// maxBodyBytes caps request bodies; the API only accepts small JSON documents.
const maxBodyBytes = 1 << 20 // 1 MiB

// decodeJSON strictly decodes a small request body into v, rejecting unknown
// fields and trailing data so malformed callers fail fast with 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return newError(http.StatusBadRequest, "bad_request", "invalid request body: %v", err)
	}
	if dec.More() {
		return newError(http.StatusBadRequest, "bad_request", "unexpected trailing data in body")
	}
	return nil
}
