package main

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/operator"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/registrygate"

	"github.com/BurntSushi/toml"
	"github.com/jackc/pgx/v5"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rotate-token replaces one credential the installer generated: an internal
// caller's token (naming.CallerTokens), the registry's write tokens, the
// Velocity forwarding secret or the database password. The new value goes into
// the installer's record first, so that whatever fails later a re-run of the
// installer puts it everywhere; then into the Secrets and files that carry it;
// then whatever read the old value at start restarts. Without -yes it prints
// what it would change and what that interrupts, and changes nothing.

const (
	defaultSecretsEnvPath = "/etc/felis/secrets.env"
	defaultLinkPropsPath  = "/opt/felis/velocity/plugins/felis-link/felis-link.properties"
	defaultForwardingPath = "/opt/felis/velocity/forwarding.secret"
	velocityUnit          = "felis-velocity"
	apiDeployment         = "felis-api"
	registryDeployment    = "registry"

	kindRegistry   = "registry"
	kindForwarding = "forwarding"
	kindDB         = "db"

	// proxyReloadWait is how long the host proxy gets, once felis-api has rolled,
	// to show it re-read its token: it reads the file again on its next call to
	// felis-api, and it calls every 15 seconds.
	proxyReloadWait = 60 * time.Second

	// apiRestartNote is in every plan: felis-api runs as one replica replaced in
	// place, so its restart is a short outage of everything that talks to it.
	apiRestartNote = "felis-api restarts (a single replica): the panel, sign-in and the proxy's calls are unavailable for the few seconds that takes"
)

// installerTokenKeys names each caller's token in the installer's secrets.env
// (deploy/bootstrap.sh load_or_make_secrets). A re-run of the installer applies
// these values to the Secrets, so a rotation that skipped the file would be
// undone by the next upgrade.
var installerTokenKeys = map[string]string{
	"velocity": "SERVICE_TOKEN",
	"limbo":    "LIMBO_TOKEN",
	"build":    "BUILD_TOKEN",
	"ops":      "OPS_TOKEN",
}

// installerRegistryKeys names the registry principals' tokens in secrets.env,
// in registrygate.Principals order.
var installerRegistryKeys = map[string]string{
	registrygate.PrincipalPlatform: "REGISTRY_PLATFORM_TOKEN",
	registrygate.PrincipalBuild:    "REGISTRY_BUILD_TOKEN",
	registrygate.PrincipalPrune:    "REGISTRY_PRUNE_TOKEN",
}

// installerForwardingKey and installerDBKey name the forwarding secret and the
// database password in secrets.env.
const (
	installerForwardingKey = "FORWARDING_SECRET"
	installerDBKey         = "DB_PASSWORD"
)

type tokenRotator struct {
	cl          client.Client
	controlNS   string
	minecraftNS string
	buildNS     string
	// secretsEnv, linkProps and forwardingFile are the installer's record and the
	// host proxy's felis-link.properties and forwarding.secret; a missing file is
	// reported and skipped.
	secretsEnv     string
	linkProps      string
	forwardingFile string
	// hostTOML, podTOML and defaultTOML are the config copies that carry the
	// database URL (defaultTOML only when it is a file of its own).
	hostTOML    string
	podTOML     string
	defaultTOML string
	newToken    func() (string, error)
	// rollout restarts a control-namespace Deployment and waits for it.
	rollout func(ctx context.Context, deployment string) error
	// restartUnit restarts a systemd unit on this host.
	restartUnit func(ctx context.Context, unit string) error
	// proxyLog is what the host proxy has logged since a moment, as far as it
	// can be read.
	proxyLog func(ctx context.Context, since time.Time) string
	// alterRole stores a password verifier for a role of the database that runs
	// as deployment ("namespace/name"); verifyDB connects with a URL.
	alterRole func(ctx context.Context, deployment, role, verifier string) error
	verifyDB  func(ctx context.Context, url string) error
	now       func() time.Time
	// reloadWait bounds the wait for the proxy to re-read its token, polled
	// every pollEvery.
	reloadWait time.Duration
	pollEvery  time.Duration
	out        io.Writer
}

