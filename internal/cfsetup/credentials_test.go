package cfsetup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testTunnelID = "0b6f3c1e-2d4a-4b8c-9e7f-1a2b3c4d5e6f"

// fakeCloudflared stands in for `cloudflared tunnel token --cred-file <path> <id>`: it
// counts its calls and writes what FAKE_CRED names into the file (valid credentials for
// the tunnel it was asked about by default).
func fakeCloudflared(t *testing.T) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "cloudflared")
	calls = filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> "` + calls + `"
case "${FAKE_CRED:-good}" in
  good) printf '{"AccountTag":"acc","TunnelSecret":"c2VjcmV0","TunnelID":"%s"}' "$5" > "$4" ;;
  garbage) printf 'not json' > "$4" ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func callCount(t *testing.T, calls string) int {
	t.Helper()
	raw, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}

func TestEnsureCredentialsKeepsAValidFile(t *testing.T) {
	bin, calls := fakeCloudflared(t)
	cred := filepath.Join(t.TempDir(), testTunnelID+".json")
	good := `{"AccountTag":"acc","TunnelSecret":"c2VjcmV0","TunnelID":"` + strings.ToUpper(testTunnelID) + `"}`
	if err := os.WriteFile(cred, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &ExecRunner{Cloudflared: bin}
	if err := r.ensureCredentials(context.Background(), testTunnelID, cred); err != nil {
		t.Fatalf("ensureCredentials: %v", err)
	}
	if n := callCount(t, calls); n != 0 {
		t.Fatalf("a valid credentials file was re-fetched (%d cloudflared calls)", n)
	}
}

func TestEnsureCredentialsReplacesAnUnusableFile(t *testing.T) {
	for name, content := range map[string]string{
		"empty":          "",
		"truncated":      `{"AccountTag":"acc","TunnelSec`,
		"missing secret": `{"AccountTag":"acc","TunnelID":"` + testTunnelID + `"}`,
		"another tunnel": `{"AccountTag":"acc","TunnelSecret":"c2VjcmV0","TunnelID":"11111111-2222-3333-4444-555555555555"}`,
	} {
		t.Run(name, func(t *testing.T) {
			bin, calls := fakeCloudflared(t)
			cred := filepath.Join(t.TempDir(), testTunnelID+".json")
			if err := os.WriteFile(cred, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			r := &ExecRunner{Cloudflared: bin}
			if err := r.ensureCredentials(context.Background(), testTunnelID, cred); err != nil {
				t.Fatalf("ensureCredentials: %v", err)
			}
			if n := callCount(t, calls); n != 1 {
				t.Fatalf("want one re-fetch, got %d cloudflared calls", n)
			}
			if p := credentialsProblem(cred, testTunnelID); p != "" {
				t.Fatalf("the regenerated file %s", p)
			}
			kept, err := os.ReadFile(cred + ".invalid")
			if err != nil || string(kept) != content {
				t.Fatalf("the unusable file was not kept aside: %q, %v", kept, err)
			}
			if fi, err := os.Stat(cred); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("the regenerated file must be 0600: %v, %v", fi, err)
			}
		})
	}
}

func TestEnsureCredentialsFetchesAMissingFile(t *testing.T) {
	bin, calls := fakeCloudflared(t)
	cred := filepath.Join(t.TempDir(), "sub", testTunnelID+".json")
	r := &ExecRunner{Cloudflared: bin}
	if err := r.ensureCredentials(context.Background(), testTunnelID, cred); err != nil {
		t.Fatalf("ensureCredentials: %v", err)
	}
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("want one fetch, got %d cloudflared calls", n)
	}
}

func TestEnsureCredentialsRefusesAnUnusableRegeneration(t *testing.T) {
	bin, _ := fakeCloudflared(t)
	t.Setenv("FAKE_CRED", "garbage")
	cred := filepath.Join(t.TempDir(), testTunnelID+".json")
	r := &ExecRunner{Cloudflared: bin}
	err := r.ensureCredentials(context.Background(), testTunnelID, cred)
	if err == nil || !strings.Contains(err.Error(), "is not valid JSON") {
		t.Fatalf("want the unusable regeneration reported, got %v", err)
	}
}
