package platform

import (
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/operator"
	"felis.lolicon.best/internal/restore"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// podAddr is the address the source pods of these checks dial from: one in k3s's
// default pod network, which an ipBlock peer has to exclude to keep them out.
var podAddr = netip.MustParseAddr("10.42.0.17")

// admits reports whether np lets a pod with podLabels in namespace ns, dialing
// from podAddr, open a TCP connection to port on the pods np selects. It follows
// the NetworkPolicy rules rather than the shapes this package happens to render:
// a policy that does not govern ingress restricts nothing; a rule with no ports
// covers every port and one with no peers every source; a port without a number
// covers every port of its protocol; a peer's selectors are full label selectors,
// matched against the pod and against its namespace's labels as Objects renders
// them (the name label only); an ipBlock admits the pod by its address. A named
// port fails the test: it resolves against a container spec this does not see.
func admits(t *testing.T, np *networkingv1.NetworkPolicy, ns string, podLabels map[string]string, port int32) bool {
	t.Helper()
	if len(np.Spec.PolicyTypes) > 0 && !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) {
		return true
	}
	selector := func(s *metav1.LabelSelector) labels.Selector {
		sel, err := metav1.LabelSelectorAsSelector(s)
		if err != nil {
			t.Fatalf("policy %s: selector %v: %v", np.Name, s, err)
		}
		return sel
	}
	prefix := func(cidr string) netip.Prefix {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			t.Fatalf("policy %s: ipBlock %q: %v", np.Name, cidr, err)
		}
		return p
	}
	for _, rule := range np.Spec.Ingress {
		portOK := len(rule.Ports) == 0
		for _, p := range rule.Ports {
			switch {
			case p.Protocol != nil && *p.Protocol != corev1.ProtocolTCP:
			case p.Port == nil:
				portOK = true
			case p.Port.Type == intstr.String:
				t.Fatalf("policy %s: named port %q, which this evaluator cannot resolve", np.Name, p.Port.StrVal)
			case p.EndPort != nil:
				portOK = portOK || (p.Port.IntVal <= port && port <= *p.EndPort)
			default:
				portOK = portOK || p.Port.IntVal == port
			}
		}
		if !portOK {
			continue
		}
		if len(rule.From) == 0 {
			return true
		}
		for _, peer := range rule.From {
			if peer.IPBlock != nil {
				in := prefix(peer.IPBlock.CIDR).Contains(podAddr)
				for _, e := range peer.IPBlock.Except {
					in = in && !prefix(e).Contains(podAddr)
				}
				if in {
					return true
				}
				continue
			}
			nsOK := peer.NamespaceSelector == nil && ns == np.Namespace
			if peer.NamespaceSelector != nil {
				nsOK = selector(peer.NamespaceSelector).Matches(labels.Set{"kubernetes.io/metadata.name": ns})
			}
			podOK := peer.PodSelector == nil || selector(peer.PodSelector).Matches(labels.Set(podLabels))
			if nsOK && podOK {
				return true
			}
		}
	}
	return false
}

