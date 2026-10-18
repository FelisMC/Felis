package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/operator"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/store"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Request backups through the internal API, which owns Job configuration.

// backupNowOutcome is the durable result of a backup request, re-printed after the TUI
// alt-screen tears down.
type backupNowOutcome struct {
	name   string
	status string // "backing_up" on success
}

// resolveInternalAPI reads the internal Service ClusterIP and the ops token from the control namespace; cluster DNS does not resolve on the host.
func resolveInternalAPI(ctx context.Context, cl client.Client, controlNamespace string) (baseURL, token string, err error) {
	var svc corev1.Service
	if err := cl.Get(ctx, types.NamespacedName{Namespace: controlNamespace, Name: platform.APIInternalServiceName}, &svc); err != nil {
		return "", "", fmt.Errorf("get %s Service: %w", platform.APIInternalServiceName, err)
	}
	ip := svc.Spec.ClusterIP
	if ip == "" || ip == corev1.ClusterIPNone {
		return "", "", fmt.Errorf("%s Service has no ClusterIP yet", platform.APIInternalServiceName)
	}

	// The console's own token, which the api serves on the backup route alone.
	var sec corev1.Secret
	if err := cl.Get(ctx, types.NamespacedName{Namespace: controlNamespace, Name: naming.OpsTokenSecretName}, &sec); err != nil {
		return "", "", fmt.Errorf("get %s Secret (re-run the installer to create it): %w", naming.OpsTokenSecretName, err)
	}
	token = string(sec.Data[naming.ServiceTokenSecretKey])
	if token == "" {
		return "", "", fmt.Errorf("secret %s has no %s key", naming.OpsTokenSecretName, naming.ServiceTokenSecretKey)
	}

	return fmt.Sprintf("http://%s:%d", ip, platform.APIInternalPort), token, nil
}

// Distinguish transport failure from API rejection in recovery output.
func requestBackup(ctx context.Context, hc *http.Client, baseURL, token, name, osUser string) (backupNowOutcome, error) {
	body, _ := json.Marshal(map[string]string{"os_user": osUser})
	url := baseURL + "/api/v1/internal/servers/" + name + "/backup"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return backupNowOutcome{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return backupNowOutcome{}, fmt.Errorf("%w: %w", errBackupAPIUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		return backupNowOutcome{name: name, status: "backing_up"}, nil
	}
	return backupNowOutcome{}, backupErrorFromResponse(resp)
}

// Prefer known error codes, then the API message, then HTTP status when explaining backup rejection.
func backupErrorFromResponse(resp *http.Response) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(raw, &e)

	switch resp.StatusCode {
	case http.StatusConflict:
		// Several refusals share 409. A body without text means the first refusal (the only 409 before the others existed); the API's text tells the rest apart.
		if e.Error.Code == "" || (e.Error.Code == "not_stopped" && e.Error.Message == "") {
			return fmt.Errorf("the server is starting or stopping — back it up once it is running or stopped")
		}
	case http.StatusServiceUnavailable:
		switch e.Error.Code {
		case "live_backup_console":
			return fmt.Errorf("the server did not answer over its console (RCON), so it could not be backed up while running: run again, or pass -stop to stop it first")
		case "", "backup_unavailable":
			return fmt.Errorf("the backup subsystem is not configured on felis-api (FELIS_IMAGE / FELIS_BACKUP_PVC unset)")
		}
	case http.StatusNotFound:
		return fmt.Errorf("no such server")
	}
	if e.Error.Message != "" {
		return fmt.Errorf("felis-api: %s", e.Error.Message)
	}
	return fmt.Errorf("felis-api returned HTTP %d", resp.StatusCode)
}

// backupRequestTimeout bounds one backup request. A running server's request returns only after the game flushes its world (up to two minutes), so the wait covers the flush.
const backupRequestTimeout = 3 * time.Minute

