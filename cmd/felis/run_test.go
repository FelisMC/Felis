package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"felis.lolicon.best/internal/config"
)

func TestBootstrapConfigKeys(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"bootstrap-assets", "config-keys"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit code = %d: %s", code, errBuf.String())
	}
	var cfg config.Config
	meta, err := toml.Decode(strings.TrimSpace(out.String())+` = "26.3"`, &cfg)
	if err != nil || len(meta.Undecoded()) != 0 {
		t.Fatalf("advertised config key is unsupported: %v, undecoded: %v", err, meta.Undecoded())
	}
	if cfg.Velocity.GameVersion != "26.3" {
		t.Fatalf("advertised key did not set the game version: %q", cfg.Velocity.GameVersion)
	}
}

func TestRunNoArgsPrintsUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run(nil, &out, &errBuf); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "Usage:") {
		t.Errorf("expected usage on stderr, got %q", errBuf.String())
	}
}

func TestRunHelp(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"help"}, &out, &errBuf); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "felis") {
		t.Errorf("expected usage on stdout, got %q", out.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{"frobnicate"}, &out, &errBuf); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "unknown command") {
		t.Errorf("expected unknown-command error, got %q", errBuf.String())
	}
}

// undocumentedCommands are routable on purpose but kept out of the usage text: they
// are called by deploy/bootstrap.sh or the operator's initContainers, not by a human
// at a prompt. Listing them here is what makes their absence from usage a deliberate
// decision rather than an oversight.
var undocumentedCommands = map[string]bool{
	"bootstrap-assets": true, "init-forwarding": true, "init-volume": true,
	"pin-images": true, "image-bundle": true,
}

// The usage text and the dispatch table must describe the same set of commands.
//
// This exists because the failure it catches already happened: `version` shipped
// implemented but unreachable — cmdVersion existed with nothing routing to it and no
// usage line — so `felis version` fell through to "unknown command", and no test
// noticed. Comparing the two lists is only possible because the router is a map; a
// switch cannot be enumerated.
//
// It compares names WITHOUT invoking anything. Running each command to see whether it
// is routed would start servers, dial clusters, and (for `update`) hit the network —
// a slow, flaky test of the wrong thing.
func TestUsageAndDispatchTableAgree(t *testing.T) {
	documented := map[string]bool{}
	var inCommands bool
	for line := range strings.SplitSeq(usage, "\n") {
		// Only the block under "Commands:" lists commands. The "Usage:" block above it
		// has the same two-space indent but its entry is the "felis <command> [flags]"
		// synopsis, which is not a subcommand.
		if strings.HasPrefix(line, "Commands:") {
			inCommands = true
			continue
		}
		if !inCommands {
			continue
		}
		// Command lines are the "  <name>  <description>" entries; the two-space indent
		// distinguishes them from wrapped continuation lines.
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		name, _, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || name == "" {
			continue
		}
		documented[name] = true
		if _, routed := commands[name]; !routed {
			t.Errorf("usage advertises %q but the dispatch table has no entry for it", name)
		}
	}
	for name := range commands {
		if !documented[name] && !undocumentedCommands[name] {
			t.Errorf("%q is routable but undocumented; add it to usage, or to undocumentedCommands if it is an internal entrypoint", name)
		}
	}
}

func TestRunApplyRequiresFileFlag(t *testing.T) {
	// Without -f the command must fail with usage (2), not try to contact a
	// cluster. It can't return 0 because no CRD was created, and it can't return 1
	// because that would be ambiguous with a real server-side failure.
	var out, errBuf bytes.Buffer
	code := run([]string{"apply"}, &out, &errBuf)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "missing required flag -f") {
		t.Errorf("expected -f usage, got %q", errBuf.String())
	}
}

func TestRunApplyRejectsInvalidJSON(t *testing.T) {
	// Sending garbage via a temp file must exit 1 (input error), not panic or hang.
	empty := t.TempDir() + "/empty.json"
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	code := run([]string{"apply", "-f", empty}, &out, &errBuf)
	// The empty file must fail JSON parsing or validation; either way it exits 1.
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "invalid") && !strings.Contains(errBuf.String(), "required") {
		t.Errorf("expected input error, got %q", errBuf.String())
	}
}

func TestRunSetupAndBreakGlassCommands(t *testing.T) {
	t.Run("setup help", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		if code := run([]string{"setup", "-h"}, &out, &errBuf); code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
		if !strings.Contains(errBuf.String(), "Usage of setup") {
			t.Errorf("expected setup help, got stderr=%q stdout=%q", errBuf.String(), out.String())
		}
		if strings.Contains(errBuf.String(), "Usage of breakGlass") {
			t.Errorf("setup must not route to breakGlass help, got %q", errBuf.String())
		}
	})

	t.Run("breakGlass help", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		if code := run([]string{"breakGlass", "-h"}, &out, &errBuf); code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
		if !strings.Contains(errBuf.String(), "Usage of breakGlass") {
			t.Errorf("expected breakGlass help, got stderr=%q stdout=%q", errBuf.String(), out.String())
		}
	})

	t.Run("lowercase breakglass is intentionally rejected", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		if code := run([]string{"breakglass", "-h"}, &out, &errBuf); code != 2 {
			t.Errorf("exit code = %d, want 2", code)
		}
		if !strings.Contains(errBuf.String(), "unknown command") {
			t.Errorf("expected lowercase alias rejection, got stderr=%q stdout=%q", errBuf.String(), out.String())
		}
	})
}

func TestRunReaperValidatesConfigBeforeDialing(t *testing.T) {
	var out, errBuf bytes.Buffer
	// Like api, reaper must fail fast (exit 1) at config load, before any
	// database or cluster contact.
	code := run([]string{"reaper", "-config", "this-file-does-not-exist.toml"}, &out, &errBuf)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "felis reaper:") {
		t.Errorf("expected reaper error on stderr, got %q", errBuf.String())
	}
}

func TestRunAPIValidatesConfigBeforeDialing(t *testing.T) {
	var out, errBuf bytes.Buffer
	// A non-existent config must fail fast (exit 1) at config load, before any
	// database or cluster contact.
	code := run([]string{"api", "-config", "this-file-does-not-exist.toml"}, &out, &errBuf)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "felis api:") {
		t.Errorf("expected api error on stderr, got %q", errBuf.String())
	}
}

func TestRunMigrateRequiresUpVerb(t *testing.T) {
	var out, errBuf bytes.Buffer
	// "migrate" with no verb should fail fast on usage, not touch a database.
	if code := run([]string{"migrate"}, &out, &errBuf); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "felis migrate up") {
		t.Errorf("expected migrate usage, got %q", errBuf.String())
	}
}
