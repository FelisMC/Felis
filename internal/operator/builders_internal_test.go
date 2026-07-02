package operator

import (
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
)

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

// A server with a health port also exposes it as a named container port so the
// kubelet can reach it.
func TestBuildStatefulSetAddsHealthPort(t *testing.T) {
	s := &v1alpha1.MinecraftServer{}
	s.Spec.Storage.Size = "1Gi"
	s.Spec.Startup.HealthHTTPPort = 8080
	sts, err := buildStatefulSet(s, 1)
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

// The login system server (and ONLY it) receives the service token, sourced from a
// Secret via secretKeyRef — never a literal — so its felis-limbo plugin can
// authenticate to the felis-api internal face.
func TestBuildEnvInjectsServiceTokenForLogin(t *testing.T) {
	s := &v1alpha1.MinecraftServer{}
	s.Name = naming.SystemLoginServer
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
	if ref.Name != naming.ServiceTokenSecretName || ref.Key != naming.ServiceTokenSecretKey {
		t.Errorf("secretKeyRef = %s/%s, want %s/%s", ref.Name, ref.Key, naming.ServiceTokenSecretName, naming.ServiceTokenSecretKey)
	}
}

// A user server (any non-login name) must NOT receive the service token — the
// reserved-name gate is what stops the internal credential leaking into a player's
// pod. naming.ValidateServerName forbids users from ever claiming "login".
func TestBuildEnvWithholdsServiceTokenFromUserServers(t *testing.T) {
	for _, name := range []string{"survival", "creative", naming.SystemLobbyServer} {
		s := &v1alpha1.MinecraftServer{}
		s.Name = name
		if tok := findEnv(buildEnv(s), envServiceToken); tok != nil {
			t.Errorf("%s: service token leaked into a non-login server", name)
		}
	}
}
