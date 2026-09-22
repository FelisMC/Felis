package api

import (
	"errors"
	"net/http"
	"strings"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
)

// errNoWorldVolume is the shared 409 for backup and restore when the server's
// world PVC does not exist: the Job would only hang Pending on the missing
// claim — invisible to the caller and to the backups list — so the handlers
// refuse up front. Starting the server once (which creates the claim via the
// StatefulSet volumeClaimTemplate) unlocks both ops.
func errNoWorldVolume() error {
	return newError(http.StatusConflict, "no_world_volume",
		"this server has no world volume yet — start it once to create it, then retry")
}

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

// handleRestoreBackup starts restoring a server's world from a backup (spec §7
// POST /servers/{name}/restore-backup; spec §466). It accepts an optional JSON
// body with a backup_id; when absent it restores the latest backup for the server
// (backward-compatible default). The authorization is deliberately stricter than
// ordinary owner-or-admin, in this order:
//
//	① name validation
//	② ServerByName — an unknown server is 404
//	③ owner-or-admin, else 403. A released world's server row is unowned
//	   (owner_id NULL → OwnerID ""), so this also enforces "重新 claim": a former
//	   owner must re-claim the server before they can restore into it.
//	④ if the optional backup_id is supplied the handler resolves the specific
//	   backup; otherwise it picks the most recent present backup, else
//	   404 no_backup
//	⑤ cross-server guard: a backup requested by id must belong to the server in
//	   the path — restoring server A's backup onto server B would be a data leak
//	⑥ former-owner match: a non-admin may restore ONLY a world they formerly
//	   owned. The current-owner gate in ③ is not enough — user B who
//	   re-claims a released server could otherwise resurrect user A's world (the
//	   backup still carries former_owner=A), a data leak. Admin skips this check.
//	⑦ stopped gate: the world PVC must be free, so restore is refused unless the
//	   server is fully stopped.
//	⑧ hand off to the Restorer. Restore is asynchronous (a restore Job, like an
//	   image build Job), so success means "enqueued" and the handler answers 202.
//
// The opaque backup_ref is resolved server-side from the backup and handed to the
// Restorer directly; the client never names a backup by handle (spec §286
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

	// Optional backup_id in the JSON body; absent → LatestBackup (backward compat).
	var body struct {
		BackupID string `json:"backup_id"`
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := decodeJSON(w, r, &body); err != nil {
			writeError(w, r, err)
			return
		}
	}

	// Resolve the backup record. When backup_id is specified the handler resolves
	// that exact backup; otherwise it picks the most recent present one.
	var backup *BackupRecord
	if body.BackupID != "" {
		backup, err = a.Repo.BackupByID(r.Context(), body.BackupID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeError(w, r, newError(http.StatusNotFound, "no_backup",
					"no matching backup exists"))
				return
			}
			writeError(w, r, err)
			return
		}
		// Cross-server guard: the backup must belong to the server named in the path.
		if backup.ServerName != name {
			writeError(w, r, errForbidden)
			return
		}
	} else {
		backup, err = a.Repo.LatestBackup(r.Context(), name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeError(w, r, newError(http.StatusNotFound, "no_backup",
					"no restorable backup exists for this server"))
				return
			}
			writeError(w, r, err)
			return
		}
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

	// World-volume gate: the restore Job mounts the world PVC read-write to unpack
	// the archive into it, so a missing claim means a Pod stuck Pending — a 202
	// "restoring" with nothing ever written. Same refusal as the backup face
	// (shared errNoWorldVolume): the operator starts the server once to create the
	// claim, then restores into it.
	if exists, err := a.Cluster.WorldVolumeExists(r.Context(), name); err != nil {
		writeError(w, r, err)
		return
	} else if !exists {
		writeError(w, r, errNoWorldVolume())
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

// handleBackupNow starts an on-demand backup of a server's world (POST
// /api/v1/servers/{name}/backup; spec §18/§19 WorldArchiver, run on demand). It is
// the "back up before I touch it" lever the break-glass console and the owner both
// reach for. Authorization mirrors handleRestoreBackup's front half — the shared
// "who may act on this server's world" gate — but stops short of restore's backup
// resolution and former-owner-match, because a backup is initiated by the CURRENT
// owner and records their ownership; there is no prior owner's data to leak:
//
//	① name validation
//	② ServerByName — an unknown server is 404
//	③ owner-or-admin, else 403 (an unowned server passes only for admin, so a
//	   released world can still be snapshotted by an operator before disposal)
//	④ stopped gate: the world PVC is RWO and held by a running server, so a backup
//	   Job cannot double-mount it — refuse unless the server is fully stopped. This
//	   also guarantees a quiescent, non-torn archive.
//	⑤ hand off to the Backuper. Backup is asynchronous (a backup Job), so success
//	   means "enqueued" and the handler answers 202.
//
// The former owner recorded on the backup is the server's current OwnerID (empty for
// an unowned server backed up by an admin), so the resulting world_backups row is
// restorable by that owner exactly like an inactivity backup.
func (a *API) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return
	}

	a.enqueueBackup(w, r, name, rec, p.Email, "external")
}

