package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/backup"
)

// cmdRestore is the in-Pod entrypoint the restore Job runs. internal/restore
// renders a Pod whose command is `/usr/local/bin/felis restore`. It extracts a
// world archive from the backup mount into the world mount and exits — it is NOT a
// user-facing command and is never invoked by hand.
//
// It deliberately holds NO database credentials and never calls config.Load:
// the felis-api made the authorization decision and looked up the archive ref;
// this process is the unprivileged hands that move bytes. Its entire input is
// the five flags below, mirroring the four-power isolation the rendered Job
// enforces (a restore Pod must not be able to reach the felis DB or the K8s
// API). All of those guarantees live in the Pod spec; this command only needs
// the archive store and the two mount roots.
func cmdRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source-url", "", "one-use archive download URL")
	sum := fs.String("sha256", "", "required digest for remote archive")
	limit := fs.Int64("max-bytes", archivetransfer.DefaultLimit, "maximum download size")
	server := fs.String("server", "", "server name being restored (for logging)")
	ref := fs.String("ref", "", "absolute path to the archive on the backup mount")
	store := fs.String("archive-store", "tarLocal", "archive backend (only tarLocal is implemented)")
	backupRoot := fs.String("backup-root", "/backups", "mount path of the backup PVC (archive refs must resolve under it)")
	worldsRoot := fs.String("worlds-root", "/world", "mount path of the world PVC the archive extracts into")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *source != "" {
		dir, err := os.MkdirTemp("/tmp", "felis-restore-")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		*backupRoot = dir
		*ref = filepath.Join(dir, "world.tar.gz")
		if err := archivetransfer.Fetch(ctx, *source, os.Getenv(archivetransfer.TokenEnv), *ref, *sum, *limit); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if *ref == "" {
		fmt.Fprintln(stderr, "felis restore: --ref is required")
		return 2
	}
	if *store != "tarLocal" {
		fmt.Fprintf(stderr, "felis restore: archive store %q is not implemented in this build (only tarLocal)\n", *store)
		return 1
	}
	// Defense in depth: the ref comes from felis-api (trusted), but this process
	// is the one that opens it, so it confirms the ref stays within the backup
	// mount. A ref outside it would mean reading an arbitrary host path, which a
	// restore Pod must never do.
	if !refWithinRoot(*ref, *backupRoot) {
		fmt.Fprintf(stderr, "felis restore: ref %q is not under backup root %q\n", *ref, *backupRoot)
		return 1
	}

	// The world PVC is mounted directly at worldsRoot, so the resolver returns it
	// for any target; the archive ref is absolute and is opened directly. This is
	// the same TarLocal the reaper uses to write archives, run in reverse.
	archiver := &backup.TarLocal{
		BackupRoot: *backupRoot,
		Resolve: func(string) (string, error) {
			return *worldsRoot, nil
		},
	}

	if err := archiver.Restore(ctx, backup.ArchiveRef(*ref), *server); err != nil {
		fmt.Fprintf(stderr, "felis restore: %v\n", err)
		return 1
	}
	if *source != "" {
		if err := backup.VerifyRestored(ctx, *ref, *worldsRoot); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "felis restore: server=%s restored from %s into %s\n", *server, *ref, *worldsRoot)
	return 0
}

// refWithinRoot reports whether ref resolves to a path inside root. Both are
// cleaned first so "/backups/../etc/passwd" cannot slip through.
func refWithinRoot(ref, root string) bool {
	cleanRoot := filepath.Clean(root)
	cleanRef := filepath.Clean(ref)
	if cleanRef == cleanRoot {
		return true
	}
	return strings.HasPrefix(cleanRef, cleanRoot+string(filepath.Separator))
}
