package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/store"
	"felis.lolicon.best/internal/watchdog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cmdStatus prints the platform at a glance: the release and the node, the
// control plane's workloads, the game proxy, every server with its players
// and newest world backup, the database backups and the off-site copy, the
// host's disks and memory, and what the watchdog has open. It changes
// nothing, and a part that is down (the cluster, PostgreSQL) reads as such
// while the rest still prints. felis doctor says what is wrong and where to
// look.
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	unitDir := fs.String("systemd-dir", systemdUnitDir, "where the installer's systemd units are")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis status: run as root (sudo felis status): it reads root-only state under /etc/felis and /var/lib/felis")
		return 1
	}
	env, err := hostStatusEnv(*unitDir)
	if err != nil {
		fmt.Fprintf(stderr, "felis status: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	printStatus(ctx, env, stdout)
	return 0
}

// statusEnv is what one status report reads the host through.
type statusEnv struct {
	cfg *config.Config
	// w is how felis-watchdog.service runs the watchdog: where the backups,
	// the off-site record and the proxy are.
	w       watchdogFlags
	cl      client.Client // nil while the API server is unreachable
	clErr   error
	backups func(ctx context.Context) (map[string]time.Time, error)
	run     func(ctx context.Context, name string, args ...string) ([]byte, error)
	unitDir string
	meminfo string
	host    string
	now     time.Time
}

// hostStatusEnv reads this host: the watchdog's settings, the configuration
// they name, and the cluster.
func hostStatusEnv(unitDir string) (statusEnv, error) {
	w, _, err := watchdogUnitFlags(filepath.Join(unitDir, "felis-watchdog.service"))
	if err != nil {
		return statusEnv{}, err
	}
	cfg, err := config.Load(w.cfgPath)
	if err != nil {
		return statusEnv{}, err
	}
	cl, clErr := buildSystemServerClient()
	if clErr != nil {
		cl = nil
	}
	host, _ := os.Hostname()
	return statusEnv{
		cfg: cfg, w: w, cl: cl, clErr: clErr,
		backups: func(ctx context.Context) (map[string]time.Time, error) {
			return newestWorldBackups(ctx, cfg.Database.URL)
		},
		run: hostCommand, unitDir: unitDir, meminfo: "/proc/meminfo", host: host, now: time.Now(),
	}, nil
}

func printStatus(ctx context.Context, env statusEnv, out io.Writer) {
	fmt.Fprintf(out, "felis %s on %s at %s\n", resolvedVersion(), env.host, env.now.UTC().Format("2006-01-02 15:04 UTC"))
	if env.cl == nil {
		fmt.Fprintf(out, "cluster:  unreachable (%v)\n", env.clErr)
	} else {
		statusCluster(ctx, env, out)
	}
	statusProxy(ctx, env, out)
	statusServers(ctx, env, out)
	statusBackups(env, out)
	statusHost(env, out)
	statusWatchdog(ctx, env, out)
}

func statusCluster(ctx context.Context, env statusEnv, out io.Writer) {
	var nodes corev1.NodeList
	if err := env.cl.List(ctx, &nodes); err != nil {
		fmt.Fprintf(out, "cluster:  unreachable (%v)\n", err)
		return
	}
	for _, n := range nodes.Items {
		ready := "NotReady"
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = "Ready"
			}
		}
		info := n.Status.NodeInfo
		fmt.Fprintf(out, "node:     %s %s, k3s %s, %s, kernel %s\n", n.Name, ready, info.KubeletVersion, info.OSImage, info.KernelVersion)
	}

	ns := env.w.controlNS
	var deps appsv1.DeploymentList
	var pods corev1.PodList
	err := env.cl.List(ctx, &deps, client.InNamespace(ns))
	if err == nil {
		err = env.cl.List(ctx, &pods, client.InNamespace(ns))
	}
	fmt.Fprintf(out, "\ncontrol plane (namespace %s):\n", ns)
	if err != nil {
		fmt.Fprintf(out, "  cannot list it: %v\n", err)
		return
	}
	sort.Slice(deps.Items, func(i, j int) bool { return deps.Items[i].Name < deps.Items[j].Name })
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, d := range deps.Items {
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		image := "-"
		if cs := d.Spec.Template.Spec.Containers; len(cs) > 0 {
			image = shortImage(cs[0].Image)
		}
		fmt.Fprintf(tw, "  %s\t%d/%d ready\t%s\trestarts %d\n", d.Name, d.Status.ReadyReplicas, want, image, podRestarts(d.Spec.Selector, pods.Items))
	}
	tw.Flush()
}

// podRestarts adds up the container restarts of the pods selector picks (the
// API server refuses a Deployment whose selector is empty).
func podRestarts(selector *metav1.LabelSelector, pods []corev1.Pod) int32 {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return 0
	}
	var n int32
	for _, p := range pods {
		if !sel.Matches(labels.Set(p.Labels)) {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			n += cs.RestartCount
		}
	}
	return n
}

