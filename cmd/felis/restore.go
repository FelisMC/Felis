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

// Restore Jobs receive no database or Kubernetes credentials; admission belongs to felis-api.
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
	// Validate archive containment inside the backup mount before opening it.
	if !refWithinRoot(*ref, *backupRoot) {
		fmt.Fprintf(stderr, "felis restore: ref %q is not under backup root %q\n", *ref, *backupRoot)
		return 1
	}

	// The Job mounts the target PVC directly; source archive references remain absolute.
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
