package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/watchdog"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The secrets a bundle host holds, each of which must be nowhere in the bundle.
var plantedSecrets = map[string]string{
	"secrets.env SERVICE_TOKEN":     "svc-token-planted-0a1b2c3d",
	"secrets.env DB_PASSWORD":       "db-pass-planted-4e5f6a7b",
	"offsite.env FELIS_OFFSITE_KEY": "offsite-key-planted+8c9d/0e1f=",
	"smtp-password":                 "smtp-pass-planted-2a3b",
	"felis-link.properties token":   "props-token-planted-4c5d",
	"k3s token secret part":         "k3s-secret-part-planted-6e7f",
	"watchdog state relay password": "cached-relay-pw-planted-8a9b",
	"pod env literal":               "env-literal-planted-0c1d",
	"deployment env literal":        "deploy-env-planted-2e3f",
	"MinecraftServer env literal":   "cr-env-planted-4a5b",
	"last-applied annotation":       "annotation-planted-6c7d",
	"logged password":               "hunter2-planted-8e9f",
	"logged bearer":                 "bearerplanted0a1b2c3d",
	"URL password":                  "urlpass-planted-4e5f",
	"token in an error":             "errtoken-planted-0f1e",
	"felis.host.toml DB password":   "cfg-db-pass-planted-1c2d",
	"service token by its env ref":  "env-ref-token-planted-3e4f",
	"init container env literal":    "init-env-planted-5a6b",
	"ephemeral container env":       "ephemeral-env-planted-7c8d",
	"statefulset env literal":       "sts-env-planted-9e0f",
	"job env literal":               "job-env-planted-1a2b",
	"cronjob env literal":           "cronjob-env-planted-3c4d",
	"commented-out offsite key":     "old-offsite-key-planted-5e6f",
	"doctor quoting a password":     "doctor-pw-planted-7a8b",
	"status quoting a password":     "status-pw-planted-9c0d",
	"journal quoting a password":    "journal-pw-planted-1e2f",
}

