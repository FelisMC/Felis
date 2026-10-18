package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func (a *API) handleWake(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
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
	// A server pending retirement stays stopped until the reaper archives it; a start would change the archived world.
	if rec != nil && rec.Retire != nil {
		writeError(w, r, errServerRetiring)
		return
	}
	policy, _, err := a.readWakePolicy(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !a.limiter().allowed(name, policy.cooldown(a.WakeCooldown)) {
		writeError(w, r, newError(http.StatusTooManyRequests, "cooldown", "wake is cooling down, retry shortly"))
		return
	}
	ok, err := a.withinRunningCap(r.Context(), info, policy.MaxRunningServers)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, newError(http.StatusServiceUnavailable, "at_capacity",
			"the cluster is at its running-server cap; retry once a server stops"))
		return
	}

	// Explicit RetryStart resets the restart budget; join-driven wakes cannot, and maintenance still blocks both.
	action := "wake"
	if info.Phase == string(v1alpha1.PhaseFailed) && info.DesiredState == string(v1alpha1.DesiredRunning) {
		action = "retry_start"
		err = a.Cluster.RetryStart(r.Context(), name)
	} else {
		err = a.Cluster.SetDesiredState(r.Context(), name, v1alpha1.DesiredRunning)
	}
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	// Consume the cooldown only once the wake flipped, so a refused wake does not burn it.
	a.limiter().record(name)
	a.audit(r, action, name)
	writeJSON(w, http.StatusAccepted, map[string]any{"name": name, "desiredState": "Running"})
}

func (a *API) handleRestart(w http.ResponseWriter, r *http.Request) {
	name, ok := a.authorizeServerFiles(w, r)
	if !ok {
		return
	}
	rec, err := a.managedServerRecord(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if rec.Retire != nil {
		writeError(w, r, errServerRetiring)
		return
	}
	if err := a.Cluster.RestartServer(r.Context(), rec.Name); err != nil {
		a.writeLookupError(w, r, maintenanceError(err, "wait for maintenance to finish before restarting"))
		return
	}
	a.audit(r, "server.restart", rec.Name)
	writeJSON(w, http.StatusAccepted, map[string]string{"name": rec.Name, "desiredState": "Running"})
}

func (a *API) handleStop(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	rec, err := a.managedServerRecord(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return
	}

	if err := a.Cluster.SetDesiredState(r.Context(), name, v1alpha1.DesiredStopped); err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	a.audit(r, "stop", name)
	writeJSON(w, http.StatusAccepted, map[string]any{"name": name, "desiredState": "Stopped"})
}

func (a *API) handleClaim(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	name := r.PathValue("name")
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

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
	if rec, err := a.Repo.ServerByName(r.Context(), name); err == nil && rec.Retire != nil {
		writeError(w, r, errServerRetiring)
		return
	}

	res, err := a.claimResources(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	ok, err := a.Repo.QuotaCheck(r.Context(), p.UserID, "", res)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, newError(http.StatusForbidden, "quota_exceeded", "server quota exhausted"))
		return
	}

	claimed, err := a.Repo.ClaimServer(r.Context(), name, p.UserID)
	if err != nil {
		// A concurrent claim that spent the last slot surfaces here: ClaimServer re-checks quota under the per-user lock.
		if errors.Is(err, ErrQuotaExceeded) {
			writeError(w, r, newError(http.StatusForbidden, "quota_exceeded", "server quota exhausted"))
			return
		}
		a.writeLookupError(w, r, err)
		return
	}
	if !claimed {
		writeError(w, r, newError(http.StatusConflict, "already_claimed", "server is already claimed"))
		return
	}

	a.audit(r, "claim", name)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "claimed": true})
}

