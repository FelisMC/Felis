package main

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"felis.lolicon.best/internal/nodecontrol"
)

const updateTaskID = "0123456789abcdef0123456789abcdef"

// fakeSystemd answers systemctl show from states in order and records every other call.
type fakeSystemd struct {
	states []string
	calls  [][]string
}

func (f *fakeSystemd) run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemctl" && args[0] == "show" {
		state := f.states[0]
		if len(f.states) > 1 {
			f.states = f.states[1:]
		}
		return []byte(state), nil
	}
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil, nil
}

const (
	unitMissing = "LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\nExecMainStatus=0\n"
	unitRunning = "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nExecMainStatus=0\n"
	unitDone    = "LoadState=loaded\nActiveState=active\nSubState=exited\nResult=success\nExecMainStatus=0\n"
	unitFailed  = "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nExecMainStatus=1\n"
)

func runUpdate(t *testing.T, sd *fakeSystemd, resumed bool, params map[string]string) ([]string, error) {
	t.Helper()
	e := nodeExecutor{binary: "/usr/local/bin/felis", runHost: sd.run, pollEvery: 1}
	ctx := nodecontrol.WithTask(context.Background(), nodecontrol.TaskInfo{ID: updateTaskID, Log: "/var/lib/felis/node-control/" + updateTaskID + ".log", Resumed: resumed})
	var stages []string
	err := e.update(ctx, nodecontrol.Request{Action: "update", Params: params}, func(s string) error {
		stages = append(stages, s)
		return nil
	}, io.Discard)
	return stages, err
}

func TestUpdateRunsTheInstallerOutsideNodeControl(t *testing.T) {
	sd := &fakeSystemd{states: []string{unitMissing, unitRunning, unitDone}}
	if _, err := runUpdate(t, sd, false, map[string]string{"version": "v0.3.0", "now": "true"}); err != nil {
		t.Fatal(err)
	}
	start := sd.calls[0]
	unit := "felis-update-" + updateTaskID + ".service"
	if start[0] != "systemd-run" || !slices.Contains(start, unit) || !slices.Contains(start, "RemainAfterExit=yes") ||
		!slices.Contains(start, "StandardOutput=append:/var/lib/felis/node-control/"+updateTaskID+".log") {
		t.Fatalf("start = %v", start)
	}
	if got := strings.Join(start[slices.Index(start, "--")+1:], " "); got != "/usr/local/bin/felis update --apply --version v0.3.0 --now" {
		t.Fatalf("command = %q", got)
	}
	if last := sd.calls[len(sd.calls)-1]; last[0] != "systemctl" || last[1] != "reset-failed" || last[2] != unit {
		t.Fatalf("finished unit left loaded: %v", sd.calls)
	}
}

func TestUpdateInsideTheWindowOmitsNow(t *testing.T) {
	sd := &fakeSystemd{states: []string{unitMissing, unitDone}}
	if _, err := runUpdate(t, sd, false, map[string]string{"version": "v0.3.0", "now": "false"}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(sd.calls[0], "--now") {
		t.Fatalf("start = %v", sd.calls[0])
	}
}

func TestUpdateResumesWithoutStartingAgain(t *testing.T) {
	sd := &fakeSystemd{states: []string{unitRunning, unitDone}}
	stages, err := runUpdate(t, sd, true, map[string]string{"version": "v0.3.0"})
	if err != nil || slices.ContainsFunc(sd.calls, func(c []string) bool { return c[0] == "systemd-run" }) || !slices.Equal(stages, []string{"update_resumed"}) {
		t.Fatalf("resume = %v, calls %v, stages %v", err, sd.calls, stages)
	}
}

func TestUpdateFailures(t *testing.T) {
	sd := &fakeSystemd{states: []string{unitMissing, unitFailed}}
	if _, err := runUpdate(t, sd, false, map[string]string{"version": "v0.3.0"}); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("failed unit = %v", err)
	}
	sd = &fakeSystemd{states: []string{unitMissing}}
	if _, err := runUpdate(t, sd, true, map[string]string{"version": "v0.3.0"}); err == nil || len(sd.calls) != 0 {
		t.Fatalf("a resumed task whose unit is gone = %v, calls %v", err, sd.calls)
	}
}
