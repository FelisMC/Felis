// Package dbbackup takes and restores logical backups of the control-plane
// PostgreSQL: users, passkeys, account links, server ownership, quotas, audit
// logs and the world_backups index that maps a world archive back to its owner.
// World archives live on their own volume (internal/archive); without this
// database they are files nobody can be matched to.
//
// A backup is one bundle, felis-db-<UTC stamp>-<label>.tar, holding
//
//	MANIFEST.json                   what is in the bundle and each member's sha256
//	db.dump                         pg_dump --format=custom of the felis database
//	state/etc/felis/...             the host state a rebuild needs: secrets.env
//	                                (the DB password, session and forwarding
//	                                secrets, registry tokens), felis.{host,pod}.toml,
//	                                the panel TLS pair
//	k8s/minecraftservers.json       the MinecraftServer objects, when the cluster
//	                                answered (best effort)
//
// plus a felis-db-....tar.sha256 sidecar in sha256sum format, so a copy shipped
// off the host can be checked with `sha256sum -c` before anyone relies on it.
//
// Bundles are written as a hidden .partial and renamed into place after an
// fsync, so a crash or a full disk leaves either a complete bundle or none.
// The dump is proven readable (pg_restore --list) before the bundle counts.
//
// Restore is all-or-nothing: the dump is replayed through psql in a single
// transaction that first drops everything the felis role owns, so a failure
// anywhere leaves the database exactly as it was, and objects a newer schema
// added do not survive to collide with the next `felis migrate up`.
package dbbackup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultDir is where the host keeps its bundles. It is deliberately off the
	// k3s storage tree: `rm -rf /var/lib/rancher` (a k3s reinstall) must not take
	// the database backups with it.
	DefaultDir = "/var/lib/felis/db-backups"
	// DefaultStateDir is the host state directory bootstrap writes.
	DefaultStateDir = "/etc/felis"

	LabelDaily      = "daily"
	LabelPreMigrate = "pre-migrate"
	LabelPreRestore = "pre-restore"
	LabelManual     = "manual"

	manifestEntry = "MANIFEST.json"
	dumpEntry     = "db.dump"
	stateEntry    = "state"
	serversEntry  = "k8s/minecraftservers.json"

	bundlePrefix = "felis-db-"
	bundleExt    = ".tar"
	sumExt       = ".sha256"
	stampLayout  = "20060102T150405Z"
	formatV1     = 1
)

// stateSkip are files in the state directory a bundle leaves out:
// bootstrap.done marks THIS host as installed, and carrying it to a fresh host
// would make the installer treat a first install as an upgrade.
var stateSkip = map[string]bool{"bootstrap.done": true}

var labelRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Tools names the PostgreSQL client binaries. Empty fields take the names on
// PATH; tests point them at fakes.
type Tools struct {
	PGDump, PGRestore, PSQL string
	// Exec, when set, is the argv prefix every tool runs under. The installer's
	// database is a k3s Deployment and the host carries no PostgreSQL client, so
	// the tools run in the database's own container:
	// `k3s kubectl exec -i -n felis deploy/felis-postgres -c postgres --`.
	// kubectl exec carries neither the environment nor files across: the dump
	// comes back on stdout and archives go in on stdin, and the tools connect as
	// Conn instead of the database URL.
	Exec []string
	// Conn is the libpq connection string the tools use under Exec: the
	// container's own socket, which trusts local connections, so no password
	// has to cross into it.
	Conn string
}

func (t Tools) pgDump() string    { return orDefault(t.PGDump, "pg_dump") }
func (t Tools) pgRestore() string { return orDefault(t.PGRestore, "pg_restore") }
func (t Tools) psql() string      { return orDefault(t.PSQL, "psql") }

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// BackupOptions configures one backup.
type BackupOptions struct {
	DatabaseURL string
	Dir         string // bundle directory; created 0700
	Label       string // daily | pre-migrate | pre-restore | manual | any [a-z0-9-]
	// Keep is how many bundles of this label survive the post-backup prune;
	// zero or less prunes nothing.
	Keep     int
	StateDir string // host state to bundle; "" bundles none
	Version  string // felis build stamp, recorded in the manifest
	Tools    Tools
	// ExportServers returns the cluster's MinecraftServer objects as JSON. A
	// failure is recorded in the manifest and does not fail the backup: the
	// database is what must not be lost, and a nightly run cannot hang on a
	// cluster that happens to be down.
	ExportServers func(ctx context.Context) ([]byte, error)
	// MetricsFile, when set, is rewritten after a successful backup with
	// node-exporter textfile metrics (felis_db_backup_last_success_timestamp_seconds
	// and felis_db_backup_last_size_bytes), which FelisDBBackupStale alerts on.
	MetricsFile string
	// Record stores a summary of the backup in platform_settings under
	// StatusKey, which the admin panel reads to show how fresh the newest
	// backup is. A failure to record is logged, not fatal.
	Record bool
	Now    func() time.Time
	Log    io.Writer
}

