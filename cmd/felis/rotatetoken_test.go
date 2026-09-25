package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
}

func tokenSecret(ns, name, val string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{"token": []byte(val)},
	}
}

func serverPod(ns, name, server string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
		Labels: map[string]string{"felis.lolicon.best/server": server}}}
}

func newRotationRig(t *testing.T, objs ...client.Object) *rotationRig {
	t.Helper()
	rig := &rotationRig{out: &bytes.Buffer{}, dir: t.TempDir()}
	rig.cl = fake.NewClientBuilder().WithScheme(haltScheme(t)).WithObjects(objs...).Build()
	rig.r = tokenRotator{
		cl:          rig.cl,
		controlNS:   "felis",
		minecraftNS: "minecraft",
		buildNS:     "felis-build",
		secretsEnv:  filepath.Join(rig.dir, "secrets.env"),
		linkProps:   filepath.Join(rig.dir, "felis-link.properties"),
		newToken:    func() (string, error) { return "NEWTOKEN", nil },
		rollAPI: func(context.Context) error {
			rig.events = append(rig.events, "roll-api")
			return nil
		},
		restartUnit: func(_ context.Context, unit string) error {
			rig.events = append(rig.events, "restart "+unit)
			return nil
		},
		out: rig.out,
	}
	return rig
}

