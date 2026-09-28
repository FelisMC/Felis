package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"felis.lolicon.best/internal/fileedit"
)

// cmdFiles is the in-Pod entrypoint the file-editor Job runs. internal/fileedit
// renders a Pod whose command is `/usr/local/bin/felis files`. It performs ONE
// file operation against the mounted world volume, prints the result as a single
// marked JSON line on stdout, and exits — it is NOT a user-facing command and is
// never invoked by hand.
//
// Like cmdRestore it deliberately holds NO database credentials and never calls
// config.Load: felis-api made the authorization decision (the caller owns this
// server, and the server is stopped so the RWO world volume is free); this process
// is the unprivileged hands that touch bytes. Its entire input is the flags
// below plus, for a write, the content variables and, for an upload, one token.
// Every isolation guarantee lives in the Pod spec (internal/fileedit/jobspec.go),
// and the path-containment guarantee lives in fileedit.Execute, which resolves
// every path through os.Root and therefore cannot be walked out of the world
// mount.
//
// Exit status carries a specific meaning that felis-api depends on: a CALLER-fault
// outcome — a path that escapes the root, a file that is missing or too large — is
// a SUCCESSFUL run that prints a Result carrying an error code, so the API can map
// it to a precise 4xx. A non-zero exit means the operation could not be attempted
// at all (the world mount is unreadable, an upload's bytes could not be fetched
// intact, the result unprintable), which the API reports as a 500.
func cmdFiles(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	fs.SetOutput(stderr)
	op := fs.String("op", "", "operation: list, read, write, mkdir, delete, rename, upload or unzip")
	path := fs.String("path", "", "path to operate on, relative to the world root (empty = the root itself)")
	worldsRoot := fs.String("worlds-root", "/data", "mount path of the world PVC; every path resolves under it")
	expect := fs.String("expect-sha256", "", "write only: refuse unless the file's current SHA-256 (hex) is this")
	createOnly := fs.Bool("create-only", false, "write only: refuse a path that already exists")
	to := fs.String("to", "", "rename only: the destination path")
	sourceURL := fs.String("source-url", "", "upload only: felis-api URL to fetch the bytes from")
	size := fs.Int64("size", -1, "upload only: the byte count the fetched file must have")
	sum := fs.String("sha256", "", "write and upload: the SHA-256 (hex) the content or the fetched file must have")
	overwrite := fs.Bool("overwrite", false, "upload and unzip: replace files already there")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *op == "" {
		fmt.Fprintln(stderr, "felis files: --op is required")
		return 2
	}
	limitHeapToCgroup()
	req := fileedit.Request{
		Op: *op, Path: *path, To: *to, Expect: *expect, CreateOnly: *createOnly, Overwrite: *overwrite,
	}

	// New content arrives base64-encoded in the environment rather than in argv:
	// a process's arguments are world-readable on the node (/proc/<pid>/cmdline),
	// whereas its environment is not, and a config file being written can carry
	// secrets — an RCON password in server.properties is the obvious case. The
	// encoding is what lets arbitrary bytes (CRLF endings, a BOM, a NUL) survive a
	// channel that must be a valid string.
	switch *op {
	case fileedit.OpWrite:
		// The content's SHA-256 comes with it, so bytes that changed on the way
		// to this Job are refused rather than written (Request.ContentSHA256).
		if *sum == "" {
			fmt.Fprintln(stderr, "felis files: a write needs --sha256")
			return 2
		}
		content, err := fileedit.ContentFromEnv(os.LookupEnv)
		if err != nil {
			fmt.Fprintf(stderr, "felis files: %v\n", err)
			return 2
		}
		req.Content, req.ContentSHA256 = content, *sum
	case fileedit.OpUpload:
		token := os.Getenv(fileedit.UploadTokenEnv)
		if *sourceURL == "" || token == "" {
			fmt.Fprintf(stderr, "felis files: an upload needs --source-url and %s\n", fileedit.UploadTokenEnv)
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		req.Upload = &fileedit.Upload{
			Size: *size, SHA256: *sum,
			Open: func() (io.ReadCloser, error) { return fetchUpload(ctx, *sourceURL, token) },
			Landed: func() {
				if err := reportLanded(ctx, *sourceURL, token); err != nil {
					// The file is in place; felis-api drops its copy when it
					// has sat idle long enough, and the panel cancels it too.
					fmt.Fprintf(stderr, "felis files: tell felis-api the upload landed: %v\n", err)
				}
			},
		}
	}
	// An upload or an unzip (the only ops that report progress) can run long
	// enough that felis-api does not wait on its Job, and the panel shows how far
	// it has got from the latest of these lines (fileedit.K8sRunner.Ops).
	req.Progress = fileedit.ThrottledProgress(stdout, time.Second, time.Now)

	res, err := fileedit.Execute(*worldsRoot, req)
	if err != nil {
		// The operation could not be attempted — infrastructure, not caller fault.
		fmt.Fprintf(stderr, "felis files: %v\n", err)
		return 1
	}
	if err := fileedit.Print(stdout, res); err != nil {
		// The result exists but could not be delivered. Exiting non-zero is the only
		// honest signal left: felis-api would otherwise find no marked line and have
		// to guess why.
		fmt.Fprintf(stderr, "felis files: %v\n", err)
		return 1
	}
	return 0
}

// fetchUpload opens the staged upload on felis-api's internal face. There is no
// retry: the token opens the upload once (fileedit.Stage), so a second attempt
// could only be refused, and the caller retries the failed Job whole (a file
// sent in parts stays staged until its Job reports it landed, so that retry
// does not send it again). Redirects are refused because the request carries the
// token and the internal face never redirects; the header timeout catches a
// wedged endpoint, and the Job's activeDeadlineSeconds bounds the body.
func fetchUpload(ctx context.Context, url, token string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{
		Transport:     &http.Transport{ResponseHeaderTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET returned %s", resp.Status)
	}
	return resp.Body, nil
}

// reportLanded tells felis-api the upload's file is in place (DELETE on the URL
// it was fetched from, with the same token), so it deletes the copy it staged.
// One try: the file has landed whatever the answer, and a copy nobody deletes
// is dropped once it has sat idle for fileedit.SessionIdle.
func reportLanded(ctx context.Context, url, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE returned %s", resp.Status)
	}
	return nil
}
