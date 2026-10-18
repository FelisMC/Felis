// Package dbbackup takes and restores logical backups of the control-plane PostgreSQL as tar bundles.
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
	DefaultDir      = "/var/lib/felis/db-backups" // kept off the k3s storage tree so a k3s reinstall does not delete it
	DefaultStateDir = "/etc/felis"

	LabelDaily      = "daily"
	LabelPreMigrate = "pre-migrate"
	LabelPreRestore = "pre-restore"
	LabelManual     = "manual"
	LabelOffsite    = "offsite"

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

// stateSkip lists state files left out of a bundle; bootstrap.done would make a fresh host look installed.
var stateSkip = map[string]bool{"bootstrap.done": true}

var labelRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Tools names the PostgreSQL client binaries; empty fields use the names on PATH.
type Tools struct {
	PGDump, PGRestore, PSQL string
	// Exec is the argv prefix every tool runs under (kubectl exec into the database container); dumps and archives travel over stdout/stdin.
	Exec []string
	// Conn is the libpq connection string used under Exec; the container socket trusts local connections.
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

type BackupOptions struct {
	DatabaseURL string
	Dir         string // Created with mode 0700.
	Label       string // daily | pre-migrate | pre-restore | manual | any [a-z0-9-]
	Keep        int    // bundles of this label kept after the prune; <= 0 prunes nothing
	StateDir    string // host state to bundle; "" bundles none
	Version     string
	Tools       Tools
	// ExportServers failure still writes a bundle and returns its path with ErrServersMissing.
	ExportServers func(ctx context.Context) ([]byte, error)
	// RequireServers fails the backup before writing a bundle when ExportServers fails.
	RequireServers bool
	// MetricsFile, when set, is rewritten after a successful backup with node-exporter textfile metrics.
	MetricsFile string
	// Record stores a summary under StatusKey in platform_settings; a failure to record is logged, not fatal.
	Record bool
	Now    func() time.Time
	Log    io.Writer
}

const StatusKey = "db_backup_last"

// ErrServersMissing is returned with the bundle path when ExportServers failed every try.
var ErrServersMissing = errors.New("the bundle holds the database but not the MinecraftServer objects")

// StaleAfter is the age after which the newest daily backup counts as missed; keep in sync with the FelisDBBackupStale alert.
const StaleAfter = 26 * time.Hour

type Status struct {
	At            time.Time `json:"at"`
	Name          string    `json:"name"`
	Label         string    `json:"label"`
	SizeBytes     int64     `json:"size_bytes"`
	FelisVersion  string    `json:"felis_version,omitempty"`
	SchemaVersion int       `json:"schema_version,omitempty"`
	Dir           string    `json:"dir"`
	ServersError  string    `json:"servers_error,omitempty"`
	DailyAt       time.Time `json:"daily_at,omitzero"` // newest daily bundle in Dir; zero when none
}

type Manifest struct {
	Format        int             `json:"format"`
	CreatedAt     time.Time       `json:"created_at"`
	Label         string          `json:"label"`
	FelisVersion  string          `json:"felis_version,omitempty"`
	Database      DatabaseInfo    `json:"database"`
	SchemaVersion int             `json:"schema_version,omitempty"`
	PGDumpVersion string          `json:"pg_dump_version,omitempty"`
	Files         []ManifestEntry `json:"files"`
	Counts        *Counts         `json:"counts,omitempty"` // nil when not recorded
	ServersError  string          `json:"servers_error,omitempty"`
}

type Counts struct {
	Users   int `json:"users"`
	Servers int `json:"servers"`
}

// Fresh reports whether the database looks like a new install: no servers and at most the first-run owner.
func (c *Counts) Fresh() bool { return c != nil && c.Servers == 0 && c.Users <= 1 }

func (c *Counts) String() string {
	if c == nil {
		return "not recorded"
	}
	count := func(n int, what string) string {
		if n == 1 {
			return "1 " + what
		}
		return fmt.Sprintf("%d %ss", n, what)
	}
	return count(c.Users, "account") + ", " + count(c.Servers, "server")
}

type DatabaseInfo struct {
	Host string `json:"host"`
	Port string `json:"port,omitempty"`
	Name string `json:"name"`
	User string `json:"user"`
}

type ManifestEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"` // absent for symlinks
	Mode   uint32 `json:"mode"`
	Link   string `json:"link,omitempty"`
}

type Bundle struct {
	Name    string
	Path    string
	Label   string
	Created time.Time
	Size    int64
}

// conn is a postgres:// URL split into a password-free URL and the password, which goes to the child via PGPASSWORD to stay out of ps.
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
	// A wrong password must fail, not prompt and hang.
	return append(env, "PGCONNECT_TIMEOUT=15")
}

type toolCmd struct {
	*exec.Cmd
	name string
}

