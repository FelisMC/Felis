package watchdog

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/imagepush"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Thresholds. The durations are how long a condition must hold before it is
// mailed (Finding.For): long enough for a rollout, a restart or an installer
// run to heal it, short enough that players are not the first to notice.
const (
	controlPlaneFor = 5 * time.Minute
	systemServerFor = 10 * time.Minute
	failedServerFor = 5 * time.Minute
	nodeNotReadyFor = 5 * time.Minute
	nodePressureFor = 2 * time.Minute
	postgresFor     = 3 * time.Minute
	kubeAPIFor      = 5 * time.Minute
	backupFor       = 10 * time.Minute
	diskLowFor      = 15 * time.Minute
	diskCriticalFor = 5 * time.Minute
	memoryLowFor    = 15 * time.Minute

	// jobFailureWindow is how far back a failed Job is still news.
	jobFailureWindow = 24 * time.Hour
	// maxBackupAge is the daily database backup's deadline: a day plus the
	// timer's randomized delay and a margin.
	maxBackupAge = 26 * time.Hour
	// maxReaperAge is the same for the daily reaper CronJob.
	maxReaperAge = 26 * time.Hour
	// maxScanDBAge is how old the registry's copy of Trivy's vulnerability DB may
	// grow. felis-build-tools.timer refreshes it twice a day and upstream publishes
	// every six hours; three days of failed refreshes means scans are passing
	// images against advisories that are no longer current.
	maxScanDBAge = 72 * time.Hour

	diskLowRatio      = 0.15
	diskCriticalRatio = 0.05
	memoryLowRatio    = 0.10
)

// controlDeployments are the control-plane Deployments, with what their outage
// takes down.
var controlDeployments = []struct {
	name, impact, impactEN string
}{
	{platform.PostgresName, "数据库不可用：登录、面板和服务器管理都会失败", "the control-plane database is down: sign-in, the panel and server management fail"},
	{platform.SAAPI, "面板、登录验证和内部接口都不可用", "the panel, sign-in and the internal API are down"},
	{platform.SAOperator, "服务器无法启动、停止或更新状态", "no server can start, stop or report status"},
	{"registry", "游戏镜像拉取和构建都会失败", "game image pulls and builds fail"},
}

// ClusterPrefixes are the keys Cluster.Check produces. When the API server
// cannot be reached, alerts under them keep their state (Report.Unknown).
var ClusterPrefixes = []string{"deployment/", "system-server/", "server-failed/", "job-failed/", "reaper-stale", "node/"}

// Cluster checks the Kubernetes side: the control plane, the system servers,
// the fleet, recent Job failures, the reaper's schedule and the nodes.
type Cluster struct {
	Client             client.Client
	ControlNamespace   string
	MinecraftNamespace string
}

// Check returns the cluster's findings. An error means the API server could
// not answer; the caller reports that and treats every cluster check as unknown.
func (c Cluster) Check(ctx context.Context, now time.Time) ([]Finding, error) {
	var out []Finding
	for _, d := range controlDeployments {
		f, err := c.deployment(ctx, d.name, d.impact, d.impactEN)
		if err != nil {
			return nil, err
		}
		if f != nil {
			out = append(out, *f)
		}
	}

	var servers v1alpha1.MinecraftServerList
	if err := c.Client.List(ctx, &servers, client.InNamespace(c.MinecraftNamespace)); err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	for i := range servers.Items {
		if f := serverFinding(&servers.Items[i], c.MinecraftNamespace); f != nil {
			out = append(out, *f)
		}
	}

	var jobs batchv1.JobList
	if err := c.Client.List(ctx, &jobs, client.InNamespace(c.MinecraftNamespace)); err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	for i := range jobs.Items {
		if f := jobFinding(&jobs.Items[i], now); f != nil {
			out = append(out, *f)
		}
	}

	var cj batchv1.CronJob
	switch err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.MinecraftNamespace, Name: platform.SAReaper}, &cj); {
	case apierrors.IsNotFound(err):
		// Retention is off; there is no reaper to be late.
	case err != nil:
		return nil, fmt.Errorf("get reaper cronjob: %w", err)
	default:
		if f := reaperFinding(&cj, now); f != nil {
			out = append(out, *f)
		}
	}

	var nodes corev1.NodeList
	if err := c.Client.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	for i := range nodes.Items {
		out = append(out, nodeFindings(&nodes.Items[i])...)
	}
	return out, nil
}

