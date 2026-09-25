package platform

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"felis.lolicon.best/internal/naming"
)

// podSpec returns the single container and the pod template of a Deployment,
// failing if the shape is not the expected single-container pod.
func podSpec(t *testing.T, d *appsv1.Deployment) (corev1.PodSpec, corev1.Container) {
	t.Helper()
	ps := d.Spec.Template.Spec
	if len(ps.Containers) != 1 {
		t.Fatalf("%s: want exactly 1 container, got %d", d.Name, len(ps.Containers))
	}
	return ps, ps.Containers[0]
}

// namedContainer returns the pod template of a Deployment and its container
// called name, failing if there is none.
func namedContainer(t *testing.T, d *appsv1.Deployment, name string) (corev1.PodSpec, corev1.Container) {
	t.Helper()
	ps := d.Spec.Template.Spec
	for _, c := range ps.Containers {
		if c.Name == name {
			return ps, c
		}
	}
	t.Fatalf("%s: no container %q", d.Name, name)
	return ps, corev1.Container{}
}

// rconPeerSelector returns the podSelector of the allow-rcon NetworkPolicy peer,
// compiled into the same labels.Selector K8s evaluates at runtime. This is the
// real gate: a pod reaches server RCON iff its labels Match this selector.
func rconPeerSelector(t *testing.T, p Params) labels.Selector {
	t.Helper()
	rcon := npByName(t, MinecraftNetworkPolicies(p), "felis-allow-rcon-from-control-plane")
	if len(rcon.Spec.Ingress) != 1 || len(rcon.Spec.Ingress[0].From) != 1 {
		t.Fatalf("rcon policy shape changed; want 1 ingress / 1 peer")
	}
	sel, err := metav1.LabelSelectorAsSelector(rcon.Spec.Ingress[0].From[0].PodSelector)
	if err != nil {
		t.Fatalf("compiling rcon podSelector: %v", err)
	}
	return sel
}

// mapSelectorMatches evaluates a Service-style equality selector (a plain label
// map: every entry must be present and equal) against a pod's labels.
func mapSelectorMatches(selector, podLabels map[string]string) bool {
	if len(selector) == 0 {
		return false // an empty Service selector selects nothing useful here
	}
	for k, v := range selector {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}

// TestControlPlaneDeployments_RunAsMatchingSA is the SA↔workload binding: the
// namespaced Roles only mean something if a workload actually runs as each SA.
// The Deployment is also NAMED for its SA (the tree convention), so a rename
// can't silently detach the labels from the identity.
func TestControlPlaneDeployments_RunAsMatchingSA(t *testing.T) {
	p := testParams()
	cases := []struct {
		name string
		dep  *appsv1.Deployment
		sa   string
	}{
		{"api", APIDeployment(p), SAAPI},
		{"operator", OperatorDeployment(p), SAOperator},
	}
	for _, c := range cases {
		ps, _ := podSpec(t, c.dep)
		if ps.ServiceAccountName != c.sa {
			t.Errorf("%s pod serviceAccountName = %q, want %q", c.name, ps.ServiceAccountName, c.sa)
		}
		if c.dep.Name != c.sa {
			t.Errorf("%s Deployment name = %q, want %q (named for its SA)", c.name, c.dep.Name, c.sa)
		}
		if c.dep.Namespace != p.ControlNamespace {
			t.Errorf("%s Deployment namespace = %q, want control ns %q", c.name, c.dep.Namespace, p.ControlNamespace)
		}
		// Selector, template labels, and object labels must agree (a mismatch
		// orphans the pods).
		if !labels.Equals(c.dep.Spec.Selector.MatchLabels, c.dep.Spec.Template.Labels) {
			t.Errorf("%s selector %v != template labels %v", c.name, c.dep.Spec.Selector.MatchLabels, c.dep.Spec.Template.Labels)
		}
	}
}

// TestControlPlanePods_SatisfyRConPeer is the second correspondence: the api and
// operator pods (which legitimately open RCON — console writes, readiness probes)
// carry labels that SATISFY the allow-rcon peer, while the registry and the reaper
// do NOT. Evaluated with the live selector, so it proves the labels and the policy
// agree rather than re-asserting the policy's shape.
func TestControlPlanePods_SatisfyRConPeer(t *testing.T) {
	p := testParams()
	sel := rconPeerSelector(t, p)

	apiPod := APIDeployment(p).Spec.Template.Labels
	opPod := OperatorDeployment(p).Spec.Template.Labels
	regPod := registryDeployment(p).Spec.Template.Labels
	reaperPod := controlPlanePodLabels(ComponentReaper)

	if !sel.Matches(labels.Set(apiPod)) {
		t.Errorf("api pod labels %v must satisfy the rcon peer", apiPod)
	}
	if !sel.Matches(labels.Set(opPod)) {
		t.Errorf("operator pod labels %v must satisfy the rcon peer", opPod)
	}
	if sel.Matches(labels.Set(regPod)) {
		t.Errorf("registry pod labels %v must NOT satisfy the rcon peer (no part-of=control-plane)", regPod)
	}
	if sel.Matches(labels.Set(reaperPod)) {
		t.Errorf("reaper pod labels %v must NOT satisfy the rcon peer (component not in {api,operator})", reaperPod)
	}
}

// TestControlPlanePods_Hardened asserts the pod/container SecurityContext on every
// workload here mirrors the build/restore Job hardening: non-root, no privilege,
// no escalation, read-only root fs, all caps dropped. (Shape-asserted: no cluster
// proves the images actually start under these constraints.)
func TestControlPlanePods_Hardened(t *testing.T) {
	p := testParams()
	for _, d := range []*appsv1.Deployment{APIDeployment(p), OperatorDeployment(p), registryDeployment(p)} {
		ps := d.Spec.Template.Spec

		if ps.SecurityContext == nil || ps.SecurityContext.RunAsNonRoot == nil || !*ps.SecurityContext.RunAsNonRoot {
			t.Errorf("%s: pod must set runAsNonRoot=true", d.Name)
		}
		if ps.SecurityContext == nil || ps.SecurityContext.RunAsUser == nil || *ps.SecurityContext.RunAsUser != nonRootUID {
			t.Errorf("%s: pod runAsUser must be %d", d.Name, nonRootUID)
		}
		for _, c := range ps.Containers {
			sc := c.SecurityContext
			if sc == nil {
				t.Fatalf("%s/%s: container has no SecurityContext", d.Name, c.Name)
			}
			if sc.Privileged == nil || *sc.Privileged {
				t.Errorf("%s/%s: container must not be privileged", d.Name, c.Name)
			}
			if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Errorf("%s/%s: container must set allowPrivilegeEscalation=false", d.Name, c.Name)
			}
			if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
				t.Errorf("%s/%s: container must set readOnlyRootFilesystem=true", d.Name, c.Name)
			}
			if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
				t.Errorf("%s/%s: container must drop ALL capabilities", d.Name, c.Name)
			}
		}
	}
}

