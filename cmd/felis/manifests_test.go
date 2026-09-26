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
		// Backup works out of the box: the archive PVC renders and the api gets
		// the env that wires the backup/restore executors to it.
		"name: felis-backups",
		"name: FELIS_BACKUP_PVC",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered bundle missing %q", want)
		}
	}
	if strings.Contains(text, "ClusterRole") {
		t.Error("rendered bundle must not contain ClusterRole/ClusterRoleBinding")
	}
	// Without the retention flags, the reaper CronJob is not rendered and the
	// generator says so on stderr, naming what that leaves: backups that never
	// expire.
	if strings.Contains(text, "kind: CronJob") {
		t.Error("no reaper CronJob must render without --archive-local-path")
	}
	if !strings.Contains(errBuf.String(), "not rendered") || !strings.Contains(errBuf.String(), "never expired") {
		t.Errorf("expected a 'reaper not rendered, backups never expired' notice on stderr, got %q", errBuf.String())
	}
}

// TestManifestsReaperRequiresStorage proves --worlds-host-path is a fail-loud
// opt-in: asking for the reaper without a writable archive store (the backup PVC,
// which defaults to felis-backups but can be emptied) and its mount path (which
// must equal [archive] local_path) is rejected rather than silently dropping
// retention or deleting worlds it could not archive first.
func TestManifestsReaperRequiresStorage(t *testing.T) {
	base := []string{"manifests", "--felis-image", "reg/felis:test", "--velocity-cidr", "10.0.0.5/32", "--worlds-host-path", "/var/lib/felis/worlds"}
	for _, extra := range [][]string{
		{},                        // missing archive-local-path (backup-pvc defaults)
		{"--backup-pvc", "other"}, // still missing archive-local-path
		// A reaper with no archive store would have nowhere to write the archive
		// it must verify before deleting a world; emptying the PVC is rejected.
		{"--archive-local-path", "/backups", "--backup-pvc="},
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

// TestManifestsBackupPVCOptOut proves --backup-pvc= renders a bundle with no
// archive store at all: no PVC and no FELIS_BACKUP_PVC env, so backup/restore
// answer 503 instead of pointing Jobs at a claim nobody provisions.
func TestManifestsBackupPVCOptOut(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"manifests", "--felis-image", "reg/felis:test",
		"--velocity-cidr", "10.0.0.5/32", "--backup-pvc="}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	for _, absent := range []string{"felis-backups", "FELIS_BACKUP_PVC"} {
		if strings.Contains(out.String(), absent) {
			t.Errorf("--backup-pvc= bundle must not contain %q", absent)
		}
	}
}

// TestManifestsRendersRetentionOnly: the archive store without a worlds root
// still gets the daily CronJob, retention-only, so backups past their expiry
// leave the store on an install that never reaps a world; the operator is told
// which of the two it got. An archive path with the store switched off is a
// mistake and fails loud.
func TestManifestsRendersRetentionOnly(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"manifests", "--felis-image", "reg/felis:test", "--velocity-cidr", "10.0.0.5/32",
		"--archive-local-path", "/var/lib/felis/archives"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	text := out.String()
	for _, want := range []string{"kind: CronJob", "name: felis-reaper", "--retention-only", "claimName: felis-backups"} {
		if !strings.Contains(text, want) {
			t.Errorf("retention-only bundle missing %q", want)
		}
	}
	if strings.Contains(text, "kind: PersistentVolume\n") || strings.Contains(text, "--worlds-root") {
		t.Error("a retention-only bundle must not reach for a worlds root")
	}
	if !strings.Contains(errBuf.String(), "retention-only") {
		t.Errorf("stderr must say the CronJob is retention-only, got %q", errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	code = run([]string{"manifests", "--felis-image", "reg/felis:test", "--velocity-cidr", "10.0.0.5/32",
		"--archive-local-path", "/var/lib/felis/archives", "--backup-pvc="}, &out, &errBuf)
	if code != 2 || out.Len() != 0 || !strings.Contains(errBuf.String(), "--backup-pvc is empty") {
		t.Errorf("archive path without an archive store: exit=%d out=%d bytes stderr=%q, want a fail-loud 2", code, out.Len(), errBuf.String())
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
	for _, want := range []string{"local-path", "nodeSelector"} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("reaper render must warn operators about %q on stderr, got %q", want, errBuf.String())
		}
	}
}

// TestManifestsReaperNodePin: --reaper-node pins the rendered CronJob's pod via
// kubernetes.io/hostname and replaces the "no nodeSelector" hazard note with the
// pin confirmation; using it without the worlds root is a fail-loud 2.
func TestManifestsReaperNodePin(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{
		"manifests",
		"--felis-image", "registry.felis.svc:5000/felis:v1",
		"--velocity-cidr", "10.0.0.5/32",
		"--worlds-host-path", "/var/lib/felis/worlds",
		"--archive-local-path", "/backups",
		"--reaper-node", "node-a",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	for _, want := range []string{
		"kubernetes.io/hostname: node-a",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("pinned render missing %q", want)
		}
	}
	if !strings.Contains(errBuf.String(), "node-a") {
		t.Errorf("stderr must confirm the pin, got %q", errBuf.String())
	}

	var out2, err2 bytes.Buffer
	if code := run([]string{
		"manifests",
		"--felis-image", "registry.felis.svc:5000/felis:v1",
		"--velocity-cidr", "10.0.0.5/32",
		"--reaper-node", "node-a",
	}, &out2, &err2); code != 2 {
		t.Errorf("--reaper-node without --worlds-host-path: exit = %d, want 2", code)
	}
}

// TestManifestsStorageSizes proves the PVC size flags reach the rendered claims
// and a size the API server would reject fails before anything is applied.
func TestManifestsStorageSizes(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"manifests", "--felis-image", "reg/felis:test", "--velocity-cidr", "10.0.0.5/32",
		"--registry-storage", "40Gi", "--uploads-storage", "8Gi"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, errBuf.String())
	}
	for _, want := range []string{"storage: 40Gi", "storage: 8Gi"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("bundle lacks %q", want)
		}
	}

	out.Reset()
	errBuf.Reset()
	code = run([]string{"manifests", "--felis-image", "reg/felis:test", "--velocity-cidr", "10.0.0.5/32",
		"--registry-storage", "lots"}, &out, &errBuf)
	if code == 0 || !strings.Contains(errBuf.String(), "registry storage") {
		t.Errorf("--registry-storage lots: exit %d, stderr %q; want a refusal naming the flag", code, errBuf.String())
	}
}

// TestManifestsOnlyPostgres: the installer renders the database before it has
// an image or a proxy address for the rest of the bundle, so --only postgres
// must render without them, and render the database and nothing else (a stray
// Deployment in that apply would start without its identities).
func TestManifestsOnlyPostgres(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"manifests", "--only", "postgres", "--control-namespace", "ctl", "--postgres-image", "example/pg:18@sha256:abc"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errBuf.String())
	}
	var kinds []string
	for _, doc := range strings.Split(out.String(), "\n---\n") {
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(line, "kind: ") {
				kinds = append(kinds, strings.TrimPrefix(line, "kind: "))
			}
		}
	}
	if got, want := strings.Join(kinds, ","), "Namespace,ConfigMap,NetworkPolicy,Deployment,Service"; got != want {
		t.Errorf("rendered kinds %s, want %s", got, want)
	}
	for _, want := range []string{"name: felis-postgres", "namespace: ctl", "image: example/pg:18@sha256:abc"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("rendered database missing %q", want)
		}
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"manifests", "--only", "registry"}, &out, &errBuf); code != 2 || out.Len() != 0 {
		t.Errorf("--only registry: exit %d with %d bytes of YAML, want 2 and none", code, out.Len())
	}
}
