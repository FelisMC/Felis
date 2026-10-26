package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
kaniko_image = "registry.felis.svc:5000/mirror/kaniko:v1.23.2"
trivy_image = "registry.felis.svc:5000/mirror/trivy:0.58.1"
build_cpu_limit = "1"
build_mem_limit = "2Gi"
build_disk_limit = "8Gi"
build_user_namespaces = "off"
build_runtime_class = "gvisor"

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
	if cfg.Registry.KanikoImage != "registry.felis.svc:5000/mirror/kaniko:v1.23.2" ||
		cfg.Registry.TrivyImage != "registry.felis.svc:5000/mirror/trivy:0.58.1" {
		t.Errorf("build image overrides = %q / %q", cfg.Registry.KanikoImage, cfg.Registry.TrivyImage)
	}
	if cfg.Registry.BuildCPULimit != "1" || cfg.Registry.BuildMemLimit != "2Gi" {
		t.Errorf("build resource overrides = %q / %q", cfg.Registry.BuildCPULimit, cfg.Registry.BuildMemLimit)
	}
	if r := cfg.Registry; r.BuildDiskLimit != "8Gi" || r.BuildUserNamespaces != "off" || r.BuildRuntimeClass != "gvisor" {
		t.Errorf("build isolation overrides = %q / %q / %q", r.BuildDiskLimit, r.BuildUserNamespaces, r.BuildRuntimeClass)
	}
}

func TestLoadRejectsUnknownBuildUserNamespaces(t *testing.T) {
	_, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[registry]
build_user_namespaces = "yes"
`))
	if err == nil || !strings.Contains(err.Error(), "build_user_namespaces") {
		t.Fatalf("err = %v, want build_user_namespaces rejected", err)
	}
}

func TestLoadGamePort(t *testing.T) {
	base := `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[velocity]
`
	cfg, err := config.Load(writeTOML(t, base+"game_port = 25570\n"))
	if err != nil || cfg.Velocity.GamePort != 25570 {
		t.Fatalf("game_port 25570: cfg=%+v err=%v", cfg, err)
	}
	for _, bad := range []string{"-1", "65536"} {
		if _, err := config.Load(writeTOML(t, base+"game_port = "+bad+"\n")); err == nil || !strings.Contains(err.Error(), "game_port") {
			t.Errorf("game_port %s: err = %v, want it rejected", bad, err)
		}
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

func TestLoadDatabaseDeployment(t *testing.T) {
	withDeployment := func(v string) string {
		return strings.Replace(validTOML, `url = "postgres://felis:secret@db:5432/felis"`,
			`url = "postgres://felis:secret@db:5432/felis"`+"\ndeployment = \""+v+"\"", 1)
	}
	cfg, err := config.Load(writeTOML(t, withDeployment("felis/felis-postgres")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Deployment != "felis/felis-postgres" {
		t.Fatalf("deployment = %q", cfg.Database.Deployment)
	}
	for _, bad := range []string{"felis-postgres", "felis/", "/felis-postgres", "Felis/pg", "felis/pg; rm -rf /", "a/b/c"} {
		if _, err := config.Load(writeTOML(t, withDeployment(bad))); err == nil || !strings.Contains(err.Error(), "namespace/name") {
			t.Errorf("deployment %q: err = %v, want a namespace/name refusal", bad, err)
		}
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

func TestLoadScanPolicy(t *testing.T) {
	cfg, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[registry]
scan_fail_on = ["critical", " High "]
scan_fail_unfixed = true
scan_accept = [" CVE-2021-35515 ", "aws-access-key-id"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Registry.ScanFailOn, ","); got != "CRITICAL,HIGH" || !cfg.Registry.ScanFailUnfixed {
		t.Errorf("scan policy = %q, unfixed %t", got, cfg.Registry.ScanFailUnfixed)
	}
	if got := strings.Join(cfg.Registry.ScanAccept, ","); got != "CVE-2021-35515,aws-access-key-id" {
		t.Errorf("scan_accept = %q", got)
	}
	_, err = config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[registry]
scan_accept = ["CVE-2021-35515,CVE-2025-67030"]
`))
	if err == nil || err.Error() != `config: [registry] scan_accept "CVE-2021-35515,CVE-2025-67030" must be a vulnerability id or secret rule id (letters, digits, and . _ : -)` {
		t.Errorf("err = %v", err)
	}
	_, err = config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[registry]
