package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/store"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Persist restore steps for retry; domain and tunnel migration remain operator actions.

// restoreStateDir holds the fetched bundle and the progress record.
const restoreStateDir = "/var/lib/felis/restore-host"

const restoreStateFile = "state.json"

// The steps, in order.
const (
	stepFetchDB       = "fetch_db"
	stepImages        = "images"
	stepUploads       = "uploads"
	stepDatabase      = "database"
	stepServers       = "servers"
	stepWorlds        = "worlds"
	stepRestoreWorlds = "restore_worlds"
	stepTakeOver      = "take_over"
)

// restoreState is the progress record.
type restoreState struct {
	Bundle string          `json:"bundle,omitempty"`
	Done   map[string]bool `json:"done"`
	// Servers are the user servers the bundle brought back; Restored is the backup each
	// was restored from ("none" for a server without one) and Failed why one was not.
	Servers  []string          `json:"servers,omitempty"`
	Restored map[string]string `json:"restored,omitempty"`
	Failed   map[string]string `json:"failed,omitempty"`
}

func loadRestoreState(dir string) (*restoreState, error) {
	st := &restoreState{Done: map[string]bool{}, Restored: map[string]string{}, Failed: map[string]string{}}
	raw, err := os.ReadFile(filepath.Join(dir, restoreStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("the progress record %s does not parse: %w", filepath.Join(dir, restoreStateFile), err)
	}
	if st.Done == nil {
		st.Done = map[string]bool{}
	}
	if st.Restored == nil {
		st.Restored = map[string]string{}
	}
	if st.Failed == nil {
		st.Failed = map[string]string{}
	}
	return st, nil
}

func (st *restoreState) save(dir string) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, restoreStateFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, restoreStateFile))
}

// restoreRunner is what the steps act through; tests replace it.
type restoreRunner interface {
	// felis runs this binary with args, its output going to the operator.
	felis(ctx context.Context, args ...string) error
	// kubectl runs k3s kubectl with stdin.
	kubectl(ctx context.Context, stdin []byte, args ...string) error
	// serverCount is how many servers the database holds now.
	serverCount(ctx context.Context) (int, error)
	// restoreWorld restores a server's newest backup and waits for it, returning the
	// backup's id, or "" when the server has none.
	restoreWorld(ctx context.Context, server string) (string, error)
}

type restoreOptions struct {
	dir      string
	bundle   string // a bundle name, or "" for the newest
	takeOver bool
	force    bool
}

