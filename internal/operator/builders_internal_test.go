package operator

import (
	"slices"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/placement"
	corev1 "k8s.io/api/core/v1"
)

func TestMigratedWorldPlacementAndFailClosedGate(t *testing.T) {
	s := &v1alpha1.MinecraftServer{}
	s.Spec.NodeName = "c"
	s.Spec.Storage.ClaimName = "world-alice-migrated"
	sts, err := buildStatefulSet(s, 1, "felis:test", "felis-api.felis.svc:443")
	if err != nil {
		t.Fatal(err)
	}
	if len(sts.Spec.VolumeClaimTemplates) != 0 || sts.Spec.Template.Spec.NodeSelector[placement.LabelIdentity] != "c" {
		t.Fatal("migration rebuilt or moved the wrong world")
	}
	found := false
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == s.Spec.Storage.ClaimName {
			found = true
		}
	}
	if !found {
		t.Fatal("active PVC not mounted")
	}
	for _, c := range sts.Spec.Template.Spec.InitContainers {
		if c.Name == "egress-gate" {
			if slices.Contains(c.Command, "--fail-open") || !slices.Contains(c.Command, "--positive-probe") {
				t.Fatal("distributed gate can fail open")
			}
		}
	}
}

// findEnv returns the env var with the given name, or nil.
func findEnv(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

// By default readiness is a plain TCP check on the game port (spec §5).
func TestReadinessProbeDefaultsToTCP(t *testing.T) {
	p := readinessProbe(&v1alpha1.MinecraftServer{})
	if p.TCPSocket == nil || p.HTTPGet != nil {
		t.Fatalf("default probe should be TCPSocket, got %+v", p.ProbeHandler)
	}
	if p.TCPSocket.Port.IntVal != GamePort {
		t.Errorf("TCP probe port = %d, want %d", p.TCPSocket.Port.IntVal, GamePort)
	}
}

// When a server declares an HTTP health port, readiness gates on an HTTP GET so
// an RCON-less loader's own "started" signal (felis-limbo) marks it Ready.
func TestReadinessProbeHTTPWhenHealthPortSet(t *testing.T) {
	s := &v1alpha1.MinecraftServer{}
	s.Spec.Startup.HealthHTTPPort = 8080
	p := readinessProbe(s)
	if p.HTTPGet == nil || p.TCPSocket != nil {
		t.Fatalf("expected HTTPGet probe, got %+v", p.ProbeHandler)
	}
	if p.HTTPGet.Port.IntVal != 8080 {
		t.Errorf("HTTP probe port = %d, want 8080", p.HTTPGet.Port.IntVal)
	}
	if p.HTTPGet.Path != "/healthz" {
		t.Errorf("HTTP probe path = %q, want default /healthz", p.HTTPGet.Path)
	}
}

func TestReadinessProbeHTTPCustomPath(t *testing.T) {
	s := &v1alpha1.MinecraftServer{}
	s.Spec.Startup.HealthHTTPPort = 9000
	s.Spec.Startup.HealthHTTPPath = "/ready"
	p := readinessProbe(s)
	if p.HTTPGet == nil || p.HTTPGet.Path != "/ready" || p.HTTPGet.Port.IntVal != 9000 {
		t.Fatalf("custom HTTP probe wrong: %+v", p.HTTPGet)
	}
}

// A user server (no system-role label) gets the forwarding-config initContainer after
// prepare-data, running the felis image and mounting the world volume. A system
// login gate handles its own properties; a build with no felis image name gets no step.
func TestBuildStatefulSetForwardingInitContainer(t *testing.T) {
	user := &v1alpha1.MinecraftServer{}
	user.Spec.Storage.Size = "1Gi"

	sts, err := buildStatefulSet(user, 1, "felis:demo")
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}
	inits := sts.Spec.Template.Spec.InitContainers
	if len(inits) != 3 || inits[0].Name != "prepare-data" || inits[1].Name != "init-forwarding" || inits[2].Name != "egress-gate" {
		t.Fatalf("want [prepare-data init-forwarding egress-gate], got %+v", inits)
	}
	ic := inits[1]
	if ic.Image != "felis:demo" {
		t.Errorf("init image = %q, want felis:demo", ic.Image)
	}
	// It shares the server's uid (pod securityContext) and holds no privilege.
	if sc := ic.SecurityContext; sc == nil || sc.RunAsUser != nil ||
		sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 || !dropsAll(sc) ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("init-forwarding must run unprivileged as the pod uid, got %+v", sc)
	}
	mounted := false
	for _, vm := range ic.VolumeMounts {
		if vm.Name == dataVolumeName && vm.MountPath == dataMountPath {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("init must mount the world volume at %s, got %+v", dataMountPath, ic.VolumeMounts)
	}
	// The whole point of the initContainer is to write the forwarding config, which it
	// cannot do without the secret: a missing Env here makes `init-forwarding` no-op and
	// the server Ready-but-unjoinable — the exact silent failure the feature removes.
	// Same secretKeyRef rule as the main container (optional so a non-modern proxy still
	// schedules), so assert it, not just the image/identity/mount above.
	fwd := findEnv(ic.Env, envForwardingSecret)
	if fwd == nil {
		t.Fatalf("init must carry %s or it writes no forwarding config", envForwardingSecret)
	}
	if fwd.ValueFrom == nil || fwd.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s on init must be a secretKeyRef, got %+v", envForwardingSecret, fwd)
	}
	if ref := fwd.ValueFrom.SecretKeyRef; ref.Name != naming.ForwardingSecretName || ref.Key != naming.ForwardingSecretKey {
		t.Errorf("init %s secretKeyRef = %s/%s, want %s/%s", envForwardingSecret, ref.Name, ref.Key, naming.ForwardingSecretName, naming.ForwardingSecretKey)
	}

	// No felis image name → nothing to run.
	noImg, _ := buildStatefulSet(user, 1, "")
	if len(noImg.Spec.Template.Spec.InitContainers) != 0 {
		t.Error("no felis image must yield no initContainer")
	}

	// The lobby shares the forwarding merge, preserving its custom Paper globals.
	sys := &v1alpha1.MinecraftServer{}
	sys.Spec.Storage.Size = "1Gi"
	sys.Labels = map[string]string{v1alpha1.LabelSystemRole: "lobby"}
	sysSts, _ := buildStatefulSet(sys, 1, "felis:demo")
	if got := sysSts.Spec.Template.Spec.InitContainers; len(got) != 3 || got[0].Name != "prepare-data" || got[1].Name != "init-forwarding" || got[2].Name != "egress-gate" {
		t.Errorf("lobby must get [prepare-data init-forwarding egress-gate], got %+v", got)
	}

	managed := false
	for _, env := range sysSts.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "FELIS_MANAGED_FORWARDING" && env.Value == "true" {
			managed = true
		}
	}
	if !managed {
		t.Fatal("lobby entrypoint would overwrite the merged forwarding config")
	}
}

