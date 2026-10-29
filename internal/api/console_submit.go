package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

type consoleSlot struct {
	refs  int
	token chan struct{}
}

// acquireSubmit serializes submissions per server and honors ctx while waiting.
func (k *K8sConsole) acquireSubmit(ctx context.Context, name string) (func(), error) {
	k.mu.Lock()
	if k.slots == nil {
		k.slots = map[string]*consoleSlot{}
	}
	slot := k.slots[name]
	if slot == nil {
		slot = &consoleSlot{token: make(chan struct{}, 1)}
		slot.token <- struct{}{}
		k.slots[name] = slot
	}
	slot.refs++
	k.mu.Unlock()
	leave := func(held bool) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if held {
			slot.token <- struct{}{}
		}
		slot.refs--
		if slot.refs == 0 {
			delete(k.slots, name)
		}
	}
	select {
	case <-ctx.Done():
		leave(false)
		return nil, ErrConsoleUnavailable
	case <-slot.token:
		return func() { leave(true) }, nil
	}
}

func consoleAcceptsInput(ms *v1alpha1.MinecraftServer) bool {
	return ms.DeletionTimestamp == nil && ms.Spec.DesiredState == v1alpha1.DesiredRunning &&
		(ms.Status.Phase == v1alpha1.PhaseStarting || ms.Status.Phase == v1alpha1.PhaseRunning)
}

// SubmitCommand switches transport only while the first one has delivered nothing, so nothing runs twice.
func (k *K8sConsole) SubmitCommand(ctx context.Context, name, command string, preferStdin bool) (ConsoleCommandResult, error) {
	command, err := normalizeConsoleCommand(command)
	if err != nil {
		return ConsoleCommandResult{}, err
	}
	limit := k.submitTimeout
	if limit <= 0 {
		limit = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	release, err := k.acquireSubmit(ctx, name)
	if err != nil {
		return ConsoleCommandResult{}, err
	}
	defer release()
	if ctx.Err() != nil {
		return ConsoleCommandResult{}, ErrConsoleUnavailable
	}
	ms, err := k.consoleServer(ctx, name)
	if err != nil {
		return ConsoleCommandResult{}, err
	}
	if !consoleAcceptsInput(ms) {
		return ConsoleCommandResult{}, ErrConsoleNotRunning
	}
	if preferStdin {
		err := k.submitStdin(ctx, name, command)
		if err == nil {
			return ConsoleCommandResult{Name: name, Output: "", Transport: "stdin"}, nil
		}
		if !errors.Is(err, ErrConsoleUnavailable) {
			return ConsoleCommandResult{}, err
		}
	}
	// The handshake can take seconds, so recheck the server before sending input.
	recheck := func() error {
		current, err := k.consoleServer(ctx, name)
		if err != nil {
			return err
		}
		if current.UID != ms.UID || !consoleAcceptsInput(current) {
			return ErrConsoleNotRunning
		}
		return nil
	}
	var output string
	var checkErr error
	sent := false
	rconErr := k.withRCON(ctx, ms, func(conn consoleRCON) error {
		if checkErr = recheck(); checkErr != nil {
			return checkErr
		}
		sent = true
		var err error
		output, err = conn.ExecuteContext(ctx, command)
		return err
	})
	switch {
	case sent && rconErr != nil:
		return ConsoleCommandResult{}, ErrConsoleOutcomeUnknown
	case sent:
		return ConsoleCommandResult{Name: name, Output: output, Transport: "rcon"}, nil
	case checkErr != nil:
		return ConsoleCommandResult{}, checkErr
	case ctx.Err() != nil:
		return ConsoleCommandResult{}, ErrConsoleUnavailable
	}
	if err := recheck(); err != nil {
		return ConsoleCommandResult{}, err
	}
	if preferStdin {
		return ConsoleCommandResult{}, ErrConsoleUnavailable
	}
	if err := k.submitStdin(ctx, name, command); err != nil {
		return ConsoleCommandResult{}, err
	}
	return ConsoleCommandResult{Name: name, Output: "", Transport: "stdin"}, nil
}

type consoleStdin struct {
	io.Reader
	read atomic.Int64
}

func (s *consoleStdin) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	s.read.Add(int64(n))
	return n, err
}

func (k *K8sConsole) submitStdin(ctx context.Context, name, command string) error {
	pods, err := k.clientset.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name + "-0",
		LabelSelector: v1alpha1.LabelServer + "=" + name + "," + v1alpha1.LabelComponent + "=" + gamePodComponent,
	})
	if err != nil {
		return ErrConsoleUnavailable
	}
	podName := ""
	for _, pod := range pods.Items {
		if pod.Name != name+"-0" || pod.DeletionTimestamp != nil ||
			pod.Labels[v1alpha1.LabelServer] != name || pod.Labels[v1alpha1.LabelComponent] != gamePodComponent {
			continue
		}
		running, writable := false, false
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == serverLogContainer && status.State.Running != nil {
				running = true
			}
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == serverLogContainer && container.Stdin && !container.StdinOnce && !container.TTY {
				writable = true
			}
		}
		if running && writable {
			podName = pod.Name
			break
		}
	}
	if podName == "" || ctx.Err() != nil {
		return ErrConsoleUnavailable
	}
	req := k.clientset.CoreV1().RESTClient().Post().Namespace(k.namespace).Resource("pods").Name(podName).SubResource("attach")
	req.VersionedParams(&corev1.PodAttachOptions{Container: serverLogContainer, Stdin: true, Stdout: false, Stderr: false, TTY: false}, scheme.ParameterCodec)
	executor, err := k.executor(k.config, http.MethodPost, req.URL())
	if err != nil || ctx.Err() != nil {
		return ErrConsoleUnavailable
	}
	input := &consoleStdin{Reader: strings.NewReader(command + "\n")}
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: input, Tty: false})
	if input.read.Load() == 0 {
		return ErrConsoleUnavailable
	}
	if err != nil || input.read.Load() != int64(len(command)+1) {
		return ErrConsoleOutcomeUnknown
	}
	return nil
}
