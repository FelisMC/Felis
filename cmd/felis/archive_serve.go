package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"felis.lolicon.best/internal/archivetransfer"
)

func cmdArchiveServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("archive-serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "/backups", "archive PVC mount (must match archive.local_path)")
	addr := fs.String("listen", ":8090", "private archive listener")
	limit := fs.Int64("max-bytes", archivetransfer.DefaultLimit, "maximum compressed archive bytes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key := os.Getenv(archivetransfer.KeyEnv)
	if len(key) < 32 || *limit <= 0 {
		fmt.Fprintln(stderr, "archive-serve: signing key and positive size limit required")
		return 2
	}
	if err := os.MkdirAll(*root, 0750); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	limitHeapToCgroup()
	transport := &archivetransfer.Server{Root: *root, Key: key, Limit: *limit}
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			if err := transport.Sweep(time.Now()); err != nil {
				fmt.Fprintln(stderr, "archive journal cleanup:", err)
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	srv := &http.Server{Addr: *addr, Handler: transport, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Hour, WriteTimeout: 2 * time.Hour, MaxHeaderBytes: 16 << 10}
	fmt.Fprintln(stdout, "archive transport listening", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
