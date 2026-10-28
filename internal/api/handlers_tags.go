package api

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"felis.lolicon.best/internal/naming"
)

const (
	maxServerTags      = 6
	maxServerTagLength = 20
)

type serverTagsRequest struct {
	Tags []string `json:"tags"`
}

type serverTagsView struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// cleanServerTags trims, rejects invalid tags and drops case-insensitive repeats, keeping the first spelling.
func cleanServerTags(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		tag := strings.TrimSpace(raw)
		if n := utf8.RuneCountInString(tag); n < 1 || n > maxServerTagLength {
			return nil, newError(http.StatusBadRequest, "bad_tags",
				"a tag must have between 1 and %d characters", maxServerTagLength)
		}
		if strings.IndexFunc(tag, func(r rune) bool { return r == ',' || !unicode.IsGraphic(r) }) >= 0 {
			return nil, newError(http.StatusBadRequest, "bad_tags",
				"a tag must not contain commas or control characters")
		}
		key := strings.ToLower(tag)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, tag)
	}
	if len(out) > maxServerTags {
		return nil, newError(http.StatusBadRequest, "bad_tags", "a server takes at most %d tags", maxServerTags)
	}
	return out, nil
}

// handleSetServerTags refuses system services, which have no row to hold tags.
func (a *API) handleSetServerTags(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	rec, err := a.managedServerRecord(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return
	}
	if naming.IsSystemServer(name) {
		writeError(w, r, newError(http.StatusConflict, "system_server", "a system server cannot be tagged"))
		return
	}

	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body serverTagsRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Tags == nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "tags is required; send an empty list to clear them"))
		return
	}
	tags, err := cleanServerTags(body.Tags)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.Repo.SetServerTags(r.Context(), name, tags); err != nil {
		a.writeLookupError(w, r, err)
		return
	}

	a.auditEntry(r, AuditEntry{Actor: auditActor(p), ActorUserID: p.UserID, Action: "server.tags", ServerName: name,
		Payload: auditPayload(map[string]any{"tags": tags})})
	writeJSON(w, http.StatusOK, serverTagsView{Name: name, Tags: tags})
}