func cmdRotateToken(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rotate-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultSetupConfigPath, "path to felis.toml")
	secretsEnv := fs.String("secrets-env", defaultSecretsEnvPath, "the installer's secrets file, updated so a re-run keeps the new value")
	linkProps := fs.String("link-properties", defaultLinkPropsPath, "the host proxy's felis-link.properties (velocity only)")
	forwarding := fs.String("forwarding-secret", defaultForwardingPath, "the host proxy's forwarding secret file (forwarding only)")
	yes := fs.Bool("yes", false, "rotate; without it the plan is printed and nothing changes")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: felis rotate-token [-yes] [flags] <%s>\n\n", strings.Join(rotationKinds(), "|"))
		fmt.Fprintln(stderr, "Replaces one generated credential: the installer's record, the Secrets and files that carry it, then what reads it.")
		fmt.Fprintln(stderr, "Without -yes it prints what would change and what that interrupts.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	kind := fs.Arg(0)
	if !knownRotation(kind) {
		fmt.Fprintf(stderr, "felis rotate-token: unknown credential %q (one of %s)\n", kind, strings.Join(rotationKinds(), ", "))
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis rotate-token: refused — rotating writes the cluster Secrets and the installer's secrets file, so it must run as root (try: sudo felis rotate-token "+kind+")")
		return 1
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis rotate-token: %v\n", err)
		return 1
	}
	cl, err := buildSystemServerClient()
	if err != nil {
		fmt.Fprintf(stderr, "felis rotate-token: %v\n", err)
		return 1
	}
	buildNS := cfg.Registry.BuildNamespace
	if buildNS == "" {
		buildNS = platform.DefaultBuildNamespace
	}
	controlNS := platform.DefaultControlNamespace
	r := tokenRotator{
		cl:             cl,
		controlNS:      controlNS,
		minecraftNS:    cfg.K8s.Namespace,
		buildNS:        buildNS,
		secretsEnv:     *secretsEnv,
		linkProps:      *linkProps,
		forwardingFile: *forwarding,
		hostTOML:       hostSetupConfigPath,
		podTOML:        podSetupConfigPath,
		defaultTOML:    defaultSetupConfigPath,
		newToken:       randomToken,
		rollout: func(ctx context.Context, deployment string) error {
			if err := kubectl(ctx, "-n", controlNS, "rollout", "restart", "deployment/"+deployment); err != nil {
				return err
			}
			return kubectl(ctx, "-n", controlNS, "rollout", "status", "deployment/"+deployment, "--timeout=180s")
		},
		restartUnit: func(ctx context.Context, unit string) error { return systemctl(ctx, "restart", unit) },
		proxyLog:    journalSince,
		alterRole:   alterRoleInPod,
		verifyDB: func(ctx context.Context, url string) error {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			conn, err := pgx.Connect(ctx, url)
			if err != nil {
				return err
			}
			return conn.Close(ctx)
		},
		now:        time.Now,
		reloadWait: proxyReloadWait,
		pollEvery:  3 * time.Second,
		out:        stdout,
	}
	if err := r.rotate(context.Background(), kind, *yes); err != nil {
		fmt.Fprintf(stderr, "felis rotate-token: %v\n", err)
		return 1
	}
	return 0
}

func callerNames() []string {
	names := make([]string, 0, len(naming.CallerTokens))
	for _, ct := range naming.CallerTokens {
		names = append(names, ct.Caller)
	}
	return names
}

func callerToken(name string) (naming.CallerToken, bool) {
	for _, ct := range naming.CallerTokens {
		if ct.Caller == name {
			return ct, true
		}
	}
	return naming.CallerToken{}, false
}

// rotationKinds is every credential rotate-token replaces: the callers' tokens
// and the installer's other generated secrets.
func rotationKinds() []string {
	return append(callerNames(), kindRegistry, kindForwarding, kindDB)
}

