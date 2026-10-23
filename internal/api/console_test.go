package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func (f *fakeConsole) SubmitCommand(ctx context.Context, name, command string) (ConsoleCommandResult, error) {
	output, err := f.RunCommand(ctx, name, command)
	return ConsoleCommandResult{Name: name, Output: output, Transport: "rcon"}, err
}

func (s *scriptedConsole) SubmitCommand(context.Context, string, string) (ConsoleCommandResult, error) {
	panic("reply-dependent operations must use RunCommand")
}

type consoleExecutorFunc func(context.Context, remotecommand.StreamOptions) error

func (f consoleExecutorFunc) Stream(options remotecommand.StreamOptions) error {
	return f(context.Background(), options)
}

func (f consoleExecutorFunc) StreamWithContext(ctx context.Context, options remotecommand.StreamOptions) error {
	return f(ctx, options)
}

type fakeConsoleRCON struct {
	output      string
	err         error
	deadlineErr error
	executed    atomic.Int32
	closed      atomic.Int32
}

func (c *fakeConsoleRCON) ExecuteContext(context.Context, string) (string, error) {
	c.executed.Add(1)
	return c.output, c.err
}

func (c *fakeConsoleRCON) SetDeadline(time.Time) error { return c.deadlineErr }
func (c *fakeConsoleRCON) Close() error                { c.closed.Add(1); return nil }

func consoleTestPod(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: "minecraft", UID: types.UID(name + "-pod"), Labels: map[string]string{
			v1alpha1.LabelServer: name, v1alpha1.LabelComponent: gamePodComponent,
		}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: serverLogContainer, Stdin: true}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: serverLogContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
}

func newConsoleTestBackend(t *testing.T, pods []corev1.Pod) (*K8sConsole, *v1alpha1.MinecraftServer) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ms := &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft", UID: "server-uid"},
		Spec:       v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredRunning},
		Status:     v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseRunning},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ms).WithStatusSubresource(ms).Build()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/minecraft/pods" {
			t.Errorf("unexpected Kubernetes request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: pods})
	}))
	t.Cleanup(server.Close)
	cfg := &rest.Config{Host: server.URL}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	k := NewK8sConsole(cl, cs, cfg, "minecraft")
	k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) {
		return nil, errors.New("unexpected attach")
	}
	return k, ms
}

func enableConsoleTestRCON(t *testing.T, k *K8sConsole, ms *v1alpha1.MinecraftServer) {
	t.Helper()
	if err := k.c.Get(context.Background(), client.ObjectKeyFromObject(ms), ms); err != nil {
		t.Fatal(err)
	}
	ms.Spec.Rcon = v1alpha1.RconSpec{Enabled: true, SecretRef: v1alpha1.SecretKeyRef{Name: "fake-rcon", Key: "password"}}
	if err := k.c.Update(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fake-rcon", Namespace: "minecraft"}, Data: map[string][]byte{"password": []byte("fake-private-password")}}
	if err := k.c.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleSubmitPrefersRCONAndNeverRepeatsExecution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		execErr error
		want    error
	}{
		{"reply", nil, nil},
		{"connection lost after execution", io.EOF, ErrConsoleOutcomeUnknown},
		{"reply timeout", context.DeadlineExceeded, ErrConsoleOutcomeUnknown},
		{"malformed reply", errors.New("fake-private-password malformed reply"), ErrConsoleOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, ms := newConsoleTestBackend(t, nil)
			enableConsoleTestRCON(t, k, ms)
			conn := &fakeConsoleRCON{output: "reply", err: tc.execErr}
			k.dial = func(string, string, time.Duration) (consoleRCON, error) { return conn, nil }
			var attached atomic.Int32
			k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) {
				attached.Add(1)
				return nil, errors.New("must not attach")
			}
			got, err := k.SubmitCommand(context.Background(), "survival", "list")
			if !errors.Is(err, tc.want) || conn.executed.Load() != 1 || attached.Load() != 0 || conn.closed.Load() != 1 {
				t.Fatalf("command repeated or failure lost: result=%+v err=%v execute=%d attach=%d closed=%d", got, err, conn.executed.Load(), attached.Load(), conn.closed.Load())
			}
			if err == nil && (got.Name != "survival" || got.Transport != "rcon" || got.Output != "reply") {
				t.Fatalf("RCON result=%+v", got)
			}
			if err != nil && strings.Contains(err.Error(), "fake-private-password") {
				t.Fatal("RCON diagnostic leaked a credential")
			}
		})
	}
}