// bundleHost is a Felis host for felis support-bundle: its secret files, its
// watchdog unit and state, a cluster with a control-plane pod that restarted
// and a game server, and logs and a journal that name secrets.
func bundleHost(t *testing.T) *supportBundle {
	t.Helper()
	p := plantedSecrets
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc-felis")
	for _, d := range []string{etc, filepath.Join(dir, "systemd"), filepath.Join(dir, "k3s")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(etc, "secrets.env"), "SERVICE_TOKEN="+p["secrets.env SERVICE_TOKEN"]+"\nDB_PASSWORD="+p["secrets.env DB_PASSWORD"]+"\nSHORT=abc\n", 0o600)
	writeTestFile(t, filepath.Join(etc, "offsite.env"), "# before the rotation\n# FELIS_OFFSITE_KEY="+p["commented-out offsite key"]+"\nFELIS_OFFSITE_KEY='"+p["offsite.env FELIS_OFFSITE_KEY"]+"'\n", 0o600)
	writeTestFile(t, filepath.Join(etc, "smtp-password"), p["smtp-password"]+"\n", 0o600)
	writeTestFile(t, filepath.Join(etc, "felis-link.properties"), "api-base-url=http://10.43.0.10:8081\nservice-token="+p["felis-link.properties token"]+"\nroot-domain=games.example.org\n", 0o640)
	writeTestFile(t, filepath.Join(dir, "k3s", "token"), "K10deadbeefcafe::server:"+p["k3s token secret part"]+"\n", 0o600)
	cfgPath := filepath.Join(etc, "felis.host.toml")
	writeTestFile(t, cfgPath, "[database]\nurl = \"postgres://felis:"+p["felis.host.toml DB password"]+"@127.0.0.1:1/felis?sslmode=disable&connect_timeout=1\"\n"+
		"[server]\nroot_domain = \"games.example.org\"\n[archive]\nstore = \"tarLocal\"\n[k8s]\negress_mode = \"nodeport\"\n"+
		"[velocity]\nservice_token_ref = \"FELIS_BUNDLE_TEST_SERVICE_TOKEN\"\n", 0o600)
	t.Setenv("FELIS_BUNDLE_TEST_SERVICE_TOKEN", p["service token by its env ref"])
	statePath := filepath.Join(dir, "state.json")
	if err := watchdog.SaveState(statePath, &watchdog.State{SMTPPassword: p["watchdog state relay password"]}); err != nil {
		t.Fatal(err)
	}
	unitDir := filepath.Join(dir, "systemd")
	writeTestFile(t, filepath.Join(unitDir, "felis-watchdog.service"), "[Service]\nExecStart=/usr/local/bin/felis watchdog -config "+cfgPath+" -state "+statePath+"\n", 0o644)
	writeTestFile(t, filepath.Join(unitDir, "felis-offsite.service"), "[Unit]\n", 0o644)
	writeTestFile(t, filepath.Join(unitDir, "k3s.service"), "[Unit]\n", 0o644)
	meminfo := filepath.Join(dir, "meminfo")
	writeTestFile(t, meminfo, "MemTotal: 8000000 kB\nMemAvailable: 2000000 kB\n", 0o644)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var w watchdogFlags
	w, _, err = watchdogUnitFlags(filepath.Join(unitDir, "felis-watchdog.service"))
	if err != nil {
		t.Fatal(err)
	}
	w.diskPaths = "/nonexistent-felis-bundle-test"

	secretEnv := corev1.EnvVar{Name: "FELIS_SMTP_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "felis-smtp"}, Key: "password"}}}
	replicas := int32(1)
	cl := fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithObjects(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "felis-api-7d9", Namespace: "felis",
				Annotations:   map[string]string{corev1.LastAppliedConfigAnnotation: `{"env":"` + p["last-applied annotation"] + `"}`, "felis.lolicon.best/kept": "yes"},
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl-client-side-apply"}},
			},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "wait-db", Image: "busybox", Env: []corev1.EnvVar{{Name: "FELIS_INIT_SETTING", Value: p["init container env literal"]}}}},
				Containers: []corev1.Container{{Name: "api", Image: "felis-api:v1", Env: []corev1.EnvVar{
					{Name: "FELIS_PLAIN_SETTING", Value: p["pod env literal"]}, secretEnv,
				}}},
				EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "debug", Env: []corev1.EnvVar{{Name: "FELIS_DEBUG_SETTING", Value: p["ephemeral container env"]}}}}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: 1, Ready: true}}},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "felis-api", Namespace: "felis"},
			Spec: appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "felis-api"}},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Env: []corev1.EnvVar{
					{Name: "FELIS_DEPLOY_SETTING", Value: p["deployment env literal"]},
				}}}}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "survival-0", Namespace: "minecraft"},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "prepare-data"}, {Name: "egress-gate"}},
				Containers:     []corev1.Container{{Name: "server"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&v1alpha1.MinecraftServer{
			ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft"},
			Spec:       v1alpha1.MinecraftServerSpec{Env: []v1alpha1.EnvVar{{Name: "DISCORD_WEBHOOK", Value: p["MinecraftServer env literal"]}}},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "felis-postgres", Namespace: "felis"},
			Spec:       appsv1.StatefulSetSpec{Template: envTemplate("FELIS_STS_SETTING", p["statefulset env literal"])},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "build-1", Namespace: "felis-build"},
			Spec:       batchv1.JobSpec{Template: envTemplate("FELIS_JOB_SETTING", p["job env literal"])},
		},
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "felis-reaper", Namespace: "felis"},
			Spec:       batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: envTemplate("FELIS_CRON_SETTING", p["cronjob env literal"])}}},
		},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "felis-1"}},
	).Build()

	secretLog := fmt.Sprintf("started with SERVICE_TOKEN=%s\nlogin password=%s ok\nAuthorization: Bearer %s\ndial postgres://felis:%s@db:5432/felis\n",
		p["secrets.env SERVICE_TOKEN"], p["logged password"], p["logged bearer"], p["URL password"])
	return &supportBundle{
		host: "felis-test", now: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), unitDir: unitDir,
		w: w, cfg: cfg, cl: cl,
		logs: func(_ context.Context, ns, pod, container string, previous bool) ([]byte, error) {
			if container == "wait-db" {
				return nil, errors.New("container \"wait-db\" is waiting to start; token=" + p["token in an error"])
			}
			return []byte(fmt.Sprintf("log of %s/%s/%s previous=%v\n%s", ns, pod, container, previous, secretLog)), nil
		},
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "journalctl":
				return []byte("journal of " + args[1] + "\nFELIS_OFFSITE_KEY=" + p["offsite.env FELIS_OFFSITE_KEY"] + "\nrelay " + p["smtp-password"] + " refused\n" +
					"relay login with the cached " + p["watchdog state relay password"] + "\ndatabase auth failed for " + p["felis.host.toml DB password"] +
					"\nservice token " + p["service token by its env ref"] + " rejected\nnode joined with " + p["k3s token secret part"] + "\n" +
					"old copies sealed with " + p["commented-out offsite key"] + "\nretrying with password=" + p["journal quoting a password"] + "\n"), nil
			case "systemctl", "df", "ip":
				return []byte(name + " output\n"), nil
			}
			return nil, errors.New("unexpected " + name)
		},
		backups: func(context.Context) (map[string]time.Time, error) {
			return nil, errors.New("connect: password=" + p["status quoting a password"])
		},
		doctor: func(_ context.Context, out io.Writer) {
			fmt.Fprintf(out, "doctor report; the k3s token K10deadbeefcafe::server:%s leaked here\nprobe said password=%s\n", p["k3s token secret part"], p["doctor quoting a password"])
		},
		secrets: secretSources{
			envFiles:   []string{filepath.Join(etc, "secrets.env"), filepath.Join(etc, "offsite.env"), filepath.Join(etc, "absent.env")},
			valueFiles: []string{filepath.Join(etc, "smtp-password"), filepath.Join(etc, "uploads-s3-secret-key")},
			propsFiles: []string{filepath.Join(etc, "felis-link.properties")},
			tokenFiles: []string{filepath.Join(dir, "k3s", "token")},
		},
		logLines: 500, since: 48 * time.Hour,
		meminfo: meminfo, osRelease: filepath.Join(dir, "os-release"), procVersion: filepath.Join(dir, "version"), stateDir: etc,
	}
}