func knownRotation(kind string) bool {
	for _, k := range rotationKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// randomToken is 32 random bytes in hex, the shape the installer generates.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// tokenFingerprint is how the proxy names the token it reloaded in its log
// (plugins/shared FileToken.fingerprint): the first twelve hex digits of its
// SHA-256, enough to tell tokens apart and useless for finding one.
func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

func (r tokenRotator) rotate(ctx context.Context, kind string, apply bool) error {
	switch kind {
	case kindRegistry:
		return r.rotateRegistry(ctx, apply)
	case kindForwarding:
		return r.rotateForwarding(ctx, apply)
	case kindDB:
		return r.rotateDB(ctx, apply)
	}
	ct, ok := callerToken(kind)
	if !ok {
		return fmt.Errorf("unknown credential %q (one of %s)", kind, strings.Join(rotationKinds(), ", "))
	}
	return r.rotateCaller(ctx, ct, apply)
}

// confirm ends the plan: without apply it says nothing changed and how to go
// ahead, and reports false.
func (r tokenRotator) confirm(kind string, apply bool) bool {
	if !apply {
		fmt.Fprintf(r.out, "\nNothing was changed. To rotate: sudo felis rotate-token -yes %s\n", kind)
		return false
	}
	fmt.Fprintln(r.out, "\nRotating:")
	return true
}

// record writes new values into the installer's secrets.env. It comes first in
// every rotation: from then on, whatever fails, a re-run of the installer puts
// the new values everywhere.
func (r tokenRotator) record(kv ...[2]string) error {
	keys := make([]string, len(kv))
	for i, p := range kv {
		keys[i] = p[0]
	}
	switch err := setKeyValueLines(r.secretsEnv, "=", kv); {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(r.out, "  - %s: not found, skipped (this host was not installed by deploy/bootstrap.sh)\n", r.secretsEnv)
	case err != nil:
		return fmt.Errorf("record the new value in %s: %w", r.secretsEnv, err)
	default:
		fmt.Fprintf(r.out, "  - %s: %s updated\n", r.secretsEnv, strings.Join(keys, ", "))
	}
	return nil
}

func (r tokenRotator) putSecret(ctx context.Context, ns, name string, data map[string][]byte) error {
	if _, err := putSecretKeys(ctx, r.cl, ns, name, corev1.SecretTypeOpaque, data); err != nil {
		return fmt.Errorf("write Secret %s/%s: %w", ns, name, err)
	}
	fmt.Fprintf(r.out, "  - Secret %s/%s: updated\n", ns, name)
	return nil
}

func (r tokenRotator) rollAPI(ctx context.Context) error {
	if err := r.rollout(ctx, apiDeployment); err != nil {
		return fmt.Errorf("roll felis-api: %w", err)
	}
	fmt.Fprintln(r.out, "  - felis-api: rolled out on the new value")
	return nil
}

// withMinecraft is the control namespace plus the minecraft namespace when that
// is a different one: where the Secrets game pods and Jobs mount are mirrored.
func (r tokenRotator) withMinecraft() []string {
	if r.minecraftNS == "" || r.minecraftNS == r.controlNS {
		return []string{r.controlNS}
	}
	return []string{r.controlNS, r.minecraftNS}
}

func (r tokenRotator) rotateCaller(ctx context.Context, ct naming.CallerToken, apply bool) error {
	namespaces := []string{r.controlNS}
	replica := map[string]string{"minecraft": r.minecraftNS, "build": r.buildNS}[ct.Replica]
	if replica != "" && replica != r.controlNS {
		namespaces = append(namespaces, replica)
	}
	key := installerTokenKeys[ct.Caller]

	fmt.Fprintf(r.out, "felis rotate-token %s: a new internal token for the %s caller\n", ct.Caller, ct.Caller)
	secrets := make([]string, len(namespaces))
	for i, ns := range namespaces {
		secrets[i] = "Secret " + ns + "/" + ct.Secret
	}
	writes := append([]string{r.secretsEnv + " (" + key + ")"}, secrets...)
	hostProxy := ct.Caller == "velocity" && fileExists(r.linkProps)
	if hostProxy {
		writes = append(writes, r.linkProps+" (service-token)")
	}
	fmt.Fprintf(r.out, "  - writes %s\n", strings.Join(writes, ", "))
	fmt.Fprintf(r.out, "  - %s\n", apiRestartNote)
	switch ct.Caller {
	case "velocity":
		if hostProxy {
			fmt.Fprintf(r.out, "  - the proxy re-reads its token from the file and keeps its players; if it has not within %s of felis-api's restart, it is restarted, which disconnects everyone online\n", r.reloadWait)
		} else {
			fmt.Fprintf(r.out, "  - no proxy on this host (%s): set service-token in your proxy's felis-link.properties to the value in Secret %s/%s afterwards; it re-reads the file without a restart\n",
				r.linkProps, r.controlNS, ct.Secret)
		}
	case "limbo":
		fmt.Fprintln(r.out, "  - the login gate's pod restarts: a player signing in at that moment reconnects")
	case "build":
		fmt.Fprintln(r.out, "  - a build fetching its context at that moment fails and can be submitted again")
	case "ops":
		fmt.Fprintln(r.out, "  - felis backup-now presents the new token on its next run")
	}
	if !r.confirm(ct.Caller, apply) {
		return nil
	}

	tok, err := r.newToken()
	if err != nil {
		return fmt.Errorf("generate a token: %w", err)
	}
	if err := r.record([2]string{key, tok}); err != nil {
		return err
	}
	for _, ns := range namespaces {
		if err := r.putSecret(ctx, ns, ct.Secret, map[string][]byte{naming.ServiceTokenSecretKey: []byte(tok)}); err != nil {
			return err
		}
	}

	// The proxy's file changes before felis-api rolls, so the file never falls
	// behind the Secret; the proxy's log is read from just before the write,
	// since it may pick the new token up before the rollout ends.
	since := r.now()
	if hostProxy {
		if err := setProxyToken(r.linkProps, tok); err != nil {
			return fmt.Errorf("write the proxy's token into %s: %w", r.linkProps, err)
		}
		fmt.Fprintf(r.out, "  - %s: service-token updated\n", r.linkProps)
	}

	if err := r.rollAPI(ctx); err != nil {
		return err
	}

	switch ct.Caller {
	case "velocity":
		if !hostProxy {
			break
		}
		if r.proxyReloaded(ctx, since, tokenFingerprint(tok)) {
			fmt.Fprintf(r.out, "  - %s: took the new token from its properties; players stayed connected\n", velocityUnit)
			break
		}
		if err := r.restartUnit(ctx, velocityUnit); err != nil {
			return fmt.Errorf("restart %s: %w", velocityUnit, err)
		}
		fmt.Fprintf(r.out, "  - %s: had not taken the new token within %s, restarted (players on the proxy were disconnected and can rejoin)\n", velocityUnit, r.reloadWait)
	case "limbo":
		// Only the operator's server pods carry its managed-by label; a backup or
		// restore Job's pod carries the server label too, and app.kubernetes.io ones.
		if err := r.cl.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(r.minecraftNS), client.MatchingLabels{
			v1alpha1.LabelServer:    naming.SystemLoginServer,
			v1alpha1.LabelManagedBy: operator.ManagedByValue,
		}); err != nil {
			return fmt.Errorf("restart the login gate: %w", err)
		}
		fmt.Fprintln(r.out, "  - login gate: pod restarted to read the new token")
	case "build":
		fmt.Fprintln(r.out, "  - builds: the next build Job reads the new token")
	case "ops":
		fmt.Fprintln(r.out, "  - felis backup-now reads the new token on its next run")
	}
	return nil
}

