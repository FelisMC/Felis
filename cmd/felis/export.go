package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/worldexport"
)

// Redact secrets on export; backups retain them for restoration.
func cmdExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "what to export: world, backup or files")
	server := fs.String("server", "", "server name being exported (for logging)")
	target := fs.String("target-url", "", "felis-api URL to PUT the export to")
	ref := fs.String("ref", "", "backup only: absolute path to the archive on the backup mount")
	backupRoot := fs.String("backup-root", "/backups", "backup only: mount path of the backup PVC (the ref must resolve under it)")
	sum := fs.String("sha256", "", "backup only: the sha256 recorded when the archive was written; a mismatch fails the export before its end is sent")
	worldsRoot := fs.String("worlds-root", "/world", "world and files: mount path of the world PVC")
	var selected []string
	fs.Func("select", "files: selected path; repeat for each item", func(value string) error { selected = append(selected, value); return nil })
	path := fs.String("path", "", "files only: the file or folder to send, relative to the world root")
	rawArchive := fs.Bool("archive-raw", false, "internal archive transfer: preserve the complete world")
	dir := fs.Bool("dir", false, "files only: the path is a folder, sent as a zip")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := os.Getenv(worldexport.TokenEnv)
	if *target == "" || token == "" {
		fmt.Fprintf(stderr, "felis export: --target-url and %s are required\n", worldexport.TokenEnv)
		return 2
	}
	limitHeapToCgroup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch *mode {
	case worldexport.ModeBackup:
		if *ref == "" {
			fmt.Fprintln(stderr, "felis export: --ref is required for a backup")
			return 2
		}
		err = exportBackup(ctx, *target, token, *ref, *backupRoot, *sum, stdout)
	case worldexport.ModeWorld:
		if *rawArchive {
			err = streamExport(ctx, *target, token, archiveType, -1, func(w io.Writer) error { _, _, err := backup.WriteTarGz(ctx, w, *worldsRoot, nil); return err })
		} else {
			err = exportWorld(ctx, *target, token, *worldsRoot, stdout)
		}
	case worldexport.ModeFiles:
		if *path == "" && len(selected) == 0 {
			fmt.Fprintln(stderr, "felis export: --path is required for files")
			return 2
		}
		if len(selected) > 0 {
			err = exportSelectedFiles(ctx, *target, token, *worldsRoot, selected, stdout)
		} else {
			err = exportFiles(ctx, *target, token, *worldsRoot, *path, *dir, stdout)
		}
	default:
		fmt.Fprintf(stderr, "felis export: --mode must be %s, %s or %s\n", worldexport.ModeWorld, worldexport.ModeBackup, worldexport.ModeFiles)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis export: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis export: server=%s mode=%s downloaded\n", *server, *mode)
	return 0
}

// archiveType is the content type of a world or backup export.
const archiveType = "application/gzip"

// errBackupDigest fails a backup export whose stored archive does not hash to the digest recorded when it was written.
var errBackupDigest = errors.New("the backup archive does not match the sha256 recorded when it was written")

// Hash the entire source archive and abort upload before completion on a digest mismatch.
func exportBackup(ctx context.Context, target, token, ref, root, want string, stdout io.Writer) error {
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
	var src io.Reader = f
	if want != "" {
		src = &digestReader{r: f, sum: sha256.New(), want: want}
	}
	var withheld []string
	err = streamExport(ctx, target, token, archiveType, -1, func(w io.Writer) error {
		var err error
		withheld, err = backup.FilterTarGz(ctx, w, src, archiveFilter)
		return err
	})
	if errors.Is(err, errBackupDigest) {
		return errBackupDigest // the jobs list shows it as it is, not wrapped as a read error
	}
	reportWithheld(stdout, len(withheld))
	return err
}

// exportWorld archives the world straight into the request body: nothing is
// staged, so a world bigger than the Pod's memory or any scratch disk exports
// the same.
func exportWorld(ctx context.Context, target, token, root string, stdout io.Writer) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	guard := fileedit.NewGuard(r)
	r.Close()
	var skipped, withheld []string
	err = streamExport(ctx, target, token, archiveType, -1, func(w io.Writer) error {
		var err error
		skipped, withheld, err = backup.WriteTarGz(ctx, w, root, worldFilter(guard))
		return err
	})
	if len(skipped) > 0 {
		fmt.Fprintf(stdout, "felis export: left out %d entries a tar cannot hold (symbolic links, devices, sockets)\n", len(skipped))
	}
	reportWithheld(stdout, len(withheld))
	return err
}

