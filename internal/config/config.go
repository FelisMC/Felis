// Package config loads and validates felis.toml.
package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

var scanIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

type Config struct {
	Server   ServerConfig   `toml:"server"`
	Database DatabaseConfig `toml:"database"`
	Velocity VelocityConfig `toml:"velocity"`
	Auth     AuthConfig     `toml:"auth"`
	K8s      K8sConfig      `toml:"k8s"`
	Registry RegistryConfig `toml:"registry"`
	Archive  ArchiveConfig  `toml:"archive"`
	Offsite  OffsiteConfig  `toml:"offsite"`
	SMTP     SMTPConfig     `toml:"smtp"`
	Audit    AuditConfig    `toml:"audit"`
	// AuthSources order sets priority; Mojang remains the code-owned identity anchor.
	AuthSources []AuthSourceConfig `toml:"auth_source"`
}

// AuthSourceConfig.Tag is permanent and hashes into player UUIDs; Prefix disambiguates player names.
type AuthSourceConfig struct {
	Tag    string `toml:"tag" json:"tag"`
	Prefix string `toml:"prefix" json:"prefix"`
	URL    string `toml:"url" json:"url"`
	// APIURL is optional for sources whose hasJoined URL does not use the standard path.
	APIURL string `toml:"api_url" json:"api_url"`
}

// Empty SMTP host disables email; PasswordRef names an environment variable.
type SMTPConfig struct {
	Host string `toml:"host"`
	// Port defaults to 587 (STARTTLS); 465 selects implicit TLS.
	Port int    `toml:"port"`
	From string `toml:"from"`
	// Username is the AUTH identity; empty means no AUTH.
	Username    string `toml:"username"`
	PasswordRef string `toml:"password_ref"`
	// MaxPerHour caps install-wide mail; 0 means DefaultMailPerHour.
	MaxPerHour int `toml:"max_per_hour"`
	// RequireTLS refuses a relay that does not offer STARTTLS (except on port 465); see TLSRequired.
	RequireTLS *bool `toml:"require_tls,omitempty"`
}

// TLSRequired reports require_tls when set, otherwise true unless the relay is localhost or a loopback address.
func (c SMTPConfig) TLSRequired() bool {
	if c.RequireTLS != nil {
		return *c.RequireTLS
	}
	if strings.EqualFold(c.Host, "localhost") {
		return false
	}
	ip := net.ParseIP(c.Host)
	return ip == nil || !ip.IsLoopback()
}

const DefaultMailPerHour = 120

type ServerConfig struct {
	Listen     string `toml:"listen"`
	RootDomain string `toml:"root_domain"`
}

type DatabaseConfig struct {
	URL string `toml:"url"`
	// Deployment is the database Deployment as "namespace/name", host copy only; pg tools run inside its container. Empty runs them from PATH against url.
	Deployment string `toml:"deployment"`
}

type VelocityConfig struct {
	PublicIP        string `toml:"public_ip"`
	ServiceTokenRef string `toml:"service_token_ref"`
	// LoginImage is the login limbo image; setup skips the login server when empty.
	LoginImage string `toml:"login_image"`
	// LobbyImage is the lobby hub image; skipped when empty.
	LobbyImage string `toml:"lobby_image"`
	// GamePort is the public proxy port; 0 means Minecraft's default, 25565.
	GamePort int `toml:"game_port"`
	// GameVersion is the login/lobby protocol version; empty means unknown (custom images).
	GameVersion string `toml:"game_version"`
}

// AuthConfig is the [auth] table; the API does not verify Access JWTs, a set audience only marks an install behind Cloudflare.
type AuthConfig struct {
	AdminHostname string `toml:"admin_hostname"`
	PanelHostname string `toml:"panel_hostname"`
	AccessJWTAud  string `toml:"access_jwt_aud"`
	// ClientIPHeader defaults to the TCP peer, or CF-Connecting-IP when AccessJWTAud is set.
	ClientIPHeader string `toml:"client_ip_header"`
}

func (a AuthConfig) EffectiveClientIPHeader() string {
	if a.ClientIPHeader != "" {
		return a.ClientIPHeader
	}
	if a.AccessJWTAud != "" {
		return "CF-Connecting-IP"
	}
	return ""
}

type K8sConfig struct {
	Namespace   string `toml:"namespace"`
	EgressMode  string `toml:"egress_mode"`
	MetalLBPool string `toml:"metallb_pool"`
}