// proxyReloaded waits up to reloadWait for the host proxy to log that it
// reloaded the token with this fingerprint (plugins/shared FileToken).
func (r tokenRotator) proxyReloaded(ctx context.Context, since time.Time, fingerprint string) bool {
	want := "(fingerprint " + fingerprint + ")"
	deadline := r.now().Add(r.reloadWait)
	for {
		if strings.Contains(r.proxyLog(ctx, since), want) {
			return true
		}
		if !r.now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(r.pollEvery):
		}
	}
}

// journalSince is the proxy unit's journal from the second since falls in. A
// journal that cannot be read reads as one without the line, which ends in the
// restart a rotation made before the proxy could reload.
func journalSince(ctx context.Context, since time.Time) string {
	out, _ := exec.CommandContext(ctx, "journalctl", "-u", velocityUnit, "--since", "@"+strconv.FormatInt(since.Unix(), 10),
		"-o", "cat", "--no-pager", "-q").Output()
	return string(out)
}

// setProxyToken writes the proxy's token into felis-link.properties and puts
// the file's modification time back. The proxy re-reads its token by itself,
// while `felis domain check` and `felis domain set` read a file newer than the
// proxy's start as config it has not loaded (the installer's
// install_if_changed keeps the time for the same reason).
func setProxyToken(path, token string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := setKeyValueLine(path, "service-token", "=", token); err != nil {
		return err
	}
	return os.Chtimes(path, time.Time{}, info.ModTime())
}

