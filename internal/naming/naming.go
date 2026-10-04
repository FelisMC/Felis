// Package naming enforces the portability and admission rules (spec §2, §22):
// a server name matches ^[a-z0-9-]{3,32}$ and is non-reserved, and every
// hostname must be a single label under the configured root_domain. The root
// domain is never hardcoded — it is always supplied by config — so this package
// stays free of any deployment-specific domain.
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

// reserved subdomains/server names that users may not claim: proxy/lobby and
// the platform's own faces.
var reserved = map[string]struct{}{
	"login":    {},
	"lobby":    {},
	"admin":    {},
	"panel":    {},
	"console":  {}, // the player web console (console.<root>); op.console carries a dot and can never collide
	"api":      {},
	"felis":    {},
	"velocity": {},
	"registry": {},
	"internal": {},
	"www":      {},
}

// System server names Felis provisions itself. They deliberately live on the
// reserved list so no user can claim them, yet the platform must be able to
// create them — see ValidateSystemServerName.
const (
	// SystemLoginServer is the always-on limbo auth gate (LOOHP/Limbo). It is the
	// front door every fresh connection lands on and the ONLY safe fallback: a
	// stopped/starting backend routes here, never onward past authentication.
	SystemLoginServer = "login"
	// SystemLobbyServer is the post-auth /menu hub (Paper + felis-paper). It is
	// reachable only after the login gate passes a player through, so it must
	// never be used as a fallback target (that would bypass the gate).
	SystemLobbyServer = "lobby"
)

// IsSystemServer identifies platform services whose reserved names may be managed
// by staff, but never created or claimed through player-facing routes.
func IsSystemServer(name string) bool {
	return name == SystemLoginServer || name == SystemLobbyServer
}

// ServiceTokenSecretName / ServiceTokenSecretKey name the proxy's internal-API
// bearer credential Secret (spec §7); CallerTokens lists it with the tokens the
// other internal callers hold. The Secrets are provisioned out-of-band
// (deploy/bootstrap.sh) and replicated by `felis setup` into the namespace whose
// pods mount them; these constants only name them.
const (
	ServiceTokenSecretName = "felis-service-token"
	ServiceTokenSecretKey  = "token"
	// LimboTokenSecretName is the login gate's token, replicated into the
	// minecraft namespace; the operator injects it into the login pod only.
	LimboTokenSecretName = "felis-limbo-token"
	// BuildTokenSecretName is the build Job's token, replicated into the build
	// namespace for the context-fetch initContainer.
	BuildTokenSecretName = "felis-build-token"
	// OpsTokenSecretName is the on-node console's token (`felis backup-now`); it
	// stays in the control namespace.
	OpsTokenSecretName = "felis-ops-token"
	// EnvAPIBaseURL carries the internal-face base URL (platform.InternalAPIBaseURL)
	// into a pod: the login gate dials it, and the api reads it to derive the build
	// contexts' fetch URLs, so both sides name the same address for the same face.
	EnvAPIBaseURL = "FELIS_API_BASE_URL"
)

// Registry write credentials (internal/registrygate). The registry namespace holds
// RegistryAuthSecretName with one key per principal (platform, build, prune),
// mounted into the gate sidecar; felis-api reads the prune key into env. The build namespace holds RegistryPushSecretName with the build
// principal's username/password, read only by a build Job's push container. Both
// are provisioned out-of-band by deploy/bootstrap.sh.
const (
	RegistryAuthSecretName  = "felis-registry-auth"
	RegistryPushSecretName  = "felis-registry-push"
	RegistryPushUsernameKey = "username"
	RegistryPushPasswordKey = "password"
)

// ForwardingSecretName / ForwardingSecretKey name the Velocity modern player-info
// forwarding secret — the shared HMAC key the proxy signs each login handshake with
// and every backend verifies. It is what makes a backend's idea of "who is this
// player" trustworthy: with modern forwarding on, the UUID arrives inside the signed
// forwarding payload rather than being derived offline from the username, which is
// the whole basis of the Owner bind (the Owner IS a Minecraft account, claimed by
// joining the login gate). Legacy/BungeeCord forwarding carries no secret at all and
// fails OPEN — anyone who can reach a backend directly can assert any UUID — so Felis
// mandates modern (spec §20).
//
// Unlike the service token this is NOT login-only: Velocity's forwarding mode is a
// single proxy-wide setting, so once it is "modern" EVERY backend must speak it or it
// rejects the proxy's logins outright. The secret authenticates the PROXY to the
// backend; every backend verifies it before accepting the forwarded identity. The
// NetworkPolicy narrows game-port reachability to declared Velocity CIDRs for non-node
// traffic, but Kubernetes always permits traffic from a pod's resident node, so the
// policy is defense in depth and never replaces HMAC verification.
//
// Provisioned out-of-band (deploy/bootstrap.sh, the same run that writes Velocity's
// forwarding.secret) and replicated into the minecraft namespace by `felis setup`.
const (
	ForwardingSecretName = "felis-forwarding-secret"
	ForwardingSecretKey  = "secret"
)