// performBackupNow composes resolve + request. controlNamespace holds the Service +
// token.
func performBackupNow(ctx context.Context, cl client.Client, controlNamespace, name, osUser string) (backupNowOutcome, error) {
	baseURL, token, err := resolveInternalAPI(ctx, cl, controlNamespace)
	if err != nil {
		return backupNowOutcome{}, err
	}
	hc := &http.Client{Timeout: backupRequestTimeout}
	return requestBackup(ctx, hc, baseURL, token, name, osUser)
}

// backupPickable excludes system servers: they have no servers-table row and reserved names, so every backup attempt would fail validation. The halt picker keeps them on purpose.
func backupPickable(servers []haltableServer) []haltableServer {
	out := make([]haltableServer, 0, len(servers))
	for _, s := range servers {
		if !s.system {
			out = append(out, s)
		}
	}
	return out
}

func cmdBackupNow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup-now", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultSetupConfigPath, "path to felis.toml")
	yes := fs.Bool("yes", false, "back up; without it the plan is printed and nothing changes")
	stop := fs.Bool("stop", false, "stop the running servers first instead of backing them up live: their players are disconnected and the servers stay stopped")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: felis backup-now [-yes] [-stop] [server ...]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Archives the world of every user server, or of the named ones, one at a time, and waits for each archive.")
		fmt.Fprintln(stderr, "A running server is backed up live: the game stops saving while its world is copied, and players stay on.")
		fmt.Fprintln(stderr, "A server starting or stopping is skipped unless -stop is given. Without -yes it prints what it would do.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis backup-now: refused — it reads the cluster's ops token, so it must run as root (try: sudo felis backup-now)")
		return 1
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis backup-now: %v\n", err)
		return 1
	}
	cl, err := buildSystemServerClient()
	if err != nil {
		fmt.Fprintf(stderr, "felis backup-now: %v\n", err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ns := cfg.K8s.Namespace
	osUser := accountableOSUser()
	jobs := api.NewK8sJobStatus(cl, ns)
	var baseURL, token string
	var repo ownerStore
	var drv *store.PostgresDriver
	defer func() {
		if drv != nil {
			_ = drv.Close()
		}
	}()
	hc := &http.Client{Timeout: backupRequestTimeout}
	r := backupNowRun{
		out:  stdout,
		errw: stderr,
		ns:   ns,
		list: func(ctx context.Context) ([]backupNowWorld, error) { return listBackupNowWorlds(ctx, cl, ns) },
		stopped: func(ctx context.Context, name string) (bool, error) {
			var ms v1alpha1.MinecraftServer
			if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ms); err != nil {
				return false, err
			}
			return backupNowStopped(ctx, cl, &ms)
		},
		halt: func(ctx context.Context, name string) error {
			// The database opens only when a server must be stopped, since the halt is audited like the console's.
			if repo == nil {
				d, err := store.Open(ctx, cfg.Database.URL)
				if err != nil {
					return fmt.Errorf("open the database for the audit log: %w", err)
				}
				drv, repo = d, api.NewPGRepo(d.DB())
			}
			out, err := performHalt(ctx, cl, repo, ns, name, osUser)
			if err != nil {
				return err
			}
			if out.auditErr != nil {
				fmt.Fprintf(stderr, "felis backup-now: the audit row for stopping %s was not written: %v\n", name, out.auditErr)
			}
			return nil
		},
		request: func(ctx context.Context, name string) error {
			if baseURL == "" {
				var err error
				if baseURL, token, err = resolveInternalAPI(ctx, cl, platform.DefaultControlNamespace); err != nil {
					return fmt.Errorf("%w: %w", errBackupAPIUnreachable, err)
				}
			}
			_, err := requestBackup(ctx, hc, baseURL, token, name, osUser)
			return err
		},
		jobs:  jobs.LatestJobs,
		now:   time.Now,
		sleep: func(ctx context.Context, d time.Duration) { sleepCtx(ctx, d) },
	}
	return r.run(ctx, fs.Args(), *yes, *stop)
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// backupNowWorld is one user server as backup-now sees it.
type backupNowWorld struct {
	name     string
	phase    string // the observed phase, or the desired state before the operator reconciled it
	stopped  bool   // the backup route's stopped gate admits it
	live     bool   // up and ready: the backup route backs it up while it runs
	hasWorld bool   // its world volume exists
}

// Exclude system servers because the backup route requires a business-database row.
func listBackupNowWorlds(ctx context.Context, cl client.Client, namespace string) ([]backupNowWorld, error) {
	var list v1alpha1.MinecraftServerList
	if err := cl.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var out []backupNowWorld
	for i := range list.Items {
		ms := &list.Items[i]
		if isSystemServer(ms.Name) {
			continue
		}
		stopped, err := backupNowStopped(ctx, cl, ms)
		if err != nil {
			return nil, err
		}
		var pvc corev1.PersistentVolumeClaim
		err = cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: naming.WorldPVCName(ms.Name)}, &pvc)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("look up the world volume of %s: %w", ms.Name, err)
		}
		phase := string(ms.Status.Phase)
		if phase == "" {
			phase = string(ms.Spec.DesiredState)
		}
		live := ms.Spec.DesiredState == v1alpha1.DesiredRunning && ms.Status.Ready && ms.Status.Phase == v1alpha1.PhaseRunning
		out = append(out, backupNowWorld{name: ms.Name, phase: phase, stopped: stopped, live: live, hasWorld: err == nil})
	}
	return out, nil
}

