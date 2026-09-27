package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/watchdog"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// supportBundleDir is where felis support-bundle writes unless told otherwise.
const supportBundleDir = "/var/lib/felis/support"

// redacted stands in for every value the bundle leaves out.
const redacted = "<redacted>"

// minScrubLen is the shortest known secret value the bundle searches for: a
// shorter one would blank ordinary words, and every secret the installer
// generates is far longer.
const minScrubLen = 8

// hostSecretSources are the files on a Felis host that hold secrets; the
// bundle reads them only to take each value out of what it collects.
var hostSecretSources = secretSources{
	envFiles:   []string{"/etc/felis/secrets.env", defaultOffsiteEnvFile},
	valueFiles: []string{hostSMTPPasswordPath, hostUploadsS3AccessKeyPath, hostUploadsS3SecretKeyPath, defaultHeartbeatFile, "/opt/felis/velocity/forwarding.secret"},
	propsFiles: []string{"/opt/felis/velocity/plugins/felis-link/felis-link.properties"},
	tokenFiles: []string{"/var/lib/rancher/k3s/server/token", "/var/lib/rancher/k3s/server/agent-token"},
}

// cmdSupportBundle collects what someone helping with this host needs into
// one tar.gz: felis status and felis doctor, the logs of the control plane,
// the builds and the systemd units, the cluster's workloads and events, and a
// summary of the configuration. It never collects a Secret, a ConfigMap, the
// contents of a configuration file, the database or a world, and takes every
// value of the host's secret files out of what it does collect. Game server
// logs, which carry player names, addresses and chat, only with -server-logs.
func cmdSupportBundle(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("o", supportBundleDir, "directory to write the bundle to (created mode 0700 when missing)")
	logLines := fs.Int64("log-lines", 2000, "lines kept from the end of each pod log and each unit's journal")
	since := fs.Duration("since", 48*time.Hour, "how far back each unit's journal is read")
	serverLogs := fs.Bool("server-logs", false, "also collect the game servers' own logs, which carry player names, IP addresses and chat")
	unitDir := fs.String("systemd-dir", systemdUnitDir, "where the installer's systemd units are")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis support-bundle: run as root (sudo felis support-bundle): it reads root-only logs and state")
		return 1
	}
	b := hostSupportBundle(*unitDir, *logLines, *since, *serverLogs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	path, err := b.write(ctx, *outDir)
	if err != nil {
		fmt.Fprintf(stderr, "felis support-bundle: %v\n", err)
		return 1
	}
	size := int64(0)
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	fmt.Fprintf(stdout, "wrote %s (%s, mode 0600)\n", path, humanSize(size))
	fmt.Fprintln(stdout, "MANIFEST.txt inside says what it holds and what was taken out. Read it through before you send it anywhere:")
	fmt.Fprintln(stdout, "redaction finds this host's known secrets and the common ways a secret is logged, and a secret logged another way stays in.")
	if !*serverLogs {
		fmt.Fprintln(stdout, "Game server logs are left out; -server-logs adds them (player names, IP addresses, chat).")
	}
	return 0
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// shortDuration is d as Duration.String writes it, less the zero minutes and
// seconds after whole hours or minutes: 48h, and 90m as 1h30m.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// supportBundle is one collection and what it reads the host through.
type supportBundle struct {
	host    string
	now     time.Time
	unitDir string
	// w is how felis-watchdog.service runs the watchdog, wErr why it could
	// not be read (w then holds the defaults).
	w          watchdogFlags
	wErr       error
	cfg        *config.Config // nil when cfgErr
	cfgErr     error
	cl         client.Client // nil when clErr
	clErr      error
	logs       func(ctx context.Context, ns, pod, container string, previous bool) ([]byte, error)
	run        func(ctx context.Context, name string, args ...string) ([]byte, error)
	backups    func(ctx context.Context) (map[string]time.Time, error)
	doctor     func(ctx context.Context, out io.Writer)
	secrets    secretSources
	logLines   int64
	since      time.Duration
	serverLogs bool
	// Host files read whole, and the directory whose listing is kept.
	meminfo, osRelease, procVersion, stateDir string
}

func hostSupportBundle(unitDir string, logLines int64, since time.Duration, serverLogs bool) *supportBundle {
	host, _ := os.Hostname()
	b := &supportBundle{
		host: host, now: time.Now(), unitDir: unitDir, run: hostCommand, secrets: hostSecretSources,
		logLines: logLines, since: since, serverLogs: serverLogs,
		meminfo: "/proc/meminfo", osRelease: "/etc/os-release", procVersion: "/proc/version", stateDir: "/etc/felis",
	}
	var found bool
	b.w, found, b.wErr = watchdogUnitFlags(filepath.Join(unitDir, "felis-watchdog.service"))
	if b.wErr == nil && !found {
		b.wErr = fmt.Errorf("%s is not installed; read the watchdog's defaults", filepath.Join(unitDir, "felis-watchdog.service"))
	}
	if b.cfg, b.cfgErr = config.Load(b.w.cfgPath); b.cfgErr == nil {
		cfg := b.cfg
		b.backups = func(ctx context.Context) (map[string]time.Time, error) {
			return newestWorldBackups(ctx, cfg.Database.URL)
		}
	} else {
		b.cfg = nil
	}
	if b.cl, b.clErr = buildSystemServerClient(); b.clErr != nil {
		b.cl = nil
	} else if rc, err := hostRESTConfig(); err != nil {
		b.clErr = err
		b.cl = nil
	} else if cs, err := kubernetes.NewForConfig(rc); err != nil {
		b.clErr = err
		b.cl = nil
	} else {
		limit := int64(8 << 20)
		b.logs = func(ctx context.Context, ns, pod, container string, previous bool) ([]byte, error) {
			return cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
				Container: container, Previous: previous, Timestamps: true, TailLines: &logLines, LimitBytes: &limit,
			}).DoRaw(ctx)
		}
	}
	b.doctor = func(ctx context.Context, out io.Writer) {
		runDoctor(ctx, doctorEnv{unitDir: unitDir, run: hostCommand, now: b.now, host: host}, out)
	}
	return b
}

