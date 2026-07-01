package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// handleWake is the single lever (spec §9.1): it authorizes per autostartPolicy,
// applies the cooldown, and flips the CRD desiredState to Running. It does not
// transfer the player — the web flow shows status and a connect hint (spec §9.2).
func (a *API) handleWake(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		writeError(w, r, err)
		return
	}

	if err := a.authorizeWake(r.Context(), p, info, rec); err != nil {
		writeError(w, r, err)
		return
	}
	if !a.limiter().allowed(name, a.WakeCooldown) {
		writeError(w, r, newError(http.StatusTooManyRequests, "cooldown", "wake is cooling down, retry shortly"))
		return
	}
	// Global running-server cap (spec §9.1). Distinct from the per-server cooldown:
	// 503 at_capacity means the cluster is full, not that this server is throttled.
	ok, err := a.withinRunningCap(r.Context(), info)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, newError(http.StatusServiceUnavailable, "at_capacity",
			"the cluster is at its running-server cap (spec §9.1); retry once a server stops"))
		return
	}

	if err := a.Cluster.SetDesiredState(r.Context(), name, v1alpha1.DesiredRunning); err != nil {
		writeError(w, r, err)
		return
	}
	// The wake actually flipped, so consume the per-server cooldown only now: a 503
	// at_capacity or the SetDesiredState failure above must not burn it (a player
	// held at capacity should retry the instant a slot frees, not wait out a
	// cooldown their refused wake never earned).
	a.limiter().record(name)
	a.audit(r, p.Email, "wake", name)
	writeJSON(w, http.StatusAccepted, map[string]any{"name": name, "desiredState": "Running"})
}

// handleStop flips desiredState to Stopped. Only the owner or an admin may stop a
// server (spec §14: operating someone else's server is admin-tier).
func (a *API) handleStop(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return
	}

	if err := a.Cluster.SetDesiredState(r.Context(), name, v1alpha1.DesiredStopped); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, p.Email, "stop", name)
	writeJSON(w, http.StatusAccepted, map[string]any{"name": name, "desiredState": "Stopped"})
}

// handleClaim is the atomic claim transaction (spec §9.3): require a verified
// account link, enforce the quota gate, then UPDATE ... WHERE owner_id IS NULL.
// A lost race (0 rows) is 409.
func (a *API) handleClaim(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	// ① verified account link
	linked, err := a.Repo.IsLinked(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !linked {
		writeError(w, r, newError(http.StatusPreconditionFailed, "not_linked",
			"link your Minecraft account before claiming (see /api/v1/account/link/start)"))
		return
	}

	// ② quota gate, evaluated before the ownership write. This gate and ③ are two
	// separate statements, not one transaction — see the quota TOCTOU KNOWN-LIMITATION
	// on QuotaAvailable (audit #4, ENV-blocked: needs real Postgres to close/verify).
	ok, err := a.Repo.QuotaAvailable(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, newError(http.StatusForbidden, "quota_exceeded", "server quota exhausted"))
		return
	}

	// ③ atomic claim
	claimed, err := a.Repo.ClaimServer(r.Context(), name, p.UserID)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !claimed {
		writeError(w, r, newError(http.StatusConflict, "already_claimed", "server is already claimed"))
		return
	}

	// ④ audit. Allowlist population happens on first successful join (spec §9.4).
	a.audit(r, p.Email, "claim", name)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "claimed": true})
}

// handleStatus returns the CRD status view (spec §7 GET /servers/{name}/status).
func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleMe returns the calling principal's own identity (spec §14 tiering). The
// panel reads it once at boot to decide which navigation surfaces to render:
// the User-Side for everyone, the Admin/SysAdmin sides only when is_admin. This
// is UX truth, NOT a security control — every admin route is independently gated
// by adminOnly + Principal.IsAdmin() server-side, so hiding a nav item never
// widens access. is_admin is computed here as IsAdmin() (Role=="admin" AND the
// admin Access path), so the client never re-derives the graded-ZT rule. App-tier:
// a principal reads only its OWN identity — the response is sourced entirely from
// the verified token, no lookup escapes it.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":  p.UserID,
		"email":    p.Email,
		"role":     p.Role,
		"is_admin": p.IsAdmin(),
		// must_change_password is meaningful only on the local-password path; the JWT
		// path leaves it false. The panel uses it to route a freshly-provisioned staff
		// account straight to the change-password card before any other surface.
		"must_change_password": p.MustChangePassword,
	})
}