// runRestoreHost runs every step not yet done, recording each.
func runRestoreHost(ctx context.Context, r restoreRunner, o restoreOptions, st *restoreState, stdout io.Writer) error {
	step := func(name, title string, do func() error) error {
		if st.Done[name] {
			fmt.Fprintf(stdout, "felis restore-host: %s: done earlier\n", title)
			return nil
		}
		fmt.Fprintf(stdout, "felis restore-host: %s\n", title)
		if err := do(); err != nil {
			return fmt.Errorf("%s: %w", title, err)
		}
		st.Done[name] = true
		return st.save(o.dir)
	}
	if !st.Done[stepDatabase] && !o.force {
		n, err := r.serverCount(ctx)
		if err != nil {
			return fmt.Errorf("read this install's database: %w", err)
		}
		if n > 0 {
			return fmt.Errorf("this install already holds %d servers; restore-host rebuilds onto a fresh install and would replace them (pass -force to replace them anyway)", n)
		}
	}
	// A rerun naming another bundle than the one fetched fetches that one instead, while
	// the database is still untouched; once the database is restored from a bundle, the
	// rebuild stays on it.
	if o.bundle != "" && st.Done[stepFetchDB] && o.bundle != st.Bundle {
		if st.Done[stepDatabase] {
			return fmt.Errorf("the database was already restored from %s, so -bundle %s cannot replace it now (delete %s to start over)", st.Bundle, o.bundle, filepath.Join(o.dir, restoreStateFile))
		}
		st.Done[stepFetchDB] = false
	}
	if err := step(stepFetchDB, "fetching the newest database bundle from the bucket", func() error {
		name := o.bundle
		if name == "" {
			name = "latest"
		}
		if err := r.felis(ctx, "offsite", "fetch-db", "-dir", o.dir, name); err != nil {
			return err
		}
		// fetch-db writes the bundle under its own name; the directory may hold one fetched
		// before, so a named bundle is taken by its name.
		if o.bundle != "" {
			if _, err := os.Stat(filepath.Join(o.dir, o.bundle)); err != nil {
				return err
			}
			st.Bundle = o.bundle
			return nil
		}
		found, err := newestBundle(o.dir)
		if err != nil {
			return err
		}
		st.Bundle = found
		return nil
	}); err != nil {
		return err
	}
	bundle := filepath.Join(o.dir, st.Bundle)
	if _, err := os.Stat(bundle); err != nil {
		return fmt.Errorf("the fetched bundle %s is gone: %w (delete %s to start over)", bundle, err, filepath.Join(o.dir, restoreStateFile))
	}
	if err := step(stepImages, "pushing the user images back into the registry", func() error {
		return r.felis(ctx, "offsite", "fetch-images")
	}); err != nil {
		return err
	}
	if err := step(stepUploads, "putting the submission uploads back", func() error {
		return r.felis(ctx, "offsite", "fetch-uploads", "-adopt-volume")
	}); err != nil {
		return err
	}
	if err := step(stepDatabase, "restoring the database", func() error {
		for _, d := range []string{platform.SAAPI, platform.SAOperator} {
			if err := r.kubectl(ctx, nil, "-n", platform.DefaultControlNamespace, "scale", "deployment", d, "--replicas=0"); err != nil {
				return err
			}
		}
		if err := r.felis(ctx, "db", "restore", "-yes", "-no-safety-backup", bundle); err != nil {
			return err
		}
		if err := r.felis(ctx, "migrate", "up", "-config", hostSetupConfigPath); err != nil {
			return err
		}
		for _, d := range []string{platform.SAAPI, platform.SAOperator} {
			if err := r.kubectl(ctx, nil, "-n", platform.DefaultControlNamespace, "scale", "deployment", d, "--replicas=1"); err != nil {
				return err
			}
		}
		return r.kubectl(ctx, nil, "-n", platform.DefaultControlNamespace, "rollout", "status", "deployment/"+platform.SAAPI, "--timeout=300s")
	}); err != nil {
		return err
	}
	if err := step(stepServers, "bringing the servers back", func() error {
		raw, err := bundleEntry(bundle, "k8s/minecraftservers.json")
		if err != nil {
			return err
		}
		list, names, err := restorableServers(raw)
		if err != nil {
			return err
		}
		st.Servers = names
		if len(names) == 0 {
			return nil
		}
		return r.kubectl(ctx, list, "apply", "-f", "-")
	}); err != nil {
		return err
	}
	if err := step(stepWorlds, "fetching the world archives from the bucket", func() error {
		return r.felis(ctx, "offsite", "fetch-worlds")
	}); err != nil {
		return err
	}
	if err := step(stepRestoreWorlds, "restoring each server's newest backup", func() error {
		for _, name := range st.Servers {
			if _, done := st.Restored[name]; done {
				continue
			}
			id, err := r.restoreWorld(ctx, name)
			if err != nil {
				st.Failed[name] = err.Error()
				fmt.Fprintf(stdout, "  %s: %v\n", name, err)
			} else {
				delete(st.Failed, name)
				if id == "" {
					id = "none"
				}
				st.Restored[name] = id
				fmt.Fprintf(stdout, "  %s: restored backup %s\n", name, id)
			}
			if err := st.save(o.dir); err != nil {
				return err
			}
		}
		if len(st.Failed) > 0 {
			return fmt.Errorf("%d servers could not be restored; run felis restore-host again to retry them", len(st.Failed))
		}
		return nil
	}); err != nil {
		return err
	}
	if o.takeOver {
		if err := step(stepTakeOver, "making this host the one that writes the bucket", func() error {
			return r.felis(ctx, "offsite", "take-over", "-yes")
		}); err != nil {
			return err
		}
	}
	return nil
}

// newestBundle names the newest database bundle in dir.
func newestBundle(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "felis-db-*.tar"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no bundle was written to %s", dir)
	}
	sort.Strings(matches) // the names start with their UTC stamp
	return filepath.Base(matches[len(matches)-1]), nil
}

