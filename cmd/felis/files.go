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

// File Jobs use os.Root without database credentials; a printed caller-error Result still exits successfully.
func cmdFiles(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	fs.SetOutput(stderr)
	op := fs.String("op", "", "operation: list, search, extensions, read, write, mkdir, delete, rename, copy, move, zip, upload, unzip, paper-global-read or paper-global-patch")
	browseURL := fs.String("browse-url", "", "internal command channel for a read-only file browser")
	query := fs.String("query", "", "search: filename substring")
	var selected []string
	fs.Func("select", "selected file or folder; repeat for each path", func(value string) error { selected = append(selected, value); return nil })
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

	if *browseURL != "" {
		limitHeapToCgroup()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := fileedit.Browse(ctx, *worldsRoot, *browseURL, os.Getenv(fileedit.BrowserTokenEnv)); err != nil {
			fmt.Fprintf(stderr, "felis files: %v\n", err)
			return 1
		}
		return 0
	}
	if *op == "" {
		fmt.Fprintln(stderr, "felis files: --op is required")
		return 2
	}
	limitHeapToCgroup()
	req := fileedit.Request{
		Op: *op, Path: *path, To: *to, Query: *query, Paths: selected, Expect: *expect, CreateOnly: *createOnly, Overwrite: *overwrite,
	}

	// Pass base64 content through the environment to keep file secrets out of process argv.
	switch *op {
	case fileedit.OpPaperGlobalPatch:
		if *sourceURL != "" || *sum == "" {
			fmt.Fprintln(stderr, "felis files: a protected configuration patch needs inline content and --sha256")
			return 2
		}
		content, err := fileedit.ContentFromEnv(os.LookupEnv)
		if err != nil {
			fmt.Fprintln(stderr, "felis files: invalid protected configuration patch input")
			return 2
		}
		req.Content, req.ContentSHA256 = content, *sum
	case fileedit.OpWrite:
		// The content's SHA-256 comes with it, so bytes that changed on the way
		// to this Job are refused rather than written (Request.ContentSHA256).
		if *sum == "" {
			fmt.Fprintln(stderr, "felis files: a write needs --sha256")
			return 2
		}
		var content []byte
		var err error
		if *sourceURL != "" {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			content, err = fetchWrite(ctx, *sourceURL, os.Getenv(fileedit.UploadTokenEnv), *size)
		} else {
			content, err = fileedit.ContentFromEnv(os.LookupEnv)
		}
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
	// Progress lines let asynchronous upload and extraction Jobs report without blocking API requests.
	req.Progress = fileedit.ThrottledProgress(stdout, time.Second, time.Now)

	res, err := fileedit.Execute(*worldsRoot, req)
	if err != nil {
		// The operation could not be attempted — infrastructure, not caller fault.
		fmt.Fprintf(stderr, "felis files: %v\n", err)
		return 1
	}
	if err := fileedit.Print(stdout, res); err != nil {
		// The result exists but could not be delivered. Exiting non-zero is the only signal left: felis-api would otherwise find no marked line and have to guess why.
		fmt.Fprintf(stderr, "felis files: %v\n", err)
		return 1
	}
	return 0
}

func fetchWrite(ctx context.Context, url, token string, size int64) ([]byte, error) {
	if token == "" || size < 0 || size > fileedit.MaxWriteBytes {
		return nil, fmt.Errorf("staged write has invalid size or token")
	}
	body, err := fetchUpload(ctx, url, token)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	content, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != size {
		return nil, fmt.Errorf("staged write returned %d bytes, expected %d", len(content), size)
	}
	return content, nil
}

// Single-use uploads reject redirects and retain staged data until landing; the Job deadline bounds transfer time.
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

// reportLanded tells felis-api the upload landed (DELETE with the same token), so it drops the staged copy. One try: the file has landed whatever the answer.
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