// shortImage is an image reference without its registry and repository path,
// and with its digest cut to 12 characters.
func shortImage(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	if name, digest, ok := strings.Cut(ref, "@sha256:"); ok && len(digest) > 12 {
		ref = name + "@" + digest[:12]
	}
	return ref
}

func statusProxy(ctx context.Context, env statusEnv, out io.Writer) {
	var parts []string
	if _, err := os.Stat(filepath.Join(env.unitDir, "felis-velocity.service")); err == nil {
		parts = append(parts, "felis-velocity "+unitActiveState(ctx, doctorEnv{run: env.run}, "felis-velocity.service"))
	}
	if env.w.proxyAddr != "" {
		d := net.Dialer{Timeout: 3 * time.Second}
		if conn, err := d.DialContext(ctx, "tcp", env.w.proxyAddr); err != nil {
			parts = append(parts, fmt.Sprintf("%s refuses connections (%v)", env.w.proxyAddr, err))
		} else {
			conn.Close()
			parts = append(parts, env.w.proxyAddr+" accepts connections")
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "not on this host")
	}
	fmt.Fprintf(out, "\nproxy:    %s\n", strings.Join(parts, ", "))
}

func statusServers(ctx context.Context, env statusEnv, out io.Writer) {
	ns := env.cfg.K8s.Namespace
	if ns == "" {
		ns = platform.DefaultMinecraftNamespace
	}
	if env.cl == nil {
		fmt.Fprintf(out, "\nservers (namespace %s): unknown while the cluster is unreachable\n", ns)
		return
	}
	var list v1alpha1.MinecraftServerList
	if err := env.cl.List(ctx, &list, client.InNamespace(ns)); err != nil {
		fmt.Fprintf(out, "\nservers (namespace %s): cannot list them: %v\n", ns, err)
		return
	}
	newest, backupErr := env.backups(ctx)
	running, online := 0, int32(0)
	for _, ms := range list.Items {
		if ms.Status.Phase == v1alpha1.PhaseRunning {
			running++
			online += ms.Status.Players.Online
		}
	}
	fmt.Fprintf(out, "\nservers (namespace %s): %d, %d running, %d players online\n", ns, len(list.Items), running, online)
	if len(list.Items) == 0 {
		return
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tROLE\tDESIRED\tPHASE\tPLAYERS\tNEWEST WORLD BACKUP")
	for _, ms := range list.Items {
		role := ms.Labels[v1alpha1.LabelSystemRole]
		if role == "" {
			role = "-"
		}
		phase := string(ms.Status.Phase)
		if phase == "" {
			phase = "-"
		}
		players := "-"
		if ms.Status.Phase == v1alpha1.PhaseRunning {
			players = fmt.Sprintf("%d/%d", ms.Status.Players.Online, ms.Status.Players.Max)
		}
		backup := "none"
		switch at, ok := newest[ms.Name]; {
		case backupErr != nil:
			backup = "?"
		case ok:
			backup = dbbackup.Age(env.now.Sub(at)) + " ago"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", ms.Name, role, orDash(string(ms.Spec.DesiredState)), phase, players, backup)
	}
	tw.Flush()
	if backupErr != nil {
		fmt.Fprintf(out, "  world backups unknown: %v\n", backupErr)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// newestWorldBackups is when each server's newest world backup that a restore
// can use was taken.
func newestWorldBackups(ctx context.Context, url string) (map[string]time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer drv.Close()
	rows, err := drv.DB().QueryContext(ctx, `SELECT server_name, max(created_at) FROM world_backups
		WHERE status = 'present' AND corrupt_at IS NULL GROUP BY server_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at time.Time
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[name] = at
	}
	return out, rows.Err()
}

func statusBackups(env statusEnv, out io.Writer) {
	fmt.Fprintln(out, "\nbackups:")
	switch bundles, err := dbbackup.List(env.w.backupDir); {
	case env.w.backupDir == "":
		fmt.Fprintln(out, "  database: not checked (felis-watchdog.service names no -backup-dir)")
	case err != nil:
		fmt.Fprintf(out, "  database: cannot read %s: %v\n", env.w.backupDir, err)
	case len(bundles) == 0:
		fmt.Fprintf(out, "  database: none in %s\n", env.w.backupDir)
	default:
		fmt.Fprintf(out, "  database: newest %s, %s ago; %d bundles in %s\n",
			bundles[0].Name, dbbackup.Age(env.now.Sub(bundles[0].Created)), len(bundles), env.w.backupDir)
	}
	if !env.cfg.Offsite.Enabled() {
		fmt.Fprintln(out, "  off-site: not configured, every backup is on this machine only")
		return
	}
	switch st, err := offsite.ReadStatus(env.w.offsiteStatus); {
	case err != nil:
		fmt.Fprintf(out, "  off-site: %v\n", err)
	case st == nil:
		fmt.Fprintln(out, "  off-site: never synced")
	case st.LastSuccess.IsZero():
		fmt.Fprintf(out, "  off-site: never succeeded; last attempt %s ago: %s\n", dbbackup.Age(env.now.Sub(st.LastAttempt)), st.LastError)
	default:
		line := fmt.Sprintf("  off-site: last good sync %s ago to %s", dbbackup.Age(env.now.Sub(st.LastSuccess)), st.Bucket)
		if st.LastAttempt.After(st.LastSuccess) { // a failed run records no success
			line += fmt.Sprintf("; the last attempt, %s ago, failed: %s", dbbackup.Age(env.now.Sub(st.LastAttempt)), st.LastError)
		}
		fmt.Fprintln(out, line)
	}
}

func statusHost(env statusEnv, out io.Writer) {
	fmt.Fprintln(out, "\nhost:")
	seen := map[uint64]bool{}
	for _, p := range splitList(env.w.diskPaths) {
		var st syscall.Stat_t
		if err := syscall.Stat(p, &st); err != nil {
			continue
		}
		dev := uint64(st.Dev) // int32 on darwin
		if seen[dev] {
			continue
		}
		seen[dev] = true
		var fs syscall.Statfs_t
		if err := syscall.Statfs(p, &fs); err != nil || fs.Blocks == 0 {
			continue
		}
		bsize := uint64(fs.Bsize) // uint32 on darwin
		total, avail := uint64(fs.Blocks)*bsize, uint64(fs.Bavail)*bsize
		fmt.Fprintf(out, "  disk %s: %s free of %s (%.0f%% free)\n", p, offsite.HumanBytes(int64(avail)), offsite.HumanBytes(int64(total)), float64(fs.Bavail)/float64(fs.Blocks)*100)
	}
	if total, avail, ok := readMeminfo(env.meminfo); ok {
		fmt.Fprintf(out, "  memory: %s available of %s\n", offsite.HumanBytes(int64(avail)), offsite.HumanBytes(int64(total)))
	}
}

// readMeminfo reads MemTotal and MemAvailable, in bytes, from a /proc/meminfo
// style file.
func readMeminfo(path string) (total, avail uint64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64) // the kernel writes numbers
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	return total, avail, total > 0
}

func statusWatchdog(ctx context.Context, env statusEnv, out io.Writer) {
	fmt.Fprintln(out, "\nwatchdog:")
	if _, err := os.Stat(filepath.Join(env.unitDir, "felis-watchdog.timer")); err != nil {
		fmt.Fprintln(out, "  not installed: nothing checks this host")
		return
	}
	line := "  timer " + unitActiveState(ctx, doctorEnv{run: env.run}, "felis-watchdog.timer")
	show, _ := env.run(ctx, "systemctl", "show", "--timestamp=unix", "-p", "Result", "-p", "ExecMainExitTimestamp", "felis-watchdog.service")
	props := map[string]string{}
	for _, ln := range strings.Split(string(show), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(ln), "="); ok {
			props[k] = v
		}
	}
	// ExecMainExitTimestamp is empty until the service has run.
	if sec, _ := strconv.ParseInt(strings.TrimPrefix(props["ExecMainExitTimestamp"], "@"), 10, 64); sec > 0 {
		line += fmt.Sprintf(", last run %s ago (%s)", dbbackup.Age(env.now.Sub(time.Unix(sec, 0))), orDash(props["Result"]))
	}
	fmt.Fprintln(out, line)

	state, err := watchdog.LoadState(watchdog.NewestState(env.w.statePath, env.w.fallbackState))
	if err != nil {
		fmt.Fprintf(out, "  alerts: unknown (%v)\n", err)
		return
	}
	var open []string
	for key, a := range state.Alerts {
		if !a.ClearedAt.IsZero() {
			continue
		}
		if a.Notified.IsZero() {
			open = append(open, fmt.Sprintf("%s (%s, seen %s ago, not mailed yet)", key, a.Severity, dbbackup.Age(env.now.Sub(a.FirstSeen))))
		} else {
			open = append(open, fmt.Sprintf("%s (%s, mailed %s ago)", key, a.Severity, dbbackup.Age(env.now.Sub(a.Notified))))
		}
	}
	if len(open) == 0 {
		fmt.Fprintln(out, "  alerts: none open")
		return
	}
	sort.Strings(open)
	fmt.Fprintf(out, "  alerts: %d open (sudo felis doctor says where to look)\n", len(open))
	for _, o := range open {
		fmt.Fprintf(out, "    %s\n", o)
	}
}
