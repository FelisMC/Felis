package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type rotationRig struct {
	r      tokenRotator
	cl     client.Client
	out    *bytes.Buffer
	events []string
	dir    string
	clock  time.Time
}

func tokenSecret(ns, name, val string) *corev1.Secret {
	return keySecret(ns, name, "token", val)
}

func keySecret(ns, name, key, val string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{key: []byte(val)},
	}
}

// serverPod is a game server's pod as the operator labels it.
func serverPod(ns, name, server string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
		Labels: map[string]string{
			"felis.lolicon.best/server":     server,
			"felis.lolicon.best/managed-by": "felis-operator",
			"felis.lolicon.best/component":  "server",
		}}}
}

// backupPod is a backup Job's pod as internal/backupjob labels it: it carries
// the server's label too, and no rotation may take it for the server's own.
func backupPod(ns, name, server string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
		Labels: map[string]string{
			"felis.lolicon.best/server":    server,
			"app.kubernetes.io/managed-by": "felis-backup",
			"app.kubernetes.io/component":  "world-backup",
		}}}
}

func newRotationRig(t *testing.T, objs ...client.Object) *rotationRig {
	t.Helper()
	rig := &rotationRig{out: &bytes.Buffer{}, dir: t.TempDir(), clock: time.Unix(1_800_000_000, 0)}
	rig.cl = fake.NewClientBuilder().WithScheme(haltScheme(t)).WithObjects(objs...).Build()
	rig.r = tokenRotator{
		cl:             rig.cl,
		controlNS:      "felis",
		minecraftNS:    "minecraft",
		buildNS:        "felis-build",
		secretsEnv:     filepath.Join(rig.dir, "secrets.env"),
		linkProps:      filepath.Join(rig.dir, "felis-link.properties"),
		forwardingFile: filepath.Join(rig.dir, "forwarding.secret"),
		hostTOML:       filepath.Join(rig.dir, "felis.host.toml"),
		podTOML:        filepath.Join(rig.dir, "felis.pod.toml"),
		defaultTOML:    filepath.Join(rig.dir, "felis.toml"),
		newToken:       func() (string, error) { return "NEWTOKEN", nil },
		rollout: func(_ context.Context, deployment string) error {
			rig.events = append(rig.events, "roll "+deployment)
			return nil
		},
		restartUnit: func(_ context.Context, unit string) error {
			rig.events = append(rig.events, "restart "+unit)
			return nil
		},
		proxyLog: func(context.Context, time.Time) string { return "" },
		alterRole: func(context.Context, string, string, string) error {
			rig.events = append(rig.events, "alter-role")
			return nil
		},
		verifyDB: func(context.Context, string) error {
			rig.events = append(rig.events, "verify-db")
			return nil
		},
		// Every look at the clock moves it a second on, so a wait measured with it
		// ends after a known number of looks.
		now: func() time.Time {
			rig.clock = rig.clock.Add(time.Second)
			return rig.clock
		},
		reloadWait: 5 * time.Second,
		out:        rig.out,
	}
	return rig
}

func (rig *rotationRig) secretKey(t *testing.T, ns, name, key string) string {
	t.Helper()
	var s corev1.Secret
	if err := rig.cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil {
		return "<missing>"
	}
	return string(s.Data[key])
}

func (rig *rotationRig) secret(t *testing.T, ns, name string) string {
	t.Helper()
	return rig.secretKey(t, ns, name, "token")
}

func (rig *rotationRig) pods(t *testing.T) string {
	t.Helper()
	var pods corev1.PodList
	if err := rig.cl.List(context.Background(), &pods, client.InNamespace("minecraft")); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(pods.Items))
	for i, p := range pods.Items {
		names[i] = p.Name
	}
	return strings.Join(names, ",")
}

func writeTestFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRotateLimboToken(t *testing.T) {
	rig := newRotationRig(t,
		tokenSecret("felis", "felis-limbo-token", "old"),
		tokenSecret("minecraft", "felis-limbo-token", "old"),
		tokenSecret("felis", "felis-service-token", "proxy"),
		serverPod("minecraft", "login-0", "login"),
		serverPod("minecraft", "survival-0", "survival"),
		backupPod("minecraft", "login-backup-x", "login"),
	)
	writeTestFile(t, rig.r.secretsEnv, "DB_PASSWORD=db\nSERVICE_TOKEN=proxy\nLIMBO_TOKEN=old\nOPS_TOKEN=ops\n", 0o600)

	// felis-api must roll only after both copies hold the new value, and the login
	// pod must still be there then: restarting it earlier would have it present
	// the new token to an api that does not know it yet.
	rig.r.rollout = func(_ context.Context, deployment string) error {
		rig.events = append(rig.events, "roll "+deployment)
		if got := rig.secret(t, "felis", "felis-limbo-token"); got != "NEWTOKEN" {
			t.Errorf("api rolled while the control Secret held %q", got)
		}
		if got := rig.secret(t, "minecraft", "felis-limbo-token"); got != "NEWTOKEN" {
			t.Errorf("api rolled while the minecraft replica held %q", got)
		}
		var pod corev1.Pod
		if err := rig.cl.Get(context.Background(), client.ObjectKey{Namespace: "minecraft", Name: "login-0"}, &pod); err != nil {
			t.Errorf("the login pod was restarted before the api rolled")
		}
		return nil
	}
	if err := rig.r.rotate(context.Background(), "limbo", true); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, rig.r.secretsEnv); got != "DB_PASSWORD=db\nSERVICE_TOKEN=proxy\nLIMBO_TOKEN=NEWTOKEN\nOPS_TOKEN=ops\n" {
		t.Errorf("secrets.env = %q", got)
	}
	if info, _ := os.Stat(rig.r.secretsEnv); info.Mode().Perm() != 0o600 {
		t.Errorf("secrets.env mode = %v, want 0600", info.Mode().Perm())
	}
	if got := rig.secret(t, "felis", "felis-service-token"); got != "proxy" {
		t.Errorf("the proxy's token changed to %q", got)
	}
	// The login pod restarted; a user server and the backup Job's pod, which
	// carries the login server's label too, are untouched.
	if got := rig.pods(t); got != "login-backup-x,survival-0" {
		t.Errorf("pods left = %s, want login-backup-x,survival-0", got)
	}
	if strings.Join(rig.events, ",") != "roll felis-api" {
		t.Errorf("events = %v, want only the api roll (no unit restart for limbo)", rig.events)
	}
	if strings.Contains(rig.out.String(), "NEWTOKEN") {
		t.Errorf("the new token was printed: %s", rig.out.String())
	}
}

const testLinkProps = "# Generated\napi-base-url=http://10.0.0.1:8081\nservice-token=old\nroot-domain=example.com\n"

func reloadLine(token string) string {
	return "[12:00:00 INFO] [felis-link]: Felis: service-token reloaded from /x/felis-link.properties (fingerprint " + tokenFingerprint(token) + ")\n"
}

// The host proxy re-reads its token: the rotation waits for it to say so and
// leaves it running.
func TestRotateVelocityTokenReloadsTheHostProxy(t *testing.T) {
	rig := newRotationRig(t, tokenSecret("felis", "felis-service-token", "old"))
	writeTestFile(t, rig.r.secretsEnv, "SERVICE_TOKEN=old\n", 0o600)
	writeTestFile(t, rig.r.linkProps, testLinkProps, 0o640)
	written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(rig.r.linkProps, written, written); err != nil {
		t.Fatal(err)
	}

	// The proxy logs its reload on its next call to felis-api, which may be
	// while the api rolls: the log must be read from before that.
	var reloadedAt time.Time
	rig.r.rollout = func(_ context.Context, deployment string) error {
		rig.events = append(rig.events, "roll "+deployment)
		if got := readTestFile(t, rig.r.linkProps); !strings.Contains(got, "service-token=NEWTOKEN\n") {
			t.Errorf("api rolled before the proxy's file held the new token: %q", got)
		}
		reloadedAt = rig.clock
		return nil
	}
	looks := 0
	rig.r.proxyLog = func(_ context.Context, since time.Time) string {
		looks++
		log := reloadLine("old") // an earlier rotation's
		if looks >= 3 && !since.After(reloadedAt) {
			log += reloadLine("NEWTOKEN")
		}
		return log
	}
	if err := rig.r.rotate(context.Background(), "velocity", true); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, rig.r.linkProps); got != "# Generated\napi-base-url=http://10.0.0.1:8081\nservice-token=NEWTOKEN\nroot-domain=example.com\n" {
		t.Errorf("felis-link.properties = %q", got)
	}
	info, _ := os.Stat(rig.r.linkProps)
	if info.Mode().Perm() != 0o640 {
		t.Errorf("properties mode = %v, want 0640 (the proxy's group must still read it)", info.Mode().Perm())
	}
	if !info.ModTime().Equal(written) {
		t.Errorf("properties mtime = %v, want it kept at %v (felis domain check would read the proxy as stale)", info.ModTime(), written)
	}
	if got := rig.secret(t, "felis", "felis-service-token"); got != "NEWTOKEN" {
		t.Errorf("control Secret = %q, want NEWTOKEN", got)
	}
	// The proxy's token has no replica: it must not appear in a workload namespace.
	if got := rig.secret(t, "minecraft", "felis-service-token"); got != "<missing>" {
		t.Errorf("rotation copied the proxy token into minecraft (%q)", got)
	}
	if strings.Join(rig.events, ",") != "roll felis-api" {
		t.Errorf("events = %v, want the api roll and no proxy restart", rig.events)
	}
	if looks != 3 {
		t.Errorf("the log was read %d times, want 3 (until the line appeared)", looks)
	}
	if out := rig.out.String(); !strings.Contains(out, "felis-velocity: took the new token from its properties; players stayed connected") {
		t.Errorf("output does not say the players stayed: %s", out)
	}
}

