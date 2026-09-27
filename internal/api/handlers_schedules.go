package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"felis.lolicon.best/internal/naming"
)

// scheduleRunTimeout bounds the first step of a run somebody asked for. The
// step outlives the request, so a client that hangs up cannot cut a stop off
// halfway.
const scheduleRunTimeout = 30 * time.Second

// scheduleServer resolves {name} for the schedule routes and applies their
// gate: 400 for a malformed name, 404 for a server that does not exist, 403
// for a caller who neither owns it nor is an admin, 503 without a store.
func (a *API) scheduleServer(w http.ResponseWriter, r *http.Request) (*ServerRecord, bool) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return nil, false
	}
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return nil, false
	}
	if !a.isOwnerOrAdmin(principalFromContext(r.Context()), rec) {
		writeError(w, r, errForbidden)
		return nil, false
	}
	if a.Schedules == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "schedules_unavailable",
			"scheduled tasks are not configured"))
		return nil, false
	}
	return rec, true
}

// scheduleID parses {id}.
func scheduleID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, r, newError(http.StatusBadRequest, "bad_id", "invalid schedule id"))
		return 0, false
	}
	return id, true
}

// writeScheduleError maps the store's schedule errors.
func (a *API) writeScheduleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrScheduleRunning):
		writeError(w, r, newError(http.StatusConflict, "schedule_running",
			"this schedule is running; try again once the run finishes"))
	case errors.Is(err, ErrScheduleLimit):
		writeError(w, r, newError(http.StatusConflict, "schedule_limit",
			"a server can have at most %d scheduled tasks", maxSchedulesPerServer))
	default:
		a.writeLookupError(w, r, err)
	}
}

// auditSchedule records a change to a schedule by the signed-in caller.
func (a *API) auditSchedule(r *http.Request, action string, s *Schedule) {
	p := principalFromContext(r.Context())
	e := AuditEntry{Actor: auditActor(p), Action: action, ServerName: s.Server,
		Payload: auditPayload(map[string]any{"schedule": s.ID, "action": s.Action})}
	if p != nil {
		e.ActorUserID = p.UserID
	}
	a.auditEntry(r, e)
}

// handleListSchedules serves GET /servers/{name}/schedules.
func (a *API) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.scheduleServer(w, r)
	if !ok {
		return
	}
	list, err := a.Schedules.ListSchedules(r.Context(), rec.Name)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if list == nil {
		list = []Schedule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": rec.Name, "schedules": list, "limit": maxSchedulesPerServer})
}

// handleCreateSchedule serves POST /servers/{name}/schedules. The schedule
// belongs to the server's current owner (see Schedule.OwnerID).
func (a *API) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.scheduleServer(w, r)
	if !ok {
		return
	}
	var in scheduleInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, r, err)
		return
	}
	s := &Schedule{Server: rec.Name, OwnerID: rec.OwnerID, CreatedBy: auditActor(principalFromContext(r.Context()))}
	if err := in.apply(s); err != nil {
		writeError(w, r, err)
		return
	}
	s.NextRunAt = a.firstRun(s)
	if err := a.Schedules.CreateSchedule(r.Context(), s, maxSchedulesPerServer); err != nil {
		a.writeScheduleError(w, r, err)
		return
	}
	a.auditSchedule(r, "schedule.create", s)
	writeJSON(w, http.StatusCreated, s)
}

// handleUpdateSchedule serves PUT /servers/{name}/schedules/{id}: new settings,
// and the schedule passes to the server's current owner.
func (a *API) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.scheduleServer(w, r)
	if !ok {
		return
	}
	id, ok := scheduleID(w, r)
	if !ok {
		return
	}
	var in scheduleInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, r, err)
		return
	}
	s, err := a.Schedules.GetSchedule(r.Context(), rec.Name, id)
	if err != nil {
		a.writeScheduleError(w, r, err)
		return
	}
	if err := in.apply(s); err != nil {
		writeError(w, r, err)
		return
	}
	s.OwnerID, s.NextRunAt = rec.OwnerID, a.firstRun(s)
	if err := a.Schedules.UpdateSchedule(r.Context(), s); err != nil {
		a.writeScheduleError(w, r, err)
		return
	}
	a.auditSchedule(r, "schedule.update", s)
	writeJSON(w, http.StatusOK, s)
}

// handleDeleteSchedule serves DELETE /servers/{name}/schedules/{id}.
func (a *API) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.scheduleServer(w, r)
	if !ok {
		return
	}
	id, ok := scheduleID(w, r)
	if !ok {
		return
	}
	s, err := a.Schedules.GetSchedule(r.Context(), rec.Name, id)
	if err != nil {
		a.writeScheduleError(w, r, err)
		return
	}
	if err := a.Schedules.DeleteSchedule(r.Context(), rec.Name, id); err != nil {
		a.writeScheduleError(w, r, err)
		return
	}
	a.auditSchedule(r, "schedule.delete", s)
	w.WriteHeader(http.StatusNoContent)
}

// handleRunSchedule serves POST /servers/{name}/schedules/{id}/run: the run
// starts now, without the players' warning, and the next scheduled run stays
// where it is. The answer is the schedule after the run's first step; a restart
// or backup goes on in the background.
func (a *API) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.scheduleServer(w, r)
	if !ok {
		return
	}
	id, ok := scheduleID(w, r)
	if !ok {
		return
	}
	s, err := a.Schedules.GetSchedule(r.Context(), rec.Name, id)
	if err != nil {
		a.writeScheduleError(w, r, err)
		return
	}
	if s.OwnerID != rec.OwnerID {
		writeError(w, r, newError(http.StatusConflict, "schedule_stale",
			"the server has a new owner since this schedule was saved; save it again first"))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), scheduleRunTimeout)
	defer cancel()
	now := a.now()
	claimed, err := a.Schedules.ClaimScheduleRun(ctx, id, nil, nil, now)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !claimed {
		a.writeScheduleError(w, r, ErrScheduleRunning)
		return
	}
	a.auditSchedule(r, "schedule.run_now", s)
	if err := a.beginRun(ctx, s); err != nil {
		writeError(w, r, err)
		return
	}
	after, err := a.Schedules.GetSchedule(ctx, rec.Name, id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, after)
}

// firstRun is the next run of a schedule just saved, nil while it is disabled.
func (a *API) firstRun(s *Schedule) *time.Time {
	if !s.Enabled {
		return nil
	}
	return ptrTime(s.nextRun(a.now()))
}
