package build

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

func sampleJobParams() JobParams {
	return JobParams{
		BuildID:        "bld-1",
		ImageRef:       "registry.felis.svc:5000/mc-paper:1.0",
		ContextRef:     "tar://contexts/abc.tar.gz",
		Namespace:      defaultNamespace,
		ServiceAccount: defaultServiceAccount,
		RegistryURL:    "registry.felis.svc:5000",
		FelisImage:     "felis:test",
		KanikoImage:    defaultKanikoImage,
		TrivyImage:     defaultTrivyImage,
		Deadline:       30 * time.Minute,
		CPULimit:       "2",
		MemLimit:       "4Gi",
	}
}

// The Job must run in the isolated build namespace under the weak build SA —
// never the api namespace/identity, and never the minecraft namespace. This is
// the central §16/§21 red line, asserted on the rendered spec because no cluster
// runs here.
func TestBuildJobRunsIsolatedUnderWeakSA(t *testing.T) {
	job, err := BuildJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	if job.Namespace != "felis-build" {
		t.Errorf("namespace = %q, want felis-build (never minecraft/felis-system)", job.Namespace)
	}
	sa := job.Spec.Template.Spec.ServiceAccountName
	if sa != "felis-build" {
		t.Errorf("service account = %q, want felis-build (never felis-api)", sa)
	}
	if sa == "felis-api" {
		t.Fatal("build Pod must NOT run as the felis-api SA")
	}
	// The SA token must not be mounted: with no token, the Pod cannot reach the
	// K8s API even if a Role were mis-bound.
	if amt := job.Spec.Template.Spec.AutomountServiceAccountToken; amt == nil || *amt {
		t.Error("AutomountServiceAccountToken must be explicitly false")
	}
}

// A poisoned build must terminate and not loop or run unbounded.
func TestBuildJobIsBoundedAndOneShot(t *testing.T) {
	job, err := BuildJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("BackoffLimit must be 0 — a poisoned build must not retry")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 0 {
		t.Error("ActiveDeadlineSeconds must be a positive wall-clock cap")
	}
	if got := *job.Spec.ActiveDeadlineSeconds; got != int64((30 * time.Minute).Seconds()) {
		t.Errorf("ActiveDeadlineSeconds = %d, want 1800", got)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("RestartPolicy must be Never")
	}
}

// A finished build must not squat in the namespace forever: without a TTL the
// Job and its completed Pod accumulate one pair per build and eventually eat
// the node's pod budget. The window is deliberately long (see buildJobTTL) so
// the admin log stream stays useful for triage.
func TestBuildJobIsReapedAfterCompletion(t *testing.T) {
	job, err := BuildJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Fatal("TTLSecondsAfterFinished must be set so the finished Job (and its log Pod) is GC'd")
	}
	if got, want := *job.Spec.TTLSecondsAfterFinished, int32((7*24*time.Hour)/time.Second); got != want {
		t.Errorf("TTLSecondsAfterFinished = %d, want %d (buildJobTTL)", got, want)
	}
}

