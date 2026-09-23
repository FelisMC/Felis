// Package config loads and validates felis.toml (spec §24). root_domain lives
// here and nowhere else in code: every FQDN is composed at runtime as
// subdomain + "." + root_domain, so changing the deployment domain is a
// one-line config edit and the source tree stays domain-agnostic.
package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
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
	SMTP     SMTPConfig     `toml:"smtp"`
	// AuthSources is the [[auth_source]] array-of-tables: the third-party Yggdrasil
	// roots the Felis-nano hasJoined multiplexer federates over, in priority order
	// (config order = priority, so array-of-tables not a map — a map would lose order
	// and silently break Mojang-first). Empty = Mojang is the only source. There is
	// deliberately NO identity/trusted field here: Mojang is the single code-owned
	// identity anchor (cmd/felis prepends it) and every configured source is
	// namespace-rewritten, so no config can mint a source whose self-asserted UUIDs are
	// trusted verbatim — the impersonation hole that rewrite closes cannot be reopened by
	// misconfiguration. (An `identity =` key here is an unknown key → Load rejects it.)
	AuthSources []AuthSourceConfig `toml:"auth_source"`
}

// AuthSourceConfig is one [[auth_source]] entry: a third-party Yggdrasil root the
// Felis-nano multiplexer federates over. Tag names the source's per-source UUID
// namespace (must be unique — two sources sharing a tag would collide onto one identity).
// It is permanent: every player UUID of the source is hashed from it byte for byte, so
// changing it, even its case, gives all of them new UUIDs and orphans their playerdata,
// account links and bans. URL is the full hasJoined endpoint (scheme-qualified) the query
// string is appended to.
// Prefix is what a player from this source is renamed with when their name belongs to a
// Mojang player (LS_steve) — player-visible, so it is written out rather than derived from
// the tag, which cannot know that "littleskin" is meant to read LS.
// No trusted/identity field, by design — see Config.AuthSources.
type AuthSourceConfig struct {
	Tag    string `toml:"tag"`
	Prefix string `toml:"prefix"`
	URL    string `toml:"url"`
}

