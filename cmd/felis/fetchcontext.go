package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// cmdFetchContext is the in-Pod entrypoint the build Job's context-fetch
// initContainer runs. It performs one read against the felis-api INTERNAL face —
// the blob the platform stored for a submission — and extracts it into the shared
// emptyDir the Kaniko container then builds from.
//
// Why this exists: the build Pod runs in the build namespace, where it can neither
// mount the control-plane uploads PVC (a PVC does not cross namespaces) nor hold
// object-store credentials, so the API that WROTE the blob is the transport. The
// route is service-token-gated; the token arrives through a namespace-local Secret
// mounted only into this initContainer, never into Kaniko's — so the untrusted
// Dockerfile's build steps have no credential to read (their containers share no
// environment, no PID namespace, and Kaniko itself mounts the context read-only).
//
// The extraction is deliberately paranoid: the tarball is attacker-controlled
// input, so absolute paths, ".." escapes, links, and special files are refused
// rather than sanitized. Kaniko treats the extracted tree as hostile regardless
// (spec §16), but the pod's own filesystem still must not be written outside the
// context directory it was given.
func cmdFetchContext(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("fetch-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "internal-face URL of the submission's build-context tarball")
	out := fs.String("out", "/context", "directory to extract the build context into")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *url == "" {
		fmt.Fprintln(stderr, "felis fetch-context: --url is required")
		return 2
	}
	token := os.Getenv("FELIS_SERVICE_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "felis fetch-context: FELIS_SERVICE_TOKEN is empty — the internal face rejects anonymous reads")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		fmt.Fprintf(stderr, "felis fetch-context: bad --url: %v\n", err)
		return 2
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// No overall client timeout: a legitimate modpack context can be large and the
	// Job's activeDeadlineSeconds is the real bound. The header timeout catches a
	// wedged endpoint without capping a healthy download.
	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: time.Minute}}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "felis fetch-context: GET failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "felis fetch-context: %s\n", resp.Status)
		return 1
	}

	if err := extractTarGz(resp.Body, *out); err != nil {
		fmt.Fprintf(stderr, "felis fetch-context: %v\n", err)
		return 1
	}
	return 0
}

// extractTarGz streams a gzip'd tarball into root, creating directories as
// needed. Every entry is vetted BEFORE anything is written: a path that is
// absolute or escapes root (via ".."), a link (symlink or hardlink), or any
// special file kind aborts the whole extraction. Refusing rather than skipping is
// deliberate — a context that needs one of those constructs is not a context this
// transport carries, and silently dropping entries would build from a corpus the
// submitter did not upload.
func extractTarGz(r io.Reader, root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create context dir: %w", err)
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("context is not a valid gzip tarball: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read context tarball: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		// The zip-slip guard: reject, never rewrite. filepath.Clean collapses any
		// "a/../../b", so these two checks are sufficient once Clean has run.
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("context entry %q escapes the context directory", hdr.Name)
		}
		target := filepath.Join(root, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create %q: %w", name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create parent of %q: %w", name, err)
			}
			mode := os.FileMode(0o644)
			if hdr.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755 // preserve executability (entrypoint scripts), nothing else
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("create %q: %w", name, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write %q: %w", name, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %q: %w", name, err)
			}
		default:
			return fmt.Errorf("context entry %q has unsupported type %q (links and special files are refused)", hdr.Name, string(hdr.Typeflag))
		}
	}
}