// backupNowStopped is the backup route's stopped gate (api.enqueueBackup and
// K8sCluster.AcquireMaintenance together): desired Stopped, not ready, phase
// Stopped and no game pod left.
func backupNowStopped(ctx context.Context, cl client.Client, ms *v1alpha1.MinecraftServer) (bool, error) {
	if ms.Spec.DesiredState != v1alpha1.DesiredStopped || ms.Status.Ready || ms.Status.Phase != v1alpha1.PhaseStopped {
		return false, nil
	}
	var pods corev1.PodList
	if err := cl.List(ctx, &pods, client.InNamespace(ms.Namespace), client.MatchingLabels{
		v1alpha1.LabelServer: ms.Name, v1alpha1.LabelComponent: operator.ComponentValue,
	}); err != nil {
		return false, fmt.Errorf("look up the pod of %s: %w", ms.Name, err)
	}
	return len(pods.Items) == 0, nil
}

// errBackupAPIUnreachable ends a backup-now run: every later world would fail the same way, and stopping servers for backups that cannot be taken only takes them from players.
var errBackupAPIUnreachable = errors.New("felis-api unreachable (a backup needs it alive)")

// The backup Job's own 30-minute deadline (backupjob) ends a hung Job, so polling needs no cap of its own.
const (
	backupNowPoll      = 2 * time.Second
	backupNowStopWait  = 10 * time.Minute
	backupNowJobAppear = time.Minute
)

// backupNowRun is backup-now over seams, so the plan and the run are tested
// without a cluster or felis-api.
type backupNowRun struct {
	out, errw io.Writer
	ns        string // where the servers and their Jobs live, for the kubectl hints
	list      func(ctx context.Context) ([]backupNowWorld, error)
	stopped   func(ctx context.Context, name string) (bool, error)
	halt      func(ctx context.Context, name string) error
	request   func(ctx context.Context, name string) error
	jobs      func(ctx context.Context, name string) ([]api.AsyncJob, error)
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration)
}

