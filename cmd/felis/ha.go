package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// felis ha is the host side of controller high availability (internal/platform ha.go):
//
//	felis ha leader [-or-unreachable]   exit 0 on the controller that runs the host's
//	                                     cluster-wide jobs (the timers' ExecCondition)
//	felis ha migrate-storage ...        move the control plane's volumes to the
//	                                     replicated class (the installer, enabling HA)
//	felis ha copy-volume -from -to      the copy one migration Job runs
func cmdHA(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: felis ha leader|migrate-storage|copy-volume [flags]")
		return 2
	}
	switch args[0] {
	case "leader":
		return cmdHALeader(args[1:], stdout, stderr)
	case "migrate-storage":
		return cmdHAMigrateStorage(args[1:], stdout, stderr)
	case "copy-volume":
		return cmdHACopyVolume(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "felis ha: unknown command %q (leader, migrate-storage, copy-volume)\n", args[0])
	return 2
}

// ---- leader ----

// The host leader is the controller running the PostgreSQL pod: the host's own tools
// reach the database on that node's loopback port only (platform.PostgresHostPort), and
// with controller high availability the control plane's other volumes are attached there
// too. On a single controller it is that controller, always. When the database's pod has
// no live node (it is being moved), the first online controller by name stands in, so the
// watchdog still runs somewhere to report it.
func hostLeader(ctx context.Context, cl client.Reader, controlNS string) (string, error) {
	var nodes corev1.NodeList
	if err := cl.List(ctx, &nodes); err != nil {
		return "", err
	}
	online := map[string]bool{}
	var candidates []string
	anyRole := false
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Labels[placement.LabelRole] != "" {
			anyRole = true
		}
		online[n.Name] = placement.Online(n)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		// A single-node install labels no roles: its one node is the controller.
		if online[n.Name] && (n.Labels[placement.LabelRole] == placement.RoleController || !anyRole) {
			candidates = append(candidates, n.Name)
		}
	}
	var pods corev1.PodList
	if err := cl.List(ctx, &pods, client.InNamespace(controlNS), client.MatchingLabels{platform.LabelComponent: platform.ComponentPostgres}); err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && p.Spec.NodeName != "" && online[p.Spec.NodeName] {
			return p.Spec.NodeName, nil
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", errors.New("no online controller")
	}
	return candidates[0], nil
}

// selfNodeName is this host's Kubernetes node name: the one the installer pinned in the
// k3s settings (write_k3s_config), else the hostname k3s defaults to.
func selfNodeName(dropin string) string {
	if f, err := os.Open(dropin); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "node-name:"); ok {
				if name := strings.Trim(strings.TrimSpace(v), `"'`); name != "" {
					return name
				}
			}
		}
	}
	host, _ := os.Hostname()
	return strings.ToLower(host)
}

const k3sFelisDropin = "/etc/rancher/k3s/config.yaml.d/50-felis.yaml"

func cmdHALeader(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ha leader", flag.ContinueOnError)
	fs.SetOutput(stderr)
	orUnreachable := fs.Bool("or-unreachable", false, "also succeed when the cluster cannot be asked (the watchdog must run then, to report it)")
	quiet := fs.Bool("q", false, "print nothing")
	controlNS := fs.String("control-namespace", platform.DefaultControlNamespace, "namespace of the PostgreSQL pod")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cl, err := buildSystemServerClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var leader string
	if err == nil {
		leader, err = hostLeader(ctx, cl, *controlNS)
	}
	return haLeaderVerdict(selfNodeName(k3sFelisDropin), leader, err, *orUnreachable, *quiet, stdout)
}

// haLeaderVerdict is the exit status of felis ha leader.
func haLeaderVerdict(self, leader string, err error, orUnreachable, quiet bool, stdout io.Writer) int {
	say := func(format string, a ...any) {
		if !quiet {
			fmt.Fprintf(stdout, format+"\n", a...)
		}
	}
	switch {
	case err != nil && orUnreachable:
		say("felis ha: the cluster cannot be asked (%v); acting here", err)
		return 0
	case err != nil:
		say("felis ha: the cluster cannot be asked (%v); leaving it to the leader", err)
		return 1
	case leader == self:
		say("felis ha: %s is the host leader", self)
		return 0
	}
	say("felis ha: %s is the host leader, not %s", leader, self)
	return 1
}

