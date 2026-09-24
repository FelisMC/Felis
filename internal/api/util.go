package api

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
)

// maxBodyBytes caps request bodies; the API only accepts small JSON documents.
const maxBodyBytes = 1 << 20 // 1 MiB

// requireJSONContentType rejects a request whose body is not declared
// application/json, returning 415 before any decode. It guards the credential-bearing
// auth writes (email-OTP, passkey, op-login, setup redeem) against a cross-site
// forgery: an HTML form can
// only POST as application/x-www-form-urlencoded, multipart/form-data, or text/plain
// — never JSON — and a cross-site fetch that forces application/json triggers a CORS
// preflight this API never answers, so neither form can be forged off-origin. The
// session cookie's SameSite=Lax already blocks the bearing of credentials cross-site;
// this is the belt to that suspenders, and it costs a legitimate same-origin caller
// nothing (the panel always sends application/json on a bodied request). Media-type
// parameters (e.g. "; charset=utf-8") are ignored — only the type/subtype must match.
func requireJSONContentType(r *http.Request) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mt, "application/json") {
		return newError(http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
	}
	return nil
}

// decodeJSON strictly decodes a small request body into v, rejecting unknown
// fields and trailing data so malformed callers fail fast with 400, and a body
// past maxBodyBytes with 413.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
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
