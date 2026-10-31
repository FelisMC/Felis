package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeRestoreRunner records the steps and fakes what they leave behind: fetch-db writes a
// bundle holding the given server list.
type fakeRestoreRunner struct {
	t        *testing.T
	dir      string
	servers  string
	count    int
	calls    []string
	applied  []byte
	fail     map[string]error
	restores map[string]string
}

func (f *fakeRestoreRunner) felis(_ context.Context, args ...string) error {
	call := "felis " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	for prefix, err := range f.fail {
		if strings.HasPrefix(call, prefix) {
			return err
		}
	}
	if len(args) >= 2 && args[0] == "offsite" && args[1] == "fetch-db" {
		name := "felis-db-20261010T000000Z-hourly.tar"
		if last := args[len(args)-1]; last != "latest" {
			name = last
		}
		writeBundle(f.t, filepath.Join(f.dir, name), f.servers)
	}
	return nil
}

func (f *fakeRestoreRunner) kubectl(_ context.Context, stdin []byte, args ...string) error {
	f.calls = append(f.calls, "kubectl "+strings.Join(args, " "))
	if stdin != nil {
		f.applied = stdin
	}
	return nil
}

func (f *fakeRestoreRunner) serverCount(context.Context) (int, error) { return f.count, nil }

func (f *fakeRestoreRunner) restoreWorld(_ context.Context, server string) (string, error) {
	f.calls = append(f.calls, "restore "+server)
	if err := f.fail["restore "+server]; err != nil {
		return "", err
	}
	return f.restores[server], nil
}