func (c Cluster) deployment(ctx context.Context, name, impact, impactEN string) (*Finding, error) {
	var d appsv1.Deployment
	err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.ControlNamespace, Name: name}, &d)
	if apierrors.IsNotFound(err) {
		return &Finding{
			Key: "deployment/" + name, Severity: Critical, For: controlPlaneFor,
			Summary:   fmt.Sprintf("控制面组件 %s 不存在：%s", name, impact),
			SummaryEN: fmt.Sprintf("control-plane Deployment %s is missing: %s", name, impactEN),
			Hint:      "re-run the installer, or `sudo felis apply`, to recreate it",
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment %s: %w", name, err)
	}
	if d.Status.AvailableReplicas > 0 {
		return nil, nil
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return &Finding{
		Key: "deployment/" + name, Severity: Critical, For: controlPlaneFor,
		Summary:   fmt.Sprintf("控制面组件 %s 没有可用副本（%d/%d 就绪）：%s", name, d.Status.ReadyReplicas, want, impact),
		SummaryEN: fmt.Sprintf("control-plane Deployment %s has no available replica (%d/%d ready): %s", name, d.Status.ReadyReplicas, want, impactEN),
		Hint:      fmt.Sprintf("kubectl -n %s get pods; kubectl -n %s logs deploy/%s --all-containers", c.ControlNamespace, c.ControlNamespace, name),
	}, nil
}

// serverFinding reports a system server that should run and does not, and a
// user server that failed. A user server that is merely stopped or starting is
// its owner's business.
func serverFinding(ms *v1alpha1.MinecraftServer, ns string) *Finding {
	phase := ms.Status.Phase
	if phase == "" {
		phase = v1alpha1.PhaseUnknown
	}
	why := failureMessage(ms)
	if role := ms.Labels[v1alpha1.LabelSystemRole]; role != "" {
		if ms.Spec.DesiredState != v1alpha1.DesiredRunning || phase == v1alpha1.PhaseRunning {
			return nil
		}
		f := &Finding{
			Key: "system-server/" + ms.Name, Severity: Warning, For: systemServerFor,
			Summary:   fmt.Sprintf("系统服务器 %s 处于 %s（应为 Running）%s", ms.Name, phase, why),
			SummaryEN: fmt.Sprintf("system server %s is %s, not Running%s", ms.Name, phase, why),
			Hint:      fmt.Sprintf("kubectl -n %s describe minecraftserver %s; docs/troubleshooting.md §1-§2", ns, ms.Name),
		}
		if role == naming.SystemLoginServer {
			f.Severity = Critical
			f.Summary = fmt.Sprintf("登录门 %s 处于 %s（应为 Running），玩家无法进服%s", ms.Name, phase, why)
			f.SummaryEN = fmt.Sprintf("login gate %s is %s, not Running: players cannot join%s", ms.Name, phase, why)
		}
		return f
	}
	if phase != v1alpha1.PhaseFailed {
		return nil
	}
	return &Finding{
		Key: "server-failed/" + ms.Name, Severity: Warning, For: failedServerFor,
		Summary:   fmt.Sprintf("服务器 %s 启动失败（Failed）%s", ms.Name, why),
		SummaryEN: fmt.Sprintf("server %s is Failed%s", ms.Name, why),
		Hint:      fmt.Sprintf("kubectl -n %s describe minecraftserver %s; docs/troubleshooting.md §2", ns, ms.Name),
	}
}

// failureMessage is the first false condition's message, as ": <message>".
func failureMessage(ms *v1alpha1.MinecraftServer) string {
	for _, c := range ms.Status.Conditions {
		if c.Status == "False" && c.Message != "" {
			return ": " + c.Message
		}
	}
	return ""
}

// jobFinding reports a Job in the minecraft namespace (a world backup, a
// restore, a reaper run) that failed within jobFailureWindow, once.
func jobFinding(j *batchv1.Job, now time.Time) *Finding {
	for _, c := range j.Status.Conditions {
		if c.Type != batchv1.JobFailed || c.Status != corev1.ConditionTrue {
			continue
		}
		if now.Sub(c.LastTransitionTime.Time) > jobFailureWindow {
			return nil
		}
		kind, kindEN := "任务", "Job"
		switch {
		case strings.HasPrefix(j.Name, "backup-"):
			kind, kindEN = "世界备份任务", "world backup Job"
		case strings.HasPrefix(j.Name, "restore-"):
			kind, kindEN = "世界恢复任务", "world restore Job"
		case strings.HasPrefix(j.Name, platform.SAReaper+"-"):
			kind, kindEN = "世界回收任务", "world reaper Job"
		}
		reason := ""
		if c.Reason != "" {
			reason = " (" + c.Reason + ")"
		}
		return &Finding{
			Key: "job-failed/" + j.Name, Severity: Warning, Event: true,
			Summary:   fmt.Sprintf("%s %s 失败%s", kind, j.Name, reason),
			SummaryEN: fmt.Sprintf("%s %s failed%s", kindEN, j.Name, reason),
			Hint:      fmt.Sprintf("kubectl -n %s logs job/%s", j.Namespace, j.Name),
		}
	}
	return nil
}

// reaperFinding reports a reaper CronJob that has not succeeded for over a day:
// idle worlds are neither backed up nor released, and expired backups pile up.
func reaperFinding(cj *batchv1.CronJob, now time.Time) *Finding {
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		return nil
	}
	last := cj.CreationTimestamp.Time
	if cj.Status.LastSuccessfulTime != nil {
		last = cj.Status.LastSuccessfulTime.Time
	}
	if now.Sub(last) <= maxReaperAge {
		return nil
	}
	return &Finding{
		Key: "reaper-stale", Severity: Warning,
		Summary:   fmt.Sprintf("世界回收任务已超过 %s 没有成功运行", roundHours(now.Sub(last))),
		SummaryEN: fmt.Sprintf("the world reaper has not succeeded for %s", roundHours(now.Sub(last))),
		Hint:      fmt.Sprintf("kubectl -n %s get jobs; docs/troubleshooting.md §10", cj.Namespace),
	}
}