scan_fail_on = ["HIGH", "SEVERE"]
`))
	if err == nil || err.Error() != `config: [registry] scan_fail_on "SEVERE" must be one of CRITICAL, HIGH, MEDIUM, LOW, UNKNOWN` {
		t.Errorf("err = %v", err)
	}
}

func TestLoadAuthSourcesPreservesOrder(t *testing.T) {
	cfg, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[[auth_source]]
tag = "littleskin"
prefix = "LS"
url = "https://littleskin.example.net/api/yggdrasil/sessionserver/session/minecraft/hasJoined"
[[auth_source]]
tag = "guild"
prefix = "GD"
url = "https://guild.example.net/sessionserver/session/minecraft/hasJoined"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.AuthSources) != 2 {
		t.Fatalf("auth sources = %d, want 2", len(cfg.AuthSources))
	}
	if cfg.AuthSources[0].Tag != "littleskin" || cfg.AuthSources[1].Tag != "guild" {
		t.Errorf("source order = %q,%q, want littleskin,guild", cfg.AuthSources[0].Tag, cfg.AuthSources[1].Tag)
	}
}

func TestLoadRejectsAuthSourceIdentityKey(t *testing.T) {
	const source = `
[[auth_source]]
tag = "evil"
prefix = "EV"
url = "https://evil.example.net/hasJoined"
identity = true
`
	_, errFull := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
`+source))
	_, errNano := config.LoadNano(writeTOML(t, source))
	for loader, err := range map[string]error{"Load": errFull, "LoadNano": errNano} {
		if err == nil || !strings.Contains(err.Error(), "unknown keys") || !strings.Contains(err.Error(), "identity") {
			t.Errorf("%s: err = %v, want the identity key rejected as unknown", loader, err)
		}
	}
}

func TestLoadRejectsDuplicateAuthSourceTag(t *testing.T) {
	_, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[[auth_source]]
tag = "dup"
prefix = "AA"
url = "https://a.example.net/hasJoined"
[[auth_source]]
tag = "dup"
prefix = "BB"
url = "https://b.example.net/hasJoined"
`))
	if err == nil {
		t.Fatal("expected error for duplicate auth_source tag")
	}
	if !strings.Contains(err.Error(), "unique") {
		t.Errorf("error should explain the tags-must-be-unique contract, got: %v", err)
	}
}

func TestLoadRejectsColonInAuthSourceTag(t *testing.T) {
	const sources = `
[[auth_source]]
tag = "guild"
prefix = "GD"
url = "https://a.example.net/hasJoined"
[[auth_source]]
tag = "guild:eu"
prefix = "GE"
url = "https://b.example.net/hasJoined"
`
	_, errFull := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
`+sources))
	_, errNano := config.LoadNano(writeTOML(t, sources))
	for loader, err := range map[string]error{"Load": errFull, "LoadNano": errNano} {
		if err == nil {
			t.Errorf("%s accepted a tag containing ':'", loader)
			continue
		}
		if !strings.Contains(err.Error(), `"guild:eu"`) || !strings.Contains(err.Error(), "':'") {
			t.Errorf("%s: error should name the tag and the ':' rule, got: %v", loader, err)
		}
	}
}

func TestLoadRejectsPaddedAuthSourceTag(t *testing.T) {
	for _, tag := range []string{"littleskin ", " littleskin", "littleskin\t"} {
		_, err := config.LoadNano(writeTOML(t, "[[auth_source]]\ntag = \""+tag+"\"\nprefix = \"LS\"\nurl = \"https://a.example.net/hasJoined\"\n"))
		if err == nil || !strings.Contains(err.Error(), "whitespace") {
			t.Errorf("tag %q: err = %v, want a whitespace refusal", tag, err)
		}
	}
}

func TestLoadRejectsMojangAuthSourceTag(t *testing.T) {
	for _, tag := range []string{"mojang", "Mojang"} {
		_, err := config.LoadNano(writeTOML(t, "[[auth_source]]\ntag = \""+tag+"\"\nprefix = \"MJ\"\nurl = \"https://sessionserver.mojang.com/session/minecraft/hasJoined\"\n"))
		if err == nil || !strings.Contains(err.Error(), "built in") {
			t.Errorf("tag %q: err = %v, want a refusal saying Mojang is built in", tag, err)
		}
	}
}