// Every server's image waits behind the egress gate, the last step before it: the
// pod's NetworkPolicy is programmed after the pod starts. The gate runs the felis
// image with nothing but the dial it needs, and it lets the server start after
// egressGateWait, where the build's gate refuses.
func TestBuildStatefulSetGatesEgress(t *testing.T) {
	for _, role := range []string{"", naming.SystemLoginServer, "lobby"} {
		server := &v1alpha1.MinecraftServer{}
		server.Spec.Storage.Size = "1Gi"
		if role != "" {
			server.Labels = map[string]string{v1alpha1.LabelSystemRole: role}
		}
		sts, err := buildStatefulSet(server, 1, "felis:demo")
		if err != nil {
			t.Fatalf("buildStatefulSet: %v", err)
		}
		inits := sts.Spec.Template.Spec.InitContainers
		gate := inits[len(inits)-1]
		if gate.Name != "egress-gate" || gate.Image != "felis:demo" {
			t.Fatalf("role %q: last init container = %s (%s), want egress-gate on the felis image", role, gate.Name, gate.Image)
		}
		want := []string{felisBinaryPath, "egress-gate", "--wait", "30s", "--fail-open"}
		if !slices.Equal(gate.Command, want) || len(gate.Args) != 0 {
			t.Errorf("role %q: gate runs %v %v, want %v", role, gate.Command, gate.Args, want)
		}
		if sc := gate.SecurityContext; sc == nil || sc.RunAsUser != nil || !dropsAll(sc) || len(sc.Capabilities.Add) != 0 ||
			sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
			sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("role %q: the gate must run unprivileged as the pod uid, got %+v", role, sc)
		}
		if len(gate.VolumeMounts) != 0 || len(gate.Env) != 0 || len(gate.EnvFrom) != 0 {
			t.Errorf("role %q: the gate needs no volume and no secret, got mounts %+v env %+v %+v", role, gate.VolumeMounts, gate.Env, gate.EnvFrom)
		}
		if gate.Resources.Limits.Memory().IsZero() {
			t.Errorf("role %q: the gate must carry a memory limit", role)
		}
	}
}