// TestVolumeBinderPod pins the pod `felis offsite fetch-worlds` runs to get the
// archive volume provisioned: hardened like the control plane (it runs in the
// Minecraft namespace under PSA), mounting the named claim, gone once it exits.
func TestVolumeBinderPod(t *testing.T) {
	pod := VolumeBinderPod("minecraft", "felis-backups", "registry.example/felis:1")
	ps := pod.Spec
	if pod.Namespace != "minecraft" || pod.GenerateName == "" || ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("pod meta = %+v, restart %s", pod.ObjectMeta, ps.RestartPolicy)
	}
	if ps.SecurityContext == nil || ps.SecurityContext.RunAsNonRoot == nil || !*ps.SecurityContext.RunAsNonRoot ||
		ps.SecurityContext.SeccompProfile == nil || ps.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v", ps.SecurityContext)
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Error("the binder needs no API token")
	}
	c := ps.Containers[0]
	if got := append(append([]string{}, c.Command...), c.Args...); !containsSeq(got, []string{felisBinaryPath, "version"}) {
		t.Errorf("command = %v", got)
	}
	if sc := c.SecurityContext; sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container security context = %+v", c.SecurityContext)
	}
	if len(ps.Volumes) != 1 || ps.Volumes[0].PersistentVolumeClaim == nil || ps.Volumes[0].PersistentVolumeClaim.ClaimName != "felis-backups" {
		t.Errorf("volumes = %+v", ps.Volumes)
	}
}

// TestAPIDeployment_Wiring pins the api entrypoint, the credential plumbing, and
// the FELIS_IMAGE passthrough.
func TestAPIDeployment_Wiring(t *testing.T) {
	p := testParams()
	d := APIDeployment(p)
	ps, c := podSpec(t, d)

	if got := append(append([]string{}, c.Command...), c.Args...); !containsSeq(got, []string{felisBinaryPath, "api"}) {
		t.Errorf("api command/args = %v, want it to start `%s api`", got, felisBinaryPath)
	}
	if !contains(c.Args, "--config") || !contains(c.Args, configFilePath) {
		t.Errorf("api args must mount config at %s, got %v", configFilePath, c.Args)
	}
	if !contains(c.Args, "--internal-addr") {
		t.Errorf("api args must set --internal-addr, got %v", c.Args)
	}
	if !contains(c.Args, "--https-addr") || !contains(c.Args, ":8443") {
		t.Errorf("api args must set HTTPS listener, got %v", c.Args)
	}
	if !contains(c.Args, "--tls-cert") || !contains(c.Args, apiTLSMountPath+"/tls.crt") ||
		!contains(c.Args, "--tls-key") || !contains(c.Args, apiTLSMountPath+"/tls.key") {
		t.Errorf("api args must point at mounted TLS secret, got %v", c.Args)
	}
	if c.Image != p.FelisImage {
		t.Errorf("api image = %q, want FelisImage %q", c.Image, p.FelisImage)
	}

	// FELIS_IMAGE passthrough (used to launch the restore Job with the same image).
	if v := envValue(c.Env, "FELIS_IMAGE"); v != p.FelisImage {
		t.Errorf("FELIS_IMAGE = %q, want %q", v, p.FelisImage)
	}
	// The internal-face base URL the api derives submission context-fetch URLs
	// from — the same address the login gate is handed.
	if v := envValue(c.Env, "FELIS_API_BASE_URL"); v != InternalAPIBaseURL(p.ControlNamespace) {
		t.Errorf("FELIS_API_BASE_URL = %q, want %q", v, InternalAPIBaseURL(p.ControlNamespace))
	}
	// Each internal caller's token comes from its own Secret, never a literal
	// value, and none is optional: a missing Secret must hold the rollout back.
	for env, secret := range map[string]string{
		"FELIS_SERVICE_TOKEN": "felis-service-token",
		"FELIS_LIMBO_TOKEN":   "felis-limbo-token",
		"FELIS_BUILD_TOKEN":   "felis-build-token",
		"FELIS_OPS_TOKEN":     "felis-ops-token",
	} {
		tok := envVar(c.Env, env)
		if tok == nil || tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s must be sourced from a secretKeyRef", env)
		}
		ref := tok.ValueFrom.SecretKeyRef
		if ref.Name != secret || ref.Key != "token" {
			t.Errorf("%s reads %s/%s, want %s/token", env, ref.Name, ref.Key, secret)
		}
		if ref.Optional != nil && *ref.Optional {
			t.Errorf("%s is optional; the api must not start without it", env)
		}
		if tok.Value != "" {
			t.Errorf("%s must not carry a literal value", env)
		}
	}

	// felis.toml carries the DB URL, so its volume must be a Secret (NOT a
	// ConfigMap), mounted read-only.
	cfgVol := volumeByName(ps.Volumes, configVolume)
	if cfgVol == nil || cfgVol.Secret == nil {
		t.Fatal("config volume must be sourced from a Secret")
	}
	if cfgVol.ConfigMap != nil {
		t.Error("config volume must NOT be a ConfigMap (felis.toml holds the DB credential)")
	}
	if cfgVol.Secret.SecretName != configSecretName {
		t.Errorf("config Secret name = %q, want %q", cfgVol.Secret.SecretName, configSecretName)
	}
	if m := mountByName(c.VolumeMounts, configVolume); m == nil || !m.ReadOnly {
		t.Error("config volume must be mounted read-only")
	}
	tlsVol := volumeByName(ps.Volumes, "tls")
	if tlsVol == nil || tlsVol.Secret == nil || tlsVol.Secret.SecretName != apiTLSSecretName {
		t.Fatalf("tls volume must mount Secret %q, got %#v", apiTLSSecretName, tlsVol)
	}
	if m := mountByName(c.VolumeMounts, "tls"); m == nil || !m.ReadOnly || m.MountPath != apiTLSMountPath {
		t.Errorf("tls volume mount = %#v, want read-only at %s", m, apiTLSMountPath)
	}

	// No backup PVC in testParams ⇒ no FELIS_BACKUP_PVC env (restore degrades to 503).
	if envVar(c.Env, "FELIS_BACKUP_PVC") != nil {
		t.Error("FELIS_BACKUP_PVC must be absent when no backup PVC is configured")
	}
}

// TestAPIDeployment_UploadsStorage pins both storage backends' wiring: the local
// uploads PVC mounted read-write, and the two S3 credential env vars sourced
// optionally from the felis-uploads-s3 Secret (so a local install still starts).
func TestAPIDeployment_UploadsStorage(t *testing.T) {
	ps, c := podSpec(t, APIDeployment(testParams()))

	// Local backend: uploads PVC mounted read-WRITE at UploadsLocalPath.
	vol := volumeByName(ps.Volumes, uploadsVolume)
	if vol == nil || vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != uploadsPVCName {
		t.Fatalf("uploads volume must mount PVC %q, got %#v", uploadsPVCName, vol)
	}
	if m := mountByName(c.VolumeMounts, uploadsVolume); m == nil || m.MountPath != UploadsLocalPath || m.ReadOnly {
		t.Errorf("uploads mount = %#v, want read-write at %s", m, UploadsLocalPath)
	}

	// Credential env vars (S3 backend + SMTP relay) come from their Secrets (never
	// literals) and are OPTIONAL, so an install without them still starts.
	for _, ev := range []struct{ name, secret, key string }{
		{UploadsS3AccessKeyEnv, UploadsS3SecretName, UploadsS3SecretAccessKey},
		{UploadsS3SecretKeyEnv, UploadsS3SecretName, UploadsS3SecretSecretKey},
		{SMTPPasswordEnv, SMTPSecretName, SMTPSecretPasswordKey},
		{RegistryPruneTokenEnv, naming.RegistryAuthSecretName, "prune"},
	} {
		e := envVar(c.Env, ev.name)
		if e == nil || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s must be sourced from a secretKeyRef", ev.name)
		}
		ref := e.ValueFrom.SecretKeyRef
		if ref.Name != ev.secret || ref.Key != ev.key {
			t.Errorf("%s ref = %s/%s, want %s/%s", ev.name, ref.Name, ref.Key, ev.secret, ev.key)
		}
		if ref.Optional == nil || !*ref.Optional {
			t.Errorf("%s secretKeyRef must be optional (an install without it has no such Secret)", ev.name)
		}
		if e.Value != "" {
			t.Errorf("%s must not carry a literal value", ev.name)
		}
	}
}