// handleInternalBackup is the internal-face backup trigger. The break-glass console
// (root on the node, holding the service token) POSTs here to snapshot a stopped
// world while felis-api is alive — it goes through the API rather than direct-to-CRD
// like halt does, because rendering the backup Job needs deployment coordinates
// (FELIS_IMAGE, FELIS_BACKUP_PVC) that only felis-api holds.
//
// There is no Principal: the service token is a trusted machine caller (auth.go), so
// the requireInternal middleware IS the authorization — the operator already has root
// on the node. It audits the action to "break-glass" so a console-initiated backup is
// distinguishable from an owner's self-service one.
//
// The console passes the OS user at the keyboard in an optional {"os_user":"..."} body,
// which becomes the audit actor (parity with the halt peer's accountability). The body
// is decoded whenever one is present — not gated on Content-Type — so a console that
// forgets the header still records the operator rather than silently attributing to the
// generic "break-glass". Absent/blank falls back to "break-glass".
func (a *API) handleInternalBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	actor := "break-glass"
	if r.ContentLength != 0 {
		var body struct {
			OSUser string `json:"os_user"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			writeError(w, r, err)
			return
		}
		if u := strings.TrimSpace(body.OSUser); u != "" {
			actor = u
		}
	}

	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}

	a.enqueueBackup(w, r, name, rec, actor, "internal")
}

// enqueueBackup is the shared tail of both backup faces: the RWO stopped-gate, the
// optional-Backuper 503, the async hand-off, and the audit + 202. Both faces validate
// the name and resolve rec themselves and differ only in how the caller is authorized
// (Principal vs trusted service token) and the audit actor/source — keeping the
// security-critical stopped-gate single-sourced so the two faces cannot diverge.
func (a *API) enqueueBackup(w http.ResponseWriter, r *http.Request, name string, rec *ServerRecord, actor, source string) {
	// Stopped gate: the world PVC is RWO and held by a running server, so a backup
	// Job cannot double-mount it (mirrors the restore gate). Ready means it is up;
	// any desiredState other than Stopped means it owns the RWO volume.
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if info.Ready || info.DesiredState != string(v1alpha1.DesiredStopped) {
		writeError(w, r, newError(http.StatusConflict, "not_stopped",
			"stop the server before backing up its world"))
		return
	}

	// World-volume gate: the Job mounts the world PVC by claim name, and a missing
	// claim would leave its Pod Pending — a 202 "backing_up" with nothing ever
	// recorded anywhere. A never-started or already-reaped server is refused with
	// the same specificity as the stopped gate.
	if exists, err := a.Cluster.WorldVolumeExists(r.Context(), name); err != nil {
		writeError(w, r, err)
		return
	} else if !exists {
		writeError(w, r, errNoWorldVolume())
		return
	}

	// Backuper is optional: when unwired the endpoint reports 503 rather than
	// panicking, so the authorization boundary above is exercised even before the
	// backup-Job executor is wired (see Backuper).
	if a.Backuper == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "backup_unavailable",
			"backup subsystem is not configured"))
		return
	}

	if err := a.Backuper.Backup(r.Context(), name, rec.OwnerID); err != nil {
		a.writeLookupError(w, r, err)
		return
	}

	_ = a.Repo.Audit(r.Context(), AuditEntry{
		Actor: actor, Source: source, Action: "backup.create",
		ServerName: name, RequestID: requestIDFromContext(r.Context()),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"name":   name,
		"status": "backing_up",
	})
}
