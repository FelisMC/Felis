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

	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/reaper"
	"felis.lolicon.best/internal/store"
	ctrl "sigs.k8s.io/controller-runtime"
)

// cmdBackup is the in-Pod entrypoint the on-demand backup Job runs. internal/backupjob
// renders a Pod whose command is `/usr/local/bin/felis backup`. It tars the mounted
// world into the archive store AND records the world_backups row, then exits — it is
// NOT a user-facing command and is never invoked by hand.
//
// Unlike `felis restore`, this command DOES hold database credentials (via the mounted
// config Secret) and calls config.Load: a backup must record its row atomically with
// the archive, exactly like the reaper — otherwise a completed archive would leak as an
// orphan file the retention pass never expires. The security review for that departure
// lives in internal/backupjob/jobspec.go. The world is mounted directly at --worlds-root
// (single-PVC mount, like restore), so the archiver's resolver returns that root for any
// PVC; the archive is written into the backup PVC at cfg.Archive.LocalPath.
func cmdBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	server := fs.String("server", "", "server name whose world is being backed up")
	formerOwner := fs.String("former-owner", "", "owner recorded on the backup row (empty for an unowned server)")
	worldsRoot := fs.String("worlds-root", "/world", "mount path of the world PVC being archived")
	reason := fs.String("reason", reasonManual, "world_backups reason: manual, or pre_restore for the safety snapshot in front of a restore")
	protect := fs.String("protect", "", "backup id the prune must keep (the one a chained restore extracts)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *server == "" {
		fmt.Fprintln(stderr, "felis backup: --server is required")
		return 2
	}
	keep, ok := map[string]int{reasonManual: -1, backupjob.ReasonPreRestore: preRestoreKeep}[*reason]
	if !ok {
		fmt.Fprintf(stderr, "felis backup: unknown --reason %q (manual or %s)\n", *reason, backupjob.ReasonPreRestore)
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
	// The [archive] parse the reaper uses; an on-demand backup takes its
	// manual_retention and manual_keep.
	rcfg, err := reaperConfig(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "felis backup: %v\n", err)
		return 1
	}
	if keep < 0 {
		keep = rcfg.ManualKeep
	}

	// The world PVC is mounted directly at worldsRoot; the resolver returns it for
	// any target, exactly as in cmdRestore. This is the same TarLocal the reaper
	// writes archives with.
	archiver := &backup.TarLocal{
		BackupRoot: cfg.Archive.LocalPath,
		Resolve: func(string) (string, error) {
			return *worldsRoot, nil
		},
	}

	ctx := ctrl.SetupSignalHandler()

	// The archive store shares the node's disk with every world and the
	// database: an owner's backup must not be what tips it into eviction.
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

	drv, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintf(stderr, "felis backup: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	rec := reaper.BackupRecord{
		ID:          newBackupID(),
		ServerName:  *server,
		FormerOwner: *formerOwner,
		BackupRef:   string(ref),
		SizeBytes:   size,
		Reason:      *reason,
		ExpiresAt:   time.Now().Add(rcfg.ManualRetention),

		SHA256:         a.SHA256,
		SkippedEntries: len(a.Skipped),
	}
	st := reaper.NewPGStore(drv.DB())
	if err := st.InsertBackup(ctx, rec); err != nil {
		// The archive is written but unrecorded — an orphan the retention pass would
		// never expire. Delete it so a failed backup leaves no leaked bytes, mirroring
		// the reaper's archive-then-record atomicity.
		if delErr := archiver.Delete(ctx, ref); delErr != nil {
			fmt.Fprintf(stderr, "felis backup: record failed (%v) AND orphan archive %s could not be removed: %v\n", err, ref, delErr)
			return 1
		}
		fmt.Fprintf(stderr, "felis backup: record failed, orphan archive removed: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "felis backup: server=%s archived %d bytes to %s (backup %s)\n", *server, size, ref, rec.ID)
	pruneBackups(ctx, st, archiver, *server, *reason, keep, *protect, stdout, stderr)
	return 0
}

const (
	reasonManual = "manual"
	// preRestoreKeep is how many safety snapshots a server keeps: enough to walk
	// back a couple of restores in a row, without every restore adding a world's
	// worth of bytes for the full manual retention.
	preRestoreKeep = 3
)

// pruneBackups keeps server's newest keep backups of this reason and removes the
// rest, oldest first, so repeated backups of one world cannot fill the shared
// archive store. protect is never removed: it is the backup a chained restore is
// about to extract. The new backup is already recorded; a removal that fails is
// reported and retried after the next backup.
func pruneBackups(ctx context.Context, st *reaper.PGStore, archiver backup.WorldArchiver, server, reason string, keep int, protect string, stdout, stderr io.Writer) {
	excess, err := st.ExcessBackups(ctx, server, reason, keep, protect)
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

// newBackupID mints a world_backups primary key, matching the reaper's "bk-"+hex
// scheme so a manual and an inactivity backup are indistinguishable downstream.
func newBackupID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is fatal and unrecoverable; a time-based fallback would
		// be a weaker ID for no benefit. A panic is the honest failure here.
		panic("felis backup: crypto/rand: " + err.Error())
	}
	return "bk-" + hex.EncodeToString(b[:])
}