// A proxy that never logs the new fingerprint (an older plugin, a stuck proxy)
// is restarted once the wait is over.
func TestRotateVelocityTokenRestartsAProxyThatDoesNotReload(t *testing.T) {
	rig := newRotationRig(t, tokenSecret("felis", "felis-service-token", "old"))
	writeTestFile(t, rig.r.secretsEnv, "SERVICE_TOKEN=old\n", 0o600)
	writeTestFile(t, rig.r.linkProps, testLinkProps, 0o640)
	looks := 0
	rig.r.proxyLog = func(context.Context, time.Time) string {
		looks++
		return reloadLine("old")
	}
	if err := rig.r.rotate(context.Background(), "velocity", true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rig.events, ",") != "roll felis-api,restart felis-velocity" {
		t.Errorf("events = %v, want the api roll then the proxy restart", rig.events)
	}
	// reloadWait is 5s and each look moves the clock a second.
	if looks != 5 {
		t.Errorf("the log was read %d times, want 5", looks)
	}
	if out := rig.out.String(); !strings.Contains(out, "felis-velocity: had not taken the new token within 5s, restarted") {
		t.Errorf("output does not explain the restart: %s", out)
	}
}

// The proxy and the Java plugin name a token by the same fingerprint
// (plugins/shared LinkConfigLoaderTest fileTokenFollowsTheFile).
func TestTokenFingerprintMatchesThePlugin(t *testing.T) {
	if got := tokenFingerprint("new-token"); got != "348e9df2a42b" {
		t.Errorf("tokenFingerprint(new-token) = %q, want 348e9df2a42b", got)
	}
}

// An external proxy has no felis-link.properties here: the Secret still rotates,
// nothing is restarted on this host, and the output says where the value is
// without printing it.
func TestRotateVelocityTokenForAnExternalProxy(t *testing.T) {
	rig := newRotationRig(t, tokenSecret("felis", "felis-service-token", "old"))
	if err := rig.r.rotate(context.Background(), "velocity", true); err != nil {
		t.Fatal(err)
	}
	if got := rig.secret(t, "felis", "felis-service-token"); got != "NEWTOKEN" {
		t.Errorf("control Secret = %q, want NEWTOKEN", got)
	}
	if strings.Join(rig.events, ",") != "roll felis-api" {
		t.Errorf("events = %v, want no proxy restart", rig.events)
	}
	out := rig.out.String()
	if !strings.Contains(out, "felis/felis-service-token") || strings.Contains(out, "NEWTOKEN") {
		t.Errorf("output should point at the Secret without the value: %s", out)
	}
}

func TestRotateBuildTokenReachesTheBuildNamespace(t *testing.T) {
	rig := newRotationRig(t, tokenSecret("felis", "felis-build-token", "old"))
	// An install from before per-caller tokens has no BUILD_TOKEN line yet.
	writeTestFile(t, rig.r.secretsEnv, "SERVICE_TOKEN=proxy", 0o600)
	if err := rig.r.rotate(context.Background(), "build", true); err != nil {
		t.Fatal(err)
	}
	if got := rig.secret(t, "felis-build", "felis-build-token"); got != "NEWTOKEN" {
		t.Errorf("build replica = %q, want NEWTOKEN (created when absent)", got)
	}
	if got := rig.secret(t, "felis", "felis-build-token"); got != "NEWTOKEN" {
		t.Errorf("control Secret = %q, want NEWTOKEN", got)
	}
	if got := readTestFile(t, rig.r.secretsEnv); got != "SERVICE_TOKEN=proxy\nBUILD_TOKEN=NEWTOKEN\n" {
		t.Errorf("secrets.env = %q", got)
	}
}

// A failed api rollout stops the rotation before the caller restarts: the login
// gate keeps running on the old value the old api pods still accept.
func TestRotateStopsWhenTheAPIDoesNotRoll(t *testing.T) {
	rig := newRotationRig(t,
		tokenSecret("felis", "felis-limbo-token", "old"),
		serverPod("minecraft", "login-0", "login"),
	)
	rig.r.rollout = func(context.Context, string) error { return errors.New("rollout timed out") }
	err := rig.r.rotate(context.Background(), "limbo", true)
	if err == nil || !strings.Contains(err.Error(), "rollout timed out") {
		t.Fatalf("err = %v, want the rollout failure", err)
	}
	if got := rig.pods(t); got != "login-0" {
		t.Errorf("pods left = %s: the login pod was restarted although the api never rolled", got)
	}
}

func TestRotateRefusesAnUnknownCredential(t *testing.T) {
	rig := newRotationRig(t)
	writeTestFile(t, rig.r.secretsEnv, "SERVICE_TOKEN=proxy\n", 0o600)
	if err := rig.r.rotate(context.Background(), "admin", true); err == nil {
		t.Fatal("rotated an unknown credential")
	}
	if got := readTestFile(t, rig.r.secretsEnv); got != "SERVICE_TOKEN=proxy\n" {
		t.Errorf("secrets.env = %q, want it untouched", got)
	}
	if len(rig.events) != 0 {
		t.Errorf("events = %v, want nothing touched", rig.events)
	}
}