func TestConsoleSubmitFallsBackOnlyBeforeExecution(t *testing.T) {
	for _, mode := range []string{"disabled", "missing secret", "dial", "authentication", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			k, ms := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival")})
			conn := &fakeConsoleRCON{}
			if mode != "disabled" {
				enableConsoleTestRCON(t, k, ms)
			}
			if mode == "missing secret" {
				if err := k.c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fake-rcon", Namespace: "minecraft"}}); err != nil {
					t.Fatal(err)
				}
			}
			k.dial = func(string, string, time.Duration) (consoleRCON, error) {
				if mode == "deadline" {
					conn.deadlineErr = errors.New("fake-private-password")
					return conn, nil
				}
				return nil, fmt.Errorf("%s: fake-private-password", mode)
			}
			calls := 0
			k.executor = func(_ *rest.Config, method string, target *url.URL) (remotecommand.Executor, error) {
				calls++
				q := target.Query()
				if method != "POST" || target.Path != "/api/v1/namespaces/minecraft/pods/survival-0/attach" ||
					q.Get("container") != "minecraft" || q.Get("stdin") != "true" || q.Get("stdout") == "true" || q.Get("stderr") == "true" || q.Get("tty") == "true" || strings.Contains(target.String(), "say") {
					t.Errorf("unsafe attach request: %s %s", method, target.String())
				}
				return consoleExecutorFunc(func(_ context.Context, options remotecommand.StreamOptions) error {
					if options.Tty || options.Stdout != nil || options.Stderr != nil {
						t.Error("attach must be stdin-only without TTY")
					}
					input, err := io.ReadAll(options.Stdin)
					if err != nil || string(input) != "say 你好\n" {
						t.Errorf("stdin=%q err=%v", input, err)
					}
					return nil
				}), nil
			}
			got, err := k.SubmitCommand(context.Background(), "survival", "/say 你好")
			if err != nil || got.Name != "survival" || got.Output != "" || got.Transport != "stdin" || calls != 1 || conn.executed.Load() != 0 {
				t.Fatalf("fallback: result=%+v err=%v attach=%d execute=%d", got, err, calls, conn.executed.Load())
			}
		})
	}
}

