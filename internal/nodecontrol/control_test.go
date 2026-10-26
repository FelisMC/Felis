package nodecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validRequest() Request {
	return Request{Action: "join", Name: "worker-01", SSHTarget: "root@192.0.2.10", ExternalIP: "192.0.2.10", ConfirmMaintenance: true}
}
func TestRequestRejectsUnsafeInputs(t *testing.T) {
	for _, change := range []func(*Request){func(r *Request) { r.ConfirmMaintenance = false }, func(r *Request) { r.SSHTarget = "-oProxyCommand=sh" }, func(r *Request) { r.SSHTarget = "root@host;id" }, func(r *Request) { r.Name = "../../escape" }, func(r *Request) { r.ExternalIP = "127.0.0.1" }, func(r *Request) { r.Peers = []string{"10.0.0.0/8"} }, func(r *Request) { r.Action = "exec" }} {
		r := validRequest()
		change(&r)
		if r.Validate() == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
	if err := validRequest().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeRequest(t *testing.T) {
	r := Request{Action: "upgrade", Name: "worker-b", SSHTarget: "root@worker-b", ConfirmMaintenance: true}
	if err := r.Validate(); err != nil {
		t.Fatalf("a worker update was refused: %v", err)
	}
	for _, change := range []func(*Request){func(r *Request) { r.SSHTarget = "-oProxyCommand=sh" }, func(r *Request) { r.Name = "" }, func(r *Request) { r.ConfirmMaintenance = false }} {
		bad := r
		change(&bad)
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestFenceRequest(t *testing.T) {
	for _, action := range []string{"fence", "unfence"} {
		r := Request{Action: action, Name: "worker-b", ConfirmMaintenance: true}
		if err := r.Validate(); err != nil {
			t.Fatalf("%s was refused: %v", action, err)
		}
		for _, change := range []func(*Request){func(r *Request) { r.Name = "" }, func(r *Request) { r.Name = "-b" }, func(r *Request) { r.ConfirmMaintenance = false }} {
			bad := r
			change(&bad)
			if bad.Validate() == nil {
				t.Fatalf("accepted %+v", bad)
			}
		}
	}
}

func TestNodeNames(t *testing.T) {
	for _, name := range []string{"worker-01", "worker.localdomain"} {
		r := validRequest()
		r.Name = name
		if err := r.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"worker..localdomain", "worker.-domain", "worker.", strings.Repeat("a", 64)} {
		r := validRequest()
		r.Name = name
		if r.Validate() == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestPersistentSerializedTasksAndLogs(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	finish := make(chan struct{})
	manager, err := Open(context.Background(), dir, func(ctx context.Context, r Request, stage func(string) error, out io.Writer) error {
		if err := stage("approval"); err != nil {
			return err
		}
		io.WriteString(out, "probe failed\n")
		close(entered)
		<-finish
		return errors.New("quarantine retained")
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := manager.Start(validRequest(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err = manager.Start(validRequest(), "owner"); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent task", err)
	}
	live, err := manager.Get(task.ID)
	if err != nil || live.Stage != "approval" || !strings.Contains(live.Log, "probe failed") {
		t.Fatal(live, err)
	}
	close(finish)
	manager.Wait()
	reopened, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(task.ID)
	if err != nil || got.State != "failed" || got.Error != "quarantine retained" || got.FinishedAt == nil || got.Actor != "owner" {
		t.Fatal(got, err)
	}
	if reopened.List()[0].Log != "" {
		t.Fatal("task list leaked logs")
	}
}
func TestInterruptedTaskIsNotReportedRunning(t *testing.T) {
	dir := t.TempDir()
	task := Task{ID: strings.Repeat("a", 32), Request: validRequest(), State: "running", StartedAt: time.Now()}
	raw, _ := json.Marshal(task)
	if err := os.WriteFile(filepath.Join(dir, task.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := manager.Get(task.ID)
	if err != nil || got.State != "failed" || got.FinishedAt == nil || !strings.Contains(got.Error, "restarted") {
		t.Fatal(got, err)
	}
}
func TestLocalClientAndBoundedOutput(t *testing.T) {
	dir := t.TempDir()
	manager, err := Open(context.Background(), dir, func(ctx context.Context, r Request, stage func(string) error, out io.Writer) error {
		for range 90 {
			if _, err := io.WriteString(out, strings.Repeat("x", MaxLog)); err != nil {
				return err
			}
		}
		io.WriteString(out, "last output")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Avoid macOS' short Unix socket path limit under t.TempDir.
	sock, err := os.CreateTemp("/tmp", "felis-node-test-")
	if err != nil {
		t.Fatal(err)
	}
	path := sock.Name()
	sock.Close()
	os.Remove(path)
	defer os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: manager.Handler()}
	go server.Serve(listener)
	defer server.Close()
	client := NewClient(path)
	task, err := client.Start(context.Background(), validRequest(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	got, err := client.Get(context.Background(), task.ID)
	if err != nil || got.State != "succeeded" || len(got.Log) > MaxLog || !strings.HasSuffix(got.Log, "last output") {
		t.Fatal("task/log", got.State, len(got.Log), err)
	}
	stat, err := os.Stat(filepath.Join(dir, task.ID+".log"))
	if err != nil || stat.Size() > 4<<20 {
		t.Fatal("unbounded file", stat, err)
	}
	if _, err := client.Get(context.Background(), "../secret"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
func TestCancellationFinishesTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	manager, err := Open(ctx, t.TempDir(), func(ctx context.Context, r Request, stage func(string) error, out io.Writer) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := manager.Start(validRequest(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	cancel()
	manager.Wait()
	got, err := manager.Get(task.ID)
	if err != nil || got.State != "failed" {
		t.Fatal(got, err)
	}
}

func TestHARequests(t *testing.T) {
	if err := (Request{Action: "enable-ha", ConfirmMaintenance: true}).Validate(); err != nil {
		t.Fatalf("enable-ha = %v", err)
	}
	if (Request{Action: "enable-ha"}).Validate() == nil {
		t.Fatal("enable-ha without the confirmation")
	}
	r := Request{Action: "join-controller", Name: "ctl-b", SSHTarget: "root@ctl-b", ExternalIP: "203.0.113.11", ConfirmMaintenance: true}
	if err := r.Validate(); err != nil {
		t.Fatalf("join-controller = %v", err)
	}
	for _, change := range []func(*Request){func(r *Request) { r.ExternalIP = "" }, func(r *Request) { r.SSHTarget = "-oProxyCommand=sh" }, func(r *Request) { r.Name = "" }} {
		bad := r
		change(&bad)
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
