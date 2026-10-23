package api

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/rcon"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Console separates reply-dependent RCON calls from interactive submissions.
type Console interface {
	RunCommand(ctx context.Context, name, command string) (string, error)
	SubmitCommand(ctx context.Context, name, command string) (ConsoleCommandResult, error)
}

type ConsoleCommandResult struct {
	Name      string `json:"name"`
	Output    string `json:"output"`
	Transport string `json:"transport"`
}

type consoleRCON interface {
	ExecuteContext(context.Context, string) (string, error)
	SetDeadline(time.Time) error
	Close() error
}

type K8sConsole struct {
	c             client.Client
	clientset     kubernetes.Interface
	config        *rest.Config
	namespace     string
	timeout       time.Duration
	submitTimeout time.Duration
	dial          func(string, string, time.Duration) (consoleRCON, error)
	executor      func(*rest.Config, string, *url.URL) (remotecommand.Executor, error)
	mu            sync.Mutex
	slots         map[string]*consoleSlot
}

func NewK8sConsole(c client.Client, cs kubernetes.Interface, config *rest.Config, namespace string) *K8sConsole {
	return &K8sConsole{
		c: c, clientset: cs, config: config, namespace: namespace,
		timeout: 5 * time.Second, submitTimeout: 10 * time.Second,
		dial: func(addr, password string, timeout time.Duration) (consoleRCON, error) {
			return rcon.Dial(addr, password, timeout)
		},
		executor: remotecommand.NewSPDYExecutor,
	}
}

// RunCommand hides dial, authentication and Secret errors behind ErrConsoleUnavailable.
func (k *K8sConsole) RunCommand(ctx context.Context, name, command string) (string, error) {
	ms, err := k.consoleServer(ctx, name)
	if err != nil {
		return "", err
	}
	conn, err := k.connectRCON(ctx, ms)
	if err != nil {
		return "", ErrConsoleUnavailable
	}
	defer conn.Close()
	out, err := conn.ExecuteContext(ctx, command)
	if err != nil {
		return "", ErrConsoleUnavailable
	}
	return out, nil
}

func (k *K8sConsole) consoleServer(ctx context.Context, name string) (*v1alpha1.MinecraftServer, error) {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, ErrConsoleUnavailable
	}
	return &ms, nil
}

func (k *K8sConsole) connectRCON(ctx context.Context, ms *v1alpha1.MinecraftServer) (consoleRCON, error) {
	if ctx.Err() != nil || !ms.Spec.Rcon.Enabled {
		return nil, ErrConsoleUnavailable
	}

	password, err := k.rconPassword(ctx, ms)
	if err != nil {
		return nil, ErrConsoleUnavailable
	}

	timeout := k.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining <= 0 {
			return nil, ErrConsoleUnavailable
		} else if remaining < timeout {
			timeout = remaining
		}
	}

	conn, err := k.dial(rconEndpoint(ms), password, timeout)
	if err != nil {
		return nil, ErrConsoleUnavailable
	}
	if ctx.Err() != nil {
		conn.Close()
		return nil, ErrConsoleUnavailable
	}
	// Honor longer caller timeouts so large-world save-all flushes can finish.
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			conn.Close()
			return nil, ErrConsoleUnavailable
		}
	}

	return conn, nil
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
