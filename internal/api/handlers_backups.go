package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
)

// errNoWorldVolume rejects missing PVCs before a backup or restore Job would hang Pending.
func errNoWorldVolume() error {
	return newError(http.StatusConflict, "no_world_volume",
		"this server has no world volume yet — start it once to create it, then retry")
}

// handleListBackups scopes non-staff queries to former ownership; request filters can only narrow that scope.
func (a *API) handleListBackups(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	q := r.URL.Query()
	opts := BackupListOpts{Server: q.Get("server")}
	// Reserved system-server names remain valid backup-list filters for staff.
	if opts.Server != "" {
		if err := naming.ValidateSystemServerName(opts.Server); err != nil {
			writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
			return
		}
	}
	opts.Limit, _ = strconv.Atoi(q.Get("limit"))
	opts.Offset, _ = strconv.Atoi(q.Get("offset"))
	if opts.Limit <= 0 {
		opts.Limit = DefaultBackupListLimit
	}
	opts.Limit = min(opts.Limit, MaxBackupListLimit)
	opts.Offset = max(opts.Offset, 0)

	var (
		backups []BackupView
		total   int
		err     error
	)
	switch {
	case p.IsOwner():
		backups, total, err = a.Repo.AllBackups(r.Context(), opts)
	case p.IsAdmin():
		backups, total, err = a.Repo.BackupsForAdmin(r.Context(), p.UserID, opts)
	default:
		backups, total, err = a.Repo.BackupsForUser(r.Context(), p.UserID, opts)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	if backups == nil {
		backups = []BackupView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": backups, "total": total})
}

func backupAccessible(p *Principal, rec *ServerRecord, collabRole string, backup *BackupRecord) bool {
	if p.IsAdmin() {
		return true
	}
	if p.UserID != "" && backup.FormerOwner == p.UserID {
		return true
	}
	return collabRole == CollabRoleManager && rec != nil && (backup.FormerOwner == "" || backup.FormerOwner == rec.OwnerID)
}

// handleDeleteBackup expires the row immediately; running restores block deletion, and retention later removes the archive.
func (a *API) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	backup, err := a.Repo.BackupByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeError(w, r, newError(http.StatusNotFound, "no_backup", "no matching backup exists"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	rec, recErr := a.Repo.ServerByName(r.Context(), backup.ServerName)
	if recErr != nil && !errors.Is(recErr, ErrNotFound) {
		writeError(w, r, recErr)
		return
	}
	if recErr == nil {
		allowed, collabRole, cerr := a.checkServerAccess(r.Context(), p, backup.ServerName, rec, ActionManage)
		if cerr != nil {
			writeError(w, r, cerr)
			return
		}
		if !allowed {
			if p.IsAdmin() {
				writeError(w, r, errForbidden)
				return
			}
			writeError(w, r, newError(http.StatusNotFound, "no_backup", "no matching backup exists"))
			return
		}
		if !backupAccessible(p, rec, collabRole, backup) {
			writeError(w, r, newError(http.StatusNotFound, "no_backup", "no matching backup exists"))
			return
		}
	} else if !p.IsAdmin() && (p.UserID == "" || backup.FormerOwner != p.UserID) {
		writeError(w, r, newError(http.StatusNotFound, "no_backup", "no matching backup exists"))
		return
	}
	if a.JobStatus != nil {
		jobs, err := a.JobStatus.LatestJobs(r.Context(), backup.ServerName)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if restoreMayRead(jobs, backup.ID) {
			writeError(w, r, newError(http.StatusConflict, "restore_in_progress",
				"a restore is running on this backup's server and may be reading it; delete it once the restore finishes"))
			return
		}
	}
	if err := a.Repo.ExpireBackup(r.Context(), backup.ID, a.now()); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, r, newError(http.StatusNotFound, "no_backup", "no matching backup exists"))
			return
		}
		writeError(w, r, err)
		return
	}
	e := AuditEntry{Actor: auditActor(p), ActorUserID: p.UserID, Action: "backup.delete", ServerName: backup.ServerName}
	e.Payload = auditPayload(map[string]any{"backup_id": backup.ID, "former_owner": backup.FormerOwner, "size_bytes": backup.SizeBytes})
	a.auditEntry(r, e)
	writeJSON(w, http.StatusOK, map[string]any{"id": backup.ID, "status": "expired"})
}

// restoreMayRead includes unfinished restores and safety snapshots awaiting restoration of this backup.
func restoreMayRead(jobs []AsyncJob, id string) bool {
	for _, j := range jobs {
		if j.Kind == "restore" && j.State == "running" {
			return true
		}
		if j.ThenRestore == maintenance.ThenRestorePending && j.RestoreBackupID == id {
			return true
		}
	}
	return false
}

