package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"felis.lolicon.best/internal/software"
)

// fakeOwned is one server SeedOwnedServer wrote.
type fakeOwned struct {
	owner string
	res   ResourceSpec
}

// fakeCaps mirrors readQuota: the user's own quota row (fakeQuotas), else the
// default quota setting when it is on, else none.
func (f *fakeRepo) fakeCaps(userID string) (*QuotaInput, string) {
	if q, ok := f.fakeQuotas[userID]; ok {
		return &QuotaInput{MaxServers: q.MaxServers, MaxCPUMilli: q.MaxCPUMilli, MaxMemoryMB: q.MaxMemoryMB, MaxStorageGB: q.MaxStorageGB}, "user"
	}
	var d selfServiceQuota
	if raw, ok := f.settings[selfServiceQuotaKey]; ok && json.Unmarshal(raw, &d) == nil && d.Enabled {
		return &QuotaInput{MaxServers: d.MaxServers, MaxCPUMilli: d.MaxCPUMilli, MaxMemoryMB: d.MaxMemoryMB, MaxStorageGB: d.MaxStorageGB}, "default"
	}
	return nil, ""
}

func (f *fakeRepo) usage(userID, exclude string) QuotaUsage {
	var u QuotaUsage
	for name, o := range f.owned {
		if o.owner != userID || name == exclude {
			continue
		}
		u.Servers++
		u.CPUMilli += o.res.CPUMilli
		u.MemoryMB += o.res.MemoryMB
		u.StorageMB += o.res.StorageMB
	}
	return u
}

func capFits(limit *int, used, incoming int) bool { return limit == nil || used+incoming <= *limit }

func (f *fakeRepo) SeedOwnedServer(_ context.Context, name, subdomain, userID string, res ResourceSpec) error {
	caps, source := f.fakeCaps(userID)
	if source == "" {
		return ErrSelfServiceDisabled
	}
	u := f.usage(userID, name)
	if !capFits(caps.MaxServers, u.Servers, 1) || !capFits(caps.MaxCPUMilli, u.CPUMilli, res.CPUMilli) ||
		!capFits(caps.MaxMemoryMB, u.MemoryMB, res.MemoryMB) ||
		(caps.MaxStorageGB != nil && u.StorageMB+res.StorageMB > *caps.MaxStorageGB*1024) {
		return ErrQuotaExceeded
	}
	if bound, ok := f.aliases[subdomain]; ok && bound != name {
		return ErrConflict
	}
	f.owned[name] = fakeOwned{owner: userID, res: res}
	f.aliases[subdomain] = name
	f.byName[name] = &ServerRecord{Name: name, OwnerID: userID}
	return nil
}

func (f *fakeRepo) DiscardSeededServer(_ context.Context, name string) error {
	delete(f.owned, name)
	delete(f.byName, name)
	for sub, bound := range f.aliases {
		if bound == name {
			delete(f.aliases, sub)
		}
	}
	return nil
}

func (f *fakeRepo) SelfServiceState(_ context.Context, userID string) (QuotaState, error) {
	caps, source := f.fakeCaps(userID)
	return QuotaState{Source: source, Limits: caps, Usage: f.usage(userID, "")}, nil
}

// fakeCatalog answers like the upstream metadata the resolver reads: Paper
// offers 1.21.4 (builds 232, recommended, and 231) and a 1.21.5 release
// candidate, both on Java 21; vanilla offers 1.21.4.
type fakeCatalog struct{ err error }

func (c fakeCatalog) Versions(_ context.Context, t software.Type) ([]software.Version, error) {
	if c.err != nil {
		return nil, c.err
	}
	return []software.Version{{ID: "1.21.5-rc1"}, {ID: "1.21.4", Stable: true}}, nil
}

func (c fakeCatalog) Builds(_ context.Context, t software.Type, mc string) ([]software.Build, error) {
	return []software.Build{{ID: "232", Stable: true, Recommended: true}, {ID: "231", Stable: true}}, nil
}

func (c fakeCatalog) RequiredJava(context.Context, software.Type, string) (int, error) {
	return 21, nil
}