// No build container may be privileged or able to escalate, and all containers
// must carry resource limits.
func TestBuildJobContainersAreHardened(t *testing.T) {
	job, err := BuildJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	all := append([]corev1.Container{}, job.Spec.Template.Spec.InitContainers...)
	all = append(all, job.Spec.Template.Spec.Containers...)
	if len(all) < 2 {
		t.Fatalf("expected kaniko initContainer + trivy container, got %d containers", len(all))
	}
	for _, c := range all {
		sc := c.SecurityContext
		if sc == nil {
			t.Fatalf("container %q has no security context", c.Name)
		}
		if sc.Privileged == nil || *sc.Privileged {
			t.Errorf("container %q must not be privileged (Kaniko needs no daemon)", c.Name)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("container %q must set allowPrivilegeEscalation=false", c.Name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 {
			t.Errorf("container %q must drop capabilities", c.Name)
		} else if string(sc.Capabilities.Drop[0]) != "ALL" {
			t.Errorf("container %q must drop ALL capabilities, got %v", c.Name, sc.Capabilities.Drop)
		}
		if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
			t.Errorf("container %q must carry CPU+memory limits", c.Name)
		}
	}
}

// The limits are caps, but the requests must be a schedulable floor: reserving the
// full 2 CPU / 4Gi on a starter node (4 vCPU / 5.5 GiB, where the api, operator,
// registry, Postgres and the game pods live too) leaves no room for the Pod —
// proven live: FailedScheduling/Insufficient memory, build stuck Pending forever.
func TestBuildJobRequestsAreASchedulableFloor(t *testing.T) {
	job, err := BuildJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	all := append([]corev1.Container{}, job.Spec.Template.Spec.InitContainers...)
	all = append(all, job.Spec.Template.Spec.Containers...)
	for _, c := range all {
		reqMem, limMem := c.Resources.Requests.Memory(), c.Resources.Limits.Memory()
		reqCPU, limCPU := c.Resources.Requests.Cpu(), c.Resources.Limits.Cpu()
		if reqMem.Cmp(*limMem) >= 0 || reqCPU.Cmp(*limCPU) >= 0 {
			t.Errorf("container %q requests must be below its limits (req %s/%s, lim %s/%s)",
				c.Name, reqCPU, reqMem, limCPU, limMem)
		}
		if reqMem.IsZero() || reqCPU.IsZero() {
			t.Errorf("container %q must still ask for a non-zero floor", c.Name)
		}
	}
	// A tiny operator-set cap must be honoured: the request never exceeds it.
	p := sampleJobParams()
	p.CPULimit, p.MemLimit = "100m", "128Mi"
	job, err = BuildJob(p)
	if err != nil {
		t.Fatalf("BuildJob(tiny): %v", err)
	}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Resources.Requests.Memory().Cmp(*c.Resources.Limits.Memory()) > 0 ||
			c.Resources.Requests.Cpu().Cmp(*c.Resources.Limits.Cpu()) > 0 {
			t.Errorf("container %q request exceeds a configured cap", c.Name)
		}
	}
}

// kaniko builds the request's exact target into a tarball and never pushes;
// trivy gates on that tarball with --exit-code 1 --severity CRITICAL; only then
// does the push container publish it. An image that fails the scan is therefore
// never in the registry, and the credential is never where the Dockerfile runs.
func TestBuildJobScansBeforePush(t *testing.T) {
	p := sampleJobParams()
	job, err := BuildJob(p)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	inits := job.Spec.Template.Spec.InitContainers
	if len(inits) != 3 || inits[0].Name != ContainerGate || inits[1].Name != ContainerKaniko || inits[2].Name != ContainerTrivy {
		t.Fatalf("initContainers = %v, want [egress-gate kaniko trivy]", initNames(inits))
	}
	kaniko, trivy := inits[1], inits[2]
	for _, want := range []string{"--destination=" + p.ImageRef, "--no-push", "--tar-path=" + imageTarPath} {
		if !hasArg(kaniko.Args, want) {
			t.Errorf("kaniko args = %v, want %s", kaniko.Args, want)
		}
	}
	for _, pushFlag := range []string{"--insecure", "--skip-tls-verify"} {
		if hasArg(kaniko.Args, pushFlag) {
			t.Errorf("kaniko args = %v still carry the push-side %s", kaniko.Args, pushFlag)
		}
	}
	// The pull direction needs its own flags: without the pull pair a
	// Dockerfile's `FROM` fails against the plain-HTTP registry ("server gave
	// HTTP response to HTTPS client") — the live failure this guards.
	for _, flag := range []string{"--insecure-pull", "--skip-tls-verify-pull"} {
		if !hasArg(kaniko.Args, flag) {
			t.Errorf("kaniko args = %v, want %s so base-image pulls use plain HTTP", kaniko.Args, flag)
		}
	}

	// The scan gate: a CRITICAL CVE must fail the Pod (and thus the Job) before
	// the push container ever starts.
	if !argPairPresent(trivy.Args, "--input", imageTarPath) {
		t.Errorf("trivy must scan the built tarball, args=%v", trivy.Args)
	}
	if hasArg(trivy.Args, p.ImageRef) {
		t.Errorf("trivy must not scan the registry ref (nothing is pushed yet), args=%v", trivy.Args)
	}
	if !argPairPresent(trivy.Args, "--exit-code", "1") {
		t.Errorf("trivy must run with --exit-code 1, args=%v", trivy.Args)
	}
	if !argPairPresent(trivy.Args, "--severity", "CRITICAL") {
		t.Errorf("trivy must gate on --severity CRITICAL, args=%v", trivy.Args)
	}
	// No DB repositories configured: Trivy keeps its own defaults.
	if hasArg(trivy.Args, "--db-repository") || hasArg(trivy.Args, "--java-db-repository") {
		t.Errorf("unset DB repositories must not render --db-repository/--java-db-repository, args=%v", trivy.Args)
	}

	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected exactly one (push) main container")
	}
	push := job.Spec.Template.Spec.Containers[0]
	if push.Name != ContainerPush || push.Image != p.FelisImage {
		t.Errorf("main container = %s (%s), want push running the platform image", push.Name, push.Image)
	}
	if !hasArg(push.Args, "push-image") || !hasArg(push.Args, "--ref="+p.ImageRef) || !hasArg(push.Args, "--tar="+imageTarPath) {
		t.Errorf("push args = %v, want push-image --tar=%s --ref=%s", push.Args, imageTarPath, p.ImageRef)
	}
	creds := map[string]string{}
	for _, e := range push.Env {
		if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Errorf("push env %s must come from a secretKeyRef, got %#v", e.Name, e)
			continue
		}
		creds[e.Name] = e.ValueFrom.SecretKeyRef.Name
	}
	for _, name := range []string{"FELIS_REGISTRY_USERNAME", "FELIS_REGISTRY_PASSWORD"} {
		if creds[name] != "felis-registry-push" {
			t.Errorf("push env %s from secret %q, want felis-registry-push", name, creds[name])
		}
	}
	// Nothing else in the pod may hold the credential.
	for _, c := range inits {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == "felis-registry-push" {
				t.Errorf("container %s holds the registry credential", c.Name)
			}
		}
	}
	// The tarball is shared through a bounded emptyDir, read-only past kaniko.
	var imgVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == imageVolume {
			imgVol = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if imgVol == nil || imgVol.EmptyDir == nil || imgVol.EmptyDir.SizeLimit == nil {
		t.Fatalf("image volume must be a size-limited emptyDir, got %#v", imgVol)
	}
	for _, c := range []corev1.Container{trivy, push} {
		ro := false
		for _, m := range c.VolumeMounts {
			if m.Name == imageVolume && m.ReadOnly {
				ro = true
			}
		}
		if !ro {
			t.Errorf("%s must mount the image tarball read-only, got %v", c.Name, c.VolumeMounts)
		}
	}
}