const (
	testHostTOML = "# Written by the installer.\n[database]\nurl = \"postgres://felis:oldpw@127.0.0.1:30432/felis?sslmode=disable\"\ndeployment = \"felis/felis-postgres\"\n\n[server]\nroot_domain = \"example.com\"\n"
	testPodTOML  = "[database]\n# The Service address.\nurl = \"postgres://felis:oldpw@felis-postgres.felis.svc:5432/felis?sslmode=disable\"\n\n[server]\nroot_domain = \"example.com\"\n"
)

// installedRig is a host as the installer leaves it, with every credential in
// place, for the plan test.
func installedRig(t *testing.T) *rotationRig {
	t.Helper()
	rig := newRotationRig(t,
		tokenSecret("felis", "felis-service-token", "old"),
		tokenSecret("felis", "felis-limbo-token", "old"),
		tokenSecret("minecraft", "felis-limbo-token", "old"),
		tokenSecret("felis", "felis-build-token", "old"),
		tokenSecret("felis-build", "felis-build-token", "old"),
		tokenSecret("felis", "felis-ops-token", "old"),
		keySecret("felis", "felis-registry-auth", "platform", "old"),
		keySecret("felis-build", "felis-registry-push", "password", "old"),
		keySecret("felis", "felis-forwarding-secret", "secret", "old"),
		keySecret("minecraft", "felis-forwarding-secret", "secret", "old"),
		keySecret("felis", "felis-config", "felis.toml", testPodTOML),
		keySecret("minecraft", "felis-config", "felis.toml", testPodTOML),
		serverPod("minecraft", "login-0", "login"),
		serverPod("minecraft", "survival-0", "survival"),
	)
	writeTestFile(t, rig.r.secretsEnv, "DB_PASSWORD=oldpw\nSERVICE_TOKEN=old\n", 0o600)
	writeTestFile(t, rig.r.linkProps, testLinkProps, 0o640)
	writeTestFile(t, rig.r.forwardingFile, "old", 0o640)
	writeTestFile(t, rig.r.hostTOML, testHostTOML, 0o600)
	writeTestFile(t, rig.r.podTOML, testPodTOML, 0o600)
	return rig
}

// Without -yes every rotation prints its plan and changes nothing.
func TestRotatePlanChangesNothing(t *testing.T) {
	for _, kind := range rotationKinds() {
		t.Run(kind, func(t *testing.T) {
			rig := installedRig(t)
			files := map[string]string{}
			for _, p := range []string{rig.r.secretsEnv, rig.r.linkProps, rig.r.forwardingFile, rig.r.hostTOML, rig.r.podTOML} {
				files[p] = readTestFile(t, p)
			}
			rig.r.newToken = func() (string, error) {
				t.Error("a plan generated a token")
				return "NEWTOKEN", nil
			}
			if err := rig.r.rotate(context.Background(), kind, false); err != nil {
				t.Fatal(err)
			}
			for p, before := range files {
				if got := readTestFile(t, p); got != before {
					t.Errorf("%s changed to %q", filepath.Base(p), got)
				}
			}
			for _, s := range [][3]string{
				{"felis", "felis-service-token", "token"}, {"felis", "felis-limbo-token", "token"},
				{"felis-build", "felis-build-token", "token"}, {"felis", "felis-ops-token", "token"},
				{"felis", "felis-registry-auth", "platform"}, {"felis-build", "felis-registry-push", "password"},
				{"minecraft", "felis-forwarding-secret", "secret"},
			} {
				if got := rig.secretKey(t, s[0], s[1], s[2]); got != "old" {
					t.Errorf("Secret %s/%s changed to %q", s[0], s[1], got)
				}
			}
			if got := rig.secretKey(t, "minecraft", "felis-config", "felis.toml"); got != testPodTOML {
				t.Errorf("felis-config changed to %q", got)
			}
			if len(rig.events) != 0 {
				t.Errorf("events = %v, want none", rig.events)
			}
			if got := rig.pods(t); got != "login-0,survival-0" {
				t.Errorf("pods left = %s, want all", got)
			}
			out := rig.out.String()
			if !strings.HasSuffix(out, "\nNothing was changed. To rotate: sudo felis rotate-token -yes "+kind+"\n") {
				t.Errorf("the plan does not end with how to go ahead: %s", out)
			}
			// Each plan names the interruption it causes.
			want := apiRestartNote
			if kind == kindForwarding {
				want = "everyone online is disconnected"
			}
			if !strings.Contains(out, want) {
				t.Errorf("the plan does not say %q: %s", want, out)
			}
		})
	}
}

