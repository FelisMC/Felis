package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"felis.lolicon.best/internal/archivepolicy"
	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/reaper"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Backup Jobs hold database credentials to record their archive row.
func cmdBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	server := fs.String("server", "", "server name whose world is being backed up")
	formerOwner := fs.String("former-owner", "", "owner recorded on the backup row (empty for an unowned server)")
	worldsRoot := fs.String("worlds-root", "/world", "mount path of the world PVC being archived")
	reason := fs.String("reason", reasonManual, "world_backups reason: manual, pre_restore for the safety snapshot in front of a restore, scheduled for felis-api's daily restore point, or replica for the frequent copy of a world on a worker")
	protect := fs.String("protect", "", "backup id the prune must keep (the one a chained restore extracts)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *server == "" {
		fmt.Fprintln(stderr, "felis backup: --server is required")
		return 2
	}
	if _, _, ok := backupPolicy(*reason, reaper.DefaultConfig()); !ok {
		fmt.Fprintf(stderr, "felis backup: unknown --reason %q (manual, %s, %s or %s)\n", *reason, backupjob.ReasonPreRestore, backupjob.ReasonScheduled, backupjob.ReasonReplica)
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis backup: %v\n", err)
		return 1
	}
	if cfg.Archive.Store != "tarLocal" {
		fmt.Fprintf(stderr, "felis backup: archive store %q is not implemented in this build (only tarLocal)\n", cfg.Archive.Store)
		return 1
	}
	// The Owner's retention policy, read once the database is open, overrides this [archive] parse.
	rcfg, err := reaperConfig(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "felis backup: %v\n", err)
		return 1
	}

	// The world PVC is mounted at worldsRoot, so the resolver returns it for any target.
	archiver := &backup.TarLocal{
		BackupRoot: cfg.Archive.LocalPath,
		Resolve: func(string) (string, error) {
			return *worldsRoot, nil
		},
	}

	ctx := ctrl.SetupSignalHandler()

	// The archive store shares the node's disk, so a backup must not push it into eviction.
	if err := backup.CheckRoom(cfg.Archive.LocalPath, *worldsRoot, backup.MinFreeAfter); err != nil {
		fmt.Fprintf(stderr, "felis backup: %v\n", err)
		return 1
	}

	a, err := archiver.Archive(ctx, *server, naming.WorldPVCName(*server))
	if err != nil {
		fmt.Fprintf(stderr, "felis backup: archive: %v\n", err)
		return 1
	}
	ref, size := a.Ref, a.Size
	if len(a.Skipped) > 0 {
		fmt.Fprintf(stderr, "felis backup: %d entries are not plain files or directories and are not in the archive: %s\n",
			len(a.Skipped), strings.Join(a.Skipped[:min(len(a.Skipped), 10)], ", "))
	}

	drv, err := openPodStore(ctx, cfg.Database.URL, "backup", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "felis backup: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	if err := archivepolicy.ApplyFrom(ctx, drv.DB(), &rcfg); err != nil {
		fmt.Fprintf(stderr, "felis backup: the panel's retention policy is unreadable, [archive] stands: %v\n", err)
	}
	keep, retention, _ := backupPolicy(*reason, rcfg)

	rec := reaper.BackupRecord{
		ID:          newBackupID(),
		ServerName:  *server,
		FormerOwner: *formerOwner,
		BackupRef:   string(ref),
		SizeBytes:   size,
		Reason:      *reason,
		ExpiresAt:   time.Now().Add(retention),

		SHA256:         a.SHA256,
		SkippedEntries: len(a.Skipped),
	}
	st := reaper.NewPGStore(drv.DB())
	if err := st.InsertBackup(ctx, rec); err != nil {
		// An unrecorded archive would never be expired by retention, so remove it.
		if delErr := archiver.Delete(ctx, ref); delErr != nil {
			fmt.Fprintf(stderr, "felis backup: record failed (%v) AND orphan archive %s could not be removed: %v\n", err, ref, delErr)
			return 1
		}
		fmt.Fprintf(stderr, "felis backup: record failed, orphan archive removed: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "felis backup: server=%s archived %d bytes to %s (backup %s)\n", *server, size, ref, rec.ID)
	pruneBackups(ctx, st, archiver, *server, *formerOwner, *reason, keep, *protect, stdout, stderr)
	return 0
}

const (
	reasonManual   = "manual"
	preRestoreKeep = 3 // safety snapshots kept per server
	// replicaKeep and replicaRetention bound a worker's frequent world copies; only the newest matter to a failover.
	replicaKeep      = 2
	replicaRetention = 48 * time.Hour
)

// backupPolicy returns the keep count and retention for a backup reason; ok is false for a reason this command does not record.
func backupPolicy(reason string, rcfg reaper.Config) (keep int, retention time.Duration, ok bool) {
	switch reason {
	case reasonManual:
		return rcfg.ManualKeep, rcfg.ManualRetention, true
	case backupjob.ReasonPreRestore:
		return preRestoreKeep, rcfg.ManualRetention, true
	case backupjob.ReasonScheduled:
		return rcfg.ScheduledKeep, rcfg.ScheduledRetention, true
	case backupjob.ReasonReplica:
		return replicaKeep, replicaRetention, true
	}
	return 0, 0, false
}

// pruneBackups removes the oldest backups of this reason beyond keep for one owner and server; protect is never removed.
// A failed removal is reported and retried after the next backup.
func pruneBackups(ctx context.Context, st *reaper.PGStore, archiver interface {
	Delete(context.Context, backup.ArchiveRef) error
}, server, owner, reason string, keep int, protect string, stdout, stderr io.Writer) {
	excess, err := st.ExcessBackups(ctx, server, owner, reason, keep, protect)
	if err != nil {
		fmt.Fprintf(stderr, "felis backup: list older backups of %s: %v\n", server, err)
		return
	}
	for _, b := range excess {
		if err := archiver.Delete(ctx, backup.ArchiveRef(b.BackupRef)); err != nil {
			fmt.Fprintf(stderr, "felis backup: remove older backup %s: %v\n", b.ID, err)
			continue
		}
		if err := st.MarkBackupDeleted(ctx, b.ID, time.Now()); err != nil {
			fmt.Fprintf(stderr, "felis backup: record the removal of %s: %v\n", b.ID, err)
			continue
		}
		fmt.Fprintf(stdout, "felis backup: removed older %s backup %s of %s (keeping the newest %d)\n", reason, b.ID, server, keep)
	}
}

func newBackupID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("felis backup: crypto/rand: " + err.Error())
	}
	return "bk-" + hex.EncodeToString(b[:])
}
