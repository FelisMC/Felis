package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"felis.lolicon.best/internal/updater"
	"felis.lolicon.best/internal/updates"
)

type updateOptions struct {
	release, ref, cfgPath, repoURL, expectedCommit string
	preserveToken                                  bool
	apply, all, force, now                         bool
	selected                                       map[string]bool
}

func (o *updateOptions) validate(dev, check, record bool) error {
	if (o.release != "" && o.ref != "") || (dev && (o.release != "" || o.ref != "")) {
		return fmt.Errorf("choose only one of --version, --ref and --dev")
	}
	if o.apply && (check || record) {
		return fmt.Errorf("--apply cannot be combined with --check or --record")
	}
	if o.expectedCommit != "" {
		if _, err := hex.DecodeString(o.expectedCommit); err != nil || len(o.expectedCommit) != 40 || !o.apply {
			return fmt.Errorf("--expect-commit requires --apply and a full commit SHA")
		}
	}
	o.expectedCommit = strings.ToLower(o.expectedCommit)
	if o.now && !o.apply {
		return fmt.Errorf("--now requires --apply")
	}
	if record && (dev || o.release != "" || o.ref != "") {
		return fmt.Errorf("--record checks installed components; use a separate target check")
	}
	if o.release != "" {
		o.release = "v" + strings.TrimPrefix(o.release, "v")
		v, err := updates.Parse(o.release)
		if err != nil || v.IsPrerelease() || strings.Contains(o.release, "+") || strings.Count(o.release, ".") != 2 {
			return fmt.Errorf("--version must be a published stable release such as v0.2.0; use --ref for other tags")
		}
	}
	return nil
}

func (o updateOptions) dependencies() bool {
	return o.all || o.selected["k3s"] || o.selected["cloudflared"]
}

func renderInstallPlan(target updater.InstallTarget, o updateOptions) string {
	var b strings.Builder
	label := target.Release
	if label == "" {
		label = "source build"
	}
	fmt.Fprintf(&b, "\nFelis target: %s\nCommit: %s\n", label, target.Revision)
	b.WriteString("Scope: host CLI, API, operator, embedded panel, platform manifests/RBAC, Velocity + Felis plugins, login/lobby images, release-pinned JRE and PostgreSQL.\n")
	if o.dependencies() {
		b.WriteString("Host dependencies: also reconcile k3s and cloudflared to this target's pins.\n")
	}
	b.WriteString("Upstream availability above is advisory; application uses this target's compatible pins, not each upstream's newest release. User server images remain pinned.\n")
	b.WriteString("Before applying: check maintenance, back up the database and host/server configuration, then check maintenance again.\n")
	b.WriteString("API, proxy and system spaces may restart.\n")
	if o.apply {
		return b.String()
	}
	b.WriteString("No update is applied by this check.\n")
	cmd := "sudo"
	if o.preserveToken {
		cmd += " --preserve-env=FELIS_GITHUB_TOKEN"
	}
	if o.repoURL != "" {
		cmd += " env FELIS_REPO_URL=" + shellQuote(o.repoURL)
	}
	cmd += " felis update --apply"
	assets := target.Release != "" && canUseReleaseAssets(target.Script, o.force)
	if target.Release != "" && !assets {
		b.WriteString("This release installer cannot pin the requested published-asset install; the apply command builds its exact source commit instead.\n")
	}
	if assets {
		cmd += " --version " + target.Release + " --expect-commit " + target.Revision
	} else {
		cmd += " --ref " + target.Revision
	}
	if o.dependencies() {
		cmd += " --all"
	}
	if o.force {
		cmd += " --force"
	}
	if o.cfgPath != "/etc/felis/felis.toml" {
		cmd += " --config " + shellQuote(o.cfgPath)
	}
	fmt.Fprintf(&b, "\nAfter reviewing, run inside the maintenance window:\n  %s\n", cmd)
	b.WriteString("Without an active window, apply is refused. For an explicit manual maintenance run, add --now. --force never bypasses these checks.\n")
	return b.String()
}

// updateApplySteps isolates the three mutating/verification boundaries so the
// ordering and refusal paths can be tested without a live node.
type updateApplySteps struct {
	window                  func(context.Context) (updates.Window, error)
	backup, install, verify func(context.Context) error
}

func runUpdateApply(ctx context.Context, o updateOptions, steps updateApplySteps) error {
	check := func() error {
		win, err := steps.window(ctx)
		if err != nil {
			return fmt.Errorf("cannot verify maintenance window: %w", err)
		}
		if !o.now && (win.Start.IsZero() || win.End.IsZero() || !win.Contains(time.Now())) {
			return fmt.Errorf("no active maintenance window; set one in the panel or explicitly use --apply --now for manual maintenance")
		}
		return ctx.Err()
	}
	if err := check(); err != nil {
		return err
	}
	if err := steps.backup(ctx); err != nil {
		return fmt.Errorf("pre-update backup failed; nothing applied: %w", err)
	}
	if err := check(); err != nil {
		return err
	}
	if err := steps.install(ctx); err != nil {
		return fmt.Errorf("installer failed; retain the pre-update backup and inspect its output before retrying: %w", err)
	}
	if err := steps.verify(ctx); err != nil {
		return fmt.Errorf("installed binary verification failed: %w", err)
	}
	return nil
}

