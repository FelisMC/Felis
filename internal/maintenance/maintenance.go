// Package maintenance serializes world-volume access using Job holders and optimistic CR admission locks.
package maintenance

import (
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	// Annotation stores the admission lock as "<kind>@<RFC3339 time>".
	Annotation = "felis.lolicon.best/maintenance"
	// Grace expires admission locks that have no backing Job.
	Grace = 2 * time.Minute

	// Maintenance executors share these Job labels.
	LabelServer    = "felis.lolicon.best/server"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelFilesMode = "felis.lolicon.best/files-mode"
	// World and file exports hold the volume; backup exports do not.
	LabelExportMode = "felis.lolicon.best/export-mode"

	// LabelThenRestore keeps a safety snapshot holding the volume until its restore is started or abandoned.
	LabelThenRestore            = "felis.lolicon.best/then-restore"
	AnnotationRestoreRef        = "felis.lolicon.best/restore-ref"
	AnnotationRestoreBackupID   = "felis.lolicon.best/restore-backup-id"
	AnnotationThenRestoreReason = "felis.lolicon.best/then-restore-reason"
)

const (
	ThenRestorePending   = "pending"
	ThenRestoreStarted   = "started"
	ThenRestoreAbandoned = "abandoned"
)

const (
	KindMigration   = "migration"
	KindRestore     = "restore"
	KindBackup      = "backup"
	KindFileWrite   = "file-write"
	KindConfigWrite = "config-write"
	KindExport      = "export"
	// KindReap has no Job; the reaper refreshes its annotation within Grace.
	KindReap = "reap"
)

const (
	FilesModeList            = "list"
	FilesModeRead            = "read"
	FilesModeSearch          = "search"
	FilesModeExtensions      = "extensions"
	FilesModePaperGlobalRead = "paper-global-read"
	FilesModeLogTail         = "log-tail"
)

const (
	ExportModeWorld  = "world"
	ExportModeBackup = "backup"
	ExportModeFiles  = "files"
)

// JobKind treats unknown file/export modes as holders; only known read-only operations are exempt.
func JobKind(j *batchv1.Job) (string, bool) {
	switch j.Labels[LabelManagedBy] {
	case "felis-restore":
		return KindRestore, true
	case "felis-backup":
		return KindBackup, true
	case "felis-files":
		if mode := j.Labels[LabelFilesMode]; mode != FilesModeList && mode != FilesModeRead && mode != FilesModeSearch && mode != FilesModeExtensions && mode != FilesModePaperGlobalRead && mode != FilesModeLogTail {
			return KindFileWrite, true
		}
	case "felis-export":
		if j.Labels[LabelExportMode] != ExportModeBackup {
			return KindExport, true
		}
	}
	return "", false
}

// RestorePending keeps safety snapshots holding the volume even after their Job finishes.
func RestorePending(j *batchv1.Job) bool {
	return j.Labels[LabelManagedBy] == "felis-backup" && j.Labels[LabelThenRestore] == ThenRestorePending
}

// JobFinished includes target conditions set before the Job controller finishes Pod teardown.
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

func LockValue(kind string, at time.Time) string {
	return kind + "@" + at.UTC().Format(time.RFC3339)
}

// LockKind returns an empty string for malformed values.
func LockKind(v string) string {
	kind, _, _ := parseLock(v)
	return kind
}

// Malformed locks are stale because they have no usable expiry.
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

// Holder prefers unfinished Jobs or pending restore chains, then admission locks younger than Grace.
func Holder(server string, annotations map[string]string, jobs []batchv1.Job, now time.Time) (string, bool) {
	for i := range jobs {
		j := &jobs[i]
		if j.Labels[LabelServer] != server {
			continue
		}
		if j.Labels["felis.lolicon.best/archive-pending"] == "true" {
			return KindBackup, true
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
		if kind, at, ok := parseLock(v); ok && (kind == KindMigration || (now.Sub(at) < Grace && at.Sub(now) < Grace)) {
			return kind, true
		}
	}
	return "", false
}