// bundleWriter streams scrubbed files into the archive and keeps what went
// wrong while collecting, for MANIFEST.txt.
type bundleWriter struct {
	tw     *tar.Writer
	prefix string
	now    time.Time
	scrub  *scrubber
	errs   []string
}

// logText adds a log, or a report that quotes errors: known secrets and
// anything logged as one come out.
func (bw *bundleWriter) logText(name string, data []byte) error {
	return bw.add(name, bw.scrub.text(data))
}

// plain adds a file whose secrets were already taken out by structure (the
// cluster's objects, the configuration summary) or that names none (the
// release, disk use, addresses): known secret values still come out, and
// nothing else is rewritten.
func (bw *bundleWriter) plain(name string, data []byte) error {
	return bw.add(name, bw.scrub.values(data))
}

func (bw *bundleWriter) add(name string, data []byte) error {
	hdr := &tar.Header{Name: path.Join(bw.prefix, name), Mode: 0o600, Size: int64(len(data)), ModTime: bw.now, Typeflag: tar.TypeReg}
	if err := bw.tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := bw.tw.Write(data)
	return err
}

func (bw *bundleWriter) failed(what string, err error) {
	bw.errs = append(bw.errs, string(bw.scrub.text([]byte(fmt.Sprintf("%s: %v", what, err)))))
}

// write collects the bundle into dir and returns its path.
func (b *supportBundle) write(ctx context.Context, dir string) (string, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	stamp := b.now.UTC().Format("20060102T150405Z")
	base := fmt.Sprintf("felis-support-%s-%s", safeName(b.host), stamp)
	final := filepath.Join(dir, base+".tar.gz")
	f, err := os.CreateTemp(dir, "."+base+".*.partial") // mode 0600
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		if !keep {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	gz := gzip.NewWriter(f)
	bw := &bundleWriter{tw: tar.NewWriter(gz), prefix: base, now: b.now, scrub: b.scrubber()}
	if err := b.collect(ctx, bw); err != nil {
		return "", err
	}
	if err := bw.tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(f.Name(), final); err != nil {
		return "", err
	}
	keep = true
	return final, nil
}

// safeName keeps a host name usable in a file name.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '-' || r == '.' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
	if s == "" {
		return "host"
	}
	return s
}

