// Package maintenance is the per-server mutual exclusion between a game server
// and the Jobs that write or snapshot its world volume (restore, backup, file
// write, world export). The world PVC is ReadWriteOnce, and RWO is exclusive per NODE: on a
// single-node cluster the game pod and a restore pod mount it side by side, so
// the access mode alone guards nothing. A server woken mid-restore boots on a
// half-extracted world and the restore then prunes what it wrote; a server woken
// mid-backup produces a torn archive that a later restore makes permanent.
//
// Two signals mark a volume as held:
//
//   - an unfinished maintenance Job labelled for the server. Once the Job exists
//     it IS the lock, for as long as it runs, however long that is.
//   - the Annotation on the MinecraftServer, written by felis-api with an
//     optimistic-lock patch before it creates the Job and removed right after.
//     It bridges the gap between "admitted" and "the Job is visible", and it is
//     what serialises admission against a wake: both write the same object under
//     its resourceVersion, so one of two racing writers always loses with a
//     conflict and re-checks.
//
// A lock older than Grace with no Job behind it is stale (felis-api died between
// the two writes) and holds nothing. The reaper is the one holder without a Job:
// it keeps its lock fresh by rewriting it while it archives and reclaims a world.
//
// A restore that starts with a safety snapshot is two Jobs in a row: the backup
// Job carries the restore to run after it (LabelThenRestore), and felis-api
// creates the restore Job once the backup has succeeded. The backup Job keeps
// holding the volume, as a restore, from its creation until felis-api has
// settled what follows it, so nothing can wake the server between the two Jobs.
//
// File reads and listings are not holders. They mount the volume read-only for a
// second or two, and a server starting beside one cannot hurt either side, so
// nobody waits for them. A world export mounts it read-only too, but it holds:
// it archives the world for as long as the download takes, and a server started
// beside it would hand the owner a torn archive. Exporting a backup reads only
// the backup store and holds nothing.
package maintenance