func canUseReleaseAssets(script string, force bool) bool {
	return strings.Contains(script, `FELIS_RELEASE="${FELIS_RELEASE:-}"`) &&
		(!force || strings.Contains(script, "${FELIS_FORCE_UPDATE:-0}"))
}

func applyHostUpdate(ctx context.Context, target updater.InstallTarget, o updateOptions, repoURL string, stdout, stderr io.Writer) error {
	if target.Release != "" {
		current, err := updates.Parse(resolvedVersion())
		want, _ := updates.Parse(target.Release)
		if err == nil && want.Compare(current) < 0 && !o.force {
			return fmt.Errorf("%s is older than %s; an intentional Felis downgrade requires --force", target.Release, current)
		}
		if !canUseReleaseAssets(target.Script, o.force) {
			return fmt.Errorf("this release installer cannot pin the requested published-asset install; use --ref %s to rebuild its exact source", target.Revision)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return runUpdateApply(ctx, o, updateApplySteps{
		window: func(ctx context.Context) (updates.Window, error) { return readUpdateWindow(ctx, o.cfgPath) },
		backup: func(ctx context.Context) error {
			fmt.Fprintln(stdout, "[felis] taking the pre-update database and deployment-state backup")
			cmd := exec.CommandContext(ctx, exe, "db", "backup", "--config", o.cfgPath, "--label", "pre-migrate")
			cmd.Stdout, cmd.Stderr = stdout, stderr
			return cmd.Run()
		},
		install: func(ctx context.Context) error {
			fmt.Fprintln(stdout, "[felis] applying the inspected Felis target")
			return runUpdateInstaller(ctx, target.Script, updateInstallerEnv(os.Environ(), target, o, repoURL), stdout, stderr)
		},
		verify: func(ctx context.Context) error {
			out, err := exec.CommandContext(ctx, hostBinDir+"/felis", "version").Output()
			if err != nil {
				return err
			}
			line, _, _ := strings.Cut(string(out), "\n")
			if !installedUpdateMatches(strings.TrimPrefix(line, "felis "), target) {
				return fmt.Errorf("wanted %s / %s, got %q", target.Release, target.Revision, line)
			}
			fmt.Fprintln(stdout, line)
			return nil
		},
	})
}

func installedUpdateMatches(version string, target updater.InstallTarget) bool {
	if target.Release != "" {
		return version == target.Release
	}
	_, sha, ok := strings.Cut(version, "+g")
	return ok && len(sha) >= 7 && strings.HasPrefix(target.Revision, sha)
}

func updateInstallerEnv(env []string, target updater.InstallTarget, o updateOptions, repoURL string) []string {
	// A setup hand-off or stale source override must never reinstall the running
	// binary or a different tree than the one just inspected.
	replace := map[string]string{
		"FELIS_BOOTSTRAP_FROM_TUI": "", "FELIS_BOOTSTRAP_BINARY": "", "FELIS_SKIP_FETCH": "", "FELIS_ARTIFACT_DIR": "",
		"FELIS_REF": target.Revision, "FELIS_RELEASE": "", "FELIS_VERSION_BOOTSTRAP": "dev",
		"FELIS_REPO_URL": repoURL, "FELIS_NO_SETUP": "1", "FELIS_INSTALL_MODE": "full",
		"FELIS_UPGRADE_DEPS": "0", "FELIS_FORCE_UPDATE": "0", "FELIS_IMAGE": "",
		"FELIS_GAME_STACK": "pinned", "FELIS_VELOCITY_VERSION": "", "FELIS_JRE_VERSION": "",
		"FELIS_K3S_VERSION": "", "FELIS_CLOUDFLARED_VERSION": "", "FELIS_PREFLIGHT": "strict", "FELIS_PRE_MIGRATE_BACKUP": "1",
	}
	if target.Release != "" {
		replace["FELIS_REF"], replace["FELIS_RELEASE"], replace["FELIS_VERSION_BOOTSTRAP"] = "", target.Release, "release"
	}
	if o.dependencies() {
		replace["FELIS_UPGRADE_DEPS"] = "1"
	}
	if o.force {
		replace["FELIS_FORCE_UPDATE"] = "1"
	}
	out := make([]string, 0, len(env)+len(replace))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if _, overridden := replace[key]; !overridden {
			out = append(out, item)
		}
	}
	for key, value := range replace {
		out = append(out, key+"="+value)
	}
	return out
}

func runUpdateInstaller(ctx context.Context, script string, env []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "bash", "-s")
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, strings.NewReader(script), stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run()
}