// nodeFindings reports a node that is not Ready or is under pressure: the
// kubelet is about to evict pods, or already is.
func nodeFindings(n *corev1.Node) []Finding {
	var out []Finding
	for _, c := range n.Status.Conditions {
		switch {
		case c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue:
			out = append(out, Finding{
				Key: "node/" + n.Name + "/NotReady", Severity: Critical, For: nodeNotReadyFor,
				Summary:   fmt.Sprintf("节点 %s 未就绪：%s", n.Name, c.Message),
				SummaryEN: fmt.Sprintf("node %s is not Ready: %s", n.Name, c.Message),
				Hint:      "systemctl status k3s; journalctl -u k3s -n 200",
			})
		case c.Type != corev1.NodeReady && c.Status == corev1.ConditionTrue &&
			(c.Type == corev1.NodeDiskPressure || c.Type == corev1.NodeMemoryPressure || c.Type == corev1.NodePIDPressure):
			out = append(out, Finding{
				Key: "node/" + n.Name + "/" + string(c.Type), Severity: Critical, For: nodePressureFor,
				Summary:   fmt.Sprintf("节点 %s 报告 %s，kubelet 正在驱逐 Pod", n.Name, c.Type),
				SummaryEN: fmt.Sprintf("node %s reports %s: the kubelet is evicting pods", n.Name, c.Type),
				Hint:      "docs/troubleshooting.md §13b",
			})
		}
	}
	return out
}

// KubeAPIDown is the finding for an API server that did not answer.
func KubeAPIDown(err error) Finding {
	return Finding{
		Key: "kube-api", Severity: Critical, For: kubeAPIFor,
		Summary:   "Kubernetes API 无法访问：面板、登录和所有服务器的状态都无法确认",
		SummaryEN: "the Kubernetes API is unreachable: the panel, sign-in and every server are in doubt",
		Hint:      fmt.Sprintf("systemctl status k3s; journalctl -u k3s -n 200 (%v)", err),
	}
}

// PostgresDown is the finding for a database that did not answer. It runs as
// the felis-postgres Deployment; the host reaches it on the loopback hostPort.
func PostgresDown(err error) Finding {
	ns := platform.DefaultControlNamespace
	return Finding{
		Key: "postgres", Severity: Critical, For: postgresFor,
		Summary:   "PostgreSQL 无法连接：登录、面板和服务器管理都会失败",
		SummaryEN: "PostgreSQL is unreachable: sign-in, the panel and server management fail",
		Hint: fmt.Sprintf("k3s kubectl -n %s get pods -l app.kubernetes.io/component=%s; k3s kubectl -n %s logs deploy/%s --tail=100 (%v)",
			ns, platform.ComponentPostgres, ns, platform.PostgresName, err),
	}
}

