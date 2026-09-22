package platform

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
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
		ps, c := podSpec(t, d)

		if ps.SecurityContext == nil || ps.SecurityContext.RunAsNonRoot == nil || !*ps.SecurityContext.RunAsNonRoot {
			t.Errorf("%s: pod must set runAsNonRoot=true", d.Name)
		}
		if ps.SecurityContext == nil || ps.SecurityContext.RunAsUser == nil || *ps.SecurityContext.RunAsUser != nonRootUID {
			t.Errorf("%s: pod runAsUser must be %d", d.Name, nonRootUID)
		}
		sc := c.SecurityContext
		if sc == nil {
			t.Fatalf("%s: container has no SecurityContext", d.Name)
		}
		if sc.Privileged == nil || *sc.Privileged {
			t.Errorf("%s: container must not be privileged", d.Name)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: container must set allowPrivilegeEscalation=false", d.Name)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s: container must set readOnlyRootFilesystem=true", d.Name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s: container must drop ALL capabilities", d.Name)
		}
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
	// FELIS_SERVICE_TOKEN must come from a Secret, never a literal value.
	tok := envVar(c.Env, "FELIS_SERVICE_TOKEN")
	if tok == nil || tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef == nil {
		t.Fatal("FELIS_SERVICE_TOKEN must be sourced from a secretKeyRef")
	}
	if tok.Value != "" {
		t.Error("FELIS_SERVICE_TOKEN must not carry a literal value")
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

	ps, c := podSpec(t, dep)
	if c.Image != defaultRegistryImage {
		t.Errorf("registry image = %q, want default %q", c.Image, defaultRegistryImage)
	}
	// REGISTRY_HTTP_ADDR pins the listen port to the Service port rather than
	// trusting the image default.
	if v := envValue(c.Env, "REGISTRY_HTTP_ADDR"); v != ":5000" {
		t.Errorf("REGISTRY_HTTP_ADDR = %q, want :5000", v)
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
		{registryDeployment(p), "/v2/", "/v2/", p.RegistryPort},
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
		_, c := podSpec(t, tc.dep)
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
// control-plane Deployments, the api external+internal Services, and the registry
// Deployment/Service/PVC, every one with TypeMeta (so its YAML header renders). The
// internal Service must be present or the login pod's felis-api:8081 path is dead.
func TestWorkloads_BundleContents(t *testing.T) {
	objs := Workloads(testParams())
	if len(objs) != 8 {
		t.Fatalf("Workloads returned %d objects, want 8", len(objs))
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

// TestReaperCronJob_Gating proves the reaper renders iff all three storage
// coordinates are present: an incomplete configuration must produce NO CronJob
// (the partial-flag mistake is rejected at the CLI; here the renderer fails safe).
func TestReaperCronJob_Gating(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *Params)
		want   bool
	}{
		{"none", func(p *Params) {}, false},
		{"worlds only", func(p *Params) { p.WorldsHostPath = "/w" }, false},
		{"worlds+backup", func(p *Params) { p.WorldsHostPath = "/w"; p.BackupPVC = "b" }, false},
		{"worlds+archive", func(p *Params) { p.WorldsHostPath = "/w"; p.ArchiveLocalPath = "/a" }, false},
		{"backup+archive (no worlds)", func(p *Params) { p.BackupPVC = "b"; p.ArchiveLocalPath = "/a" }, false},
		{"all three", func(p *Params) { p.WorldsHostPath = "/w"; p.BackupPVC = "b"; p.ArchiveLocalPath = "/a" }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testParams()
			c.mutate(&p)
			if got := reaperEnabled(p); got != c.want {
				t.Errorf("reaperEnabled = %v, want %v", got, c.want)
			}
			cj := findCronJob(Workloads(p))
			if c.want && cj == nil {
				t.Error("CronJob must be in Workloads when enabled")
			}
			if !c.want && cj != nil {
				t.Error("CronJob must NOT be in Workloads when disabled")
			}
		})
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

	// Hardening mirrors the other control-plane pods.
	if ps.SecurityContext == nil || ps.SecurityContext.RunAsNonRoot == nil || !*ps.SecurityContext.RunAsNonRoot {
		t.Error("reaper pod must set runAsNonRoot=true")
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("reaper container must set readOnlyRootFilesystem=true")
	}
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) == 0 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Error("reaper container must drop ALL capabilities")
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

	// config: Secret, mounted read-only (it carries the DB URL).
	cfgVol := volumeByName(ps.Volumes, configVolume)
	if cfgVol == nil || cfgVol.Secret == nil || cfgVol.Secret.SecretName != configSecretName {
		t.Errorf("config volume must be Secret %q", configSecretName)
	}
	if m := mountByName(c.VolumeMounts, configVolume); m == nil || !m.ReadOnly {
		t.Error("config must be mounted read-only")
	}

	// worlds: node hostPath at WorldsHostPath, type Directory, mounted READ-ONLY at
	// /worlds — the reaper only reads worlds to tar them (deletion is a PVC API call).
	wVol := volumeByName(ps.Volumes, worldsVolume)
	if wVol == nil || wVol.HostPath == nil || wVol.HostPath.Path != p.WorldsHostPath {
		t.Errorf("worlds volume must be hostPath %q, got %+v", p.WorldsHostPath, wVol)
	}
	if wVol != nil && (wVol.HostPath == nil || wVol.HostPath.Type == nil || *wVol.HostPath.Type != corev1.HostPathDirectory) {
		t.Error("worlds hostPath must be type Directory (fail loud if the dir is absent)")
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
