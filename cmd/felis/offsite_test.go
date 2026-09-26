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
	created := fetchT0.AddDate(0, 0, -daysAgo)
	dump := []byte("PGDMP " + created.String())
	sum := sha256.Sum256(dump)
	manifest, err := json.Marshal(dbbackup.Manifest{
		Format: 1, CreatedAt: created, Label: dbbackup.LabelDaily, FelisVersion: "v1.2.3", SchemaVersion: 21, Counts: counts,
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

func TestRecordRunMarksAKeyMismatch(t *testing.T) {
	t0 := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		err      error
		mismatch bool
		success  bool
	}{
		{nil, false, true},
		{errors.New("list worlds/ in the bucket: connection reset"), false, false},
		{fmt.Errorf("%w: the bucket records key id 0123456789abcdef", offsite.ErrKeyMismatch), true, false},
	} {
		st := offsite.Status{LastAttempt: t0}
		recordRun(&st, offsite.Result{RemoteDB: 2}, tc.err)
		if st.KeyMismatch != tc.mismatch || st.LastSuccess.Equal(t0) != tc.success || st.Result.RemoteDB != 2 {
			t.Errorf("err %v: status %+v", tc.err, st)
		}
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
	}
	if len(lines) != len(want)+1 || !strings.HasPrefix(lines[0], "database bundles (4, newest first") {
		t.Fatalf("output:\n%s", out.String())
	}
	for i, w := range want {
		if l := lines[i+1]; !strings.HasPrefix(l, "  "+w.name+"  ") || !strings.Contains(l, w.holds) {
			t.Errorf("line %d = %q, want %s with %q", i+1, l, w.name, w.holds)
		}
	}
}
