package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/software"
)

// SoftwareCatalog reads upstream server-software metadata: the version and
// build pickers, and what a chosen version needs. *software.Resolver is the
// implementation; tests use a fake.
type SoftwareCatalog interface {
	Versions(ctx context.Context, t software.Type) ([]software.Version, error)
	Builds(ctx context.Context, t software.Type, mc string) ([]software.Build, error)
	RequiredJava(ctx context.Context, t software.Type, mc string) (int, error)
	ForwardingFor(ctx context.Context, s software.Spec) (string, error)
}

var errSoftwareUnavailable = newError(http.StatusServiceUnavailable, "software_unavailable",
	"server software is not configured on this install")

// softwareRequest is the software block of a create or patch form.
type softwareRequest struct {
	Type             string `json:"type"`
	MinecraftVersion string `json:"minecraftVersion,omitempty"`
	// Build is optional: empty takes the recommended build.
	Build string `json:"build,omitempty"`
	// Java is optional: zero takes the runtime the version needs.
	Java int    `json:"java,omitempty"`
	Jar  string `json:"jar,omitempty"`
}

// softwareError maps a catalog failure onto the API's error shape: a version or
// build upstream does not publish is the caller's mistake, anything else is the
// upstream being unreachable.
func softwareError(err error) error {
	var nf *software.NotFoundError
	switch {
	case errors.As(err, &nf):
		return newError(http.StatusBadRequest, "software_not_found", "%v", err)
	case errors.Is(err, software.ErrInvalid):
		return newError(http.StatusBadRequest, "bad_software", "%v", err)
	default:
		return newError(http.StatusBadGateway, "software_metadata_unavailable",
			"the server software's download site could not be reached: %v", err)
	}
}

// resolveSoftware validates a software block against upstream and fills in what
// it leaves out: the recommended build, the Java runtime the version needs, and
// whether the proxy's player identity can reach the server. It returns the spec
// and the pinned runtime image the server will run on.
func (a *API) resolveSoftware(ctx context.Context, req softwareRequest) (*v1alpha1.SoftwareSpec, string, error) {
	if a.Software == nil {
		return nil, "", errSoftwareUnavailable
	}
	t := software.Type(req.Type)
	if !t.Valid() {
		return nil, "", newError(http.StatusBadRequest, "bad_software", "unknown server software %q", req.Type)
	}
	s := software.Spec{Type: t, MinecraftVersion: req.MinecraftVersion, Build: req.Build, Jar: req.Jar}
	if t == software.Custom {
		s.Java = req.Java
		if s.Java == 0 {
			s.Java = 21
		}
		s.Forwarding = software.ForwardingNone
		if err := s.Validate(); err != nil {
			return nil, "", softwareError(err)
		}
	} else {
		vs, err := a.Software.Versions(ctx, t)
		if err != nil {
			return nil, "", softwareError(err)
		}
		if !slices.ContainsFunc(vs, func(v software.Version) bool { return v.ID == s.MinecraftVersion }) {
			return nil, "", newError(http.StatusBadRequest, "software_not_found",
				"%s does not offer Minecraft %q", t, s.MinecraftVersion)
		}
		if t.HasBuilds() {
			builds, err := a.Software.Builds(ctx, t, s.MinecraftVersion)
			if err != nil {
				return nil, "", softwareError(err)
			}
			if s.Build == "" {
				s.Build = recommendedBuild(builds)
			} else if !slices.ContainsFunc(builds, func(b software.Build) bool { return b.ID == s.Build }) {
				return nil, "", newError(http.StatusBadRequest, "software_not_found",
					"%s has no build %q for Minecraft %s", t, s.Build, s.MinecraftVersion)
			}
		}
		required, err := a.Software.RequiredJava(ctx, t, s.MinecraftVersion)
		if err != nil {
			return nil, "", softwareError(err)
		}
		runtime, err := software.RuntimeFor(required)
		if err != nil {
			return nil, "", newError(http.StatusBadRequest, "java_unavailable", "%v", err)
		}
		s.Java = runtime
		if req.Java != 0 {
			if !software.ValidRuntime(req.Java) {
				return nil, "", newError(http.StatusBadRequest, "bad_software", "java must be one of %v", software.Runtimes)
			}
			if req.Java < required {
				return nil, "", newError(http.StatusBadRequest, "java_too_old",
					"Minecraft %s needs Java %d or newer", s.MinecraftVersion, required)
			}
			s.Java = req.Java
		}
		if s.Forwarding, err = a.Software.ForwardingFor(ctx, s); err != nil {
			return nil, "", softwareError(err)
		}
		if err := s.Validate(); err != nil {
			return nil, "", softwareError(err)
		}
	}
	ref := build.RuntimeImageRef(a.RuntimeRegistry, s.Java)
	if ref == "" || a.RuntimeRegistry == "" {
		return nil, "", errSoftwareUnavailable
	}
	image, err := a.pinImage(ctx, ref)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.code == "image_not_in_registry" {
			return nil, "", newError(http.StatusServiceUnavailable, "runtime_unavailable",
				"the Java %d runtime has not been copied into the platform registry yet; run `felis mirror-build-tools` on the host, or wait for its next scheduled run", s.Java)
		}
		return nil, "", err
	}
	return &v1alpha1.SoftwareSpec{
		Type:             string(s.Type),
		MinecraftVersion: s.MinecraftVersion,
		Build:            s.Build,
		Java:             int32(s.Java),
		Jar:              s.Jar,
		Forwarding:       s.Forwarding,
	}, image, nil
}

