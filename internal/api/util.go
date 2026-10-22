package api

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
)

const maxBodyBytes = 1 << 20

// requireJSONContentType blocks cross-origin form submissions to credential-bearing endpoints; media-type parameters are allowed.
func requireJSONContentType(r *http.Request) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mt, "application/json") {
		return newError(http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
	}
	return nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return decodeJSONLimit(w, r, v, maxBodyBytes)
}

func decodeJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return newError(http.StatusRequestEntityTooLarge, "too_large",
				"request body is larger than %d bytes", tooBig.Limit)
		}
		return newError(http.StatusBadRequest, "bad_request", "invalid request body: %v", err)
	}
	if dec.More() {
		return newError(http.StatusBadRequest, "bad_request", "unexpected trailing data in body")
	}
	return nil
}
