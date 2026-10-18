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

// Console returns ErrNotFound or ErrConsoleUnavailable; RCON passwords must never appear in arguments or returned output.
type Console interface {
	RunCommand(ctx context.Context, name, command string) (string, error)
}

type K8sConsole struct {
	c         client.Client
	namespace string
	timeout   time.Duration
}

func NewK8sConsole(c client.Client, namespace string) *K8sConsole {
	return &K8sConsole{c: c, namespace: namespace, timeout: 5 * time.Second}
}

// RunCommand hides dial, authentication and Secret errors behind ErrConsoleUnavailable.
func (k *K8sConsole) RunCommand(ctx context.Context, name, command string) (string, error) {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	if !ms.Spec.Rcon.Enabled {
		return "", ErrConsoleUnavailable
	}

	password, err := k.rconPassword(ctx, &ms)
	if err != nil {
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
		return "", ErrConsoleUnavailable
	}
	defer conn.Close()
	// Honor longer caller timeouts so large-world save-all flushes can finish.
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return "", ErrConsoleUnavailable
		}
	}

	out, err := conn.ExecuteContext(ctx, command)
	if err != nil {
		return "", ErrConsoleUnavailable
	}
	return out, nil
}

// rconEndpoint must match the operator's rconAddress convention.
func rconEndpoint(server *v1alpha1.MinecraftServer) string {
	port := server.Spec.Rcon.Port
	if port <= 0 {
		port = rcon.DefaultPort
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", server.Name, server.Namespace, port)
}

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
