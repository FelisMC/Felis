package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"felis.lolicon.best/internal/build"
)

// cmdFetchContext is the in-Pod entrypoint the build Job's context-fetch
// initContainer runs. It reads the blob the platform stored for a submission
// from the felis-api INTERNAL face (with a bounded retry — see
// fetchContextWithRetry) and extracts it into the shared emptyDir the Kaniko
// container then builds from.
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
	want := fs.String("sha256", "", "refuse the context unless the tarball's sha256 is this lowercase hex digest")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *want != "" && !build.IsSHA256Hex(*want) {
		fmt.Fprintf(stderr, "felis fetch-context: --sha256 %q is not a lowercase hex sha256\n", *want)
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

	// Validate the URL once up front: a bad one is a usage error (2), not
	// something to sit in the retry loop.
	if _, err := http.NewRequest(http.MethodGet, *url, nil); err != nil {
		fmt.Fprintf(stderr, "felis fetch-context: bad --url: %v\n", err)
		return 2
	}
	// No overall client timeout: a legitimate modpack context can be large and the
	// Job's activeDeadlineSeconds is the real bound. The header timeout catches a
	// wedged endpoint without capping a healthy download.
	// Redirects are refused: the request carries the service token, and the
	// internal face never redirects, so a 3xx is someone steering the token.
	client := &http.Client{
		Transport:     &http.Transport{ResponseHeaderTimeout: time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := fetchContextWithRetry(ctx, client, *url, token, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "felis fetch-context: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	h := sha256.New()
	body := io.TeeReader(resp.Body, h)
	if err := extractTarGz(body, *out); err != nil {
		fmt.Fprintf(stderr, "felis fetch-context: %v\n", err)
		return 1
	}
	if *want == "" {
		return 0
	}
	// The tar end marker comes before the gzip trailer and whatever follows it,
	// so read to EOF: the digest must cover every byte the blob holds. The blob
	// itself is size-capped at upload, which bounds this read.
	if _, err := io.Copy(io.Discard, io.LimitReader(body, maxContextBytes)); err != nil {
		fmt.Fprintf(stderr, "felis fetch-context: %v\n", err)
		return 1
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != *want {
		// The init container failing is what keeps Kaniko from ever starting on
		// the extracted tree.
		fmt.Fprintf(stderr, "felis fetch-context: the context's sha256 is %s, the approved digest is %s: it changed after approval; refusing to build\n", got, *want)
		return 1
	}
	return 0
}

// fetchRetryInterval/fetchRetryWindow bound how long the fetch waits out a
// control-plane blip before giving up. The api pod being replaced is a normal
// event (rollout, eviction, a chaos drill), and without a retry one refused
// dial turns it into a failed build: BackoffLimit=0 gives the Job no second
// Pod, so the terminal verdict costs a manual re-approval — the live drill hit
// exactly this (context-fetch exit 1 on `connect: connection refused` while
// the api pod rolled; the new pod was serving 11 seconds later and the same
// 198-byte blob). The window is tiny next to the Job's 30-minute
// activeDeadline; a 4xx (missing blob, rejected token) still fails fast.
//
// Vars, not consts, so tests can shrink the window.
var (
	fetchRetryInterval = 3 * time.Second
	fetchRetryWindow   = 45 * time.Second
)

// fetchContextWithRetry GETs the context tarball, retrying transport failures
// and 5xx responses until fetchRetryWindow runs out. A 4xx is an answer, not a
// blip — retrying it only delays the honest error.
func fetchContextWithRetry(ctx context.Context, client *http.Client, url, token string, stderr io.Writer) (*http.Response, error) {
	deadline := time.Now().Add(fetchRetryWindow)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("bad --url: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if err == nil {
			status := resp.Status
			_ = resp.Body.Close()
			err = fmt.Errorf("GET returned %s", status)
			if resp.StatusCode < 500 {
				return nil, err
			}
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("GET failed: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("GET failed (retried for %s): %w", fetchRetryWindow, err)
		}
		fmt.Fprintf(stderr, "felis fetch-context: %v; retrying (the internal face may be restarting)\n", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("GET failed: %w", err)
		case <-time.After(fetchRetryInterval):
		}
	}
}

// maxContextBytes / maxContextEntries bound what one context may expand to. The
// compressed upload is capped at 1 GiB, but gzip turns that into hundreds of GiB
// or millions of empty files, and the emptyDir's 4 GiB sizeLimit is only
// enforced by the kubelet's periodic sweep, after the disk has filled. The byte
// cap matches that sizeLimit; the entry cap is far above any real modpack (a
// large one is a few thousand files) and far below an inode exhaustion.
//
// Vars, not consts, so tests can shrink them.
var (
	maxContextBytes   int64 = 4 << 30
	maxContextEntries       = 200_000
)

// extractTarGz streams a gzip'd tarball into root, creating directories as
// needed. Every entry is vetted BEFORE anything is written: a path that is
// absolute or escapes root (via ".."), a link (symlink or hardlink), or any
// special file kind aborts the whole extraction. Refusing rather than skipping is
// deliberate — a context that needs one of those constructs is not a context this
// transport carries, and silently dropping entries would build from a corpus the
// submitter did not upload. The whole extraction is also bounded by
// maxContextBytes and maxContextEntries.
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
	var written int64
	entries := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read context tarball: %w", err)
		}
		if entries++; entries > maxContextEntries {
			return fmt.Errorf("the build context has more than %d entries", maxContextEntries)
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
		case tar.TypeReg:
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
			n, err := io.Copy(f, io.LimitReader(tr, maxContextBytes-written+1))
			written += n
			if err != nil {
				_ = f.Close()
				return fmt.Errorf("write %q: %w", name, err)
			}
			if written > maxContextBytes {
				_ = f.Close()
				return fmt.Errorf("the build context expands past %d bytes", maxContextBytes)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %q: %w", name, err)
			}
		default:
			return fmt.Errorf("context entry %q has unsupported type %q (links and special files are refused)", hdr.Name, string(hdr.Typeflag))
		}
	}
}