// Stream files at their known length and directories as ZIPs; dir records the caller's observed type.
func exportFiles(ctx context.Context, target, token, root, path string, dir bool, stdout io.Writer) error {
	d, err := fileedit.OpenDownload(root, path, dir)
	if err != nil {
		return err
	}
	defer d.Close()
	err = streamExport(ctx, target, token, d.ContentType, d.Size, func(w io.Writer) error { return d.WriteTo(ctx, w) })
	if d.Skipped > 0 {
		fmt.Fprintf(stdout, "felis export: left out %d entries a zip does not carry (symbolic links, devices, sockets)\n", d.Skipped)
	}
	reportWithheld(stdout, d.Withheld)
	return err
}

func exportSelectedFiles(ctx context.Context, target, token, root string, paths []string, stdout io.Writer) error {
	d, err := fileedit.OpenSelectedDownload(root, paths)
	if err != nil {
		return err
	}
	defer d.Close()
	err = streamExport(ctx, target, token, d.ContentType, d.Size, func(w io.Writer) error { return d.WriteTo(ctx, w) })
	reportWithheld(stdout, d.Withheld)
	return err
}

func reportWithheld(stdout io.Writer, n int) {
	if n > 0 {
		fmt.Fprintf(stdout, "felis export: left out %d files that hold platform secrets\n", n)
	}
}

// worldFilter guards a live world by file identity, so a link to a guarded
// file under another name is caught as well.
func worldFilter(g fileedit.Guard) backup.Filter {
	return func(name string, info fs.FileInfo) (bool, func([]byte) []byte) {
		if fileedit.InternalPath(name) {
			return true, nil
		}
		return guardAction(g.Rule(info))
	}
}

// archiveFilter guards a stored archive, which has only names.
func archiveFilter(name string, _ fs.FileInfo) (bool, func([]byte) []byte) {
	return guardAction(fileedit.ArchiveRule(name))
}

func guardAction(withhold, redact bool) (bool, func([]byte) []byte) {
	if redact {
		return withhold, fileedit.RedactProps
	}
	return withhold, nil
}

// digestReader passes r through, hashing it, and turns r's EOF into
// errBackupDigest when the bytes do not hash to want.
type digestReader struct {
	r    io.Reader
	sum  hash.Hash
	want string
}

func (d *digestReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.sum.Write(p[:n])
	if err == io.EOF && !strings.EqualFold(hex.EncodeToString(d.sum.Sum(nil)), d.want) {
		return n, errBackupDigest
	}
	return n, err
}

// Abort chunked uploads on producer errors; the API verifies the digest before releasing the final bytes.
func streamExport(ctx context.Context, target, token, contentType string, size int64, write func(io.Writer) error) error {
	pr, pw := io.Pipe()
	trailer := http.Header{worldexport.DigestTrailer: nil}
	werr := make(chan error, 1)
	go func() {
		sum := sha256.New()
		err := write(io.MultiWriter(pw, sum))
		if err == nil {
			// Set before the body ends: the transport reads the trailer once it
			// has read the body to its end.
			trailer.Set(worldexport.DigestTrailer, "sha-256=:"+base64.StdEncoding.EncodeToString(sum.Sum(nil))+":")
		}
		pw.CloseWithError(err)
		werr <- err
	}()
	err := putExport(ctx, target, token, contentType, pr, size, trailer)
	pr.CloseWithError(io.ErrClosedPipe)
	if w := <-werr; w != nil && !errors.Is(w, io.ErrClosedPipe) {
		return w
	}
	return err
}

// Upload tokens are single-use: reject redirects and carry known length separately from chunked framing.
func putExport(ctx context.Context, target, token, contentType string, body io.Reader, size int64, trailer http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return err
	}
	req.ContentLength = -1
	req.Trailer = trailer
	if size >= 0 {
		req.Header.Set(worldexport.LengthHeader, strconv.FormatInt(size, 10))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
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
