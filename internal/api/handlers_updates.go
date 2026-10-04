package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"felis.lolicon.best/internal/updates"
)

// SysAdmin-set maintenance window for the auto-update subsystem (task #38; the
// decision core is internal/updates). The user red line is "不要强制自动更新": Felis
// never force-upgrades. A PolicyScheduled component may only be applied by Felis
// while now falls inside a window a SysAdmin explicitly set here; outside it, the
// same component degrades to notify-only. These two admin routes are how that
// window is read and set.
//
// SINGLE GLOBAL WINDOW (deliberate). internal/updates models the window PER
// Component (Component.Window), but this endpoint stores ONE platform-wide window.
// The task scopes "the SysAdmin-set update Window" as a single maintenance slot,
// and the core's own comment defers recurrence to the caller. The host
// `felis update --apply` reads this window before backup and before installation.
// A future need for per-component windows would layer keys on top; this is the
// platform default.
//
// Felis applies no update on its own. `felis update` reports this window, and
// explicit --apply refuses outside it unless --now starts manual maintenance.
// An unreadable window is always a refusal, including for --now and --force.

// updateWindowKey is the platform_settings key holding the maintenance window as
// JSON {"start","end"} (RFC3339, or null when unset). It reuses the generic
// settings KV rather than a dedicated table: the window is a single small tuple a
// human edits rarely, exactly the shape platform_settings exists for (like
// LocalAuthEnabledKey).
const updateWindowKey = "update_window"

// updateWindow is the wire + storage shape of the maintenance window. Pointer times
// make "unset" (null) distinguishable from a real instant on both decode and
// encode: an absent/null start or end marshals back to JSON null, and the core's
// zero-Window semantics ("unset ⇒ Contains is always false") map onto a stored
// {null,null}. The two ends are an absolute [start,end) interval — the SysAdmin
// picks a concrete next window; recurrence is the runner's concern, not this store's.
type updateWindow struct {
	Start *time.Time `json:"start"`
	End   *time.Time `json:"end"`
}

// handleGetUpdateWindow returns the current maintenance window (admin-tier). An
// unset window — never written, or explicitly cleared to {null,null} — both read
// back as {start:null,end:null}, so the caller has one shape to handle. Only a
// genuinely missing key is treated as unset (ErrNotFound → 200 nulls); any OTHER
// store error is a real failure and 500s rather than masquerading as "no window"
// (a fail-open read on a DB blip would silently drop a scheduled window).
func (a *API) handleGetUpdateWindow(w http.ResponseWriter, r *http.Request) {
	raw, err := a.Repo.GetSetting(r.Context(), updateWindowKey)
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusOK, updateWindow{})
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	var win updateWindow
	if err := json.Unmarshal(raw, &win); err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, win)
}

// handleSetUpdateWindow persists the maintenance window (admin-tier). Validation
// mirrors the decision core's fail-closed Window: a window is either fully set
// (both ends, end strictly after start) or fully cleared (both null) — never
// half-set, never inverted. A half-set body (exactly one end) or an inverted /
// empty interval (end not after start) is a 400, so a malformed schedule can never
// be stored and later fail open. Both-null clears the window to unset.
//
// No "not in the past" check is enforced: a window whose end has passed is harmless
// — the core's Window.Contains returns false once now ≥ end — so a stale window
// simply never re-opens an apply rather than being an error to store.
func (a *API) handleSetUpdateWindow(w http.ResponseWriter, r *http.Request) {
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body updateWindow
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	switch {
	case body.Start == nil && body.End == nil:
		// clearing to unset — fall through to persist {null,null}
	case body.Start == nil || body.End == nil:
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"start and end must both be set or both be null"))
		return
	case !body.End.After(*body.Start):
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"end must be after start"))
		return
	}

	value, err := json.Marshal(body)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.Repo.SetSetting(r.Context(), updateWindowKey, value); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "updates.window_set", "")
	writeJSON(w, http.StatusOK, body)
}

// updateReportView is the wire shape of the newest version check. Report is null
// until felis-update-check.timer (or `felis update --record`) has run once; Stale
// is true for a missing report too, so the panel has one flag for "the versions
// shown are not today's".
type updateReportView struct {
	Report        *updates.StatusReport `json:"report"`
	Stale         bool                  `json:"stale"`
	MaxAgeSeconds int64                 `json:"max_age_seconds"`
}

// handleGetUpdateReport returns the newest recorded version check (admin-tier).
// The check runs on the host, where the installed versions are readable, and
// records itself in platform_settings[updates.StatusKey]. Only a missing key
// reads as "never checked"; any other store error is a 500.
func (a *API) handleGetUpdateReport(w http.ResponseWriter, r *http.Request) {
	view := updateReportView{Stale: true, MaxAgeSeconds: int64(updates.StatusStaleAfter.Seconds())}
	raw, err := a.Repo.GetSetting(r.Context(), updates.StatusKey)
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusOK, view)
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	var rep updates.StatusReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		writeError(w, r, err)
		return
	}
	if rep.Components == nil {
		rep.Components = []updates.ComponentStatus{}
	}
	view.Report = &rep
	view.Stale = a.now().Sub(rep.CheckedAt) > updates.StatusStaleAfter
	writeJSON(w, http.StatusOK, view)
}