func (b *supportBundle) collect(ctx context.Context, bw *bundleWriter) error {
	var buf bytes.Buffer
	cmdVersion(nil, &buf, io.Discard)
	for _, p := range []string{b.osRelease, b.procVersion} {
		if raw, err := os.ReadFile(p); err == nil {
			fmt.Fprintf(&buf, "\n# %s\n%s", p, raw)
		}
	}
	if err := bw.plain("version.txt", buf.Bytes()); err != nil {
		return err
	}

	buf.Reset()
	switch {
	case b.cfgErr != nil:
		fmt.Fprintf(&buf, "felis status: the configuration did not load: %v\n", b.cfgErr)
	default:
		printStatus(ctx, statusEnv{
			cfg: b.cfg, w: b.w, cl: b.cl, clErr: b.clErr, backups: b.backups, run: b.run,
			unitDir: b.unitDir, meminfo: b.meminfo, host: b.host, now: b.now,
		}, &buf)
	}
	if err := bw.logText("status.txt", buf.Bytes()); err != nil {
		return err
	}
	buf.Reset()
	b.doctor(ctx, &buf)
	if err := bw.logText("doctor.txt", buf.Bytes()); err != nil {
		return err
	}
	if b.cfg != nil {
		if err := bw.plain("config.txt", configSummary(b.cfg, b.w.cfgPath)); err != nil {
			return err
		}
	}
	if err := b.collectHost(ctx, bw); err != nil {
		return err
	}
	if err := b.collectJournal(ctx, bw); err != nil {
		return err
	}
	if b.cl == nil {
		bw.failed("cluster", b.clErr)
	} else if err := b.collectCluster(ctx, bw); err != nil {
		return err
	}
	return bw.add("MANIFEST.txt", b.manifest(bw))
}

func (b *supportBundle) collectHost(ctx context.Context, bw *bundleWriter) error {
	commands := []struct {
		name string
		argv []string
	}{
		{"host/systemd-units.txt", []string{"systemctl", "list-units", "--all", "--no-pager", "--plain", "felis-*", "k3s.service"}},
		{"host/systemd-timers.txt", []string{"systemctl", "list-timers", "--all", "--no-pager", "felis-*"}},
		{"host/df.txt", []string{"df", "-h"}},
		{"host/addresses.txt", []string{"ip", "-brief", "address"}},
	}
	for _, c := range commands {
		out, err := b.run(ctx, c.argv[0], c.argv[1:]...)
		if err != nil && len(out) == 0 {
			bw.failed(strings.Join(c.argv, " "), err)
			continue
		}
		if err := bw.plain(c.name, out); err != nil {
			return err
		}
	}
	if raw, err := os.ReadFile(b.meminfo); err == nil {
		if err := bw.plain("host/meminfo.txt", raw); err != nil {
			return err
		}
	}
	listing, err := dirListing(b.stateDir)
	if err != nil {
		bw.failed("list "+b.stateDir, err)
		return nil
	}
	return bw.plain("host/etc-felis.txt", listing)
}

// dirListing names every file under dir with its mode, size and time, and
// holds nothing of what is in them.
func dirListing(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "# %s: names, modes, sizes and times only; no contents\n", dir)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", info.Mode(), info.Size(), info.ModTime().UTC().Format(time.RFC3339), p)
		return nil
	})
	tw.Flush()
	return buf.Bytes(), err
}

func (b *supportBundle) collectJournal(ctx context.Context, bw *bundleWriter) error {
	units, _ := filepath.Glob(filepath.Join(b.unitDir, "felis-*.service"))
	if _, err := os.Stat(filepath.Join(b.unitDir, "k3s.service")); err == nil {
		units = append(units, filepath.Join(b.unitDir, "k3s.service"))
	}
	since := "@" + strconv.FormatInt(b.now.Add(-b.since).Unix(), 10)
	for _, u := range units {
		unit := filepath.Base(u)
		out, err := b.run(ctx, "journalctl", "-u", unit, "--since", since, "-n", strconv.FormatInt(b.logLines, 10), "--no-pager", "-o", "short-iso")
		if err != nil && len(out) == 0 {
			bw.failed("journalctl -u "+unit, err)
			continue
		}
		if err := bw.logText("journal/"+strings.TrimSuffix(unit, ".service")+".log", out); err != nil {
			return err
		}
	}
	return nil
}