func (c fakeCatalog) ForwardingFor(_ context.Context, s software.Spec) (string, error) {
	if s.Type == software.Vanilla {
		return software.ForwardingNone, nil
	}
	return software.ForwardingVelocity, nil
}

const runtimeRegistry = "registry.felis.svc:5000"

// newSelfServiceAPI is an API whose caller is player "u1", linked to a
// Minecraft account, with server software configured.
func newSelfServiceAPI() (*API, *fakeRepo, *fakeCluster) {
	repo := newFakeRepo()
	cl := newFakeCluster()
	a := newTestAPI(repo, cl)
	a.Software = fakeCatalog{}
	a.RuntimeRegistry = runtimeRegistry
	a.External = staticExternal{p: &Principal{UserID: "u1", Email: "player@example.net", Role: "user"}}
	repo.linked["u1"] = true
	return a, repo, cl
}

func enableDefaultQuota(repo *fakeRepo, servers, memoryMB int) {
	raw, _ := json.Marshal(selfServiceQuota{Enabled: true, MaxServers: &servers, MaxMemoryMB: &memoryMB})
	repo.settings[selfServiceQuotaKey] = raw
}

func selfCreate(a *API, name, memory string) (int, string, string) {
	body := `{"name":"` + name + `","subdomain":"` + name + `","memory":"` + memory + `","storage":"10Gi",` +
		`"software":{"type":"paper","minecraftVersion":"1.21.4"},"acceptEula":true}`
	w := do(a.ExternalHandler(), "POST", "/api/v1/me/servers", body, nil)
	code := ""
	if w.Code >= 400 {
		var raw map[string]map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &raw)
		code = raw["error"]["code"]
	}
	return w.Code, code, w.Body.String()
}

// The Owner's answer for a fresh install: nobody without a quota of their own
// can create a server until a default quota is turned on.
func TestSelfCreateIsOffWithoutAQuota(t *testing.T) {
	a, _, cl := newSelfServiceAPI()
	if status, code, body := selfCreate(a, "mine", "2Gi"); status != http.StatusForbidden || code != "self_service_disabled" {
		t.Fatalf("create without a quota = %d %s (%s), want 403 self_service_disabled", status, code, body)
	}
	if len(cl.created) != 0 {
		t.Errorf("a server was created: %v", cl.created)
	}
}

// Under a default quota of one 4 GiB server a user gets exactly one, owned by
// them, running the software they picked on the runtime image of its Java.
func TestSelfCreateWithinTheDefaultQuota(t *testing.T) {
	a, repo, cl := newSelfServiceAPI()
	enableDefaultQuota(repo, 1, 4096)

	if status, code, body := selfCreate(a, "mine", "6Gi"); status != http.StatusForbidden || code != "quota_exceeded" {
		t.Fatalf("6Gi under a 4096 MB quota = %d %s (%s), want 403 quota_exceeded", status, code, body)
	}
	if status, _, body := selfCreate(a, "mine", "2Gi"); status != http.StatusCreated {
		t.Fatalf("2Gi under a 4096 MB quota = %d (%s), want 201", status, body)
	}
	in := cl.created["mine"]
	if in.Software == nil || in.Software.Type != "paper" || in.Software.MinecraftVersion != "1.21.4" {
		t.Fatalf("created software = %+v, want paper 1.21.4", in.Software)
	}
	if in.Software.Build != "232" {
		t.Errorf("build = %q, want the recommended 232 when none was picked", in.Software.Build)
	}
	if in.Software.Java != 21 || in.Image != runtimeRegistry+"/mirror/eclipse-temurin:21-jre" {
		t.Errorf("java %d on image %q, want the Java 21 runtime image", in.Software.Java, in.Image)
	}
	if in.Software.Forwarding != software.ForwardingVelocity {
		t.Errorf("forwarding = %q, want velocity", in.Software.Forwarding)
	}
	if rec, err := repo.ServerByName(context.Background(), "mine"); err != nil || rec.OwnerID != "u1" {
		t.Errorf("owner of the new server = %+v, %v; want u1", rec, err)
	}
	if status, code, body := selfCreate(a, "second", "1Gi"); status != http.StatusForbidden || code != "quota_exceeded" {
		t.Fatalf("a second server under a one-server quota = %d %s (%s), want 403 quota_exceeded", status, code, body)
	}
}