// pickBackupNowWorlds narrows worlds to names, in the order given, or keeps them
// all when names is empty.
func pickBackupNowWorlds(worlds []backupNowWorld, names []string) ([]backupNowWorld, error) {
	if len(names) == 0 {
		return worlds, nil
	}
	byName := make(map[string]backupNowWorld, len(worlds))
	for _, w := range worlds {
		byName[w.name] = w
	}
	var out []backupNowWorld
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		w, ok := byName[n]
		switch {
		case ok:
			out = append(out, w)
		case isSystemServer(n):
			return nil, fmt.Errorf("%s is a system server: its world is rebuilt by felis setup and has no backups", n)
		default:
			return nil, fmt.Errorf("no server named %q", n)
		}
	}
	return out, nil
}

// backupNowAction is what the plan does with a world.
func backupNowAction(w backupNowWorld, stop bool) string {
	switch {
	case w.stopped && !w.hasWorld:
		return "skip: no world volume (never started, nothing to save)"
	case w.stopped:
		return "back up"
	case stop:
		return "stop, then back up"
	case w.live:
		return "back up while it runs (saving pauses for the copy)"
	default:
		return "skip: starting or stopping (run again later, or pass -stop)"
	}
}

func (r *backupNowRun) run(ctx context.Context, names []string, yes, stop bool) int {
	all, err := r.list(ctx)
	if err != nil {
		fmt.Fprintf(r.errw, "felis backup-now: list the servers: %v\n", err)
		return 1
	}
	worlds, err := pickBackupNowWorlds(all, names)
	if err != nil {
		fmt.Fprintf(r.errw, "felis backup-now: %v\n", err)
		return 2
	}
	if len(worlds) == 0 {
		fmt.Fprintln(r.out, "felis backup-now: there are no user servers")
		return 0
	}

	// The stopped worlds go first, so a felis-api that cannot take a backup is
	// found before any server is stopped for one.
	sort.SliceStable(worlds, func(i, j int) bool { return worlds[i].stopped && !worlds[j].stopped })
	width := 0
	for _, w := range worlds {
		width = max(width, len(w.name))
	}
	fmt.Fprintf(r.out, "felis backup-now: %d server(s), backed up one at a time:\n", len(worlds))
	stopping, work := false, 0
	for _, w := range worlds {
		fmt.Fprintf(r.out, "  %-*s  %-8s  %s\n", width, w.name, w.phase, backupNowAction(w, stop))
		stopping = stopping || (!w.stopped && stop)
		if (w.stopped && w.hasWorld) || (!w.stopped && (stop || w.live)) {
			work++
		}
	}
	if work > 0 {
		fmt.Fprintln(r.out, "Each archive is a manual backup: a server that already holds [archive] manual_keep of them loses its oldest.")
	}
	if stopping {
		fmt.Fprintln(r.out, "Stopping disconnects the players on those servers, and they stay stopped afterwards.")
	}
	if !yes {
		if work == 0 {
			fmt.Fprintln(r.out, "Nothing to back up.")
		} else {
			fmt.Fprintln(r.out, "Nothing changed. Run again with -yes to back them up.")
		}
		return 0
	}

	var done, failed, busy, empty int
	var leftStopped []string
	for _, w := range worlds {
		if err := ctx.Err(); err != nil {
			break
		}
		switch {
		case w.stopped && !w.hasWorld:
			empty++
			continue
		case !w.stopped && !stop && !w.live:
			busy++
			continue
		}
		if !w.stopped && stop {
			halted, err := r.stopWorld(ctx, w.name)
			if halted {
				leftStopped = append(leftStopped, w.name)
			}
			if err != nil {
				fmt.Fprintf(r.out, "  %s: failed: %v\n", w.name, err)
				failed++
				continue
			}
		}
		err := r.backUp(ctx, w.name)
		if errors.Is(err, errBackupAPIUnreachable) {
			fmt.Fprintf(r.out, "  %s: failed: %v\n", w.name, err)
			fmt.Fprintln(r.out, "Stopped: nothing more can be backed up until felis-api answers (kubectl -n felis get pods).")
			failed++
			break
		}
		if err != nil {
			fmt.Fprintf(r.out, "  %s: failed: %v\n", w.name, err)
			failed++
			continue
		}
		done++
	}

	interrupted := ctx.Err() != nil
	if interrupted {
		fmt.Fprintln(r.out, "Interrupted: a backup Job already started runs to its end.")
	}
	parts := []string{fmt.Sprintf("%d backed up", done)}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if busy > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (starting or stopping)", busy))
	}
	if empty > 0 {
		parts = append(parts, fmt.Sprintf("%d without a world", empty))
	}
	fmt.Fprintf(r.out, "%s.\n", strings.Join(parts, ", "))
	if len(leftStopped) > 0 {
		fmt.Fprintf(r.out, "Left stopped: %s. Start them from the panel when you are done.\n", strings.Join(leftStopped, ", "))
	}
	if done > 0 {
		fmt.Fprintln(r.out, "The archives reach the off-site bucket with the hourly copy; sudo systemctl start felis-offsite.service sends them now.")
	}
	if failed > 0 || busy > 0 || interrupted {
		return 1
	}
	return 0
}