func TestAPIService_NodePort(t *testing.T) {
	p := testParams()
	p.PanelNodePort = 30445
	svc := apiService(p)
	dep := APIDeployment(p)

	if svc.Name != SAAPI || svc.Namespace != p.ControlNamespace {
		t.Errorf("api Service = %s/%s, want %s/%s", svc.Namespace, svc.Name, p.ControlNamespace, SAAPI)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("api Service type = %s, want NodePort", svc.Spec.Type)
	}
	if !mapSelectorMatches(svc.Spec.Selector, dep.Spec.Template.Labels) {
		t.Errorf("api Service selector %v does not select api pod labels %v", svc.Spec.Selector, dep.Spec.Template.Labels)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("api Service ports = %v, want one", svc.Spec.Ports)
	}
	port := svc.Spec.Ports[0]
	if port.Port != 443 || port.TargetPort.StrVal != "https" || port.NodePort != p.PanelNodePort {
		t.Errorf("api Service port = %#v, want 443 -> https NodePort %d", port, p.PanelNodePort)
	}
}

// TestAPIInternalService_ClusterIP pins the separate internal-face Service: it must
// be ClusterIP (never NodePort — the internal face is service-token-only and must not
// be published on a node's external IP), expose 8081 -> the api pod's "internal"
// port, carry NO nodePort, and select the same api pods as the external Service. It
// is what makes the felis-api DNS name actually answer on 8081 (the login pod path)
// and gives the on-node console a ClusterIP to dial.
func TestAPIInternalService_ClusterIP(t *testing.T) {
	p := testParams()
	svc := apiInternalService(p)
	dep := APIDeployment(p)

	if svc.Name != APIInternalServiceName || svc.Namespace != p.ControlNamespace {
		t.Errorf("internal Service = %s/%s, want %s/%s", svc.Namespace, svc.Name, p.ControlNamespace, APIInternalServiceName)
	}
	if svc.Name == SAAPI {
		t.Errorf("internal Service must not collide with the external Service name %q", SAAPI)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("internal Service type = %s, want ClusterIP (never expose the no-Zero-Trust face on a node)", svc.Spec.Type)
	}
	if !mapSelectorMatches(svc.Spec.Selector, dep.Spec.Template.Labels) {
		t.Errorf("internal Service selector %v does not select api pod labels %v", svc.Spec.Selector, dep.Spec.Template.Labels)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("internal Service ports = %v, want one", svc.Spec.Ports)
	}
	port := svc.Spec.Ports[0]
	if port.Port != apiInternalPort || port.TargetPort.StrVal != "internal" || port.NodePort != 0 {
		t.Errorf("internal Service port = %#v, want %d -> internal with no nodePort", port, apiInternalPort)
	}
}

// TestAPIDeployment_BackupPVC proves the FELIS_BACKUP_PVC env appears only when a
// backup PVC is named.
func TestAPIDeployment_BackupPVC(t *testing.T) {
	p := testParams()
	p.BackupPVC = "felis-backups"
	_, c := podSpec(t, APIDeployment(p))
	if v := envValue(c.Env, "FELIS_BACKUP_PVC"); v != "felis-backups" {
		t.Errorf("FELIS_BACKUP_PVC = %q, want %q", v, "felis-backups")
	}
}

// TestBackupPVC_RendersWithTheStore proves the world-archive PVC is part of the
// bundle exactly when a backup PVC is named, and that it lands in the Minecraft
// namespace where every pod mounting it runs (the backup/restore Jobs and the
// reaper CronJob) — the one property that made the reaper's first placement
// unschedulable. Without a name the bundle must NOT create one: the api env is
// gated on the same value, so the endpoints answer 503 instead of pointing a Job
// at a claim nobody provisioned.
func TestBackupPVC_RendersWithTheStore(t *testing.T) {
	p := testParams()
	p.BackupPVC = "felis-backups"
	var got *corev1.PersistentVolumeClaim
	for _, o := range Workloads(p) {
		if pvc, ok := o.(*corev1.PersistentVolumeClaim); ok && pvc.Name == "felis-backups" {
			got = pvc
		}
	}
	if got == nil {
		t.Fatalf("Workloads() must render PVC %q when BackupPVC is set", p.BackupPVC)
	}
	if got.Namespace != p.MinecraftNamespace {
		t.Errorf("backup PVC namespace = %q, want %q (Pod↔PVC mounts are same-namespace only)", got.Namespace, p.MinecraftNamespace)
	}
	if len(got.Spec.AccessModes) != 1 || got.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("backup PVC access modes = %v, want [ReadWriteOnce]", got.Spec.AccessModes)
	}
	if q := got.Spec.Resources.Requests.Storage(); q == nil || q.String() != backupStorageSize {
		t.Errorf("backup PVC storage = %v, want %s", q, backupStorageSize)
	}
	if got.Spec.StorageClassName != nil {
		t.Errorf("backup PVC pins storageClassName %q; the cluster default is the only safe binding", *got.Spec.StorageClassName)
	}

	for _, o := range Workloads(testParams()) {
		if pvc, ok := o.(*corev1.PersistentVolumeClaim); ok && pvc.Name == "felis-backups" {
			t.Error("no backup PVC may render when none is named")
		}
	}
}

// TestOperatorDeployment_Wiring pins the operator entrypoint, its namespace split,
// and its deliberately smaller surface (NO config Secret — it holds no DB URL).
func TestOperatorDeployment_Wiring(t *testing.T) {
	p := testParams()
	d := OperatorDeployment(p)
	ps, c := podSpec(t, d)

	if got := append(append([]string{}, c.Command...), c.Args...); !containsSeq(got, []string{felisBinaryPath, "operator"}) {
		t.Errorf("operator command/args = %v, want it to start `%s operator`", got, felisBinaryPath)
	}
	if !contains(c.Args, "--namespace") || !contains(c.Args, p.MinecraftNamespace) {
		t.Errorf("operator must watch --namespace %s, got %v", p.MinecraftNamespace, c.Args)
	}
	if !contains(c.Args, "--health-probe-bind-address") || !contains(c.Args, fmt.Sprintf(":%d", operatorHealthPort)) {
		t.Errorf("operator must bind the health probe listener on :%d, got %v", operatorHealthPort, c.Args)
	}
	if c.Image != p.FelisImage {
		t.Errorf("operator image = %q, want FelisImage %q", c.Image, p.FelisImage)
	}
	// No config Secret volume: the operator reads config from flags + the API only.
	for _, v := range ps.Volumes {
		if v.Secret != nil {
			t.Errorf("operator must mount NO Secret volume, found %q", v.Name)
		}
	}
	// It carries exactly one plain env — FELIS_IMAGE, for the forwarding-config
	// initContainer it injects into user servers — and NO credential env: nothing
	// sourced from a Secret (valueFrom), since it holds no DB URL or token.
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			t.Errorf("operator must carry no credential env, found %q with valueFrom", e.Name)
		}
	}
	if len(c.Env) != 1 || c.Env[0].Name != "FELIS_IMAGE" || c.Env[0].Value != p.FelisImage {
		t.Errorf("operator env = %v, want exactly FELIS_IMAGE=%q", c.Env, p.FelisImage)
	}
}

