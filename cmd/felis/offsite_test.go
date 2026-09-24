package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"felis.lolicon.best/internal/config"
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
