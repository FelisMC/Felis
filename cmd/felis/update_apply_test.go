package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/updater"
	"felis.lolicon.best/internal/updates"
)

const updateTestSHA = "0123456789abcdef0123456789abcdef01234567"

func TestUpdateRefPlanPinsTheReviewedCommit(t *testing.T) {
	target := updater.InstallTarget{Revision: updateTestSHA}
	for _, opts := range []updateOptions{
		{cfgPath: "/etc/felis/felis.toml"},
		{cfgPath: "/etc/felis/felis.toml", force: true, all: true},
		{cfgPath: "/etc/felis/felis.toml", selected: map[string]bool{"k3s": true}},
	} {
		out := renderInstallPlan(target, opts)
		if !strings.Contains(out, "sudo felis update --apply --ref "+updateTestSHA) || strings.Contains(out, "--dev") {
			t.Fatalf("moving target in apply command: %s", out)
		}
		if !strings.Contains(out, "No update is applied") || !strings.Contains(out, "User server images remain pinned") {
			t.Fatalf("missing scope: %s", out)
		}
		if strings.Contains(out, " --all") != opts.dependencies() || strings.Contains(out, "--ref "+updateTestSHA+" --all --force") != opts.force {
			t.Fatalf("apply command lost options: %s", out)
		}
	}
	opts := updateOptions{cfgPath: "/path/'literal $(id).toml", repoURL: "https://github.com/example/Felis.git", preserveToken: true}
	out := renderInstallPlan(updater.InstallTarget{Release: "v0.2.0", Revision: updateTestSHA, Script: `#!/bin/bash
FELIS_RELEASE="${FELIS_RELEASE:-}"`}, opts)
	for _, want := range []string{"--version v0.2.0 --expect-commit " + updateTestSHA, "--preserve-env=FELIS_GITHUB_TOKEN", "FELIS_REPO_URL=" + shellQuote(opts.repoURL), "--config " + shellQuote(opts.cfgPath)} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	older := renderInstallPlan(updater.InstallTarget{Release: "v0.2.0", Revision: updateTestSHA, Script: "#!/bin/bash\n# FELIS_RELEASE"}, updateOptions{cfgPath: "/etc/felis/felis.toml"})
	if !strings.Contains(older, "--apply --ref "+updateTestSHA) || strings.Contains(older, "--version v0.2.0") {
		t.Fatalf("old installer must offer a working pinned source apply: %s", older)
	}
	if strings.Contains(renderInstallPlan(target, updateOptions{apply: true}), "No update is applied") {
		t.Fatal("apply must not claim it is only a check")
	}
}

func TestUpdateRejectsConflictingOrUnsafeFlagsBeforeDiscovery(t *testing.T) {
	for _, args := range [][]string{
		{"--apply", "--check"}, {"--apply", "--record"}, {"--now"},
		{"--dev", "--version", "v0.2.0"}, {"--dev", "--ref", "main"},
		{"--ref", "main", "--version", "v0.2.0"}, {"--record", "--dev"},
		{"--version", "v0.2.0-rc.1"}, {"--version", "v0.2.0+gabc1234"},
		{"--apply", "--mc"}, {"apply"}, {"--apply", "--expect-commit", "short"}, {"--expect-commit", updateTestSHA},
	} {
		var out, errb strings.Builder
		if got := cmdUpdate(args, &out, &errb); got != 2 || out.Len() != 0 {
			t.Errorf("args %v: code %d, out %q, err %q", args, got, out.String(), errb.String())
		}
	}
	o := updateOptions{release: "0.2.0"}
	if err := o.validate(false, true, false); err != nil || o.release != "v0.2.0" {
		t.Fatalf("normalize version: %v / %q", err, o.release)
	}
}