// TestRegistry_DeploymentServicePVC pins the in-cluster registry: its pinned
// listen port, token-mount hygiene, the Service that gives it its DNS name, and
// the backing PVC — the trio the build egress policy targets.
func TestRegistry_DeploymentServicePVC(t *testing.T) {
	// testParams leaves RegistryNamespace/RegistryPort zero; the renderers fill them
	// via withDefaults, so compare against the defaulted Params.
	p := testParams().withDefaults()
	dep := registryDeployment(p)
	svc := registryService(p)
	pvc := registryPVC(p)

	ps, c := namedContainer(t, dep, registryName)
	if len(ps.Containers) != 3 {
		t.Fatalf("registry pod containers = %d, want registry + gate + gc", len(ps.Containers))
	}
	if c.Image != defaultRegistryImage {
		t.Errorf("registry image = %q, want default %q", c.Image, defaultRegistryImage)
	}
	// registry:2 itself listens on loopback only and exposes nothing: every request
	// from outside the pod passes the gate, which is what makes writes authorized.
	if v := envValue(c.Env, "REGISTRY_HTTP_ADDR"); v != fmt.Sprintf("127.0.0.1:%d", p.RegistryPort+1) {
		t.Errorf("REGISTRY_HTTP_ADDR = %q, want loopback 127.0.0.1:%d", v, p.RegistryPort+1)
	}
	if len(c.Ports) != 0 {
		t.Errorf("registry container ports = %+v, want none (the gate owns the port)", c.Ports)
	}
	// Deletion on, and the blob descriptor cache off: a cached descriptor for a
	// blob the GC removed would let the next push skip uploading it.
	if v := envValue(c.Env, "REGISTRY_STORAGE_DELETE_ENABLED"); v != "true" {
		t.Errorf("REGISTRY_STORAGE_DELETE_ENABLED = %q, want true", v)
	}
	if v := envValue(c.Env, "REGISTRY_STORAGE_CACHE_BLOBDESCRIPTOR"); v == "inmemory" || v == "redis" || v == "" {
		t.Errorf("REGISTRY_STORAGE_CACHE_BLOBDESCRIPTOR = %q, want the cache disabled", v)
	}
	// The registry's limits are deliberately NOT the control-plane template's: audit
	// #46 caught the registry OOM-killed mid-upload at 256Mi on a real 475MB-layer push.
	if mem := c.Resources.Limits[corev1.ResourceMemory]; mem.Value() < 2*1024*1024*1024 {
		t.Errorf("registry memory limit = %s, want >= 2Gi (audit #46: 256Mi OOM-killed on a 475MB-layer push)", mem.String())
	}

	// The gate: the platform image, forwarding to the loopback registry, reading
	// the per-principal tokens from the optional Secret.
	_, gate := namedContainer(t, dep, registryGateName)
	if gate.Image != p.FelisImage {
		t.Errorf("gate image = %q, want the platform image %q", gate.Image, p.FelisImage)
	}
	if len(gate.Command) != 2 || gate.Command[1] != "registry-gate" {
		t.Errorf("gate command = %v, want felis registry-gate", gate.Command)
	}
	for _, want := range []string{
		fmt.Sprintf("--listen=:%d", p.RegistryPort),
		fmt.Sprintf("--upstream=http://127.0.0.1:%d", p.RegistryPort+1),
		"--auth-dir=" + registryAuthMountPath,
		// The GC handshake has no authentication: loopback only.
		fmt.Sprintf("--maint-listen=127.0.0.1:%d", p.RegistryPort+2),
		"--maint-dir=" + registryMaintMountPath,
		"--data-dir=" + registryDataPath,
	} {
		if !contains(gate.Args, want) {
			t.Errorf("gate args = %v, want %s", gate.Args, want)
		}
	}

	// The GC sidecar: the registry image on the same data volume, hardened like
	// the rest, pointed at the gate's maintenance port, never --delete-untagged
	// (digest-pinned servers may boot an untagged manifest).
	_, gc := namedContainer(t, dep, registryGCName)
	if gc.Image != p.RegistryImage {
		t.Errorf("gc image = %q, want the registry image %q", gc.Image, p.RegistryImage)
	}
	if v := envValue(gc.Env, "FELIS_GC_MAINT_PORT"); v != fmt.Sprint(p.RegistryPort+2) {
		t.Errorf("FELIS_GC_MAINT_PORT = %q, want %d", v, p.RegistryPort+2)
	}
	script := strings.Join(gc.Command, " ")
	if !strings.Contains(script, "garbage-collect") || strings.Contains(script, "delete-untagged") {
		t.Errorf("gc command must run garbage-collect without --delete-untagged: %s", script)
	}
	if !strings.Contains(script, "/readonly?lease=") || !strings.Contains(script, "/readwrite") {
		t.Errorf("gc command must take and hand back the gate's read-only window: %s", script)
	}
	gcData := false
	for _, m := range gc.VolumeMounts {
		if m.Name == registryAuthVolume {
			t.Error("the gc sidecar must not mount the write tokens")
		}
		if m.Name == registryVolume && m.MountPath == registryDataPath {
			gcData = true
		}
	}
	if !gcData {
		t.Errorf("gc sidecar must mount the registry data at %s, mounts=%v", registryDataPath, gc.VolumeMounts)
	}
	if gc.SecurityContext == nil || gc.SecurityContext.ReadOnlyRootFilesystem == nil || !*gc.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("gc sidecar must run with a read-only root filesystem")
	}
	// The node-side pull path: exactly one container port, mirrored by a LOOPBACK
	// hostPort. Node containerd cannot dial the Service VIP, so its registries.yaml
	// mirror rewrites the Service name onto 127.0.0.1:<port>; nothing else may be
	// exposed (the registry serves plain HTTP).
	if len(gate.Ports) != 1 {
		t.Fatalf("gate ports = %+v, want exactly 1", gate.Ports)
	}
	if p0 := gate.Ports[0]; p0.ContainerPort != p.RegistryPort || p0.HostPort != p.RegistryPort || p0.HostIP != registryLoopbackHost {
		t.Errorf("gate port = %+v, want container/host port %d bound to %s", p0, p.RegistryPort, registryLoopbackHost)
	}
	auth := volumeByName(ps.Volumes, registryAuthVolume)
	if auth == nil || auth.Secret == nil || auth.Secret.SecretName != "felis-registry-auth" {
		t.Fatalf("gate token volume = %#v, want Secret felis-registry-auth", auth)
	}
	// Optional: a missing Secret must degrade to "reads only", never to a registry
	// pod stuck in ContainerCreating that every game pull depends on.
	if auth.Secret.Optional == nil || !*auth.Secret.Optional {
		t.Error("the registry-auth Secret volume must be optional")
	}
	mounted := false
	for _, m := range gate.VolumeMounts {
		if m.Name == registryAuthVolume && m.ReadOnly {
			mounted = true
		}
	}
	for _, m := range c.VolumeMounts {
		if m.Name == registryAuthVolume {
			t.Error("registry:2 must not mount the write tokens")
		}
	}
	// The gate reads the manifest index off the data volume, and never writes it.
	for _, m := range gate.VolumeMounts {
		if m.Name == registryVolume && !m.ReadOnly {
			t.Error("the gate must mount the registry data read-only")
		}
	}
	if !mounted {
		t.Errorf("gate must mount the tokens read-only, mounts=%v", gate.VolumeMounts)
	}
	// Registry never calls the K8s API ⇒ no auto-mounted token.
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Error("registry pod must set automountServiceAccountToken=false")
	}
	// Data is on the PVC named "registry".
	dataVol := volumeByName(ps.Volumes, registryVolume)
	if dataVol == nil || dataVol.PersistentVolumeClaim == nil || dataVol.PersistentVolumeClaim.ClaimName != registryName {
		t.Errorf("registry data volume must be PVC %q", registryName)
	}

	// Service: gives the pinned DNS name registry.<ns>.svc:5000.
	if svc.Name != registryName || svc.Namespace != p.RegistryNamespace {
		t.Errorf("registry Service = %s/%s, want %s/%s", svc.Namespace, svc.Name, p.RegistryNamespace, registryName)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != p.RegistryPort {
		t.Errorf("registry Service port = %v, want %d", svc.Spec.Ports, p.RegistryPort)
	}
	// The Service selector must select the registry pods...
	if !mapSelectorMatches(svc.Spec.Selector, dep.Spec.Template.Labels) {
		t.Errorf("registry Service selector %v does not select registry pod labels %v", svc.Spec.Selector, dep.Spec.Template.Labels)
	}
	// ...and must NOT select the api pods (distinct component).
	if mapSelectorMatches(svc.Spec.Selector, APIDeployment(p).Spec.Template.Labels) {
		t.Error("registry Service selector must not select the api pod")
	}

	// PVC: RWO with a concrete request, no pinned storage class.
	if pvc.Name != registryName || pvc.Namespace != p.RegistryNamespace {
		t.Errorf("registry PVC = %s/%s, want %s/%s", pvc.Namespace, pvc.Name, p.RegistryNamespace, registryName)
	}
	if !contains(accessModeStrings(pvc.Spec.AccessModes), string(corev1.ReadWriteOnce)) {
		t.Errorf("registry PVC access modes = %v, want ReadWriteOnce", pvc.Spec.AccessModes)
	}
	if pvc.Spec.Resources.Requests.Storage().IsZero() {
		t.Error("registry PVC must request a non-zero storage size")
	}
}