func TestBuildJobNeedsFelisImage(t *testing.T) {
	p := sampleJobParams()
	p.FelisImage = ""
	if _, err := BuildJob(p); err == nil {
		t.Fatal("a build without FelisImage has no push container and must fail to render")
	}
}

// Kaniko (and only kaniko) must carry back exactly the unpacking capability
// subset: drop-ALL broke every FROM <base image> build live ("failed to get
// filesystem from image: chown /etc/gshadow: operation not permitted"), while a
// scratch COPY masked it. The pod stays unprivileged and every other container
// keeps the pure baseline.
func TestBuildJobKanikoGetsOnlyUnpackCapabilities(t *testing.T) {
	job, err := BuildJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	var kaniko *corev1.Container
	for i := range job.Spec.Template.Spec.InitContainers {
		if job.Spec.Template.Spec.InitContainers[i].Name == "kaniko" {
			kaniko = &job.Spec.Template.Spec.InitContainers[i]
		}
	}
	if kaniko == nil {
		t.Fatal("no kaniko initContainer")
	}
	sc := kaniko.SecurityContext
	if sc == nil || sc.Capabilities == nil {
		t.Fatal("kaniko lost its security context")
	}
	if sc.Privileged == nil || *sc.Privileged ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("kaniko must stay unprivileged with no escalation, got %+v", sc)
	}
	if len(sc.Capabilities.Drop) != 1 || string(sc.Capabilities.Drop[0]) != "ALL" {
		t.Errorf("kaniko must still drop ALL, got %v", sc.Capabilities.Drop)
	}
	want := map[corev1.Capability]bool{"CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true}
	if len(sc.Capabilities.Add) != len(want) {
		t.Fatalf("kaniko capabilities.add = %v, want exactly the unpack trio", sc.Capabilities.Add)
	}
	for _, c := range sc.Capabilities.Add {
		if !want[c] {
			t.Errorf("kaniko must not gain %q — only the unpack trio is justified", c)
		}
	}
	// Every other container keeps the pure baseline: nothing re-added.
	all := append([]corev1.Container{}, job.Spec.Template.Spec.InitContainers...)
	all = append(all, job.Spec.Template.Spec.Containers...)
	for _, c := range all {
		if c.Name == "kaniko" {
			continue
		}
		if c.SecurityContext != nil && c.SecurityContext.Capabilities != nil && len(c.SecurityContext.Capabilities.Add) > 0 {
			t.Errorf("container %q must not add capabilities, got %v", c.Name, c.SecurityContext.Capabilities.Add)
		}
	}
}

