package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"strings"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var errEULARequired = newError(http.StatusBadRequest, "eula_required",
	"a server built from server software runs under the Minecraft EULA; accept it to continue (https://aka.ms/MinecraftEULA)")

// admitImage admits a whitelisted server image and pins the build its tag names
// now: the world is created on that build and stays on it until an admin
// changes the image. Admission is data-driven (the whitelist), never a free
// image string.
func (a *API) admitImage(ctx context.Context, ref string) (string, error) {
	// The whitelist lives in the build subsystem; with no Builder there is no
	// admission source.
	if a.Builder == nil {
		return "", errBuildUnavailable
	}
	if strings.TrimSpace(ref) == "" {
		return "", newError(http.StatusBadRequest, "bad_request", "image is required")
	}
	admitted, err := a.Builder.ImageAdmitted(ctx, ref)
	if err != nil {
		return "", err
	}
	if !admitted {
		return "", newError(http.StatusBadRequest, "image_not_whitelisted", "image %q is not on the whitelist", ref)
	}
	return a.pinImage(ctx, ref)
}

// checkNewServerName refuses a name or subdomain already taken, before any
// write. The CRD is the lifecycle source; SeedServer re-checks the alias
// atomically. A duplicate name with a fresh subdomain would otherwise reach the
// seed and bind the new alias onto the existing server before the CRD create
// failed. A server deleted outside the reaper (kubectl delete) leaves its world
// volume behind, and a new server of the same name would mount it and hand the
// old world to its new owner, so the name stays taken until an operator removes
// the volume.
func (a *API) checkNewServerName(ctx context.Context, name, subdomain string) error {
	switch _, err := a.Cluster.GetBySubdomain(ctx, subdomain); {
	case err == nil:
		return newError(http.StatusConflict, "subdomain_taken", "subdomain %q is already in use", subdomain)
	case !errors.Is(err, ErrNotFound):
		return err
	}
	switch _, err := a.Cluster.GetServer(ctx, name); {
	case err == nil:
		return newError(http.StatusConflict, "already_exists", "a server named %q already exists", name)
	case !errors.Is(err, ErrNotFound):
		return err
	}
	switch exists, err := a.Cluster.WorldVolumeExists(ctx, name); {
	case err != nil:
		return err
	case exists:
		return newError(http.StatusConflict, "world_volume_exists",
			"the world volume of an earlier server named %q still exists; delete it or choose another name", name)
	}
	return nil
}

// ---- the Owner's default quota ----

const selfServiceQuotaKey = "self_service_quota"

// selfServiceQuota is the default quota: the caps of every user without a quota
// of their own, once Enabled. A nil cap is unlimited, 0 grants none. Off (the
// default), a user without their own quota cannot create servers and claims stay
// unlimited for them, as before the setting existed.
type selfServiceQuota struct {
	Enabled      bool `json:"enabled"`
	MaxServers   *int `json:"max_servers"`
	MaxCPUMilli  *int `json:"max_cpu_milli"`
	MaxMemoryMB  *int `json:"max_memory_mb"`
	MaxStorageGB *int `json:"max_storage_gb"`
}

type selfServiceQuotaView struct {
	selfServiceQuota
	Revision string `json:"revision"`
}

func (q selfServiceQuota) valid() bool {
	for _, v := range []*int{q.MaxServers, q.MaxCPUMilli, q.MaxMemoryMB, q.MaxStorageGB} {
		if v != nil && (*v < 0 || *v > math.MaxInt32) {
			return false
		}
	}
	return true
}

func (a *API) readSelfServiceQuota(ctx context.Context) (selfServiceQuotaView, []byte, error) {
	var view selfServiceQuotaView
	raw, err := a.Repo.GetSetting(ctx, selfServiceQuotaKey)
	switch {
	case errors.Is(err, ErrNotFound):
		raw = nil
	case err != nil:
		return view, nil, err
	default:
		if err := json.Unmarshal(raw, &view.selfServiceQuota); err != nil {
			return view, nil, err
		}
	}
	canonical, _ := json.Marshal(view.selfServiceQuota)
	sum := sha256.Sum256(canonical)
	view.Revision = hex.EncodeToString(sum[:])
	return view, raw, nil
}