// envTemplate is a pod template whose one container sets name to a literal.
func envTemplate(name, value string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Env: []corev1.EnvVar{{Name: name, Value: value}}}}}}
}

// readBundle is every file in the bundle at path, by its name inside the
// bundle's directory, and each one's mode.
func readBundle(t *testing.T, path string) (map[string]string, map[string]int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files, modes := map[string]string{}, map[string]int64{}
	prefix := strings.TrimSuffix(filepath.Base(path), ".tar.gz") + "/"
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name, ok := strings.CutPrefix(hdr.Name, prefix)
		if !ok {
			t.Errorf("%s is outside the bundle's directory %s", hdr.Name, prefix)
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[name], modes[name] = string(raw), hdr.Mode
	}
	return files, modes
}

func TestSupportBundle(t *testing.T) {
	b := bundleHost(t)
	out := filepath.Join(t.TempDir(), "support")
	path, err := b.write(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(out, "felis-support-felis-test-20260927T120000Z.tar.gz"); path != want {
		t.Errorf("path %s, want %s", path, want)
	}
	for p, want := range map[string]os.FileMode{out: 0o700 | os.ModeDir, path: 0o600} {
		if st, err := os.Stat(p); err != nil || st.Mode() != want {
			t.Errorf("%s: mode %v (err %v), want %v", p, st.Mode(), err, want)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(out, ".*partial")); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}

	files, modes := readBundle(t, path)
	var names []string
	for n := range files {
		names = append(names, n)
		if modes[n] != 0o600 {
			t.Errorf("%s: mode %o in the archive, want 600", n, modes[n])
		}
	}
	sort.Strings(names)
	want := []string{
		"MANIFEST.txt",
		"cluster/felis-build/cronjobs.yaml", "cluster/felis-build/deployments.yaml", "cluster/felis-build/events.yaml", "cluster/felis-build/jobs.yaml",
		"cluster/felis-build/networkpolicies.yaml", "cluster/felis-build/persistentvolumeclaims.yaml", "cluster/felis-build/pods.yaml",
		"cluster/felis-build/services.yaml", "cluster/felis-build/statefulsets.yaml",
		"cluster/felis/cronjobs.yaml", "cluster/felis/deployments.yaml", "cluster/felis/events.yaml", "cluster/felis/jobs.yaml",
		"cluster/felis/networkpolicies.yaml", "cluster/felis/persistentvolumeclaims.yaml", "cluster/felis/pods.yaml",
		"cluster/felis/services.yaml", "cluster/felis/statefulsets.yaml",
		"cluster/minecraft/cronjobs.yaml", "cluster/minecraft/deployments.yaml", "cluster/minecraft/events.yaml", "cluster/minecraft/jobs.yaml",
		"cluster/minecraft/networkpolicies.yaml", "cluster/minecraft/persistentvolumeclaims.yaml", "cluster/minecraft/pods.yaml",
		"cluster/minecraft/services.yaml", "cluster/minecraft/statefulsets.yaml",
		"cluster/minecraftservers.yaml", "cluster/nodes.yaml", "cluster/persistentvolumes.yaml", "cluster/pods-all-namespaces.txt",
		"config.txt", "doctor.txt",
		"host/addresses.txt", "host/df.txt", "host/etc-felis.txt", "host/meminfo.txt", "host/systemd-timers.txt", "host/systemd-units.txt",
		"journal/felis-offsite.log", "journal/felis-watchdog.log", "journal/k3s.log",
		"logs/felis/felis-api-7d9/api.log", "logs/felis/felis-api-7d9/api.previous.log",
		"logs/minecraft/survival-0/egress-gate.log", "logs/minecraft/survival-0/prepare-data.log",
		"status.txt", "version.txt",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Errorf("bundle holds\n  %s\nwant\n  %s", strings.Join(names, "\n  "), strings.Join(want, "\n  "))
	}

	for what, secret := range plantedSecrets {
		for n, body := range files {
			if strings.Contains(body, secret) {
				t.Errorf("%s (%s) is in %s:\n%s", what, secret, n, body)
			}
		}
	}

	// What was taken out leaves what a reader needs around it.
	for name, wants := range map[string][]string{
		"logs/felis/felis-api-7d9/api.previous.log": {"log of felis/felis-api-7d9/api previous=true\n", "SERVICE_TOKEN=<redacted>\n", "login password=<redacted> ok\n", "Authorization: Bearer <redacted>\n", "postgres://felis:<redacted>@db:5432/felis\n"},
		"journal/k3s.log": {"journal of k3s.service\n", "FELIS_OFFSITE_KEY=<redacted>\n", "relay <redacted> refused\n",
			"relay login with the cached <redacted>\n", "database auth failed for <redacted>\n", "service token <redacted> rejected\n", "node joined with <redacted>\n"},
		"cluster/felis/statefulsets.yaml": {"name: FELIS_STS_SETTING\n"},
		"cluster/felis/cronjobs.yaml":     {"name: FELIS_CRON_SETTING\n"},
		"cluster/felis-build/jobs.yaml":   {"name: FELIS_JOB_SETTING\n"},
		"cluster/felis/pods.yaml":         {"name: FELIS_PLAIN_SETTING\n        value: <redacted>\n", "name: FELIS_SMTP_PASSWORD\n        valueFrom:\n          secretKeyRef:\n            key: password\n            name: felis-smtp\n", "felis.lolicon.best/kept: \"yes\"", "name: FELIS_INIT_SETTING\n", "name: FELIS_DEBUG_SETTING\n"},
		"cluster/felis/deployments.yaml":  {"name: FELIS_DEPLOY_SETTING\n            value: <redacted>\n"},
		"cluster/minecraftservers.yaml":   {"name: DISCORD_WEBHOOK\n      value: <redacted>\n"},
		"config.txt":                      {fmt.Sprintf("%-34s %s\n", "server.root_domain", "games.example.org"), fmt.Sprintf("%-34s %s\n", "database.url (no password)", "postgres://felis@127.0.0.1:1/felis")},
		"doctor.txt":                      {"the k3s token <redacted> leaked here\nprobe said password=<redacted>\n"},
		"status.txt":                      {"  world backups unknown: connect: password=<redacted>\n"},
		"host/etc-felis.txt":              {"secrets.env\n", "smtp-password\n", "felis-link.properties\n"},
		"MANIFEST.txt": {
			"The game servers' own logs are left out",
			"  journal/                 up to 500 lines per Felis unit and k3s, from the last 48h\n",
			"  - 11 secret values, wherever they appear, read from:\n",
			"secrets.env\n", "offsite.env\n", "smtp-password\n", "felis-link.properties\n", "token\n",
			"logs/felis/felis-api-7d9/wait-db.log: container \"wait-db\" is waiting to start; token=<redacted>\n",
			// Its own words about what is redacted come through whole.
			"  - passwords in URLs, private keys, Bearer and Basic credentials\n",
			"access_key=, private_key= or credentials= (and the same with a colon)\n",
			"Read it through before you send it anywhere",
		},
	} {
		for _, w := range wants {
			if !strings.Contains(files[name], w) {
				t.Errorf("%s lacks %q:\n%s", name, w, files[name])
			}
		}
	}
	for _, gone := range []string{"managedFields", "last-applied-configuration"} {
		if strings.Contains(files["cluster/felis/pods.yaml"], gone) {
			t.Errorf("pods.yaml keeps %s:\n%s", gone, files["cluster/felis/pods.yaml"])
		}
	}
	if strings.Contains(files["MANIFEST.txt"], "absent.env") || strings.Contains(files["MANIFEST.txt"], "uploads-s3-secret-key") {
		t.Errorf("MANIFEST names files this host does not have:\n%s", files["MANIFEST.txt"])
	}
}

// -server-logs adds the game servers' own logs, and says so.
func TestSupportBundleServerLogs(t *testing.T) {
	b := bundleHost(t)
	b.serverLogs = true
	path, err := b.write(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files, _ := readBundle(t, path)
	if _, ok := files["logs/minecraft/survival-0/server.log"]; !ok {
		t.Error("no server.log with -server-logs")
	}
	if !strings.Contains(files["MANIFEST.txt"], "The game servers' own logs are in logs/ (-server-logs)") {
		t.Errorf("MANIFEST does not say server logs are in:\n%s", files["MANIFEST.txt"])
	}
}

// With the cluster and the configuration gone the bundle still holds what the
// host shows, and MANIFEST says what it could not collect.
func TestSupportBundleWithTheClusterDown(t *testing.T) {
	b := bundleHost(t)
	b.cl, b.clErr = nil, errors.New("connection refused")
	b.cfg, b.cfgErr = nil, errors.New("felis.host.toml: no such file")
	b.wErr = errors.New("felis-watchdog.service is not installed; read the watchdog's defaults")
	path, err := b.write(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files, _ := readBundle(t, path)
	for _, n := range []string{"status.txt", "doctor.txt", "host/etc-felis.txt", "journal/k3s.log"} {
		if _, ok := files[n]; !ok {
			t.Errorf("no %s", n)
		}
	}
	for n := range files {
		if strings.HasPrefix(n, "cluster/") || strings.HasPrefix(n, "logs/") || n == "config.txt" {
			t.Errorf("%s with no cluster and no configuration", n)
		}
	}
	if !strings.Contains(files["status.txt"], "felis status: the configuration did not load: felis.host.toml: no such file") {
		t.Errorf("status.txt:\n%s", files["status.txt"])
	}
	if !strings.Contains(files["MANIFEST.txt"], "\nThe watchdog's settings: felis-watchdog.service is not installed; read the watchdog's defaults\n") ||
		!strings.Contains(files["MANIFEST.txt"], "  - cluster: connection refused\n") {
		t.Errorf("MANIFEST.txt:\n%s", files["MANIFEST.txt"])
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		48 * time.Hour:             "48h",
		90 * time.Minute:           "1h30m",
		45 * time.Minute:           "45m",
		30 * time.Second:           "30s",
		time.Hour + 30*time.Second: "1h0m30s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestScrubber(t *testing.T) {
	s := &scrubber{}
	for _, v := range []string{"known-secret-value", "known-secret-value-longer", "short", "known-secret-value"} {
		s.add(v)
	}
	if got := strings.Join(s.vals, ","); got != "known-secret-value-longer,known-secret-value" {
		t.Errorf("kept %q; want each value once, longest first, none under %d characters", s.vals, minScrubLen)
	}
	for in, want := range map[string]string{
		"a known-secret-value-longer b":                                     "a <redacted> b",
		"a known-secret-value b":                                            "a <redacted> b",
		"password=abcd1234 next":                                            "password=<redacted> next",
		`{"token": "abcd1234"}`:                                             `{"token": "<redacted>"}`,
		"SMTP_PASSWORD: s3cr3t!x":                                           "SMTP_PASSWORD: <redacted>",
		"x-api-key=zzzz9999&q=1":                                            "x-api-key=<redacted>&q=1",
		"Authorization: Basic dXNlcjpwYXNzd29yZA==":                         "Authorization: Basic <redacted>",
		"s3://AKIA:secretpart123@bucket/key":                                "s3://AKIA:<redacted>@bucket/key",
		"https://user@host/path":                                            "https://user@host/path",
		"-----BEGIN EC PRIVATE KEY-----\nMHc\n-----END EC PRIVATE KEY-----": "<redacted private key>",
		"tokens: 3": "tokens: 3",
		"read the relay password (keeping the cached one)": "read the relay password (keeping the cached one)",
		`secrets "felis-smtp" not found`:                   `secrets "felis-smtp" not found`,
	} {
		if got := string(s.text([]byte(in))); got != want {
			t.Errorf("text(%q) = %q, want %q", in, got, want)
		}
	}
	// Structured files keep what only looks like a secret.
	if got := string(s.values([]byte("secretName: felis-forwarding-secret and known-secret-value"))); got != "secretName: felis-forwarding-secret and <redacted>" {
		t.Errorf("values() = %q", got)
	}
}