// The evaluator itself, against the NetworkPolicy rules it claims to follow: the
// fence checks below mean nothing if it waves through a shape it does not read.
func TestAdmits_FollowsTheNetworkPolicyRules(t *testing.T) {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	port := func(n int32) *intstr.IntOrString { p := intstr.FromInt32(n); return &p }
	endPort := func(n int32) *int32 { return &n }
	api := map[string]string{"component": "api"}
	other := map[string]string{"component": "reaper"}
	policy := func(rules ...networkingv1.NetworkPolicyIngressRule) *networkingv1.NetworkPolicy {
		return netpol("under-test", "felis", metav1.LabelSelector{}, rules)
	}
	inFelis := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "felis"}}
	on5432 := []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: port(5432)}}

	for _, c := range []struct {
		name   string
		np     *networkingv1.NetworkPolicy
		ns     string
		labels map[string]string
		port   int32
		want   bool
	}{
		{"no rules admit nothing", policy(), "felis", api, 5432, false},
		{"a rule without peers admits every source", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432}), "minecraft", other, 5432, true},
		{"a rule without ports covers every port", policy(networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: api}}}}), "felis", api, 9999, true},
		{"a port without a number covers every TCP port", policy(networkingv1.NetworkPolicyIngressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp}}}), "felis", api, 9999, true},
		{"a UDP port admits no TCP connection", policy(networkingv1.NetworkPolicyIngressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: port(5432)}}}), "felis", api, 5432, false},
		{"a port range covers its inside", policy(networkingv1.NetworkPolicyIngressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Port: port(5000), EndPort: endPort(6000)}}}), "felis", api, 5432, true},
		{"a port range stops at its end", policy(networkingv1.NetworkPolicyIngressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Port: port(5000), EndPort: endPort(5431)}}}), "felis", api, 5432, false},
		{"another port", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: api}}}}), "felis", api, 5433, false},
		{"an ipBlock over the pod network admits a pod", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}}), "minecraft", other, 5432, true},
		{"an ipBlock excepting the pod's address", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"10.42.0.0/16"}}}}}), "minecraft", other, 5432, false},
		{"an ipBlock elsewhere", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "192.168.0.0/16"}}}}), "minecraft", other, 5432, false},
		{"a pod selector alone stays in the policy's namespace", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: api}}}}), "minecraft", api, 5432, false},
		{"an empty namespace selector spans every namespace", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}, PodSelector: &metav1.LabelSelector{MatchLabels: api}}}}), "minecraft", api, 5432, true},
		{"a namespace selector without a pod selector admits the whole namespace", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: inFelis}}}), "felis", other, 5432, true},
		{"a namespace selector keeps other namespaces out", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: inFelis}}}), "minecraft", api, 5432, false},
		{"matchExpressions narrow a selector", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "component", Operator: metav1.LabelSelectorOpIn, Values: []string{"api"}}}}}}}), "felis", other, 5432, false},
		{"matchExpressions admit what they match", policy(networkingv1.NetworkPolicyIngressRule{Ports: on5432,
			From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "component", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"api"}}}}}}}), "felis", other, 5432, true},
		{"a policy that governs only egress restricts no ingress", &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "egress-only", Namespace: "felis"},
			Spec:       networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}},
		}, "minecraft", other, 5432, true},
	} {
		if got := admits(t, c.np, c.ns, c.labels, c.port); got != c.want {
			t.Errorf("%s: admitted = %v, want %v", c.name, got, c.want)
		}
	}
}

func podTemplateOf(t *testing.T, objs []Object, kind, name string) corev1.PodTemplateSpec {
	t.Helper()
	for _, o := range objs {
		switch v := o.(type) {
		case *appsv1.Deployment:
			if kind == "Deployment" && v.Name == name {
				return v.Spec.Template
			}
		case *batchv1.CronJob:
			if kind == "CronJob" && v.Name == name {
				return v.Spec.JobTemplate.Spec.Template
			}
		}
	}
	t.Fatalf("%s %s not rendered", kind, name)
	return corev1.PodTemplateSpec{}
}