// SMTPConfig is the [smtp] table: the outbound mail relay felis-api delivers
// email one-time codes through (onboarding, email login, op-login). It is
// OPTIONAL — an empty host means "no mailer", and felis-api falls back to
// logging each code server-side (the pre-SMTP bootstrap posture). Only the
// coordinates live here; the password follows the tree's credential rule
// (ArchiveS3Config, RegistryS3Config): PasswordRef NAMES the environment
// variable felis-api reads it from — the secret itself is never written into
// felis.toml. The setup wizard's "configure email" step creates the felis-smtp
// Secret the deployment injects that variable from.
type SMTPConfig struct {
	Host string `toml:"host"`
	// Port defaults to 587 (STARTTLS submission). 465 selects implicit TLS.
	Port int `toml:"port"`
	// From is the envelope/header sender address the codes are mailed as.
	From string `toml:"from"`
	// Username is the AUTH identity; empty means the relay needs no AUTH.
	Username    string `toml:"username"`
	PasswordRef string `toml:"password_ref"`
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
	// KanikoImage / TrivyImage / BuildCPULimit / BuildMemLimit override the
	// build subsystem's compiled-in defaults (gcr.io/kaniko-project/executor and
	// aquasec/trivy, 2 CPU / 4Gi per build container). The defaults assume the
	// build namespace can reach those registries; on an air-gapped or mirrored
	// install there IS no such reach (the build egress policy allows only DNS,
	// the internal registry and explicit package mirrors), so the operator must
	// point these at whatever their box can actually pull — typically images
	// mirrored into the in-cluster registry (docs/troubleshooting.md §8e); a
	// bare node-containerd import does not survive an image GC, there is no pull
	// source for it. Empty keeps the default.
	KanikoImage   string `toml:"kaniko_image"`
	TrivyImage    string `toml:"trivy_image"`
	BuildCPULimit string `toml:"build_cpu_limit"`
	BuildMemLimit string `toml:"build_mem_limit"`
	// TrivyDBRepository points Trivy at an OCI repository holding the
	// vulnerability DB (--db-repository). Trivy's default fetches from
	// mirror.gcr.io/ghcr.io, which the build egress lock denies — so on a
	// default install the scan step fails closed and no build ever completes.
	// The supported shape is an internal mirror: copy
	// mirror.gcr.io/aquasec/trivy-db:2 into this cluster's registry (recipe in
	// docs/troubleshooting.md §8) and set this to
	// registry.<ns>.svc:5000/mirror/trivy-db:2. The scan runs with --insecure,
	// so the plain-HTTP internal registry works. Empty keeps Trivy's own
	// default (only usable on an install that deliberately opens internet
	// egress to the DB hosts).
	TrivyDBRepository string `toml:"trivy_db_repository"`
	// UserUploadsContext is the object-store base under which a user-submitted
	// modpack's Kaniko build context is pinned. It belongs to the §16 build
	// subsystem's input domain (the build-context store), introduced by the
	// user-directed modpack approval lane (see internal/submit package doc). The
	// lane derives {UserUploadsContext}/{submissionID}/context.tar.gz; both transports
	// that place the blob there now ship (submit.LocalContextStore for a local path,
	// submit.S3ContextStore for an s3:// base, selected in cmd/felis by the shape of
	// this value), and so does the read end: the build Pod's fetch initContainer
	// streams the blob back over the API's internal face, so this value just names
	// where the API stores it, not where Kaniko must reach. It is
	// kept distinct from [archive] on purpose — a world
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
	// deployment points it at a real object store. It is only a parseable prefix —
	// an s3:// base with no credentials leaves the upload transport unwired, and
	// the endpoint answers an honest 503 (see the §16 build subsystem and the
	// internal/submit package doc for the lane's provenance).
	defaultUserUploadsContext = "s3://felis-user-uploads"
	// defaultSMTPPort is the STARTTLS submission port; applied only when [smtp]
	// host is set (a portless [smtp] block with no host stays fully zero).
	defaultSMTPPort = 587
)

// decodeConfig reads a felis.toml and rejects unknown keys (typos surface as errors
// rather than silently ignored config). Both the full Load and the nano-only LoadNano
// share it, so the unknown-key contract is owned in one place.
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

// Load reads and validates a full felis.toml (the control-plane binaries: api, migrate,
// reaper).
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

// LoadNano reads a felis.toml for a Felis-nano host — the hasJoined multiplexer only, no
// control plane. It validates just the [[auth_source]] block and deliberately skips the
// control-plane requirements (database.url, root_domain, archive store) that a nano host has
// no Postgres or FQDN for: forcing a fake database.url onto a pure hasJoined federator would
// be a lie that breaks the moment anything touches it. The auth-source rules (unique tags,
// scheme-qualified URLs) are the SAME code path Load enforces, so nano cannot reopen the
// cross-source impersonation hole a full deployment is protected from.
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
		return fmt.Errorf("config: [archive] store %q is not implemented in this build — only tarLocal is supported; set store = \"tarLocal\"", c.Archive.Store)
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
	// [smtp] is optional as a whole, but once a host is named the block must be
	// deliverable: a From address (relays reject MAIL FROM:<>) and a sane port.
	// Fail at load, not at the first OTP a player is waiting on.
	if c.SMTP.Host != "" {
		if !strings.Contains(c.SMTP.From, "@") {
			return fmt.Errorf("config: [smtp] from %q must be the sender email address codes are mailed as", c.SMTP.From)
		}
		if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
			return fmt.Errorf("config: [smtp] port %d must be 1-65535 (587 STARTTLS, 465 implicit TLS)", c.SMTP.Port)
		}
	}
	return c.validateAuthSources()
}