func TestRotateRegistryTokens(t *testing.T) {
	rig := newRotationRig(t,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "felis", Name: "felis-registry-auth"},
			Data: map[string][]byte{"platform": []byte("a"), "build": []byte("b"), "prune": []byte("c")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "felis-build", Name: "felis-registry-push"},
			Data: map[string][]byte{"username": []byte("build"), "password": []byte("b")}},
	)
	writeTestFile(t, rig.r.secretsEnv, "DB_PASSWORD=db\nREGISTRY_PLATFORM_TOKEN=a\nREGISTRY_BUILD_TOKEN=b\nREGISTRY_PRUNE_TOKEN=c\n", 0o600)
	n := 0
	rig.r.newToken = func() (string, error) {
		n++
		return fmt.Sprintf("NEWTOKEN%d", n), nil
	}
	// The registry reads its tokens as it starts: it restarts after the Secret
	// holds them, and felis-api (the prune token) after that.
	rig.r.rollout = func(_ context.Context, deployment string) error {
		rig.events = append(rig.events, "roll "+deployment)
		if got := rig.secretKey(t, "felis", "felis-registry-auth", "prune"); got != "NEWTOKEN3" {
			t.Errorf("%s rolled while the prune token was %q", deployment, got)
		}
		return nil
	}
	if err := rig.r.rotate(context.Background(), kindRegistry, true); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, rig.r.secretsEnv); got != "DB_PASSWORD=db\nREGISTRY_PLATFORM_TOKEN=NEWTOKEN1\nREGISTRY_BUILD_TOKEN=NEWTOKEN2\nREGISTRY_PRUNE_TOKEN=NEWTOKEN3\n" {
		t.Errorf("secrets.env = %q", got)
	}
	for key, want := range map[string]string{"platform": "NEWTOKEN1", "build": "NEWTOKEN2", "prune": "NEWTOKEN3"} {
		if got := rig.secretKey(t, "felis", "felis-registry-auth", key); got != want {
			t.Errorf("felis-registry-auth %s = %q, want %s", key, got, want)
		}
	}
	if got := rig.secretKey(t, "felis-build", "felis-registry-push", "password"); got != "NEWTOKEN2" {
		t.Errorf("the build Jobs' push password = %q, want the build token", got)
	}
	if got := rig.secretKey(t, "felis-build", "felis-registry-push", "username"); got != "build" {
		t.Errorf("the build Jobs' push username = %q, want build", got)
	}
	if strings.Join(rig.events, ",") != "roll registry,roll felis-api" {
		t.Errorf("events = %v, want the registry then felis-api", rig.events)
	}
	if strings.Contains(rig.out.String(), "NEWTOKEN") {
		t.Errorf("a new token was printed: %s", rig.out.String())
	}
}

func TestRotateForwardingSecret(t *testing.T) {
	lock := time.Unix(1_800_000_000, 0)
	rig := newRotationRig(t,
		keySecret("felis", "felis-forwarding-secret", "secret", "old"),
		keySecret("minecraft", "felis-forwarding-secret", "secret", "old"),
		serverPod("minecraft", "survival-0", "survival"),
		serverPod("minecraft", "lobby-0", "lobby"),
		// creative is being backed up: a running Job holds its world.
		serverPod("minecraft", "creative-0", "creative"),
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "minecraft", Name: "creative-backup",
			Labels: map[string]string{maintenance.LabelServer: "creative", maintenance.LabelManagedBy: "felis-backup"}}},
		// survival's last backup is over; its finished pod stays until the Job's TTL.
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "minecraft", Name: "survival-backup",
			Labels: map[string]string{maintenance.LabelServer: "survival", maintenance.LabelManagedBy: "felis-backup"}},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}},
		backupPod("minecraft", "survival-backup-x", "survival"),
		// A pod already on its way out is neither counted nor deleted again.
		func() *corev1.Pod {
			p := serverPod("minecraft", "lobby-old", "lobby")
			p.DeletionTimestamp = &metav1.Time{Time: lock}
			p.Finalizers = []string{"felis.lolicon.best/test"}
			return p
		}(),
		// skyblock's restore has just been admitted, its Job not created yet.
		serverPod("minecraft", "skyblock-0", "skyblock"),
		&v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Namespace: "minecraft", Name: "skyblock",
			Annotations: map[string]string{maintenance.Annotation: maintenance.LockValue(maintenance.KindRestore, lock)}}},
		&v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Namespace: "minecraft", Name: "survival"}},
	)
	writeTestFile(t, rig.r.secretsEnv, "DB_PASSWORD=db\nFORWARDING_SECRET=old\n", 0o600)
	writeTestFile(t, rig.r.forwardingFile, "old", 0o640)
	// The proxy restarts after the servers were told to: a player who rejoins
	// must not meet a server still on the old secret.
	rig.r.restartUnit = func(_ context.Context, unit string) error {
		rig.events = append(rig.events, "restart "+unit)
		if got := rig.pods(t); strings.Contains(got, "survival-0") {
			t.Errorf("the proxy restarted before the servers (pods %s)", got)
		}
		if got := readTestFile(t, rig.r.forwardingFile); got != "NEWTOKEN" {
			t.Errorf("the proxy restarted on forwarding.secret %q", got)
		}
		return nil
	}
	if err := rig.r.rotate(context.Background(), kindForwarding, true); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, rig.r.secretsEnv); got != "DB_PASSWORD=db\nFORWARDING_SECRET=NEWTOKEN\n" {
		t.Errorf("secrets.env = %q", got)
	}
	if info, _ := os.Stat(rig.r.forwardingFile); info.Mode().Perm() != 0o640 {
		t.Errorf("forwarding.secret mode = %v, want 0640", info.Mode().Perm())
	}
	for _, ns := range []string{"felis", "minecraft"} {
		if got := rig.secretKey(t, ns, "felis-forwarding-secret", "secret"); got != "NEWTOKEN" {
			t.Errorf("Secret %s/felis-forwarding-secret = %q, want NEWTOKEN", ns, got)
		}
	}
	if got := rig.pods(t); got != "creative-0,lobby-old,skyblock-0,survival-backup-x" {
		t.Errorf("pods left = %s, want the held servers, the terminating pod and the backup's pod", got)
	}
	if strings.Join(rig.events, ",") != "restart felis-velocity" {
		t.Errorf("events = %v, want only the proxy restart (felis-api does not hold this secret)", rig.events)
	}
	out := rig.out.String()
	for _, want := range []string{
		"every running server restarts to read it (2 now)",
		"left running, because a backup, restore or file write holds its world: creative (backup), skyblock (restore).",
		"  - servers: 2 restarting on the new secret\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %s", want, out)
		}
	}
	if strings.Contains(out, "NEWTOKEN") {
		t.Errorf("the new secret was printed: %s", out)
	}
}