func (t Tools) command(ctx context.Context, c conn, tool string, args ...string) toolCmd {
	if len(t.Exec) > 0 {
		argv := append(append(append([]string{}, t.Exec[1:]...), tool), args...)
		return toolCmd{exec.CommandContext(ctx, t.Exec[0], argv...), filepath.Base(tool)}
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = c.env()
	return toolCmd{cmd, filepath.Base(tool)}
}

func (t Tools) dsn(c conn) string {
	if len(t.Exec) > 0 {
		return t.Conn
	}
	return c.uri
}

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

var foreignObjectRe = regexp.MustCompile(`permission denied for (table|sequence|schema|view|materialized view) ([^\s]+)`)

func dumpHint(err error, db DatabaseInfo, t Tools) string {
	m := foreignObjectRe.FindStringSubmatch(err.Error())
	if m == nil {
		return ""
	}
	su := "sudo -u postgres psql"
	if len(t.Exec) > 0 {
		su = "sudo " + strings.Join(t.Exec, " ") + " psql -U postgres"
	}
	kind, name := strings.ToUpper(m[1]), m[2]
	return fmt.Sprintf("\n  %s %s belongs to another role, so %s cannot dump it. Hand it over with\n"+
		"    %s -d %s -c 'ALTER %s %s OWNER TO %s'\n"+
		"  or drop it if it is a leftover.", strings.ToLower(kind), name, db.User, su, db.Name, kind, name, db.User)
}

func BundleName(t time.Time, label string) string {
	return bundlePrefix + t.UTC().Format(stampLayout) + "-" + label + bundleExt
}

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

// Newest returns the first bundle of label; bundles must be ordered newest first, as List returns them.
func Newest(bundles []Bundle, label string) (Bundle, bool) {
	for _, b := range bundles {
		if b.Label == label {
			return b, true
		}
	}
	return Bundle{}, false
}

// Prune deletes all but the newest keep bundles of label, with their sidecars.
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

var ErrNewestBundle = errors.New("the newest database backup is the latest restore point and cannot be deleted; take a new one first")

// Delete removes a bundle and its sidecar under the directory lock; it refuses the newest bundle.
func Delete(dir, name string) error {
	if _, _, ok := ParseBundleName(name); !ok || filepath.Base(name) != name {
		return fmt.Errorf("%q is not a database backup name", name)
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer unlock()
	all, err := List(dir)
	if err != nil {
		return err
	}
	for i, b := range all {
		if b.Name != name {
			continue
		}
		if i == 0 {
			return ErrNewestBundle
		}
		if err := os.Remove(b.Path); err != nil {
			return err
		}
		_ = os.Remove(b.Path + sumExt)
		return nil
	}
	return fmt.Errorf("no database backup %s in %s", name, dir)
}

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

// removeStalePartials drops leftovers of a writer that died mid-bundle; call it under the directory lock.
func removeStalePartials(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+bundlePrefix) && strings.HasSuffix(e.Name(), ".partial") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Backup writes one bundle and returns its path; with ErrServersMissing the bundle lacks the MinecraftServer objects.
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

	// Names have one-second resolution; a second bundle of the same label within that second moves to the next free one.
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
	if err := listArchive(ctx, c, o.Tools, dump); err != nil {
		return "", fmt.Errorf("the dump does not read back: %w", err)
	}

	m := Manifest{Format: formatV1, CreatedAt: created, Label: o.Label, FelisVersion: o.Version, Database: c.info}
	if out, err := run(o.Tools.command(ctx, c, o.Tools.pgDump(), "--version")); err == nil {
		m.PGDumpVersion = strings.TrimSpace(string(out))
	}
	m.SchemaVersion = schemaVersion(ctx, c, o.Tools)
	m.Counts = counts(ctx, c, o.Tools)

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
			if o.RequireServers {
				return "", fmt.Errorf("export the MinecraftServer objects: %w", err)
			}
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
			FelisVersion: o.Version, SchemaVersion: m.SchemaVersion, Dir: o.Dir, ServersError: m.ServersError}
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
	if m.ServersError != "" {
		return final, fmt.Errorf("%w: %s", ErrServersMissing, m.ServersError)
	}
	return final, nil
}

// dumpTo writes pg_dump's custom-format archive to path (0600) via stdout, the only channel out of Tools.Exec.
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

// record upserts st into platform_settings, setting DailyAt from st.Dir; the JSON goes in as a psql variable over stdin because -c does not interpolate.
func record(ctx context.Context, c conn, t Tools, st Status) error {
	if all, err := List(st.Dir); err == nil {
		if d, ok := Newest(all, LabelDaily); ok {
			st.DailyAt = d.Created
		}
	}
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
	// node-exporter usually runs unprivileged.
	return os.Chmod(path, 0o644)
}

func schemaVersion(ctx context.Context, c conn, t Tools) int {
	out, err := run(t.command(ctx, c, t.psql(), "-X", "-q", "-t", "-A", "-w", "-d", t.dsn(c),
		"-c", "SELECT coalesce(max(version), 0) FROM schema_migrations"))
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return v
}

func counts(ctx context.Context, c conn, t Tools) *Counts {
	out, err := run(t.command(ctx, c, t.psql(), "-X", "-q", "-t", "-A", "-w", "-d", t.dsn(c),
		"-c", "SELECT (SELECT count(*) FROM users WHERE deleted_at IS NULL), (SELECT count(*) FROM servers WHERE deleted_at IS NULL)"))
	if err != nil {
		return nil
	}
	users, servers, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	u, uerr := strconv.Atoi(users)
	s, serr := strconv.Atoi(servers)
	if uerr != nil || serr != nil {
		return nil
	}
	return &Counts{Users: u, Servers: s}
}

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

// stateMembers bundles the regular files and symlinks directly in dir under state/<absolute dir>/.
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

// Check uses only daily bundles so manual backups cannot hide a stopped timer.
func Check(dir string, maxAge time.Duration, now time.Time) (*Bundle, error) {
	all, err := List(dir)
	if err != nil {
		return nil, err
	}
	daily, ok := Newest(all, LabelDaily)
	if !ok {
		return nil, fmt.Errorf("no daily database backup in %s (felis-db-backup.timer writes them)", dir)
	}
	if age := now.Sub(daily.Created); age > maxAge {
		return &daily, fmt.Errorf("newest daily database backup %s is %s old (limit %s)", daily.Name, Age(age), maxAge)
	}
	return &daily, nil
}
