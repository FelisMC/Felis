package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	felis "felis.lolicon.best"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/cfsetup"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/platform"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The panel's platform operations run here, on the controller host, through node-control:
// each is what the matching CLI command does, run as root, with its output in the task log.
// The node lifecycle (join, approve, fence...) stays in node_control.go.

// hostSettingsFile keeps the host settings the panel changed (Platform settings → Host);
// the installer takes them as its defaults on every later run (recall_host_settings).
const hostSettingsFile = "/etc/felis/host-settings.env"

// hostSettingKeys maps the panel's names to the installer's variables.
var hostSettingKeys = map[string]string{
	"dbBackupTime": "FELIS_DB_BACKUP_TIME",
	"dbBackupKeep": "FELIS_DB_BACKUP_KEEP",
	"velocityXmx":  "FELIS_VELOCITY_XMX",
}

func (e nodeExecutor) hostTask(ctx context.Context, r nodecontrol.Request, stage func(string) error, out io.Writer) error {
	p := r.Params
	switch r.Action {
	case "db-backup":
		if err := stage("database_backup"); err != nil {
			return err
		}
		return e.felis(ctx, out, "db", "backup", "-config", e.config, "-dir", dbbackup.DefaultDir, "-label", dbbackup.LabelManual)
	case "db-verify":
		if err := stage("verify"); err != nil {
			return err
		}
		return e.felis(ctx, out, "db", "verify", "-dir", dbbackup.DefaultDir, p["bundle"])
	case "db-delete":
		if err := stage("delete"); err != nil {
			return err
		}
		return e.felis(ctx, out, "db", "delete", "-dir", dbbackup.DefaultDir, p["bundle"])
	case "db-restore":
		return e.restoreDatabase(ctx, p["bundle"], stage, out)
	case "offsite-sync":
		if err := e.requireUnit(systemdUnitDir, "felis-offsite.service"); err != nil {
			return err
		}
		if err := stage("start"); err != nil {
			return err
		}
		return e.systemctl(ctx, out, "start", "--no-block", "felis-offsite.service")
	case "offsite-list":
		return e.felis(ctx, out, "offsite", "list", "-config", e.config)
	case "offsite-check-key":
		return e.felis(ctx, out, "offsite", "check-key", "-config", e.config)
	case "offsite-take-over":
		if err := stage("take_over"); err != nil {
			return err
		}
		if err := e.felis(ctx, out, "offsite", "take-over", "-config", e.config, "-yes"); err != nil {
			return err
		}
		return e.systemctl(ctx, out, "start", "--no-block", "felis-offsite.service")
	case "offsite-configure":
		return e.configureOffsite(ctx, r, stage, out)
	case "offsite-disable":
		return e.disableOffsite(ctx, stage, out)
	case "restore-host":
		args := []string{"restore-host", "-config", e.config, "-yes"}
		if b := p["bundle"]; b != "" {
			args = append(args, "-bundle", b)
		}
		if p["takeOver"] == "true" {
			args = append(args, "-take-over")
		}
		if p["force"] == "true" {
			args = append(args, "-force")
		}
		if err := stage("rebuild"); err != nil {
			return err
		}
		return e.felis(ctx, out, args...)
	case "backup-now":
		args := []string{"backup-now", "-config", e.config, "-yes"}
		if p["stop"] == "true" {
			args = append(args, "-stop")
		}
		if s := p["servers"]; s != "" {
			args = append(args, strings.Split(s, ",")...)
		}
		if err := stage("backup"); err != nil {
			return err
		}
		return e.felis(ctx, out, args...)
	case "rotate-token":
		if err := stage("rotate"); err != nil {
			return err
		}
		return e.felis(ctx, out, "rotate-token", "-config", e.config, "-yes", p["kind"])
	case "domain-check":
		return e.felis(ctx, out, "domain", "check")
	case "domain-set":
		if err := stage("domain"); err != nil {
			return err
		}
		return e.felis(ctx, out, "domain", "set", "-yes", p["domain"])
	case "storage-configure":
		return e.configureStorage(ctx, r, stage, out)
	case "connection-configure":
		return e.configureConnection(ctx, r, stage, out)
	case "converge":
		args := []string{"converge", "-config", e.config}
		if p["userRcon"] == "true" {
			args = append(args, "-user-rcon")
		}
		return e.felis(ctx, out, args...)
	case "doctor":
		return e.doctor(ctx, out)
	case "support-bundle":
		return e.supportBundle(ctx, p["serverLogs"] == "true", out)
	case "host-settings":
		return e.hostSettings(ctx, p, stage, out)
	case "server-forwarding":
		return e.serverForwarding(ctx, p["server"], p["legacy"] == "true", out)
	}
	return fmt.Errorf("unsupported host operation %q", r.Action)
}