// TestWorkloads_DeploymentsCarryProbes pins probes on ALL three rendered
// Deployments (api, operator, registry): an unprobed control plane cannot be
// told apart from a wedged one, and every probe must target a port the container
// actually declares — a probe pointed at a dead port would leave the pod
// NotReady forever and surface only as a mysteriously empty Service.
func TestWorkloads_DeploymentsCarryProbes(t *testing.T) {
	p := testParams().withDefaults()
	cases := []struct {
		dep                 *appsv1.Deployment
		readyPath, livePath string
		port                int32
	}{
		{APIDeployment(p), "/readyz", "/healthz", apiInternalPort},
		{OperatorDeployment(p), "/readyz", "/healthz", operatorHealthPort},
		{registryDeployment(p), "/healthz", "/livez", p.RegistryPort},
	}
	// resolve maps a probe target (by number or container-port name) to the
	// declared container port it denotes.
	resolve := func(port intstr.IntOrString, ports []corev1.ContainerPort) (int32, bool) {
		if port.IntValue() != 0 {
			return int32(port.IntValue()), true
		}
		for _, cp := range ports {
			if cp.Name == port.StrVal {
				return cp.ContainerPort, true
			}
		}
		return 0, false
	}
	for _, tc := range cases {
		var c corev1.Container
		if tc.dep.Name == registryName {
			// The gate owns the registry port; registry:2 behind it is probed
			// through the gate's /healthz.
			_, c = namedContainer(t, tc.dep, registryGateName)
		} else {
			_, c = podSpec(t, tc.dep)
		}
		if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil {
			t.Fatalf("%s: readiness probe missing or not an HTTP GET", tc.dep.Name)
		}
		if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil {
			t.Fatalf("%s: liveness probe missing or not an HTTP GET", tc.dep.Name)
		}
		for _, probe := range []struct {
			kind string
			p    *corev1.Probe
			path string
		}{
			{"readiness", c.ReadinessProbe, tc.readyPath},
			{"liveness", c.LivenessProbe, tc.livePath},
		} {
			if got := probe.p.HTTPGet.Path; got != probe.path {
				t.Errorf("%s: %s probe path = %q, want %q", tc.dep.Name, probe.kind, got, probe.path)
			}
			got, ok := resolve(probe.p.HTTPGet.Port, c.Ports)
			if !ok {
				t.Errorf("%s: %s probe targets %v, which the container does not declare", tc.dep.Name, probe.kind, probe.p.HTTPGet.Port)
			} else if got != tc.port {
				t.Errorf("%s: %s probe port = %d, want %d", tc.dep.Name, probe.kind, got, tc.port)
			}
		}
	}
}

// TestWorkloads_BundleContents sanity-checks the slice Workloads returns: the two
// control-plane Deployments, the api external+internal Services, the operator
// metrics Service, and the registry
// Deployment/Service/PVC, every one with TypeMeta (so its YAML header renders). The
// internal Service must be present or the login pod's felis-api:8081 path is dead.
func TestWorkloads_BundleContents(t *testing.T) {
	objs := Workloads(testParams())
	if len(objs) != 9 {
		t.Fatalf("Workloads returned %d objects, want 9", len(objs))
	}
	var haveInternalSvc bool
	for _, o := range objs {
		gvk := o.GetObjectKind().GroupVersionKind()
		if gvk.Kind == "" || gvk.Version == "" {
			t.Errorf("%T missing TypeMeta (kind=%q version=%q)", o, gvk.Kind, gvk.Version)
		}
		if svc, ok := o.(*corev1.Service); ok && svc.Name == APIInternalServiceName {
			haveInternalSvc = true
		}
	}
	if !haveInternalSvc {
		t.Errorf("Workloads bundle is missing the internal-face Service %q", APIInternalServiceName)
	}
}

// TestControlPlanePriorityClass pins the node-pressure eviction shield: every
// control-plane pod template (api/operator/reaper/registry) runs under the
// BUILT-IN system-cluster-critical class (value 2e9), at which kubelet's
// eviction manager refuses to evict the pod. A live drill showed the whole
// cascade with plain ordering: disk pressure evicted the game pods and then the
// control plane, whose images (air-gapped, containerd-only) were GC'd →
// ImagePullBackOff plus a manual re-import. User-defined classes are capped at
// 1e9 (API-enforced), so the built-in class is the only way to reach the
// critical threshold.
func TestControlPlanePriorityClass(t *testing.T) {
	objs := Workloads(reaperParams()) // include the reaper CronJob's pod template

	deployments, cronJobs := 0, 0
	for _, o := range objs {
		switch v := o.(type) {
		case *appsv1.Deployment:
			deployments++
			if v.Spec.Template.Spec.PriorityClassName != controlPlanePriorityName {
				t.Errorf("deployment %s pod priority = %q, want %q", v.Name, v.Spec.Template.Spec.PriorityClassName, controlPlanePriorityName)
			}
		case *batchv1.CronJob:
			cronJobs++
			if v.Spec.JobTemplate.Spec.Template.Spec.PriorityClassName != controlPlanePriorityName {
				t.Errorf("CronJob %s pod priority = %q, want %q", v.Name, v.Spec.JobTemplate.Spec.Template.Spec.PriorityClassName, controlPlanePriorityName)
			}
		}
	}
	if deployments != 3 || cronJobs != 1 {
		t.Errorf("scanned %d deployments / %d cronjobs, want 3 / 1 — a pod template escaped the class check", deployments, cronJobs)
	}
	// The name must be the built-in critical class: any custom class is capped at
	// 1e9 by the API server and would be evictable.
	if controlPlanePriorityName != "system-cluster-critical" {
		t.Errorf("control-plane priority class = %q; only the built-in critical classes reach the 2e9 eviction-refusal threshold", controlPlanePriorityName)
	}
}