func TestLoadRejectsSchemelessAuthSourceURL(t *testing.T) {
	_, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[[auth_source]]
tag = "bare"
prefix = "BR"
url = "bare.example.net/hasJoined"
`))
	if err == nil {
		t.Fatal("expected error for schemeless auth_source url")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error should explain the scheme contract, got: %v", err)
	}
}

func TestLoadRejectsUnqueryableAuthSourceURL(t *testing.T) {
	for _, u := range []string{
		"ftp://a.example.net/hasJoined",
		"https://",
		"https://a.example.net/hasJoined?token=x",
		"https://a.example.net/hasJoined?",
		"https://a.example.net/hasJoined#x",
		"https://a.example.net/hasJoined ",
		"https://a.example.net:bad/hasJoined",
	} {
		_, err := config.LoadNano(writeTOML(t, "[[auth_source]]\ntag = \"a\"\nprefix = \"AA\"\nurl = \""+u+"\"\n"))
		if err == nil {
			t.Errorf("url %q loaded; it can never be queried", u)
		}
	}
	if _, err := config.LoadNano(writeTOML(t, "[[auth_source]]\ntag = \"a\"\nprefix = \"AA\"\nurl = \"http://127.0.0.1:8080/hasJoined\"\n")); err != nil {
		t.Errorf("a plain loopback endpoint must load: %v", err)
	}
}

func TestAuthSourceProfileAPIURL(t *testing.T) {
	for _, tc := range []struct {
		url   string
		valid bool
	}{
		{"https://ygg.example.net/api/yggdrasil", true},
		{"http://127.0.0.1:8080/ygg", true},
		{"ygg.example.net/api", false},
		{"http://ygg.example.net/api", false},
		{"https://ygg.example.net/api?token=x", false},
	} {
		cfg, err := config.LoadNano(writeTOML(t, "[[auth_source]]\ntag = \"a\"\nprefix = \"AA\"\nurl = \"https://ygg.example.net/custom-check\"\napi_url = \""+tc.url+"\"\n"))
		if tc.valid {
			if err != nil || cfg.AuthSources[0].APIURL != tc.url {
				t.Fatalf("api_url %q = %v, %v", tc.url, cfg, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "api_url") {
			t.Fatalf("invalid api_url %q: %v", tc.url, err)
		}
	}
}

func TestLoadRejectsPlaintextPublicAuthSource(t *testing.T) {
	load := func(u string) error {
		_, err := config.LoadNano(writeTOML(t, "[[auth_source]]\ntag = \"a\"\nprefix = \"AA\"\nurl = \""+u+"\"\n"))
		return err
	}
	for _, host := range []string{"ygg.example.net", "203.0.113.9", "ygg.lan", "172.32.0.1", "169.254.1.1", "0.0.0.0", "[2001:db8::1]"} {
		u := "http://" + host + "/hasJoined"
		if err := load(u); err == nil || !strings.Contains(err.Error(), "https://") {
			t.Errorf("url %q: err = %v, want a refusal that asks for https://", u, err)
		}
		if err := load("https://" + host + "/hasJoined"); err != nil {
			t.Errorf("the same host over https must load: %v", err)
		}
	}
	for _, host := range []string{"localhost", "LOCALHOST:8080", "127.0.0.1:8080", "127.1.2.3", "[::1]:8080", "10.0.0.5", "172.16.3.4", "192.168.1.2", "[fd00::1]"} {
		if err := load("http://" + host + "/hasJoined"); err != nil {
			t.Errorf("a plaintext root on %s must load: %v", host, err)
		}
	}
}

func TestLoadNanoAcceptsMinimalConfig(t *testing.T) {
	cfg, err := config.LoadNano(writeTOML(t, `
[[auth_source]]
tag = "littleskin"
prefix = "LS"
url = "https://littleskin.example.net/api/yggdrasil/sessionserver/session/minecraft/hasJoined"
`))
	if err != nil {
		t.Fatalf("LoadNano minimal: %v", err)
	}
	if len(cfg.AuthSources) != 1 || cfg.AuthSources[0].Tag != "littleskin" {
		t.Fatalf("auth sources = %+v, want one littleskin source", cfg.AuthSources)
	}
}

func TestLoadNanoStillEnforcesAuthSourceRules(t *testing.T) {
	_, err := config.LoadNano(writeTOML(t, `
[[auth_source]]
tag = "dup"
prefix = "AA"
url = "https://a.example.net/hasJoined"
[[auth_source]]
tag = "dup"
prefix = "BB"
url = "https://b.example.net/hasJoined"
`))
	if err == nil {
		t.Fatal("expected LoadNano to reject a duplicate auth_source tag")
	}
	if !strings.Contains(err.Error(), "unique") {
		t.Errorf("error should explain the tags-must-be-unique contract, got: %v", err)
	}
}

func TestLoadRejectsBadAuthSourcePrefix(t *testing.T) {
	for name, prefix := range map[string]string{
		"missing":    "",
		"too long":   "TOOLONG",
		"underscore": "L_",
		"non-ascii":  "皮肤",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.LoadNano(writeTOML(t, `
[[auth_source]]
tag = "littleskin"
prefix = "`+prefix+`"
url = "https://littleskin.example.net/hasJoined"
`))
			if err == nil {
				t.Fatalf("expected error for prefix %q", prefix)
			}
			if !strings.Contains(err.Error(), "prefix") {
				t.Errorf("error should name the prefix contract, got: %v", err)
			}
		})
	}
}

func TestLoadRejectsDuplicateAuthSourcePrefix(t *testing.T) {
	_, err := config.LoadNano(writeTOML(t, `
[[auth_source]]
tag = "littleskin"
prefix = "LS"
url = "https://a.example.net/hasJoined"
[[auth_source]]
tag = "otherskin"
prefix = "ls"
url = "https://b.example.net/hasJoined"
`))
	if err == nil {
		t.Fatal("expected LoadNano to reject two sources sharing a prefix (case-insensitively)")
	}
	if !strings.Contains(err.Error(), "prefix") {
		t.Errorf("error should name the prefix contract, got: %v", err)
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

func TestLoadSMTPDefaultsPort(t *testing.T) {
	cfg, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[smtp]
host = "smtp.example.net"
from = "felis@example.net"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SMTP.Port != 587 {
		t.Errorf("smtp port = %d, want the 587 default", cfg.SMTP.Port)
	}

	cfg, err = config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
`))
	if err != nil {
		t.Fatalf("Load without [smtp]: %v", err)
	}
	if cfg.SMTP.Host != "" || cfg.SMTP.Port != 0 {
		t.Errorf("absent [smtp] must stay zero, got %+v", cfg.SMTP)
	}
}