// felis runs this binary with args, its output into the task log.
func (e nodeExecutor) felis(ctx context.Context, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, e.binary, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(), "KUBECONFIG="+e.kubeconfig)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("felis %s failed (%v); the log above says why", args[0], err)
	}
	return nil
}

func (e nodeExecutor) systemctl(ctx context.Context, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (e nodeExecutor) requireUnit(dir, unit string) error {
	if _, err := os.Stat(filepath.Join(dir, unit)); err != nil {
		if unit == "felis-offsite.service" {
			return errors.New("the off-site copy is not configured on this host")
		}
		return fmt.Errorf("%s is not installed on this host", unit)
	}
	return nil
}

// reconfigure runs the embedded installer for the named targets only
// (FELIS_INSTALL_MODE=reconfigure): the timers and units it owns are rewritten from the
// same functions a full run uses, with the host settings it recalls.
func (e nodeExecutor) reconfigure(ctx context.Context, out io.Writer, targets []string, env ...string) error {
	shell := e.shell
	if shell == "" {
		shell = "bash"
	}
	cmd := exec.CommandContext(ctx, shell, "-s")
	cmd.Stdin = strings.NewReader(felis.BootstrapScript())
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(append(os.Environ(),
		"FELIS_INSTALL_MODE=reconfigure", "FELIS_RECONFIGURE="+strings.Join(targets, ","),
		"FELIS_BOOTSTRAP_BINARY="+e.binary, "FELIS_NODE_CONTROL_TASK=1", "FELIS_NO_SETUP=1",
		"KUBECONFIG="+e.kubeconfig), env...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the installer could not apply the change (%v); the log above says why", err)
	}
	return nil
}

// restoreDatabase replaces the control-plane database with a bundle: the panel and the
// operator stop, the bundle is restored (the current database is bundled first), the
// migrations of this release are applied, and both start again. The task runs in
// node-control, which the stop does not touch; whatever fails, the control plane is
// started again so the panel can say what happened.
func (e nodeExecutor) restoreDatabase(ctx context.Context, bundle string, stage func(string) error, out io.Writer) (resultErr error) {
	if err := stage("verify"); err != nil {
		return err
	}
	if err := e.felis(ctx, out, "db", "verify", "-dir", dbbackup.DefaultDir, bundle); err != nil {
		return err
	}
	if err := stage("stop_control_plane"); err != nil {
		return err
	}
	deployments := []string{platform.SAAPI, platform.SAOperator}
	replicas := map[string]int32{}
	for _, d := range deployments {
		dep, err := e.cs.AppsV1().Deployments(e.controlNamespace).Get(ctx, d, metav1.GetOptions{})
		if err != nil {
			return err
		}
		replicas[d] = 1
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas > 1 {
			replicas[d] = *dep.Spec.Replicas
		}
	}
	defer func() {
		start, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := e.scaleControlPlane(start, replicas, out); err != nil && resultErr == nil {
			resultErr = err
		}
	}()
	if err := e.scaleControlPlane(ctx, map[string]int32{platform.SAAPI: 0, platform.SAOperator: 0}, out); err != nil {
		return err
	}
	if err := stage("restore_database"); err != nil {
		return err
	}
	if err := e.felis(ctx, out, "db", "restore", "-config", e.config, "-dir", dbbackup.DefaultDir, "-yes", bundle); err != nil {
		return err
	}
	if err := stage("migrate"); err != nil {
		return err
	}
	if err := e.felis(ctx, out, "migrate", "up", "-config", e.config); err != nil {
		return err
	}
	return stage("start_control_plane")
}

// scaleControlPlane sets the deployments' replicas and waits until each has that many
// ready (or none left, for zero).
func (e nodeExecutor) scaleControlPlane(ctx context.Context, want map[string]int32, out io.Writer) error {
	names := make([]string, 0, len(want))
	for d := range want {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, want[d])
		if _, err := e.cs.AppsV1().Deployments(e.controlNamespace).Patch(ctx, d, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %d replica(s)\n", d, want[d])
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		settled := true
		for _, d := range names {
			dep, err := e.cs.AppsV1().Deployments(e.controlNamespace).Get(ctx, d, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if want[d] == 0 && dep.Status.Replicas != 0 || want[d] > 0 && dep.Status.ReadyReplicas < want[d] {
				settled = false
			}
		}
		if settled {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the control plane did not reach its replica count: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// readEnvFile reads KEY='value' lines (the installer's quoting) into a map.
func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		}
		vals[strings.TrimSpace(k)] = v
	}
	return vals, sc.Err()
}

// writeEnvFile writes vals as KEY='value' lines under header, atomically, mode 0600.
func writeEnvFile(path, header string, vals map[string]string) error {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(header)
	for _, k := range keys {
		if strings.ContainsAny(vals[k], "'\n") {
			return fmt.Errorf("%s holds a character the file cannot carry", k)
		}
		fmt.Fprintf(&b, "%s='%s'\n", k, vals[k])
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".felis-env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(b.String()); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// configureOffsite points the off-site copy at a bucket: [offsite] in both host config
// files, the bucket's credentials (and the key that seals the copies) in offsite.env, the
// hourly timer through the installer, then a check that the bucket answers and its
// objects are sealed with this host's key. A key the bucket's objects are not sealed with
// is never put in place of the one already kept: the installer refuses it.
func (e nodeExecutor) configureOffsite(ctx context.Context, r nodecontrol.Request, stage func(string) error, out io.Writer) error {
	p, s := r.Params, r.Secrets
	if err := stage("configure"); err != nil {
		return err
	}
	keep := config.DefaultOffsiteDBKeep
	if v := p["dbKeep"]; v != "" {
		keep, _ = strconv.Atoi(v)
	}
	current, err := readEnvFile(defaultOffsiteEnvFile)
	if err != nil {
		return err
	}
	if (s["accessKey"] == "" || s["secretKey"] == "") && (current[config.DefaultOffsiteAccessKeyEnv] == "" || current[config.DefaultOffsiteSecretKeyEnv] == "") {
		return errors.New("the bucket's access key and secret key are required")
	}
	if err := writeOffsiteConfig(config.OffsiteConfig{Endpoint: p["endpoint"], Region: p["region"], Bucket: p["bucket"], Prefix: p["prefix"], DBKeep: keep}); err != nil {
		return err
	}
	if err := applyFelisConfigSecret(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "[offsite] written to the host configuration")
	var env []string
	if s["accessKey"] != "" {
		env = append(env, config.DefaultOffsiteAccessKeyEnv+"="+s["accessKey"], config.DefaultOffsiteSecretKeyEnv+"="+s["secretKey"])
	}
	if s["key"] != "" {
		env = append(env, config.DefaultOffsiteKeyEnv+"="+s["key"])
	}
	if err := stage("install_timer"); err != nil {
		return err
	}
	if err := e.reconfigure(ctx, out, []string{"offsite"}, env...); err != nil {
		return err
	}
	if err := stage("check_bucket"); err != nil {
		return err
	}
	return e.felis(ctx, out, "offsite", "check-key", "-config", e.config)
}

// disableOffsite drops [offsite] and the timer. offsite.env stays: it holds the key the
// copies already in the bucket are sealed with.
func (e nodeExecutor) disableOffsite(ctx context.Context, stage func(string) error, out io.Writer) error {
	if err := stage("configure"); err != nil {
		return err
	}
	if err := writeOffsiteConfig(config.OffsiteConfig{}); err != nil {
		return err
	}
	if err := applyFelisConfigSecret(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "[offsite] removed; the key stays in "+defaultOffsiteEnvFile)
	return e.reconfigure(ctx, out, []string{"offsite"})
}

func writeOffsiteConfig(o config.OffsiteConfig) error {
	for _, path := range []string{hostSetupConfigPath, podSetupConfigPath} {
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		cfg.Offsite = o
		if err := writeConfig(path, cfg); err != nil {
			return err
		}
	}
	return nil
}

// offsiteKey reads the key that seals this host's off-site copies, for the Owner to keep
// somewhere other than this machine.
func offsiteKey() (string, error) {
	vals, err := readEnvFile(defaultOffsiteEnvFile)
	if err != nil {
		return "", err
	}
	if vals[config.DefaultOffsiteKeyEnv] == "" {
		return "", errors.New("no off-site key is kept on this host")
	}
	return vals[config.DefaultOffsiteKeyEnv], nil
}

func (e nodeExecutor) configureStorage(ctx context.Context, r nodecontrol.Request, stage func(string) error, out io.Writer) error {
	if err := stage("configure"); err != nil {
		return err
	}
	method := storageLocal
	in := s3Inputs{}
	if r.Params["mode"] == "s3" {
		method = storageS3
		in = s3Inputs{endpoint: r.Params["endpoint"], bucket: r.Params["bucket"], region: r.Params["region"],
			accessKey: r.Secrets["accessKey"], secretKey: r.Secrets["secretKey"]}
		fmt.Fprintf(out, "checking s3://%s at %s\n", in.bucket, in.endpoint)
	}
	if err := applyStorageConfig(ctx, method, in); err != nil {
		return err
	}
	fmt.Fprintln(out, "uploads storage updated; felis-api restarted")
	return nil
}

// configureConnection changes how the panel is reached. local and reverse-proxy also take
// a Cloudflare edge down (connector, Access audience, the NodePort fence), so the change
// is whole; cloudflare builds the tunnel and Access through cfsetup, logging in to
// Cloudflare first when this host has never done so: the log then carries the URL to open.
func (e nodeExecutor) configureConnection(ctx context.Context, r nodecontrol.Request, stage func(string) error, out io.Writer) error {
	p := r.Params
	cfg, err := config.Load(hostSetupConfigPath)
	if err != nil {
		return err
	}
	panelHost, adminHost := p["panelHost"], p["adminHost"]
	if panelHost == "" {
		panelHost = cfg.Auth.PanelHostname
	}
	if adminHost == "" {
		adminHost = cfg.Auth.AdminHostname
	}
	if p["mode"] != "cloudflare" {
		if err := stage("configure"); err != nil {
			return err
		}
		if err := takeDownCloudflareEdge(ctx, out); err != nil {
			return err
		}
		if p["mode"] == "reverse-proxy" {
			return applyReverseProxy(ctx, panelHost, adminHost)
		}
		if err := writeConnectionConfig(panelHost, adminHost, "", ""); err != nil {
			return err
		}
		if err := applyFelisConfigSecret(ctx); err != nil {
			return err
		}
		if err := kubectl(ctx, "-n", "felis", "rollout", "restart", "deployment/felis-api"); err != nil {
			return err
		}
		return kubectl(ctx, "-n", "felis", "rollout", "status", "deployment/felis-api", "--timeout=180s")
	}
	// cloudflared and cfsetup keep the login certificate under $HOME/.cloudflared; a root
	// service has no HOME of its own.
	if os.Getenv("HOME") == "" {
		os.Setenv("HOME", "/root")
	}
	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		return errors.New("cloudflared is not installed on this host; rerun the installer, which installs it")
	}
	pre := cfsetup.DetectPreconditions(r.Secrets["apiToken"])
	if !pre.CertExists {
		if err := stage("cloudflare_login"); err != nil {
			return err
		}
		fmt.Fprintln(out, "Open the Cloudflare URL below and authorize this host; the task carries on once Cloudflare confirms.")
		if err := e.runLogged(ctx, out, bin, "tunnel", "login"); err != nil {
			return fmt.Errorf("cloudflared tunnel login: %w", err)
		}
		pre = cfsetup.DetectPreconditions(r.Secrets["apiToken"])
	}
	if err := stage("configure"); err != nil {
		return err
	}
	var id cfsetup.AccessIdentity
	if identity := p["identity"]; strings.HasPrefix(identity, "@") {
		id.EmailDomains = []string{strings.TrimPrefix(identity, "@")}
	} else {
		id.Emails = []string{identity}
	}
	params := cfsetup.Params{
		PanelHostname: panelHost, AdminHostname: adminHost, PanelOrigin: localPanelOrigin(),
		TunnelName: defaultTunnelName, ConfigPath: defaultTunnelConfigPath, AccessIdentity: id, Pre: pre,
		OnProgress: func(s string) { fmt.Fprintln(out, s) },
	}
	result, err := cfsetup.Setup(ctx, &cfsetup.ExecRunner{Cloudflared: bin, APIToken: r.Secrets["apiToken"], AccountID: p["accountID"]}, params)
	if err != nil {
		return err
	}
	if err := stage("apply"); err != nil {
		return err
	}
	if err := applyCloudflareEdge(ctx, result, panelHost, adminHost, bin); err != nil {
		return err
	}
	return nodecontrol.SetResult(ctx, map[string]any{"routedHostnames": result.RoutedHostnames, "tunnelID": result.TunnelID})
}

// takeDownCloudflareEdge stops the Felis connector and reopens the panel NodePort the
// edge fenced; a host that never had the edge has nothing to take down.
func takeDownCloudflareEdge(ctx context.Context, out io.Writer) error {
	if _, err := os.Stat(cloudflaredFelisUnit); err == nil {
		if err := systemctl(ctx, "disable", "--now", "cloudflared-felis.service"); err != nil {
			return err
		}
		fmt.Fprintln(out, "cloudflared-felis.service stopped and disabled")
	}
	if _, err := exec.LookPath("nft"); err == nil {
		if err := unfenceOriginNodePort(ctx); err != nil {
			return err
		}
	}
	return nil
}

// runLogged runs a command whose output is the task log, as it comes.
func (e nodeExecutor) runLogged(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(), "HOME=/root")
	return cmd.Run()
}

func (e nodeExecutor) doctor(ctx context.Context, out io.Writer) error {
	host, _ := os.Hostname()
	env := doctorEnv{unitDir: systemdUnitDir, run: hostCommand, now: time.Now(), host: host, panel: panelSettings}
	report := collectDoctor(ctx, env)
	printDoctor(out, report)
	return nodecontrol.SetResult(ctx, report.view())
}

// supportBundle collects the redacted bundle `felis support-bundle` writes and offers it
// for download.
func (e nodeExecutor) supportBundle(ctx context.Context, serverLogs bool, out io.Writer) error {
	started := time.Now().Add(-time.Second)
	dir := e.supportDir
	if dir == "" {
		dir = supportBundleDir
	}
	args := []string{"support-bundle", "-o", dir}
	if serverLogs {
		args = append(args, "-server-logs")
	}
	if err := e.felis(ctx, out, args...); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	newest, newestAt := "", time.Time{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(started) {
			continue
		}
		if info.ModTime().After(newestAt) {
			newest, newestAt = filepath.Join(dir, entry.Name()), info.ModTime()
		}
	}
	if newest == "" {
		return errors.New("the support bundle was not written")
	}
	return nodecontrol.SetArtifact(ctx, newest)
}

// hostSettings records the changed settings in host-settings.env, where every later
// installer run finds them, and has the installer rewrite what reads them now: the
// database backup timer, or the proxy's unit (which restarts it, disconnecting players).
func (e nodeExecutor) hostSettings(ctx context.Context, p map[string]string, stage func(string) error, out io.Writer) error {
	if err := stage("configure"); err != nil {
		return err
	}
	settings := e.settingsFile
	if settings == "" {
		settings = hostSettingsFile
	}
	vals, err := readEnvFile(settings)
	if err != nil {
		return err
	}
	var targets []string
	for name, value := range p {
		key := hostSettingKeys[name]
		if key == "" || value == "" {
			continue
		}
		if name == "dbBackupTime" {
			value = "*-*-* " + value + ":00"
		}
		vals[key] = value
		target := "db-backup"
		if name == "velocityXmx" {
			target = "velocity"
		}
		if !slices.Contains(targets, target) {
			targets = append(targets, target)
		}
	}
	sort.Strings(targets)
	if err := writeEnvFile(settings, "# Host settings changed in the panel (Platform settings → Host). The installer takes\n# them as its defaults; a variable set for a run still wins.\n", vals); err != nil {
		return err
	}
	return e.reconfigure(ctx, out, targets)
}

// serverForwarding sets or drops the legacy forwarding label on a user server, the
// switch https://docs.felismc.com/en/guide/servers#forwarding-mode documents with kubectl. The proxy picks it up at its next
// server-list refresh.
func (e nodeExecutor) serverForwarding(ctx context.Context, server string, legacy bool, out io.Writer) error {
	role, err := kubectlOutput(ctx, "-n", e.namespace, "get", "minecraftserver", server, "-o", "jsonpath={.metadata.labels.felis\\.lolicon\\.best/system-role}")
	if err != nil {
		return fmt.Errorf("server %s: %w", server, err)
	}
	if strings.TrimSpace(string(role)) != "" {
		return errors.New("the login space and the lobby keep modern forwarding")
	}
	label := v1alpha1.LabelForwarding + "-"
	if legacy {
		label = v1alpha1.LabelForwarding + "=" + v1alpha1.ForwardingLegacy
	}
	if err := kubectl(ctx, "-n", e.namespace, "label", "minecraftserver", server, label, "--overwrite"); err != nil {
		return err
	}
	if legacy {
		fmt.Fprintf(out, "%s now receives the player's identity BungeeCord-style; the proxy applies it at its next server-list refresh\n", server)
	} else {
		fmt.Fprintf(out, "%s is back on modern forwarding\n", server)
	}
	return nil
}

// hostQuiet holds the watchdog's mail until until, through the marker the installer
// writes while it restarts things; the zero time removes it.
func hostQuiet(path string) func(time.Time) error {
	return func(until time.Time) error {
		if until.IsZero() {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(strconv.FormatInt(until.Unix(), 10)+"\n"), 0644)
	}
}

var unitXmx = regexp.MustCompile(`-Xmx([0-9]+[MG])`)