// StatusKey is the platform_settings key Record writes; internal/api reads it.
const StatusKey = "db_backup_last"

// StaleAfter is how old the newest backup may get before it counts as missed:
// a day plus the timer's randomized delay and a slow dump. `felis db check`,
// the admin panel and the FelisDBBackupStale alert (deploy/alerts) share it.
const StaleAfter = 26 * time.Hour

// Status is the value stored under StatusKey.
type Status struct {
	At            time.Time `json:"at"`
	Name          string    `json:"name"`
	Label         string    `json:"label"`
	SizeBytes     int64     `json:"size_bytes"`
	FelisVersion  string    `json:"felis_version,omitempty"`
	SchemaVersion int       `json:"schema_version,omitempty"`
	Dir           string    `json:"dir"`
}

// Manifest describes a bundle.
type Manifest struct {
	Format        int             `json:"format"`
	CreatedAt     time.Time       `json:"created_at"`
	Label         string          `json:"label"`
	FelisVersion  string          `json:"felis_version,omitempty"`
	Database      DatabaseInfo    `json:"database"`
	SchemaVersion int             `json:"schema_version,omitempty"`
	PGDumpVersion string          `json:"pg_dump_version,omitempty"`
	Files         []ManifestEntry `json:"files"`
	// ServersError is why k8s/minecraftservers.json is absent, when it is.
	ServersError string `json:"servers_error,omitempty"`
}

// DatabaseInfo is the connection a bundle was taken from, password excluded.
type DatabaseInfo struct {
	Host string `json:"host"`
	Port string `json:"port,omitempty"`
	Name string `json:"name"`
	User string `json:"user"`
}

// ManifestEntry is one bundle member.
type ManifestEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"` // absent for symlinks
	Mode   uint32 `json:"mode"`
	Link   string `json:"link,omitempty"`
}

// Bundle is one bundle on disk.
type Bundle struct {
	Name    string
	Path    string
	Label   string
	Created time.Time
	Size    int64
}

// conn splits a postgres:// URL into the URL libpq should see (password
// removed) and the password, which goes to the child through PGPASSWORD so it
// never shows up in ps.
type conn struct {
	uri      string
	password string
	info     DatabaseInfo
}

func parseConn(raw string) (conn, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return conn{}, errors.New("database url must be a postgres:// URL")
	}
	c := conn{info: DatabaseInfo{Host: u.Hostname(), Port: u.Port(), Name: strings.TrimPrefix(u.Path, "/")}}
	if u.User != nil {
		c.info.User = u.User.Username()
		c.password, _ = u.User.Password()
		u.User = url.User(c.info.User)
	}
	if c.info.Name == "" {
		return conn{}, errors.New("database url names no database")
	}
	c.uri = u.String()
	return c, nil
}

func (c conn) env() []string {
	env := os.Environ()
	if c.password != "" {
		env = append(env, "PGPASSWORD="+c.password)
	}
	// Never prompt: a timer-driven run with a wrong password must fail, not hang.
	return append(env, "PGCONNECT_TIMEOUT=15")
}

// toolCmd is one run of a client tool, named for errors by the tool itself:
// under Tools.Exec the process is kubectl, which says nothing.
type toolCmd struct {
	*exec.Cmd
	name string
}

// command runs tool with args, directly or under t.Exec.
func (t Tools) command(ctx context.Context, c conn, tool string, args ...string) toolCmd {
	if len(t.Exec) > 0 {
		argv := append(append(append([]string{}, t.Exec[1:]...), tool), args...)
		return toolCmd{exec.CommandContext(ctx, t.Exec[0], argv...), filepath.Base(tool)}
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = c.env()
	return toolCmd{cmd, filepath.Base(tool)}
}

// dsn is what the tools pass as --dbname / -d.
func (t Tools) dsn(c conn) string {
	if len(t.Exec) > 0 {
		return t.Conn
	}
	return c.uri
}

// run executes cmd and folds its stderr into the error.
func run(cmd toolCmd) ([]byte, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return out, fmt.Errorf("%s: %w", cmd.name, err)
		}
		return out, fmt.Errorf("%s: %w: %s", cmd.name, err, msg)
	}
	return out, nil
}