type RegistryConfig struct {
	URL            string `toml:"url"`
	BuildNamespace string `toml:"build_namespace"`
	// KanikoImage, TrivyImage, BuildCPULimit and BuildMemLimit override the build defaults (the mirror/ copies in this registry, 2 CPU / 4Gi). Empty keeps the default.
	KanikoImage   string `toml:"kaniko_image"`
	TrivyImage    string `toml:"trivy_image"`
	BuildCPULimit string `toml:"build_cpu_limit"`
	BuildMemLimit string `toml:"build_mem_limit"`
	// BuildDiskLimit caps a build pod's ephemeral storage; empty keeps 12Gi.
	BuildDiskLimit string `toml:"build_disk_limit"`
	// BuildUserNamespaces is "auto" (default; on when the startup probe pod ran with it), "on" or "off".
	BuildUserNamespaces string `toml:"build_user_namespaces"`
	// BuildRuntimeClass runs build pods under a sandbox RuntimeClass such as gVisor; empty uses the node default.
	BuildRuntimeClass string `toml:"build_runtime_class"`
	// MaxConcurrentBuilds caps concurrent builds; zero keeps 2, the maximum is 6 (the build namespace pod quota).
	MaxConcurrentBuilds int `toml:"max_concurrent_builds"`
	// TrivyDBRepository is the OCI repository for --db-repository; empty keeps <url>/mirror/trivy-db:2 because the build egress lock denies Trivy's default.
	TrivyDBRepository string `toml:"trivy_db_repository"`
	// TrivyJavaDBRepository is the --java-db-repository; empty keeps <url>/mirror/trivy-java-db:1.
	TrivyJavaDBRepository string `toml:"trivy_java_db_repository"`
	// ScanFailOn lists the severities that block a built image; empty keeps CRITICAL (build.DefaultScanFailOn).
	ScanFailOn []string `toml:"scan_fail_on"`
	// ScanFailUnfixed also blocks vulnerabilities with no fixed release.
	ScanFailUnfixed bool `toml:"scan_fail_unfixed"`
	// ScanAccept lists vulnerability and secret rule ids accepted as known risks (listed, never blocking).
	ScanAccept []string `toml:"scan_accept"`
	// UserUploadsContext stores {submissionID}/context.tar.gz under a local or s3:// base.
	UserUploadsContext string `toml:"user_uploads_context"`
	// UserUploadsMaxBytes caps all users' uploaded contexts together, as a quantity; empty keeps 4Gi.
	UserUploadsMaxBytes string `toml:"user_uploads_max_bytes"`
	// ContextMaxBytes defaults to 1Gi; uploads use 32 MiB parts to fit Cloudflare limits.
	ContextMaxBytes string `toml:"context_max_bytes"`
	// S3 configures the store for an s3:// user_uploads_context; the bucket comes from that value.
	S3 RegistryS3Config `toml:"s3"`
}

// RegistryS3Config is the [registry.s3] subtable; the *Ref fields name environment variables, never secrets.
type RegistryS3Config struct {
	Endpoint     string `toml:"endpoint"`
	Region       string `toml:"region"`
	AccessKeyRef string `toml:"access_key_ref"`
	SecretKeyRef string `toml:"secret_key_ref"`
}

// ArchiveConfig bounds manual, scheduled, and released-world backups; zero values use defaults.
type ArchiveConfig struct {
	Store              string          `toml:"store"`
	LocalPath          string          `toml:"local_path"`
	Retention          string          `toml:"retention"`
	WarnBefore         []string        `toml:"warn_before"`
	MaxLocalBytes      string          `toml:"max_local_bytes"`
	ManualRetention    string          `toml:"manual_retention"`
	ManualKeep         int             `toml:"manual_keep"`
	ManualCooldown     string          `toml:"manual_cooldown"`
	ScheduledEvery     string          `toml:"scheduled_every"`
	ScheduledKeep      int             `toml:"scheduled_keep"`
	ScheduledRetention string          `toml:"scheduled_retention"`
	S3                 ArchiveS3Config `toml:"s3"`
}

type ArchiveS3Config struct {
	Endpoint     string `toml:"endpoint"`
	Bucket       string `toml:"bucket"`
	AccessKeyRef string `toml:"access_key_ref"`
	SecretKeyRef string `toml:"secret_key_ref"`
}

// AuditConfig is the [audit] table; Retention is "365d", "18mo" or "forever", and empty means DefaultAuditRetention.
type AuditConfig struct {
	Retention string `toml:"retention"`
}

// MinAuditRetention exists because the manual-backup cooldown and incident investigations read recent audit rows.
const (
	DefaultAuditRetention = 365 * 24 * time.Hour
	MinAuditRetention     = 30 * 24 * time.Hour
)