// ValidateServerName checks the §22 name rule and reservation list.
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

// ValidateSystemServerName checks the §22 format rule (^[a-z0-9-]{3,32}$, no
// leading/trailing dash) but DELIBERATELY skips the reservation check. It is the
// admission path for platform-provisioned system services (login, lobby), which
// carry reserved names on purpose: users can never claim them via
// ValidateServerName, yet setup must still be able to create them. It is not a
// public claim path — only the setup/system-service provisioner calls it.
func ValidateSystemServerName(name string) error {
	if !serverNameRE.MatchString(name) {
		return fmt.Errorf("naming: invalid system server name %q: must match ^[a-z0-9-]{3,32}$", name)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("naming: system server name %q must not start or end with '-'", name)
	}
	return nil
}

// zeroWidthJoiner (U+200D) glues emoji such as the rainbow flag into one picture.
const zeroWidthJoiner = 0x200D

// MaxDisplayName is the most characters a server's display name may have. The
// panel shows it on one line in the fleet grid and the server header.
const MaxDisplayName = 64

// CleanDisplayName trims a server's display name and checks what is left: at most
// MaxDisplayName characters, every one a visible character or a space. Control
// characters, line breaks and the invisible format characters (bidi overrides,
// zero-width spaces) are refused, since they would break the line the panel puts
// the name on or make it read as something else; the zero-width joiner stays,
// because emoji such as the rainbow flag are built with it. An empty result is
// valid: the server then goes by its name.
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

// worldVolumeName mirrors operator.dataVolumeName: the per-server StatefulSet's
// volumeClaimTemplate is named "world", so a single-replica server's world PVC
// is "world-<name>-0". This is the one naming convention shared by the operator
// (which creates the PVC), the reaper (which deletes it), and restore (which
// mounts it), so it lives here rather than being duplicated per subsystem.
const worldVolumeName = "world"

// GameUID / GameGID are the identity every game server process runs as, and so the
// owner every file on a world volume must carry. The operator pins them in the
// server pod's securityContext (whatever USER the image declares), its prepare-data
// initContainer chowns a world that an older root-run release or a root restore
// left behind, and the file editor hands a file it creates to them. 1000 is the
// uid the eclipse-temurin (Ubuntu) images the Felis game images build on already
// reserve for their unprivileged user.
const (
	GameUID int64 = 1000
	GameGID int64 = 1000
)

// WorldPVCName returns the world PersistentVolumeClaim name for a server,
// matching the operator's StatefulSet volumeClaimTemplate naming
// ("world-<name>-0" for the sole replica).
func WorldPVCName(server string) string {
	return worldVolumeName + "-" + server + "-0"
}

// RconSecretKey is the key holding the password inside a server's RCON Secret.
const RconSecretKey = "password"

// RconSecretName returns the per-server RCON password Secret name. Like
// WorldPVCName this convention is shared rather than duplicated: felis-api writes
// it into spec.rcon.secretRef when creating a server, `felis setup` writes the
// same for the system lobby, and the operator both provisions the Secret under
// that name and reads it back for the readiness probe. One password per server,
// never a shared one — the console grants whoever holds it full command authority
// over that server, so a single cluster-wide value would make every owner an
// operator of everyone else's world.
func RconSecretName(server string) string {
	return server + "-rcon"
}

// Hostname composes subdomain.rootDomain after validating the subdomain.
func Hostname(subdomain, rootDomain string) (string, error) {
	if err := ValidateServerName(subdomain); err != nil {
		return "", err
	}
	if rootDomain == "" {
		return "", fmt.Errorf("naming: root domain is empty")
	}
	return subdomain + "." + rootDomain, nil
}

// ValidateHostname enforces the §2 invariant that host is a single label
// directly under rootDomain.
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

// CallerToken ties one internal-face caller (api.Caller) to the Secret holding
// its token, the env var felis-api reads that token from, and the namespace a
// replica of the Secret must reach for the caller's pods ("" when the caller
// runs outside the cluster or in the control namespace).
type CallerToken struct {
	Caller string
	Secret string
	APIEnv string
	// Replica names where the caller's pods run: "minecraft" or "build".
	Replica string
}

// CallerTokens is every internal caller, one token each. felis-api refuses to
// start when two share a value, so a token copied from one namespace opens only
// the routes that caller is listed on.
var CallerTokens = []CallerToken{
	{Caller: "velocity", Secret: ServiceTokenSecretName, APIEnv: "FELIS_SERVICE_TOKEN"},
	{Caller: "limbo", Secret: LimboTokenSecretName, APIEnv: "FELIS_LIMBO_TOKEN", Replica: "minecraft"},
	{Caller: "build", Secret: BuildTokenSecretName, APIEnv: "FELIS_BUILD_TOKEN", Replica: "build"},
	{Caller: "ops", Secret: OpsTokenSecretName, APIEnv: "FELIS_OPS_TOKEN"},
}