// bundleEntry reads one file out of a bundle.
func bundleEntry(bundle, name string) ([]byte, error) {
	f, err := os.Open(bundle)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s holds no %s; the servers come from the newest bundle `felis offsite list` shows without that gap (felis restore-host -bundle <name>)", filepath.Base(bundle), name)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == name {
			return io.ReadAll(io.LimitReader(tr, 64<<20))
		}
	}
}

// Restore user servers stopped, without old node placement, claims, or locks.
func restorableServers(raw []byte) ([]byte, []string, error) {
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil, fmt.Errorf("parse the bundle's servers: %w", err)
	}
	var kept []map[string]any
	var names []string
	for _, it := range list.Items {
		md, _ := it["metadata"].(map[string]any)
		if md == nil {
			continue
		}
		if labels, _ := md["labels"].(map[string]any); labels != nil && labels[v1alpha1.LabelSystemRole] != nil {
			continue
		}
		if ann, _ := md["annotations"].(map[string]any); ann != nil {
			for _, k := range []string{maintenance.Annotation, distributed.MigrationAnnotation, v1alpha1.AnnotationLiveBackup, v1alpha1.AnnotationRestart, v1alpha1.AnnotationStartRetry} {
				delete(ann, k)
			}
		}
		spec, _ := it["spec"].(map[string]any)
		if spec == nil {
			spec = map[string]any{}
			it["spec"] = spec
		}
		spec["desiredState"] = string(v1alpha1.DesiredStopped)
		delete(spec, "nodeName")
		if storage, _ := spec["storage"].(map[string]any); storage != nil {
			delete(storage, "claimName")
		}
		name, _ := md["name"].(string)
		if name == "" {
			continue
		}
		names = append(names, name)
		kept = append(kept, it)
	}
	if kept == nil {
		kept = []map[string]any{}
	}
	out, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": kept})
	return out, names, err
}

func cmdRestoreHost(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore-host", flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "rebuild; without it the steps are printed and nothing changes")
	bundle := fs.String("bundle", "", "a bundle name from `felis offsite list` instead of the newest")
	takeOver := fs.Bool("take-over", false, "make this host the one that writes the bucket once the rebuild is done (skip it on a rehearsal machine)")
	force := fs.Bool("force", false, "restore over an install that already holds servers")
	dir := fs.String("dir", restoreStateDir, "where the bundle and the progress record are kept")
	cfgPath := fs.String("config", defaultSetupConfigPath, "path to felis.toml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if !*yes {
		fmt.Fprintln(stdout, "felis restore-host rebuilds this install from its off-site copy:")
		for _, s := range []string{
			"fetch the newest database bundle from the bucket (or -bundle <name>)",
			"push the user images back into the registry and put the submission uploads back",
			"restore the database over this fresh install's and migrate it",
			"bring the user servers back, stopped",
			"fetch the world archives and restore each server's newest backup",
			"with -take-over, make this host the one that writes the bucket",
		} {
			fmt.Fprintf(stdout, "  - %s\n", s)
		}
		fmt.Fprintln(stdout, "Run it again with -yes to do it. The bucket and its key come from this install's [offsite] settings.")
		return 0
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis restore-host: run it as root (sudo felis restore-host -yes)")
		return 1
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis restore-host: %v\n", err)
		return 1
	}
	if !cfg.Offsite.Enabled() {
		fmt.Fprintln(stderr, "felis restore-host: this install has no [offsite] bucket; reinstall with FELIS_OFFSITE_ENDPOINT, FELIS_OFFSITE_BUCKET, FELIS_OFFSITE_ACCESS_KEY, FELIS_OFFSITE_SECRET_KEY and FELIS_OFFSITE_KEY set to the old install's")
		return 1
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "felis restore-host: %v\n", err)
		return 1
	}
	st, err := loadRestoreState(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "felis restore-host: %v\n", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "felis restore-host: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()
	r := &hostRestoreRunner{exe: exe, cfg: cfg, stdout: stdout, stderr: stderr}
	runErr := runRestoreHost(ctx, r, restoreOptions{dir: *dir, bundle: *bundle, takeOver: *takeOver, force: *force}, st, stdout)
	printRestoreSummary(stdout, st, cfg.Server.RootDomain, *takeOver)
	if runErr != nil {
		fmt.Fprintf(stderr, "felis restore-host: %v\n", runErr)
		return 1
	}
	return 0
}