// bundleNamespaces are the namespaces whose objects and logs the bundle
// collects: the control plane, the builds and the game servers.
func (b *supportBundle) bundleNamespaces() (control, build, minecraft string) {
	control, build, minecraft = b.w.controlNS, platform.DefaultBuildNamespace, platform.DefaultMinecraftNamespace
	if b.cfg != nil {
		if b.cfg.Registry.BuildNamespace != "" {
			build = b.cfg.Registry.BuildNamespace
		}
		if b.cfg.K8s.Namespace != "" {
			minecraft = b.cfg.K8s.Namespace
		}
	}
	return control, build, minecraft
}

func (b *supportBundle) collectCluster(ctx context.Context, bw *bundleWriter) error {
	control, build, minecraft := b.bundleNamespaces()
	dump := func(name string, list client.ObjectList, opts ...client.ListOption) error {
		if err := b.cl.List(ctx, list, opts...); err != nil {
			bw.failed("list "+name, err)
			return nil
		}
		redactList(list)
		out, err := yaml.Marshal(list)
		if err != nil {
			bw.failed("encode "+name, err)
			return nil
		}
		return bw.plain(name, out)
	}
	if err := dump("cluster/nodes.yaml", &corev1.NodeList{}); err != nil {
		return err
	}
	if err := dump("cluster/persistentvolumes.yaml", &corev1.PersistentVolumeList{}); err != nil {
		return err
	}
	if err := dump("cluster/minecraftservers.yaml", &v1alpha1.MinecraftServerList{}, client.InNamespace(minecraft)); err != nil {
		return err
	}
	for _, ns := range []string{control, build, minecraft} {
		for _, k := range []struct {
			name string
			list client.ObjectList
		}{
			{"pods", &corev1.PodList{}},
			{"deployments", &appsv1.DeploymentList{}},
			{"statefulsets", &appsv1.StatefulSetList{}},
			{"jobs", &batchv1.JobList{}},
			{"cronjobs", &batchv1.CronJobList{}},
			{"services", &corev1.ServiceList{}},
			{"persistentvolumeclaims", &corev1.PersistentVolumeClaimList{}},
			{"networkpolicies", &networkingv1.NetworkPolicyList{}},
			{"events", &corev1.EventList{}},
		} {
			if err := dump("cluster/"+ns+"/"+k.name+".yaml", k.list, client.InNamespace(ns)); err != nil {
				return err
			}
		}
	}

	var all corev1.PodList
	if err := b.cl.List(ctx, &all); err != nil {
		bw.failed("list every pod", err)
	} else if err := bw.plain("cluster/pods-all-namespaces.txt", podTable(all.Items, b.now)); err != nil {
		return err
	}

	for _, ns := range []string{control, build, minecraft} {
		var pods corev1.PodList
		if err := b.cl.List(ctx, &pods, client.InNamespace(ns)); err != nil {
			continue // already recorded by the dump above
		}
		for _, p := range pods.Items {
			if err := b.collectPodLogs(ctx, bw, p, ns == minecraft && !b.serverLogs); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectPodLogs keeps the tail of each container's log, and of its previous
// run when it restarted. initOnly keeps only the init containers: a game
// server's own log carries player names, addresses and chat.
func (b *supportBundle) collectPodLogs(ctx context.Context, bw *bundleWriter, p corev1.Pod, initOnly bool) error {
	type ctr struct {
		name     string
		restarts int32
	}
	var ctrs []ctr
	restarts := map[string]int32{}
	for _, cs := range append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		restarts[cs.Name] = cs.RestartCount
	}
	for _, c := range p.Spec.InitContainers {
		ctrs = append(ctrs, ctr{c.Name, restarts[c.Name]})
	}
	if !initOnly {
		for _, c := range p.Spec.Containers {
			ctrs = append(ctrs, ctr{c.Name, restarts[c.Name]})
		}
	}
	for _, c := range ctrs {
		for _, previous := range []bool{false, true} {
			if previous && c.restarts == 0 {
				continue
			}
			name := fmt.Sprintf("logs/%s/%s/%s.log", p.Namespace, p.Name, c.name)
			if previous {
				name = fmt.Sprintf("logs/%s/%s/%s.previous.log", p.Namespace, p.Name, c.name)
			}
			out, err := b.logs(ctx, p.Namespace, p.Name, c.name, previous)
			if err != nil {
				bw.failed(name, err)
				continue
			}
			if err := bw.logText(name, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// podTable is every pod on the node, one line each.
func podTable(pods []corev1.Pod, now time.Time) []byte {
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tNAME\tPHASE\tREADY\tRESTARTS\tAGE")
	for _, p := range pods {
		var ready, restarts int32
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		age := "-"
		if !p.CreationTimestamp.IsZero() {
			age = now.Sub(p.CreationTimestamp.Time).Round(time.Minute).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%d\t%s\n", p.Namespace, p.Name, p.Status.Phase, ready, len(p.Spec.Containers), restarts, age)
	}
	tw.Flush()
	return buf.Bytes()
}

// redactList takes out of every object what the bundle must not carry: the
// literal env values of pod specs and of MinecraftServers (valueFrom
// references stay, naming the Secret without its contents), the
// last-applied-configuration annotation that repeats them, and managedFields.
func redactList(list client.ObjectList) {
	items, err := meta.ExtractList(list)
	if err != nil {
		return
	}
	for _, it := range items {
		if acc, err := meta.Accessor(it); err == nil {
			acc.SetManagedFields(nil)
			if ann := acc.GetAnnotations(); ann != nil {
				delete(ann, corev1.LastAppliedConfigAnnotation)
				acc.SetAnnotations(ann)
			}
		}
		switch o := it.(type) {
		case *corev1.Pod:
			redactPodSpec(&o.Spec)
		case *appsv1.Deployment:
			redactPodSpec(&o.Spec.Template.Spec)
		case *appsv1.StatefulSet:
			redactPodSpec(&o.Spec.Template.Spec)
		case *batchv1.Job:
			redactPodSpec(&o.Spec.Template.Spec)
		case *batchv1.CronJob:
			redactPodSpec(&o.Spec.JobTemplate.Spec.Template.Spec)
		case *v1alpha1.MinecraftServer:
			for i := range o.Spec.Env {
				if o.Spec.Env[i].Value != "" {
					o.Spec.Env[i].Value = redacted
				}
			}
		}
	}
}

func redactPodSpec(s *corev1.PodSpec) {
	blank := func(cs []corev1.Container) {
		for i := range cs {
			for j := range cs[i].Env {
				if cs[i].Env[j].Value != "" {
					cs[i].Env[j].Value = redacted
				}
			}
		}
	}
	blank(s.InitContainers)
	blank(s.Containers)
	for i := range s.EphemeralContainers {
		for j := range s.EphemeralContainers[i].Env {
			if s.EphemeralContainers[i].Env[j].Value != "" {
				s.EphemeralContainers[i].Env[j].Value = redacted
			}
		}
	}
}

// configSummary is felis.toml without a single credential: the hostnames,
// namespaces and which features are on. Fields are picked one by one, so a
// field added later stays out until someone decides it is safe.
func configSummary(c *config.Config, path string) []byte {
	var buf bytes.Buffer
	line := func(k string, v any) { fmt.Fprintf(&buf, "%-34s %v\n", k, v) }
	fmt.Fprintf(&buf, "# a summary of %s; no password, key or token is in it\n", path)
	line("server.root_domain", c.Server.RootDomain)
	line("server.listen", c.Server.Listen)
	db := "(unparsable)"
	if u, err := url.Parse(c.Database.URL); err == nil {
		db = u.Scheme + "://" + u.User.Username() + "@" + u.Host + u.Path
	}
	line("database.url (no password)", db)
	line("database.deployment", c.Database.Deployment)
	line("velocity.public_ip", c.Velocity.PublicIP)
	line("velocity.game_port", c.Velocity.GamePort)
	line("velocity.login_image", c.Velocity.LoginImage)
	line("velocity.lobby_image", c.Velocity.LobbyImage)
	line("auth.panel_hostname", c.Auth.PanelHostname)
	line("auth.admin_hostname", c.Auth.AdminHostname)
	line("auth.client_ip_header", c.Auth.ClientIPHeader)
	line("k8s.namespace", c.K8s.Namespace)
	line("k8s.egress_mode", c.K8s.EgressMode)
	line("k8s.metallb_pool", c.K8s.MetalLBPool)
	line("registry.url", c.Registry.URL)
	line("registry.build_namespace", c.Registry.BuildNamespace)
	line("registry.trivy_db_repository", c.Registry.TrivyDBRepository)
	line("registry.build_user_namespaces", c.Registry.BuildUserNamespaces)
	line("registry.build_runtime_class", c.Registry.BuildRuntimeClass)
	line("registry.max_concurrent_builds", c.Registry.MaxConcurrentBuilds)
	line("registry.user_uploads_context", c.Registry.UserUploadsContext)
	line("archive.store", c.Archive.Store)
	line("archive.local_path", c.Archive.LocalPath)
	line("archive.retention", c.Archive.Retention)
	line("archive.scheduled_every", c.Archive.ScheduledEvery)
	line("archive.scheduled_keep", c.Archive.ScheduledKeep)
	line("offsite (configured)", c.Offsite.Enabled())
	if c.Offsite.Enabled() {
		line("offsite.endpoint", c.Offsite.Endpoint)
		line("offsite.bucket", c.Offsite.Bucket)
		line("offsite.prefix", c.Offsite.Prefix)
	}
	line("smtp.host", c.SMTP.Host)
	if c.SMTP.Host != "" {
		line("smtp.port", c.SMTP.Port)
		line("smtp.require_tls", c.SMTP.TLSRequired())
		line("smtp.max_per_hour", c.SMTP.MaxPerHour)
	}
	for i, s := range c.AuthSources {
		line(fmt.Sprintf("auth_source[%d]", i), s.Tag+" "+s.Prefix+" "+s.URL)
	}
	return buf.Bytes()
}

func (b *supportBundle) manifest(bw *bundleWriter) []byte {
	control, build, minecraft := b.bundleNamespaces()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Felis support bundle\nhost %s, collected %s, felis %s\n\n", b.host, b.now.UTC().Format(time.RFC3339), resolvedVersion())
	fmt.Fprintf(&buf, `What it holds:
  status.txt, doctor.txt   felis status and felis doctor at collection time
  version.txt              the release, the OS and the kernel
  config.txt               a summary of felis.toml: hostnames, namespaces, which features are on
  host/                    systemd units and timers, disk use, memory, addresses, and the names,
                           modes, sizes and times of the files under %s (not what is in them)
  journal/                 up to %d lines per Felis unit and k3s, from the last %s
  logs/                    up to %d lines of each container of the pods in %s and %s, and of the
                           init containers of the game server pods in %s; the previous run too
                           where a container restarted
  cluster/                 nodes, volumes, the MinecraftServers, and the pods, workloads, services,
                           volume claims, network policies and events of %s, %s and %s
`, b.stateDir, b.logLines, shortDuration(b.since), b.logLines, control, build, minecraft, control, build, minecraft)
	if b.serverLogs {
		fmt.Fprintln(&buf, "\nThe game servers' own logs are in logs/ (-server-logs): they carry player names, IP addresses and chat.")
	} else {
		fmt.Fprintln(&buf, "\nThe game servers' own logs are left out (they carry player names, IP addresses and chat; -server-logs adds them).")
	}
	fmt.Fprint(&buf, `
Never collected: Kubernetes Secrets and ConfigMaps, what is in /etc/felis or any configuration
file, the database, worlds, uploads.

Taken out:
`)
	if len(bw.scrub.sources) > 0 {
		fmt.Fprintf(&buf, "  - %d secret values, wherever they appear, read from:\n", len(bw.scrub.vals))
		for _, s := range bw.scrub.sources {
			fmt.Fprintf(&buf, "      %s\n", s)
		}
	} else {
		fmt.Fprintln(&buf, "  - no secret file was found on this host to take values from")
	}
	fmt.Fprint(&buf, `  - passwords in URLs, private keys, Bearer and Basic credentials
  - in logs and command output, whatever follows password=, secret=, token=, api_key=,
    access_key=, private_key= or credentials= (and the same with a colon)
  - every literal env value in pod specs and MinecraftServers (valueFrom references stay)

Read it through before you send it anywhere: redaction finds this host's known secrets and the
common ways a secret is logged, and a secret logged another way stays in.
`)
	if b.wErr != nil {
		fmt.Fprintf(&buf, "\nThe watchdog's settings: %v\n", b.wErr)
	}
	if len(bw.errs) > 0 {
		fmt.Fprintln(&buf, "\nNot collected:")
		for _, e := range bw.errs {
			fmt.Fprintf(&buf, "  - %s\n", e)
		}
	}
	// Its own words name what is redacted, and would be redacted themselves;
	// what it quotes was scrubbed as it was recorded.
	return buf.Bytes()
}

// secretSources are files that hold secrets, by how each is laid out.
type secretSources struct {
	envFiles   []string // KEY=VALUE lines, every value a secret (a commented-out one too)
	valueFiles []string // one secret, the whole file
	propsFiles []string // key=value lines; the keys naming a token, secret, password or key hold one
	tokenFiles []string // k3s join tokens: the whole token and its secret part after the last ':'
}

// scrubber takes secrets out of what the bundle collects.
type scrubber struct {
	vals    []string // longest first, so a secret that contains another goes whole
	sources []string // the files vals came from
}

var (
	pemPrivateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	urlUserinfo   = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/\s:@]*:)[^/\s@]+@`)
	authScheme    = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	secretAssign  = regexp.MustCompile(`(?i)((?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?)[A-Za-z0-9_.-]*"?[ \t]*[=:][ \t]*"?)([^\s"',;&]{4,})`)
	secretPropKey = regexp.MustCompile(`(?i)token|secret|password|key`)
)

func (b *supportBundle) scrubber() *scrubber {
	s := &scrubber{}
	s.readSources(b.secrets)
	if b.cfg != nil {
		if u, err := url.Parse(b.cfg.Database.URL); err == nil {
			if pw, ok := u.User.Password(); ok {
				s.add(pw)
			}
		}
		for _, ref := range []string{
			b.cfg.Velocity.ServiceTokenRef, b.cfg.SMTP.PasswordRef,
			b.cfg.Offsite.AccessKeyRef, b.cfg.Offsite.SecretKeyRef, b.cfg.Offsite.KeyRef,
			b.cfg.Registry.S3.AccessKeyRef, b.cfg.Registry.S3.SecretKeyRef,
			b.cfg.Archive.S3.AccessKeyRef, b.cfg.Archive.S3.SecretKeyRef,
		} {
			if ref != "" {
				s.add(os.Getenv(ref))
			}
		}
	}
	if st, err := watchdog.LoadState(watchdog.NewestState(b.w.statePath, b.w.fallbackState)); err == nil {
		s.add(st.SMTPPassword)
	}
	return s
}

func (s *scrubber) add(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < minScrubLen {
		return false
	}
	for _, have := range s.vals {
		if have == v {
			return true
		}
	}
	s.vals = append(s.vals, v)
	sort.SliceStable(s.vals, func(i, j int) bool { return len(s.vals[i]) > len(s.vals[j]) })
	return true
}

func (s *scrubber) readSources(src secretSources) {
	read := func(p string, take func(content string) bool) {
		raw, err := os.ReadFile(p)
		if err != nil {
			return
		}
		if take(string(raw)) {
			s.sources = append(s.sources, p)
		}
	}
	keyValues := func(content string, keep func(key string) bool) bool {
		found := false
		for _, line := range strings.Split(content, "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok || !keep(strings.TrimSpace(k)) {
				continue
			}
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			found = s.add(v) || found
		}
		return found
	}
	for _, p := range src.envFiles {
		read(p, func(c string) bool { return keyValues(c, func(string) bool { return true }) })
	}
	for _, p := range src.propsFiles {
		read(p, func(c string) bool { return keyValues(c, secretPropKey.MatchString) })
	}
	for _, p := range src.valueFiles {
		read(p, func(c string) bool { return s.add(c) })
	}
	for _, p := range src.tokenFiles {
		read(p, func(c string) bool {
			c = strings.TrimSpace(c)
			whole := s.add(c)
			part := false
			if i := strings.LastIndex(c, ":"); i >= 0 {
				part = s.add(c[i+1:])
			}
			return whole || part
		})
	}
}

// values takes every known secret value out of data.
func (s *scrubber) values(data []byte) []byte {
	t := pemPrivateKey.ReplaceAllString(string(data), "<redacted private key>")
	for _, v := range s.vals {
		t = strings.ReplaceAll(t, v, redacted)
	}
	t = urlUserinfo.ReplaceAllString(t, "${1}"+redacted+"@")
	t = authScheme.ReplaceAllString(t, "${1} "+redacted)
	return []byte(t)
}

// text is values, and whatever a log names as a secret as well.
func (s *scrubber) text(data []byte) []byte {
	return secretAssign.ReplaceAll(s.values(data), []byte("${1}"+redacted))
}