// Configured DB repositories (the internal mirrors) must reach Trivy as
// --db-repository / --java-db-repository: without them the scan tries the
// internet, which the build egress lock denies, and every build fails closed at
// the scan gate — the Java DB the moment the image contains a jar.
func TestBuildJobTrivyDBRepositoryOverride(t *testing.T) {
	p := sampleJobParams()
	p.TrivyDBRepository = "registry.felis.svc:5000/mirror/trivy-db:2"
	p.TrivyJavaDBRepository = "registry.felis.svc:5000/mirror/trivy-java-db:1"
	job, err := BuildJob(p)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	trivy := job.Spec.Template.Spec.InitContainers[2]
	if trivy.Name != ContainerTrivy {
		t.Fatalf("initContainers = %v, want trivy third", initNames(job.Spec.Template.Spec.InitContainers))
	}
	if !argPairPresent(trivy.Args, "--db-repository", p.TrivyDBRepository) {
		t.Errorf("trivy args = %v, want --db-repository %s", trivy.Args, p.TrivyDBRepository)
	}
	if !argPairPresent(trivy.Args, "--java-db-repository", p.TrivyJavaDBRepository) {
		t.Errorf("trivy args = %v, want --java-db-repository %s", trivy.Args, p.TrivyJavaDBRepository)
	}
	if !argPairPresent(trivy.Args, "--input", imageTarPath) {
		t.Errorf("trivy args = %v, want --input %s", trivy.Args, imageTarPath)
	}
}

// The build namespace egress lock must be default-deny: deny all ingress, and
// allow egress only to cluster DNS + the internal registry — never an allow-all rule.
func TestBuildNetworkPolicyIsDefaultDeny(t *testing.T) {
	np := BuildNetworkPolicy(NetPolParams{
		Namespace:         "felis-build",
		RegistryNamespace: "felis-system",
		RegistryPort:      5000,
	})

	if !hasPolicyType(np, networkingv1.PolicyTypeEgress) || !hasPolicyType(np, networkingv1.PolicyTypeIngress) {
		t.Fatal("policy must govern both Ingress and Egress")
	}
	// Ingress: empty rule slice = deny all.
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("ingress must be empty (deny all), got %d rules", len(np.Spec.Ingress))
	}
	// The selector must capture every build Pod by the managed-by label.
	if np.Spec.PodSelector.MatchLabels[LabelManagedBy] != managedByValue {
		t.Errorf("pod selector must match managed-by=%s", managedByValue)
	}
	// No egress rule may be an allow-all (a rule with neither To peers nor Ports
	// would permit unrestricted egress).
	for i, rule := range np.Spec.Egress {
		if len(rule.To) == 0 && len(rule.Ports) == 0 {
			t.Errorf("egress rule %d is allow-all (no To, no Ports) — internet would be open", i)
		}
	}
	// The registry must be reachable (by namespace selector), and DNS allowed.
	if !egressAllowsNamespace(np, "felis-system") {
		t.Error("egress must allow the registry namespace")
	}
	if !egressAllowsPort(np, 53) {
		t.Error("egress must allow DNS (port 53)")
	}
	// ...but only to the cluster resolver: port 53 to any address is a way out
	// of the sandbox for anything that speaks DNS (or anything at all) on 53.
	for i, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port == nil || port.Port.IntVal != 53 {
				continue
			}
			if len(rule.To) != 1 || rule.To[0].PodSelector == nil || rule.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" ||
				rule.To[0].NamespaceSelector == nil || rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
				t.Errorf("egress rule %d opens port 53 to %v, want only kube-system/k8s-app=kube-dns", i, rule.To)
			}
		}
	}
	// The context fetch: build Pods stream submissions from the control
	// namespace's internal face (defaults: felis + 8081).
	if !egressAllowsNamespace(np, "felis") {
		t.Error("egress must allow the control namespace (context fetch)")
	}
	if !egressAllowsPort(np, 8081) {
		t.Error("egress must allow the internal face's port (8081)")
	}
}