func (r tokenRotator) rotateRegistry(ctx context.Context, apply bool) error {
	keys := make([]string, len(registrygate.Principals))
	for i, p := range registrygate.Principals {
		keys[i] = installerRegistryKeys[p]
	}
	fmt.Fprintf(r.out, "felis rotate-token %s: new write tokens for the image registry's principals (%s)\n", kindRegistry, strings.Join(registrygate.Principals, ", "))
	fmt.Fprintf(r.out, "  - writes %s (%s), Secret %s/%s, Secret %s/%s\n", r.secretsEnv, strings.Join(keys, ", "),
		r.controlNS, naming.RegistryAuthSecretName, r.buildNS, naming.RegistryPushSecretName)
	fmt.Fprintln(r.out, "  - the registry restarts to load them: a build pushing its image at that moment fails and can be submitted again, and an image pull in that moment retries")
	fmt.Fprintf(r.out, "  - %s (it presents the prune token)\n", apiRestartNote)
	if !r.confirm(kindRegistry, apply) {
		return nil
	}

	tokens := map[string][]byte{}
	kv := make([][2]string, 0, len(registrygate.Principals))
	for _, p := range registrygate.Principals {
		tok, err := r.newToken()
		if err != nil {
			return fmt.Errorf("generate a token: %w", err)
		}
		tokens[p] = []byte(tok)
		kv = append(kv, [2]string{installerRegistryKeys[p], tok})
	}
	if err := r.record(kv...); err != nil {
		return err
	}
	if err := r.putSecret(ctx, r.controlNS, naming.RegistryAuthSecretName, tokens); err != nil {
		return err
	}
	if err := r.putSecret(ctx, r.buildNS, naming.RegistryPushSecretName, map[string][]byte{
		naming.RegistryPushUsernameKey: []byte(registrygate.PrincipalBuild),
		naming.RegistryPushPasswordKey: tokens[registrygate.PrincipalBuild],
	}); err != nil {
		return err
	}
	if err := r.rollout(ctx, registryDeployment); err != nil {
		return fmt.Errorf("restart the registry: %w", err)
	}
	fmt.Fprintln(r.out, "  - registry: restarted on the new tokens")
	return r.rollAPI(ctx)
}

