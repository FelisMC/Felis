package operator

import (
	"encoding/json"
	"slices"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/software"
	corev1 "k8s.io/api/core/v1"
)

func softwareServer() *v1alpha1.MinecraftServer {
	s := &v1alpha1.MinecraftServer{}
	s.Name = "survival"
	s.Namespace = "minecraft"
	s.Spec.Image = "registry.felis.svc:5000/mirror/eclipse-temurin:21-jre@sha256:" + "ab"
	s.Spec.Software = &v1alpha1.SoftwareSpec{Type: "fabric", MinecraftVersion: "1.21.4", Build: "0.19.5", Java: 21, Forwarding: "velocity"}
	s.Spec.Rcon = v1alpha1.RconSpec{Enabled: true, SecretRef: v1alpha1.SecretKeyRef{Name: "survival-rcon", Key: "password"}}
	return s
}

func containerNamed(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

// A runtime-image server installs its software after the egress gate (nothing
// is downloaded before the pod's fence stands), writes its forwarding config
// for what it installed, and starts through `felis launch`.
func TestSoftwareServerPodOrder(t *testing.T) {
	sts, err := buildStatefulSetWith(softwareServer(), 1, podOptions{FelisImage: "felis:test", ArtifactCache: "http://felis-artifacts.felis.svc:8090"})
	if err != nil {
		t.Fatal(err)
	}
	pod := sts.Spec.Template.Spec
	var names []string
	for _, c := range pod.InitContainers {
		names = append(names, c.Name)
	}
	if want := []string{"install-self", "prepare-data", "egress-gate", "install-server", "init-forwarding"}; !slices.Equal(names, want) {
		t.Fatalf("init containers = %v, want %v", names, want)
	}
	install := containerNamed(pod.InitContainers, "install-server")
	if install.Image != softwareServer().Spec.Image {
		t.Errorf("install-server image = %s, want the runtime image (a Forge installer needs its Java)", install.Image)
	}
	var spec software.Spec
	if err := json.Unmarshal([]byte(findEnv(install.Env, envSoftware).Value), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Type != software.Fabric || spec.MinecraftVersion != "1.21.4" || spec.Build != "0.19.5" || spec.Java != 21 || spec.Forwarding != "velocity" {
		t.Errorf("FELIS_SOFTWARE = %+v", spec)
	}
	if e := findEnv(install.Env, envArtifacts); e == nil || e.Value != "http://felis-artifacts.felis.svc:8090" {
		t.Errorf("FELIS_ARTIFACT_CACHE = %+v", e)
	}
	main := pod.Containers[0]
	if !slices.Equal(main.Command, []string{"/felis/bin/felis", "launch", "--data", "/data"}) || main.WorkingDir != "/data" {
		t.Errorf("server container command %v in %s, want felis launch in /data", main.Command, main.WorkingDir)
	}
	if findEnv(main.Env, envSoftware) == nil || findEnv(main.Env, "RCON_PASSWORD") == nil {
		t.Errorf("server container env = %v, want the software and RCON", main.Env)
	}
}

// The install step runs the owner's choice of upstream software, Forge
// installers included: it gets the game uid and no capability like the server,
// and neither of the secrets the server holds.
func TestSoftwareInstallStepHoldsNoSecret(t *testing.T) {
	sts, err := buildStatefulSetWith(softwareServer(), 1, podOptions{FelisImage: "felis:test"})
	if err != nil {
		t.Fatal(err)
	}
	install := containerNamed(sts.Spec.Template.Spec.InitContainers, "install-server")
	for _, e := range install.Env {
		if e.ValueFrom != nil || e.Name == "RCON_PASSWORD" || e.Name == envForwardingSecret {
			t.Errorf("install-server env carries %s", e.Name)
		}
	}
	sc := install.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || !slices.Contains(sc.Capabilities.Drop, "ALL") {
		t.Errorf("install-server security context = %+v", sc)
	}
	pod := sts.Spec.Template.Spec
	if pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser == 0 {
		t.Errorf("pod runs as %v, want the game uid", pod.SecurityContext.RunAsUser)
	}
	for _, m := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == felisBinVolume && !m.ReadOnly {
			t.Error("the server can rewrite the felis binary its next start runs")
		}
	}
}

func TestSoftwareServerNeedsTheFelisImage(t *testing.T) {
	if _, err := buildStatefulSetWith(softwareServer(), 1, podOptions{}); err == nil {
		t.Error("a runtime-image server was built with no image to run its install steps")
	}
}