// An http(s) context ref — the submit lane's derived shape — must render the
// fetch initContainer ahead of Kaniko, with the service token mounted ONLY into
// that container, and hand Kaniko the extracted local directory.
func TestBuildJobFetchesHTTPContext(t *testing.T) {
	p := sampleJobParams()
	p.ContextRef = "http://felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/submissions/sub-abc/context"
	p.FelisImage = "felis:test"
	job, err := BuildJob(p)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	inits := job.Spec.Template.Spec.InitContainers
	if len(inits) != 4 || inits[0].Name != ContainerGate || inits[1].Name != ContainerFetch ||
		inits[2].Name != ContainerKaniko || inits[3].Name != ContainerTrivy {
		t.Fatalf("initContainers = %v, want [%s %s %s %s]", initNames(inits), ContainerGate, ContainerFetch, ContainerKaniko, ContainerTrivy)
	}
	fetch, kaniko := inits[1], inits[2]
	if fetch.Image != p.FelisImage {
		t.Errorf("fetch image = %q, want the platform image %q", fetch.Image, p.FelisImage)
	}
	if !hasArg(fetch.Args, "fetch-context") || !hasArg(fetch.Args, "--url="+p.ContextRef) ||
		!hasArg(fetch.Args, "--out="+contextMountPath) {
		t.Errorf("fetch args = %v, want fetch-context --url=%s --out=%s", fetch.Args, p.ContextRef, contextMountPath)
	}
	// The token comes from the namespace-local Secret and is mounted into the
	// fetch container only — never into Kaniko, which executes the untrusted
	// Dockerfile.
	var fetchToken *corev1.EnvVar
	for i := range fetch.Env {
		if fetch.Env[i].Name == "FELIS_SERVICE_TOKEN" {
			fetchToken = &fetch.Env[i]
		}
	}
	if fetchToken == nil || fetchToken.ValueFrom == nil || fetchToken.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("fetch container must read FELIS_SERVICE_TOKEN from a secretKeyRef, got %#v", fetchToken)
	}
	if fetchToken.Value != "" {
		t.Error("fetch container must not carry a literal token")
	}
	if len(kaniko.Env) != 0 {
		t.Errorf("kaniko must carry no env (especially no token), got %v", kaniko.Env)
	}
	// The fetch container extracts as root — the uid Kaniko runs as — because
	// Kaniko re-copies the Dockerfile and chowns it to the source owner, which no
	// other uid can satisfy under this pod's dropped capabilities.
	if fetch.SecurityContext == nil || fetch.SecurityContext.RunAsUser == nil || *fetch.SecurityContext.RunAsUser != 0 {
		t.Error("fetch container must extract as root so Kaniko can inherit the context ownership")
	}
	if fetch.SecurityContext != nil && (fetch.SecurityContext.Privileged == nil || *fetch.SecurityContext.Privileged) {
		t.Error("fetch container must still be unprivileged")
	}
	if !hasArg(kaniko.Args, "--context="+contextMountPath) {
		t.Errorf("kaniko context = %v, want the fetched local dir %s", kaniko.Args, contextMountPath)
	}
	// The shared emptyDir must exist, be bounded, and be read-only to Kaniko.
	var ctxVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == contextVolume {
			ctxVol = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if ctxVol == nil || ctxVol.EmptyDir == nil || ctxVol.EmptyDir.SizeLimit == nil {
		t.Fatalf("context volume must be a size-limited emptyDir, got %#v", ctxVol)
	}
	mountedRO := false
	for _, m := range kaniko.VolumeMounts {
		if m.Name == contextVolume && m.MountPath == contextMountPath && m.ReadOnly {
			mountedRO = true
		}
	}
	if !mountedRO {
		t.Errorf("kaniko must mount the context read-only at %s, got %v", contextMountPath, kaniko.VolumeMounts)
	}
}