// cronPodSpec returns the single container and pod template of a CronJob's Job
// template, failing if the shape is not a single-container pod (the reaper's shape).
func cronPodSpec(t *testing.T, cj *batchv1.CronJob) (corev1.PodSpec, corev1.Container) {
	t.Helper()
	ps := cj.Spec.JobTemplate.Spec.Template.Spec
	if len(ps.Containers) != 1 {
		t.Fatalf("%s: want exactly 1 container, got %d", cj.Name, len(ps.Containers))
	}
	return ps, ps.Containers[0]
}

// findCronJob returns the first CronJob in objs, or nil — used to assert the
// reaper's presence/absence in the rendered Workloads slice.
func findCronJob(objs []Object) *batchv1.CronJob {
	for _, o := range objs {
		if cj, ok := o.(*batchv1.CronJob); ok {
			return cj
		}
	}
	return nil
}

// reaperParams is testParams with the retention storage trio supplied, so the
// reaper CronJob renders. The paths are illustrative (no cluster runs here).
func reaperParams() Params {
	p := testParams()
	p.WorldsHostPath = "/var/lib/felis/worlds"
	p.BackupPVC = "felis-backups"
	p.ArchiveLocalPath = "/backups"
	return p
}

// TestReaperCronJob_Gating proves the reaper reaps iff all three storage
// coordinates are present, and that the archive store alone (backup PVC + its
// mount path) still renders the CronJob in its retention-only shape, with no
// worlds-root pair: backups must expire on an install that never reaps worlds.
// Anything less renders none (the partial-flag mistake is rejected at the CLI;
// here the renderer fails safe).
func TestReaperCronJob_Gating(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(p *Params)
		reap, cron bool
	}{
		{"none", func(p *Params) {}, false, false},
		{"worlds only", func(p *Params) { p.WorldsHostPath = "/w" }, false, false},
		{"worlds+backup", func(p *Params) { p.WorldsHostPath = "/w"; p.BackupPVC = "b" }, false, false},
		{"worlds+archive", func(p *Params) { p.WorldsHostPath = "/w"; p.ArchiveLocalPath = "/a" }, false, false},
		{"backup+archive (no worlds)", func(p *Params) { p.BackupPVC = "b"; p.ArchiveLocalPath = "/a" }, false, true},
		{"all three", func(p *Params) { p.WorldsHostPath = "/w"; p.BackupPVC = "b"; p.ArchiveLocalPath = "/a" }, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testParams()
			c.mutate(&p)
			if got := reaperEnabled(p); got != c.reap {
				t.Errorf("reaperEnabled = %v, want %v", got, c.reap)
			}
			objs := Workloads(p)
			cj := findCronJob(objs)
			if c.cron != (cj != nil) {
				t.Fatalf("CronJob rendered = %v, want %v", cj != nil, c.cron)
			}
			var pv bool
			for _, o := range objs {
				if _, ok := o.(*corev1.PersistentVolume); ok {
					pv = true
				}
			}
			if pv != c.reap {
				t.Errorf("worlds-root PV rendered = %v, want %v", pv, c.reap)
			}
			if cj != nil {
				_, ctr := cronPodSpec(t, cj)
				if got := contains(ctr.Args, "--retention-only"); got == c.reap {
					t.Errorf("args = %v: --retention-only must be passed exactly when no world is reaped", ctr.Args)
				}
			}
		})
	}
}

// TestReaperCronJob_RetentionOnlyShape pins what the store-only run is left
// with: the config and the archive store, and nothing that reaches a world or
// the API — no worlds mount, no SMTP password, no service account token.
func TestReaperCronJob_RetentionOnlyShape(t *testing.T) {
	p := reaperParams()
	p.WorldsHostPath = ""
	ps, c := cronPodSpec(t, reaperCronJob(p))
	if want := []string{"--config", configFilePath, "--retention-only"}; !reflect.DeepEqual(c.Args, want) {
		t.Errorf("args = %v, want %v", c.Args, want)
	}
	if volumeByName(ps.Volumes, worldsVolume) != nil || mountByName(c.VolumeMounts, worldsVolume) != nil {
		t.Error("a retention-only run must not mount a worlds root")
	}
	if m := mountByName(c.VolumeMounts, backupVolume); m == nil || m.MountPath != p.ArchiveLocalPath || m.ReadOnly {
		t.Errorf("backup must be mounted read-write at ArchiveLocalPath %q, got %+v", p.ArchiveLocalPath, m)
	}
	if len(c.Env) != 0 {
		t.Errorf("env = %v, want none (it sends no warnings)", c.Env)
	}
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Error("a retention-only run never calls the API and must not mount a service account token")
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Add) != 1 || c.SecurityContext.Capabilities.Add[0] != "DAC_OVERRIDE" {
		t.Error("it reads back and deletes archives other identities wrote, so it keeps DAC_OVERRIDE")
	}
}

// TestReaperCronJob_NodePin proves the optional multi-node pin: no selector by
// default (the single-node starter), and exactly the kubernetes.io/hostname
// selector when ReaperNode names the node holding the worlds hostPath.
func TestReaperCronJob_NodePin(t *testing.T) {
	ps, _ := cronPodSpec(t, reaperCronJob(reaperParams()))
	if ps.NodeSelector != nil {
		t.Errorf("NodeSelector = %v, want none without ReaperNode", ps.NodeSelector)
	}
	p := reaperParams()
	p.ReaperNode = "node-a"
	ps, _ = cronPodSpec(t, reaperCronJob(p))
	if got := ps.NodeSelector["kubernetes.io/hostname"]; got != "node-a" {
		t.Errorf("nodeSelector = %v, want kubernetes.io/hostname=node-a", ps.NodeSelector)
	}
}