// A user's own quota applies even with the default quota off.
func TestSelfCreateUnderTheUsersOwnQuota(t *testing.T) {
	a, repo, _ := newSelfServiceAPI()
	two := 2
	repo.fakeQuotas["u1"] = &QuotaView{UserID: "u1", MaxServers: &two}
	for _, name := range []string{"first", "second"} {
		if status, _, body := selfCreate(a, name, "2Gi"); status != http.StatusCreated {
			t.Fatalf("create %s = %d (%s), want 201", name, status, body)
		}
	}
	if status, code, _ := selfCreate(a, "third", "2Gi"); code != "quota_exceeded" {
		t.Fatalf("third server under a two-server quota = %d %s, want quota_exceeded", status, code)
	}
}

func TestSelfCreateNeedsALinkedAccountAndTheEULA(t *testing.T) {
	a, repo, _ := newSelfServiceAPI()
	enableDefaultQuota(repo, 1, 4096)
	noEULA := `{"name":"mine","subdomain":"mine","memory":"2Gi","storage":"10Gi","software":{"type":"paper","minecraftVersion":"1.21.4"}}`
	if w := do(a.ExternalHandler(), "POST", "/api/v1/me/servers", noEULA, nil); w.Code != http.StatusBadRequest || decodeErr(t, w) != "eula_required" {
		t.Fatalf("create without accepting the EULA = %d %s, want 400 eula_required", w.Code, w.Body.String())
	}
	repo.linked["u1"] = false
	if status, code, _ := selfCreate(a, "mine", "2Gi"); status != http.StatusPreconditionFailed || code != "not_linked" {
		t.Fatalf("create by an unlinked user = %d %s, want 412 not_linked", status, code)
	}
}

// A server the cluster refuses must not keep the quota slot or the subdomain:
// the next attempt, once the cluster takes it, fits the same one-server quota.
func TestSelfCreateGivesTheSlotBackWhenTheClusterRefuses(t *testing.T) {
	a, repo, cl := newSelfServiceAPI()
	enableDefaultQuota(repo, 1, 4096)
	cl.createErr = errors.New("admission webhook said no")
	if status, _, _ := selfCreate(a, "mine", "2Gi"); status < 500 {
		t.Fatalf("create the cluster refused = %d, want a server error", status)
	}
	cl.createErr = nil
	// Another name: re-seeding the same name never counts the old row, so only
	// a fresh name shows whether the refused server still holds the one slot.
	if status, _, body := selfCreate(a, "other", "2Gi"); status != http.StatusCreated {
		t.Fatalf("next create after the refusal = %d (%s), want 201", status, body)
	}
}

func TestResolveSoftwareRefusesWhatUpstreamDoesNotOffer(t *testing.T) {
	a, _, _ := newSelfServiceAPI()
	for _, tc := range []struct {
		name string
		req  softwareRequest
		code string
	}{
		{"unknown version", softwareRequest{Type: "paper", MinecraftVersion: "1.99"}, "software_not_found"},
		{"unknown build", softwareRequest{Type: "paper", MinecraftVersion: "1.21.4", Build: "9999"}, "software_not_found"},
		{"java older than the version needs", softwareRequest{Type: "paper", MinecraftVersion: "1.21.4", Java: 17}, "java_too_old"},
		{"java the platform does not ship", softwareRequest{Type: "paper", MinecraftVersion: "1.21.4", Java: 23}, "bad_software"},
		{"custom jar outside the server", softwareRequest{Type: "custom", Jar: "../../etc/passwd.jar"}, "bad_software"},
		{"unknown type", softwareRequest{Type: "bukkit", MinecraftVersion: "1.21.4"}, "bad_software"},
	} {
		_, _, err := a.resolveSoftware(context.Background(), tc.req)
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.code != tc.code {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.code)
		}
	}
	if _, _, err := a.resolveSoftware(context.Background(), softwareRequest{Type: "paper", MinecraftVersion: "1.21.4", Java: 25}); err != nil {
		t.Errorf("a newer Java than required is the owner's choice: %v", err)
	}
	a.Software = fakeCatalog{err: errors.New("dial tcp: i/o timeout")}
	_, _, err := a.resolveSoftware(context.Background(), softwareRequest{Type: "paper", MinecraftVersion: "1.21.4"})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.status != http.StatusBadGateway {
		t.Errorf("unreachable upstream: err = %v, want 502", err)
	}
}

