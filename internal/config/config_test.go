package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"felis.lolicon.best/internal/config"
)

// writeTOML writes content to a temp felis.toml and returns its path.
func writeTOML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "felis.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	return path
}

// Note: tests use the neutral example domain mc.example.net, never a real
// deployment domain, to keep the source tree clean of domain literals.
const validTOML = `
[server]
listen = "0.0.0.0:9090"
root_domain = "mc.example.net"

[database]
url = "postgres://felis:secret@db:5432/felis"

[velocity]
public_ip = "203.0.113.4"
service_token_ref = "felis-velocity-token"

[auth]
admin_hostname = "admin.example.net"
panel_hostname = "panel.example.net"
access_jwt_aud = "felis-panel"

[k8s]
namespace = "minecraft"
egress_mode = "loadbalancer"
metallb_pool = "192.0.2.200-250"

[registry]
url = "registry.felis.svc:5000"
build_namespace = "felis-build"

[archive]
store = "tarLocal"
local_path = "backup-pvc"
retention = "3mo"
warn_before = ["3d", "1d"]
max_local_bytes = "200Gi"
[archive.s3]
endpoint = ""
bucket = "felis-backups"
access_key_ref = ""
secret_key_ref = ""
`

func TestLoadValid(t *testing.T) {
	cfg, err := config.Load(writeTOML(t, validTOML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != "0.0.0.0:9090" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Server.RootDomain != "mc.example.net" {
		t.Errorf("root_domain = %q", cfg.Server.RootDomain)
	}
	if cfg.Database.URL == "" {
		t.Error("database url empty")
	}
	if cfg.Archive.Store != "tarLocal" {
		t.Errorf("archive store = %q", cfg.Archive.Store)
	}
	if len(cfg.Archive.WarnBefore) != 2 || cfg.Archive.WarnBefore[0] != "3d" {
		t.Errorf("warn_before = %v", cfg.Archive.WarnBefore)
	}
	if cfg.Archive.S3.Bucket != "felis-backups" {
		t.Errorf("s3 bucket = %q", cfg.Archive.S3.Bucket)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != "0.0.0.0:8080" {
		t.Errorf("default listen = %q, want 0.0.0.0:8080", cfg.Server.Listen)
	}
	if cfg.K8s.Namespace != "minecraft" {
		t.Errorf("default namespace = %q, want minecraft", cfg.K8s.Namespace)
	}
	if cfg.K8s.EgressMode != "loadbalancer" {
		t.Errorf("default egress_mode = %q", cfg.K8s.EgressMode)
	}
	if cfg.Archive.Store != "tarLocal" {
		t.Errorf("default archive store = %q", cfg.Archive.Store)
	}
}

func TestLoadRejectsMissingDatabaseURL(t *testing.T) {
	_, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
`))
	if err == nil {
		t.Fatal("expected error when database.url is missing")
	}
}

func TestLoadRejectsMissingRootDomain(t *testing.T) {
	_, err := config.Load(writeTOML(t, `
[database]
url = "postgres://felis@db/felis"
`))
	if err == nil {
		t.Fatal("expected error when root_domain is missing")
	}
}

func TestLoadRejectsUnknownArchiveStore(t *testing.T) {
	_, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[archive]
store = "magicbox"
`))
	if err == nil {
		t.Fatal("expected error for unknown archive store")
	}
}

// TestLoadRejectsUnimplementedArchiveStore guards the §19/build-reality gap:
// tarS3, volumeSnapshot and longhorn are recognized store names but only
// tarLocal is implemented in this build. A config naming one of them must be
// rejected at load — otherwise felis-api boots green while the reaper CronJob
// fails every run and restore silently 503s. The error must point the operator
// at the fix (tarLocal), distinct from the "unknown store" message.
func TestLoadRejectsUnimplementedArchiveStore(t *testing.T) {
	for _, store := range []string{"tarS3", "volumeSnapshot", "longhorn"} {
		t.Run(store, func(t *testing.T) {
			_, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[archive]
store = "`+store+`"
`))
			if err == nil {
				t.Fatalf("expected error for recognized-but-unimplemented store %q", store)
			}
			if !strings.Contains(err.Error(), "tarLocal") {
				t.Errorf("error for %q should point at the tarLocal remediation, got: %v", store, err)
			}
		})
	}
}

// TestLoadRejectsSchemeQualifiedRegistryURL guards the §24 split-brain: the
// registry url is a bare host[:port], read scheme-tolerantly by the admin build
// path (registryHost strips the scheme) but scheme-INtolerantly by the
// user-modpack lane (deriveImageRef concatenates raw). A scheme-qualified url
// would boot felis-api green and 500 every approve while admin builds keep
// working, so it must be rejected at load with the bare-host contract spelled
// out. Both http:// and https:// are caught (the check is on "://").
func TestLoadRejectsSchemeQualifiedRegistryURL(t *testing.T) {
	for _, url := range []string{"http://registry.felis.svc:5000", "https://registry.felis.svc:5000"} {
		t.Run(url, func(t *testing.T) {
			_, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[registry]
url = "`+url+`"
`))
			if err == nil {
				t.Fatalf("expected error for scheme-qualified registry url %q", url)
			}
			if !strings.Contains(err.Error(), "scheme") {
				t.Errorf("error for %q should explain the bare-host contract, got: %v", url, err)
			}
		})
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
typo_field = "oops"
[database]
url = "postgres://felis@db/felis"
`))
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}
