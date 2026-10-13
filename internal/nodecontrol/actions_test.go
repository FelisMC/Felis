package nodecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHostTaskRequests pins what each host task takes: its parameters, its secrets, and
// the confirmation for the ones that disconnect players, restart the panel or replace data.
func TestHostTaskRequests(t *testing.T) {
	bundle := "felis-db-20261001T033000Z-daily.tar"
	accepted := []Request{
		{Action: "db-backup"},
		{Action: "db-verify", Params: map[string]string{"bundle": bundle}},
		{Action: "db-restore", Params: map[string]string{"bundle": bundle}, ConfirmMaintenance: true},
		{Action: "offsite-sync"},
		{Action: "offsite-configure", Params: map[string]string{"endpoint": "https://s3.example.com", "bucket": "felis-backup", "region": "auto", "prefix": "host-a/", "dbKeep": "30"},
			Secrets: map[string]string{"accessKey": "AKIA0", "secretKey": "s3cr/t+=", "key": "k3y="}},
		{Action: "offsite-take-over", ConfirmMaintenance: true},
		{Action: "restore-host", Params: map[string]string{"bundle": bundle, "takeOver": "true", "force": "false"}, ConfirmMaintenance: true},
		{Action: "backup-now"},
		{Action: "backup-now", Params: map[string]string{"stop": "true", "servers": "survival,creative"}, ConfirmMaintenance: true},
		{Action: "rotate-token", Params: map[string]string{"kind": "forwarding"}, ConfirmMaintenance: true},
		{Action: "domain-set", Params: map[string]string{"domain": "play.example.com"}, ConfirmMaintenance: true},
		{Action: "storage-configure", Params: map[string]string{"mode": "local"}, ConfirmMaintenance: true},
		{Action: "storage-configure", Params: map[string]string{"mode": "s3", "endpoint": "https://s3.example.com", "bucket": "uploads"}, Secrets: map[string]string{"accessKey": "a", "secretKey": "b"}, ConfirmMaintenance: true},
		{Action: "connection-configure", Params: map[string]string{"mode": "reverse-proxy", "adminHost": "op.console.example.com"}, ConfirmMaintenance: true},
		{Action: "connection-configure", Params: map[string]string{"mode": "cloudflare", "adminHost": "op.console.example.com", "identity": "@example.com", "accountID": strings.Repeat("a", 32)},
			Secrets: map[string]string{"apiToken": "token"}, ConfirmMaintenance: true},
		{Action: "converge", Params: map[string]string{"userRcon": "false"}},
		{Action: "doctor"},
		{Action: "support-bundle", Params: map[string]string{"serverLogs": "true"}},
		{Action: "host-settings", Params: map[string]string{"dbBackupTime": "04:15", "dbBackupKeep": "21"}},
		{Action: "host-settings", Params: map[string]string{"velocityXmx": "768M"}, ConfirmMaintenance: true},
		{Action: "server-forwarding", Params: map[string]string{"server": "legacy18", "legacy": "false"}},
		{Action: "server-forwarding", Params: map[string]string{"server": "legacy18", "legacy": "true"}, ConfirmMaintenance: true},
		{Action: "enable-ha", Params: map[string]string{"vip": "192.0.2.50", "vipInterface": "eth0", "vrid": "51"}, ConfirmMaintenance: true},
	}
	for _, r := range accepted {
		if err := r.Validate(); err != nil {
			t.Errorf("%s %v was refused: %v", r.Action, r.Params, err)
		}
	}
	refused := []Request{
		{Action: "db-verify", Params: map[string]string{"bundle": "../../etc/shadow"}},
		{Action: "db-restore", Params: map[string]string{"bundle": bundle}},
		{Action: "db-backup", Params: map[string]string{"label": "daily"}},
		{Action: "doctor", Secrets: map[string]string{"apiToken": "x"}},
		{Action: "offsite-configure", Params: map[string]string{"endpoint": "s3.example.com", "bucket": "b-b"}},
		{Action: "offsite-configure", Params: map[string]string{"endpoint": "https://s3", "bucket": "B"}},
		{Action: "offsite-configure", Params: map[string]string{"endpoint": "https://s3", "bucket": "bucket", "dbKeep": "0"}},
		{Action: "offsite-configure", Params: map[string]string{"endpoint": "https://s3", "bucket": "bucket"}, Secrets: map[string]string{"accessKey": "it's"}},
		{Action: "offsite-take-over"},
		{Action: "restore-host", Params: map[string]string{"force": "yes"}, ConfirmMaintenance: true},
		{Action: "backup-now", Params: map[string]string{"stop": "true"}},
		{Action: "backup-now", Params: map[string]string{"servers": "a,../b"}},
		{Action: "rotate-token", Params: map[string]string{"kind": "everything"}, ConfirmMaintenance: true},
		{Action: "domain-set", Params: map[string]string{"domain": "-x.com"}, ConfirmMaintenance: true},
		{Action: "storage-configure", Params: map[string]string{"mode": "s3", "endpoint": "https://s3", "bucket": "uploads"}, ConfirmMaintenance: true},
		{Action: "storage-configure", Params: map[string]string{"mode": "local"}},
		{Action: "connection-configure", Params: map[string]string{"mode": "cloudflare", "adminHost": "op.example.com", "identity": "owner@example.com", "accountID": strings.Repeat("a", 32)}, ConfirmMaintenance: true},
		{Action: "connection-configure", Params: map[string]string{"mode": "local", "identity": "@example.com"}, ConfirmMaintenance: true},
		{Action: "connection-configure", Params: map[string]string{"mode": "reverse-proxy"}, ConfirmMaintenance: true},
		{Action: "converge", Params: map[string]string{"userRcon": "true"}},
		{Action: "host-settings"},
		{Action: "host-settings", Params: map[string]string{"dbBackupTime": "25:00"}},
		{Action: "host-settings", Params: map[string]string{"velocityXmx": "128M"}, ConfirmMaintenance: true},
		{Action: "host-settings", Params: map[string]string{"velocityXmx": "1G"}},
		{Action: "server-forwarding", Params: map[string]string{"server": "legacy18", "legacy": "true"}},
		{Action: "enable-ha", Params: map[string]string{"vipInterface": "eth0"}, ConfirmMaintenance: true},
		{Action: "enable-ha", Params: map[string]string{"vip": "2001:db8::1"}, ConfirmMaintenance: true},
		{Action: "join", Name: "worker-01", SSHTarget: "root@192.0.2.10", ExternalIP: "192.0.2.10", Params: map[string]string{"vip": "192.0.2.50"}, ConfirmMaintenance: true},
		{Action: "shell", ConfirmMaintenance: true},
	}
	for _, r := range refused {
		if r.Validate() == nil {
			t.Errorf("accepted %s %v %v", r.Action, r.Params, r.Secrets)
		}
	}
}