// handleRestoreBackup defaults to the latest backup and a safety snapshot; clients never supply storage references.
func (a *API) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	rec, collabRole, ok := a.authorizeServer(w, r, ActionManage)
	if !ok {
		return
	}
	name := rec.Name
	if name == naming.SystemLoginServer && !p.IsOwner() {
		writeError(w, r, errForbidden)
		return
	}
	*r = *r.WithContext(withAuthorizedServer(r.Context(), rec))

	var body struct {
		BackupID       string `json:"backup_id"`
		SafetySnapshot *bool  `json:"safety_snapshot"`
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := decodeJSON(w, r, &body); err != nil {
			writeError(w, r, err)
			return
		}
	}

	var (
		backup *BackupRecord
		err    error
	)
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
		if backup.ServerName != name {
			writeError(w, r, errForbidden)
			return
		}
		// Reject known-corrupt archives before acquiring the world-volume lock.
		if backup.Corrupt {
			writeError(w, r, newError(http.StatusConflict, "backup_corrupt",
				"this backup did not read back intact and cannot be restored; pick another"))
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

	// A new claimant must not gain access to a previous owner's backup.
	if !backupAccessible(p, rec, collabRole, backup) {
		writeError(w, r, errForbidden)
		return
	}
	if _, ok := a.recheckAuthorizedServer(w, r, name); !ok {
		return
	}

	// The Job can mount beside a live server on the same node; acquireWorld makes the stopped check atomic.
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

	if exists, err := a.Cluster.WorldVolumeExists(r.Context(), name); err != nil {
		writeError(w, r, err)
		return
	} else if !exists {
		writeError(w, r, errNoWorldVolume())
		return
	}

	if a.Restorer == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "restore_unavailable",
			"restore subsystem is not configured"))
		return
	}

	snapshotter, snapshot := a.snapshotFirst()
	if body.SafetySnapshot != nil && !*body.SafetySnapshot {
		snapshot = false
	}
	if !p.IsAdmin() {
		if err := a.backupCooldownAllowance(r.Context(), name); err != nil {
			writeError(w, r, err)
			return
		}
		if snapshot {
			if err := a.backupStoreAllowance(r.Context(), rec.OwnerID); err != nil {
				writeError(w, r, err)
				return
			}
		}
	}

	// Hold the world-volume lock until restoration finishes to prevent startup on a partially extracted world.
	release, ok := a.acquireWorld(w, r, name, maintenance.KindRestore, "stop the server before restoring a backup")
	if !ok {
		return
	}
	cur, ok := a.recheckAuthorizedServer(w, r, name)
	if !ok {
		release()
		return
	}
	defer release()

	if snapshot {
		err = snapshotter.BackupThenRestore(r.Context(), name, cur.OwnerID, backup.ID, backup.BackupRef, backup.SHA256)
	} else {
		err = a.Restorer.Restore(r.Context(), name, backup.BackupRef, backup.SHA256)
	}
	if err != nil {
		if isRestoreInProgress(err) {
			writeError(w, r, newError(http.StatusConflict, "restore_in_progress",
				"a restore of another backup is still running on this server's world; retry once it finishes"))
			return
		}
		a.writeLookupError(w, r, err)
		return
	}

	a.audit(r, "backup.restore", name)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"name":            name,
		"status":          "restoring",
		"backup_id":       backup.ID,
		"safety_snapshot": snapshot,
	})
}

func (a *API) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	rec, _, ok := a.authorizeServer(w, r, ActionOperate)
	if !ok {
		return
	}
	name := rec.Name
	if !p.IsAdmin() {
		if err := a.backupAllowance(r.Context(), name, rec.OwnerID); err != nil {
			writeError(w, r, err)
			return
		}
	}

	a.enqueueBackup(w, r, name, rec, auditActor(p), "external")
}

// backupAllowance bounds manual backups by per-server cooldown and the owner's total stored backup size.
func (a *API) backupAllowance(ctx context.Context, name, ownerID string) error {
	if err := a.backupCooldownAllowance(ctx, name); err != nil {
		return err
	}
	return a.backupStoreAllowance(ctx, ownerID)
}

func (a *API) backupCooldownAllowance(ctx context.Context, name string) error {
	if cooldown := a.backupCooldown(ctx); cooldown > 0 {
		now := a.now()
		last, err := a.Repo.LastBackupRequest(ctx, name, now.Add(-cooldown))
		if err != nil {
			return err
		}
		if !last.IsZero() {
			wait := last.Add(cooldown).Sub(now)
			if wait > 0 {
				return newError(http.StatusTooManyRequests, "backup_cooldown",
					"a backup of this server was started %s ago; the next one can start in %s",
					now.Sub(last).Round(time.Second), wait.Round(time.Second)).retryAfter(wait)
			}
		}
	}
	return nil
}

func (a *API) backupStoreAllowance(ctx context.Context, ownerID string) error {
	limit := a.backupStoreCap(ctx)
	if limit <= 0 {
		return nil
	}
	if ownerID != "" {
		ownerUsed, owners, err := a.Repo.OwnerBackupStoreBytes(ctx, ownerID)
		if err != nil {
			return err
		}
		share := max(limit/int64(max(owners, 1)), 1)
		if ownerUsed >= share {
			return newError(http.StatusInsufficientStorage, "backup_store_full",
				"your share of the backup store is full; delete an old backup or ask an administrator to free space")
		}
	}
	used, err := a.Repo.BackupStoreBytes(ctx)
	if err != nil {
		return err
	}
	if used >= limit {
		return newError(http.StatusInsufficientStorage, "backup_store_full",
			"the backup store is full; ask an administrator to free space")
	}
	return nil
}