func TestUpdateApplySafetyAndOrdering(t *testing.T) {
	now := time.Now()
	open := updates.Window{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	future := updates.Window{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)}
	ended := updates.Window{Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)}
	failed := errors.New("failed")
	for _, tc := range []struct {
		name                                        string
		opts                                        updateOptions
		windows                                     []updates.Window
		windowErr, backupErr, installErr, verifyErr error
		want                                        []string
		wantErr                                     bool
	}{
		{name: "active", windows: []updates.Window{open, open}, want: []string{"window", "backup", "window", "install", "verify"}},
		{name: "unset", windows: []updates.Window{{}}, want: []string{"window"}, wantErr: true},
		{name: "future", windows: []updates.Window{future}, want: []string{"window"}, wantErr: true},
		{name: "ended", windows: []updates.Window{ended}, want: []string{"window"}, wantErr: true},
		{name: "force cannot bypass", opts: updateOptions{force: true}, windows: []updates.Window{future}, want: []string{"window"}, wantErr: true},
		{name: "manual maintenance", opts: updateOptions{now: true}, windows: []updates.Window{{}, {}}, want: []string{"window", "backup", "window", "install", "verify"}},
		{name: "manual cannot bypass unreadable window", opts: updateOptions{now: true}, windowErr: failed, want: []string{"window"}, wantErr: true},
		{name: "backup failure", windows: []updates.Window{open}, backupErr: failed, want: []string{"window", "backup"}, wantErr: true},
		{name: "expires during backup", windows: []updates.Window{open, ended}, want: []string{"window", "backup", "window"}, wantErr: true},
		{name: "install failure", windows: []updates.Window{open, open}, installErr: failed, want: []string{"window", "backup", "window", "install"}, wantErr: true},
		{name: "verification failure", windows: []updates.Window{open, open}, verifyErr: failed, want: []string{"window", "backup", "window", "install", "verify"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			reads := 0
			steps := updateApplySteps{
				window: func(context.Context) (updates.Window, error) {
					calls = append(calls, "window")
					if tc.windowErr != nil {
						return updates.Window{}, tc.windowErr
					}
					w := tc.windows[reads]
					reads++
					return w, nil
				},
				backup:  func(context.Context) error { calls = append(calls, "backup"); return tc.backupErr },
				install: func(context.Context) error { calls = append(calls, "install"); return tc.installErr },
				verify:  func(context.Context) error { calls = append(calls, "verify"); return tc.verifyErr },
			}
			err := runUpdateApply(context.Background(), tc.opts, steps)
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("calls %v, err %v; want %v, error %v", calls, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestUpdateInstallerCannotUseStaleSourceOrBypassGuards(t *testing.T) {
	env := []string{"FELIS_REF=stale", "FELIS_REF=duplicate", "FELIS_BOOTSTRAP_BINARY=/old", "FELIS_BOOTSTRAP_FROM_TUI=1", "FELIS_SKIP_FETCH=1", "FELIS_RELEASE=v0.1.0", "FELIS_GAME_STACK=latest", "FELIS_PREFLIGHT=warn", "FELIS_PRE_MIGRATE_BACKUP=0", "FELIS_GITHUB_TOKEN=private-token", "PATH=/usr/bin"}
	for _, release := range []string{"", "v0.2.0"} {
		out := updateInstallerEnv(env, updater.InstallTarget{Release: release, Revision: updateTestSHA}, updateOptions{force: true, all: true}, "https://github.com/FelisMC/Felis.git")
		got := map[string]string{}
		for _, item := range out {
			key, value, _ := strings.Cut(item, "=")
			if _, exists := got[key]; exists {
				t.Fatalf("duplicate environment key %s", key)
			}
			got[key] = value
		}
		for key, want := range map[string]string{"FELIS_BOOTSTRAP_BINARY": "", "FELIS_BOOTSTRAP_FROM_TUI": "", "FELIS_SKIP_FETCH": "", "FELIS_RELEASE": release, "FELIS_GAME_STACK": "pinned", "FELIS_PREFLIGHT": "strict", "FELIS_PRE_MIGRATE_BACKUP": "1", "FELIS_FORCE_UPDATE": "1", "FELIS_UPGRADE_DEPS": "1", "FELIS_GITHUB_TOKEN": "private-token"} {
			if got[key] != want {
				t.Errorf("%s = %q; want %q", key, got[key], want)
			}
		}
		wantRef := updateTestSHA
		if release != "" {
			wantRef = ""
		}
		if got["FELIS_REF"] != wantRef {
			t.Fatalf("wrong source: %v", got)
		}
	}
}

func TestUpdateExecutesInstallerAndPropagatesFailure(t *testing.T) {
	var stdout, stderr strings.Builder
	err := runUpdateInstaller(context.Background(), "#!/bin/bash\nprintf 'target:%s' \"$FELIS_REF\"\nprintf 'diagnostic' >&2\nexit 7\n", append(os.Environ(), "FELIS_REF="+updateTestSHA), &stdout, &stderr)
	if err == nil || stdout.String() != "target:"+updateTestSHA || stderr.String() != "diagnostic" {
		t.Fatalf("out %q, stderr %q, err %v", stdout.String(), stderr.String(), err)
	}
}

func TestUpdateCancellationStopsInstallerChildren(t *testing.T) {
	dir := t.TempDir()
	ready, stopped := filepath.Join(dir, "ready"), filepath.Join(dir, "child-stopped")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	script := `#!/bin/bash
trap 'exit 0' TERM
(
  trap 'echo stopped > "$STOPPED"; exit 0' TERM
  echo ready > "$READY"
  sleep 30
) &
wait
`
	go func() {
		done <- runUpdateInstaller(ctx, script, append(os.Environ(), "READY="+ready, "STOPPED="+stopped), io.Discard, io.Discard)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("installer did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted installation must not report success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("installer children kept running after cancellation")
	}
	if _, err := os.Stat(stopped); err != nil {
		t.Fatalf("installer child did not receive cancellation: %v", err)
	}
}

func TestInstalledUpdateMustMatchExactTarget(t *testing.T) {
	for _, tc := range []struct {
		version, release string
		want             bool
	}{
		{"v0.2.0", "v0.2.0", true}, {"v0.2.1", "v0.2.0", false},
		{"v0.0.0+g0123456", "", true}, {"v0.0.0+gunknown", "", false},
		{"v0.0.0+g012", "", false}, {"v0.0.0+g9876543", "", false},
	} {
		if got := installedUpdateMatches(tc.version, updater.InstallTarget{Release: tc.release, Revision: updateTestSHA}); got != tc.want {
			t.Errorf("%s / %s matched = %v", tc.version, tc.release, got)
		}
	}
}