// TestReaperCronJob_Shape pins the rendered CronJob: its scheduling guards, its
// run-as identity (felis-reaper WITH an auto-mounted token, because it legitimately
// calls the K8s API — unlike the weak Job/registry pods), the hardening, the
// entrypoint, and the three-mount storage crux (config RO, worlds hostPath RO at
// /worlds, backup PVC RW at ArchiveLocalPath). Shape-asserted, runtime-unverified.
func TestReaperCronJob_Shape(t *testing.T) {
	p := reaperParams()
	cj := reaperCronJob(p)

	if cj.Kind != "CronJob" || cj.APIVersion != "batch/v1" {
		t.Errorf("CronJob TypeMeta = %s/%s, want batch/v1 CronJob", cj.APIVersion, cj.Kind)
	}
	if cj.Name != SAReaper {
		t.Errorf("CronJob name = %q, want %q", cj.Name, SAReaper)
	}
	// The CronJob must sit where its PVC lives: a Pod cannot mount a PVC across
	// namespaces, and the backup PVC is provisioned in the Minecraft namespace.
	// (Rendered under ControlNamespace it failed to schedule on a live cluster.)
	if cj.Namespace != p.MinecraftNamespace {
		t.Errorf("CronJob namespace = %q, want minecraft ns %q (its backup PVC's namespace)", cj.Namespace, p.MinecraftNamespace)
	}

	spec := cj.Spec
	if spec.Schedule == "" {
		t.Error("CronJob must set a schedule")
	}
	if spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Errorf("concurrencyPolicy = %q, want Forbid (retention runs must not overlap)", spec.ConcurrencyPolicy)
	}
	if spec.StartingDeadlineSeconds == nil {
		t.Error("CronJob must set startingDeadlineSeconds (a missed run should still start, bounded)")
	}
	if spec.SuccessfulJobsHistoryLimit == nil || spec.FailedJobsHistoryLimit == nil {
		t.Error("CronJob must bound job history")
	}
	js := spec.JobTemplate.Spec
	if js.BackoffLimit == nil {
		t.Error("Job must set backoffLimit")
	}
	if js.ActiveDeadlineSeconds == nil {
		t.Error("Job must set activeDeadlineSeconds (a wedged run must not hold the Forbid lock forever)")
	}

	ps, c := cronPodSpec(t, cj)
	if ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod restartPolicy = %q, want Never", ps.RestartPolicy)
	}
	if ps.ServiceAccountName != SAReaper {
		t.Errorf("pod serviceAccountName = %q, want %q (it patches MinecraftServers and deletes PVCs)", ps.ServiceAccountName, SAReaper)
	}
	// The reaper LEGITIMATELY calls the K8s API, so — unlike the build/restore/
	// registry pods — it must NOT disable the SA-token auto-mount.
	if ps.AutomountServiceAccountToken != nil {
		t.Errorf("reaper pod must auto-mount its SA token (got AutomountServiceAccountToken=%v); it needs the API", *ps.AutomountServiceAccountToken)
	}

	// The reaper is the one world-touching workload, so its identity is ROOT, not
	// the control-plane's non-root uid: the worlds it archives and deletes sit in a
	// root-owned storage root and hold Paper's mode-0600 files, whichever uid (the
	// game uid, or root for a world an older release wrote) owns them.
	if ps.SecurityContext == nil || ps.SecurityContext.RunAsNonRoot == nil || *ps.SecurityContext.RunAsNonRoot {
		t.Error("reaper pod must NOT require non-root: root is the owner-matching identity for game-image worlds")
	}
	if ps.SecurityContext == nil || ps.SecurityContext.RunAsUser == nil || *ps.SecurityContext.RunAsUser != 0 {
		t.Error("reaper pod must run as uid 0")
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("reaper container must set readOnlyRootFilesystem=true")
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) == 0 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Error("reaper container must drop ALL capabilities")
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Add) != 1 || c.SecurityContext.Capabilities.Add[0] != "DAC_OVERRIDE" {
		t.Error("reaper container must add exactly DAC_OVERRIDE")
	}

	// Entrypoint: `/usr/local/bin/felis reaper --config <cfg> --worlds-root /worlds`.
	if got := append(append([]string{}, c.Command...), c.Args...); !containsSeq(got, []string{felisBinaryPath, "reaper"}) {
		t.Errorf("reaper command/args = %v, want it to start `%s reaper`", got, felisBinaryPath)
	}
	if !contains(c.Args, "--config") || !contains(c.Args, configFilePath) {
		t.Errorf("reaper must read config at %s, got %v", configFilePath, c.Args)
	}
	if !contains(c.Args, "--worlds-root") || !contains(c.Args, worldsMountPath) {
		t.Errorf("reaper must read worlds at %s, got %v", worldsMountPath, c.Args)
	}
	if c.Image != p.FelisImage {
		t.Errorf("reaper image = %q, want FelisImage %q", c.Image, p.FelisImage)
	}

	// The relay password for pre-reap warning emails: same optional Secret as
	// felis-api, resolved against the minecraft-ns mirror. Optional so an install
	// without SMTP still starts (the reaper then logs suppressed warnings).
	smtpEnv := envVar(c.Env, SMTPPasswordEnv)
	if smtpEnv == nil || smtpEnv.ValueFrom == nil || smtpEnv.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("reaper must wire %s from a secretKeyRef", SMTPPasswordEnv)
	}
	if ref := smtpEnv.ValueFrom.SecretKeyRef; ref.Name != SMTPSecretName || ref.Key != SMTPSecretPasswordKey {
		t.Errorf("reaper %s ref = %s/%s, want %s/%s", SMTPPasswordEnv, ref.Name, ref.Key, SMTPSecretName, SMTPSecretPasswordKey)
	} else if ref.Optional == nil || !*ref.Optional {
		t.Errorf("reaper %s secretKeyRef must be optional", SMTPPasswordEnv)
	}

	// config: Secret, mounted read-only (it carries the DB URL).
	cfgVol := volumeByName(ps.Volumes, configVolume)
	if cfgVol == nil || cfgVol.Secret == nil || cfgVol.Secret.SecretName != configSecretName {
		t.Errorf("config volume must be Secret %q", configSecretName)
	}
	if m := mountByName(c.VolumeMounts, configVolume); m == nil || !m.ReadOnly {
		t.Error("config must be mounted read-only")
	}

	// worlds: the static worlds-root claim (never an inline hostPath, which the
	// namespace's PodSecurity baseline refuses), mounted READ-ONLY at /worlds — the
	// reaper only reads worlds to tar them (deletion is a PVC API call).
	wVol := volumeByName(ps.Volumes, worldsVolume)
	if wVol == nil || wVol.HostPath != nil || wVol.PersistentVolumeClaim == nil ||
		wVol.PersistentVolumeClaim.ClaimName != worldsRootName(p) || !wVol.PersistentVolumeClaim.ReadOnly {
		t.Errorf("worlds volume must be the read-only claim %q, got %+v", worldsRootName(p), wVol)
	}
	if m := mountByName(c.VolumeMounts, worldsVolume); m == nil || m.MountPath != worldsMountPath || !m.ReadOnly {
		t.Errorf("worlds must be mounted read-only at %s, got %+v", worldsMountPath, m)
	}

	// backup: PVC, mounted READ-WRITE at ArchiveLocalPath (== felis.toml [archive]
	// local_path, so tarLocal's absolute archive refs resolve under it).
	bVol := volumeByName(ps.Volumes, backupVolume)
	if bVol == nil || bVol.PersistentVolumeClaim == nil || bVol.PersistentVolumeClaim.ClaimName != p.BackupPVC {
		t.Errorf("backup volume must be PVC %q, got %+v", p.BackupPVC, bVol)
	}
	if m := mountByName(c.VolumeMounts, backupVolume); m == nil || m.MountPath != p.ArchiveLocalPath || m.ReadOnly {
		t.Errorf("backup must be mounted read-write at ArchiveLocalPath %q, got %+v", p.ArchiveLocalPath, m)
	}

	// The reaper never opens RCON, so its pod labels must NOT satisfy the RCON peer.
	if sel := rconPeerSelector(t, p); sel.Matches(labels.Set(cj.Spec.JobTemplate.Spec.Template.Labels)) {
		t.Error("reaper pod labels must NOT satisfy the rcon peer (component not in {api,operator})")
	}
}