// TestPostgresIngress_AdmitsExactlyTheDatabaseClients checks the fence against
// the labels the pods actually carry: the three workloads that open the
// database must get through and every other pod the platform runs must not.
// A selector typo would either cut felis-api off from its database or leave a
// game server able to try passwords against it.
func TestPostgresIngress_AdmitsExactlyTheDatabaseClients(t *testing.T) {
	p := reaperParams().withDefaults()
	objs := Objects(p)
	np := PostgresIngressPolicy(p)

	db := podTemplateOf(t, objs, "Deployment", PostgresName)
	if !labels.SelectorFromSet(np.Spec.PodSelector.MatchLabels).Matches(labels.Set(db.Labels)) {
		t.Fatalf("the policy selects %v, which is not the database pod %v", np.Spec.PodSelector.MatchLabels, db.Labels)
	}

	backup, err := backupjob.BackupJob(backupjob.JobParams{
		Server: "survival", WorldPVC: "world-survival-0", BackupPVC: "felis-backups",
		Namespace: p.MinecraftNamespace, Image: "felis:test", ConfigSecret: "felis-config",
		ConfigMount: "/etc/felis", BackupRoot: "/backups", WorldsRoot: "/world",
		Deadline: time.Minute, CPULimit: "1", MemLimit: "1Gi",
	})
	if err != nil {
		t.Fatal(err)
	}
	rst, err := restore.RestoreJob(restore.JobParams{
		Server: "survival", WorldPVC: "world-survival-0", BackupPVC: "felis-backups",
		BackupRef: "/backups/survival/x.tar.gz", ArchiveStore: "tarLocal",
		Namespace: p.MinecraftNamespace, Image: "felis:test", BackupRoot: "/backups",
		WorldsRoot: "/world", Deadline: time.Minute, CPULimit: "1", MemLimit: "1Gi",
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := fileedit.FilesJob(fileedit.JobParams{
		Server: "survival", OpID: "deadbeefcafe0001", Op: fileedit.OpRead, Path: "server.properties",
		WorldPVC: "world-survival-0", Namespace: p.MinecraftNamespace, Image: "felis:test",
		WorldsRoot: "/data", Deadline: time.Minute, CPULimit: "500m", MemLimit: "256Mi",
	})
	if err != nil {
		t.Fatal(err)
	}
	game := map[string]string{
		v1alpha1.LabelManagedBy: operator.ManagedByValue,
		v1alpha1.LabelComponent: operator.ComponentValue,
	}

	for _, c := range []struct {
		who    string
		ns     string
		labels map[string]string
		want   bool
	}{
		{"felis-api", p.ControlNamespace, podTemplateOf(t, objs, "Deployment", "felis-api").Labels, true},
		{"the reaper", p.MinecraftNamespace, podTemplateOf(t, objs, "CronJob", SAReaper).Labels, true},
		{"a world-backup Job", p.MinecraftNamespace, backup.Spec.Template.Labels, true},

		{"felis-operator", p.ControlNamespace, podTemplateOf(t, objs, "Deployment", "felis-operator").Labels, false},
		{"the registry", p.RegistryNamespace, podTemplateOf(t, objs, "Deployment", registryName).Labels, false},
		{"a restore Job", p.MinecraftNamespace, rst.Spec.Template.Labels, false},
		{"a files Job", p.MinecraftNamespace, files.Spec.Template.Labels, false},
		{"a game server", p.MinecraftNamespace, game, false},
		{"a build Job", p.BuildNamespace, map[string]string{}, false},
		// felis-api's labels in the wrong namespace: a pod anyone can create in
		// the Minecraft namespace must not borrow them.
		{"api labels outside the control namespace", p.MinecraftNamespace, podTemplateOf(t, objs, "Deployment", "felis-api").Labels, false},
	} {
		if got := admits(t, np, c.ns, c.labels, PostgresPort); got != c.want {
			t.Errorf("%s (%s, %v): admitted = %v, want %v", c.who, c.ns, c.labels, got, c.want)
		}
	}
	if admits(t, np, p.ControlNamespace, podTemplateOf(t, objs, "Deployment", "felis-api").Labels, PostgresPort+1) {
		t.Error("the policy admits felis-api on a port other than the database's")
	}
}

// hbaMethod returns the auth method pg_hba.conf picks for a connection:
// PostgreSQL takes the first line whose type, user and address match.
func hbaMethod(t *testing.T, hba, connType, user, addr string) string {
	t.Helper()
	for _, line := range strings.Split(hba, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if f[0] != connType || (f[2] != "all" && f[2] != user) {
			continue
		}
		if connType == "local" {
			return f[3]
		}
		prefix, err := netip.ParsePrefix(f[3])
		if err != nil {
			t.Fatalf("pg_hba.conf line %q: %v", line, err)
		}
		if prefix.Contains(netip.MustParseAddr(addr)) {
			return f[4]
		}
	}
	return "(no line: rejected)"
}

// TestPostgresHBA_SuperuserOnlyOverTheSocket walks the rendered pg_hba.conf the
// way the server does. The superuser's password is a random value nobody keeps,
// so TCP must refuse the role outright; a felis role must be asked for its
// password from every address family, never trusted.
func TestPostgresHBA_SuperuserOnlyOverTheSocket(t *testing.T) {
	hba := postgresHBAConfig(testParams().withDefaults()).Data["pg_hba.conf"]
	for _, c := range []struct{ connType, user, addr, want string }{
		{"local", "postgres", "", "trust"},
		{"local", "felis", "", "trust"},
		{"host", "postgres", "10.42.0.7", "reject"},
		{"host", "postgres", "127.0.0.1", "reject"},
		{"host", "postgres", "fd00::7", "reject"},
		{"host", "felis", "10.42.0.7", "scram-sha-256"},
		{"host", "felis", "127.0.0.1", "scram-sha-256"},
		{"host", "felis", "fd00::7", "scram-sha-256"},
	} {
		if got := hbaMethod(t, hba, c.connType, c.user, c.addr); got != c.want {
			t.Errorf("%s %s from %q: %s, want %s", c.connType, c.user, c.addr, got, c.want)
		}
	}
}

// TestPostgresDeployment_RunsTheImageSafely pins what keeps the data directory
// intact and the database off the network, against the official image's own
// layout (VOLUME /var/lib/postgresql, the postgres account uid 999).
func TestPostgresDeployment_RunsTheImageSafely(t *testing.T) {
	p := testParams().withDefaults()
	objs := PostgresObjects(p)
	var dep *appsv1.Deployment
	var svc *corev1.Service
	var hba *corev1.ConfigMap
	for _, o := range objs {
		switch v := o.(type) {
		case *appsv1.Deployment:
			dep = v
		case *corev1.Service:
			svc = v
		case *corev1.ConfigMap:
			hba = v
		}
	}
	if dep == nil || svc == nil || hba == nil {
		t.Fatalf("PostgresObjects is missing the Deployment, Service or ConfigMap: %v", objs)
	}

	// Two servers on one data directory corrupt it.
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 || dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("replicas %v strategy %q: want exactly one pod, stopped before its replacement starts", dep.Spec.Replicas, dep.Spec.Strategy.Type)
	}
	pod := dep.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("want one container, got %d", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Name != PostgresContainer || c.Image != p.PostgresImage {
		t.Errorf("container %q runs %q, want %q running %q", c.Name, c.Image, PostgresContainer, p.PostgresImage)
	}
	if sc := pod.SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 999 || sc.RunAsGroup == nil || *sc.RunAsGroup != 999 {
		t.Errorf("pod runs as %+v, want the image's postgres account 999:999 (the installer owns the data directory to it)", sc)
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("the database container's root filesystem is writable")
	}

	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
		if _, ok := volumes[m.Name]; !ok {
			t.Errorf("mount %s names volume %q, which the pod does not declare", m.MountPath, m.Name)
		}
	}
	data, ok := mounts["/var/lib/postgresql"]
	if !ok {
		t.Fatal("nothing is mounted at the image's VOLUME /var/lib/postgresql: the cluster would live in the container")
	}
	hp := volumes[data.Name].HostPath
	if hp == nil || hp.Path != PostgresDataHostPath || hp.Type == nil || *hp.Type != corev1.HostPathDirectory {
		t.Errorf("data volume = %+v, want hostPath %s of type Directory", volumes[data.Name].VolumeSource, PostgresDataHostPath)
	}
	for _, path := range []string{PostgresSocketDir, "/tmp", "/dev/shm"} {
		if _, ok := mounts[path]; !ok {
			t.Errorf("nothing writable at %s under a read-only root", path)
		}
	}

	// hba_file must name a file the ConfigMap mount actually provides.
	var hbaFile string
	for i, a := range c.Args {
		if a == "-c" && i+1 < len(c.Args) && strings.HasPrefix(c.Args[i+1], "hba_file=") {
			hbaFile = strings.TrimPrefix(c.Args[i+1], "hba_file=")
		}
	}
	if len(c.Args) == 0 || c.Args[0] != "postgres" {
		t.Errorf("args %q: the entrypoint initialises the cluster only when the first argument is postgres", c.Args)
	}
	dir, key := hbaFile[:max(strings.LastIndex(hbaFile, "/"), 0)], hbaFile[strings.LastIndex(hbaFile, "/")+1:]
	m, ok := mounts[dir]
	if !ok || volumes[m.Name].ConfigMap == nil || volumes[m.Name].ConfigMap.Name != hba.Name {
		t.Errorf("hba_file %q is not in the %s ConfigMap's mount", hbaFile, hba.Name)
	} else if _, ok := hba.Data[key]; !ok {
		t.Errorf("hba_file %q: the ConfigMap has no key %q", hbaFile, key)
	}

	// The host's tools get the port on loopback only.
	if len(c.Ports) != 1 || c.Ports[0].HostPort != PostgresHostPort || c.Ports[0].HostIP != "127.0.0.1" || c.Ports[0].ContainerPort != PostgresPort {
		t.Errorf("ports = %+v, want %d published on 127.0.0.1:%d and nowhere else", c.Ports, PostgresPort, PostgresHostPort)
	}
	// The Service reaches that port on that pod.
	if !labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(dep.Spec.Template.Labels)) {
		t.Errorf("Service selector %v misses the pod %v", svc.Spec.Selector, dep.Spec.Template.Labels)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != PostgresPort || svc.Spec.Ports[0].TargetPort.StrVal != c.Ports[0].Name {
		t.Errorf("Service ports %+v do not lead to the container's %q port", svc.Spec.Ports, c.Ports[0].Name)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type %q: the database is for the cluster only", svc.Spec.Type)
	}

	// The superuser password is read by initdb alone; a restart must not need it.
	var pw *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "POSTGRES_PASSWORD" {
			pw = &c.Env[i]
		}
	}
	if pw == nil || pw.ValueFrom == nil || pw.ValueFrom.SecretKeyRef == nil ||
		pw.ValueFrom.SecretKeyRef.Name != PostgresSuperuserSecret || pw.ValueFrom.SecretKeyRef.Optional == nil || !*pw.ValueFrom.SecretKeyRef.Optional {
		t.Errorf("POSTGRES_PASSWORD = %+v, want an optional reference to Secret %s", pw, PostgresSuperuserSecret)
	}

	// Every probe goes over TCP: during initialisation the entrypoint's
	// temporary server answers on the socket only, and must not count as up.
	for name, pr := range map[string]*corev1.Probe{"startup": c.StartupProbe, "readiness": c.ReadinessProbe, "liveness": c.LivenessProbe} {
		if pr == nil || pr.Exec == nil {
			t.Errorf("%s probe missing", name)
			continue
		}
		cmd := strings.Join(pr.Exec.Command, " ")
		if !strings.HasPrefix(cmd, "pg_isready") || !strings.Contains(cmd, "-h 127.0.0.1") {
			t.Errorf("%s probe %q does not ping the TCP listener", name, cmd)
		}
	}
	if c.StartupProbe != nil && c.StartupProbe.PeriodSeconds*c.StartupProbe.FailureThreshold < 300 {
		t.Errorf("startup allows %ds: crash recovery or initdb on a slow disk gets killed mid-way",
			c.StartupProbe.PeriodSeconds*c.StartupProbe.FailureThreshold)
	}

	// The fast shutdown's own timeout must end before the kubelet's SIGKILL.
	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || c.Lifecycle.PreStop.Exec == nil {
		t.Fatal("no preStop fast shutdown")
	}
	stop := strings.Join(c.Lifecycle.PreStop.Exec.Command, " ")
	tm := regexp.MustCompile(`pg_ctl .*-m fast .*-t (\d+) stop`).FindStringSubmatch(stop)
	if tm == nil {
		t.Fatalf("preStop %q is not a timed fast pg_ctl stop", stop)
	}
	timeout, _ := strconv.Atoi(tm[1])
	if pod.TerminationGracePeriodSeconds == nil || int64(timeout) >= *pod.TerminationGracePeriodSeconds {
		t.Errorf("pg_ctl waits %ds but the grace period is %v", timeout, pod.TerminationGracePeriodSeconds)
	}
}

// TestObjects_CarryThePostgresObjectsUnchanged: bootstrap applies
// PostgresObjects first and the full bundle afterwards. An object that renders
// differently in the two would flip back and forth on every install, and the
// Deployment would restart the database each time.
func TestObjects_CarryThePostgresObjectsUnchanged(t *testing.T) {
	p := reaperParams()
	full := map[string]Object{}
	for _, o := range Objects(p) {
		full[o.GetObjectKind().GroupVersionKind().Kind+"/"+o.GetNamespace()+"/"+o.GetName()] = o
	}
	for _, o := range PostgresObjects(p) {
		key := o.GetObjectKind().GroupVersionKind().Kind + "/" + o.GetNamespace() + "/" + o.GetName()
		got, ok := full[key]
		if !ok {
			t.Errorf("%s is in PostgresObjects but not in Objects", key)
			continue
		}
		if !reflect.DeepEqual(got, o) {
			t.Errorf("%s renders differently in Objects and PostgresObjects", key)
		}
	}
}