func (r tokenRotator) rotateForwarding(ctx context.Context, apply bool) error {
	restart, held, err := r.gamePods(ctx)
	if err != nil {
		return err
	}
	hostProxy := fileExists(r.forwardingFile)
	fmt.Fprintf(r.out, "felis rotate-token %s: a new Velocity forwarding secret, the key a server checks each player's identity with\n", kindForwarding)
	secrets := []string{}
	for _, ns := range r.withMinecraft() {
		secrets = append(secrets, "Secret "+ns+"/"+naming.ForwardingSecretName)
	}
	writes := append([]string{r.secretsEnv + " (" + installerForwardingKey + ")"}, secrets...)
	if hostProxy {
		writes = append(writes, r.forwardingFile)
	}
	fmt.Fprintf(r.out, "  - writes %s\n", strings.Join(writes, ", "))
	fmt.Fprintf(r.out, "  - every running server restarts to read it (%d now), saving its world on the way down, and the proxy restarts: everyone online is disconnected and can rejoin once their server is back\n", len(restart))
	if len(held) > 0 {
		fmt.Fprintf(r.out, "  - left running, because a backup, restore or file write holds its world: %s. Players cannot join it until it restarts: stop and start it from the panel once that finishes\n", strings.Join(held, ", "))
	}
	if !hostProxy {
		fmt.Fprintf(r.out, "  - no proxy on this host (%s): put the value in Secret %s/%s into your proxy's forwarding secret file and restart it\n",
			r.forwardingFile, r.controlNS, naming.ForwardingSecretName)
	}
	if !r.confirm(kindForwarding, apply) {
		return nil
	}

	tok, err := r.newToken()
	if err != nil {
		return fmt.Errorf("generate a secret: %w", err)
	}
	if err := r.record([2]string{installerForwardingKey, tok}); err != nil {
		return err
	}
	if hostProxy {
		info, err := os.Stat(r.forwardingFile)
		if err != nil {
			return err
		}
		if err := replaceFileKeepingMode(r.forwardingFile, info, []byte(tok)); err != nil {
			return fmt.Errorf("write %s: %w", r.forwardingFile, err)
		}
		fmt.Fprintf(r.out, "  - %s: updated\n", r.forwardingFile)
	}
	for _, ns := range r.withMinecraft() {
		if err := r.putSecret(ctx, ns, naming.ForwardingSecretName, map[string][]byte{naming.ForwardingSecretKey: []byte(tok)}); err != nil {
			return err
		}
	}
	// The servers go first: their pods read the Secret as they are recreated,
	// and a player who rejoins through the restarted proxy meets a server on the
	// new secret, or one still starting.
	for i := range restart {
		p := &restart[i]
		if err := r.cl.Delete(ctx, p, client.Preconditions{UID: &p.UID}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return fmt.Errorf("restart %s: %w", p.Labels[v1alpha1.LabelServer], err)
		}
	}
	fmt.Fprintf(r.out, "  - servers: %d restarting on the new secret\n", len(restart))
	if hostProxy {
		if err := r.restartUnit(ctx, velocityUnit); err != nil {
			return fmt.Errorf("restart %s: %w", velocityUnit, err)
		}
		fmt.Fprintf(r.out, "  - %s: restarted on the new secret\n", velocityUnit)
	}
	return nil
}

// gamePods lists the running game server pods: those a rotation may restart,
// and the servers left alone because a backup, restore or file write holds
// their world (internal/maintenance), which a restart in the middle of would
// break.
func (r tokenRotator) gamePods(ctx context.Context) ([]corev1.Pod, []string, error) {
	var pods corev1.PodList
	// The operator's managed-by label is on its server pods alone (see rotateCaller).
	if err := r.cl.List(ctx, &pods, client.InNamespace(r.minecraftNS), client.MatchingLabels{v1alpha1.LabelManagedBy: operator.ManagedByValue}); err != nil {
		return nil, nil, fmt.Errorf("list the servers' pods: %w", err)
	}
	var jobs batchv1.JobList
	if err := r.cl.List(ctx, &jobs, client.InNamespace(r.minecraftNS)); err != nil {
		return nil, nil, fmt.Errorf("list the maintenance Jobs: %w", err)
	}
	var restart []corev1.Pod
	var held []string
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		server := p.Labels[v1alpha1.LabelServer]
		var ms v1alpha1.MinecraftServer
		if err := r.cl.Get(ctx, client.ObjectKey{Namespace: r.minecraftNS, Name: server}, &ms); client.IgnoreNotFound(err) != nil {
			return nil, nil, fmt.Errorf("read server %s: %w", server, err)
		}
		if kind, ok := maintenance.Holder(server, ms.Annotations, jobs.Items, r.now()); ok {
			held = append(held, server+" ("+kind+")")
			continue
		}
		restart = append(restart, p)
	}
	return restart, held, nil
}

