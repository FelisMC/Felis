package api

import (
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	defaultAuditListLimit = 50
	maxAuditListLimit     = 200
)

type auditPageView struct {
	Entries    []AuditRecord `json:"entries"`
	NextCursor string        `json:"next_cursor"`
}

func (a *API) handleListAudit(w http.ResponseWriter, r *http.Request) {
	q, err := parseAuditQuery(r.URL.Query())
	if err != nil {
		writeError(w, r, err)
		return
	}
	size := q.Limit
	q.Limit = size + 1 // the extra row shows whether an older page exists
	rows, err := a.Repo.ListAudit(r.Context(), q)
	if err != nil {
		writeError(w, r, err)
		return
	}
	next := ""
	if len(rows) > size {
		rows = rows[:size]
		next = strconv.FormatInt(rows[size-1].ID, 10)
	}
	if rows == nil {
		rows = []AuditRecord{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, auditPageView{Entries: rows, NextCursor: next})
}

func parseAuditQuery(v url.Values) (AuditQuery, error) {
	q := AuditQuery{
		Actor:   v.Get("actor"),
		Action:  v.Get("action"),
		Server:  v.Get("server"),
		Keyword: v.Get("q"),
		Limit:   defaultAuditListLimit,
	}
	var err error
	if q.Since, err = auditTimeParam(v, "since"); err != nil {
		return q, err
	}
	if q.Until, err = auditTimeParam(v, "until"); err != nil {
		return q, err
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && !q.Until.After(q.Since) {
		return q, newError(http.StatusBadRequest, "bad_request", "until must come after since")
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxAuditListLimit {
			return q, newError(http.StatusBadRequest, "bad_request", "limit must be between 1 and %d", maxAuditListLimit)
		}
		q.Limit = n
	}
	if s := v.Get("cursor"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id < 1 {
			return q, newError(http.StatusBadRequest, "bad_request", "cursor is not valid")
		}
		q.BeforeID = id
	}
	return q, nil
}

func auditTimeParam(v url.Values, key string) (time.Time, error) {
	s := v.Get(key)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, newError(http.StatusBadRequest, "bad_request", "%s must be an RFC 3339 time", key)
	}
	return t, nil
}