// authSourcePrefixRe is the shape of a prefix. It is prepended to a real Minecraft
// username (LS_steve), so it is confined to the username charset and kept short enough to
// leave a legible name behind after truncation.
var authSourcePrefixRe = regexp.MustCompile(`^[A-Za-z0-9]{1,4}$`)

// validateAuthSources checks the [[auth_source]] block: each needs a namespace tag, a rename
// prefix, and a scheme-qualified hasJoined URL, and both tag and prefix must be unique. A
// blank, duplicate or colon-bearing tag collapses two sources into one UUID namespace
// (cross-source impersonation — the exact invariant the per-source rewrite exists to
// hold); a duplicate prefix collapses two same-named players from different sources onto
// one in-game name
// (they stay distinct identities, but neither can be online while the other is); a URL
// the resolver cannot query leaves the source silently dead (never validates any login).
// All fail fast at load, not per-login. Split out from Validate so the nano-only
// LoadNano (no control-plane fields) enforces the identical rules — the impersonation guard
// has one owner, shared by full-api and nano.
func (c *Config) validateAuthSources() error {
	seenTags := make(map[string]struct{}, len(c.AuthSources))
	seenPrefixes := make(map[string]struct{}, len(c.AuthSources))
	for i, s := range c.AuthSources {
		if s.Tag == "" {
			return fmt.Errorf("config: [[auth_source]] #%d has an empty tag; each source's tag is its per-source UUID namespace", i+1)
		}
		// A player's UUID is derived from tag+":"+nativeID, and the native id is whatever the
		// source says it is. With a ':' allowed in tags, "guild" answering id "eu:X" hashes
		// exactly like "guild:eu" answering "X", so one source could mint another's players.
		// Colon-free tags make the join unambiguous. The charset is otherwise left open, because
		// renaming an existing tag would move every one of its players to a new UUID.
		if strings.Contains(s.Tag, ":") {
			return fmt.Errorf("config: [[auth_source]] tag %q contains ':'; the tag and a player's native id are joined with ':' to derive their UUID, so a ':' in a tag would let another source mint this source's players", s.Tag)
		}
		// Refused for the same permanence: a stray space is invisible in the file yet is a
		// different namespace, and so a different UUID for every player of the source.
		if strings.TrimSpace(s.Tag) != s.Tag {
			return fmt.Errorf("config: [[auth_source]] tag %q has leading or trailing whitespace; the tag is hashed into every player UUID of the source, so an invisible edit to it would give all of them new ones", s.Tag)
		}
		// Mojang is the built-in first source. A listed "mojang" is never it: it is asked again,
		// after Mojang, on every login that reaches it, and nano's startup list then reads as if
		// Mojang had been pointed at that url.
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
		// Case-insensitively — the proxy's player registry folds case, so LS and ls would
		// collide there even though they read as two different prefixes here.
		lower := strings.ToLower(s.Prefix)
		if _, dup := seenPrefixes[lower]; dup {
			return fmt.Errorf("config: [[auth_source]] prefix %q is used twice — two sources sharing a prefix rewrite their same-named players onto the same in-game name", s.Prefix)
		}
		seenPrefixes[lower] = struct{}{}
		if problem := hasJoinedURLProblem(s.URL); problem != "" {
			return fmt.Errorf("config: [[auth_source]] %q url %q %s", s.Tag, s.URL, problem)
		}
	}
	return nil
}

// hasJoinedURLProblem says why u cannot be queried as a hasJoined endpoint, or "" if it
// can. The resolver appends "?username=…&serverId=…" to it as a string, so a query or
// fragment already in it swallows those parameters, and a URL the client cannot send only
// fails one login at a time, with the source looking like it knows nobody.
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

// plaintextHostOK is decided on the literal host because nothing is resolved at load time,
// so a LAN root named by hostname needs its IP address or https.
func plaintextHostOK(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
