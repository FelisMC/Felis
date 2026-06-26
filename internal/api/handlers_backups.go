package api

import (
	"errors"
	"net/http"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
)

// handleListBackups lists the world backups visible to the caller (spec §7 GET
// /api/v1/backups; world_backups in §22). It is app-tier: an admin sees every
// present backup; a regular user sees only the backups of worlds they formerly
// owned. The scope is decided here from the Principal and enforced by which Repo
// query runs (AllBackups vs BackupsForUser) — there is no client-supplied filter
// a user could widen, so "a user cannot see another's backups" is a property of
// the query, not of request parsing.
func (a *API) handleListBackups(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())

	var (
		backups []BackupView
		err     error
	)
	if p.IsAdmin() {
		backups, err = a.Repo.AllBackups(r.Context())
	} else {
		backups, err = a.Repo.BackupsForUser(r.Context(), p.UserID)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	if backups == nil {
		backups = []BackupView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": backups})
}

// handleRestoreBackup starts restoring a server's world from its most recent
// backup (spec §7 POST /servers/{name}/restore-backup; spec §466: former_owner
// 3mo 内重新 claim → restore PVC). The authorization is deliberately stricter than
// ordinary owner-or-admin, in this order:
//
//	① name validation
//	② ServerByName — an unknown server is 404
//	③ owner-or-admin, else 403. A released world's server row is unowned
//	   (owner_id NULL → OwnerID ""), so this also enforces "重新 claim": a former
//	   owner must re-claim the server before they can restore into it.
//	④ the latest present backup, else 404 no_backup
//	⑤ former-owner match: a non-admin may restore ONLY a world they formerly owned.
//	   The current-owner gate in ③ is not enough — user B who re-claims a released
//	   server could otherwise resurrect user A's world (the backup still carries
//	   former_owner=A), a data leak. Admin skips this check.
//	⑥ stopped gate: the world PVC must be free, so restore is refused unless the
//	   server is fully stopped. A running OR starting server still holds the RWO
//	   world volume, which a restore Job could not mount — a clean 409 beats a Job
//	   that fails to schedule.
//	⑦ hand off to the Restorer. Restore is asynchronous (a restore Job, like an
//	   image build Job), so success means "enqueued" and the handler answers 202.
//
// The opaque backup_ref is resolved server-side from the latest backup and handed
// to the Restorer directly; the client never names a backup by handle (spec §286
// principle).
func (a *API) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	// Ownership: owner or admin, mirroring handleStop. An unknown server is 404; an
	// unowned (released) server fails the owner check for everyone but admin, which
	// is exactly the "must re-claim first" rule.
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return
	}

	backup, err := a.Repo.LatestBackup(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "no_backup",
				"no restorable backup exists for this server"))
			return
		}
		writeError(w, r, err)
		return
	}

	// A non-admin may restore only a world they formerly owned (spec §466). Without
	// this a fresh claimant of a released server could resurrect the previous
	// owner's world data.
	if !p.IsAdmin() && backup.FormerOwner != p.UserID {
		writeError(w, r, errForbidden)
		return
	}

	// Stopped gate: the world PVC must be free for the restore to write into it.
	// Refuse unless the server is fully stopped — Ready means it is up, and any
	// desiredState other than Stopped means it is up or coming up and still owns the
	// RWO volume (spec §141 readiness is an RCON probe; DesiredStopped is the
	// intent). This yields a specific 409 instead of a restore Job that cannot mount.
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if info.Ready || info.DesiredState != string(v1alpha1.DesiredStopped) {
		writeError(w, r, newError(http.StatusConflict, "not_stopped",
			"stop the server before restoring a backup"))
		return
	}

	// Restorer is optional: when unwired the endpoint reports 503 rather than
	// panicking, so the authorization boundary above is exercised even before the
	// restore-Job executor is wired (see Restorer).
	if a.Restorer == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "restore_unavailable",
			"restore subsystem is not configured"))
		return
	}

	if err := a.Restorer.Restore(r.Context(), name, backup.BackupRef); err != nil {
		// ErrNotFound (server vanished from the execution backend) → 404; else 500.
		a.writeLookupError(w, r, err)
		return
	}

	a.audit(r, p.Email, "backup.restore", name)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"name":      name,
		"status":    "restoring",
		"backup_id": backup.ID,
	})
}