func (r tokenRotator) rotateDB(ctx context.Context, apply bool) error {
	cfg, err := config.Load(r.hostTOML)
	if err != nil {
		return err
	}
	if cfg.Database.Deployment == "" {
		return fmt.Errorf("[database] deployment is unset in %s, so the database is not the installer's felis-postgres: change the role's password where it runs, then in [database] url of each config copy", r.hostTOML)
	}
	u, err := neturl.Parse(cfg.Database.URL)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return fmt.Errorf("[database] url in %s names no role", r.hostTOML)
	}
	role := u.User.Username()
	targets, err := tomlTargetsOf(r.hostTOML, r.podTOML, r.defaultTOML)
	if err != nil {
		return err
	}
	paths := make([]string, len(targets))
	for i, t := range targets {
		paths[i] = t.path
	}
	secrets := []string{}
	for _, ns := range r.withMinecraft() {
		secrets = append(secrets, ns+"/"+platform.ConfigSecretName)
	}
	fmt.Fprintf(r.out, "felis rotate-token %s: a new password for the database role %q\n", kindDB, role)
	fmt.Fprintf(r.out, "  - writes %s (%s), the role in %s, [database] url in %s, Secret %s\n",
		r.secretsEnv, installerDBKey, cfg.Database.Deployment, strings.Join(paths, " and "), strings.Join(secrets, " and "))
	fmt.Fprintf(r.out, "  - %s\n", apiRestartNote)
	fmt.Fprintln(r.out, "  - a backup, restore or file Job that connects in the seconds between the password change and felis-api's restart fails and can be run again; the host's timers read the new config on their next run")
	if !r.confirm(kindDB, apply) {
		return nil
	}

	password, err := r.newToken()
	if err != nil {
		return fmt.Errorf("generate a password: %w", err)
	}
	// Every config copy is edited in memory first, so one this cannot edit stops
	// the rotation before the role's password changes.
	edited := make([][]byte, len(targets))
	var hostURL string
	var podConfig []byte
	for i, t := range targets {
		raw, err := os.ReadFile(t.real)
		if err != nil {
			return err
		}
		var doc struct {
			Database struct {
				URL string `toml:"url"`
			} `toml:"database"`
		}
		if _, err := toml.Decode(string(raw), &doc); err != nil {
			return fmt.Errorf("%s: %w", t.path, err)
		}
		next, err := withPassword(doc.Database.URL, password)
		if err != nil {
			return fmt.Errorf("%s: %w", t.path, err)
		}
		if edited[i], err = editTOMLStrings(raw, []tomlStringEdit{{"database", "url", next}}); err != nil {
			return fmt.Errorf("%s: %w; set the password in its [database] url by hand", t.path, err)
		}
		switch t.path {
		case r.hostTOML:
			hostURL = next
		case r.podTOML:
			podConfig = edited[i]
		}
	}
	if podConfig == nil {
		return fmt.Errorf("%s resolves to the same file as %s; the pods reach the database at another address and need a copy of their own", r.podTOML, r.hostTOML)
	}

	if err := r.record([2]string{installerDBKey, password}); err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	verifier, err := scramVerifier(password, salt, scramIterations)
	if err != nil {
		return err
	}
	if err := r.alterRole(ctx, cfg.Database.Deployment, role, verifier); err != nil {
		return fmt.Errorf("set the role's password: %w", err)
	}
	if err := r.verifyDB(ctx, hostURL); err != nil {
		return fmt.Errorf("the database does not accept the new password (%v); the config copies still hold the old one: run the installer again (sudo bash deploy/bootstrap.sh), which sets the password in %s everywhere", err, r.secretsEnv)
	}
	fmt.Fprintf(r.out, "  - role %s: password changed, and the database accepts it\n", role)
	for i, t := range targets {
		info, err := os.Stat(t.real)
		if err != nil {
			return err
		}
		if err := replaceFileKeepingMode(t.real, info, edited[i]); err != nil {
			return fmt.Errorf("write %s: %w", t.path, err)
		}
		fmt.Fprintf(r.out, "  - %s: [database] url updated\n", t.path)
	}
	for _, ns := range r.withMinecraft() {
		if err := r.putSecret(ctx, ns, platform.ConfigSecretName, map[string][]byte{platform.ConfigSecretKey: podConfig}); err != nil {
			return err
		}
	}
	return r.rollAPI(ctx)
}

