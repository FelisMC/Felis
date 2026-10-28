package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"felis.lolicon.best/internal/nodecontrol"
)

const updatePollEvery = 3 * time.Second

// updateUnitName is the transient systemd unit of one panel update; the installer restarts node-control, never this unit.
func updateUnitName(taskID string) string {
	return "felis-update-" + taskID + ".service"
}

type unitState struct {
	Load, Active, Sub, Result, Status string
}

func (e nodeExecutor) hostCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	if e.runHost != nil {
		return e.runHost(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (e nodeExecutor) updateUnitState(ctx context.Context, unit string) (unitState, error) {
	out, err := e.hostCommand(ctx, "systemctl", "show", unit, "--property=LoadState,ActiveState,SubState,Result,ExecMainStatus")
	if err != nil {
		return unitState{}, fmt.Errorf("read %s: %v: %s", unit, err, bytes.TrimSpace(out))
	}
	var st unitState
	fields := map[string]*string{"LoadState": &st.Load, "ActiveState": &st.Active, "SubState": &st.Sub, "Result": &st.Result, "ExecMainStatus": &st.Status}
	scan := bufio.NewScanner(bytes.NewReader(out))
	for scan.Scan() {
		if key, value, ok := strings.Cut(scan.Text(), "="); ok && fields[key] != nil {
			*fields[key] = value
		}
	}
	return st, scan.Err()
}

// update applies a release in a transient unit and waits for it, also after node-control restarts mid-update.
func (e nodeExecutor) update(ctx context.Context, r nodecontrol.Request, stage func(string) error, out io.Writer) error {
	task, ok := nodecontrol.CurrentTask(ctx)
	if !ok {
		return errors.New("no host task is running")
	}
	unit := updateUnitName(task.ID)
	st, err := e.updateUnitState(ctx, unit)
	if err != nil {
		return err
	}
	switch {
	case st.Load == "not-found" && task.Resumed:
		return errors.New("the update stopped with the host; run `felis version` and `felis update --check` on the host before retrying")
	case st.Load == "not-found":
		if err := stage("update"); err != nil {
			return err
		}
		args := []string{"--unit", unit, "--description", "Felis update to " + r.Params["version"] + " started from the panel",
			"--property", "RemainAfterExit=yes",
			"--property", "StandardOutput=append:" + task.Log, "--property", "StandardError=append:" + task.Log,
			"--", e.binary, "update", "--apply", "--version", r.Params["version"]}
		if r.Params["now"] == "true" {
			args = append(args, "--now")
		}
		if raw, err := e.hostCommand(ctx, "systemd-run", args...); err != nil {
			return fmt.Errorf("start the update: %v: %s", err, bytes.TrimSpace(raw))
		}
	default:
		if err := stage("update_resumed"); err != nil {
			return err
		}
	}
	poll := e.pollEvery
	if poll == 0 {
		poll = updatePollEvery
	}
	for {
		st, err := e.updateUnitState(ctx, unit)
		if err != nil {
			return err
		}
		switch {
		case st.Active == "failed":
			e.releaseUnit(unit)
			return fmt.Errorf("the update failed (%s, exit status %s); the log shows the installer output and the pre-update backup is kept", st.Result, st.Status)
		case st.Active == "active" && st.Sub == "exited":
			e.releaseUnit(unit)
			if st.Status != "0" {
				return fmt.Errorf("the update ended with exit status %s; the log shows why", st.Status)
			}
			fmt.Fprintln(out, "[felis] update finished")
			return nil
		case st.Load == "not-found":
			return errors.New("the update unit disappeared before reporting a result; check `felis version` on the host")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// releaseUnit unloads the finished unit; a leftover only keeps its result visible to systemctl.
func (e nodeExecutor) releaseUnit(unit string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = e.hostCommand(ctx, "systemctl", "stop", unit)
	_, _ = e.hostCommand(ctx, "systemctl", "reset-failed", unit)
}