// claimResources refreshes sizes from the cluster because release clears the resource cache.
func (a *API) claimResources(ctx context.Context, name string) (ResourceSpec, error) {
	info, err := a.Cluster.GetServer(ctx, name)
	if err != nil {
		return ResourceSpec{}, err
	}
	storage, _ := resource.ParseQuantity(info.StorageSize)
	res := ResourceSpec{
		CPUMilli:  quantityToMilli(info.Resources.Limits[corev1.ResourceCPU]),
		MemoryMB:  quantityToMB(info.Resources.Limits[corev1.ResourceMemory]),
		StorageMB: quantityToMB(storage),
	}
	if err := a.Repo.UpdateServerResources(ctx, name, res.CPUMilli, res.MemoryMB, res.StorageMB); err != nil {
		return ResourceSpec{}, err
	}
	return res, nil
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	// The internal face (no principal) is the platform itself; externally, only the owner and staff get the full record.
	if p := principalFromContext(r.Context()); p != nil {
		rec, err := a.Repo.ServerByName(r.Context(), name)
		if err != nil && !errors.Is(err, ErrNotFound) {
			writeError(w, r, err)
			return
		}
		if !a.isOwnerOrAdmin(p, rec) {
			info = publicServerInfo(info)
		} else if rec != nil && rec.Retire != nil {
			withRetire := *info
			withRetire.Retiring = rec.Retire
			info = &withRetire
		}
	}
	writeJSON(w, http.StatusOK, info)
}

func publicServerInfo(s *ServerInfo) *ServerInfo {
	return &ServerInfo{
		Name:          s.Name,
		Subdomain:     s.Subdomain,
		DisplayName:   s.DisplayName,
		Phase:         s.Phase,
		Ready:         s.Ready,
		PlayersOnline: s.PlayersOnline,
		PlayersMax:    s.PlayersMax,
	}
}

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	emailVerified := false
	if u, err := a.Repo.UserByID(r.Context(), p.UserID); err == nil {
		emailVerified = u.EmailVerified
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":        p.UserID,
		"email":          p.Email,
		"role":           p.Role,
		"is_admin":       p.IsAdmin(),
		"is_owner":       p.IsOwner(),
		"email_verified": emailVerified,
	})
}