// withPassword is a database URL with its password replaced.
func withPassword(raw, password string) (string, error) {
	u, err := neturl.Parse(raw)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return "", errors.New("its [database] url names no role")
	}
	u.User = neturl.UserPassword(u.User.Username(), password)
	return u.String(), nil
}

// scramIterations is PostgreSQL's default scram_iterations.
const scramIterations = 4096

// scramVerifier is the SCRAM-SHA-256 verifier PostgreSQL stores for a password
// (RFC 5802 and RFC 7677, in the form libpq's PQencryptPasswordConn makes).
// ALTER ROLE stores a verifier as it is given, so the password itself never
// reaches the server, where a failing statement is logged with its text.
func scramVerifier(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := func(msg string) []byte {
		h := hmac.New(sha256.New, salted)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac("Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(mac("Server Key"))), nil
}

// alterRoleInPod runs ALTER ROLE as the superuser inside the database's
// container, over its socket, with the statement on stdin.
func alterRoleInPod(ctx context.Context, deployment, role, verifier string) error {
	ns, name, _ := strings.Cut(deployment, "/")
	return kubectlWithInput(ctx, []byte(alterRoleSQL(role, verifier)), "-n", ns, "exec", "-i", "deploy/"+name, "-c", platform.PostgresContainer, "--",
		"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres")
}

// alterRoleSQL sets role's password to a SCRAM verifier, which holds no quote.
func alterRoleSQL(role, verifier string) string {
	return "ALTER ROLE \"" + strings.ReplaceAll(role, `"`, `""`) + "\" WITH PASSWORD '" + verifier + "';\n"
}

// setKeyValueLine rewrites the `key<sep>value` line of a flat key/value file
// (secrets.env, a .properties file), appending one when the key is absent. The
// file is replaced atomically and keeps its mode and owner: felis-link.properties
// is root:felis-velocity 0640, and the proxy must still be able to read it.
func setKeyValueLine(path, key, sep, value string) error {
	return setKeyValueLines(path, sep, [][2]string{{key, value}})
}

// setKeyValueLines is setKeyValueLine for several keys in one rewrite.
func setKeyValueLines(path, sep string, kv [][2]string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for _, p := range kv {
		key, value := p[0], p[1]
		found := false
		for i, ln := range lines {
			k, _, ok := strings.Cut(ln, sep)
			if ok && strings.TrimSpace(k) == key {
				lines[i] = key + sep + value
				found = true
			}
		}
		if !found {
			lines = append(lines, key+sep+value)
		}
	}
	return replaceFileKeepingMode(path, info, []byte(strings.Join(lines, "\n")+"\n"))
}

// replaceFileKeepingMode atomically replaces path with data, keeping the mode and
// owner info describes: these files are read by other users (the proxy's) and
// some hold credentials, so a rewrite must not widen or narrow who can read them.
func replaceFileKeepingMode(path string, info os.FileInfo, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil {
			tmp.Close()
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