func recommendedBuild(builds []software.Build) string {
	for _, b := range builds {
		if b.Recommended {
			return b.ID
		}
	}
	if len(builds) > 0 {
		return builds[0].ID
	}
	return ""
}

// softwareTypeView is one entry of the software catalog.
type softwareTypeView struct {
	Type software.Type `json:"type"`
	// Builds is true when the type picks a build or loader version.
	Builds bool `json:"builds"`
}

// handleSoftwareCatalog lists the software types and Java runtimes on offer.
func (a *API) handleSoftwareCatalog(w http.ResponseWriter, r *http.Request) {
	if a.Software == nil {
		writeError(w, r, errSoftwareUnavailable)
		return
	}
	types := make([]softwareTypeView, 0, len(software.Types))
	for _, t := range software.Types {
		types = append(types, softwareTypeView{Type: t, Builds: t.HasBuilds()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"types": types, "runtimes": software.Runtimes})
}

// handleSoftwareVersions lists the Minecraft versions a type runs.
func (a *API) handleSoftwareVersions(w http.ResponseWriter, r *http.Request) {
	if a.Software == nil {
		writeError(w, r, errSoftwareUnavailable)
		return
	}
	t := software.Type(r.PathValue("type"))
	if !t.Valid() || t == software.Custom {
		writeError(w, r, newError(http.StatusNotFound, "not_found", "no versions for %q", t))
		return
	}
	vs, err := a.Software.Versions(r.Context(), t)
	if err != nil {
		writeError(w, r, softwareError(err))
		return
	}
	if vs == nil {
		vs = []software.Version{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
}

// handleSoftwareVersion describes one version: the Java it needs, its builds,
// and whether the proxy's player identity can reach a server running it.
func (a *API) handleSoftwareVersion(w http.ResponseWriter, r *http.Request) {
	if a.Software == nil {
		writeError(w, r, errSoftwareUnavailable)
		return
	}
	t := software.Type(r.PathValue("type"))
	mc := r.PathValue("version")
	if !t.Valid() || t == software.Custom {
		writeError(w, r, newError(http.StatusNotFound, "not_found", "no versions for %q", t))
		return
	}
	ctx := r.Context()
	vs, err := a.Software.Versions(ctx, t)
	if err != nil {
		writeError(w, r, softwareError(err))
		return
	}
	if !slices.ContainsFunc(vs, func(v software.Version) bool { return v.ID == mc }) {
		writeError(w, r, newError(http.StatusNotFound, "software_not_found", "%s does not offer Minecraft %q", t, mc))
		return
	}
	builds := []software.Build{}
	if t.HasBuilds() {
		if builds, err = a.Software.Builds(ctx, t, mc); err != nil {
			writeError(w, r, softwareError(err))
			return
		}
	}
	required, err := a.Software.RequiredJava(ctx, t, mc)
	if err != nil {
		writeError(w, r, softwareError(err))
		return
	}
	runtime, rerr := software.RuntimeFor(required)
	fwd, err := a.Software.ForwardingFor(ctx, software.Spec{Type: t, MinecraftVersion: mc})
	if err != nil {
		writeError(w, r, softwareError(err))
		return
	}
	view := map[string]any{
		"version":      mc,
		"requiredJava": required,
		"builds":       builds,
		"forwarding":   fwd,
	}
	if rerr == nil {
		view["java"] = runtime
	}
	writeJSON(w, http.StatusOK, view)
}
