package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestManifestsRequiresVelocityCIDR proves the generator refuses to emit a bundle
// without --velocity-cidr (the game policy would otherwise fail closed silently).
func TestManifestsRequiresVelocityCIDR(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"manifests"}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("exit code = 0, want nonzero (missing --velocity-cidr)")
	}
	if !strings.Contains(errBuf.String(), "velocity-cidr") {
		t.Errorf("expected a --velocity-cidr error, got %q", errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("no YAML must be written when the flag is missing, got %q", out.String())
	}
}

// TestManifestsRejectsBadCIDR proves CIDR inputs are validated. --felis-image is
// supplied so the only defect is the CIDR (the felis-image requirement is checked
// before CIDR validation, so omitting it would surface the wrong error).
func TestManifestsRejectsBadCIDR(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"manifests", "--felis-image", "reg/felis:test", "--velocity-cidr", "not-a-cidr"}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("exit code = 0, want nonzero (invalid CIDR)")
	}
	if !strings.Contains(errBuf.String(), "invalid CIDR") {
		t.Errorf("expected an invalid-CIDR error, got %q", errBuf.String())
	}
}

// TestManifestsRequiresFelisImage proves the generator refuses to emit a bundle
// without --felis-image (the api/operator Deployments have no default image, and
// FELIS_IMAGE has no safe guess). Same fail-loud contract as --velocity-cidr.
func TestManifestsRequiresFelisImage(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"manifests", "--velocity-cidr", "10.0.0.5/32"}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("exit code = 0, want nonzero (missing --felis-image)")
	}
	if !strings.Contains(errBuf.String(), "felis-image") {
		t.Errorf("expected a --felis-image error, got %q", errBuf.String())
	}
	if out.Len() != 0 {
		t.Errorf("no YAML must be written when --felis-image is missing, got %q", out.String())
	}
}

// TestManifestsRendersBundle proves the happy path: a valid invocation writes a
// multi-doc YAML bundle containing the fence kinds and no cluster-scoped RBAC.
func TestManifestsRendersBundle(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"manifests", "--felis-image", "registry.felis.svc:5000/felis:v1", "--velocity-cidr", "10.0.0.5/32"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	text := out.String()
	for _, want := range []string{
		"kind: Namespace",
		"kind: ServiceAccount",
		"kind: Role",
		"kind: RoleBinding",
		"kind: NetworkPolicy",
		// The running control-plane workloads now in the bundle.
		"kind: Deployment",
		"kind: Service",
		"kind: PersistentVolumeClaim",
		"felis-allow-rcon-from-control-plane",
		"felis-allow-game-from-velocity",
		"10.0.0.5/32",
		// The felis image flows through to the Deployments.
		"registry.felis.svc:5000/felis:v1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered bundle missing %q", want)
		}
	}
	if strings.Contains(text, "ClusterRole") {
		t.Error("rendered bundle must not contain ClusterRole/ClusterRoleBinding")
	}
	// Without the retention flags, the reaper CronJob is not rendered and the
	// generator says so on stderr.
	if strings.Contains(text, "kind: CronJob") {
		t.Error("no reaper CronJob must render without --worlds-host-path")
	}
	if !strings.Contains(errBuf.String(), "not rendered") {
		t.Errorf("expected a 'reaper not rendered' notice on stderr, got %q", errBuf.String())
	}
}

// TestManifestsReaperRequiresTrio proves --worlds-host-path is a fail-loud opt-in:
// asking for the reaper without the backup PVC and its mount path (which must equal
// [archive] local_path) is rejected rather than silently dropping retention.
func TestManifestsReaperRequiresTrio(t *testing.T) {
	base := []string{"manifests", "--felis-image", "reg/felis:test", "--velocity-cidr", "10.0.0.5/32", "--worlds-host-path", "/var/lib/felis/worlds"}
	for _, extra := range [][]string{
		{},                                   // neither backup-pvc nor archive-local-path
		{"--backup-pvc", "felis-backups"},    // missing archive-local-path
		{"--archive-local-path", "/backups"}, // missing backup-pvc
	} {
		var out, errBuf bytes.Buffer
		code := run(append(append([]string{}, base...), extra...), &out, &errBuf)
		if code == 0 {
			t.Fatalf("extra=%v: exit code = 0, want nonzero (incomplete reaper config)", extra)
		}
		if !strings.Contains(errBuf.String(), "backup-pvc") || !strings.Contains(errBuf.String(), "archive-local-path") {
			t.Errorf("extra=%v: expected the trio requirement on stderr, got %q", extra, errBuf.String())
		}
		if out.Len() != 0 {
			t.Errorf("extra=%v: no YAML must be written on a fail-loud reject, got %q", extra, out.String())
		}
	}
}

// TestManifestsRendersReaper proves the happy path with the full retention trio:
// a batch/v1 CronJob is emitted, named felis-reaper, mounting the backup PVC at the
// supplied archive path.
func TestManifestsRendersReaper(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{
		"manifests",
		"--felis-image", "registry.felis.svc:5000/felis:v1",
		"--velocity-cidr", "10.0.0.5/32",
		"--worlds-host-path", "/var/lib/felis/worlds",
		"--backup-pvc", "felis-backups",
		"--archive-local-path", "/backups",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	text := out.String()
	for _, want := range []string{
		"kind: CronJob",
		"name: felis-reaper",
		"/var/lib/felis/worlds", // the worlds hostPath
		"claimName: felis-backups",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered bundle with reaper missing %q", want)
		}
	}
	// Rendering the reaper must also warn the operator about the two preconditions
	// this generator cannot verify (else a misarranged hostPath silently no-ops
	// retention): the <path>/<pvc> arrangement-dependency and the multi-node
	// nodeSelector hazard.
	for _, want := range []string{"local-path-provisioner", "nodeSelector"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("reaper render must warn operators about %q on stderr, got %q", want, errBuf.String())
		}
	}
}