// stopWorld stops one server and waits until the backup route would admit it.
// halted is whether the stop was asked for: the server then stays stopped, even
// when it takes longer than the wait.
func (r *backupNowRun) stopWorld(ctx context.Context, name string) (halted bool, err error) {
	fmt.Fprintf(r.out, "  %s: stopping\n", name)
	if err := r.halt(ctx, name); err != nil {
		return false, err
	}
	start := r.now()
	for {
		ok, err := r.stopped(ctx, name)
		if err != nil {
			return true, err
		}
		if ok {
			fmt.Fprintf(r.out, "  %s: stopped after %s\n", name, r.now().Sub(start).Round(time.Second))
			return true, nil
		}
		if r.now().Sub(start) >= backupNowStopWait {
			return true, fmt.Errorf("did not stop within %s (kubectl -n %s describe minecraftserver %s)", backupNowStopWait, r.ns, name)
		}
		if err := ctx.Err(); err != nil {
			return true, err
		}
		r.sleep(ctx, backupNowPoll)
	}
}

// backUp requests one world's backup and waits for its Job to finish. The Job is
// the one of this server that was not there before the request.
func (r *backupNowRun) backUp(ctx context.Context, name string) error {
	before, err := r.jobs(ctx, name)
	if err != nil {
		return fmt.Errorf("list its backup Jobs: %w", err)
	}
	known := make(map[string]bool, len(before))
	for _, j := range before {
		known[j.Name] = true
	}
	if err := r.request(ctx, name); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "  %s: backing up\n", name)
	start := r.now()
	seen := ""
	for {
		jobs, err := r.jobs(ctx, name)
		if err != nil {
			return fmt.Errorf("list its backup Jobs: %w", err)
		}
		var job *api.AsyncJob
		for i := range jobs {
			if jobs[i].Kind == "backup" && (jobs[i].Name == seen || seen == "" && !known[jobs[i].Name]) {
				job = &jobs[i]
				break
			}
		}
		switch {
		case job == nil && seen != "":
			return fmt.Errorf("its backup Job %s was deleted before it finished", seen)
		case job == nil && r.now().Sub(start) >= backupNowJobAppear:
			return fmt.Errorf("felis-api accepted the backup, but no backup Job appeared within %s (kubectl -n %s get jobs)", backupNowJobAppear, r.ns)
		case job != nil && job.State == "succeeded":
			fmt.Fprintf(r.out, "  %s: archived in %s\n", name, r.now().Sub(start).Round(time.Second))
			return nil
		case job != nil && job.State == "failed":
			msg := job.Message
			if msg == "" {
				msg = "the backup Job failed"
			}
			return fmt.Errorf("%s (kubectl -n %s logs job/%s)", msg, r.ns, job.Name)
		case job != nil:
			seen = job.Name
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		r.sleep(ctx, backupNowPoll)
	}
}
