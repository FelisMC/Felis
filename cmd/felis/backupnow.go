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

// backupnow is the break-glass "back up a world now" op (§B4 "Sync"). Unlike halt —
// which writes the CRD directly — a backup needs felis-api's deployment coordinates
// (FELIS_IMAGE / FELIS_BACKUP_PVC) to render the one-shot backup Job, so the console
// cannot do it in-process. It POSTs the felis-api INTERNAL face (ops-token auth)
// while the API is alive, and the API renders the Job and audits the action. This file
// is the pure core (no bubbletea); tui_backupnow.go is the terminal glue. The
// `felis backup-now` command (cmdBackupNow, below) takes the same route for every
// user server in turn.

// backupNowOutcome is the durable result of a backup request, re-printed after the TUI
// alt-screen tears down.
type backupNowOutcome struct {
	name   string
	status string // "backing_up" on success
}

// resolveInternalAPI reads the two things the on-node console needs to reach the
// felis-api internal face: the felis-api-internal Service ClusterIP (the host's
// resolver is not CoreDNS, so the cluster-DNS name is useless here) and the ops
// token. Both live in the control namespace.
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

// requestBackup POSTs the internal backup endpoint and maps the response to a friendly
// outcome. osUser is sent for audit attribution (parity with halt); the API records it
// as the actor. A transport failure is distinguished from an HTTP error status because
// break-glass runs when things are broken — and this op needs the API alive by design,
// so "the API is down" is the useful message.
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

// backupErrorFromResponse turns a non-202 into a human message. The well-known codes get
// an operator-facing explanation; anything else falls back to the API's
// {"error":{code,message}} body, then the bare status code.
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
		// Two refusals share 409: the stopped gate and the missing-world-volume
		// gate. The body's code distinguishes them; a code-less body reads as the
		// stopped gate (the only 409 before the volume gate existed), and any other
		// coded 409 falls through to the API's own operator text.
		if e.Error.Code == "" || e.Error.Code == "not_stopped" {
			return fmt.Errorf("the server must be stopped before its world can be backed up — halt it first")
		}
	case http.StatusServiceUnavailable: // backup_unavailable
		return fmt.Errorf("the backup subsystem is not configured on felis-api (FELIS_IMAGE / FELIS_BACKUP_PVC unset)")
	case http.StatusNotFound:
		return fmt.Errorf("no such server")
	}
	if e.Error.Message != "" {
		return fmt.Errorf("felis-api: %s", e.Error.Message)
	}
	return fmt.Errorf("felis-api returned HTTP %d", resp.StatusCode)
}

// performBackupNow composes resolve + request against a short-timeout HTTP client (a
// non-routable ClusterIP must fail fast, not hang the TUI). controlNamespace holds the
// Service + token.
func performBackupNow(ctx context.Context, cl client.Client, controlNamespace, name, osUser string) (backupNowOutcome, error) {
	baseURL, token, err := resolveInternalAPI(ctx, cl, controlNamespace)
	if err != nil {
		return backupNowOutcome{}, err
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	return requestBackup(ctx, hc, baseURL, token, name, osUser)
}

// backupPickable narrows the backup picker to servers the backup API can accept.
// System servers (login/lobby) are excluded: they have no row in the servers
// table and carry reserved names, so every attempt dies in name validation —
// offering them would be a dead pick. The halt picker keeps them on purpose
// (break-glass retains full power over system servers); only the API-backed
// backup op cannot reach them.
func backupPickable(servers []haltableServer) []haltableServer {
	out := make([]haltableServer, 0, len(servers))
	for _, s := range servers {
		if !s.system {
			out = append(out, s)
		}
	}
	return out
}

// cmdBackupNow is `felis backup-now`: the world of every user server (or of the
// named ones) archived now, one at a time, through the internal backup route the
// console's Sync uses. A world lives only in its volume and the off-site copy holds
// only its archives, so this is the lever in front of a planned move to another
// host, a disk swap or anything else that could lose a volume.
func cmdBackupNow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup-now", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultSetupConfigPath, "path to felis.toml")
	yes := fs.Bool("yes", false, "back up; without it the plan is printed and nothing changes")
	stop := fs.Bool("stop", false, "stop the running servers first: their players are disconnected and the servers stay stopped")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: felis backup-now [-yes] [-stop] [server ...]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Archives the world of every user server, or of the named ones, one at a time, and waits for each archive.")
		fmt.Fprintln(stderr, "A running server is skipped unless -stop is given. Without -yes it prints what it would do.")
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
	hc := &http.Client{Timeout: 10 * time.Second}
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
			// The database is opened only once a server is to be stopped: the halt
			// is audited like the console's, and a run with nothing running needs
			// no more than the API.
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
	hasWorld bool   // its world volume exists
}

// listBackupNowWorlds lists the user servers of namespace in the API server's
// order (by name), each with what the backup route checks. System servers are
// left out: they have no row in the servers table, so the route refuses them
// (backupPickable).
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
		out = append(out, backupNowWorld{name: ms.Name, phase: phase, stopped: stopped, hasWorld: err == nil})
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

// errBackupAPIUnreachable is a backup request that never reached felis-api. It
// ends a backup-now run: every later world would fail the same way, and stopping
// servers for backups that cannot be taken only takes them away from players.
var errBackupAPIUnreachable = errors.New("felis-api unreachable (a backup needs it alive)")

// Polling of backup-now. A graceful stop saves the world first; the backup Job's
// own deadline (backupjob, 30 minutes) ends a Job that hangs, so its wait needs no
// cap of its own.
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
	default:
		return "skip: running (stop it first, or pass -stop)"
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
		if (w.stopped && w.hasWorld) || (!w.stopped && stop) {
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

	var done, failed, running, empty int
	var leftStopped []string
	for _, w := range worlds {
		if err := ctx.Err(); err != nil {
			break
		}
		switch {
		case w.stopped && !w.hasWorld:
			empty++
			continue
		case !w.stopped && !stop:
			running++
			continue
		}
		if !w.stopped {
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
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (running)", running))
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
	if failed > 0 || running > 0 || interrupted {
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