// printRestoreSummary says what came back and what is left to the operator.
func printRestoreSummary(w io.Writer, st *restoreState, rootDomain string, tookOver bool) {
	fmt.Fprintf(w, "\nfelis restore-host: bundle %s\n", orUnknown(st.Bundle))
	fmt.Fprintf(w, "  servers brought back: %d\n", len(st.Servers))
	for _, name := range st.Servers {
		switch {
		case st.Restored[name] == "none":
			fmt.Fprintf(w, "    %s: no backup was recorded, so its world starts empty\n", name)
		case st.Restored[name] != "":
			fmt.Fprintf(w, "    %s: world restored from backup %s\n", name, st.Restored[name])
		case st.Failed[name] != "":
			fmt.Fprintf(w, "    %s: not restored: %s\n", name, st.Failed[name])
		default:
			fmt.Fprintf(w, "    %s: not restored yet\n", name)
		}
	}
	fmt.Fprintln(w, "  Left to do by hand (https://docs.felismc.com/en/operations/backup#rebuild-checks):")
	fmt.Fprintf(w, "    - the install answers at %s; point your own domain here and run `felis domain set`, or keep this address (passkeys registered on the old address stop working; users sign in with an email code and add a new one)\n", rootDomain)
	fmt.Fprintln(w, "    - bring back the Cloudflare tunnel with `felis setup` if the old host served the panel through one")
	if !tookOver {
		fmt.Fprintln(w, "    - when this host is to replace the old one for good: sudo felis offsite take-over -yes")
	}
}

// hostRestoreRunner is the real restoreRunner.
type hostRestoreRunner struct {
	exe            string
	cfg            *config.Config
	stdout, stderr io.Writer
}

func (h *hostRestoreRunner) felis(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, h.exe, args...)
	cmd.Stdout, cmd.Stderr = h.stdout, h.stderr
	return cmd.Run()
}

func (h *hostRestoreRunner) kubectl(ctx context.Context, stdin []byte, args ...string) error {
	cmd := exec.CommandContext(ctx, "k3s", append([]string{"kubectl"}, args...)...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+hostBootstrapKubeconfigPath)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout, cmd.Stderr = h.stdout, h.stderr
	return cmd.Run()
}

func (h *hostRestoreRunner) serverCount(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, h.cfg.Database.URL)
	if err != nil {
		return 0, err
	}
	defer drv.Close()
	var n int
	err = drv.DB().QueryRowContext(ctx, `SELECT count(*) FROM servers WHERE deleted_at IS NULL`).Scan(&n)
	return n, err
}

func (h *hostRestoreRunner) restoreWorld(ctx context.Context, server string) (string, error) {
	cl, err := buildSystemServerClient()
	if err != nil {
		return "", err
	}
	base, token, err := resolveInternalAPI(ctx, cl, platform.DefaultControlNamespace)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/internal/servers/"+server+"/restore", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		BackupID string `json:"backup_id"`
		Error    struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
	switch {
	case resp.StatusCode == http.StatusNotFound && body.Error.Code == "no_backup":
		return "", nil
	case resp.StatusCode != http.StatusAccepted:
		return "", fmt.Errorf("the API refused the restore (%d %s: %s)", resp.StatusCode, body.Error.Code, body.Error.Message)
	}
	return body.BackupID, waitRestoreJob(ctx, cl, h.cfg.K8s.Namespace, server)
}

// waitRestoreJob waits for the server's restore Job to finish.
func waitRestoreJob(ctx context.Context, cl client.Client, namespace, server string) error {
	deadline := time.Now().Add(2 * time.Hour)
	for {
		var jobs batchv1.JobList
		if err := cl.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{maintenance.LabelServer: server, maintenance.LabelManagedBy: "felis-restore"}); err != nil {
			return err
		}
		running := false
		for i := range jobs.Items {
			j := &jobs.Items[i]
			if !maintenance.JobFinished(j) {
				running = true
				continue
			}
			for _, c := range j.Status.Conditions {
				if c.Type == batchv1.JobFailed && c.Status == "True" {
					return fmt.Errorf("the restore Job %s failed: %s", j.Name, strings.TrimSpace(c.Message))
				}
			}
		}
		if !running && len(jobs.Items) > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the restore did not finish within two hours")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