func dropsAll(sc *corev1.SecurityContext) bool {
	return sc.Capabilities != nil && len(sc.Capabilities.Drop) == 1 && sc.Capabilities.Drop[0] == "ALL"
}

// The server pod runs as the game uid under the runtime's seccomp filter, and the
// game container holds no capability. Only prepare-data runs as root, with exactly
// the two capabilities a chown walk needs — both inside the PodSecurity baseline.
func TestBuildStatefulSetRunsGameAsNonRoot(t *testing.T) {
	s := &v1alpha1.MinecraftServer{}
	s.Spec.Storage.Size = "1Gi"
	sts, err := buildStatefulSet(s, 1, "felis:demo")
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}
	pod := sts.Spec.Template.Spec.SecurityContext
	if pod == nil || pod.RunAsNonRoot == nil || !*pod.RunAsNonRoot ||
		pod.RunAsUser == nil || *pod.RunAsUser != naming.GameUID ||
		pod.RunAsGroup == nil || *pod.RunAsGroup != naming.GameGID ||
		pod.FSGroup == nil || *pod.FSGroup != naming.GameGID ||
		pod.SeccompProfile == nil || pod.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod securityContext = %+v, want non-root %d:%d with fsGroup and RuntimeDefault seccomp",
			pod, naming.GameUID, naming.GameGID)
	}
	game := sts.Spec.Template.Spec.Containers[0].SecurityContext
	if game == nil || game.AllowPrivilegeEscalation == nil || *game.AllowPrivilegeEscalation ||
		!dropsAll(game) || len(game.Capabilities.Add) != 0 || game.RunAsUser != nil {
		t.Errorf("game container securityContext = %+v, want no escalation and drop ALL", game)
	}

	prep := sts.Spec.Template.Spec.InitContainers[0]
	sc := prep.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 0 || sc.RunAsNonRoot == nil || *sc.RunAsNonRoot {
		t.Fatalf("prepare-data must run as root, got %+v", sc)
	}
	if !dropsAll(sc) || len(sc.Capabilities.Add) != 2 ||
		sc.Capabilities.Add[0] != "CHOWN" || sc.Capabilities.Add[1] != "DAC_OVERRIDE" {
		t.Errorf("prepare-data capabilities = %+v, want drop ALL + CHOWN, DAC_OVERRIDE", sc.Capabilities)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("prepare-data must forbid privilege escalation")
	}
	if len(prep.Command) < 2 || prep.Command[1] != "init-volume" {
		t.Errorf("prepare-data command = %v, want felis init-volume", prep.Command)
	}
	if prep.Resources.Limits.Memory().IsZero() {
		t.Error("prepare-data must carry a memory limit")
	}
}

// The system-role label travels onto the pod: the platform's NetworkPolicies select
// on it (felis-login-to-internal-api opens 8081 only to name=login AND
// system-role=login), and a pod without it would be fenced off the one service it
// exists to call. The selector stays the immutable labelsFor subset, so existing
// StatefulSets roll instead of failing to update.
func TestBuildStatefulSetCopiesSystemRoleOntoPods(t *testing.T) {
	login := &v1alpha1.MinecraftServer{}
	login.Name = naming.SystemLoginServer
	login.Labels = map[string]string{v1alpha1.LabelSystemRole: naming.SystemLoginServer}
	sts, err := buildStatefulSet(login, 1, "felis:demo")
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}
	pod := sts.Spec.Template.Labels
	if pod[v1alpha1.LabelSystemRole] != naming.SystemLoginServer {
		t.Errorf("pod labels = %v, want %s=%s", pod, v1alpha1.LabelSystemRole, naming.SystemLoginServer)
	}
	for k, v := range sts.Spec.Selector.MatchLabels {
		if pod[k] != v {
			t.Errorf("selector %s=%s does not match the pod template", k, v)
		}
	}
	if _, ok := sts.Spec.Selector.MatchLabels[v1alpha1.LabelSystemRole]; ok {
		t.Error("the system role must stay out of the (immutable) selector")
	}

	user := &v1alpha1.MinecraftServer{}
	user.Name = "survival"
	userSts, _ := buildStatefulSet(user, 1, "felis:demo")
	if _, ok := userSts.Spec.Template.Labels[v1alpha1.LabelSystemRole]; ok {
		t.Errorf("a user server pod must carry no system role, got %v", userSts.Spec.Template.Labels)
	}
}