// A ref Kaniko reads natively (or an installer pre-mounted) must NOT grow the
// fetch initContainer: the transport is for http(s) only.
func TestBuildJobNativeContextNeedsNoFetch(t *testing.T) {
	p := sampleJobParams()
	p.ContextRef = "s3://bucket/prefix/context.tar.gz"
	job, err := BuildJob(p)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	if inits := job.Spec.Template.Spec.InitContainers; len(inits) != 3 || inits[1].Name != ContainerKaniko {
		t.Errorf("a native ref must render just the gate, kaniko and trivy, got %v", initNames(inits))
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == contextVolume {
			t.Errorf("a native ref must render no context volume, got %v", job.Spec.Template.Spec.Volumes)
		}
	}
}

// Nothing runs before the egress gate, and the gate itself holds nothing: no
// credential, no root, no writable filesystem.
func TestBuildJobGatesEgressFirst(t *testing.T) {
	p := sampleJobParams()
	p.ContextRef = "http://felis-api-internal.felis.svc.cluster.local:8081/ctx"
	job, err := BuildJob(p)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	gate := job.Spec.Template.Spec.InitContainers[0]
	if gate.Name != ContainerGate || gate.Image != p.FelisImage || len(gate.Args) != 1 || gate.Args[0] != "egress-gate" {
		t.Fatalf("first initContainer = %s %s %v, want the platform image's egress-gate", gate.Name, gate.Image, gate.Args)
	}
	if len(gate.Env) != 0 || len(gate.VolumeMounts) != 0 {
		t.Errorf("the gate must hold nothing, got env %v mounts %v", gate.Env, gate.VolumeMounts)
	}
	sc := gate.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("the gate must run non-root on a read-only root, got %#v", sc)
	}
}

// The pod runs under RuntimeDefault seccomp always, and in a user namespace or
// a sandbox runtime when the install asks for them.
func TestBuildJobSandboxing(t *testing.T) {
	job, err := BuildJob(sampleJobParams())
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	spec := job.Spec.Template.Spec
	if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil ||
		spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod securityContext = %#v, want seccompProfile RuntimeDefault", spec.SecurityContext)
	}
	if spec.HostUsers != nil || spec.RuntimeClassName != nil {
		t.Errorf("defaults must leave hostUsers and runtimeClassName unset, got %v / %v", spec.HostUsers, spec.RuntimeClassName)
	}
	p := sampleJobParams()
	p.UserNamespaces, p.RuntimeClass = true, "gvisor"
	if job, err = BuildJob(p); err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	spec = job.Spec.Template.Spec
	if spec.HostUsers == nil || *spec.HostUsers {
		t.Errorf("UserNamespaces must render hostUsers: false, got %v", spec.HostUsers)
	}
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != "gvisor" {
		t.Errorf("runtimeClassName = %v, want gvisor", spec.RuntimeClassName)
	}
}

// Every container carries an ephemeral-storage request and limit, and kaniko's
// limit, the largest, is the configured disk cap.
func TestBuildJobBoundsEphemeralStorage(t *testing.T) {
	p := sampleJobParams()
	p.ContextRef = "http://felis-api-internal.felis.svc.cluster.local:8081/ctx"
	job, err := BuildJob(p)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	all := append(append([]corev1.Container{}, job.Spec.Template.Spec.InitContainers...), job.Spec.Template.Spec.Containers...)
	for _, c := range all {
		lim, lok := c.Resources.Limits[corev1.ResourceEphemeralStorage]
		req, rok := c.Resources.Requests[corev1.ResourceEphemeralStorage]
		if !lok || !rok || lim.IsZero() || req.Cmp(lim) > 0 {
			t.Errorf("container %s ephemeral-storage request %v limit %v", c.Name, req.String(), lim.String())
		}
		if c.Name == ContainerKaniko && lim.String() != defaultDiskLimit {
			t.Errorf("kaniko ephemeral-storage limit = %s, want the default %s", lim.String(), defaultDiskLimit)
		}
	}
	p.DiskLimit = "512Mi"
	if job, err = BuildJob(p); err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name != ContainerKaniko {
			continue
		}
		lim := c.Resources.Limits[corev1.ResourceEphemeralStorage]
		req := c.Resources.Requests[corev1.ResourceEphemeralStorage]
		if lim.String() != "512Mi" || req.Cmp(lim) > 0 {
			t.Errorf("a 512Mi disk cap rendered limit %s request %s", lim.String(), req.String())
		}
	}
	p.DiskLimit = "lots"
	if _, err := BuildJob(p); err == nil {
		t.Error("an unparsable disk limit was accepted")
	}
}