func (a *API) handleGetSelfServiceQuota(w http.ResponseWriter, r *http.Request) {
	view, _, err := a.readSelfServiceQuota(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleSetSelfServiceQuota(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		selfServiceQuota
		Revision string `json:"revision"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !body.selfServiceQuota.valid() {
		writeError(w, r, newError(http.StatusBadRequest, "invalid_quota",
			"each cap must be a whole number from 0 to 2147483647, or null for unlimited"))
		return
	}
	current, expected, err := a.readSelfServiceQuota(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if body.Revision != current.Revision {
		writeError(w, r, newError(http.StatusConflict, "conflict", "the default quota changed; reload before saving"))
		return
	}
	raw, _ := json.Marshal(body.selfServiceQuota)
	if err := a.Repo.CompareAndSetSetting(r.Context(), selfServiceQuotaKey, expected, raw); err != nil {
		writeError(w, r, err)
		return
	}
	a.audit(r, "platform.self_service_quota", "platform")
	view, _, err := a.readSelfServiceQuota(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// ---- the caller's own quota ----

// serverQuotaView answers GET /me/server-quota: whether the caller can create a
// server, under which quota, and what their servers use of it.
type serverQuotaView struct {
	QuotaState
	// CanCreate is true when the caller may create a server now. Reason names
	// what stops them otherwise: not_linked, self_service_disabled,
	// software_unavailable or quota_full.
	CanCreate bool   `json:"canCreate"`
	Reason    string `json:"reason,omitempty"`
}

func (a *API) handleMyServerQuota(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	st, err := a.Repo.SelfServiceState(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	view := serverQuotaView{QuotaState: st}
	linked, err := a.Repo.IsLinked(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	switch {
	case !linked:
		view.Reason = "not_linked"
	case a.Software == nil:
		view.Reason = "software_unavailable"
	case st.Source == "":
		view.Reason = "self_service_disabled"
	case st.Limits.MaxServers != nil && st.Usage.Servers >= *st.Limits.MaxServers:
		view.Reason = "quota_full"
	default:
		view.CanCreate = true
	}
	writeJSON(w, http.StatusOK, view)
}

// ---- creating a server for yourself ----

// selfCreateRequest is the form a user creates their own server with. It has
// no image and no resource overrides: a user's server always runs server
// software on the runtime image, sized by memory (and optionally CPU) within
// their quota.
type selfCreateRequest struct {
	Name            string          `json:"name"`
	Subdomain       string          `json:"subdomain"`
	DisplayName     string          `json:"displayName,omitempty"`
	Software        softwareRequest `json:"software"`
	Memory          string          `json:"memory"`
	CPU             string          `json:"cpu,omitempty"`
	Storage         string          `json:"storage"`
	AutostartPolicy string          `json:"autostartPolicy,omitempty"`
	AcceptEULA      bool            `json:"acceptEula"`
}

// handleSelfCreateServer (POST /me/servers) creates a server owned by the
// caller, within the quota in force for them: their own, else the Owner's
// default quota. The quota check and the owned row are one transaction in the
// caller's claim lane (SeedOwnedServer), so concurrent creates cannot both pass
// a cap.
func (a *API) handleSelfCreateServer(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	ctx := r.Context()
	var body selfCreateRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	linked, err := a.Repo.IsLinked(ctx, p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !linked {
		writeError(w, r, newError(http.StatusPreconditionFailed, "not_linked",
			"link your Minecraft account before creating a server (see /api/v1/account/link/start)"))
		return
	}
	if err := naming.ValidateServerName(body.Name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	if err := naming.ValidateServerName(body.Subdomain); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_subdomain", "invalid subdomain: %v", err))
		return
	}
	displayName, err := naming.CleanDisplayName(body.DisplayName)
	if err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_display_name", "invalid display name: %v", err))
		return
	}
	policy, err := parseAutostartPolicy(body.AutostartPolicy)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var rr *resourceRequest
	if body.CPU != "" {
		rr = &resourceRequest{CPU: body.CPU}
	}
	javaMemory, resources, err := resolveResources(body.Memory, rr)
	if err != nil {
		writeError(w, r, err)
		return
	}
	storage, err := parseStorageSize(body.Storage)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !body.AcceptEULA {
		writeError(w, r, errEULARequired)
		return
	}
	sw, image, err := a.resolveSoftware(ctx, body.Software)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := a.checkNewServerName(ctx, body.Name, body.Subdomain); err != nil {
		writeError(w, r, err)
		return
	}
	// In distributed mode the server goes to the admitted node carrying the
	// fewest user servers, the same choice a failover makes.
	var node string
	if a.Distribution != nil {
		if node, err = a.Distribution.FailoverTarget(ctx); err != nil {
			writeError(w, r, newError(http.StatusServiceUnavailable, "no_node", "no node can take a new server right now: %v", err))
			return
		}
	}

	storQ, _ := resource.ParseQuantity(storage)
	res := ResourceSpec{
		CPUMilli:  quantityToMilli(resources.Limits[corev1.ResourceCPU]),
		MemoryMB:  quantityToMB(resources.Limits[corev1.ResourceMemory]),
		StorageMB: quantityToMB(storQ),
	}
	switch err := a.Repo.SeedOwnedServer(ctx, body.Name, body.Subdomain, p.UserID, res); {
	case errors.Is(err, ErrSelfServiceDisabled):
		writeError(w, r, newError(http.StatusForbidden, "self_service_disabled",
			"creating servers is not enabled for your account; ask the platform owner for a quota"))
		return
	case errors.Is(err, ErrQuotaExceeded):
		writeError(w, r, newError(http.StatusForbidden, "quota_exceeded", "this server does not fit in your quota"))
		return
	case errors.Is(err, ErrConflict):
		writeError(w, r, newError(http.StatusConflict, "subdomain_taken", "subdomain %q is already in use", body.Subdomain))
		return
	case err != nil:
		writeError(w, r, err)
		return
	}
	in := CreateServerInput{
		NodeName:        node,
		Name:            body.Name,
		Subdomain:       body.Subdomain,
		DisplayName:     displayName,
		Image:           image,
		JavaMemory:      javaMemory,
		StorageSize:     storage,
		AutostartPolicy: policy,
		Resources:       resources,
		Software:        sw,
	}
	if err := a.Cluster.CreateServer(ctx, in); err != nil {
		if derr := a.Repo.DiscardSeededServer(context.WithoutCancel(ctx), body.Name); derr != nil {
			log.Printf("api: self-created server %s refused by the cluster and its row could not be discarded (request_id=%s): %v",
				body.Name, requestIDFromContext(ctx), derr)
		}
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_exists", "a server named %q already exists", body.Name))
			return
		}
		writeError(w, r, err)
		return
	}
	a.audit(r, "server.self_create", body.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":         body.Name,
		"subdomain":    body.Subdomain,
		"desiredState": string(v1alpha1.DesiredStopped),
	})
}

// ---- an owner changing their own server ----

// ownerPatchRequest is what a server's owner may change: its name in the
// panel, who may wake it, idle stop, its memory and CPU within their quota, and
// its server software. The image, the finer resource fields and storage stay
// with admins.
type ownerPatchRequest struct {
	DisplayName     *string          `json:"displayName,omitempty"`
	AutostartPolicy *string          `json:"autostartPolicy,omitempty"`
	IdleStopSeconds *int32           `json:"idleStopSeconds,omitempty"`
	Memory          *string          `json:"memory,omitempty"`
	CPU             *string          `json:"cpu,omitempty"`
	Software        *softwareRequest `json:"software,omitempty"`
	// ConfirmSoftwareChange acknowledges that moving to another type or
	// Minecraft version opens the world with it (see patchServerRequest).
	ConfirmSoftwareChange bool `json:"confirmSoftwareChange,omitempty"`
	AcceptEULA            bool `json:"acceptEula,omitempty"`
}

// handleOwnerPatchServer (PATCH /me/servers/{name}) is the owner's spec change.
// It runs the admin patch's validation and quota gate on the fields an owner may
// set.
func (a *API) handleOwnerPatchServer(w http.ResponseWriter, r *http.Request) {
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
	if rec.OwnerID == "" || rec.OwnerID != p.UserID {
		writeError(w, r, errForbidden)
		return
	}
	var body ownerPatchRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	patch := patchServerRequest{
		DisplayName:           body.DisplayName,
		AutostartPolicy:       body.AutostartPolicy,
		IdleStopSeconds:       body.IdleStopSeconds,
		Memory:                body.Memory,
		Software:              body.Software,
		ConfirmSoftwareChange: body.ConfirmSoftwareChange,
		AcceptEULA:            body.AcceptEULA,
	}
	if body.CPU != nil {
		patch.Resources = &patchResourceRequest{CPU: body.CPU}
	}
	a.patchServer(w, r, name, patch)
}
