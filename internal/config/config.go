// Package config loads and validates felis.toml (spec §24). root_domain lives
// here and nowhere else in code: every FQDN is composed at runtime as
// subdomain + "." + root_domain, so changing the deployment domain is a
// one-line config edit and the source tree stays domain-agnostic.
package config

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the parsed felis.toml.
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Database DatabaseConfig `toml:"database"`
	Velocity VelocityConfig `toml:"velocity"`
	Auth     AuthConfig     `toml:"auth"`
	K8s      K8sConfig      `toml:"k8s"`
	Registry RegistryConfig `toml:"registry"`
	Archive  ArchiveConfig  `toml:"archive"`
}

// ServerConfig is the [server] table.
type ServerConfig struct {
	Listen     string `toml:"listen"`
	RootDomain string `toml:"root_domain"`
}

// DatabaseConfig is the [database] table.
type DatabaseConfig struct {
	URL string `toml:"url"`
}

// VelocityConfig is the [velocity] table.
type VelocityConfig struct {
	PublicIP        string `toml:"public_ip"`
	ServiceTokenRef string `toml:"service_token_ref"`
	// LoginImage is the container image for the always-on "login" limbo (the
	// LOOHP/Limbo auth gate). setup provisions the login system service only when
	// this is set; empty means "don't guess" — setup skips the login server and
	// says so, the same fail-loud stance manifests takes for images it cannot
	// safely default. No official LOOHP/Limbo image exists, so a deployment builds
	// one (see deploy/limbo) and points this at the pushed ref.
	LoginImage string `toml:"login_image"`
	// LobbyImage is the container image for the always-on "lobby" hub (Paper plus
	// the felis-paper /menu plugin). Same skip-when-empty contract as LoginImage.
	LobbyImage string `toml:"lobby_image"`
}

// AuthConfig is the [auth] table: the two privileged faces and the access-JWT
// audience the API enforces.
type AuthConfig struct {
	AdminHostname string `toml:"admin_hostname"`
	PanelHostname string `toml:"panel_hostname"`
	AccessJWTAud  string `toml:"access_jwt_aud"`
}

// K8sConfig is the [k8s] table.
type K8sConfig struct {
	Namespace   string `toml:"namespace"`
	EgressMode  string `toml:"egress_mode"`
	MetalLBPool string `toml:"metallb_pool"`
}

// RegistryConfig is the [registry] table.
type RegistryConfig struct {
	URL            string `toml:"url"`
	BuildNamespace string `toml:"build_namespace"`
	// UserUploadsContext is the object-store base under which a user-submitted
	// modpack's Kaniko build context is pinned. It belongs to the §16 build
	// subsystem's input domain (the build-context store), introduced by the
	// user-directed modpack approval lane (see internal/submit package doc). The
	// lane derives {UserUploadsContext}/{submissionID}/context.tar.gz; the upload
	// transport that places the blob there is a separate, deferred integration
	// (INTEGRATION-ONLY). It is kept distinct from [archive] on purpose — a world
	// archive (§19 WorldArchiver) and a build context (§16) are different artifacts
	// with different lifecycles, so the two must not share a store binding.
	UserUploadsContext string `toml:"user_uploads_context"`
	// S3 configures the object-store backend for user_uploads_context when it is an
	// s3:// base (the alternative to a local uploads path). It mirrors
	// ArchiveS3Config: Endpoint + Region locate the store and the *Ref fields NAME
	// the environment variables felis-api reads the credentials from — never the
	// secrets themselves, so no S3 key is ever written into felis.toml. The setup
	// wizard injects those env vars into felis-api from a separate Secret
	// (felis-uploads-s3). The bucket (and any key prefix) is taken from
	// user_uploads_context itself, so it is not duplicated here. Empty for a
	// local-storage install.
	S3 RegistryS3Config `toml:"s3"`
}

// RegistryS3Config is the [registry.s3] subtable: the object-store coordinates for
// a user_uploads_context that is an s3:// base. It deliberately reads like
// ArchiveS3Config (endpoint + credential refs) so the two S3 bindings are
// consistent, but omits Bucket because the s3:// base already carries it.
type RegistryS3Config struct {
	Endpoint     string `toml:"endpoint"`
	Region       string `toml:"region"`
	AccessKeyRef string `toml:"access_key_ref"`
	SecretKeyRef string `toml:"secret_key_ref"`
}

// ArchiveConfig is the [archive] table plus its [archive.s3] subtable (spec §19).
type ArchiveConfig struct {
	Store         string          `toml:"store"`
	LocalPath     string          `toml:"local_path"`
	Retention     string          `toml:"retention"`
	WarnBefore    []string        `toml:"warn_before"`
	MaxLocalBytes string          `toml:"max_local_bytes"`
	S3            ArchiveS3Config `toml:"s3"`
}