// TestBlockingAndNodeActions pins which tasks hold the maintenance gate and which audit as
// node lifecycle actions; an action this release does not know holds the gate.
func TestBlockingAndNodeActions(t *testing.T) {
	for action, blocking := range map[string]bool{"approve": true, "db-restore": true, "restore-host": true, "rotate-token": true, "domain-set": true,
		"doctor": false, "db-backup": false, "backup-now": false, "offsite-sync": false, "support-bundle": false, "a-newer-action": true} {
		if Blocking(action) != blocking {
			t.Errorf("Blocking(%s) = %v", action, !blocking)
		}
	}
	if !NodeAction("join") || !NodeAction("enable-ha") || NodeAction("doctor") || NodeAction("db-restore") {
		t.Fatal("NodeAction misclassifies")
	}
	if timeoutFor("restore-host") != 12*time.Hour || timeoutFor("doctor") != 3*time.Minute || timeoutFor("join") != 45*time.Minute {
		t.Fatal("per-action time limits")
	}
	if limitText(12*time.Hour) != "12-hour" || limitText(45*time.Minute) != "45-minute" {
		t.Fatal(limitText(12*time.Hour), limitText(45*time.Minute))
	}
}

// TestSecretsReachOnlyTheExecutor pins that a task's secrets are handed to the executor
// and are neither kept on disk nor reported back.
func TestSecretsReachOnlyTheExecutor(t *testing.T) {
	dir := t.TempDir()
	seen := make(chan map[string]string, 1)
	manager, err := Open(context.Background(), dir, func(ctx context.Context, r Request, stage func(string) error, out io.Writer) error {
		seen <- r.Secrets
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r := Request{Action: "storage-configure", Params: map[string]string{"mode": "s3", "endpoint": "https://s3.example.com", "bucket": "uploads"},
		Secrets: map[string]string{"accessKey": "AKIA-hidden", "secretKey": "very-hidden"}, ConfirmMaintenance: true}
	task, err := manager.Start(r, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got["secretKey"] != "very-hidden" {
		t.Fatalf("the executor got %v", got)
	}
	manager.Wait()
	if task.Request.Secrets != nil {
		t.Fatal("Start reported the secrets")
	}
	got, err := manager.Get(task.ID)
	if err != nil || got.Request.Secrets != nil || manager.List()[0].Request.Secrets != nil {
		t.Fatalf("secrets reported: %+v %v", got.Request, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, task.ID+".json"))
	if err != nil || strings.Contains(string(raw), "hidden") {
		t.Fatalf("secrets stored: %s %v", raw, err)
	}
	if got.Request.Params["bucket"] != "uploads" {
		t.Fatal("parameters were not kept")
	}
}

// socketServer serves m on a Unix socket and returns a client for it.
func socketServer(t *testing.T, m *Manager) *Client {
	t.Helper()
	sock, err := os.CreateTemp("/tmp", "felis-node-test-")
	if err != nil {
		t.Fatal(err)
	}
	path := sock.Name()
	sock.Close()
	os.Remove(path)
	t.Cleanup(func() { os.Remove(path) })
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: m.Handler()}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return NewClient(path)
}

// TestTaskResultAndArtifact pins that a task's result is kept with it and its file is
// served only from an artifact directory, as a regular file, once the task succeeded.
func TestTaskResultAndArtifact(t *testing.T) {
	artifacts := t.TempDir()
	outside := t.TempDir()
	bundle := filepath.Join(artifacts, "felis-support-1.tar.gz")
	if err := os.WriteFile(bundle, []byte("bundle bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(outside, "shadow")
	os.WriteFile(stray, []byte("secret"), 0600)
	link := filepath.Join(artifacts, "link.tar.gz")
	os.Symlink(stray, link)
	next := make(chan string, 4)
	fail := false
	manager, err := Open(context.Background(), t.TempDir(), func(ctx context.Context, r Request, stage func(string) error, out io.Writer) error {
		if err := SetResult(ctx, map[string]int{"critical": 2}); err != nil {
			return err
		}
		if err := SetArtifact(ctx, <-next); err != nil {
			return err
		}
		if fail {
			return errors.New("broke")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.ArtifactDirs = []string{artifacts}
	client := socketServer(t, manager)
	run := func(path string) Task {
		next <- path
		task, err := manager.Start(Request{Action: "support-bundle"}, "owner")
		if err != nil {
			t.Fatal(err)
		}
		manager.Wait()
		got, _ := manager.Get(task.ID)
		return got
	}
	ok := run(bundle)
	if string(ok.Result) != `{"critical":2}` || ok.Artifact != bundle {
		t.Fatalf("result %s artifact %s", ok.Result, ok.Artifact)
	}
	body, size, name, err := client.Artifact(context.Background(), ok.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "bundle bytes" || size != int64(len("bundle bytes")) || name != "felis-support-1.tar.gz" {
		t.Fatalf("download %q %d %q", data, size, name)
	}
	for _, path := range []string{stray, link, artifacts} {
		task := run(path)
		if _, _, _, err := client.Artifact(context.Background(), task.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s was served: %v", path, err)
		}
	}
	fail = true
	failed := run(bundle)
	if _, _, _, err := client.Artifact(context.Background(), failed.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a failed task's file was served: %v", err)
	}
	if err := SetResult(context.Background(), 1); err == nil {
		t.Fatal("SetResult outside a task")
	}
}

// TestHostHooksOverTheSocket pins the overview, the watchdog hold and the off-site key.
func TestHostHooksOverTheSocket(t *testing.T) {
	manager, err := Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := socketServer(t, manager)
	if _, err := client.Host(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no overview hook: %v", err)
	}
	var holds []time.Time
	manager.Host = func(context.Context) (any, error) { return map[string]string{"host": "ctl-a"}, nil }
	manager.Quiet = func(until time.Time) error { holds = append(holds, until); return nil }
	manager.OffsiteKey = func() (string, error) { return "sealing-key", nil }
	raw, err := client.Host(context.Background())
	if err != nil || string(raw) != `{"host":"ctl-a"}`+"\n" && string(raw) != `{"host":"ctl-a"}` {
		t.Fatalf("overview %s %v", raw, err)
	}
	if err := client.Quiet(context.Background(), time.Now().Add(-time.Minute)); err == nil {
		t.Fatal("a hold in the past was taken")
	}
	if err := client.Quiet(context.Background(), time.Now().Add(8*24*time.Hour)); err == nil {
		t.Fatal("a hold past a week was taken")
	}
	if err := client.Quiet(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := client.Quiet(context.Background(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(holds) != 2 || holds[0].IsZero() || !holds[1].IsZero() {
		t.Fatalf("holds %v", holds)
	}
	if key, err := client.OffsiteKey(context.Background()); err != nil || key != "sealing-key" {
		t.Fatalf("key %q %v", key, err)
	}
	rec := httptest.NewRecorder()
	manager.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/host/offsite-key", nil))
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the key may be cached")
	}
	var parsed map[string]string
	if json.Unmarshal(rec.Body.Bytes(), &parsed) != nil || parsed["key"] != "sealing-key" {
		t.Fatal(rec.Body.String())
	}
}