// BackupFinding reports a control-plane database backup older than a day, or
// none at all, in dir.
func BackupFinding(dir string, now time.Time) *Finding {
	bundles, err := dbbackup.List(dir)
	if err != nil {
		return &Finding{
			Key: "db-backup", Severity: Critical, For: backupFor,
			Summary:   fmt.Sprintf("无法读取数据库备份目录 %s", dir),
			SummaryEN: fmt.Sprintf("cannot read the database backup directory %s: %v", dir, err),
			Hint:      "docs/troubleshooting.md §16",
		}
	}
	if len(bundles) > 0 && now.Sub(bundles[0].Created) <= maxBackupAge {
		return nil
	}
	f := &Finding{
		Key: "db-backup", Severity: Critical, For: backupFor,
		Summary:   fmt.Sprintf("%s 里没有任何控制面数据库备份", dir),
		SummaryEN: fmt.Sprintf("no control-plane database backup in %s", dir),
		Hint:      "journalctl -u felis-db-backup -n 50; take one now with `sudo felis db backup` (docs/troubleshooting.md §16)",
	}
	if len(bundles) > 0 {
		age := roundHours(now.Sub(bundles[0].Created))
		f.Summary = fmt.Sprintf("最新的控制面数据库备份已是 %s 前（%s）", age, bundles[0].Name)
		f.SummaryEN = fmt.Sprintf("the newest control-plane database backup is %s old (%s)", age, bundles[0].Name)
	}
	return f
}

// OffsiteFinding reports an off-site copy that has not completed a clean run
// within offsite.StaleAfter, going by the record `felis offsite sync` leaves
// in statusFile. It is a warning: the local copies are intact, but a lost
// node would now lose what the bucket lacks, and the reaper keeps idle worlds
// on disk until their archives reach the bucket.
func OffsiteFinding(statusFile string, now time.Time) *Finding {
	const hint = "journalctl -u felis-offsite -n 50; `sudo felis offsite status` (docs/troubleshooting.md §16)"
	st, err := offsite.ReadStatus(statusFile)
	if err != nil {
		return &Finding{
			Key: "offsite", Severity: Warning, For: backupFor,
			Summary:   fmt.Sprintf("无法读取异地备份状态 %s", statusFile),
			SummaryEN: fmt.Sprintf("cannot read the off-site copy status %s: %v", statusFile, err),
			Hint:      hint,
		}
	}
	if st != nil && st.KeyMismatch {
		return &Finding{
			Key: "offsite", Severity: Warning, For: backupFor,
			Summary:   "异地备份已停止：桶里的对象是用另一把密钥加密的，本机不往桶里写入也不清理任何对象（" + st.LastError + "）",
			SummaryEN: "the off-site copy has stopped: the bucket's objects are sealed with another key, and this host writes and prunes nothing there (" + st.LastError + ")",
			Hint:      "`sudo felis offsite check-key`; set FELIS_OFFSITE_KEY in /etc/felis/offsite.env to the key the bucket was written with (docs/troubleshooting.md §16)",
		}
	}
	if st != nil && !st.LastSuccess.IsZero() && now.Sub(st.LastSuccess) <= offsite.StaleAfter {
		return nil
	}
	f := &Finding{
		Key: "offsite", Severity: Warning, For: backupFor,
		Summary:   "异地备份从未成功同步过，世界归档、数据库备份、用户镜像与上传的整合包只在本机",
		SummaryEN: "the off-site copy has never completed; world archives, database bundles, user images and uploaded modpacks exist on this machine only",
		Hint:      hint,
	}
	if st != nil && !st.LastSuccess.IsZero() {
		age := roundHours(now.Sub(st.LastSuccess))
		f.Summary = fmt.Sprintf("异地备份已有 %s 没有成功同步", age)
		f.SummaryEN = fmt.Sprintf("the off-site copy last completed %s ago", age)
	}
	if st != nil && st.LastError != "" {
		f.Summary += "（最近一次错误：" + st.LastError + "）"
		f.SummaryEN += " (last error: " + st.LastError + ")"
	}
	return f
}