func (a *API) handleMyServers(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	servers, err := a.Repo.MyServers(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Live counts fall back to zero and cached phase on cluster failure; ownership details are joined only for owned rows.
	if infos, err := a.Cluster.ListServers(r.Context()); err == nil {
		byName := make(map[string]ServerInfo, len(infos))
		for _, s := range infos {
			byName[s.Name] = s
		}
		for i := range servers {
			info, ok := byName[servers[i].Name]
			if !ok {
				continue
			}
			v := &servers[i]
			v.PlayersOnline = info.PlayersOnline
			v.PlayersMax = info.PlayersMax
			v.DisplayName = info.DisplayName
			if info.Phase != "" {
				v.Phase = info.Phase
			}
			if v.Owned {
				v.DesiredState = info.DesiredState
				v.AutostartPolicy = info.AutostartPolicy
				v.PlayerCountUnknown = info.PlayerCountUnknown
				v.AutoRestarts = info.AutoRestarts
				v.StartGaveUp = info.StartGaveUp
			}
		}
	}
	if servers == nil {
		servers = []MyServerView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

func (a *API) handleFleet(w http.ResponseWriter, r *http.Request) {
	servers, err := a.Cluster.ListServers(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Ownership is best-effort: on a Postgres failure the rows say the owner is unknown and offer no claim.
	p := principalFromContext(r.Context())
	owners, err := a.Repo.ServerOwners(r.Context())
	unknown := err != nil
	views := make([]fleetServerView, len(servers))
	for i, s := range servers {
		o, known := owners[s.Name]
		system := naming.IsSystemServer(s.Name)
		s.Retiring = o.Retire
		views[i] = fleetServerView{ServerInfo: s, Owner: o.Owner, System: system,
			Owned:        o.OwnerID != "" && o.OwnerID == p.UserID,
			Claimable:    known && o.OwnerID == "" && o.Retire == nil && !system,
			OwnerUnknown: unknown}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": views})
}

type fleetServerView struct {
	ServerInfo
	// Owner is display text; OwnerUnknown distinguishes lookup failure from an unclaimed server.
	Owner string `json:"owner,omitempty"`
	Owned bool   `json:"owned"`
	// Claimable is false while ownership is unknown or deletion is pending.
	Claimable bool `json:"claimable"`
	// OwnerUnknown means an empty Owner does not establish that the server is unclaimed.
	OwnerUnknown bool `json:"ownerUnknown,omitempty"`
	// System identifies platform services with reserved names and no player ownership.
	System bool `json:"system,omitempty"`
}

type createServerRequest struct {
	NodeName        string           `json:"nodeName,omitempty"`
	Name            string           `json:"name"`
	Subdomain       string           `json:"subdomain"`
	DisplayName     string           `json:"displayName,omitempty"`
	Image           string           `json:"image"`
	Memory          string           `json:"memory"`
	Storage         string           `json:"storage"`
	AutostartPolicy string           `json:"autostartPolicy,omitempty"`
	Resources       *resourceRequest `json:"resources,omitempty"`
	Software        *softwareRequest `json:"software,omitempty"`
	// AcceptEULA authorizes writing eula=true on every start of runtime server software.
	AcceptEULA bool `json:"acceptEula,omitempty"`
}

type resourceRequest struct {
	CPU           string `json:"cpu,omitempty"`
	CPURequest    string `json:"cpuRequest,omitempty"`
	Memory        string `json:"memory,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
}

func (a *API) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var body createServerRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	if a.Distribution != nil {
		if err := a.Distribution.ValidateNode(r.Context(), body.NodeName); err != nil {
			writeError(w, r, newError(400, "bad_node", "select an approved worker: %v", err))
			return
		}
	} else if body.NodeName != "" {
		writeError(w, r, newError(400, "bad_node", "node selection requires distributed deployment"))
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

	var (
		image string
		sw    *v1alpha1.SoftwareSpec
	)
	if body.Software != nil {
		if strings.TrimSpace(body.Image) != "" {
			writeError(w, r, newError(http.StatusBadRequest, "bad_request", "give either an image or server software, not both"))
			return
		}
		if !body.AcceptEULA {
			writeError(w, r, errEULARequired)
			return
		}
		if sw, image, err = a.resolveSoftware(r.Context(), *body.Software); err != nil {
			writeError(w, r, err)
			return
		}
	} else if image, err = a.admitImage(r.Context(), body.Image); err != nil {
		writeError(w, r, err)
		return
	}

	// Staff-created servers start unowned; user quota is charged when claiming, not at creation.

	if err := a.checkNewServerName(r.Context(), body.Name, body.Subdomain); err != nil {
		writeError(w, r, err)
		return
	}

	cpuMilli := quantityToMilli(resources.Limits[corev1.ResourceCPU])
	memMB := quantityToMB(resources.Limits[corev1.ResourceMemory])
	storQ, _ := resource.ParseQuantity(storage)
	storMB := quantityToMB(storQ)
	if err := a.Repo.SeedServer(r.Context(), body.Name, body.Subdomain, cpuMilli, memMB, storMB); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "subdomain_taken",
				"subdomain %q is already in use", body.Subdomain))
			return
		}
		writeError(w, r, err)
		return
	}

	in := CreateServerInput{
		NodeName:        body.NodeName,
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
	if err := a.Cluster.CreateServer(r.Context(), in); err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(w, r, newError(http.StatusConflict, "already_exists",
				"a server named %q already exists", body.Name))
			return
		}
		writeError(w, r, err)
		return
	}

	a.audit(r, "server.create", body.Name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":         body.Name,
		"subdomain":    body.Subdomain,
		"desiredState": string(v1alpha1.DesiredStopped),
	})
}

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

// resolveResources derives the heap from the final nonzero pod memory limit, including overrides.
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

	if err := checkResourceBounds(limits, requests); err != nil {
		return "", corev1.ResourceRequirements{}, err
	}

	memLim, ok := limits[corev1.ResourceMemory]
	if !ok || memLim.IsZero() {
		return "", corev1.ResourceRequirements{}, newError(http.StatusInternalServerError, "internal",
			"refusing to create a server without a memory ceiling")
	}

	return deriveJavaHeap(memLim), corev1.ResourceRequirements{Limits: limits, Requests: requests}, nil
}

func checkResourceBounds(limits, requests corev1.ResourceList) error {
	if memReq, hasReq := requests[corev1.ResourceMemory]; hasReq {
		if memLim, hasLim := limits[corev1.ResourceMemory]; hasLim && memReq.Cmp(memLim) > 0 {
			return newError(http.StatusBadRequest, "bad_request",
				"memory request %s exceeds limit %s", memReq.String(), memLim.String())
		}
	}
	if cpuReq, hasReq := requests[corev1.ResourceCPU]; hasReq {
		if cpuLim, hasLim := limits[corev1.ResourceCPU]; hasLim && cpuReq.Cmp(cpuLim) > 0 {
			return newError(http.StatusBadRequest, "bad_request",
				"cpu request %s exceeds limit %s", cpuReq.String(), cpuLim.String())
		}
	}
	return nil
}

// patchResourceRequest keeps absent fields, permits clearing CPU constraints and never removes the memory limit.
type patchResourceRequest struct {
	CPU           *string `json:"cpu,omitempty"`
	CPURequest    *string `json:"cpuRequest,omitempty"`
	Memory        *string `json:"memory,omitempty"`
	MemoryRequest *string `json:"memoryRequest,omitempty"`
}

// mergeResources preserves absent fields and derives the heap from the final memory limit; top-level memory sets both request and limit.
func mergeResources(cur corev1.ResourceRequirements, memory *string, rr *patchResourceRequest) (string, bool, corev1.ResourceRequirements, error) {
	out := *cur.DeepCopy()
	if out.Limits == nil {
		out.Limits = corev1.ResourceList{}
	}
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	fail := func(err error) (string, bool, corev1.ResourceRequirements, error) {
		return "", false, corev1.ResourceRequirements{}, err
	}
	set := func(list corev1.ResourceList, key corev1.ResourceName, v *string, field string, removable bool) error {
		if v == nil {
			return nil
		}
		if *v == "" {
			if !removable {
				return newError(http.StatusBadRequest, "bad_request", "%s cannot be empty", field)
			}
			delete(list, key)
			return nil
		}
		q, err := parsePositiveQuantity(*v, field)
		if err != nil {
			return err
		}
		list[key] = q
		return nil
	}

	if err := set(out.Limits, corev1.ResourceMemory, memory, "memory", false); err != nil {
		return fail(err)
	}
	if memory != nil {
		out.Requests[corev1.ResourceMemory] = out.Limits[corev1.ResourceMemory]
	}
	if rr != nil {
		for _, f := range []struct {
			list      corev1.ResourceList
			key       corev1.ResourceName
			v         *string
			field     string
			removable bool
		}{
			{out.Limits, corev1.ResourceMemory, rr.Memory, "resources.memory", false},
			{out.Requests, corev1.ResourceMemory, rr.MemoryRequest, "resources.memoryRequest", false},
			{out.Limits, corev1.ResourceCPU, rr.CPU, "resources.cpu", true},
			{out.Requests, corev1.ResourceCPU, rr.CPURequest, "resources.cpuRequest", true},
		} {
			if err := set(f.list, f.key, f.v, f.field, f.removable); err != nil {
				return fail(err)
			}
		}
	}
	if err := checkResourceBounds(out.Limits, out.Requests); err != nil {
		return fail(err)
	}

	memLim, ok := out.Limits[corev1.ResourceMemory]
	if !ok || memLim.IsZero() {
		return fail(newError(http.StatusBadRequest, "bad_request",
			"this server has no memory ceiling; set memory in the same patch"))
	}
	curLim := cur.Limits[corev1.ResourceMemory]
	return deriveJavaHeap(memLim), memLim.Cmp(curLim) != 0, out, nil
}

// deriveJavaHeap emits JVM-compatible MiB and reserves the larger of 512 MiB or 25% for off-heap use, capped at half the pod limit.
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

func parseStorageSize(s string) (string, error) {
	q, err := parsePositiveQuantity(s, "storage")
	if err != nil {
		return "", err
	}
	return q.String(), nil
}

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

type patchServerRequest struct {
	DisplayName     *string `json:"displayName,omitempty"`
	AutostartPolicy *string `json:"autostartPolicy,omitempty"`
	Image           *string `json:"image,omitempty"`
	// ConfirmImageChange acknowledges world-format changes that may prevent downgrade.
	ConfirmImageChange bool                  `json:"confirmImageChange,omitempty"`
	Memory             *string               `json:"memory,omitempty"`
	Resources          *patchResourceRequest `json:"resources,omitempty"`
	IdleStopSeconds    *int32                `json:"idleStopSeconds,omitempty"`
	// Software type or Minecraft-version changes require confirmation; moving from a server image also requires EULA acceptance.
	Software              *softwareRequest `json:"software,omitempty"`
	ConfirmSoftwareChange bool             `json:"confirmSoftwareChange,omitempty"`
	AcceptEULA            bool             `json:"acceptEula,omitempty"`
	// Storage is recognized only to return storage_immutable; this build does not orchestrate PVC expansion.
	Storage *string `json:"storage,omitempty"`
}

const (
	minIdleStopSeconds = 60
	maxIdleStopSeconds = 86400
)

func (a *API) handlePatchServer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}

	var body patchServerRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	a.patchServer(w, r, name, body)
}

func (a *API) patchServer(w http.ResponseWriter, r *http.Request, name string, body patchServerRequest) {
	if naming.IsSystemServer(name) &&
		((body.AutostartPolicy != nil && *body.AutostartPolicy != string(v1alpha1.AutostartPublic)) ||
			(body.IdleStopSeconds != nil && *body.IdleStopSeconds != 0)) {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "system services must remain public and exempt from idle stop"))
		return
	}

	if body.DisplayName == nil && body.AutostartPolicy == nil && body.Image == nil &&
		body.Memory == nil && body.Resources == nil && body.Storage == nil && body.IdleStopSeconds == nil &&
		body.Software == nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request",
			"patch must set at least one field"))
		return
	}
	if body.Storage != nil {
		writeError(w, r, newError(http.StatusBadRequest, "storage_immutable",
			"storage size cannot be changed through this endpoint (PVC capacity is immutable)"))
		return
	}

	var patch ServerSpecPatch
	// Keep changed non-nil so no-op patches serialize as an empty array.
	changed := []string{}
	var imageFrom string
	var cur *ServerInfo
	current := func() (*ServerInfo, error) {
		if cur != nil {
			return cur, nil
		}
		info, err := a.Cluster.GetServer(r.Context(), name)
		cur = info
		return info, err
	}

	if body.DisplayName != nil {
		displayName, err := naming.CleanDisplayName(*body.DisplayName)
		if err != nil {
			writeError(w, r, newError(http.StatusBadRequest, "bad_display_name", "invalid display name: %v", err))
			return
		}
		patch.DisplayName = &displayName
		changed = append(changed, "displayName")
	}

	if body.AutostartPolicy != nil {
		// Patch policies reject explicit empty values instead of applying creation defaults.
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

	if body.IdleStopSeconds != nil {
		if s := *body.IdleStopSeconds; s != 0 && (s < minIdleStopSeconds || s > maxIdleStopSeconds) {
			writeError(w, r, newError(http.StatusBadRequest, "bad_idle_stop",
				"idleStopSeconds must be 0 (off) or between %d and %d", minIdleStopSeconds, maxIdleStopSeconds))
			return
		}
		patch.IdleStopSeconds = body.IdleStopSeconds
		changed = append(changed, "idleStopSeconds")
	}

	if body.Image != nil {
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
		image, err := a.pinImage(r.Context(), *body.Image)
		if err != nil {
			writeError(w, r, err)
			return
		}
		info, err := current()
		if err != nil {
			a.writeLookupError(w, r, err)
			return
		}
		// Selecting the original tag again may resolve to a newer digest; only matching the current digest is a no-op.
		if image != info.Image {
			if !body.ConfirmImageChange {
				writeError(w, r, newError(http.StatusConflict, "image_change_unconfirmed",
					"changing the image from %q to %q opens this world with the new image's Minecraft version, "+
						"and chunks it upgrades cannot be opened by the old one again; back the world up first, "+
						"then resend with confirmImageChange", info.Image, image))
				return
			}
			patch.Image = &image
			changed = append(changed, "image")
			imageFrom = info.Image
			patch.ClearSoftware = info.Software != nil
		}
	}

	if body.Software != nil {
		if body.Image != nil {
			writeError(w, r, newError(http.StatusBadRequest, "bad_request", "give either an image or server software, not both"))
			return
		}
		info, err := current()
		if err != nil {
			a.writeLookupError(w, r, err)
			return
		}
		if info.Software == nil && !body.AcceptEULA {
			writeError(w, r, errEULARequired)
			return
		}
		sw, image, err := a.resolveSoftware(r.Context(), *body.Software)
		if err != nil {
			writeError(w, r, err)
			return
		}
		// Type or Minecraft-version changes may irreversibly upgrade chunks; same-version build or Java changes do not require confirmation.
		moves := info.Software == nil || info.Software.Type != sw.Type || info.Software.MinecraftVersion != sw.MinecraftVersion
		if moves && !body.ConfirmSoftwareChange {
			writeError(w, r, newError(http.StatusConflict, "software_change_unconfirmed",
				"moving this server to %s %s opens its world with that software and version, and chunks it upgrades "+
					"cannot be opened by an older version again; back the world up first, then resend with confirmSoftwareChange",
				sw.Type, sw.MinecraftVersion))
			return
		}
		patch.Software = sw
		if image != info.Image {
			patch.Image = &image
		}
		changed = append(changed, "software")
	}

	var (
		newResources corev1.ResourceRequirements
		resUpdated   bool
	)
	if body.Memory != nil || body.Resources != nil {
		info, err := current()
		if err != nil {
			a.writeLookupError(w, r, err)
			return
		}
		javaMemory, memMoved, resources, err := mergeResources(info.Resources, body.Memory, body.Resources)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if body.Memory != nil || memMoved {
			patch.JavaMemory = &javaMemory
		}
		patch.Resources = &resources
		if body.Memory != nil {
			changed = append(changed, "memory")
		}
		if body.Resources != nil {
			changed = append(changed, "resources")
		}
		newResources = resources
		resUpdated = true
	}

	// Resize the quota cache under the owner's claim lock before cluster writes; shrink-only changes remain allowed above quota.
	if resUpdated && !naming.IsSystemServer(name) {
		newCPU := quantityToMilli(newResources.Limits[corev1.ResourceCPU])
		newMemMB := quantityToMB(newResources.Limits[corev1.ResourceMemory])

		prev, err := a.Repo.ResizeServer(r.Context(), name, newCPU, newMemMB)
		if errors.Is(err, ErrQuotaExceeded) {
			writeError(w, r, newError(http.StatusForbidden, "quota_exceeded",
				"this change would exceed the server owner's resource quota"))
			return
		}
		if err != nil {
			writeError(w, r, err)
			return
		}
		if err := a.Cluster.PatchServerSpec(r.Context(), name, patch); err != nil {
			// On cluster-write failure restore the cache from the cluster or, if unreadable, its previous values.
			ctx := context.WithoutCancel(r.Context())
			if _, rerr := a.claimResources(ctx, name); rerr != nil {
				if rerr := a.Repo.UpdateServerResources(ctx, name, prev.CPUMilli, prev.MemoryMB, prev.StorageMB); rerr != nil {
					log.Printf("api: resize of %s refused by the cluster, resource cache left at the new size (request_id=%s): %v",
						name, requestIDFromContext(ctx), rerr)
				}
			}
			a.writeLookupError(w, r, err)
			return
		}
	} else {
		if err := a.Cluster.PatchServerSpec(r.Context(), name, patch); err != nil {
			a.writeLookupError(w, r, err)
			return
		}
	}

	switch {
	case patch.Software != nil:
		a.audit(r, "server.software", name)
	case patch.Image != nil:
		a.auditImageChange(r, name, imageFrom, *patch.Image)
	default:
		a.audit(r, "server.patch", name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    name,
		"patched": changed,
	})
}

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
	default:
		return errForbidden
	}
}

func (a *API) isOwnerOrAdmin(p *Principal, rec *ServerRecord) bool {
	if p.IsAdmin() {
		return true
	}
	return rec != nil && rec.OwnerID != "" && rec.OwnerID == p.UserID
}

func quantityToMilli(q resource.Quantity) int {
	if q.IsZero() {
		return 0
	}
	return int(q.MilliValue())
}

// quantityToMB rounds up in MiB, which quota columns call MB.
func quantityToMB(q resource.Quantity) int {
	const mib = 1 << 20
	return int((q.Value() + mib - 1) / mib)
}