// ArchiveS3Config is the [archive.s3] subtable.
type ArchiveS3Config struct {
	Endpoint     string `toml:"endpoint"`
	Bucket       string `toml:"bucket"`
	AccessKeyRef string `toml:"access_key_ref"`
	SecretKeyRef string `toml:"secret_key_ref"`
}

// archive store backends recognized by §19.
var archiveStores = map[string]struct{}{
	"tarLocal":       {},
	"tarS3":          {},
	"volumeSnapshot": {},
	"longhorn":       {},
}

// archive store backends this build can actually honor. §19 names four, but only
// tarLocal is implemented: the reaper's buildArchiver, the `felis restore`
// command, and the felis-api restore executor all construct tarLocal and nothing
// else. A config naming a recognized-but-unimplemented store is a footgun — it
// clears the "is this a real store name" check yet silently breaks retention (the
// reaper CronJob fails every run) and restore (503), while felis-api otherwise
// looks healthy. Validate rejects it so every binary that loads config (migrate,
// api, reaper) fails fast at startup with a clear remediation instead. (`felis
// restore` enforces the same invariant on its own --store flag: it runs inside
// the sandboxed weak-SA restore Job and by design never loads felis.toml or holds
// DB credentials, so it cannot lean on this load-time check.)
var implementedArchiveStores = map[string]struct{}{
	"tarLocal": {},
}

// Defaults that callers get when the field is omitted.
const (
	defaultListen     = "0.0.0.0:8080"
	defaultNamespace  = "minecraft"
	defaultEgressMode = "loadbalancer"
	defaultStore      = "tarLocal"
	// defaultUserUploadsContext is a non-empty, platform-namespaced placeholder so
	// the modpack approval lane's derived context ref is well-formed even before a
	// deployment points it at a real object store. The blob transport is deferred,
	// so this base only has to be a sensible, parseable prefix (see the §16 build
	// subsystem and the internal/submit package doc for the lane's provenance).
	defaultUserUploadsContext = "s3://felis-user-uploads"
)

// Load reads and validates a felis.toml from path.
func Load(path string) (*Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		// Surface typos rather than silently ignoring unknown keys.
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("config: unknown keys in %s: %s", path, strings.Join(keys, ", "))
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = defaultListen
	}
	if c.K8s.Namespace == "" {
		c.K8s.Namespace = defaultNamespace
	}
	if c.K8s.EgressMode == "" {
		c.K8s.EgressMode = defaultEgressMode
	}
	if c.Archive.Store == "" {
		c.Archive.Store = defaultStore
	}
	if c.Registry.UserUploadsContext == "" {
		c.Registry.UserUploadsContext = defaultUserUploadsContext
	}
}

// Validate enforces the mandatory fields (spec §24: database.url is 强制) and
// the closed value sets.
func (c *Config) Validate() error {
	if c.Database.URL == "" {
		return fmt.Errorf("config: [database] url is required")
	}
	if c.Server.RootDomain == "" {
		return fmt.Errorf("config: [server] root_domain is required")
	}
	if !strings.Contains(c.Server.RootDomain, ".") {
		return fmt.Errorf("config: [server] root_domain %q is not a domain", c.Server.RootDomain)
	}
	if _, ok := archiveStores[c.Archive.Store]; !ok {
		return fmt.Errorf("config: [archive] store %q is not one of tarLocal|tarS3|volumeSnapshot|longhorn", c.Archive.Store)
	}
	if _, ok := implementedArchiveStores[c.Archive.Store]; !ok {
		return fmt.Errorf("config: [archive] store %q is recognized by §19 but not implemented in this build — only tarLocal is supported; set store = \"tarLocal\"", c.Archive.Store)
	}
	switch c.K8s.EgressMode {
	case "loadbalancer", "nodeport":
	default:
		return fmt.Errorf("config: [k8s] egress_mode %q must be loadbalancer or nodeport", c.K8s.EgressMode)
	}
	// The registry URL is a bare host[:port] (spec §24: url="registry.felis.svc:5000"),
	// never a scheme-qualified URL. This is not cosmetic: two consumers read it with
	// different robustness. The admin build path normalizes via registryHost() (which
	// strips a scheme), but the user-modpack approval lane derives its push target by
	// string concatenation (submit.Manager.deriveImageRef → "{url}/user-uploads/{id}:latest")
	// with no stripping. A "http://" prefix would make the lane's pre-CAS build.Validate
	// reject every derived ref (imageNameRE forbids the leading "http:/…") and collapse
	// EVERY approve to 500 while admin builds keep working — a silent split-brain. Fail
	// fast at load instead, with the contract spelled out.
	if c.Registry.URL != "" && strings.Contains(c.Registry.URL, "://") {
		return fmt.Errorf("config: [registry] url %q must be a bare host[:port] with no scheme (e.g. registry.felis.svc:5000); a scheme breaks the user-modpack build lane's derived push target", c.Registry.URL)
	}
	return nil
}
