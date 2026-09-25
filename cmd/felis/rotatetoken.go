package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/platform"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rotate-token replaces one internal caller's token (naming.CallerTokens): a new
// value goes into the installer's record, the control-namespace Secret and the
// replica the caller's pods mount, felis-api rolls so it accepts only the new
// value, and then the caller restarts so it presents it. Between the api's
// rollout and the caller's restart the caller is turned away with 401; for the
// login gate and the proxy that is the few seconds of a pod or unit restart.

const (
	defaultSecretsEnvPath = "/etc/felis/secrets.env"
	defaultLinkPropsPath  = "/opt/felis/velocity/plugins/felis-link/felis-link.properties"
	velocityUnit          = "felis-velocity"
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

type tokenRotator struct {
	cl          client.Client
	controlNS   string
	minecraftNS string
	buildNS     string
	// secretsEnv and linkProps are the installer's record and the proxy's
	// felis-link.properties; a missing file is reported and skipped.
	secretsEnv string
	linkProps  string
	newToken   func() (string, error)
	// rollAPI restarts felis-api and waits for the rollout.
	rollAPI func(ctx context.Context) error
	// restartUnit restarts a systemd unit on this host.
	restartUnit func(ctx context.Context, unit string) error
	out         io.Writer
}

func cmdRotateToken(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rotate-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultSetupConfigPath, "path to felis.toml")
	secretsEnv := fs.String("secrets-env", defaultSecretsEnvPath, "the installer's secrets file, updated so a re-run keeps the new value")
	linkProps := fs.String("link-properties", defaultLinkPropsPath, "the host proxy's felis-link.properties (velocity only)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: felis rotate-token [flags] <%s>\n\n", strings.Join(callerNames(), "|"))
		fmt.Fprintln(stderr, "Replaces one internal caller's token: the Secrets, felis-api, then the caller itself.")
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
	if _, ok := callerToken(fs.Arg(0)); !ok {
		fmt.Fprintf(stderr, "felis rotate-token: unknown caller %q (one of %s)\n", fs.Arg(0), strings.Join(callerNames(), ", "))
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "felis rotate-token: refused — rotating writes the cluster Secrets and the installer's secrets file, so it must run as root (try: sudo felis rotate-token "+fs.Arg(0)+")")
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
	r := tokenRotator{
		cl:          cl,
		controlNS:   platform.DefaultControlNamespace,
		minecraftNS: cfg.K8s.Namespace,
		buildNS:     buildNS,
		secretsEnv:  *secretsEnv,
		linkProps:   *linkProps,
		newToken:    randomToken,
		rollAPI: func(ctx context.Context) error {
			if err := kubectl(ctx, "-n", platform.DefaultControlNamespace, "rollout", "restart", "deployment/felis-api"); err != nil {
				return err
			}
			return kubectl(ctx, "-n", platform.DefaultControlNamespace, "rollout", "status", "deployment/felis-api", "--timeout=180s")
		},
		restartUnit: func(ctx context.Context, unit string) error { return systemctl(ctx, "restart", unit) },
		out:         stdout,
	}
	if err := r.rotate(context.Background(), fs.Arg(0)); err != nil {
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

// randomToken is 32 random bytes in hex, the shape the installer generates.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (r tokenRotator) rotate(ctx context.Context, caller string) error {
	ct, ok := callerToken(caller)
	if !ok {
		return fmt.Errorf("unknown caller %q", caller)
	}
	tok, err := r.newToken()
	if err != nil {
		return fmt.Errorf("generate a token: %w", err)
	}

	// The installer's record first: from here on, whatever fails, a re-run of the
	// installer puts the new value everywhere.
	switch err := setKeyValueLine(r.secretsEnv, installerTokenKeys[ct.Caller], "=", tok); {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(r.out, "  - %s: not found, skipped (this host was not installed by deploy/bootstrap.sh)\n", r.secretsEnv)
	case err != nil:
		return fmt.Errorf("record the new token in %s: %w", r.secretsEnv, err)
	default:
		fmt.Fprintf(r.out, "  - %s: %s updated\n", r.secretsEnv, installerTokenKeys[ct.Caller])
	}

	namespaces := []string{r.controlNS}
	replica := map[string]string{"minecraft": r.minecraftNS, "build": r.buildNS}[ct.Replica]
	if replica != "" && replica != r.controlNS {
		namespaces = append(namespaces, replica)
	}
	for _, ns := range namespaces {
		if err := writeTokenSecret(ctx, r.cl, ns, ct.Secret, tok); err != nil {
			return fmt.Errorf("write Secret %s/%s: %w", ns, ct.Secret, err)
		}
		fmt.Fprintf(r.out, "  - Secret %s/%s: updated\n", ns, ct.Secret)
	}

	hostProxy := false
	if ct.Caller == "velocity" {
		switch err := setKeyValueLine(r.linkProps, "service-token", "=", tok); {
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(r.out, "  - %s: not found; set service-token in your proxy's felis-link.properties to the value in Secret %s/%s and restart it\n",
				r.linkProps, r.controlNS, ct.Secret)
		case err != nil:
			return fmt.Errorf("write the proxy's token into %s: %w", r.linkProps, err)
		default:
			hostProxy = true
			fmt.Fprintf(r.out, "  - %s: service-token updated\n", r.linkProps)
		}
	}

	if err := r.rollAPI(ctx); err != nil {
		return fmt.Errorf("roll felis-api: %w", err)
	}
	fmt.Fprintln(r.out, "  - felis-api: rolled out, accepting only the new token")

	switch ct.Caller {
	case "velocity":
		if hostProxy {
			if err := r.restartUnit(ctx, velocityUnit); err != nil {
				return fmt.Errorf("restart %s: %w", velocityUnit, err)
			}
			fmt.Fprintf(r.out, "  - %s: restarted (players on the proxy were disconnected and can rejoin)\n", velocityUnit)
		}
	case "limbo":
		if err := r.cl.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(r.minecraftNS),
			client.MatchingLabels{v1alpha1.LabelServer: naming.SystemLoginServer}); err != nil {
			return fmt.Errorf("restart the login gate: %w", err)
		}
		fmt.Fprintln(r.out, "  - login gate: pod restarted to read the new token")
	case "build":
		fmt.Fprintln(r.out, "  - builds: the next build Job reads the new token; one fetching its context right now fails and can be submitted again")
	case "ops":
		fmt.Fprintln(r.out, "  - felis backup-now reads the new token on its next run")
	}
	return nil
}

// writeTokenSecret sets the token in a Secret, creating it when absent.
func writeTokenSecret(ctx context.Context, cl client.Client, namespace, name, token string) error {
	var sec corev1.Secret
	err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec)
	if apierrors.IsNotFound(err) {
		return cl.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{naming.ServiceTokenSecretKey: []byte(token)},
		})
	}
	if err != nil {
		return err
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[naming.ServiceTokenSecretKey] = []byte(token)
	return cl.Update(ctx, &sec)
}

// setKeyValueLine rewrites the `key<sep>value` line of a flat key/value file
// (secrets.env, a .properties file), appending one when the key is absent. The
// file is replaced atomically and keeps its mode and owner: felis-link.properties
// is root:felis-velocity 0640, and the proxy must still be able to read it.
func setKeyValueLine(path, key, sep, value string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
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
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
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
