package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/worldexport"
)

// cmdExport is the in-Pod entrypoint the export Job runs. internal/worldexport
// renders a Pod whose command is `/usr/local/bin/felis export`. It archives the
// mounted world (or opens one archive on the mounted backup store), PUTs the
// tar.gz to felis-api's internal face, and exits once felis-api says the
// owner's browser got all of it. It is NOT a user-facing command and is never
// invoked by hand.
//
// Like cmdRestore it holds no database credentials and never calls config.Load:
// felis-api made every decision (who may download what, that the server is
// stopped, which archive) before the Job existed. Its input is the flags below
// plus the one-time upload token in the environment, which opens this one
// export and nothing else.
//
// Exit status: 0 once felis-api answers 204 (the download completed), 1 when
// the archive could not be read or handed over, or felis-api refused it (the
// browser never came, left early, or the backup failed its digest check), 2 on
// bad flags. The last stderr line reaches the export's status and the jobs list.
func cmdExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "what to export: world or backup")
	server := fs.String("server", "", "server name being exported (for logging)")
	target := fs.String("target-url", "", "felis-api URL to PUT the archive to")
	ref := fs.String("ref", "", "backup only: absolute path to the archive on the backup mount")
	backupRoot := fs.String("backup-root", "/backups", "backup only: mount path of the backup PVC (the ref must resolve under it)")
	worldsRoot := fs.String("worlds-root", "/world", "world only: mount path of the world PVC to archive")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := os.Getenv(worldexport.TokenEnv)
	if *target == "" || token == "" {
		fmt.Fprintf(stderr, "felis export: --target-url and %s are required\n", worldexport.TokenEnv)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch *mode {
	case worldexport.ModeBackup:
		if *ref == "" {
			fmt.Fprintln(stderr, "felis export: --ref is required for a backup")
			return 2
		}
		err = exportBackup(ctx, *target, token, *ref, *backupRoot)
	case worldexport.ModeWorld:
		err = exportWorld(ctx, *target, token, *worldsRoot, stdout)
	default:
		fmt.Fprintf(stderr, "felis export: --mode must be %s or %s\n", worldexport.ModeWorld, worldexport.ModeBackup)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis export: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis export: server=%s mode=%s downloaded\n", *server, *mode)
	return 0
}

// exportBackup hands over one stored archive as it is, with its length, so the
// browser shows real progress and felis-api can check its recorded digest.
func exportBackup(ctx context.Context, target, token, ref, root string) error {
	// Defense in depth, as in cmdRestore: the ref comes from felis-api, but this
	// process opens it, so it confirms the ref stays on the backup mount.
	if !refWithinRoot(ref, root) {
		return fmt.Errorf("ref %q is not under backup root %q", ref, root)
	}
	f, err := os.Open(ref)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return putExport(ctx, target, token, f, st.Size())
}

// exportWorld archives the world straight into the request body: nothing is
// staged, so a world bigger than the Pod's memory or any scratch disk exports
// the same. A read error mid-way aborts the chunked body, and felis-api cuts
// the browser's download off rather than end it.
func exportWorld(ctx context.Context, target, token, root string, stdout io.Writer) error {
	pr, pw := io.Pipe()
	skippedc := make(chan []string, 1)
	go func() {
		skipped, err := backup.WriteTarGz(ctx, pw, root)
		pw.CloseWithError(err)
		skippedc <- skipped
	}()
	err := putExport(ctx, target, token, pr, -1)
	pr.CloseWithError(io.ErrClosedPipe) // stops the archiver if the PUT ended first
	if skipped := <-skippedc; len(skipped) > 0 {
		fmt.Fprintf(stdout, "felis export: left out %d entries a tar cannot hold (symbolic links, devices, sockets)\n", len(skipped))
	}
	return err
}

// putExport PUTs the archive to felis-api. There is no retry: the token opens
// the export once, so a second attempt could only be refused. Redirects are
// refused because the request carries the token and the internal face never
// redirects. felis-api answers only after the whole download, which the Job's
// activeDeadlineSeconds bounds, so the header timeout is a backstop for a
// wedged endpoint and not the real limit.
func putExport(ctx context.Context, target, token string, body io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")
	client := &http.Client{
		Transport:     &http.Transport{ResponseHeaderTimeout: 10 * time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&e) == nil && e.Error.Message != "" {
		return fmt.Errorf("felis-api answered %s: %s", resp.Status, e.Error.Message)
	}
	return fmt.Errorf("felis-api answered %s", resp.Status)
}
