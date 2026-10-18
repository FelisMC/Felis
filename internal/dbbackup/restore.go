package dbbackup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func writeTar(w io.Writer, mtime time.Time, manifest []byte, members []member) error {
	tw := tar.NewWriter(w)
	hdr := func(name string, mode uint32, size int64) *tar.Header {
		return &tar.Header{Name: name, Mode: int64(mode), Size: size, ModTime: mtime, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	}
	if err := tw.WriteHeader(hdr(manifestEntry, 0o600, int64(len(manifest)))); err != nil {
		return err
	}
	if _, err := tw.Write(manifest); err != nil {
		return err
	}
	for _, m := range members {
		if m.entry.Link != "" {
			if err := tw.WriteHeader(&tar.Header{Name: m.entry.Name, Linkname: m.entry.Link, Mode: 0o777,
				ModTime: mtime, Typeflag: tar.TypeSymlink, Format: tar.FormatPAX}); err != nil {
				return err
			}
			continue
		}
		if err := tw.WriteHeader(hdr(m.entry.Name, m.entry.Mode, m.entry.Size)); err != nil {
			return err
		}
		if m.data != nil {
			if _, err := tw.Write(m.data); err != nil {
				return err
			}
			continue
		}
		f, err := os.Open(m.path)
		if err != nil {
			return err
		}
		// CopyN rejects files shortened since the hashing pass wrote their size.
		_, err = io.CopyN(tw, f, m.entry.Size)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", m.entry.Name, err)
		}
	}
	return tw.Close()
}

// Verify checks the sidecar when present and every member against the manifest.
func Verify(path string) (Manifest, error) {
	return readBundle(path, nil)
}

var errNotBundle = fmt.Errorf("not a felis database bundle (no %s)", manifestEntry)

// ReadManifest reads only the leading manifest; Verify validates the members.
func ReadManifest(r io.Reader) (Manifest, error) {
	return readManifest(tar.NewReader(r))
}

func readManifest(tr *tar.Reader) (Manifest, error) {
	var m Manifest
	first, err := tr.Next()
	switch {
	case err == nil && first.Name == manifestEntry:
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, tar.ErrHeader):
		return m, errNotBundle
	default:
		return m, err
	}
	raw, err := io.ReadAll(io.LimitReader(tr, 1<<20))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("read %s: %w", manifestEntry, err)
	}
	if m.Format != formatV1 {
		return m, fmt.Errorf("bundle format %d is not one this felis reads (want %d)", m.Format, formatV1)
	}
	return m, nil
}

func readBundle(path string, dumpTo io.Writer) (Manifest, error) {
	var m Manifest
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	whole := sha256.New()
	tr := tar.NewReader(io.TeeReader(f, whole))

	m, err = readManifest(tr)
	if errors.Is(err, errNotBundle) {
		return m, fmt.Errorf("%s is not a felis database bundle (no %s)", filepath.Base(path), manifestEntry)
	}
	if err != nil {
		return m, err
	}
	want := map[string]ManifestEntry{}
	for _, e := range m.Files {
		want[e.Name] = e
	}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("read bundle: %w", err)
		}
		e, ok := want[h.Name]
		if !ok {
			return m, fmt.Errorf("bundle member %s is not in the manifest", h.Name)
		}
		seen[h.Name] = true
		if h.Typeflag == tar.TypeSymlink {
			if h.Linkname != e.Link {
				return m, fmt.Errorf("bundle member %s links to %q, manifest says %q", h.Name, h.Linkname, e.Link)
			}
			continue
		}
		dst := io.Discard
		if h.Name == dumpEntry && dumpTo != nil {
			dst = dumpTo
		}
		sum := sha256.New()
		n, err := io.Copy(io.MultiWriter(dst, sum), tr)
		if err != nil {
			return m, fmt.Errorf("read %s: %w", h.Name, err)
		}
		if n != e.Size || hex.EncodeToString(sum.Sum(nil)) != e.SHA256 {
			return m, fmt.Errorf("bundle member %s is corrupt (size or sha256 differs from the manifest)", h.Name)
		}
	}
	for name := range want {
		if !seen[name] {
			return m, fmt.Errorf("bundle is missing %s", name)
		}
	}
	if _, ok := want[dumpEntry]; !ok {
		return m, fmt.Errorf("bundle holds no %s", dumpEntry)
	}
	// The tar end marker is not the end of the file; the sidecar covers every byte.
	if _, err := io.Copy(io.Discard, io.TeeReader(f, whole)); err != nil {
		return m, err
	}
	if sidecar, err := os.ReadFile(path + sumExt); err == nil {
		fields := strings.Fields(string(sidecar))
		if len(fields) == 0 || fields[0] != hex.EncodeToString(whole.Sum(nil)) {
			return m, fmt.Errorf("%s does not match %s%s", filepath.Base(path), filepath.Base(path), sumExt)
		}
	}
	return m, nil
}

type RestoreOptions struct {
	DatabaseURL string
	Bundle      string
	// Dir holds the scratch dump and safety backup.
	Dir              string
	Force            bool
	SkipSafetyBackup bool
	Safety           BackupOptions
	Tools            Tools
	Log              io.Writer
}

// ErrClientsConnected prevents restore while other database clients are connected.
var ErrClientsConnected = errors.New("other clients are connected to the database")