// foreignObjectRe finds the object pg_dump could not read, and its kind.
var foreignObjectRe = regexp.MustCompile(`permission denied for (table|sequence|schema|view|materialized view) ([^\s]+)`)

// dumpHint explains the one pg_dump failure an operator causes without noticing:
// an object created in the felis database by another role (typically postgres,
// from a manual psql session). The dump runs as the felis role and must read
// everything; leaving the object out would make the bundle an incomplete restore.
func dumpHint(err error, db DatabaseInfo, t Tools) string {
	m := foreignObjectRe.FindStringSubmatch(err.Error())
	if m == nil {
		return ""
	}
	// The superuser's psql: the host's postgres account, or the image's
	// postgres role over the container's socket.
	su := "sudo -u postgres psql"
	if len(t.Exec) > 0 {
		su = "sudo " + strings.Join(t.Exec, " ") + " psql -U postgres"
	}
	kind, name := strings.ToUpper(m[1]), m[2]
	return fmt.Sprintf("\n  %s %s belongs to another role, so %s cannot dump it. Hand it over with\n"+
		"    %s -d %s -c 'ALTER %s %s OWNER TO %s'\n"+
		"  or drop it if it is a leftover.", strings.ToLower(kind), name, db.User, su, db.Name, kind, name, db.User)
}

// BundleName is the file name of a bundle taken at t with label.
func BundleName(t time.Time, label string) string {
	return bundlePrefix + t.UTC().Format(stampLayout) + "-" + label + bundleExt
}