func TestRotateForwardingSecretForAnExternalProxy(t *testing.T) {
	rig := newRotationRig(t, keySecret("felis", "felis-forwarding-secret", "secret", "old"))
	if err := rig.r.rotate(context.Background(), kindForwarding, true); err != nil {
		t.Fatal(err)
	}
	if got := rig.secretKey(t, "felis", "felis-forwarding-secret", "secret"); got != "NEWTOKEN" {
		t.Errorf("control Secret = %q, want NEWTOKEN", got)
	}
	if len(rig.events) != 0 {
		t.Errorf("events = %v, want no proxy restart on this host", rig.events)
	}
	if out := rig.out.String(); !strings.Contains(out, "put the value in Secret felis/felis-forwarding-secret into your proxy's forwarding secret file") {
		t.Errorf("output does not say where the value is: %s", out)
	}
}

// dbRig is an installed host for the database rotation: felis.toml links to the
// host copy, as the installer makes it.
func dbRig(t *testing.T) *rotationRig {
	t.Helper()
	rig := newRotationRig(t,
		keySecret("felis", "felis-config", "felis.toml", testPodTOML),
		keySecret("minecraft", "felis-config", "felis.toml", testPodTOML),
	)
	writeTestFile(t, rig.r.secretsEnv, "DB_PASSWORD=oldpw\nSERVICE_TOKEN=s\n", 0o600)
	writeTestFile(t, rig.r.hostTOML, testHostTOML, 0o600)
	writeTestFile(t, rig.r.podTOML, testPodTOML, 0o600)
	if err := os.Symlink("felis.host.toml", rig.r.defaultTOML); err != nil {
		t.Fatal(err)
	}
	return rig
}

const (
	wantHostTOML = "# Written by the installer.\n[database]\nurl = \"postgres://felis:NEWTOKEN@127.0.0.1:30432/felis?sslmode=disable\"\ndeployment = \"felis/felis-postgres\"\n\n[server]\nroot_domain = \"example.com\"\n"
	wantPodTOML  = "[database]\n# The Service address.\nurl = \"postgres://felis:NEWTOKEN@felis-postgres.felis.svc:5432/felis?sslmode=disable\"\n\n[server]\nroot_domain = \"example.com\"\n"
)

