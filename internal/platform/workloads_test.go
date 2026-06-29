package platform

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	if c.Image != p.FelisImage {
		t.Errorf("operator image = %q, want FelisImage %q", c.Image, p.FelisImage)
	}
	// No config Secret volume: the operator reads config from flags + the API only.
	for _, v := range ps.Volumes {
		if v.Secret != nil {
			t.Errorf("operator must mount NO Secret volume, found %q", v.Name)
		}
	}
	// And it must hold no credential env at all.
	if len(c.Env) != 0 {
		t.Errorf("operator must carry no env (flags-only), got %v", c.Env)
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

// TestWorkloads_BundleContents sanity-checks the slice Workloads returns: the two
// control-plane Deployments, the api Service, and the registry Deployment/Service/PVC,
// every one with TypeMeta (so its YAML header renders).
func TestWorkloads_BundleContents(t *testing.T) {
	objs := Workloads(testParams())
	if len(objs) != 6 {
		t.Fatalf("Workloads returned %d objects, want 6", len(objs))
	}
	for _, o := range objs {
		gvk := o.GetObjectKind().GroupVersionKind()
		if gvk.Kind == "" || gvk.Version == "" {
			t.Errorf("%T missing TypeMeta (kind=%q version=%q)", o, gvk.Kind, gvk.Version)
		}
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
	if cj.Namespace != p.ControlNamespace {
		t.Errorf("CronJob namespace = %q, want control ns %q", cj.Namespace, p.ControlNamespace)
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