// Restore replaces database contents atomically and returns the manifest and optional safety-bundle path.
func Restore(ctx context.Context, o RestoreOptions) (Manifest, string, error) {
	c, err := parseConn(o.DatabaseURL)
	if err != nil {
		return Manifest{}, "", err
	}
	logw := o.Log
	if logw == nil {
		logw = io.Discard
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return Manifest{}, "", err
	}
	scratch, err := os.CreateTemp(o.Dir, ".restore-*.dump")
	if err != nil {
		return Manifest{}, "", err
	}
	defer os.Remove(scratch.Name())
	m, err := readBundle(o.Bundle, scratch)
	if cerr := scratch.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return m, "", err
	}
	if err := listArchive(ctx, c, o.Tools, scratch.Name()); err != nil {
		return m, "", fmt.Errorf("the bundle's dump does not read: %w", err)
	}

	if !o.Force {
		n, err := otherClients(ctx, c, o.Tools)
		if err != nil {
			return m, "", fmt.Errorf("count connected clients: %w", err)
		}
		if n > 0 {
			return m, "", fmt.Errorf("%w (%d); scale felis-api and felis-operator to 0 first, or pass -force", ErrClientsConnected, n)
		}
	}

	var safety string
	if !o.SkipSafetyBackup {
		so := o.Safety
		so.DatabaseURL, so.Dir, so.Label, so.Tools = o.DatabaseURL, o.Dir, LabelPreRestore, o.Tools
		if so.Log == nil {
			so.Log = logw
		}
		// A safety bundle without cluster objects still covers everything this database-only restore replaces.
		if safety, err = Backup(ctx, so); err != nil && !errors.Is(err, ErrServersMissing) {
			return m, "", fmt.Errorf("safety backup of the current database: %w (pass -no-safety-backup to restore without one)", err)
		}
		fmt.Fprintf(logw, "felis db restore: current database saved to %s\n", safety)
	}

	if err := replay(ctx, c, o.Tools, scratch.Name()); err != nil {
		return m, safety, err
	}
	// The restored dump has an older freshness record; update it to the newest on-disk bundle.
	if err := recordNewest(ctx, c, o.Tools, o.Dir); err != nil {
		fmt.Fprintf(logw, "felis db restore: record the newest backup for the panel: %v\n", err)
	}
	return m, safety, nil
}

func recordNewest(ctx context.Context, c conn, t Tools, dir string) error {
	all, err := List(dir)
	if err != nil || len(all) == 0 {
		return err
	}
	b := all[0]
	st := Status{At: b.Created, Name: b.Name, Label: b.Label, SizeBytes: b.Size, Dir: dir}
	if m, err := Verify(b.Path); err == nil {
		st.FelisVersion, st.SchemaVersion, st.ServersError = m.FelisVersion, m.SchemaVersion, m.ServersError
	}
	return record(ctx, c, t, st)
}

func otherClients(ctx context.Context, c conn, t Tools) (int, error) {
	out, err := run(t.command(ctx, c, t.psql(), "-X", "-q", "-t", "-A", "-w", "-d", t.dsn(c), "-c",
		"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type = 'client backend'"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

const dropOwned = "DROP OWNED BY CURRENT_USER;\n"

// Emit COMMIT only after pg_restore succeeds; producer failure must roll back the partial stream.
// pg_restore reads the archive on stdin, sequentially, the only way in under Tools.Exec.
func replay(ctx context.Context, c conn, t Tools, dump string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	in, err := os.Open(dump)
	if err != nil {
		return err
	}
	defer in.Close()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	var restoreErr, psqlErr bytes.Buffer
	gen := t.command(ctx, c, t.pgRestore(), "--no-owner", "--no-privileges", "--file=-")
	gen.Stdin, gen.Stdout, gen.Stderr = in, w, &restoreErr
	if err := gen.Start(); err != nil {
		r.Close()
		w.Close()
		return fmt.Errorf("pg_restore: %w", err)
	}
	w.Close()

	tail := &commitAfter{gen: gen.Cmd}
	apply := t.command(ctx, c, t.psql(), "-X", "-q", "-w", "-v", "ON_ERROR_STOP=1", "-d", t.dsn(c))
	apply.Stdin = io.MultiReader(strings.NewReader("BEGIN;\n"+dropOwned), r, tail)
	apply.Stdout, apply.Stderr = io.Discard, &psqlErr
	aerr := apply.Run()
	// Close the read end to unblock a producer after early psql failure and reap it once.
	r.Close()
	gerr := tail.wait()
	if aerr == nil {
		return nil
	}
	msg := "replay the dump (rolled back, the database is unchanged)"
	if s := strings.TrimSpace(psqlErr.String()); s != "" {
		msg += ": psql: " + s
	}
	if s := strings.TrimSpace(restoreErr.String()); gerr != nil && s != "" {
		msg += ": pg_restore: " + s
	}
	return fmt.Errorf("%s: %w", msg, aerr)
}

// commitAfter emits COMMIT only after pg_restore exits successfully.
type commitAfter struct {
	gen       *exec.Cmd
	waited    bool
	err       error
	committed bool
	rest      []byte
}

func (c *commitAfter) wait() error {
	if !c.waited {
		c.waited, c.err = true, c.gen.Wait()
	}
	return c.err
}

func (c *commitAfter) Read(p []byte) (int, error) {
	if !c.committed {
		if err := c.wait(); err != nil {
			return 0, err
		}
		c.committed, c.rest = true, []byte("COMMIT;\n")
	}
	if len(c.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}