// A server with a health port also exposes it as a named container port so the
// kubelet can reach it.
func TestBuildStatefulSetAddsHealthPort(t *testing.T) {
	s := &v1alpha1.MinecraftServer{}
	s.Spec.Storage.Size = "1Gi"
	s.Spec.Startup.HealthHTTPPort = 8080
	sts, err := buildStatefulSet(s, 1, "")
	if err != nil {
		t.Fatalf("buildStatefulSet: %v", err)
	}
	found := false
	for _, port := range sts.Spec.Template.Spec.Containers[0].Ports {
		if port.Name == "health" && port.ContainerPort == 8080 {
			found = true
		}
	}
	if !found {
		t.Error("health container port 8080 not exposed")
	}
}

// The login system server (and ONLY it) receives the login gate's own token,
// sourced from a Secret via secretKeyRef — never a literal — so its felis-limbo
// plugin can authenticate to the felis-api internal face as the limbo caller. It
// must not be the proxy's felis-service-token, which opens every game route.
func TestBuildEnvInjectsServiceTokenForLogin(t *testing.T) {
	s := &v1alpha1.MinecraftServer{}
	s.Name = naming.SystemLoginServer
	s.Labels = map[string]string{v1alpha1.LabelSystemRole: naming.SystemLoginServer}
	tok := findEnv(buildEnv(s), envServiceToken)
	if tok == nil {
		t.Fatalf("%s not injected for the login server", envServiceToken)
	}
	if tok.Value != "" {
		t.Errorf("%s carries a literal value %q — it must be a secretKeyRef", envServiceToken, tok.Value)
	}
	if tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s must be sourced from a secretKeyRef", envServiceToken)
	}
	ref := tok.ValueFrom.SecretKeyRef
	if ref.Name != "felis-limbo-token" || ref.Key != "token" {
		t.Errorf("secretKeyRef = %s/%s, want felis-limbo-token/token", ref.Name, ref.Key)
	}
}

// A user server must NOT receive the service token. This includes a legacy,
// unlabeled server named "login" that predates the reserved-name rule.
func TestBuildEnvWithholdsServiceTokenFromUserServers(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
	}{
		{name: "survival"},
		{name: "creative"},
		{name: naming.SystemLobbyServer, labels: map[string]string{v1alpha1.LabelSystemRole: naming.SystemLobbyServer}},
		{name: naming.SystemLoginServer},
		{name: naming.SystemLoginServer, labels: map[string]string{v1alpha1.LabelSystemRole: naming.SystemLobbyServer}},
	}
	for _, tt := range tests {
		s := &v1alpha1.MinecraftServer{}
		s.Name = tt.name
		s.Labels = tt.labels
		if tok := findEnv(buildEnv(s), envServiceToken); tok != nil {
			t.Errorf("%s labels=%v: service token leaked into a non-system login server", tt.name, tt.labels)
		}
	}
}

// Every backend receives the Velocity modern-forwarding secret — system and user alike.
// This is the opposite rule from the service token, and deliberately so: Velocity's
// forwarding mode is proxy-wide, so a backend without the secret cannot verify the
// signed handshake and rejects every login the proxy sends it. It is also what makes a
// backend's view of a player's UUID trustworthy (Mojang-verified via the signed payload,
// not offline-derived from the username) — the premise the Owner bind rests on.
// Sourced from a Secret, never a literal, and optional so a cluster whose proxy is not
// in modern mode still schedules its pods.
func TestBuildEnvInjectsForwardingSecretIntoEveryBackend(t *testing.T) {
	for _, name := range []string{naming.SystemLoginServer, naming.SystemLobbyServer, "survival"} {
		s := &v1alpha1.MinecraftServer{}
		s.Name = name
		fwd := findEnv(buildEnv(s), envForwardingSecret)
		if fwd == nil {
			t.Errorf("%s: %s not injected — the backend would reject every proxied login", name, envForwardingSecret)
			continue
		}
		if fwd.Value != "" {
			t.Errorf("%s: %s carries a literal value %q — it must be a secretKeyRef", name, envForwardingSecret, fwd.Value)
			continue
		}
		if fwd.ValueFrom == nil || fwd.ValueFrom.SecretKeyRef == nil {
			t.Errorf("%s: %s must be sourced from a secretKeyRef", name, envForwardingSecret)
			continue
		}
		ref := fwd.ValueFrom.SecretKeyRef
		if ref.Name != naming.ForwardingSecretName || ref.Key != naming.ForwardingSecretKey {
			t.Errorf("%s: secretKeyRef = %s/%s, want %s/%s", name, ref.Name, ref.Key, naming.ForwardingSecretName, naming.ForwardingSecretKey)
		}
		if ref.Optional == nil || !*ref.Optional {
			t.Errorf("%s: secretKeyRef must be optional, or a cluster without the Secret wedges every pod in CreateContainerConfigError", name)
		}
	}
}
