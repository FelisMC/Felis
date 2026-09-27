package main

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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/imagepush"
	"felis.lolicon.best/internal/offsite"
)

func TestLoadOffsiteEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offsite.env")
	body := `# written by bootstrap
FELIS_OFFSITE_ACCESS_KEY=AKIA123
export FELIS_OFFSITE_SECRET_KEY="se=cret"
FELIS_OFFSITE_KEY='k'

not a line
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FELIS_OFFSITE_ACCESS_KEY", "from-the-shell")
	t.Setenv("FELIS_OFFSITE_SECRET_KEY", "")
	t.Setenv("FELIS_OFFSITE_KEY", "")
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"FELIS_OFFSITE_ACCESS_KEY": "from-the-shell", // the environment wins
		"FELIS_OFFSITE_SECRET_KEY": "se=cret",
		"FELIS_OFFSITE_KEY":        "k",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := loadEnvFile(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("a missing env file is not an error: %v", err)
	}
}

func TestResolveOffsiteNamesTheMissingVariable(t *testing.T) {
	c := config.OffsiteConfig{
		Endpoint: "https://s3.example", Bucket: "b",
		AccessKeyRef: "T_AK", SecretKeyRef: "T_SK", KeyRef: "T_KEY",
	}
	t.Setenv("T_AK", "ak")
	t.Setenv("T_SK", "sk")
	t.Setenv("T_KEY", "")
	if _, err := resolveOffsite(c); err == nil || !strings.Contains(err.Error(), "T_KEY") {
		t.Fatalf("err = %v, want it to name T_KEY", err)
	}
	t.Setenv("T_KEY", "not base64 at all")
	if _, err := resolveOffsite(c); err == nil {
		t.Fatal("a malformed key was accepted")
	}
	key, _ := offsite.NewKey()
	t.Setenv("T_KEY", key)
	env, err := resolveOffsite(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.key) != offsite.KeySize {
		t.Fatalf("key is %d bytes", len(env.key))
	}
	if _, err := resolveOffsite(config.OffsiteConfig{}); err == nil {
		t.Fatal("an unconfigured [offsite] resolved")
	}
}

func TestOffsiteKeygen(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdOffsite([]string{"keygen"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if _, err := offsite.ParseKey(strings.TrimSpace(out.String())); err != nil {
		t.Fatalf("keygen printed %q: %v", out.String(), err)
	}
}

func TestOffsiteFetchDBRejectsOddNames(t *testing.T) {
	key, _ := offsite.NewKey()
	t.Setenv("FELIS_OFFSITE_ACCESS_KEY", "ak")
	t.Setenv("FELIS_OFFSITE_SECRET_KEY", "sk")
	t.Setenv("FELIS_OFFSITE_KEY", key)
	var out, errb bytes.Buffer
	code := cmdOffsite([]string{"fetch-db", "-env-file", "", "-endpoint", "http://127.0.0.1:1", "-bucket", "b",
		"-dir", t.TempDir(), "../../etc/shadow"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "not a bundle name") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

func TestOffsiteRegistryEndpoint(t *testing.T) {
	for _, tc := range []struct{ flag, url, want string }{
		{"", "registry.felis.svc:5000", "127.0.0.1:5000"},
		{"", "registry.felis.svc.cluster.local:5001", "127.0.0.1:5001"},
		{"", "ghcr.io/acme", ""},
		{"", "", ""},
		{"off", "registry.felis.svc:5000", ""},
		{"10.0.0.5:5000", "ghcr.io/acme", "10.0.0.5:5000"},
	} {
		if got := offsiteRegistryEndpoint(tc.flag, config.RegistryConfig{URL: tc.url}); got != tc.want {
			t.Errorf("offsiteRegistryEndpoint(%q, %q) = %q, want %q", tc.flag, tc.url, got, tc.want)
		}
	}
}

func TestRegistryGoneMarksNotFound(t *testing.T) {
	if err := registryGone(&imagepush.StatusError{Op: "get blob", Code: 404}); !errors.Is(err, offsite.ErrImageGone) {
		t.Fatalf("404 = %v, want ErrImageGone", err)
	}
	if err := registryGone(&imagepush.StatusError{Op: "get blob", Code: 503}); errors.Is(err, offsite.ErrImageGone) {
		t.Fatalf("503 = %v, want it kept an ordinary failure", err)
	}
}

// mapBucket is an in-memory offsite.Bucket.
type mapBucket map[string][]byte

func (b mapBucket) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	b[key] = data
	return err
}

func (b mapBucket) Get(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := b[key]
	if !ok {
		return nil, offsite.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b mapBucket) List(_ context.Context, prefix string) ([]offsite.Object, error) {
	var out []offsite.Object
	for k, v := range b {
		if strings.HasPrefix(k, prefix) {
			out = append(out, offsite.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (b mapBucket) Remove(_ context.Context, key string) error {
	delete(b, key)
	return nil
}

var fetchT0 = time.Date(2026, 9, 20, 3, 30, 0, 0, time.UTC)

// putBundle seals a bundle that verifies, taken daysAgo days before fetchT0,
// into b and returns its name.
func putBundle(t *testing.T, b mapBucket, key []byte, daysAgo int, counts *dbbackup.Counts) string {
	t.Helper()
	return putBundleWith(t, b, key, daysAgo, counts, "")
}

// putBundleWith is putBundle for a bundle whose server export failed with
// serversError, when that is not empty.
func putBundleWith(t *testing.T, b mapBucket, key []byte, daysAgo int, counts *dbbackup.Counts, serversError string) string {
	t.Helper()
	created := fetchT0.AddDate(0, 0, -daysAgo)
	dump := []byte("PGDMP " + created.String())
	sum := sha256.Sum256(dump)
	manifest, err := json.Marshal(dbbackup.Manifest{
		Format: 1, CreatedAt: created, Label: dbbackup.LabelDaily, FelisVersion: "v1.2.3", SchemaVersion: 21, Counts: counts, ServersError: serversError,
		Files: []dbbackup.ManifestEntry{{Name: "db.dump", Size: int64(len(dump)), SHA256: hex.EncodeToString(sum[:]), Mode: 0o600}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	for _, f := range []struct {
		name string
		data []byte
	}{{"MANIFEST.json", manifest}, {"db.dump", dump}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var sealed bytes.Buffer
	if err := offsite.Encrypt(&sealed, &plain, key); err != nil {
		t.Fatal(err)
	}
	name := dbbackup.BundleName(created, dbbackup.LabelDaily)
	b[offsite.DBKey(name)] = sealed.Bytes()
	return name
}

func TestOffsiteFetchDB(t *testing.T) {
	rawKey, _ := offsite.NewKey()
	key, _ := offsite.ParseKey(rawKey)
	now := fetchT0.Add(2 * time.Hour)
	fetch := func(b mapBucket, arg string) (dir string, code int, stdout, stderr string) {
		dir = t.TempDir()
		var out, errb bytes.Buffer
		code = fetchDB(context.Background(), b, key, arg, dir, now, &out, &errb)
		return dir, code, out.String(), errb.String()
	}
	fetched := func(t *testing.T, dir string) []string {
		t.Helper()
		var names []string
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}

	t.Run("latest skips a rebuilt host's empty bundle", func(t *testing.T) {
		b := mapBucket{}
		full := putBundle(t, b, key, 3, &dbbackup.Counts{Users: 5, Servers: 3})
		empty := putBundle(t, b, key, 0, &dbbackup.Counts{})
		dir, code, out, errb := fetch(b, "latest")
		if code != 1 || !strings.Contains(errb, empty) || !strings.Contains(errb, full+" (5 accounts, 3 servers)") {
			t.Fatalf("exit %d, stdout %q, stderr %q; want a refusal naming %s", code, out, errb, full)
		}
		if got := fetched(t, dir); len(got) != 0 {
			t.Errorf("a refused fetch wrote %v", got)
		}
	})

	t.Run("latest takes the newest bundle and says what it holds", func(t *testing.T) {
		b := mapBucket{}
		putBundle(t, b, key, 3, &dbbackup.Counts{Users: 5, Servers: 2})
		newest := putBundle(t, b, key, 1, &dbbackup.Counts{Users: 5, Servers: 3})
		dir, code, out, errb := fetch(b, "latest")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errb)
		}
		if got := fetched(t, dir); !slices.Equal(got, []string{newest}) {
			t.Errorf("wrote %v, want %s", got, newest)
		}
		for _, want := range []string{
			"wrote " + filepath.Join(dir, newest) + " (verified)",
			"taken   2026-09-19T03:30:00Z (daily, 26h0m ago)",
			"felis   v1.2.3, schema 21",
			"holds   5 accounts, 3 servers",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "new install") {
			t.Errorf("a bundle with servers flagged as a new install's:\n%s", out)
		}
	})

	t.Run("an empty bundle named outright is fetched with a warning", func(t *testing.T) {
		b := mapBucket{}
		putBundle(t, b, key, 3, &dbbackup.Counts{Users: 5, Servers: 3})
		empty := putBundle(t, b, key, 0, &dbbackup.Counts{Users: 1})
		dir, code, out, errb := fetch(b, empty)
		if code != 0 || !slices.Equal(fetched(t, dir), []string{empty}) {
			t.Fatalf("exit %d, wrote %v: %s", code, fetched(t, dir), errb)
		}
		if !strings.Contains(out, "holds   1 account, 0 servers") || !strings.Contains(out, "like a new install's") {
			t.Errorf("stdout = %s", out)
		}
	})

	t.Run("a bundle without the servers says where they come from", func(t *testing.T) {
		b := mapBucket{}
		gapped := putBundleWith(t, b, key, 1, &dbbackup.Counts{Users: 5, Servers: 3}, "connection refused (tried 3 times)")
		_, code, out, errb := fetch(b, gapped)
		if code != 0 || !strings.Contains(out, "This bundle lacks the MinecraftServer objects (connection refused (tried 3 times))") ||
			!strings.Contains(out, "k8s/minecraftservers.json in the newest bundle `felis offsite list` shows without that gap") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errb)
		}
		whole := putBundle(t, b, key, 0, &dbbackup.Counts{Users: 5, Servers: 3})
		if _, _, out, _ := fetch(b, whole); strings.Contains(out, "lacks the MinecraftServer objects") {
			t.Errorf("a whole bundle flagged:\n%s", out)
		}
	})

	t.Run("a bundle from before counts says so", func(t *testing.T) {
		b := mapBucket{}
		old := putBundle(t, b, key, 0, nil)
		_, code, out, errb := fetch(b, "latest")
		if code != 0 || !strings.Contains(out, old) || !strings.Contains(out, "holds   not recorded") || strings.Contains(out, "new install") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errb)
		}
	})
}

func TestOffsiteCheckKey(t *testing.T) {
	newKey := func() []byte {
		raw, _ := offsite.NewKey()
		k, _ := offsite.ParseKey(raw)
		return k
	}
	key, other := newKey(), newKey()
	marked := func(k []byte) mapBucket { return mapBucket{"felis-key-id": []byte(offsite.KeyID(k) + "\n")} }
	unmarked := func(k []byte) mapBucket {
		b := mapBucket{}
		putBundle(t, b, k, 0, nil)
		return b
	}
	for _, tc := range []struct {
		what   string
		bucket mapBucket
		code   int
		says   []string
	}{
		{"the recorded key", marked(key), 0, []string{"records key id " + offsite.KeyID(key)}},
		{"an unmarked bucket the key opens", unmarked(key), 0, []string{"open with this key", "the next sync records it"}},
		{"an empty bucket", mapBucket{}, 0, []string{"no sealed object yet", "records key id " + offsite.KeyID(key)}},
		{"another recorded key", marked(other), 3, []string{offsite.KeyID(other), offsite.KeyID(key), "FELIS_OFFSITE_KEY"}},
		{"an unmarked bucket under another key", unmarked(other), 3, []string{"opens none of db/felis-db-"}},
		{"a marker Felis did not write", mapBucket{"felis-key-id": []byte("hello")}, 1, []string{"not a key id"}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			before := len(tc.bucket)
			var out, errb bytes.Buffer
			code := checkKey(context.Background(), tc.bucket, key, &out, &errb)
			if code != tc.code {
				t.Fatalf("exit %d, want %d; stdout %q, stderr %q", code, tc.code, out.String(), errb.String())
			}
			said := out.String() + errb.String()
			for _, s := range tc.says {
				if !strings.Contains(said, s) {
					t.Errorf("output lacks %q: %s", s, said)
				}
			}
			if len(tc.bucket) != before {
				t.Errorf("check-key wrote to the bucket: %d objects, had %d", len(tc.bucket), before)
			}
		})
	}

	// fetch-db names both ids when the bucket records another key.
	b := marked(other)
	name := putBundle(t, b, other, 0, &dbbackup.Counts{Users: 5, Servers: 3})
	for _, arg := range []string{"latest", name} {
		var out, errb bytes.Buffer
		if code := fetchDB(context.Background(), b, key, arg, t.TempDir(), fetchT0, &out, &errb); code != 1 ||
			!strings.Contains(errb.String(), "the bucket records key id "+offsite.KeyID(other)+", and this key is "+offsite.KeyID(key)) {
			t.Errorf("fetch-db %s under another key: exit %d, stderr %q", arg, code, errb.String())
		}
	}
	// A bundle the bucket lacks is not the key's fault.
	var missOut, missErr bytes.Buffer
	if code := fetchDB(context.Background(), b, key, "felis-db-20200101T000000Z-daily.tar", t.TempDir(), fetchT0, &missOut, &missErr); code != 1 || strings.Contains(missErr.String(), "records key id") {
		t.Errorf("missing bundle: exit %d, stderr %q", code, missErr.String())
	}
	// A bundle damaged under the recorded key gets no such hint.
	b = marked(key)
	name = putBundle(t, b, key, 0, &dbbackup.Counts{Users: 5, Servers: 3})
	b[offsite.DBKey(name)][60] ^= 1
	var out, errb bytes.Buffer
	if code := fetchDB(context.Background(), b, key, name, t.TempDir(), fetchT0, &out, &errb); code != 1 || strings.Contains(errb.String(), "records key id") {
		t.Errorf("damaged bundle: exit %d, stderr %q", code, errb.String())
	}
}

func TestRecordRun(t *testing.T) {
	t0 := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	w := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: t0}
	for _, tc := range []struct {
		err                                   error
		mismatch, standby, displaced, success bool
	}{
		{nil, false, false, false, true},
		{errors.New("list worlds/ in the bucket: connection reset"), false, false, false, false},
		{fmt.Errorf("%w: the bucket records key id 0123456789abcdef", offsite.ErrKeyMismatch), true, false, false, false},
		{&offsite.WriterError{Kind: offsite.ErrStandby, Writer: w}, false, true, false, false},
		{&offsite.WriterError{Kind: offsite.ErrDisplaced, Writer: w}, false, false, true, false},
	} {
		st := offsite.Status{LastAttempt: t0}
		recordRun(&st, offsite.Result{RemoteDB: 2}, tc.err, offsite.Lease{})
		if st.KeyMismatch != tc.mismatch || st.Standby != tc.standby || st.Displaced != tc.displaced ||
			(st.Writer != nil) != (tc.standby || tc.displaced) || st.LastSuccess.Equal(t0) != tc.success || st.Result.RemoteDB != 2 {
			t.Errorf("err %v: status %+v", tc.err, st)
		}
	}

	// A host that copied before writers were recorded keeps that claim over
	// failed runs until it has an id.
	l := offsite.Lease{IDFile: filepath.Join(t.TempDir(), offsite.HostIDFile), Inherited: true}
	st := offsite.Status{}
	recordRun(&st, offsite.Result{}, errors.New("cannot reach bucket"), l)
	if !st.Inherited {
		t.Error("a failed run on a host with no id dropped the older release's claim")
	}
	writeTestFile(t, l.IDFile, "aaaaaaaaaaaaaaaa\n", 0o600)
	st = offsite.Status{Inherited: true}
	recordRun(&st, offsite.Result{}, nil, l)
	if st.Inherited {
		t.Error("a host with an id still carries the older release's claim")
	}
}

// A refused run has copied nothing and listed nothing: its zero counts would
// tell the journal the bucket is empty. A pass that ran and failed a step
// shows what it did get to.
func TestReportRun(t *testing.T) {
	w := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		name   string
		res    offsite.Result
		err    error
		code   int
		counts bool
	}{
		{"a pass", offsite.Result{DBUploaded: 1, RemoteDB: 3}, nil, 0, true},
		{"a pass with a failed step", offsite.Result{RemoteDB: 3, Errors: []string{"copy db/x: timeout"}}, errors.New("1 of this run's steps failed; first: copy db/x: timeout"), 1, true},
		{"standing by", offsite.Result{}, &offsite.WriterError{Kind: offsite.ErrStandby, Writer: w}, 1, false},
		{"another key", offsite.Result{}, fmt.Errorf("%w: the bucket records key id 1111111111111111", offsite.ErrKeyMismatch), 1, false},
		{"no bucket", offsite.Result{}, errors.New("bucket: access denied"), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := reportRun(tc.res, tc.err, &out, &errOut)
			if code != tc.code {
				t.Errorf("exit %d, want %d", code, tc.code)
			}
			if got := strings.Contains(out.String(), "bucket holds 0 worlds (0 B), 3 bundles"); got != tc.counts {
				t.Errorf("counts shown = %v, want %v: %q", got, tc.counts, out.String())
			}
			if !tc.counts && out.Len() > 0 {
				t.Errorf("a refused run printed %q", out.String())
			}
			if tc.err != nil && !strings.Contains(errOut.String(), "felis offsite sync: "+tc.err.Error()) {
				t.Errorf("stderr %q lacks the error", errOut.String())
			}
		})
	}
}

func TestOffsiteTakeOver(t *testing.T) {
	newKey := func() []byte {
		raw, _ := offsite.NewKey()
		k, _ := offsite.ParseKey(raw)
		return k
	}
	key, other := newKey(), newKey()
	const mine, theirs = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	t0 := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	sealed := func(k []byte, writer string) mapBucket {
		b := mapBucket{}
		putBundle(t, b, k, 0, nil)
		b["felis-key-id"] = []byte(offsite.KeyID(k) + "\n")
		if writer != "" {
			raw, _ := json.Marshal(offsite.Writer{HostID: writer, Host: "prod-1", At: t0.Add(-20 * time.Minute)})
			b["felis-writer"] = raw
		}
		return b
	}
	lease := func(id string) offsite.Lease {
		l := offsite.Lease{IDFile: filepath.Join(t.TempDir(), offsite.HostIDFile), Host: "spare-1"}
		if id != "" {
			writeTestFile(t, l.IDFile, id+"\n", 0o600)
		}
		return l
	}
	run := func(b mapBucket, l offsite.Lease, statusFile string, yes bool) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := takeOver(context.Background(), b, key, l, statusFile, yes, t0, &out, &errb)
		return code, out.String(), errb.String()
	}
	for _, tc := range []struct {
		what   string
		bucket mapBucket
		id     string
		code   int
		says   []string
	}{
		{"this host writes the bucket", sealed(key, mine), mine, 0, []string{"this host (id " + mine + ") writes the bucket; nothing to take over"}},
		{"an empty bucket", mapBucket{}, "", 0, []string{"names no host writing it; this host's next sync records itself"}},
		{"a host built from the writer's backup", sealed(key, theirs), "", 4, []string{"host prod-1 (id " + theirs + ") writes the bucket, last at", "(20m ago)", "built from its backup", "sudo felis offsite take-over -yes"}},
		{"another host's copies, no writer named", sealed(key, ""), "", 4, []string{"holds copies this host did not write, and names no host writing it", "take-over -yes"}},
		{"a host another one took over from", sealed(key, theirs), mine, 5, []string{"took the bucket over from this host"}},
		{"a key the bucket refuses", sealed(other, theirs), "", 3, []string{"sealed with another key"}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			before := string(tc.bucket["felis-writer"])
			l := lease(tc.id)
			code, out, errb := run(tc.bucket, l, filepath.Join(t.TempDir(), "status.json"), false)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\n%s%s", code, tc.code, out, errb)
			}
			for _, s := range tc.says {
				if !strings.Contains(out+errb, s) {
					t.Errorf("output lacks %q:\n%s%s", s, out, errb)
				}
			}
			if string(tc.bucket["felis-writer"]) != before {
				t.Error("take-over without -yes wrote the bucket's writer")
			}
			if tc.id == "" {
				if _, err := os.Stat(l.IDFile); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("take-over without -yes made this host an id: %v", err)
				}
			}
		})
	}

	// -yes on a standby host: the bucket names it, and the refusal the last
	// sync recorded is cleared, so the watchdog mails again at once.
	b := sealed(key, theirs)
	l := lease("")
	statusFile := filepath.Join(t.TempDir(), "status.json")
	lastSuccess := t0.Add(-48 * time.Hour)
	if err := offsite.WriteStatus(statusFile, offsite.Status{
		LastAttempt: t0.Add(-time.Hour), LastSuccess: lastSuccess, LastError: "offsite: another host writes this bucket", Format: offsite.StatusFormat,
		Standby: true, Writer: &offsite.Writer{HostID: theirs, Host: "prod-1", At: t0}, Inherited: true,
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := run(b, l, statusFile, true)
	id, _ := l.ID()
	if code != 0 || id == "" || !strings.Contains(out, "this host (id "+id+") writes the bucket now; host prod-1 (id "+theirs+") stops at its next copy") || !strings.Contains(out, "systemctl start felis-offsite.service") {
		t.Fatalf("take-over -yes: exit %d, id %q\n%s%s", code, id, out, errb)
	}
	if w, err := offsite.BucketWriter(context.Background(), b); err != nil || w.HostID != id || w.Host != "spare-1" || !w.At.Equal(t0) {
		t.Errorf("writer after take-over -yes = %+v, %v", w, err)
	}
	st, err := offsite.ReadStatus(statusFile)
	if err != nil || st.Standby || st.Writer != nil || st.LastError != "" || st.Inherited || !st.LastSuccess.Equal(lastSuccess) {
		t.Errorf("status after take-over -yes = %+v, %v; want the refusal cleared and the last success kept", st, err)
	}

	// -yes with a key the bucket refuses writes nothing.
	b = sealed(other, theirs)
	before := string(b["felis-writer"])
	if code, _, _ := run(b, lease(""), filepath.Join(t.TempDir(), "status.json"), true); code != 3 || string(b["felis-writer"]) != before {
		t.Errorf("take-over -yes under another key: exit %d, writer %s", code, b["felis-writer"])
	}
}

func TestOffsiteStatusSaysWhoWritesTheBucket(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "felis.toml")
	writeTestFile(t, cfg, installerTOML("example.com", "127.0.0.1")+"\n[offsite]\nendpoint = \"https://s3.example.com\"\nbucket = \"felis-backups\"\n", 0o600)
	statusFile := filepath.Join(dir, "status.json")
	now := time.Now()
	w := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: now.Add(-30 * time.Minute)}
	stale := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: now.Add(-offsite.WriterLive - time.Hour)}
	for _, tc := range []struct {
		what string
		st   offsite.Status
		says []string
		not  string
	}{
		{"standing by for a live writer", offsite.Status{Standby: true, Writer: w}, []string{"This host stands by: host prod-1 (id bbbbbbbbbbbbbbbb) writes the bucket", "mails no watchdog alert", "take-over -yes"}, "wrote it, last at"},
		{"standing by for a writer gone quiet", offsite.Status{Standby: true, Writer: stale}, []string{"copies nothing into the bucket: host prod-1 (id bbbbbbbbbbbbbbbb) wrote it, last at", "take-over -yes"}, "mails no watchdog alert"},
		{"standing by, no writer named", offsite.Status{Standby: true}, []string{"names no host writing it", "take-over -yes"}, "stands by:"},
		{"displaced", offsite.Status{Displaced: true, Writer: w}, []string{"host prod-1 (id bbbbbbbbbbbbbbbb) took the bucket over", "rehearsal machine", "take-over -yes"}, "stands by"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			tc.st.LastAttempt, tc.st.LastSuccess, tc.st.LastError = now.Add(-time.Minute), now.Add(-time.Hour), "offsite: another host writes this bucket"
			if err := offsite.WriteStatus(statusFile, tc.st); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			code := cmdOffsite([]string{"status", "-config", cfg, "-status-file", statusFile}, &out, &errb)
			if code != 1 || strings.Contains(out.String(), "bucket holds:") || strings.Contains(out.String(), tc.not) {
				t.Errorf("exit %d\n%s%s", code, out.String(), errb.String())
			}
			for _, s := range tc.says {
				if !strings.Contains(out.String(), s) {
					t.Errorf("output lacks %q:\n%s", s, out.String())
				}
			}
		})
	}
}

func TestOffsiteStatusSaysTheKeyWasRefused(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "felis.toml")
	writeTestFile(t, cfg, installerTOML("example.com", "127.0.0.1")+"\n[offsite]\nendpoint = \"https://s3.example.com\"\nbucket = \"felis-backups\"\n", 0o600)
	statusFile := filepath.Join(dir, "status.json")
	st := offsite.Status{LastAttempt: time.Now().Add(-time.Minute), LastSuccess: time.Now().Add(-time.Hour), KeyID: "0123456789abcdef"}
	status := func() (int, string) {
		t.Helper()
		if err := offsite.WriteStatus(statusFile, st); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := cmdOffsite([]string{"status", "-config", cfg, "-status-file", statusFile}, &out, &errb)
		return code, out.String() + errb.String()
	}
	if code, out := status(); code != 0 || strings.Contains(out, "refused") {
		t.Fatalf("a recent success: exit %d\n%s", code, out)
	}
	st.LastError, st.KeyMismatch = "offsite: the bucket's objects are sealed with another key", true
	code, out := status()
	if code != 1 || !strings.Contains(out, "The last run was refused") || !strings.Contains(out, "key id 0123456789abcdef") || strings.Contains(out, "bucket holds:") {
		t.Fatalf("a refused run: exit %d\n%s", code, out)
	}
}

func TestPrintDBBundlesSaysWhatEachHolds(t *testing.T) {
	rawKey, _ := offsite.NewKey()
	key, _ := offsite.ParseKey(rawKey)
	otherRaw, _ := offsite.NewKey()
	other, _ := offsite.ParseKey(otherRaw)
	b := mapBucket{}
	gapped := putBundleWith(t, b, key, 4, &dbbackup.Counts{Users: 5, Servers: 3}, "connection refused")
	old := putBundle(t, b, key, 3, nil)
	full := putBundle(t, b, key, 2, &dbbackup.Counts{Users: 5, Servers: 3})
	sealedElsewhere := putBundle(t, b, other, 1, &dbbackup.Counts{Users: 5, Servers: 3})
	empty := putBundle(t, b, key, 0, &dbbackup.Counts{})
	bundles, err := offsite.ListDB(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printDBBundles(context.Background(), b, key, bundles, &out)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []struct{ name, holds string }{
		{empty, "0 accounts, 0 servers"},
		{sealedElsewhere, "unreadable: offsite: object does not decrypt with this key"},
		{full, "5 accounts, 3 servers"},
		{old, "not recorded"},
		{gapped, "5 accounts, 3 servers, no MinecraftServer objects"},
	}
	if len(lines) != len(want)+1 || !strings.HasPrefix(lines[0], "database bundles (5, newest first") {
		t.Fatalf("output:\n%s", out.String())
	}
	for i, w := range want {
		if l := lines[i+1]; !strings.HasPrefix(l, "  "+w.name+"  ") || !strings.Contains(l, w.holds) || strings.Contains(l, "no MinecraftServer") != (w.name == gapped) {
			t.Errorf("line %d = %q, want %s with %q", i+1, l, w.name, w.holds)
		}
	}
}

// TestRestoredHostKeepsStandingBy walks the status file across runs: a host
// restored from the writer's backup stands by on its first run and on every
// run after it, a host an older release left copying claims the bucket once,
// and a failed first run after the upgrade keeps that claim.
func TestRestoredHostKeepsStandingBy(t *testing.T) {
	rawKey, _ := offsite.NewKey()
	key, _ := offsite.ParseKey(rawKey)
	cfg := config.OffsiteConfig{Endpoint: "https://s3.example.com", Bucket: "felis-backups"}
	t0 := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	standby := &offsite.WriterError{Kind: offsite.ErrStandby, Writer: &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: t0}}
	pass := func(statusFile string, at time.Time, err error) offsite.Lease {
		t.Helper()
		st, lease := startRun(cfg, key, statusFile, at)
		recordRun(&st, offsite.Result{}, err, lease)
		if werr := offsite.WriteStatus(statusFile, st); werr != nil {
			t.Fatal(werr)
		}
		return lease
	}

	restored := filepath.Join(t.TempDir(), "status.json")
	for i := range 3 {
		if l := pass(restored, t0.Add(time.Duration(i)*time.Hour), standby); l.Inherited {
			t.Fatalf("run %d of a restored host claims the bucket", i+1)
		}
	}
	if st, _ := offsite.ReadStatus(restored); !st.Standby || st.Format != offsite.StatusFormat || st.KeyID != offsite.KeyID(key) || st.Bucket != "felis-backups" {
		t.Errorf("restored host's status = %+v", st)
	}

	upgraded := filepath.Join(t.TempDir(), "status.json")
	if err := offsite.WriteStatus(upgraded, offsite.Status{LastAttempt: t0.Add(-time.Hour), LastSuccess: t0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if l := pass(upgraded, t0, errors.New("cannot reach bucket")); !l.Inherited {
		t.Fatal("the first run after the upgrade does not claim the bucket")
	}
	l := pass(upgraded, t0.Add(time.Hour), nil)
	if !l.Inherited {
		t.Fatal("a failed first run after the upgrade lost the claim")
	}
	if st, _ := offsite.ReadStatus(upgraded); !st.LastSuccess.Equal(t0.Add(time.Hour)) {
		t.Errorf("upgraded host's status = %+v", st)
	}
}

// TestOffsiteSyncerSnapshotsAndSweeps: the pass `offsite sync` runs takes its
// snapshot the way `felis db backup` does, in the database pod, into the
// bundle directory, keeping the newest one there; and it sweeps unrecorded
// world objects only past the longest retention [archive] gives any backup.
func TestOffsiteSyncerSnapshotsAndSweeps(t *testing.T) {
	dir := newPodRig(t)
	bundles := filepath.Join(dir, "bundles")
	env := &offsiteEnv{cfg: config.OffsiteConfig{DBKeep: 5}}
	var log bytes.Buffer
	cfg := &config.Config{Database: podDB, Archive: config.ArchiveConfig{Retention: "120d"}}
	s := offsiteSyncer(cfg, env, offsiteSources{dbDir: bundles}, "/archives", "/uploads", nil, &log)
	if s.DBDir != bundles || s.DBKeep != 5 || s.ArchiveDir != "/archives" || s.UploadsDir != "/uploads" {
		t.Fatalf("syncer = %+v", s)
	}
	if s.OrphanAfter != 120*24*time.Hour {
		t.Errorf("OrphanAfter = %s, want the 120d retention", s.OrphanAfter)
	}
	if s.Snapshot == nil {
		t.Fatal("the pass takes no snapshot after copying archives")
	}
	for i := 0; i < 2; i++ {
		if err := s.Snapshot(context.Background()); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}
	got, err := dbbackup.List(bundles)
	if err != nil || len(got) != 1 || got[0].Label != dbbackup.LabelOffsite {
		t.Fatalf("bundle directory = %+v, %v; want the newest offsite bundle alone", got, err)
	}
	if _, err := dbbackupVerify(got[0].Path); err != nil {
		t.Fatalf("the snapshot does not verify: %v", err)
	}
	// The MinecraftServer objects are exported alongside, as in the daily bundle.
	argv, _ := os.ReadFile(filepath.Join(dir, "k3s.args"))
	if ran := string(argv); !strings.Contains(ran, podExecPrefix+"pg_dump --format=custom") {
		t.Errorf("k3s ran %q, want pg_dump in the pod", ran)
	}
	if !strings.Contains(string(bundleServers(t, got[0].Path)), `"name": "lobby"`) {
		t.Errorf("the snapshot holds no MinecraftServer objects")
	}
	if !strings.Contains(log.String(), "took database bundle "+got[0].Name) {
		t.Errorf("the snapshot is not logged:\n%s", log.String())
	}
	// It becomes the newest bundle in the bucket, so with the cluster away it
	// fails, leaves no bundle, and the next pass tries again.
	noServerExportWait(t)
	writeTestFile(t, filepath.Join(dir, "servers_fail"), "99", 0o600)
	if err := s.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "export the MinecraftServer objects") {
		t.Errorf("snapshot with the cluster away: %v, want a failure", err)
	}
	if again, _ := dbbackup.List(bundles); len(again) != 1 || again[0].Name != got[0].Name {
		t.Errorf("bundle directory after the failed snapshot = %+v, want %s alone", again, got[0].Name)
	}

	for _, c := range []struct {
		archive config.ArchiveConfig
		want    time.Duration
	}{
		{config.ArchiveConfig{}, 90 * 24 * time.Hour},
		{config.ArchiveConfig{ScheduledRetention: "200d"}, 200 * 24 * time.Hour},
		{config.ArchiveConfig{ManualRetention: "150d", Retention: "30d", ScheduledRetention: "60d"}, 150 * 24 * time.Hour},
	} {
		s := offsiteSyncer(&config.Config{Database: podDB, Archive: c.archive}, env, offsiteSources{dbDir: bundles}, "", "", nil, io.Discard)
		if s.OrphanAfter != c.want {
			t.Errorf("%+v: OrphanAfter = %s, want %s", c.archive, s.OrphanAfter, c.want)
		}
	}
	log.Reset()
	s = offsiteSyncer(&config.Config{Database: podDB, Archive: config.ArchiveConfig{Retention: "soon"}}, env, offsiteSources{}, "", "", nil, &log)
	if s.OrphanAfter != 0 || !strings.Contains(log.String(), "world objects no backup records are kept") {
		t.Errorf("a retention that does not parse: OrphanAfter %s, log %q; want no sweep, said", s.OrphanAfter, log.String())
	}
	if s.Snapshot != nil {
		t.Error("a pass that copies no bundles takes a snapshot")
	}
}
