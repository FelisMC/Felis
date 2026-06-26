package main

import (
	"bytes"
	"strings"
	"testing"
)

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

func TestRunNotImplementedSubcommands(t *testing.T) {
	for _, cmd := range []string{"apply"} {
		var out, errBuf bytes.Buffer
		if code := run([]string{cmd}, &out, &errBuf); code != 3 {
			t.Errorf("%s exit code = %d, want 3", cmd, code)
		}
		if !strings.Contains(errBuf.String(), "not implemented yet") {
			t.Errorf("%s: expected not-implemented notice, got %q", cmd, errBuf.String())
		}
	}
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
