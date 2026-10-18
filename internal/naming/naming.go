// Package naming enforces server name and hostname rules; the root domain always comes from config.
package naming

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	serverNameRE = regexp.MustCompile(`^[a-z0-9-]{3,32}$`)
	dnsLabelRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

var reserved = map[string]struct{}{
	"login":    {},
	"lobby":    {},
	"admin":    {},
	"panel":    {},
	"console":  {},
	"api":      {},
	"felis":    {},
	"velocity": {},
	"registry": {},
	"internal": {},
	"www":      {},
}

// System server names are reserved from users but still creatable by the platform.
const (
	// SystemLoginServer is the always-on login gate: the only fallback for a stopped backend, never a route past authentication.
	SystemLoginServer = "login"
	// SystemLobbyServer is the post-login hub; never use it as a fallback, which would bypass the login gate.
	SystemLobbyServer    = "lobby"
	ExperienceConfigFile = "felis-experience.json"
)

// IsSystemServer identifies platform services whose reserved names may be managed
// by staff, but never created or claimed through player-facing routes.
func IsSystemServer(name string) bool {
	return name == SystemLoginServer || name == SystemLobbyServer
}

// Names the internal API token Secret; bootstrap.sh provisions it and felis setup replicates it.
const (
	ServiceTokenSecretName = "felis-service-token"
	ServiceTokenSecretKey  = "token"
	// Only the login Pod receives LimboTokenSecretName.
	LimboTokenSecretName = "felis-limbo-token"
	BuildTokenSecretName = "felis-build-token"
	OpsTokenSecretName   = "felis-ops-token"
	// Env var carrying the internal API base URL into pods; both sides must name the same address.
	EnvAPIBaseURL = "FELIS_API_BASE_URL"
)

// Registry write Secrets, provisioned out of band by deploy/bootstrap.sh.
const (
	RegistryAuthSecretName  = "felis-registry-auth"
	RegistryPushSecretName  = "felis-registry-push"
	RegistryPushUsernameKey = "username"
	RegistryPushPasswordKey = "password"
)

// Velocity modern-forwarding HMAC key that backends use to trust the player UUID; legacy forwarding has no key and fails open.
const (
	ForwardingSecretName = "felis-forwarding-secret"
	ForwardingSecretKey  = "secret"
)

// ValidateServerName applies the name format rule and the reserved-name list.
func ValidateServerName(name string) error {
	if !serverNameRE.MatchString(name) {
		return fmt.Errorf("naming: invalid server name %q: must match ^[a-z0-9-]{3,32}$", name)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("naming: server name %q must not start or end with '-'", name)
	}
	if _, ok := reserved[name]; ok {
		return fmt.Errorf("naming: server name %q is reserved", name)
	}
	return nil
}

// ValidateSystemServerName checks the format but skips the reserved-name check; only the setup provisioner calls it.
func ValidateSystemServerName(name string) error {
	if !serverNameRE.MatchString(name) {
		return fmt.Errorf("naming: invalid system server name %q: must match ^[a-z0-9-]{3,32}$", name)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("naming: system server name %q must not start or end with '-'", name)
	}
	return nil
}

const zeroWidthJoiner = 0x200D

const MaxDisplayName = 64

// CleanDisplayName trims a display name and rejects control, line-break and invisible format characters; zero-width joiners stay for emoji.
func CleanDisplayName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n := utf8.RuneCountInString(s); n > MaxDisplayName {
		return "", fmt.Errorf("naming: display name has %d characters, at most %d are allowed", n, MaxDisplayName)
	}
	for _, r := range s {
		if r == utf8.RuneError || !unicode.IsGraphic(r) && r != zeroWidthJoiner {
			return "", fmt.Errorf("naming: display name contains %U, which is not a visible character", r)
		}
	}
	return s, nil
}

// worldVolumeName must match operator.dataVolumeName: the world PVC is "world-<name>-0"; keep the two in sync.
const worldVolumeName = "world"

// GameUID and GameGID own every world file; 1000 is the uid the eclipse-temurin images reserve for their unprivileged user.
const (
	GameUID int64 = 1000
	GameGID int64 = 1000
)

// WorldPVCName returns the world PVC name for a server: "world-<name>-0" for the sole replica.
func WorldPVCName(server string) string {
	return worldVolumeName + "-" + server + "-0"
}

const RconSecretKey = "password"

// RconSecretName returns the per-server RCON Secret name; each server has its own password because RCON grants full command authority.
func RconSecretName(server string) string {
	return server + "-rcon"
}

func Hostname(subdomain, rootDomain string) (string, error) {
	if err := ValidateServerName(subdomain); err != nil {
		return "", err
	}
	if rootDomain == "" {
		return "", fmt.Errorf("naming: root domain is empty")
	}
	return subdomain + "." + rootDomain, nil
}

// ValidateHostname requires the host to be a single label directly under rootDomain.
func ValidateHostname(host, rootDomain string) error {
	if rootDomain == "" {
		return fmt.Errorf("naming: root domain is empty")
	}
	suffix := "." + rootDomain
	if !strings.HasSuffix(host, suffix) {
		return fmt.Errorf("naming: hostname %q must be under %q", host, rootDomain)
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return fmt.Errorf("naming: hostname %q must be a single label under %q", host, rootDomain)
	}
	if !dnsLabelRE.MatchString(label) {
		return fmt.Errorf("naming: invalid hostname label %q", label)
	}
	return nil
}

// CallerToken ties an internal caller to its token Secret, the env var that holds the token, and the namespace the Secret must be replicated into.
type CallerToken struct {
	Caller string
	Secret string
	APIEnv string
	// Replica names where the caller's pods run: "minecraft" or "build".
	Replica string
}

// CallerTokens lists every internal caller with its own token; felis-api refuses to start when two share a value.
var CallerTokens = []CallerToken{
	{Caller: "velocity", Secret: ServiceTokenSecretName, APIEnv: "FELIS_SERVICE_TOKEN"},
	{Caller: "limbo", Secret: LimboTokenSecretName, APIEnv: "FELIS_LIMBO_TOKEN", Replica: "minecraft"},
	{Caller: "build", Secret: BuildTokenSecretName, APIEnv: "FELIS_BUILD_TOKEN", Replica: "build"},
	{Caller: "ops", Secret: OpsTokenSecretName, APIEnv: "FELIS_OPS_TOKEN"},
}