func (rig *rotationRig) secret(t *testing.T, ns, name string) string {
	t.Helper()
	var s corev1.Secret
	if err := rig.cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil {
		return "<missing>"
	}
	return string(s.Data["token"])
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

func TestRotateLimboToken(t *testing.T) {
	rig := newRotationRig(t,
		tokenSecret("felis", "felis-limbo-token", "old"),
		tokenSecret("minecraft", "felis-limbo-token", "old"),
		tokenSecret("felis", "felis-service-token", "proxy"),
		serverPod("minecraft", "login-0", "login"),
		serverPod("minecraft", "survival-0", "survival"),
	)
	writeTestFile(t, rig.r.secretsEnv, "DB_PASSWORD=db\nSERVICE_TOKEN=proxy\nLIMBO_TOKEN=old\nOPS_TOKEN=ops\n", 0o600)

	// felis-api must roll only after both copies hold the new value, and the login
	// pod must still be there then: restarting it earlier would have it present
	// the new token to an api that does not know it yet.
	rig.r.rollAPI = func(context.Context) error {
		rig.events = append(rig.events, "roll-api")
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
	if err := rig.r.rotate(context.Background(), "limbo"); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(rig.r.secretsEnv)
	if string(raw) != "DB_PASSWORD=db\nSERVICE_TOKEN=proxy\nLIMBO_TOKEN=NEWTOKEN\nOPS_TOKEN=ops\n" {
		t.Errorf("secrets.env = %q", raw)
	}
	if info, _ := os.Stat(rig.r.secretsEnv); info.Mode().Perm() != 0o600 {
		t.Errorf("secrets.env mode = %v, want 0600", info.Mode().Perm())
	}
	if got := rig.secret(t, "felis", "felis-service-token"); got != "proxy" {
		t.Errorf("the proxy's token changed to %q", got)
	}
	var pods corev1.PodList
	if err := rig.cl.List(context.Background(), &pods, client.InNamespace("minecraft")); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Name != "survival-0" {
		t.Errorf("pods left = %v, want only survival-0 (the login pod restarted, user servers untouched)", pods.Items)
	}
	if strings.Join(rig.events, ",") != "roll-api" {
		t.Errorf("events = %v, want only the api roll (no unit restart for limbo)", rig.events)
	}
	if strings.Contains(rig.out.String(), "NEWTOKEN") {
		t.Errorf("the new token was printed: %s", rig.out.String())
	}
}

func TestRotateVelocityTokenOnTheHostProxy(t *testing.T) {
	rig := newRotationRig(t, tokenSecret("felis", "felis-service-token", "old"))
	writeTestFile(t, rig.r.secretsEnv, "SERVICE_TOKEN=old\n", 0o600)
	writeTestFile(t, rig.r.linkProps, "# Generated\napi-base-url=http://10.0.0.1:8081\nservice-token=old\nroot-domain=example.com\n", 0o640)

	if err := rig.r.rotate(context.Background(), "velocity"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(rig.r.linkProps)
	if string(raw) != "# Generated\napi-base-url=http://10.0.0.1:8081\nservice-token=NEWTOKEN\nroot-domain=example.com\n" {
		t.Errorf("felis-link.properties = %q", raw)
	}
	if info, _ := os.Stat(rig.r.linkProps); info.Mode().Perm() != 0o640 {
		t.Errorf("properties mode = %v, want 0640 (the proxy's group must still read it)", info.Mode().Perm())
	}
	if got := rig.secret(t, "felis", "felis-service-token"); got != "NEWTOKEN" {
		t.Errorf("control Secret = %q, want NEWTOKEN", got)
	}
	// The proxy's token has no replica: it must not appear in a workload namespace.
	if got := rig.secret(t, "minecraft", "felis-service-token"); got != "<missing>" {
		t.Errorf("rotation copied the proxy token into minecraft (%q)", got)
	}
	if strings.Join(rig.events, ",") != "roll-api,restart felis-velocity" {
		t.Errorf("events = %v, want the api roll then the proxy restart", rig.events)
	}
}

// An external proxy has no felis-link.properties here: the Secret still rotates,
// nothing is restarted on this host, and the output says where the value is
// without printing it.
func TestRotateVelocityTokenForAnExternalProxy(t *testing.T) {
	rig := newRotationRig(t, tokenSecret("felis", "felis-service-token", "old"))
	if err := rig.r.rotate(context.Background(), "velocity"); err != nil {
		t.Fatal(err)
	}
	if got := rig.secret(t, "felis", "felis-service-token"); got != "NEWTOKEN" {
		t.Errorf("control Secret = %q, want NEWTOKEN", got)
	}
	if strings.Join(rig.events, ",") != "roll-api" {
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
	if err := rig.r.rotate(context.Background(), "build"); err != nil {
		t.Fatal(err)
	}
	if got := rig.secret(t, "felis-build", "felis-build-token"); got != "NEWTOKEN" {
		t.Errorf("build replica = %q, want NEWTOKEN (created when absent)", got)
	}
	if got := rig.secret(t, "felis", "felis-build-token"); got != "NEWTOKEN" {
		t.Errorf("control Secret = %q, want NEWTOKEN", got)
	}
	raw, _ := os.ReadFile(rig.r.secretsEnv)
	if string(raw) != "SERVICE_TOKEN=proxy\nBUILD_TOKEN=NEWTOKEN\n" {
		t.Errorf("secrets.env = %q", raw)
	}
}

// A failed api rollout stops the rotation before the caller restarts: the login
// gate keeps running on the old value the old api pods still accept.
func TestRotateStopsWhenTheAPIDoesNotRoll(t *testing.T) {
	rig := newRotationRig(t,
		tokenSecret("felis", "felis-limbo-token", "old"),
		serverPod("minecraft", "login-0", "login"),
	)
	rig.r.rollAPI = func(context.Context) error { return errors.New("rollout timed out") }
	err := rig.r.rotate(context.Background(), "limbo")
	if err == nil || !strings.Contains(err.Error(), "rollout timed out") {
		t.Fatalf("err = %v, want the rollout failure", err)
	}
	var pod corev1.Pod
	if err := rig.cl.Get(context.Background(), client.ObjectKey{Namespace: "minecraft", Name: "login-0"}, &pod); err != nil {
		t.Error("the login pod was restarted although the api never rolled")
	}
}

func TestRotateRefusesAnUnknownCaller(t *testing.T) {
	rig := newRotationRig(t)
	writeTestFile(t, rig.r.secretsEnv, "SERVICE_TOKEN=proxy\n", 0o600)
	if err := rig.r.rotate(context.Background(), "admin"); err == nil {
		t.Fatal("rotated a token for an unknown caller")
	}
	if raw, _ := os.ReadFile(rig.r.secretsEnv); string(raw) != "SERVICE_TOKEN=proxy\n" {
		t.Errorf("secrets.env = %q, want it untouched", raw)
	}
	if len(rig.events) != 0 {
		t.Errorf("events = %v, want nothing touched", rig.events)
	}
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
		if !strings.Contains(script, key+`="${`+key+`:-$(openssl rand -hex 32)}"`) {
			t.Errorf("bootstrap.sh does not generate %s", key)
		}
		if !strings.Contains(script, key+"=${"+key+"}\n") {
			t.Errorf("bootstrap.sh does not persist %s to secrets.env", key)
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