// handleMyServers lists what the caller owns or may claim (spec §7 GET /me/servers).
func (a *API) handleMyServers(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	servers, err := a.Repo.MyServers(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

// handleFleet is the SysAdmin cockpit's fleet-wide read: the lifecycle view of
// EVERY MinecraftServer, from CRD + status. It is the admin-tier counterpart of
// the app-tier handleMyServers — where /me/servers scopes to the caller, this
// returns the whole fleet, so it gates on the admin Zero-Trust path via adminOnly.
//
// This is a frontend-cockpit-driven extension (the SysAdmin FleetTable in
// panel/DESIGN-WEB-3SIDES.md), NOT a spec §7 route: §7 lists only the internal
// velocity pull (GET /servers, service-tier) and the app-tier GET /me/servers,
// neither of which is an external admin read. It reuses Cluster.ListServers (the
// same CRD-truth source as the velocity pull, §1) but is a DISTINCT handler so
// each route's provenance and tier stay honest, and so the two never share a
// {method, path} key — the OpenAPI parity test forbids one path carrying both the
// service and admin tiers across faces. Lifecycle is read from the CRD (§1); the
// one business field the cockpit needs — the owner — is joined READ-ONLY from
// Postgres at request time (§6 business authority) purely for display. This keeps
// §1 honest: owner is never written back to the CRD and the CRD is never treated
// as its source; the two stores keep their split, the read just renders both.
func (a *API) handleFleet(w http.ResponseWriter, r *http.Request) {
	servers, err := a.Cluster.ListServers(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Owner is presentational and best-effort. The cockpit exists for the lifecycle
	// view, so a Postgres hiccup must degrade to owner-less rows, never 500 the whole
	// fleet: a lookup error is swallowed and owners stays nil, leaving every row's
	// Owner "" (a nil map reads as zero values).
	owners, _ := a.Repo.ServerOwners(r.Context())
	views := make([]fleetServerView, len(servers))
	for i, s := range servers {
		views[i] = fleetServerView{ServerInfo: s, Owner: owners[s.Name]}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": views})
}

// fleetServerView is one row of the SysAdmin cockpit's fleet read: the CRD
// lifecycle view (ServerInfo, §1 authority) with the owner's display identity
// joined alongside. The embed keeps every lifecycle field flat in the JSON so the
// shape is a strict superset of ServerInfo; Owner is the only addition.
type fleetServerView struct {
	ServerInfo
	// Owner is the claiming user's display identity (email, or username when the
	// address is absent), or "" when the server is unclaimed or the best-effort
	// owner lookup failed — the cockpit renders "" as "unclaimed".
	Owner string `json:"owner,omitempty"`
}

// createServerRequest is the structured §15 create-server form. This is the
// ONLY way to create a server from the Web: every field is a typed, validated
// value and decodeJSON rejects unknown fields, so a caller can never smuggle
// free-form YAML or raw CRD fields through this endpoint.
type createServerRequest struct {
	Name            string           `json:"name"`
	Subdomain       string           `json:"subdomain"`
	DisplayName     string           `json:"displayName,omitempty"`
	Image           string           `json:"image"`
	Memory          string           `json:"memory"`
	Storage         string           `json:"storage"`
	AutostartPolicy string           `json:"autostartPolicy,omitempty"`
	Resources       *resourceRequest `json:"resources,omitempty"`
}

// resourceRequest is the optional override block. The required top-level memory
// already sets the pod memory limit+request (the §22 ceiling); these fields let
// an admin widen/narrow the cgroup envelope. Each is a Kubernetes quantity
// string ("500m", "2", "1Gi").
type resourceRequest struct {
	CPU           string `json:"cpu,omitempty"`
	CPURequest    string `json:"cpuRequest,omitempty"`
	Memory        string `json:"memory,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
}

// handleCreateServer (spec §15) is the admin-tier structured create flow:
// validate the form, admit the image against the whitelist, seed the business
// rows, then create the MinecraftServer CRD cold (DesiredState=Stopped) and
// unowned. There is no free-YAML path — the request is a typed form.
func (a *API) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	// The image whitelist lives in the build subsystem; with no Builder there is
	// no admission source, so create cannot run safely.
	if a.Builder == nil {
		writeError(w, r, errBuildUnavailable)
		return
	}
	p := principalFromContext(r.Context())

	var body createServerRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	// Server name and subdomain both obey the §22 portability rule and the
	// reservation list.
	if err := naming.ValidateServerName(body.Name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	if err := naming.ValidateServerName(body.Subdomain); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_subdomain", "invalid subdomain: %v", err))
		return
	}

	policy, err := parseAutostartPolicy(body.AutostartPolicy)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Memory is required: it is both the JVM heap hint and the default pod memory
	// limit+request. resolveResources fails closed if the §22 ceiling would be
	// zero, so a CRD is never written without a concrete memory limit.
	javaMemory, resources, err := resolveResources(body.Memory, body.Resources)
	if err != nil {
		writeError(w, r, err)
		return
	}

	storage, err := parseStorageSize(body.Storage)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Image admission is data-driven (the whitelist), never a free image string.
	if strings.TrimSpace(body.Image) == "" {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "image is required"))
		return
	}
	admitted, err := a.Builder.ImageAdmitted(r.Context(), body.Image)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !admitted {
		writeError(w, r, newError(http.StatusBadRequest, "image_not_whitelisted",
			"image %q is not on the whitelist", body.Image))
		return
	}

	// Quota is intentionally NOT enforced here. §15 creates an UNOWNED server
	// (owner_id NULL); the per-user quota is charged at claim time (spec §9.3 /
	// §22). The create path is quota-free by design, not by oversight.

	// Reject a duplicate subdomain before any write. The CRD list is the
	// lifecycle source; SeedServer re-checks the PG alias atomically below.
	switch _, err := a.Cluster.GetBySubdomain(r.Context(), body.Subdomain); {
	case err == nil:
		writeError(w, r, newError(http.StatusConflict, "subdomain_taken",
			"subdomain %q is already in use", body.Subdomain))
		return
	case !errors.Is(err, ErrNotFound):
		writeError(w, r, err)
		return
	}

	// Reject a duplicate server NAME before any write too. Without this, a
	// dup-name create (fresh subdomain) would reach SeedServer, which would bind
	// the new alias onto the PRE-EXISTING server and commit it — then CreateServer
	// fails 409 but the stray alias persists. Checking the CRD here keeps the
	// failed create side-effect-free. The narrow concurrent same-name race stays
	// inside the documented non-transactional tradeoff, backstopped by
	// CreateServer's AlreadyExists→409 below.
	switch _, err := a.Cluster.GetServer(r.Context(), body.Name); {
	case err == nil:
		writeError(w, r, newError(http.StatusConflict, "already_exists",
			"a server named %q already exists", body.Name))
		return
	case !errors.Is(err, ErrNotFound):
		writeError(w, r, err)
		return
	}

	// Seed the business rows FIRST (servers + alias). ClaimServer needs the row,
	// so a CRD-only server would be unclaimable. PG-first means a later CRD
	// failure leaves a claimable ghost row — acceptable, not transactional.
	if err := a.Repo.SeedServer(r.Context(), body.Name, body.Subdomain); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "subdomain_taken",
				"subdomain %q is already in use", body.Subdomain))
			return
		}
		writeError(w, r, err)
		return
	}

	in := CreateServerInput{
		Name:            body.Name,
		Subdomain:       body.Subdomain,
		DisplayName:     body.DisplayName,
		Image:           body.Image,
		JavaMemory:      javaMemory,
		StorageSize:     storage,
		AutostartPolicy: policy,
		Resources:       resources,
	}
	if err := a.Cluster.CreateServer(r.Context(), in); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_exists",
				"a server named %q already exists", body.Name))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, p.Email, "server.create", body.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":         body.Name,
		"subdomain":    body.Subdomain,
		"desiredState": string(v1alpha1.DesiredStopped),
	})
}

// parseAutostartPolicy maps the form value to a CRD policy. An empty value
// defaults to the safest policy (ownerOnly); any other unknown value is a 400.
func parseAutostartPolicy(s string) (v1alpha1.AutostartPolicy, error) {
	switch s {
	case "":
		return v1alpha1.AutostartOwnerOnly, nil
	case string(v1alpha1.AutostartOwnerOnly):
		return v1alpha1.AutostartOwnerOnly, nil
	case string(v1alpha1.AutostartPublic):
		return v1alpha1.AutostartPublic, nil
	case string(v1alpha1.AutostartAllowlist):
		return v1alpha1.AutostartAllowlist, nil
	default:
		return "", newError(http.StatusBadRequest, "bad_request",
			"invalid autostartPolicy %q (want ownerOnly, public, or allowlist)", s)
	}
}

// resolveResources turns the required memory string and the optional override
// block into the pod resource requirements. The top-level memory seeds both the
// memory limit and request; the override block may widen CPU and memory. It
// fails closed: the returned limit's memory is guaranteed non-zero so the §22
// ceiling is never absent from the CRD. The first return value is the derived
// JVM max-heap string (JavaMemory / -Xmx), computed from the FINAL memory limit
// — NOT the raw form value — so an overridden ceiling is honored and the heap
// stays below the cgroup limit (see deriveJavaHeap).
func resolveResources(memory string, rr *resourceRequest) (string, corev1.ResourceRequirements, error) {
	memQ, err := parsePositiveQuantity(memory, "memory")
	if err != nil {
		return "", corev1.ResourceRequirements{}, err
	}
	limits := corev1.ResourceList{corev1.ResourceMemory: memQ}
	requests := corev1.ResourceList{corev1.ResourceMemory: memQ}

	if rr != nil {
		if rr.Memory != "" {
			q, err := parsePositiveQuantity(rr.Memory, "resources.memory")
			if err != nil {
				return "", corev1.ResourceRequirements{}, err
			}
			limits[corev1.ResourceMemory] = q
		}
		if rr.MemoryRequest != "" {
			q, err := parsePositiveQuantity(rr.MemoryRequest, "resources.memoryRequest")
			if err != nil {
				return "", corev1.ResourceRequirements{}, err
			}
			requests[corev1.ResourceMemory] = q
		}
		if rr.CPU != "" {
			q, err := parsePositiveQuantity(rr.CPU, "resources.cpu")
			if err != nil {
				return "", corev1.ResourceRequirements{}, err
			}
			limits[corev1.ResourceCPU] = q
		}
		if rr.CPURequest != "" {
			q, err := parsePositiveQuantity(rr.CPURequest, "resources.cpuRequest")
			if err != nil {
				return "", corev1.ResourceRequirements{}, err
			}
			requests[corev1.ResourceCPU] = q
		}
	}

	// A request that exceeds its limit is rejected by Kubernetes; fail fast here
	// with a clear 400 instead of letting the CRD write bounce.
	if memReq, memLim := requests[corev1.ResourceMemory], limits[corev1.ResourceMemory]; memReq.Cmp(memLim) > 0 {
		return "", corev1.ResourceRequirements{}, newError(http.StatusBadRequest, "bad_request",
			"memory request %s exceeds limit %s", memReq.String(), memLim.String())
	}
	if cpuReq, hasReq := requests[corev1.ResourceCPU]; hasReq {
		if cpuLim, hasLim := limits[corev1.ResourceCPU]; hasLim && cpuReq.Cmp(cpuLim) > 0 {
			return "", corev1.ResourceRequirements{}, newError(http.StatusBadRequest, "bad_request",
				"cpu request %s exceeds limit %s", cpuReq.String(), cpuLim.String())
		}
	}

	// §22 fail-closed: never hand the operator a CRD without a concrete memory
	// ceiling. This cannot trigger given the positive memQ above, but the assert
	// guarantees the invariant survives future edits.
	memLim, ok := limits[corev1.ResourceMemory]
	if !ok || memLim.IsZero() {
		return "", corev1.ResourceRequirements{}, newError(http.StatusInternalServerError, "internal",
			"refusing to create a server without a memory ceiling (§22)")
	}

	return deriveJavaHeap(memLim), corev1.ResourceRequirements{Limits: limits, Requests: requests}, nil
}

// deriveJavaHeap converts the pod memory ceiling into a JVM max-heap string
// (JavaMemory → JAVA_MEMORY → -Xmx). Two reasons the raw K8s quantity cannot be
// forwarded as-is:
//
//   - Format: the JVM's -Xmx accepts k/m/g (1024-based) suffixes, NOT the
//     Kubernetes "Ki/Mi/Gi" forms. "-Xmx2Gi" fails to start the JVM, so we emit
//     a plain "<N>M" value, which both -Xmx and the container entrypoint accept.
//   - Headroom: metaspace, thread stacks, Netty direct buffers and GC structures
//     live OUTSIDE the heap. Setting -Xmx to the full cgroup limit guarantees an
//     eventual OOMKill, so we reserve off-heap room (the larger of 512Mi or 25%,
//     capped at half the limit) and size the heap to what remains.
//
// This is a sane default the operator/runtime may later refine; it is purely a
// derivation of the §22 ceiling and never exceeds it.
func deriveJavaHeap(limit resource.Quantity) string {
	const mib = int64(1024 * 1024)
	bytes := limit.Value()

	reserve := bytes / 4
	if floor := 512 * mib; reserve < floor {
		reserve = floor
	}
	if half := bytes / 2; reserve > half {
		reserve = half
	}

	heapMiB := (bytes - reserve) / mib
	if heapMiB < 1 {
		heapMiB = 1
	}
	return fmt.Sprintf("%dM", heapMiB)
}

// parseStorageSize validates the required storage size and returns the
// canonical quantity string for the PVC (spec §15).
func parseStorageSize(s string) (string, error) {
	q, err := parsePositiveQuantity(s, "storage")
	if err != nil {
		return "", err
	}
	return q.String(), nil
}

// parsePositiveQuantity parses a Kubernetes quantity string and rejects any
// non-positive value with a 400 naming the offending field.
func parsePositiveQuantity(s, field string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return resource.Quantity{}, newError(http.StatusBadRequest, "bad_request",
			"invalid %s quantity %q: %v", field, s, err)
	}
	if q.Sign() <= 0 {
		return resource.Quantity{}, newError(http.StatusBadRequest, "bad_request",
			"%s must be a positive quantity", field)
	}
	return q, nil
}

// patchServerRequest is the structured §7 PATCH /servers/{name} form. Like the
// §15 create form it is a CLOSED set of typed fields (decodeJSON rejects unknown
// fields), so an admin can never smuggle raw CRD/YAML knobs through a patch.
// Every field is a pointer: a nil pointer means "absent — leave unchanged",
// which a plain zero value could not distinguish from "set to empty". It mutates
// only CRD-authoritative spec fields (spec §22); it deliberately has no field for
// the dual-write routing identity (name is the immutable object key; subdomain
// would desync the Postgres alias) nor for the world PVC size (see below).
type patchServerRequest struct {
	DisplayName     *string          `json:"displayName,omitempty"`
	AutostartPolicy *string          `json:"autostartPolicy,omitempty"`
	Image           *string          `json:"image,omitempty"`
	Memory          *string          `json:"memory,omitempty"`
	Resources       *resourceRequest `json:"resources,omitempty"`
	// Storage is recognized only so the endpoint can reject it with a precise
	// reason rather than an opaque "unknown field": a StatefulSet's PVC capacity
	// is immutable except for storage-class-gated expansion, which this build does
	// not orchestrate. Accepting it would write a CRD change the operator cannot
	// honor, so it is refused (storage_immutable) instead of silently dropped.
	Storage *string `json:"storage,omitempty"`
}

// handlePatchServer (spec §7 PATCH /servers/{name}) is the admin-tier spec
// mutation: it validates the structured form, re-admits any new image against the
// whitelist, re-derives the §22 memory ceiling, and applies a merge patch to the
// MinecraftServer CRD. Only CRD-authoritative fields move; the business layer
// (Postgres) is untouched, so the two never desync (spec §22). The admin gate is
// the adminOnly wrapper in routing — every caller here is already an admin.
func (a *API) handlePatchServer(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	var body patchServerRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	// An empty patch is a client mistake, not a no-op success.
	if body.DisplayName == nil && body.AutostartPolicy == nil && body.Image == nil &&
		body.Memory == nil && body.Resources == nil && body.Storage == nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"patch must set at least one field"))
		return
	}
	if body.Storage != nil {
		writeError(w, r, newError(http.StatusBadRequest, "storage_immutable",
			"storage size cannot be changed through this endpoint (PVC capacity is immutable)"))
		return
	}

	// Build the resolved patch field-by-field, validating each present field with
	// the SAME helpers the create form uses. `changed` records what actually moves
	// so the response and audit name the real mutation.
	var patch ServerSpecPatch
	var changed []string

	if body.DisplayName != nil {
		patch.DisplayName = body.DisplayName
		changed = append(changed, "displayName")
	}

	if body.AutostartPolicy != nil {
		// Unlike create, an explicit empty policy is rejected rather than defaulted:
		// a patch states an intent, so "" is ambiguous, not "the safe default".
		if *body.AutostartPolicy == "" {
			writeError(w, r, newError(http.StatusBadRequest, "bad_request",
				"autostartPolicy cannot be empty"))
			return
		}
		policy, err := parseAutostartPolicy(*body.AutostartPolicy)
		if err != nil {
			writeError(w, r, err)
			return
		}
		patch.AutostartPolicy = &policy
		changed = append(changed, "autostartPolicy")
	}

	if body.Image != nil {
		// A new image must be re-admitted against the whitelist, exactly as create
		// does — admission is the only source of a legal image. With no Builder
		// there is no whitelist to check against, so the change cannot run safely.
		if a.Builder == nil {
			writeError(w, r, errBuildUnavailable)
			return
		}
		if strings.TrimSpace(*body.Image) == "" {
			writeError(w, r, newError(http.StatusBadRequest, "bad_request", "image cannot be empty"))
			return
		}
		admitted, err := a.Builder.ImageAdmitted(r.Context(), *body.Image)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if !admitted {
			writeError(w, r, newError(http.StatusBadRequest, "image_not_whitelisted",
				"image %q is not on the whitelist", *body.Image))
			return
		}
		patch.Image = body.Image
		changed = append(changed, "image")
	}

	// Memory and the resource overrides move together: resolveResources derives the
	// JVM heap and the §22 non-zero ceiling from the FINAL memory limit, and the
	// override block is meaningless without that base. A resources-only patch has no
	// base ceiling to widen (this endpoint does not read the current spec back), so
	// it is rejected rather than guessed.
	if body.Memory != nil {
		javaMemory, resources, err := resolveResources(*body.Memory, body.Resources)
		if err != nil {
			writeError(w, r, err)
			return
		}
		patch.JavaMemory = &javaMemory
		patch.Resources = &resources
		changed = append(changed, "memory")
		if body.Resources != nil {
			changed = append(changed, "resources")
		}
	} else if body.Resources != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"resources overrides require memory to be set in the same patch"))
		return
	}

	if err := a.Cluster.PatchServerSpec(r.Context(), name, patch); err != nil {
		a.writeLookupError(w, r, err)
		return
	}

	a.audit(r, p.Email, "server.patch", name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    name,
		"patched": changed,
	})
}

// ---- authorization helpers ----

// authorizeWake applies the autostartPolicy gate (spec §9.4). The owner and any
// admin may always wake; otherwise the policy decides. An empty/unknown policy
// fails safe (owner-only).
func (a *API) authorizeWake(ctx context.Context, p *Principal, info *ServerInfo, rec *ServerRecord) error {
	if p.IsAdmin() {
		return nil
	}
	if rec != nil && rec.OwnerID != "" && rec.OwnerID == p.UserID {
		return nil
	}
	switch info.AutostartPolicy {
	case string(v1alpha1.AutostartPublic):
		return nil
	case string(v1alpha1.AutostartAllowlist):
		ok, err := a.Repo.UserInAllowlist(ctx, info.Name, p.UserID)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		return errForbidden
	default: // ownerOnly or unset → only owner/admin, already handled above
		return errForbidden
	}
}

// isOwnerOrAdmin reports whether p owns rec or is an admin.
func (a *API) isOwnerOrAdmin(p *Principal, rec *ServerRecord) bool {
	if p.IsAdmin() {
		return true
	}
	return rec != nil && rec.OwnerID != "" && rec.OwnerID == p.UserID
}

// audit writes a best-effort audit row; a logging failure must not fail the
// underlying operation, which already succeeded.
func (a *API) audit(r *http.Request, actor, action, server string) {
	_ = a.Repo.Audit(r.Context(), AuditEntry{
		Actor:      actor,
		Source:     "external",
		Action:     action,
		ServerName: server,
		RequestID:  requestIDFromContext(r.Context()),
	})
}