func TestSMTPRequireTLS(t *testing.T) {
	load := func(smtp string) config.SMTPConfig {
		t.Helper()
		cfg, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[smtp]
from = "felis@example.net"
`+smtp))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg.SMTP
	}
	cases := []struct {
		smtp string
		want bool
	}{
		{`host = "smtp.example.net"`, true},
		{`host = "10.0.0.5"`, true},
		{`host = "localhost"`, false},
		{`host = "LocalHost"`, false},
		{`host = "127.0.0.1"`, false},
		{`host = "127.0.0.53"`, false},
		{`host = "::1"`, false},
		{"host = \"smtp.example.net\"\nrequire_tls = false", false},
		{"host = \"127.0.0.1\"\nrequire_tls = true", true},
	}
	for _, c := range cases {
		if got := load(c.smtp).TLSRequired(); got != c.want {
			t.Errorf("%q: TLSRequired = %v, want %v", c.smtp, got, c.want)
		}
	}
}

func TestLoadRejectsSMTPWithoutFrom(t *testing.T) {
	_, err := config.Load(writeTOML(t, `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
[smtp]
host = "smtp.example.net"
`))
	if err == nil {
		t.Fatal("expected error when [smtp] host is set without a from address")
	}
}

func TestClientIPHeaderAndMailCap(t *testing.T) {
	base := `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
`
	for _, tc := range []struct {
		name, auth, want string
	}{
		{"unset", "", ""},
		{"cloudflare audience implies CF-Connecting-IP", "access_jwt_aud = \"aud123\"\n", "CF-Connecting-IP"},
		{"explicit wins", "access_jwt_aud = \"aud123\"\nclient_ip_header = \"X-Forwarded-For\"\n", "X-Forwarded-For"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeTOML(t, base+"[auth]\n"+tc.auth))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Auth.EffectiveClientIPHeader(); got != tc.want {
				t.Fatalf("EffectiveClientIPHeader = %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := config.Load(writeTOML(t, base+"[auth]\nclient_ip_header = \"CF-Connecting-IP: 1.2.3.4\"\n")); err == nil {
		t.Fatal("a header line with a value was accepted as a header name")
	}
	if _, err := config.Load(writeTOML(t, base+"[smtp]\nhost = \"smtp.example.net\"\nfrom = \"f@example.net\"\nmax_per_hour = -1\n")); err == nil {
		t.Fatal("negative max_per_hour accepted")
	}
}

func TestLoadOffsite(t *testing.T) {
	const head = `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
`
	cfg, err := config.Load(writeTOML(t, head))
	if err != nil {
		t.Fatalf("Load without [offsite]: %v", err)
	}
	if cfg.Offsite.Enabled() || cfg.Offsite.KeyRef != "" {
		t.Fatalf("absent [offsite] must stay off and zero, got %+v", cfg.Offsite)
	}

	cfg, err = config.Load(writeTOML(t, head+`
[offsite]
endpoint = "https://s3.example.net"
bucket = "felis-copies"
prefix = "site-a"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Offsite.Enabled() {
		t.Fatalf("endpoint and bucket set but offsite is off: %+v", cfg.Offsite)
	}

	for name, body := range map[string]string{
		"no bucket":        "endpoint = \"https://s3.example.net\"",
		"no endpoint":      "bucket = \"b\"",
		"path in endpoint": "endpoint = \"https://s3.example.net/b\"\nbucket = \"b\"",
		"ftp endpoint":     "endpoint = \"ftp://s3.example.net\"\nbucket = \"b\"",
		"bucket with key":  "endpoint = \"s3.example.net\"\nbucket = \"b/x\"",
		"negative keep":    "endpoint = \"s3.example.net\"\nbucket = \"b\"\ndb_keep = -1",
		"secret in toml":   "endpoint = \"s3.example.net\"\nbucket = \"b\"\nsecret_key = \"x\"",
	} {
		if _, err := config.Load(writeTOML(t, head+"[offsite]\n"+body+"\n")); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestAuditRetention(t *testing.T) {
	const head = `
[server]
root_domain = "mc.example.net"
[database]
url = "postgres://felis@db/felis"
`
	const day = 24 * time.Hour
	for _, tc := range []struct {
		name, table string
		want        time.Duration
	}{
		{"unset", "", 365 * day},
		{"empty", "[audit]\nretention = \"\"\n", 365 * day},
		{"forever", "[audit]\nretention = \"forever\"\n", 0},
		{"days", "[audit]\nretention = \"90d\"\n", 90 * day},
		{"months", "[audit]\nretention = \"18mo\"\n", 540 * day},
		{"hours", "[audit]\nretention = \"720h\"\n", 30 * day},
		{"exactly the minimum", "[audit]\nretention = \"30d\"\n", 30 * day},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeTOML(t, head+tc.table))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := cfg.Audit.RetentionPeriod()
			if err != nil || got != tc.want {
				t.Fatalf("RetentionPeriod = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct{ value, wantErr string }{
		{"29d", `config: [audit] retention "29d" is shorter than the 30d minimum`},
		{"719h", `config: [audit] retention "719h" is shorter than the 30d minimum`},
		{"a year", `config: [audit] retention "a year" must be a span such as 365d or 18mo, or forever`},
		{"Forever", `config: [audit] retention "Forever" must be a span such as 365d or 18mo, or forever`},
	} {
		_, err := config.Load(writeTOML(t, head+"[audit]\nretention = \""+tc.value+"\"\n"))
		if err == nil || err.Error() != tc.wantErr {
			t.Errorf("retention %q: err = %v, want %q", tc.value, err, tc.wantErr)
		}
	}
}