// ParseBundleName reverses BundleName: the stamp and label of a bundle file name.
func ParseBundleName(name string) (time.Time, string, bool) {
	if !strings.HasPrefix(name, bundlePrefix) || !strings.HasSuffix(name, bundleExt) {
		return time.Time{}, "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, bundlePrefix), bundleExt)
	if len(rest) < len(stampLayout)+2 || rest[len(stampLayout)] != '-' {
		return time.Time{}, "", false
	}
	t, err := time.Parse(stampLayout, rest[:len(stampLayout)])
	label := rest[len(stampLayout)+1:]
	if err != nil || !labelRe.MatchString(label) {
		return time.Time{}, "", false
	}
	return t, label, true
}

// List returns the bundles in dir, newest first. A missing dir is no bundles.
func List(dir string) ([]Bundle, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Bundle
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		t, label, ok := ParseBundleName(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Bundle{Name: e.Name(), Path: filepath.Join(dir, e.Name()), Label: label, Created: t, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

// Prune deletes all but the newest keep bundles of label (and their sidecars)
// and returns what it removed. keep <= 0 removes nothing.
func Prune(dir, label string, keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	all, err := List(dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	n := 0
	for _, b := range all {
		if b.Label != label {
			continue
		}
		if n++; n <= keep {
			continue
		}
		if err := os.Remove(b.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		_ = os.Remove(b.Path + sumExt)
		removed = append(removed, b.Name)
	}
	return removed, nil
}

// lockDir serializes bundle writers (the nightly timer, a pre-migrate snapshot
// and an operator's manual run) on dir/.lock.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", dir, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// removeStalePartials drops leftovers of a writer that died mid-bundle. Only
// called under the directory lock, so no live writer owns them.
func removeStalePartials(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+bundlePrefix) && strings.HasSuffix(e.Name(), ".partial") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Backup writes one bundle and returns its path.
func Backup(ctx context.Context, o BackupOptions) (string, error) {
	if !labelRe.MatchString(o.Label) {
		return "", fmt.Errorf("invalid label %q (want [a-z0-9-], e.g. daily or manual)", o.Label)
	}
	c, err := parseConn(o.DatabaseURL)
	if err != nil {
		return "", err
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	logw := o.Log
	if logw == nil {
		logw = io.Discard
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(o.Dir, 0o700); err != nil {
		return "", err
	}
	unlock, err := lockDir(o.Dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	removeStalePartials(o.Dir)

	// Names have one-second resolution. A second bundle of the same label within
	// that second (a restore retried right after a failed one takes two
	// pre-restore snapshots) moves to the next free second; the lock makes the
	// probe race-free.
	created := now().UTC().Truncate(time.Second)
	name := BundleName(created, o.Label)
	for i := 0; ; i++ {
		if _, err := os.Lstat(filepath.Join(o.Dir, name)); errors.Is(err, fs.ErrNotExist) {
			break
		} else if err != nil {
			return "", err
		}
		if i == 60 {
			return "", fmt.Errorf("no free bundle name after %s", name)
		}
		created = created.Add(time.Second)
		name = BundleName(created, o.Label)
	}
	final := filepath.Join(o.Dir, name)

	dump := filepath.Join(o.Dir, "."+name+".dump.partial")
	defer os.Remove(dump)
	if err := dumpTo(ctx, c, o.Tools, dump); err != nil {
		return "", err
	}
	// A dump pg_restore cannot read is not a backup; find out now, not on the
	// day it is needed.
	if err := listArchive(ctx, c, o.Tools, dump); err != nil {
		return "", fmt.Errorf("the dump does not read back: %w", err)
	}

	m := Manifest{Format: formatV1, CreatedAt: created, Label: o.Label, FelisVersion: o.Version, Database: c.info}
	if out, err := run(o.Tools.command(ctx, c, o.Tools.pgDump(), "--version")); err == nil {
		m.PGDumpVersion = strings.TrimSpace(string(out))
	}
	m.SchemaVersion = schemaVersion(ctx, c, o.Tools)

	var members []member
	dm, err := fileMember(dumpEntry, dump)
	if err != nil {
		return "", err
	}
	members = append(members, dm)
	if o.StateDir != "" {
		sm, err := stateMembers(o.StateDir)
		if err != nil {
			return "", fmt.Errorf("read host state: %w", err)
		}
		members = append(members, sm...)
	}
	if o.ExportServers != nil {
		if data, err := o.ExportServers(ctx); err != nil {
			m.ServersError = err.Error()
			fmt.Fprintf(logw, "felis db backup: MinecraftServer objects not included: %v\n", err)
		} else {
			members = append(members, bytesMember(serversEntry, data))
		}
	}
	for _, mb := range members {
		m.Files = append(m.Files, mb.entry)
	}

	sum, err := writeBundle(o.Dir, final, m, members)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(final+sumExt, []byte(sum+"  "+name+"\n")); err != nil {
		return "", fmt.Errorf("write checksum: %w", err)
	}
	if info, err := os.Stat(final); err == nil {
		st := Status{At: created, Name: name, Label: o.Label, SizeBytes: info.Size(),
			FelisVersion: o.Version, SchemaVersion: m.SchemaVersion, Dir: o.Dir}
		if o.Record {
			if err := record(ctx, c, o.Tools, st); err != nil {
				fmt.Fprintf(logw, "felis db backup: record the backup for the panel: %v\n", err)
			}
		}
		if o.MetricsFile != "" {
			if err := writeMetrics(o.MetricsFile, st); err != nil {
				fmt.Fprintf(logw, "felis db backup: write %s: %v\n", o.MetricsFile, err)
			}
		}
	}
	if removed, err := Prune(o.Dir, o.Label, o.Keep); err != nil {
		fmt.Fprintf(logw, "felis db backup: prune old %s bundles: %v\n", o.Label, err)
	} else if len(removed) > 0 {
		fmt.Fprintf(logw, "felis db backup: pruned %d old %s bundle(s)\n", len(removed), o.Label)
	}
	return final, nil
}

// dumpTo writes pg_dump's custom-format archive of the database to path,
// created 0600. The archive travels on stdout, the one channel that reaches
// the host from a tool running under Tools.Exec.
func dumpTo(ctx context.Context, c conn, t Tools, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cmd := t.command(ctx, c, t.pgDump(), "--format=custom", "--no-password", "--dbname="+t.dsn(c))
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = f, &stderr
	err = cmd.Run()
	if cerr := f.Close(); err == nil && cerr != nil {
		return cerr
	}
	if err != nil {
		err = fmt.Errorf("%s: %w", cmd.name, err)
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return fmt.Errorf("dump the database: %w%s", err, dumpHint(err, c.info, t))
	}
	return nil
}

// listArchive proves pg_restore can read the archive at path, fed on stdin.
func listArchive(ctx context.Context, c conn, t Tools, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := t.command(ctx, c, t.pgRestore(), "--list")
	cmd.Stdin = f
	_, err = run(cmd)
	return err
}

// record upserts st into platform_settings. The JSON travels as a psql
// variable, quoted by psql itself, over stdin (-c does not interpolate).
func record(ctx context.Context, c conn, t Tools, st Status) error {
	v, err := json.Marshal(st)
	if err != nil {
		return err
	}
	cmd := t.command(ctx, c, t.psql(), "-X", "-q", "-w", "-v", "ON_ERROR_STOP=1", "-v", "v="+string(v), "-d", t.dsn(c))
	cmd.Stdin = strings.NewReader("INSERT INTO platform_settings (key, value) VALUES ('" + StatusKey + "', :'v'::jsonb)\n" +
		"ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();\n")
	_, err = run(cmd)
	return err
}

// writeMetrics rewrites a node-exporter textfile-collector file for st.
func writeMetrics(path string, st Status) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(`# HELP felis_db_backup_last_success_timestamp_seconds Unix time of the newest successful control-plane database backup.
# TYPE felis_db_backup_last_success_timestamp_seconds gauge
felis_db_backup_last_success_timestamp_seconds{label=%q} %d
# HELP felis_db_backup_last_size_bytes Size of the newest control-plane database backup bundle.
# TYPE felis_db_backup_last_size_bytes gauge
felis_db_backup_last_size_bytes{label=%q} %d
`, st.Label, st.At.Unix(), st.Label, st.SizeBytes)
	if err := writeFileAtomic(path, []byte(body)); err != nil {
		return err
	}
	// Read by node-exporter, which usually runs unprivileged.
	return os.Chmod(path, 0o644)
}

// schemaVersion reads the newest applied migration, or 0 when it cannot.
func schemaVersion(ctx context.Context, c conn, t Tools) int {
	out, err := run(t.command(ctx, c, t.psql(), "-X", "-q", "-t", "-A", "-w", "-d", t.dsn(c),
		"-c", "SELECT coalesce(max(version), 0) FROM schema_migrations"))
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return v
}

// member is one bundle entry: a file on disk, bytes, or a symlink.
type member struct {
	entry ManifestEntry
	path  string
	data  []byte
}

func fileMember(name, path string) (member, error) {
	f, err := os.Open(path)
	if err != nil {
		return member{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return member{}, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return member{}, err
	}
	return member{entry: ManifestEntry{Name: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), Mode: uint32(info.Mode().Perm())}, path: path}, nil
}

func bytesMember(name string, data []byte) member {
	s := sha256.Sum256(data)
	return member{entry: ManifestEntry{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(s[:]), Mode: 0o600}, data: data}
}

// stateMembers bundles the regular files and symlinks directly in dir, under
// state/<absolute dir>/. Subdirectories are not state bootstrap writes.
func stateMembers(dir string) ([]member, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	prefix := stateEntry + filepath.ToSlash(abs) + "/"
	var out []member
	for _, e := range entries {
		if stateSkip[e.Name()] {
			continue
		}
		p := filepath.Join(abs, e.Name())
		switch {
		case e.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return nil, err
			}
			out = append(out, member{entry: ManifestEntry{Name: prefix + e.Name(), Mode: 0o777, Link: target}})
		case e.Type().IsRegular():
			m, err := fileMember(prefix+e.Name(), p)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// writeBundle writes MANIFEST.json and the members to final via a .partial and
// returns the bundle's sha256.
func writeBundle(dir, final string, m Manifest, members []member) (string, error) {
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	partial := filepath.Join(dir, "."+filepath.Base(final)+".partial")
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer os.Remove(partial)
	h := sha256.New()
	if err := writeTar(io.MultiWriter(f, h), m.CreatedAt, manifest, members); err != nil {
		f.Close()
		return "", fmt.Errorf("write bundle: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(partial, final); err != nil {
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	partial := filepath.Join(dir, "."+filepath.Base(path)+".partial")
	defer os.Remove(partial)
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(partial, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Age renders a bundle's age for a human: seconds under a minute, then
// minutes, then days and hours past two days.
func Age(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return d.Truncate(time.Second).String()
	case d < 48*time.Hour:
		return strings.TrimSuffix(d.Truncate(time.Minute).String(), "0s")
	default:
		return fmt.Sprintf("%dd%dh", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	}
}

// Check reports the newest bundle in dir and an error when there is none or it
// is older than maxAge.
func Check(dir string, maxAge time.Duration, now time.Time) (*Bundle, error) {
	all, err := List(dir)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no database backup in %s", dir)
	}
	newest := all[0]
	if age := now.Sub(newest.Created); age > maxAge {
		return &newest, fmt.Errorf("newest database backup %s is %s old (limit %s)", newest.Name, Age(age), maxAge)
	}
	return &newest, nil
}