// Moving a world to another version is one-way, so the owner confirms it; the
// change is theirs to make and nobody else's.
func TestOwnerChangesTheirServersSoftware(t *testing.T) {
	a, repo, cl := newSelfServiceAPI()
	enableDefaultQuota(repo, 1, 4096)
	if status, _, body := selfCreate(a, "mine", "2Gi"); status != http.StatusCreated {
		t.Fatalf("create = %d (%s)", status, body)
	}
	h := a.ExternalHandler()
	move := `{"software":{"type":"vanilla","minecraftVersion":"1.21.4"}}`
	if w := do(h, "PATCH", "/api/v1/me/servers/mine", move, nil); w.Code != http.StatusConflict || decodeErr(t, w) != "software_change_unconfirmed" {
		t.Fatalf("unconfirmed move = %d %s, want 409 software_change_unconfirmed", w.Code, w.Body.String())
	}
	confirmed := strings.TrimSuffix(move, "}") + `,"confirmSoftwareChange":true}`
	if w := do(h, "PATCH", "/api/v1/me/servers/mine", confirmed, nil); w.Code != http.StatusOK {
		t.Fatalf("confirmed move = %d %s, want 200", w.Code, w.Body.String())
	}
	if got := cl.byName["mine"].Software; got == nil || got.Type != "vanilla" || got.Forwarding != "none" {
		t.Errorf("software after the move = %+v, want vanilla without forwarding", got)
	}
	newBuild := `{"software":{"type":"vanilla","minecraftVersion":"1.21.4","java":25}}`
	if w := do(h, "PATCH", "/api/v1/me/servers/mine", newBuild, nil); w.Code != http.StatusOK {
		t.Errorf("another Java for the same version = %d %s, want 200 without confirmation", w.Code, w.Body.String())
	}

	a.External = staticExternal{p: &Principal{UserID: "u2", Email: "other@example.net", Role: "user"}}
	if w := do(a.ExternalHandler(), "PATCH", "/api/v1/me/servers/mine", confirmed, nil); w.Code != http.StatusForbidden {
		t.Errorf("another user's patch = %d, want 403", w.Code)
	}
}

func TestMyServerQuotaSaysWhyNot(t *testing.T) {
	a, repo, _ := newSelfServiceAPI()
	read := func() serverQuotaView {
		t.Helper()
		w := do(a.ExternalHandler(), "GET", "/api/v1/me/server-quota", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET /me/server-quota = %d %s", w.Code, w.Body.String())
		}
		var v serverQuotaView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := read(); v.CanCreate || v.Reason != "self_service_disabled" {
		t.Errorf("without a quota: %+v, want self_service_disabled", v)
	}
	enableDefaultQuota(repo, 1, 4096)
	if v := read(); !v.CanCreate || v.Source != "default" {
		t.Errorf("under the default quota: %+v, want canCreate from default", v)
	}
	if status, _, body := selfCreate(a, "mine", "2Gi"); status != http.StatusCreated {
		t.Fatalf("create = %d (%s)", status, body)
	}
	if v := read(); v.CanCreate || v.Reason != "quota_full" || v.Usage.Servers != 1 || v.Usage.MemoryMB != 2048 {
		t.Errorf("with the one slot used: %+v, want quota_full and the 2048 MB server counted", v)
	}
}