// --- small env/volume helpers (test-local) ---

func envVar(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

func envValue(env []corev1.EnvVar, name string) string {
	if v := envVar(env, name); v != nil {
		return v.Value
	}
	return ""
}

func volumeByName(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		if vols[i].Name == name {
			return &vols[i]
		}
	}
	return nil
}

func mountByName(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

func accessModeStrings(modes []corev1.PersistentVolumeAccessMode) []string {
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = string(m)
	}
	return out
}

// containsSeq reports whether sub appears as a contiguous prefix-anchored run at
// the START of seq (command then args), which is what we want for an entrypoint.
func containsSeq(seq, sub []string) bool {
	if len(sub) > len(seq) {
		return false
	}
	for i := range sub {
		if seq[i] != sub[i] {
			return false
		}
	}
	return true
}

// TestWorldsRootStaticPV pins the pair that replaced the reaper's inline hostPath:
// the PV names the node directory (type Directory, so a missing root fails loud),
// is kept by Retain, sits outside every StorageClass and is reserved for exactly
// its PVC, which in turn names it back. Both render only alongside the reaper.
func TestWorldsRootStaticPV(t *testing.T) {
	p := reaperParams()
	var pv *corev1.PersistentVolume
	var pvc *corev1.PersistentVolumeClaim
	for _, obj := range Workloads(p) {
		switch o := obj.(type) {
		case *corev1.PersistentVolume:
			pv = o
		case *corev1.PersistentVolumeClaim:
			if o.Name == worldsRootName(p) {
				pvc = o
			}
		}
	}
	if pv == nil || pvc == nil {
		t.Fatalf("reaper-enabled bundle must render the worlds-root PV and PVC (pv=%v pvc=%v)", pv != nil, pvc != nil)
	}
	hp := pv.Spec.HostPath
	if hp == nil || hp.Path != p.WorldsHostPath || hp.Type == nil || *hp.Type != corev1.HostPathDirectory {
		t.Errorf("PV hostPath = %+v, want %s type Directory", hp, p.WorldsHostPath)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("PV reclaim policy = %q, want Retain", pv.Spec.PersistentVolumeReclaimPolicy)
	}
	if pv.Spec.StorageClassName != "" || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "" {
		t.Error("PV and PVC must both opt out of every StorageClass")
	}
	if ref := pv.Spec.ClaimRef; ref == nil || ref.Namespace != p.withDefaults().MinecraftNamespace || ref.Name != pvc.Name {
		t.Errorf("PV claimRef = %+v, want %s/%s", ref, p.withDefaults().MinecraftNamespace, pvc.Name)
	}
	if pvc.Spec.VolumeName != pv.Name || pvc.Namespace != p.withDefaults().MinecraftNamespace {
		t.Errorf("PVC %s/%s volumeName = %q, want %q", pvc.Namespace, pvc.Name, pvc.Spec.VolumeName, pv.Name)
	}
	if pv.Spec.NodeAffinity != nil {
		t.Error("no ReaperNode: the PV must carry no node affinity")
	}

	// A pinned reaper pins its PV to the same node, and a moved root gets a new name
	// because hostPath and node affinity are immutable on a live PV.
	pinned := reaperParams()
	pinned.ReaperNode = "node-a"
	ppv := worldsRootPV(pinned)
	terms := ppv.Spec.NodeAffinity
	if terms == nil || terms.Required == nil || len(terms.Required.NodeSelectorTerms) != 1 ||
		terms.Required.NodeSelectorTerms[0].MatchExpressions[0].Values[0] != "node-a" {
		t.Errorf("pinned PV node affinity = %+v, want kubernetes.io/hostname in [node-a]", terms)
	}
	if ppv.Name == pv.Name {
		t.Error("a different node pin must render a differently named PV")
	}
	moved := reaperParams()
	moved.WorldsHostPath = "/srv/worlds"
	if worldsRootName(moved) == pv.Name {
		t.Error("a different worlds-root must render a differently named PV")
	}

	for _, obj := range Workloads(testParams()) {
		if _, ok := obj.(*corev1.PersistentVolume); ok {
			t.Error("without the reaper trio no PV may render")
		}
	}
}

// TestOperatorMetricsService: the operator's /metrics has a ClusterIP scrape
// target selecting the operator pods on their "metrics" port, marked for
// prometheus.io discovery like the api's internal face.
func TestOperatorMetricsService(t *testing.T) {
	p := testParams()
	svc := operatorMetricsService(p)
	dep := OperatorDeployment(p)
	if svc.Name != OperatorMetricsServiceName || svc.Namespace != p.ControlNamespace || svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("Service = %s/%s %s, want %s/%s ClusterIP", svc.Namespace, svc.Name, svc.Spec.Type, p.ControlNamespace, OperatorMetricsServiceName)
	}
	if !mapSelectorMatches(svc.Spec.Selector, dep.Spec.Template.Labels) {
		t.Errorf("selector %v does not select operator pod labels %v", svc.Spec.Selector, dep.Spec.Template.Labels)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].TargetPort.StrVal != "metrics" || svc.Spec.Ports[0].Port != operatorMetricsPort {
		t.Errorf("ports = %+v, want %d -> metrics", svc.Spec.Ports, operatorMetricsPort)
	}
	for _, s := range []*corev1.Service{svc, apiInternalService(p)} {
		if s.Annotations["prometheus.io/scrape"] != "true" || s.Annotations["prometheus.io/port"] != fmt.Sprint(s.Spec.Ports[0].Port) {
			t.Errorf("%s annotations = %v, want prometheus.io scrape on its port", s.Name, s.Annotations)
		}
	}
	found := false
	for _, o := range Workloads(p) {
		if s, ok := o.(*corev1.Service); ok && s.Name == OperatorMetricsServiceName {
			found = true
		}
	}
	if !found {
		t.Error("Workloads does not render the operator metrics Service")
	}
}

// TestPVCSizes proves the three claim sizes follow Params and default when unset,
// and that Validate refuses a size the API server would reject.
func TestPVCSizes(t *testing.T) {
	sizes := func(p Params) map[string]string {
		got := map[string]string{}
		for _, o := range Workloads(p.withDefaults()) {
			if pvc, ok := o.(*corev1.PersistentVolumeClaim); ok {
				got[pvc.Name] = pvc.Spec.Resources.Requests.Storage().String()
			}
		}
		return got
	}
	p := testParams()
	p.BackupPVC = "felis-backups"
	def := sizes(p)
	if def[registryName] != registryStorageSize || def[uploadsPVCName] != uploadsStorageSize || def["felis-backups"] != backupStorageSize {
		t.Errorf("default sizes = %v", def)
	}
	p.RegistryStorage, p.UploadsStorage, p.BackupStorage = "40Gi", "8Gi", "100Gi"
	if got := sizes(p); got[registryName] != "40Gi" || got[uploadsPVCName] != "8Gi" || got["felis-backups"] != "100Gi" {
		t.Errorf("configured sizes = %v", got)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate(%+v) = %v", p, err)
	}
	for _, bad := range []string{"lots", "0", "-1Gi"} {
		q := testParams()
		q.UploadsStorage = bad
		if err := q.Validate(); err == nil {
			t.Errorf("Validate accepted uploads storage %q", bad)
		}
	}
}