// RetentionPeriod resolves Retention: 0 keeps every row.
func (a AuditConfig) RetentionPeriod() (time.Duration, error) {
	switch v := strings.TrimSpace(a.Retention); v {
	case "":
		return DefaultAuditRetention, nil
	case "forever":
		return 0, nil
	default:
		d, err := ParseSpan(v)
		if err != nil {
			return 0, fmt.Errorf("config: [audit] retention %q must be a span such as 365d or 18mo, or forever", a.Retention)
		}
		if d < MinAuditRetention {
			return 0, fmt.Errorf("config: [audit] retention %q is shorter than the 30d minimum", a.Retention)
		}
		return d, nil
	}
}

// ParseSpan parses retention spans: "3mo" (30-day months), "15d", or any time.ParseDuration unit.
func ParseSpan(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasSuffix(s, "mo"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "mo"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 30 * 24 * time.Hour, nil
	case strings.HasSuffix(s, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return time.ParseDuration(s)
	}
}

// OffsiteConfig enables encrypted copies; the reaper then requires an offsite archive before deleting a world.
type OffsiteConfig struct {
	// Endpoint is https://host[:port]; a bare host means TLS.
	Endpoint string `toml:"endpoint"`
	Region   string `toml:"region"`
	Bucket   string `toml:"bucket"`
	// Prefix is the key prefix, so one bucket can hold several installs.
	Prefix       string `toml:"prefix"`
	AccessKeyRef string `toml:"access_key_ref"`
	SecretKeyRef string `toml:"secret_key_ref"`
	// KeyRef names the variable holding the encryption key; keep a copy off this machine.
	KeyRef string `toml:"key_ref"`
	// DBKeep is how many of the newest database bundles the bucket keeps.
	DBKeep int `toml:"db_keep"`
}

func (o OffsiteConfig) Enabled() bool { return o.Bucket != "" }

const (
	DefaultOffsiteAccessKeyEnv = "FELIS_OFFSITE_ACCESS_KEY"
	DefaultOffsiteSecretKeyEnv = "FELIS_OFFSITE_SECRET_KEY"
	DefaultOffsiteKeyEnv       = "FELIS_OFFSITE_KEY"
	DefaultOffsiteDBKeep       = 30
)

var archiveStores = map[string]struct{}{
	"tarLocal":       {},
	"tarS3":          {},
	"volumeSnapshot": {},
	"longhorn":       {},
}

var implementedArchiveStores = map[string]struct{}{
	"tarLocal": {},
}

const (
	defaultListen     = "0.0.0.0:8080"
	defaultNamespace  = "minecraft"
	defaultEgressMode = "loadbalancer"
	defaultStore      = "tarLocal"
	// defaultUserUploadsContext is a parseable placeholder; with no S3 credentials the upload endpoint answers 503.
	defaultUserUploadsContext = "s3://felis-user-uploads"
	defaultSMTPPort           = 587
)

func decodeConfig(path string) (Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("config: decode %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return cfg, fmt.Errorf("config: unknown keys in %s: %s", path, strings.Join(keys, ", "))
	}
	return cfg, nil
}

func Load(path string) (*Config, error) {
	cfg, err := decodeConfig(path)
	if err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadNano reads a felis.toml for a Felis-nano host: it validates only [[auth_source]] and skips the control-plane requirements.
func LoadNano(path string) (*Config, error) {
	cfg, err := decodeConfig(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.validateAuthSources(); err != nil {
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
	if c.SMTP.Host != "" && c.SMTP.Port == 0 {
		c.SMTP.Port = defaultSMTPPort
	}
	if c.Offsite.Enabled() {
		if c.Offsite.AccessKeyRef == "" {
			c.Offsite.AccessKeyRef = DefaultOffsiteAccessKeyEnv
		}
		if c.Offsite.SecretKeyRef == "" {
			c.Offsite.SecretKeyRef = DefaultOffsiteSecretKeyEnv
		}
		if c.Offsite.KeyRef == "" {
			c.Offsite.KeyRef = DefaultOffsiteKeyEnv
		}
		if c.Offsite.DBKeep == 0 {
			c.Offsite.DBKeep = DefaultOffsiteDBKeep
		}
	}
}

func (c *Config) Validate() error {
	if c.Database.URL == "" {
		return fmt.Errorf("config: [database] url is required")
	}
	if d := c.Database.Deployment; d != "" {
		ns, name, ok := strings.Cut(d, "/")
		if !ok || !dnsLabel.MatchString(ns) || !dnsLabel.MatchString(name) {
			return fmt.Errorf("config: [database] deployment %q is not namespace/name", d)
		}
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
		return fmt.Errorf("config: [archive] store %q is not implemented in this build — only tarLocal is supported; set store = \"tarLocal\"", c.Archive.Store)
	}
	switch c.K8s.EgressMode {
	case "loadbalancer", "nodeport":
	default:
		return fmt.Errorf("config: [k8s] egress_mode %q must be loadbalancer or nodeport", c.K8s.EgressMode)
	}
	// The registry URL is a bare host[:port]: a scheme would break the user-modpack lane, which concatenates it into the push target.
	if c.Registry.URL != "" && strings.Contains(c.Registry.URL, "://") {
		return fmt.Errorf("config: [registry] url %q must be a bare host[:port] with no scheme (e.g. registry.felis.svc:5000); a scheme breaks the user-modpack build lane's derived push target", c.Registry.URL)
	}
	switch c.Registry.BuildUserNamespaces {
	case "", "auto", "on", "off":
	default:
		return fmt.Errorf("config: [registry] build_user_namespaces %q must be auto, on or off", c.Registry.BuildUserNamespaces)
	}
	if p := c.Velocity.GamePort; p < 0 || p > 65535 {
		return fmt.Errorf("config: [velocity] game_port %d must be 1-65535 (0 keeps 25565)", p)
	}
	if n := c.Registry.MaxConcurrentBuilds; n < 0 || n > 6 {
		return fmt.Errorf("config: [registry] max_concurrent_builds %d must be 1-6 (0 keeps 2)", n)
	}
	for i, sev := range c.Registry.ScanFailOn {
		sev = strings.ToUpper(strings.TrimSpace(sev))
		c.Registry.ScanFailOn[i] = sev
		switch sev {
		case "CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN":
		default:
			return fmt.Errorf("config: [registry] scan_fail_on %q must be one of CRITICAL, HIGH, MEDIUM, LOW, UNKNOWN", sev)
		}
	}
	for i, id := range c.Registry.ScanAccept {
		id = strings.TrimSpace(id)
		c.Registry.ScanAccept[i] = id
		if !scanIDPattern.MatchString(id) {
			return fmt.Errorf("config: [registry] scan_accept %q must be a vulnerability id or secret rule id (letters, digits, and . _ : -)", id)
		}
	}
	if c.SMTP.Host != "" {
		if !strings.Contains(c.SMTP.From, "@") {
			return fmt.Errorf("config: [smtp] from %q must be the sender email address codes are mailed as", c.SMTP.From)
		}
		if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
			return fmt.Errorf("config: [smtp] port %d must be 1-65535 (587 STARTTLS, 465 implicit TLS)", c.SMTP.Port)
		}
	}
	if c.SMTP.MaxPerHour < 0 {
		return fmt.Errorf("config: [smtp] max_per_hour %d must be positive (0 means the default %d)", c.SMTP.MaxPerHour, DefaultMailPerHour)
	}
	if err := c.Offsite.validate(); err != nil {
		return err
	}
	if _, err := c.Audit.RetentionPeriod(); err != nil {
		return err
	}
	if h := c.Auth.ClientIPHeader; strings.ContainsAny(h, " :\t\r\n") {
		return fmt.Errorf("config: [auth] client_ip_header %q must be a bare header name such as CF-Connecting-IP or X-Forwarded-For", h)
	}
	return c.validateAuthSources()
}

// Reject partially configured offsite storage; an entirely absent configuration is valid.
func (o OffsiteConfig) validate() error {
	if !o.Enabled() {
		if o.Endpoint != "" || o.Prefix != "" {
			return fmt.Errorf("config: [offsite] names an endpoint or prefix but no bucket; set bucket, or remove the table")
		}
		return nil
	}
	if strings.TrimSpace(o.Endpoint) == "" {
		return fmt.Errorf("config: [offsite] endpoint is required with bucket %q (e.g. https://s3.eu-central-1.amazonaws.com)", o.Bucket)
	}
	if u, err := url.Parse(o.Endpoint); strings.Contains(o.Endpoint, "://") && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Trim(u.Path, "/") != "") {
		return fmt.Errorf("config: [offsite] endpoint %q must be http(s)://host[:port] with no path; the bucket goes in bucket", o.Endpoint)
	}
	if strings.ContainsAny(o.Bucket, "/ ") {
		return fmt.Errorf("config: [offsite] bucket %q must be a bare bucket name; put a key prefix in prefix", o.Bucket)
	}
	if strings.Contains(o.Prefix, "..") {
		return fmt.Errorf("config: [offsite] prefix %q must not contain \"..\"", o.Prefix)
	}
	if o.DBKeep < 1 {
		return fmt.Errorf("config: [offsite] db_keep %d must be at least 1", o.DBKeep)
	}
	return nil
}

var authSourcePrefixRe = regexp.MustCompile(`^[A-Za-z0-9]{1,4}$`)

// ValidateAuthSources shares identity and endpoint validation between panel and TOML settings.
func ValidateAuthSources(sources []AuthSourceConfig) error {
	return (&Config{AuthSources: sources}).validateAuthSources()
}

// Validate source namespaces, unique prefixes, and queryable hasJoined endpoints before accepting configuration.
func (c *Config) validateAuthSources() error {
	seenTags := make(map[string]struct{}, len(c.AuthSources))
	seenPrefixes := make(map[string]struct{}, len(c.AuthSources))
	for i, s := range c.AuthSources {
		if s.Tag == "" {
			return fmt.Errorf("config: [[auth_source]] #%d has an empty tag; each source's tag is its per-source UUID namespace", i+1)
		}
		// Colon-free permanent tags keep tag+":"+nativeID unambiguous without changing existing UUIDs.
		if strings.Contains(s.Tag, ":") {
			return fmt.Errorf("config: [[auth_source]] tag %q contains ':'; the tag and a player's native id are joined with ':' to derive their UUID, so a ':' in a tag would let another source mint this source's players", s.Tag)
		}
		// Invisible whitespace would change every player UUID in this source.
		if strings.TrimSpace(s.Tag) != s.Tag {
			return fmt.Errorf("config: [[auth_source]] tag %q has leading or trailing whitespace; the tag is hashed into every player UUID of the source, so an invisible edit to it would give all of them new ones", s.Tag)
		}
		if strings.EqualFold(s.Tag, "mojang") {
			return fmt.Errorf("config: [[auth_source]] tag %q is reserved: Mojang is built in as the first source and must not be listed", s.Tag)
		}
		if _, dup := seenTags[s.Tag]; dup {
			return fmt.Errorf("config: [[auth_source]] tag %q is used twice — tags are per-source UUID namespaces and must be unique", s.Tag)
		}
		seenTags[s.Tag] = struct{}{}
		if !authSourcePrefixRe.MatchString(s.Prefix) {
			return fmt.Errorf(`config: [[auth_source]] %q needs prefix = "XX" (1-4 letters or digits, e.g. "LS" for LittleSkin), got %q; a player of this source whose name belongs to a Mojang account is renamed XX_name so the two can be online at once`, s.Tag, s.Prefix)
		}
		// Prefixes must be unique after case folding, matching the proxy player registry.
		lower := strings.ToLower(s.Prefix)
		if _, dup := seenPrefixes[lower]; dup {
			return fmt.Errorf("config: [[auth_source]] prefix %q is used twice — two sources sharing a prefix rewrite their same-named players onto the same in-game name", s.Prefix)
		}
		seenPrefixes[lower] = struct{}{}
		if problem := hasJoinedURLProblem(s.URL); problem != "" {
			return fmt.Errorf("config: [[auth_source]] %q url %q %s", s.Tag, s.URL, problem)
		}
		if s.APIURL != "" {
			if problem := hasJoinedURLProblem(s.APIURL); problem != "" {
				return fmt.Errorf("config: [[auth_source]] %q api_url %q %s", s.Tag, s.APIURL, problem)
			}
		}
	}
	return nil
}

// Reject existing queries or fragments because the resolver appends hasJoined parameters directly.
func hasJoinedURLProblem(u string) string {
	if strings.TrimSpace(u) != u {
		return "has leading or trailing whitespace"
	}
	p, err := url.Parse(u)
	switch {
	case err != nil:
		return "does not parse: " + err.Error()
	case p.Scheme != "http" && p.Scheme != "https":
		return "must be a scheme-qualified http(s):// hasJoined endpoint"
	case p.Host == "":
		return "has no host"
	case strings.ContainsAny(u, "?#"):
		return "must not carry a query or fragment; the username and serverId parameters are appended to it"
	case p.Scheme == "http" && !plaintextHostOK(p.Hostname()):
		return "sends logins in plaintext to a public host, where anyone on the path can answer as any player of this source; use https://, or http:// only for localhost or a loopback or private IP address"
	}
	return ""
}

// Validate plaintext hosts literally; DNS is not resolved at configuration load time.
func plaintextHostOK(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
