package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"

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
// below plus, for a write, one environment variable. Every isolation guarantee
// lives in the Pod spec (internal/fileedit/jobspec.go), and the path-containment
// guarantee lives in fileedit.Execute, which resolves the path through os.Root and
// therefore cannot be walked out of the world mount.
//
// Exit status carries a specific meaning that felis-api depends on: a CALLER-fault
// outcome — a path that escapes the root, a file that is missing or too large — is
// a SUCCESSFUL run that prints a Result carrying an error code, so the API can map
// it to a precise 4xx. A non-zero exit means the operation could not be attempted
// at all (the world mount is unreadable, the result unprintable), which the API
// reports as a 500.
func cmdFiles(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	fs.SetOutput(stderr)
	op := fs.String("op", "", "operation: list, read, or write")
	path := fs.String("path", "", "path to operate on, relative to the world root (empty = the root itself)")
	worldsRoot := fs.String("worlds-root", "/data", "mount path of the world PVC; every path resolves under it")
	expect := fs.String("expect-sha256", "", "write only: refuse unless the file's current SHA-256 (hex) is this")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *op == "" {
		fmt.Fprintln(stderr, "felis files: --op is required (list, read, or write)")
		return 2
	}

	// New content arrives base64-encoded in the environment rather than in argv:
	// a process's arguments are world-readable on the node (/proc/<pid>/cmdline),
	// whereas its environment is not, and a config file being written can carry
	// secrets — an RCON password in server.properties is the obvious case. The
	// encoding is what lets arbitrary bytes (CRLF endings, a BOM, a NUL) survive a
	// channel that must be a valid string.
	var content []byte
	if *op == fileedit.OpWrite {
		raw, ok := os.LookupEnv(fileedit.ContentEnv)
		if !ok {
			fmt.Fprintf(stderr, "felis files: a write needs %s in the environment\n", fileedit.ContentEnv)
			return 2
		}
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			fmt.Fprintf(stderr, "felis files: %s is not valid base64: %v\n", fileedit.ContentEnv, err)
			return 2
		}
		content = decoded
	}

	res, err := fileedit.Execute(*worldsRoot, *op, *path, content, *expect)
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
