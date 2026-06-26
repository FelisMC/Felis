package api

import (
	"context"
	"fmt"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/rcon"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Console is the synchronous RCON write channel the API depends on (spec §8,
// 读写分离: 写=RCON). RunCommand sends one command to the named server's RCON
// endpoint and returns the server's reply. It returns ErrNotFound if no server
// of that name exists, and ErrConsoleUnavailable if the RCON channel cannot be
// reached (dial timeout / refused / auth rejected) — which, because readiness
// IS an RCON probe (spec §141), is the expected outcome when the server isn't
// truly Running. The RCON password is resolved internally and is NEVER part of
// any argument or return value (spec §286: RCON 密码绝不下发前端).
//
// It is an interface so handlers are tested against a fake (api_test.go); the
// controller-runtime implementation (K8sConsole) is integration-tested only.
type Console interface {
	RunCommand(ctx context.Context, name, command string) (string, error)
}

// K8sConsole is the production Console: it resolves the per-server RCON password
// from the Secret named by the CRD's spec.rcon.secretRef, dials the in-cluster
// RCON endpoint, and runs one command (spec §8 写=RCON). It mirrors the
// operator's readiness prober — the same address convention and the same
// secret-read path as internal/operator (rconAddress / reconciler.rconPassword)
// — so the single reviewed way to reach a server's RCON is the only way the API
// reaches it too.
//
// INTEGRATION-ONLY: like K8sCluster / pgRepo this needs a live cluster and a
// reachable RCON port; it compiles here but is exercised only by integration
// tests against a real cluster, never by the hermetic api_test.go suite. The
// Oracle verifies the handler layer (handleCommand) against a fake Console.
//
// Security: the resolved password authenticates the dial and is never logged
// nor placed in any return value — only Execute's reply body (the command
// output) flows back to the caller (spec §286). Port 25575 is reachable only
// from felis-api by NetworkPolicy (spec §8), so this dial is the single
// sanctioned write path.
type K8sConsole struct {
	c         client.Client
	namespace string
	timeout   time.Duration
}

// NewK8sConsole builds a Console over c, scoped to namespace.
func NewK8sConsole(c client.Client, namespace string) *K8sConsole {
	return &K8sConsole{c: c, namespace: namespace, timeout: 5 * time.Second}
}

// RunCommand reads the server CRD, resolves its RCON password, dials the
// in-cluster RCON endpoint, and runs command. Every unreachable/auth/secret
// failure collapses to ErrConsoleUnavailable (the handler maps it to 503) so no
// driver detail — and certainly no password — ever reaches the caller.
func (k *K8sConsole) RunCommand(ctx context.Context, name, command string) (string, error) {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	if !ms.Spec.Rcon.Enabled {
		// No RCON means no write channel at all (spec §8).
		return "", ErrConsoleUnavailable
	}

	password, err := k.rconPassword(ctx, &ms)
	if err != nil {
		// A missing/garbled secret is a server misconfiguration, but to the caller
		// it still means the console cannot be reached — and the underlying error
		// must not leak. Surface it as unavailable.
		return "", ErrConsoleUnavailable
	}

	timeout := k.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}

	conn, err := rcon.Dial(rconEndpoint(&ms), password, timeout)
	if err != nil {
		// Dial refused / timed out / auth rejected: the channel is not reachable.
		// Per spec §141 readiness IS this probe, so this is the expected "not
		// actually up" outcome — surfaced as 503, not 500.
		return "", ErrConsoleUnavailable
	}
	defer conn.Close()

	out, err := conn.Execute(command)
	if err != nil {
		return "", ErrConsoleUnavailable
	}
	return out, nil
}

// rconEndpoint is the in-cluster RCON address for a server, matching the
// operator's convention (internal/operator builders.rconAddress): the headless
// client Service is named after the server, in its own namespace.
func rconEndpoint(server *v1alpha1.MinecraftServer) string {
	port := server.Spec.Rcon.Port
	if port <= 0 {
		port = rcon.DefaultPort
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", server.Name, server.Namespace, port)
}

// rconPassword resolves the RCON password from the Secret named by the CRD's
// spec.rcon.secretRef. It mirrors internal/operator reconciler.rconPassword
// exactly so the API reads the credential the same reviewed way the operator
// does. The returned value is used solely to authenticate the dial.
func (k *K8sConsole) rconPassword(ctx context.Context, server *v1alpha1.MinecraftServer) (string, error) {
	ref := server.Spec.Rcon.SecretRef
	if ref.Name == "" || ref.Key == "" {
		return "", fmt.Errorf("rcon.secretRef.name and .key are required when rcon is enabled")
	}
	var secret corev1.Secret
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &secret); err != nil {
		return "", err
	}
	b, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %q has no key %q", ref.Name, ref.Key)
	}
	return string(b), nil
}