func TestRotateDatabasePassword(t *testing.T) {
	rig := dbRig(t)
	rig.r.alterRole = func(_ context.Context, deployment, role, verifier string) error {
		rig.events = append(rig.events, "alter-role")
		if deployment != "felis/felis-postgres" || role != "felis" {
			t.Errorf("alterRole(%q, %q), want felis/felis-postgres and felis", deployment, role)
		}
		if !strings.Contains(readTestFile(t, rig.r.secretsEnv), "DB_PASSWORD=NEWTOKEN\n") {
			t.Error("the role changed before secrets.env recorded the new password")
		}
		// The verifier is the new password's, under the salt it carries.
		m := regexp.MustCompile(`^SCRAM-SHA-256\$4096:([^$]+)\$`).FindStringSubmatch(verifier)
		if m == nil {
			t.Fatalf("verifier %q is not a SCRAM-SHA-256 verifier", verifier)
		}
		salt, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil || len(salt) != 16 {
			t.Fatalf("verifier salt %q: %v", m[1], err)
		}
		if want, _ := scramVerifier("NEWTOKEN", salt, 4096); verifier != want {
			t.Errorf("verifier = %q, want %q", verifier, want)
		}
		return nil
	}
	// The configs change only once the database accepts the new password.
	rig.r.verifyDB = func(_ context.Context, url string) error {
		rig.events = append(rig.events, "verify-db")
		if url != "postgres://felis:NEWTOKEN@127.0.0.1:30432/felis?sslmode=disable" {
			t.Errorf("verified with %q, want the host URL with the new password", url)
		}
		if got := readTestFile(t, rig.r.hostTOML); got != testHostTOML {
			t.Errorf("the host config changed before the database accepted the password: %q", got)
		}
		return nil
	}
	rig.r.rollout = func(_ context.Context, deployment string) error {
		rig.events = append(rig.events, "roll "+deployment)
		if got := rig.secretKey(t, "felis", "felis-config", "felis.toml"); got != wantPodTOML {
			t.Errorf("felis-api rolled on felis-config %q", got)
		}
		return nil
	}
	if err := rig.r.rotate(context.Background(), kindDB, true); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, rig.r.secretsEnv); got != "DB_PASSWORD=NEWTOKEN\nSERVICE_TOKEN=s\n" {
		t.Errorf("secrets.env = %q", got)
	}
	if got := readTestFile(t, rig.r.hostTOML); got != wantHostTOML {
		t.Errorf("host config = %q", got)
	}
	if got := readTestFile(t, rig.r.podTOML); got != wantPodTOML {
		t.Errorf("pod config = %q", got)
	}
	if info, err := os.Lstat(rig.r.defaultTOML); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("felis.toml is no longer the link to the host copy (%v)", err)
	}
	for _, ns := range []string{"felis", "minecraft"} {
		if got := rig.secretKey(t, ns, "felis-config", "felis.toml"); got != wantPodTOML {
			t.Errorf("Secret %s/felis-config = %q", ns, got)
		}
	}
	if strings.Join(rig.events, ",") != "alter-role,verify-db,roll felis-api" {
		t.Errorf("events = %v", rig.events)
	}
	if strings.Contains(rig.out.String(), "NEWTOKEN") {
		t.Errorf("the new password was printed: %s", rig.out.String())
	}
}

// A password the database does not accept leaves the configs on the old one
// and felis-api running: the installer's record, already new, is the way back.
func TestRotateDatabasePasswordStopsWhenTheDatabaseRefuses(t *testing.T) {
	rig := dbRig(t)
	rig.r.verifyDB = func(context.Context, string) error {
		rig.events = append(rig.events, "verify-db")
		return errors.New("password authentication failed")
	}
	err := rig.r.rotate(context.Background(), kindDB, true)
	if err == nil || !strings.Contains(err.Error(), "password authentication failed") || !strings.Contains(err.Error(), "run the installer again") {
		t.Fatalf("err = %v, want the refusal and the way back", err)
	}
	if got := readTestFile(t, rig.r.hostTOML); got != testHostTOML {
		t.Errorf("host config = %q, want it unchanged", got)
	}
	if got := readTestFile(t, rig.r.podTOML); got != testPodTOML {
		t.Errorf("pod config = %q, want it unchanged", got)
	}
	if got := rig.secretKey(t, "felis", "felis-config", "felis.toml"); got != testPodTOML {
		t.Errorf("felis-config = %q, want it unchanged", got)
	}
	if strings.Join(rig.events, ",") != "alter-role,verify-db" {
		t.Errorf("events = %v, want no api roll", rig.events)
	}
}

// Everything that can refuse does so before the installer's record or the
// role change; what the host config alone shows is refused by the plan too.
func TestRotateDatabasePasswordRefusesEarly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply bool
		setup func(t *testing.T, rig *rotationRig)
		want  string
	}{
		{"a database the installer does not run", false, func(t *testing.T, rig *rotationRig) {
			writeTestFile(t, rig.r.hostTOML, strings.Replace(testHostTOML, "deployment = \"felis/felis-postgres\"\n", "", 1), 0o600)
		}, "[database] deployment is unset"},
		{"a database url without a role", false, func(t *testing.T, rig *rotationRig) {
			writeTestFile(t, rig.r.hostTOML, strings.Replace(testHostTOML, "felis:oldpw@", "", 1), 0o600)
		}, "names no role"},
		{"a config copy the line editor cannot edit", true, func(t *testing.T, rig *rotationRig) {
			writeTestFile(t, rig.r.podTOML, "[database]\nurl = \"\"\"\npostgres://felis:oldpw@felis-postgres.felis.svc:5432/felis\"\"\"\n", 0o600)
		}, "set the password in its [database] url by hand"},
		{"a pod copy that is the host copy", true, func(t *testing.T, rig *rotationRig) {
			if err := os.Remove(rig.r.podTOML); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("felis.host.toml", rig.r.podTOML); err != nil {
				t.Fatal(err)
			}
		}, "resolves to the same file as"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := dbRig(t)
			tc.setup(t, rig)
			err := rig.r.rotate(context.Background(), kindDB, tc.apply)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if got := readTestFile(t, rig.r.secretsEnv); got != "DB_PASSWORD=oldpw\nSERVICE_TOKEN=s\n" {
				t.Errorf("secrets.env = %q, want it untouched", got)
			}
			if len(rig.events) != 0 {
				t.Errorf("events = %v, want nothing done", rig.events)
			}
		})
	}
}