// ScanDBFinding reports the build lane's tool copies (the vulnerability DBs in
// particular) not refreshed within maxScanDBAge, going by the record
// `felis mirror-build-tools` leaves in statusFile. A build still runs and its scan
// still gates, but against an old DB: a warning.
func ScanDBFinding(statusFile string, now time.Time) *Finding {
	const hint = "journalctl -u felis-build-tools -n 50; refresh now with `sudo felis mirror-build-tools` (docs/troubleshooting.md §8e)"
	st, err := imagepush.ReadMirrorStatus(statusFile)
	if err != nil {
		return &Finding{
			Key: "scan-db", Severity: Warning, For: backupFor,
			Summary:   fmt.Sprintf("无法读取构建工具镜像状态 %s", statusFile),
			SummaryEN: fmt.Sprintf("cannot read the build tools status %s: %v", statusFile, err),
			Hint:      hint,
		}
	}
	if st != nil && !st.LastSuccess.IsZero() && now.Sub(st.LastSuccess) <= maxScanDBAge {
		return nil
	}
	f := &Finding{
		Key: "scan-db", Severity: Warning, For: backupFor,
		Summary:   "漏洞库从未复制进内置 registry，构建的漏洞扫描无法运行",
		SummaryEN: "the vulnerability DB was never copied into the registry; build scans cannot run",
		Hint:      hint,
	}
	if st != nil && !st.LastSuccess.IsZero() {
		age := roundHours(now.Sub(st.LastSuccess))
		f.Summary = fmt.Sprintf("漏洞库已有 %s 没有刷新，构建扫描用的是过期数据", age)
		f.SummaryEN = fmt.Sprintf("the vulnerability DB was last refreshed %s ago; build scans use stale advisories", age)
	}
	if st != nil && st.LastError != "" {
		f.Summary += "（最近一次错误：" + st.LastError + "）"
		f.SummaryEN += " (last error: " + st.LastError + ")"
	}
	return f
}

// DiskFindings reports each filesystem under paths that is running out of
// space. Paths on one filesystem are reported once, under the first of them; a
// path that does not exist is skipped (a feature that is not in use).
func DiskFindings(paths []string) []Finding {
	var out []Finding
	seen := map[uint64]bool{}
	for _, p := range paths {
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
		free := float64(fs.Bavail) / float64(fs.Blocks)
		bsize := uint64(fs.Bsize) // uint32 on darwin
		avail := humanBytes(uint64(fs.Bavail) * bsize)
		switch {
		case free < diskCriticalRatio:
			out = append(out, Finding{
				Key: "disk/" + p, Severity: Critical, For: diskCriticalFor,
				Summary:   fmt.Sprintf("%s 所在磁盘只剩 %.1f%%（%s）：kubelet 即将驱逐游戏服务器", p, free*100, avail),
				SummaryEN: fmt.Sprintf("the filesystem holding %s is %.1f%% free (%s): the kubelet is about to evict game servers", p, free*100, avail),
				Hint:      "docs/troubleshooting.md §13b",
			})
		case free < diskLowRatio:
			out = append(out, Finding{
				Key: "disk/" + p, Severity: Warning, For: diskLowFor,
				Summary:   fmt.Sprintf("%s 所在磁盘只剩 %.1f%%（%s）", p, free*100, avail),
				SummaryEN: fmt.Sprintf("the filesystem holding %s is %.1f%% free (%s)", p, free*100, avail),
				Hint:      "docs/troubleshooting.md §13b",
			})
		}
	}
	return out
}

// MemoryFinding reports sustained low available memory from a /proc/meminfo
// style file. A file that cannot be read (not Linux) reports nothing.
func MemoryFinding(meminfo string) *Finding {
	f, err := os.Open(meminfo)
	if err != nil {
		return nil
	}
	defer f.Close()
	vals := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			vals[strings.TrimSuffix(fields[0], ":")] = v * 1024
		}
	}
	total, avail := vals["MemTotal"], vals["MemAvailable"]
	if total == 0 || float64(avail)/float64(total) >= memoryLowRatio {
		return nil
	}
	return &Finding{
		Key: "memory", Severity: Warning, For: memoryLowFor,
		Summary:   fmt.Sprintf("主机可用内存只剩 %s / %s：有 OOM 风险", humanBytes(avail), humanBytes(total)),
		SummaryEN: fmt.Sprintf("host memory available is %s of %s: processes risk being OOM-killed", humanBytes(avail), humanBytes(total)),
		Hint:      "PostgreSQL, the control plane, the registry and game servers share this node; stop idle servers or lower their memory",
	}
}

// roundHours prints a duration to the hour, "35h" rather than "35h0m0s".
func roundHours(d time.Duration) string {
	return strings.TrimSuffix(d.Round(time.Hour).String(), "0m0s")
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