func writeBundle(t *testing.T, path, servers string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, body := range map[string]string{"db.dump": "dump", "k8s/minecraftservers.json": servers} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

const rebuildServers = `{"apiVersion":"v1","kind":"List","items":[
 {"apiVersion":"felis.lolicon.best/v1alpha1","kind":"MinecraftServer","metadata":{"name":"lobby","namespace":"minecraft","labels":{"felis.lolicon.best/system-role":"lobby"}},"spec":{"desiredState":"Running"}},
 {"apiVersion":"felis.lolicon.best/v1alpha1","kind":"MinecraftServer","metadata":{"name":"survival","namespace":"minecraft","annotations":{"felis.lolicon.best/maintenance":"migration@2026-10-01T00:00:00Z","felis.lolicon.best/migration":"{}","felis.lolicon.best/live-backup":"{}","felis.lolicon.best/owner-note":"kept"}},"spec":{"desiredState":"Running","nodeName":"worker-b","subdomain":"survival","storage":{"size":"16Gi","claimName":"world-survival-m1234"}}},
 {"apiVersion":"felis.lolicon.best/v1alpha1","kind":"MinecraftServer","metadata":{"name":"creative","namespace":"minecraft"},"spec":{"desiredState":"Stopped"}}
]}`

func TestRestorableServersCleansTheOldCluster(t *testing.T) {
	out, names, err := restorableServers([]byte(rebuildServers))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "survival,creative" {
		t.Fatalf("names = %v", names)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec map[string]any `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatal(err)
	}
	s := list.Items[0]
	if s.Metadata.Name != "survival" || s.Spec["desiredState"] != "Stopped" || s.Spec["nodeName"] != nil || s.Spec["subdomain"] != "survival" {
		t.Errorf("survival spec = %v", s.Spec)
	}
	if storage := s.Spec["storage"].(map[string]any); storage["claimName"] != nil || storage["size"] != "16Gi" {
		t.Errorf("survival storage = %v", storage)
	}
	if len(s.Metadata.Annotations) != 1 || s.Metadata.Annotations["felis.lolicon.best/owner-note"] != "kept" {
		t.Errorf("survival annotations = %v", s.Metadata.Annotations)
	}
}

func TestRestoreHostRunsTheRunbookOnce(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRestoreRunner{t: t, dir: dir, servers: rebuildServers, restores: map[string]string{"survival": "bk-9"}}
	st, _ := loadRestoreState(dir)
	if err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir, takeOver: true}, st, io.Discard); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "felis-db-20261010T000000Z-hourly.tar")
	want := []string{
		"felis offsite fetch-db -dir " + dir + " latest",
		"felis offsite fetch-images",
		"felis offsite fetch-uploads -adopt-volume",
		"kubectl -n felis scale deployment felis-api --replicas=0",
		"kubectl -n felis scale deployment felis-operator --replicas=0",
		"felis db restore -yes -no-safety-backup " + bundle,
		"felis migrate up -config /etc/felis/felis.host.toml",
		"kubectl -n felis scale deployment felis-api --replicas=1",
		"kubectl -n felis scale deployment felis-operator --replicas=1",
		"kubectl -n felis rollout status deployment/felis-api --timeout=300s",
		"kubectl apply -f -",
		"felis converge",
		"felis offsite fetch-worlds",
		"restore survival",
		"restore creative",
		"felis offsite take-over -yes",
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps:\n%s\nwant:\n%s", strings.Join(r.calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(string(r.applied), `"survival"`) || strings.Contains(string(r.applied), `"lobby"`) {
		t.Errorf("applied = %s", r.applied)
	}
	saved, err := loadRestoreState(dir)
	if err != nil || saved.Bundle != filepath.Base(bundle) || saved.Restored["survival"] != "bk-9" || saved.Restored["creative"] != "none" || !saved.Done[stepTakeOver] {
		t.Fatalf("saved state = %+v (%v)", saved, err)
	}

	r.calls = nil
	r.count = 2 // the restored install now holds servers: the guard must not stop the rerun
	if err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir, takeOver: true}, saved, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("a finished rebuild ran again: %v", r.calls)
	}
}

func TestRestoreHostResumesAfterAFailure(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRestoreRunner{t: t, dir: dir, servers: rebuildServers, fail: map[string]error{"felis offsite fetch-uploads -adopt-volume": errors.New("bucket timeout")}}
	st, _ := loadRestoreState(dir)
	if err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir}, st, io.Discard); err == nil || !strings.Contains(err.Error(), "bucket timeout") {
		t.Fatalf("a failed step = %v", err)
	}
	saved, _ := loadRestoreState(dir)
	if !saved.Done[stepFetchDB] || !saved.Done[stepImages] || saved.Done[stepUploads] {
		t.Fatalf("recorded = %v", saved.Done)
	}
	r.calls, r.fail = nil, map[string]error{"restore creative": errors.New("job failed")}
	if err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir}, saved, io.Discard); err == nil {
		t.Fatal("a server that could not be restored passed silently")
	}
	if r.calls[0] != "felis offsite fetch-uploads -adopt-volume" {
		t.Errorf("the rerun started at %q", r.calls[0])
	}
	saved, _ = loadRestoreState(dir)
	if saved.Restored["survival"] != "none" || saved.Failed["creative"] == "" || saved.Done[stepRestoreWorlds] {
		t.Fatalf("after one failed restore = %+v", saved)
	}
	r.calls, r.fail = nil, nil
	if err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir}, saved, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.calls, ",") != "restore creative" {
		t.Errorf("the retry ran %v, want only creative", r.calls)
	}
	if saved.Failed["creative"] != "" || saved.Done[stepTakeOver] {
		t.Errorf("after the retry = %+v", saved)
	}
}

func TestRestoreHostTakesTheNamedBundle(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRestoreRunner{t: t, dir: dir, servers: rebuildServers, fail: map[string]error{"felis offsite fetch-images": errors.New("bucket timeout")}}
	st, _ := loadRestoreState(dir)
	if err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir}, st, io.Discard); err == nil {
		t.Fatal("the failing step passed")
	}
	older := "felis-db-20261001T000000Z-daily.tar"
	r.calls, r.fail = nil, nil
	if err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir, bundle: older}, st, io.Discard); err != nil {
		t.Fatal(err)
	}
	if r.calls[0] != "felis offsite fetch-db -dir "+dir+" "+older || st.Bundle != older {
		t.Errorf("the rerun fetched %q and used %q, want %s", r.calls[0], st.Bundle, older)
	}
	if !slices.Contains(r.calls, "felis db restore -yes -no-safety-backup "+filepath.Join(dir, older)) {
		t.Errorf("the database was not restored from %s: %v", older, r.calls)
	}
	r.calls = nil
	err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir, bundle: "felis-db-20261005T000000Z-daily.tar"}, st, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already restored from "+older) || len(r.calls) != 0 {
		t.Errorf("another bundle after the restore = %v (ran %v)", err, r.calls)
	}
}

func TestRestoreHostRefusesAnInstallWithServers(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRestoreRunner{t: t, dir: dir, servers: rebuildServers, count: 3}
	st, _ := loadRestoreState(dir)
	if err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir}, st, io.Discard); err == nil || !strings.Contains(err.Error(), "already holds 3 servers") {
		t.Fatalf("an install with servers = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("it ran %v before refusing", r.calls)
	}
	if err := runRestoreHost(context.Background(), r, restoreOptions{dir: dir, force: true}, st, io.Discard); err != nil {
		t.Fatalf("-force = %v", err)
	}
}

func TestNewestBundleAndBundleEntry(t *testing.T) {
	dir := t.TempDir()
	if _, err := newestBundle(dir); err == nil {
		t.Error("an empty directory named a bundle")
	}
	for _, n := range []string{"felis-db-20261001T000000Z-hourly.tar", "felis-db-20261009T120000Z-hourly.tar", "felis-db-20261005T000000Z-daily.tar"} {
		writeBundle(t, filepath.Join(dir, n), "{}")
	}
	if got, err := newestBundle(dir); err != nil || got != "felis-db-20261009T120000Z-hourly.tar" {
		t.Errorf("newest = %q (%v)", got, err)
	}
	got, err := bundleEntry(filepath.Join(dir, "felis-db-20261001T000000Z-hourly.tar"), "k8s/minecraftservers.json")
	if err != nil || string(got) != "{}" {
		t.Errorf("entry = %q (%v)", got, err)
	}
	if _, err := bundleEntry(filepath.Join(dir, "felis-db-20261001T000000Z-hourly.tar"), "k8s/nothing.json"); err == nil {
		t.Error("a missing entry was found")
	}
}