// The RFC 7677 example (user "user", password "pencil"), with the stored and
// server keys computed by openssl's PBKDF2 and HMAC rather than this code.
func TestScramVerifier(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	got, err := scramVerifier("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="; got != want {
		t.Errorf("scramVerifier = %q\nwant            %q", got, want)
	}
}

func TestAlterRoleSQL(t *testing.T) {
	if got := alterRoleSQL(`fe"lis`, "SCRAM-SHA-256$4096:c2FsdA==$a:b"); got != "ALTER ROLE \"fe\"\"lis\" WITH PASSWORD 'SCRAM-SHA-256$4096:c2FsdA==$a:b';\n" {
		t.Errorf("alterRoleSQL = %q", got)
	}
}

// installerSecretKeys is every secrets.env key a rotation writes.
func installerSecretKeys() map[string]bool {
	keys := map[string]bool{installerForwardingKey: true, installerDBKey: true}
	for _, k := range installerTokenKeys {
		keys[k] = true
	}
	for _, k := range installerRegistryKeys {
		keys[k] = true
	}
	return keys
}

// The installer and rotate-token must agree on where each caller's token lives:
// rotate-token writes installerTokenKeys into secrets.env, and the installer
// applies those same keys to the Secrets on its next run. A key the installer
// does not read would be silently reverted by the next upgrade.
func TestInstallerProvisionsEveryCallerToken(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	for caller, key := range installerTokenKeys {
		ct, ok := callerToken(caller)
		if !ok {
			t.Fatalf("installerTokenKeys names unknown caller %q", caller)
		}
		apply := regexp.MustCompile(`apply_literal_secret "\$CONTROL_NS" ` + regexp.QuoteMeta(ct.Secret) + ` token "\$` + key + `"`)
		if !apply.MatchString(script) {
			t.Errorf("bootstrap.sh does not apply %s from %s in the control namespace", ct.Secret, key)
		}
	}
	// The replicas the installer applies straight into the workload namespaces, so an
	// upgrade has them before the new operator and build Jobs reference them.
	for _, line := range []string{
		`apply_literal_secret "$MINECRAFT_NS" felis-limbo-token token "$LIMBO_TOKEN"`,
		`apply_literal_secret "$BUILD_NS" felis-build-token token "$BUILD_TOKEN"`,
		`kube -n "$MINECRAFT_NS" delete secret felis-service-token --ignore-not-found`,
		`kube -n "$BUILD_NS" delete secret felis-service-token --ignore-not-found`,
	} {
		if !strings.Contains(script, line) {
			t.Errorf("bootstrap.sh lacks %s", line)
		}
	}
	for _, ns := range []string{"MINECRAFT_NS", "BUILD_NS"} {
		if strings.Contains(script, `apply_literal_secret "$`+ns+`" felis-service-token`) {
			t.Errorf("bootstrap.sh still copies the proxy's token into %s", ns)
		}
	}
	if len(installerTokenKeys) != len(callerNames()) {
		t.Errorf("installerTokenKeys covers %d callers, naming.CallerTokens lists %d", len(installerTokenKeys), len(callerNames()))
	}
}

// Every value the installer keeps in secrets.env has a rotation that rewrites
// that very key, and every key a rotation writes is one the installer
// generates, keeps and so re-applies on its next run.
func TestInstallerSecretsAreAllRotatable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	m := regexp.MustCompile(`(?s)write_file_atomic "\$SECRETS_ENV" 0600 <<EOF\n(.*?)\nEOF\n`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal("bootstrap.sh: no secrets.env heredoc found")
	}
	persisted := map[string]bool{}
	for _, line := range strings.Split(m[1], "\n") {
		key, value, _ := strings.Cut(line, "=")
		if value != "${"+key+"}" {
			t.Errorf("secrets.env line %q is not KEY=${KEY}", line)
		}
		persisted[key] = true
	}
	rotated := installerSecretKeys()
	for key := range persisted {
		if !rotated[key] {
			t.Errorf("secrets.env keeps %s, which no rotation replaces", key)
		}
	}
	for key := range rotated {
		if !persisted[key] {
			t.Errorf("a rotation writes %s, which the installer does not keep in secrets.env", key)
		}
		if !regexp.MustCompile(`\n  ` + key + `="\$\{` + key + `:-\$\(openssl rand -hex [0-9]+\)\}"\n`).MatchString(script) {
			t.Errorf("bootstrap.sh does not generate %s when secrets.env lacks it", key)
		}
	}
}