func TestConsoleSubmitRequiresWritableRunningContainer(t *testing.T) {
	for _, mode := range []string{"missing", "wrong name", "wrong label", "waiting", "only init running", "deleting", "stdin disabled", "stdin once", "TTY"} {
		t.Run(mode, func(t *testing.T) {
			pod := consoleTestPod("survival")
			pods := []corev1.Pod{pod}
			switch mode {
			case "missing":
				pods = nil
			case "wrong name":
				pods[0].Name = "other-0"
			case "wrong label":
				pods[0].Labels[v1alpha1.LabelServer] = "other"
			case "waiting":
				pods[0].Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
			case "only init running":
				pods[0].Status.InitContainerStatuses = pods[0].Status.ContainerStatuses
				pods[0].Status.ContainerStatuses = nil
			case "deleting":
				now := metav1.Now()
				pods[0].DeletionTimestamp = &now
			case "stdin disabled":
				pods[0].Spec.Containers[0].Stdin = false
			case "stdin once":
				pods[0].Spec.Containers[0].StdinOnce = true
			case "TTY":
				pods[0].Spec.Containers[0].TTY = true
			}
			k, _ := newConsoleTestBackend(t, pods)
			var calls atomic.Int32
			k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) { calls.Add(1); return nil, nil }
			_, err := k.SubmitCommand(context.Background(), "survival", "list")
			if !errors.Is(err, ErrConsoleUnavailable) || calls.Load() != 0 {
				t.Fatalf("invalid Pod accepted: err=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestConsoleSubmitRequiresRunningIntentAndActivePhase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		desired v1alpha1.DesiredState
		phase   v1alpha1.Phase
		allowed bool
	}{
		{"starting", v1alpha1.DesiredRunning, v1alpha1.PhaseStarting, true},
		{"running", v1alpha1.DesiredRunning, v1alpha1.PhaseRunning, true},
		{"stop requested", v1alpha1.DesiredStopped, v1alpha1.PhaseRunning, false},
		{"stopped", v1alpha1.DesiredRunning, v1alpha1.PhaseStopped, false},
		{"stopping", v1alpha1.DesiredRunning, v1alpha1.PhaseStopping, false},
		{"failed", v1alpha1.DesiredRunning, v1alpha1.PhaseFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, ms := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival")})
			if err := k.c.Get(context.Background(), client.ObjectKeyFromObject(ms), ms); err != nil {
				t.Fatal(err)
			}
			ms.Spec.DesiredState = tc.desired
			if err := k.c.Update(context.Background(), ms); err != nil {
				t.Fatal(err)
			}
			ms.Status.Phase = tc.phase
			if err := k.c.Status().Update(context.Background(), ms); err != nil {
				t.Fatal(err)
			}
			calls := 0
			k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) {
				calls++
				return consoleExecutorFunc(func(_ context.Context, o remotecommand.StreamOptions) error {
					_, err := io.ReadAll(o.Stdin)
					return err
				}), nil
			}
			got, err := k.SubmitCommand(context.Background(), "survival", "list")
			if tc.allowed {
				if err != nil || got.Transport != "stdin" || calls != 1 {
					t.Fatalf("active input: result=%+v err=%v calls=%d", got, err, calls)
				}
			} else if !errors.Is(err, ErrConsoleNotRunning) || calls != 0 {
				t.Fatalf("inactive input: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestConsoleSubmitRechecksLifecycleAfterHandshake(t *testing.T) {
	for _, change := range []string{"stop requested", "replacement"} {
		for _, connected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s connected=%v", change, connected), func(t *testing.T) {
				k, ms := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival")})
				enableConsoleTestRCON(t, k, ms)
				conn := &fakeConsoleRCON{}
				k.dial = func(string, string, time.Duration) (consoleRCON, error) {
					if err := k.c.Get(context.Background(), client.ObjectKeyFromObject(ms), ms); err != nil {
						t.Fatal(err)
					}
					if change == "replacement" {
						if err := k.c.Delete(context.Background(), ms); err != nil {
							t.Fatal(err)
						}
						ms.UID, ms.ResourceVersion = "replacement-server", ""
						if err := k.c.Create(context.Background(), ms); err != nil {
							t.Fatal(err)
						}
					} else {
						ms.Spec.DesiredState = v1alpha1.DesiredStopped
						if err := k.c.Update(context.Background(), ms); err != nil {
							t.Fatal(err)
						}
					}
					if connected {
						return conn, nil
					}
					return nil, errors.New("handshake unavailable")
				}
				calls := 0
				k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) { calls++; return nil, nil }
				_, err := k.SubmitCommand(context.Background(), "survival", "list")
				if !errors.Is(err, ErrConsoleNotRunning) || calls != 0 || conn.executed.Load() != 0 {
					t.Fatalf("stale server received input: err=%v attaches=%d executions=%d", err, calls, conn.executed.Load())
				}
				if connected && conn.closed.Load() != 1 {
					t.Fatal("pre-send RCON connection was not closed")
				}
			})
		}
	}
}

func TestConsoleSubmitCancellationDuringHandshakeDoesNotAttach(t *testing.T) {
	k, ms := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival")})
	enableConsoleTestRCON(t, k, ms)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &fakeConsoleRCON{}
	k.dial = func(string, string, time.Duration) (consoleRCON, error) { cancel(); return conn, nil }
	attaches := 0
	k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) { attaches++; return nil, nil }
	_, err := k.SubmitCommand(ctx, "survival", "list")
	if !errors.Is(err, ErrConsoleUnavailable) || attaches != 0 || conn.executed.Load() != 0 || conn.closed.Load() != 1 {
		t.Fatalf("canceled handshake sent input: err=%v attaches=%d executions=%d closed=%d", err, attaches, conn.executed.Load(), conn.closed.Load())
	}
}

func TestConsoleAttachFailureDoesNotRetry(t *testing.T) {
	for _, mode := range []string{"setup", "before reading", "partial", "partial success", "after reading", "timeout", "false success"} {
		t.Run(mode, func(t *testing.T) {
			k, _ := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival")})
			if mode == "timeout" {
				k.submitTimeout = time.Second
			}
			calls := 0
			k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) {
				calls++
				if mode == "setup" {
					return nil, errors.New("fake-private-password invalid transport")
				}
				return consoleExecutorFunc(func(ctx context.Context, o remotecommand.StreamOptions) error {
					switch mode {
					case "partial", "partial success":
						b := make([]byte, 2)
						_, _ = o.Stdin.Read(b)
					case "after reading", "timeout":
						_, _ = io.ReadAll(o.Stdin)
					}
					if mode == "false success" || mode == "partial success" {
						return nil
					}
					if mode == "timeout" {
						<-ctx.Done()
						return ctx.Err()
					}
					return errors.New("fake-private-password broken stream")
				}), nil
			}
			_, err := k.SubmitCommand(context.Background(), "survival", "list")
			want := ErrConsoleOutcomeUnknown
			if mode == "setup" || mode == "before reading" || mode == "false success" {
				want = ErrConsoleUnavailable
			}
			if !errors.Is(err, want) || calls != 1 || strings.Contains(err.Error(), "fake-private-password") {
				t.Fatalf("failure resent or exposed data: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestConsoleSubmitCancellationAndLockCleanup(t *testing.T) {
	k, _ := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival")})
	var attaches atomic.Int32
	k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) {
		attaches.Add(1)
		return nil, errors.New("unexpected attach")
	}
	release, err := k.acquireSubmit(context.Background(), "survival")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = k.SubmitCommand(ctx, "survival", "list")
	if !errors.Is(err, ErrConsoleUnavailable) || attaches.Load() != 0 {
		t.Fatalf("waiting command escaped its lock: err=%v attaches=%d", err, attaches.Load())
	}
	other, err := k.acquireSubmit(context.Background(), "creative")
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	k.mu.Lock()
	remaining := len(k.slots)
	k.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("canceled waiter leaked lock entries: %d", remaining)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, err = k.SubmitCommand(ctx, "survival", "list")
	if !errors.Is(err, ErrConsoleUnavailable) || attaches.Load() != 0 {
		t.Fatalf("canceled request sent a command: err=%v attaches=%d", err, attaches.Load())
	}
}

func TestConsoleSubmitReadsStateAfterLock(t *testing.T) {
	k, ms := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival")})
	release, err := k.acquireSubmit(context.Background(), "survival")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := k.SubmitCommand(context.Background(), "survival", "list"); result <- err }()
	if err := k.c.Get(context.Background(), client.ObjectKeyFromObject(ms), ms); err != nil {
		t.Fatal(err)
	}
	ms.Spec.DesiredState = v1alpha1.DesiredStopped
	if err := k.c.Update(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-result:
		if !errors.Is(err, ErrConsoleNotRunning) {
			t.Fatalf("queued command used stale state: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("state check did not finish")
	}
}

func TestConsoleSubmitSerializesCompleteLines(t *testing.T) {
	k, ms := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival"), consoleTestPod("creative")})
	other := ms.DeepCopy()
	other.Name, other.UID, other.ResourceVersion = "creative", "creative-server", ""
	if err := k.c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, 3)
	finishFirst := make(chan struct{})
	var active atomic.Int32
	var overlap atomic.Bool
	k.executor = func(_ *rest.Config, _ string, target *url.URL) (remotecommand.Executor, error) {
		name := strings.Contains(target.Path, "/survival-0/")
		return consoleExecutorFunc(func(ctx context.Context, o remotecommand.StreamOptions) error {
			if name {
				if active.Add(1) != 1 {
					overlap.Store(true)
				}
				defer active.Add(-1)
			}
			line, err := io.ReadAll(o.Stdin)
			if err != nil {
				return err
			}
			entered <- string(line)
			if string(line) == "say first\n" {
				select {
				case <-finishFirst:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}), nil
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := k.SubmitCommand(context.Background(), "survival", "say first"); first <- err }()
	select {
	case line := <-entered:
		if line != "say first\n" {
			t.Fatalf("first line=%q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first command did not start")
	}
	go func() { _, err := k.SubmitCommand(context.Background(), "survival", "say second"); second <- err }()
	if _, err := k.SubmitCommand(context.Background(), "creative", "say other"); err != nil {
		t.Fatal(err)
	}
	if line := <-entered; line != "say other\n" {
		t.Fatalf("same-server commands interleaved before the first finished: %q", line)
	}
	close(finishFirst)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if line := <-entered; line != "say second\n" || overlap.Load() {
		t.Fatalf("serial lines=%q overlap=%v", line, overlap.Load())
	}
}

func TestConsoleRunCommandAndLiveBackupNeverUseStdin(t *testing.T) {
	k, _ := newConsoleTestBackend(t, []corev1.Pod{consoleTestPod("survival")})
	var attached atomic.Int32
	k.executor = func(*rest.Config, string, *url.URL) (remotecommand.Executor, error) { attached.Add(1); return nil, nil }
	if _, err := k.RunCommand(context.Background(), "survival", "save-all flush"); !errors.Is(err, ErrConsoleUnavailable) {
		t.Fatalf("reply-dependent call accepted stdin: %v", err)
	}
	f := newLiveFixture()
	f.a.Console = k
	if err := f.a.startLiveBackup(context.Background(), "survival", "owner1", ""); !errors.Is(err, errLiveBackupConsole) || len(f.backup.live) != 0 || attached.Load() != 0 {
		t.Fatalf("backup used a submission as save completion: err=%v copied=%v attaches=%d", err, f.backup.live, attached.Load())
	}
}