// handleInternalBackup uses service-token authorization and attributes auditing to os_user or break-glass.
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

	a.enqueueBackup(w, r, name, rec, actor, internalSource(r))
}

// enqueueBackup shares volume gating and asynchronous handoff between user and internal backup routes.
func (a *API) enqueueBackup(w http.ResponseWriter, r *http.Request, name string, rec *ServerRecord, actor, source string) {
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if info.hibernated() && info.Phase != string(v1alpha1.PhaseHibernated) {
		writeError(w, r, newError(http.StatusConflict, "not_stopped",
			"the server is hibernating or waking; back it up once it is hibernated or running"))
		return
	}
	if !info.hibernated() && (info.Ready || info.DesiredState != string(v1alpha1.DesiredStopped)) {
		// Use live backup only for running servers; transitional states cannot provide a consistent world.
		if !info.Ready || info.DesiredState != string(v1alpha1.DesiredRunning) || a.Backuper == nil {
			writeError(w, r, newError(http.StatusConflict, "not_stopped",
				"the server is starting or stopping; back it up once it is running or stopped"))
			return
		}
		if err := a.startLiveBackup(r.Context(), name, rec.OwnerID, ""); err != nil {
			if errors.Is(err, ErrNotRunning) {
				err = newError(http.StatusConflict, "not_stopped", "the server is starting or stopping; back it up once it is running or stopped")
			}
			a.writeLookupError(w, r, maintenanceError(err, "stop the server before backing up its world"))
			return
		}
		e := AuditEntry{Actor: actor, Source: source, Action: "backup.create", ServerName: name, Payload: []byte(`{"live":true}`)}
		if p := principalFromContext(r.Context()); p != nil {
			e.ActorUserID = p.UserID
		}
		a.auditEntry(r, e)
		writeJSON(w, http.StatusAccepted, map[string]any{"name": name, "status": "backing_up"})
		return
	}

	if exists, err := a.Cluster.WorldVolumeExists(r.Context(), name); err != nil {
		writeError(w, r, err)
		return
	} else if !exists {
		writeError(w, r, errNoWorldVolume())
		return
	}

	if a.Backuper == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "backup_unavailable",
			"backup subsystem is not configured"))
		return
	}

	// Hold the world-volume lock to prevent startup during archival.
	release, ok := a.acquireWorld(w, r, name, maintenance.KindBackup, "stop the server before backing up its world")
	if !ok {
		return
	}
	defer release()

	if err := a.Backuper.Backup(r.Context(), name, rec.OwnerID); err != nil {
		a.writeLookupError(w, r, err)
		return
	}

	e := AuditEntry{Actor: actor, Source: source, Action: "backup.create", ServerName: name}
	if p := principalFromContext(r.Context()); p != nil {
		e.ActorUserID = p.UserID
	}
	a.auditEntry(r, e)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"name":   name,
		"status": "backing_up",
	})
}

// handleInternalRestore creates the PVC and restores without a safety snapshot on a rebuilt host; the server must be stopped.
func (a *API) handleInternalRestore(w http.ResponseWriter, r *http.Request) {
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
	backup, err := a.Repo.LatestBackupForOwner(r.Context(), name, rec.OwnerID)
	if errors.Is(err, ErrNotFound) {
		writeError(w, r, newError(http.StatusNotFound, "no_backup", "no restorable backup exists for this server"))
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if info.Ready || info.DesiredState != string(v1alpha1.DesiredStopped) {
		writeError(w, r, newError(http.StatusConflict, "not_stopped", "stop the server before restoring a backup"))
		return
	}
	if a.Restorer == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "restore_unavailable", "restore subsystem is not configured"))
		return
	}
	if err := a.Cluster.EnsureWorldVolume(r.Context(), name); err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	release, ok := a.acquireWorld(w, r, name, maintenance.KindRestore, "stop the server before restoring a backup")
	if !ok {
		return
	}
	defer release()
	if err := a.Restorer.Restore(r.Context(), name, backup.BackupRef, backup.SHA256); err != nil {
		if isRestoreInProgress(err) {
			writeError(w, r, newError(http.StatusConflict, "restore_in_progress",
				"a restore is still running on this server's world; retry once it finishes"))
			return
		}
		a.writeLookupError(w, r, err)
		return
	}
	payload, _ := json.Marshal(map[string]any{"backup_id": backup.ID, "rebuild": true})
	a.auditEntry(r, AuditEntry{Actor: "restore-host", Source: internalSource(r), Action: "backup.restore", ServerName: name, Payload: payload})
	writeJSON(w, http.StatusAccepted, map[string]any{"name": name, "backup_id": backup.ID})
}