// ---- copy-volume ----

func cmdHACopyVolume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ha copy-volume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "/source", "the volume being moved, mounted read-only")
	to := fs.String("to", "/target", "its replicated copy, emptied first")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	started := time.Now()
	st, err := copyTree(*from, *to, copyOptions{owner: true, replace: true})
	if err != nil {
		fmt.Fprintf(stderr, "felis ha copy-volume: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "felis ha copy-volume: copied %d files (%d bytes) in %s\n", st.files, st.bytes, time.Since(started).Round(time.Millisecond))
	return 0
}

// ---- migrate-storage ----

// haVolume is one control-plane volume to move: from a claim, or (the database) from a
// host directory, into its replicated copy.
type haVolume struct {
	Namespace string `json:"namespace"`
	From      string `json:"from"`     // the source claim
	HostPath  string `json:"hostPath"` // or the source host directory
	To        string `json:"to"`
	Size      string `json:"size"`
	// Server is the system server whose world this is, which then names the copy.
	Server string `json:"server,omitempty"`
}

type haMigration struct {
	cl        client.Client
	controlNS string
	mcNS      string
	backupPVC string
	class     string
	node      string // where the sources are: the home controller
	image     string
	dbSize    string
	// dbPath is the database's host directory (platform.PostgresDataHostPath).
	dbPath   string
	stateDir string
	poll     time.Duration
	timeout  time.Duration
	out      io.Writer
}

type haState struct {
	Copied  map[string]bool   `json:"copied"`
	Desired map[string]string `json:"desired"` // system servers' desired state before the move
}

func (m *haMigration) loadState() (*haState, error) {
	st := &haState{Copied: map[string]bool{}, Desired: map[string]string{}}
	raw, err := os.ReadFile(filepath.Join(m.stateDir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, err
	}
	if st.Copied == nil {
		st.Copied = map[string]bool{}
	}
	if st.Desired == nil {
		st.Desired = map[string]string{}
	}
	return st, nil
}

func (m *haMigration) saveState(st *haState) error {
	if err := os.MkdirAll(m.stateDir, 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	tmp := filepath.Join(m.stateDir, "state.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.stateDir, "state.json"))
}

// plan lists the volumes to move: the database's directory, the uploads, registry and
// archive claims, and each system server's world.
func (m *haMigration) plan(ctx context.Context) ([]haVolume, error) {
	var vols []haVolume
	// A fresh install has no database directory yet: its replicated volume starts empty.
	if fi, err := os.Stat(m.dbPath); err == nil && fi.IsDir() {
		vols = append(vols, haVolume{Namespace: m.controlNS, HostPath: m.dbPath, To: platform.PostgresHAClaim, Size: m.dbSize})
	} else {
		fmt.Fprintf(m.out, "felis ha: %s does not exist; the database starts on an empty replicated volume\n", m.dbPath)
	}
	for _, v := range []haVolume{
		{Namespace: m.controlNS, From: platform.UploadsPVCName},
		{Namespace: m.controlNS, From: "registry"},
		{Namespace: m.mcNS, From: m.backupPVC},
	} {
		if v.From == "" {
			continue
		}
		v.To = platform.HAClaimName(true, v.From)
		vols = append(vols, v)
	}
	var servers v1alpha1.MinecraftServerList
	if err := m.cl.List(ctx, &servers, client.InNamespace(m.mcNS)); err != nil {
		return nil, err
	}
	for i := range servers.Items {
		s := &servers.Items[i]
		if s.Labels[v1alpha1.LabelSystemRole] == "" {
			continue
		}
		vols = append(vols, haVolume{Namespace: m.mcNS, From: s.WorldPVC(), To: "world-" + s.Name + "-ha", Server: s.Name})
	}
	// A source that does not exist has nothing to move (a claim never bound, a
	// feature never used); its copy is simply created empty by the HA bundle.
	var out []haVolume
	for _, v := range vols {
		if v.HostPath != "" {
			out = append(out, v)
			continue
		}
		var pvc corev1.PersistentVolumeClaim
		err := m.cl.Get(ctx, types.NamespacedName{Namespace: v.Namespace, Name: v.From}, &pvc)
		if apierrors.IsNotFound(err) {
			fmt.Fprintf(m.out, "felis ha: %s/%s does not exist; nothing to move\n", v.Namespace, v.From)
			continue
		}
		if err != nil {
			return nil, err
		}
		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == m.class {
			continue // already replicated
		}
		if v.Size == "" {
			q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			v.Size = q.String()
		}
		out = append(out, v)
	}
	return out, nil
}

// Control-plane workloads stopped for the move; the HA bundle starts them again.
var haQuiesce = []struct{ ns, name string }{
	{"control", platform.SAOperator},
	{"control", platform.SAAPI},
	{"control", "registry"},
	{"minecraft", platform.ArchiveName},
	{"control", platform.PostgresName},
}

func (m *haMigration) ns(which string) string {
	if which == "minecraft" {
		return m.mcNS
	}
	return m.controlNS
}

// quiesce stops what writes the volumes: the system servers through the operator first
// (remembering their desired state), then the control plane, and the reaper.
func (m *haMigration) quiesce(ctx context.Context, st *haState) error {
	var servers v1alpha1.MinecraftServerList
	if err := m.cl.List(ctx, &servers, client.InNamespace(m.mcNS)); err != nil {
		return err
	}
	for i := range servers.Items {
		s := &servers.Items[i]
		if s.Labels[v1alpha1.LabelSystemRole] == "" || s.Spec.DesiredState == v1alpha1.DesiredStopped {
			continue
		}
		if _, ok := st.Desired[s.Name]; !ok {
			st.Desired[s.Name] = string(s.Spec.DesiredState)
			if err := m.saveState(st); err != nil {
				return err
			}
		}
		patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"desiredState":"Stopped"}}`))
		if err := m.cl.Patch(ctx, s, patch); err != nil {
			return err
		}
	}
	if err := m.waitGone(ctx, m.mcNS, map[string]string{v1alpha1.LabelComponent: "server"}, func(p corev1.Pod) bool {
		return p.Labels[v1alpha1.LabelSystemRole] != "" || isSystemServerNamed(servers.Items, p.Labels[v1alpha1.LabelServer])
	}); err != nil {
		return fmt.Errorf("stop the system servers: %w", err)
	}
	for _, q := range haQuiesce {
		var d appsv1.Deployment
		err := m.cl.Get(ctx, types.NamespacedName{Namespace: m.ns(q.ns), Name: q.name}, &d)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
			continue
		}
		if err := m.cl.Patch(ctx, &d, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":0}}`))); err != nil {
			return err
		}
	}
	var crons batchv1.CronJobList
	if err := m.cl.List(ctx, &crons, client.InNamespace(m.mcNS)); err != nil {
		return err
	}
	for i := range crons.Items {
		if err := m.cl.Patch(ctx, &crons.Items[i], client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`))); err != nil {
			return err
		}
	}
	for _, q := range haQuiesce {
		if err := m.waitGone(ctx, m.ns(q.ns), nil, func(p corev1.Pod) bool { return strings.HasPrefix(p.Name, q.name+"-") }); err != nil {
			return fmt.Errorf("stop %s: %w", q.name, err)
		}
	}
	return nil
}

func isSystemServerNamed(servers []v1alpha1.MinecraftServer, name string) bool {
	for _, s := range servers {
		if s.Name == name && s.Labels[v1alpha1.LabelSystemRole] != "" {
			return true
		}
	}
	return false
}

// waitGone waits until no pod in ns matching labels and match is left.
func (m *haMigration) waitGone(ctx context.Context, ns string, labels map[string]string, match func(corev1.Pod) bool) error {
	deadline := time.Now().Add(m.timeout)
	for {
		var pods corev1.PodList
		opts := []client.ListOption{client.InNamespace(ns)}
		if labels != nil {
			opts = append(opts, client.MatchingLabels(labels))
		}
		if err := m.cl.List(ctx, &pods, opts...); err != nil {
			return err
		}
		left := 0
		for _, p := range pods.Items {
			if match(p) {
				left++
			}
		}
		if left == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d pod(s) still running after %s", left, m.timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.poll):
		}
	}
}

// copyVolume makes v's replicated claim and runs the Job copying v into it, on the node
// holding the source, as root keeping every owner.
func (m *haMigration) copyVolume(ctx context.Context, v haVolume) error {
	size, err := resource.ParseQuantity(v.Size)
	if err != nil || size.IsZero() {
		size = resource.MustParse("10Gi")
	}
	class := m.class
	target := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: v.To, Namespace: v.Namespace, Labels: map[string]string{haMigratedLabel: "true"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if v.Server != "" {
		target.Labels[v1alpha1.LabelServer] = v.Server
	}
	if err := m.cl.Create(ctx, target); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	job := haCopyJob(v, m.node, m.image)
	_ = m.cl.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: job.Namespace}}, client.PropagationPolicy(metav1.DeletePropagationForeground))
	if err := m.waitJobGone(ctx, job); err != nil {
		return err
	}
	if err := m.cl.Create(ctx, job); err != nil {
		return err
	}
	deadline := time.Now().Add(m.timeout)
	for {
		var j batchv1.Job
		if err := m.cl.Get(ctx, client.ObjectKeyFromObject(job), &j); err != nil {
			return err
		}
		for _, c := range j.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			switch c.Type {
			case batchv1.JobComplete:
				return nil
			case batchv1.JobFailed:
				return fmt.Errorf("the copy of %s failed (kubectl -n %s logs job/%s)", v.To, v.Namespace, job.Name)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the copy of %s did not finish within %s", v.To, m.timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.poll):
		}
	}
}

func (m *haMigration) waitJobGone(ctx context.Context, job *batchv1.Job) error {
	deadline := time.Now().Add(m.timeout)
	for {
		err := m.cl.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the previous copy Job %s is still there", job.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.poll):
		}
	}
}

const haMigratedLabel = "felis.lolicon.best/ha-copy"

// haCopyJob is the Job copying one volume: pinned to the node holding the source, which
// it mounts read-only (a claim, or the database's host directory), with the replicated
// copy read-write; root with only what keeping owners takes.
func haCopyJob(v haVolume, node, image string) *batchv1.Job {
	source := corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: v.From, ReadOnly: true}}
	if v.HostPath != "" {
		dir := corev1.HostPathDirectory
		source = corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: v.HostPath, Type: &dir}}
	}
	backoff, root := int32(0), int64(0)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ha-copy-" + v.To, Namespace: v.Namespace, Labels: map[string]string{haMigratedLabel: "true"}},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{haMigratedLabel: "true"}},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					NodeSelector:                 map[string]string{"kubernetes.io/hostname": node},
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              &corev1.PodSecurityContext{RunAsUser: &root, RunAsGroup: &root, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name:    "copy",
						Image:   image,
						Command: []string{"/usr/local/bin/felis", "ha", "copy-volume", "-from", "/source", "-to", "/target"},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							ReadOnlyRootFilesystem:   boolPtr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER"}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "source", MountPath: "/source", ReadOnly: true}, {Name: "target", MountPath: "/target"}},
					}},
					Volumes: []corev1.Volume{
						{Name: "source", VolumeSource: source},
						{Name: "target", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: v.To}}},
					},
				},
			},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

// pointServers moves each system server onto its replicated world and gives it back the
// desired state it had before the move. The operator, started again by the HA bundle,
// creates its pods on any controller.
func (m *haMigration) pointServers(ctx context.Context, vols []haVolume, st *haState) error {
	for _, v := range vols {
		if v.Server == "" {
			continue
		}
		var s v1alpha1.MinecraftServer
		if err := m.cl.Get(ctx, types.NamespacedName{Namespace: m.mcNS, Name: v.Server}, &s); err != nil {
			return err
		}
		before := s.DeepCopy()
		s.Spec.Storage.ClaimName = v.To
		s.Spec.Storage.StorageClassName = m.class
		if d := st.Desired[s.Name]; d != "" {
			s.Spec.DesiredState = v1alpha1.DesiredState(d)
		}
		if err := m.cl.Patch(ctx, &s, client.MergeFrom(before)); err != nil {
			return err
		}
	}
	return nil
}

// run moves every volume not moved yet. It is resumable: a copy that stopped is done
// again from an empty target, and a finished one is never repeated.
func (m *haMigration) run(ctx context.Context) error {
	st, err := m.loadState()
	if err != nil {
		return err
	}
	vols, err := m.plan(ctx)
	if err != nil {
		return err
	}
	var todo []haVolume
	for _, v := range vols {
		if !st.Copied[v.Namespace+"/"+v.To] {
			todo = append(todo, v)
		}
	}
	if len(todo) > 0 {
		fmt.Fprintf(m.out, "felis ha: stopping the control plane to move %d volume(s) to %s\n", len(todo), m.class)
		if err := m.quiesce(ctx, st); err != nil {
			return err
		}
	}
	for _, v := range todo {
		from := v.From
		if v.HostPath != "" {
			from = v.HostPath
		}
		fmt.Fprintf(m.out, "felis ha: copying %s into %s/%s\n", from, v.Namespace, v.To)
		if err := m.copyVolume(ctx, v); err != nil {
			return err
		}
		st.Copied[v.Namespace+"/"+v.To] = true
		if err := m.saveState(st); err != nil {
			return err
		}
	}
	if err := m.pointServers(ctx, vols, st); err != nil {
		return err
	}
	fmt.Fprintln(m.out, "felis ha: the control plane's volumes are replicated; the originals are kept")
	return nil
}

func cmdHAMigrateStorage(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ha migrate-storage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	m := &haMigration{out: stdout, poll: 3 * time.Second, timeout: 2 * time.Hour}
	fs.StringVar(&m.controlNS, "control-namespace", platform.DefaultControlNamespace, "the control plane's namespace")
	fs.StringVar(&m.mcNS, "minecraft-namespace", platform.DefaultMinecraftNamespace, "the servers' namespace")
	fs.StringVar(&m.backupPVC, "backup-pvc", "felis-backups", "the world-archive claim")
	fs.StringVar(&m.class, "class", platform.DefaultReplicatedStorageClass, "the replicated StorageClass")
	fs.StringVar(&m.node, "node", "", "the node holding the volumes (this controller) (required)")
	fs.StringVar(&m.image, "image", "", "the felis image the copy Jobs run (required)")
	fs.StringVar(&m.dbSize, "postgres-storage", "10Gi", "the replicated database volume's capacity")
	fs.StringVar(&m.dbPath, "postgres-dir", platform.PostgresDataHostPath, "the database's host directory")
	fs.StringVar(&m.stateDir, "state", "/var/lib/felis/ha-migrate", "where progress is kept")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if m.node == "" || m.image == "" {
		fmt.Fprintln(stderr, "felis ha migrate-storage: -node and -image are required")
		return 2
	}
	cl, err := buildSystemServerClient()
	if err != nil {
		fmt.Fprintf(stderr, "felis ha migrate-storage: %v\n", err)
		return 1
	}
	m.cl = cl
	if err := m.run(context.Background()); err != nil {
		fmt.Fprintf(stderr, "felis ha migrate-storage: %v\n", err)
		return 1
	}
	return 0
}