import (
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	// Annotation is the admission lock on a MinecraftServer. Its value is
	// "<kind>@<RFC3339 time>" (LockValue).
	Annotation = "felis.lolicon.best/maintenance"
	// Grace is how long a lock counts as held when no Job backs it. felis-api
	// creates the Job within milliseconds of taking the lock and then drops it, so
	// a lock this old means the process died in between.
	Grace = 2 * time.Minute

	// LabelServer / LabelManagedBy are the labels every maintenance executor puts
	// on its Job (internal/restore, internal/backupjob, internal/fileedit and
	// internal/worldexport keep their own copies; maintenance_test pins them
	// against these).
	LabelServer    = "felis.lolicon.best/server"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelFilesMode is the file operation (list, read, write, mkdir, delete,
	// rename, upload) a files Job performs. Every one but list and read holds the
	// volume.
	LabelFilesMode = "felis.lolicon.best/files-mode"
	// LabelExportMode is what an export Job archives: ExportModeWorld (the live
	// world, which holds the volume) or ExportModeBackup (a stored archive).
	LabelExportMode = "felis.lolicon.best/export-mode"

	// LabelThenRestore marks a backup Job that is the safety snapshot in front of
	// a restore. Its value is the chain's state: ThenRestorePending until felis-api
	// settles it, then ThenRestoreStarted or ThenRestoreAbandoned. A label, so the
	// settling loop finds pending chains with a selector.
	LabelThenRestore = "felis.lolicon.best/then-restore"
	// AnnotationRestoreRef / AnnotationRestoreBackupID name the backup the chained
	// restore extracts: its archive ref and its world_backups id.
	AnnotationRestoreRef      = "felis.lolicon.best/restore-ref"
	AnnotationRestoreBackupID = "felis.lolicon.best/restore-backup-id"
	// AnnotationThenRestoreReason is the code saying why a chain was abandoned
	// (internal/api defines the codes).
	AnnotationThenRestoreReason = "felis.lolicon.best/then-restore-reason"
)

// States of LabelThenRestore.
const (
	ThenRestorePending   = "pending"
	ThenRestoreStarted   = "started"
	ThenRestoreAbandoned = "abandoned"
)

// Kinds of holder.
const (
	KindRestore   = "restore"
	KindBackup    = "backup"
	KindFileWrite = "file-write"
	KindExport    = "export"
	// KindReap is the reaper archiving an idle world and reclaiming its volume.
	// It runs no Job: the reaper holds the Annotation itself and rewrites it
	// well inside Grace for as long as it works on the world.
	KindReap = "reap"
)

// The LabelFilesMode values of the two file operations that only look: they
// mount the world read-only, so they hold nothing.
const (
	FilesModeList = "list"
	FilesModeRead = "read"
)

// The LabelExportMode values.
const (
	ExportModeWorld  = "world"
	ExportModeBackup = "backup"
)

// JobKind names the holder a Job represents, or reports false for a Job that
// holds nothing (a file read, a backup export, a build, anything else in the
// namespace). A files Job holds unless it is a list or a read, so an operation
// this build does not know — and a Job without LabelFilesMode, which can only be
// an old one still inside its TTL — counts as a change: over-counting is the
// safe side. An export Job holds unless it names a backup, for the same reason.
func JobKind(j *batchv1.Job) (string, bool) {
	switch j.Labels[LabelManagedBy] {
	case "felis-restore":
		return KindRestore, true
	case "felis-backup":
		return KindBackup, true
	case "felis-files":
		if mode := j.Labels[LabelFilesMode]; mode != FilesModeList && mode != FilesModeRead {
			return KindFileWrite, true
		}
	case "felis-export":
		if j.Labels[LabelExportMode] != ExportModeBackup {
			return KindExport, true
		}
	}
	return "", false
}

// RestorePending reports whether j is a safety-snapshot backup Job whose restore
// felis-api has not yet started or abandoned. Such a Job holds the volume as a
// restore whether or not it has finished.
func RestorePending(j *batchv1.Job) bool {
	return j.Labels[LabelManagedBy] == "felis-backup" && j.Labels[LabelThenRestore] == ThenRestorePending
}

// JobFinished reports whether a Job has reached a terminal condition. The
// success/failure-target conditions count as terminal: the Job controller sets
// them the moment the outcome is decided, before it finishes tearing the pods
// down, and waiting for Complete would keep a wake refused for no reason.
func JobFinished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete, batchv1.JobFailed, batchv1.JobSuccessCriteriaMet, batchv1.JobFailureTarget:
			return true
		}
	}
	return false
}

// LockValue renders the Annotation value for a holder admitted at `at`.
func LockValue(kind string, at time.Time) string {
	return kind + "@" + at.UTC().Format(time.RFC3339)
}

// parseLock splits a lock value. A value that does not parse is stale: a lock
// nobody can date must not be able to hold a server down forever.
func parseLock(v string) (string, time.Time, bool) {
	kind, stamp, ok := strings.Cut(v, "@")
	if !ok || kind == "" {
		return "", time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return "", time.Time{}, false
	}
	return kind, at, true
}

// Holder reports what, if anything, holds the server's world volume at `now`:
// the first unfinished maintenance Job among jobs (or a safety snapshot whose
// restore is still pending), else a lock in annotations younger than Grace. jobs
// may contain unrelated Jobs; only the server's own holders count.
func Holder(server string, annotations map[string]string, jobs []batchv1.Job, now time.Time) (string, bool) {
	for i := range jobs {
		j := &jobs[i]
		if j.Labels[LabelServer] != server {
			continue
		}
		if RestorePending(j) {
			return KindRestore, true
		}
		if JobFinished(j) {
			continue
		}
		if kind, ok := JobKind(j); ok {
			return kind, true
		}
	}
	if v, ok := annotations[Annotation]; ok {
		if kind, at, ok := parseLock(v); ok && now.Sub(at) < Grace && at.Sub(now) < Grace {
			return kind, true
		}
	}
	return "", false
}