func TestBuildLimitRangeCoversEphemeralStorage(t *testing.T) {
	lr := BuildLimitRange("felis-build")
	if lr.Namespace != "felis-build" || len(lr.Spec.Limits) != 1 {
		t.Fatalf("limit range = %#v", lr)
	}
	item := lr.Spec.Limits[0]
	for _, res := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		d, dr := item.Default[res], item.DefaultRequest[res]
		if d.IsZero() || dr.IsZero() || dr.Cmp(d) > 0 {
			t.Errorf("%s default %s request %s", res, d.String(), dr.String())
		}
	}
}

func initNames(cs []corev1.Container) []string {
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	return names
}

// An explicit control namespace/port override must reach the egress rule (a
// renamed control namespace otherwise silently blocks every context fetch).
func TestBuildNetworkPolicyHonoursControlNamespaceOverride(t *testing.T) {
	np := BuildNetworkPolicy(NetPolParams{
		Namespace:         "felis-build",
		RegistryNamespace: "felis-system",
		ControlNamespace:  "control-plane",
		APIPort:           9081,
	})
	if !egressAllowsNamespace(np, "control-plane") {
		t.Error("egress must allow the overridden control namespace")
	}
	if !egressAllowsPort(np, 9081) {
		t.Error("egress must allow the overridden api port")
	}
}

// With no package-source CIDRs configured, there must be zero IPBlock egress —
// the most locked-down default (no open internet).
func TestBuildNetworkPolicyDefaultsToNoInternet(t *testing.T) {
	np := BuildNetworkPolicy(NetPolParams{
		Namespace:         "felis-build",
		RegistryNamespace: "felis-system",
	})
	for i, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				t.Errorf("egress rule %d has an IPBlock but no package sources were configured", i)
			}
		}
	}
}

func TestBuildNetworkPolicyAllowsConfiguredPackageMirrors(t *testing.T) {
	cidr := "192.0.2.0/24"
	np := BuildNetworkPolicy(NetPolParams{
		Namespace:          "felis-build",
		RegistryNamespace:  "felis-system",
		PackageSourceCIDRs: []string{cidr},
	})
	found := false
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == cidr {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("configured package-mirror CIDR %q not present in egress", cidr)
	}
}

// The build SA is the most dangerous identity if mis-scoped: it must be bare —
// no secrets, token automount disabled, and (by having no Role anywhere) no API
// rights. We assert the spec-level properties the renderer controls.
func TestBuildServiceAccountIsBare(t *testing.T) {
	sa := BuildServiceAccount("felis-build", "felis-build")
	if sa.Namespace != "felis-build" {
		t.Errorf("SA namespace = %q, want felis-build", sa.Namespace)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("SA must disable token automounting")
	}
	if len(sa.Secrets) != 0 {
		t.Errorf("SA must carry no secrets, got %d", len(sa.Secrets))
	}
	if len(sa.ImagePullSecrets) != 0 {
		t.Errorf("SA must carry no image-pull secrets, got %d", len(sa.ImagePullSecrets))
	}
}

// An invalid resource limit must surface as an error rather than render a Job
// with no limits.
func TestBuildJobRejectsBadResourceLimit(t *testing.T) {
	p := sampleJobParams()
	p.CPULimit = "not-a-quantity"
	if _, err := BuildJob(p); err == nil {
		t.Error("expected error for an unparseable CPU limit")
	}
}

// ---- helpers ----

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// argPairPresent reports whether flag is immediately followed by val (the
// `--exit-code 1` two-token form).
func argPairPresent(args []string, flag, val string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func hasPolicyType(np *networkingv1.NetworkPolicy, t networkingv1.PolicyType) bool {
	for _, pt := range np.Spec.PolicyTypes {
		if pt == t {
			return true
		}
	}
	return false
}

func egressAllowsNamespace(np *networkingv1.NetworkPolicy, ns string) bool {
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil &&
				peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == ns {
				return true
			}
		}
	}
	return false
}

func egressAllowsPort(np *networkingv1.NetworkPolicy, port int32) bool {
	for _, rule := range np.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == port {
				return true
			}
		}
	}
	return false
}

// guard against accidental shorthand: the test image ref must be host-qualified.
func TestSampleRefIsHostQualified(t *testing.T) {
	if !strings.Contains(sampleJobParams().ImageRef, "/") {
		t.Fatal("sample image ref must be host-qualified")
	}
}
