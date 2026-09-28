package worldexport

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const secretToken = "5ecret5ecret5ecret5ecret5ecret5ecret5ecret5ecret5ecret5ecret5ecr"

func params(mode string) JobParams {
	return JobParams{
		Server: "survival", ID: "0011223344556677", Mode: mode,
		WorldPVC: "world-survival-0", BackupPVC: "felis-backups",
		BackupRef: "/backups/survival-1.tar.gz", BackupSHA256: strings.Repeat("ab", 32),
		Path: "plugins/Essentials", Dir: true,
		TargetURL: "http://felis-api-internal.felis.svc.cluster.local:8081/api/v1/internal/exports/0011223344556677",
		Token:     secretToken,
		Namespace: "minecraft", ServiceAccount: "felis-restore", Image: "felis:1",
		BackupRoot: "/backups", WorldsRoot: "/world", Deadline: time.Hour,
		CPULimit: "1", MemLimit: "256Mi", TTLAfterFinished: 5 * time.Minute,
	}
}

// The export Pod reads a world or an archive and hands it to felis-api. It gets
// exactly the one volume it reads, read-only at both ends, no Secret, no API
// token, and no capability beyond DAC_OVERRIDE: a compromised export can read
// that one volume and talk to that one upload URL, nothing more.
func TestExportJobIsolation(t *testing.T) {
	for _, tc := range []struct {
		mode, volume, claim, mountPath string
		args                           []string
	}{
		{ModeWorld, worldVolume, "world-survival-0", "/world", []string{"--worlds-root", "/world"}},
		{ModeBackup, backupVolume, "felis-backups", "/backups", []string{"--ref", "/backups/survival-1.tar.gz", "--backup-root", "/backups", "--sha256", strings.Repeat("ab", 32)}},
		{ModeFiles, worldVolume, "world-survival-0", "/world", []string{"--worlds-root", "/world", "--path", "plugins/Essentials", "--dir"}},
	} {
		job, err := ExportJob(params(tc.mode))
		if err != nil {
			t.Fatalf("%s: ExportJob: %v", tc.mode, err)
		}
		if job.Name != "export-survival-0011223344556677" || job.Namespace != "minecraft" {
			t.Errorf("%s: job = %s/%s", tc.mode, job.Namespace, job.Name)
		}
		for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
			if labels[LabelManagedBy] != "felis-export" || labels[LabelServer] != "survival" || labels[LabelMode] != tc.mode {
				t.Errorf("%s: labels = %v", tc.mode, labels)
			}
		}
		spec := job.Spec.Template.Spec
		if spec.ServiceAccountName != "felis-restore" {
			t.Errorf("%s: service account = %q", tc.mode, spec.ServiceAccountName)
		}
		if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
			t.Errorf("%s: the SA token is mounted", tc.mode)
		}
		if spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("%s: restart policy = %q", tc.mode, spec.RestartPolicy)
		}
		if len(spec.Volumes) != 1 {
			t.Fatalf("%s: volumes = %+v, want exactly one", tc.mode, spec.Volumes)
		}
		v := spec.Volumes[0]
		if v.Name != tc.volume || v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != tc.claim || !v.PersistentVolumeClaim.ReadOnly {
			t.Errorf("%s: volume = %+v, want %s read-only", tc.mode, v, tc.claim)
		}
		if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 {
			t.Fatalf("%s: containers = %d, init = %d", tc.mode, len(spec.Containers), len(spec.InitContainers))
		}
		c := spec.Containers[0]
		if len(c.VolumeMounts) != 1 || c.VolumeMounts[0] != (corev1.VolumeMount{Name: tc.volume, MountPath: tc.mountPath, ReadOnly: true}) {
			t.Errorf("%s: mounts = %+v", tc.mode, c.VolumeMounts)
		}
		if len(c.EnvFrom) != 0 || len(c.Env) != 1 || c.Env[0] != (corev1.EnvVar{Name: TokenEnv, Value: secretToken}) {
			t.Errorf("%s: env = %+v, envFrom = %+v; want only the token", tc.mode, c.Env, c.EnvFrom)
		}
		if strings.Contains(strings.Join(c.Args, " "), secretToken) {
			t.Errorf("%s: the token rides argv: %v", tc.mode, c.Args)
		}
		wantArgs := append([]string{"--mode", tc.mode, "--server", "survival", "--target-url", params(tc.mode).TargetURL}, tc.args...)
		if !slices.Equal(c.Command, []string{felisBinaryPath, "export"}) || !slices.Equal(c.Args, wantArgs) {
			t.Errorf("%s: command = %v %v", tc.mode, c.Command, c.Args)
		}
		sc := c.SecurityContext
		if sc == nil || sc.Privileged == nil || *sc.Privileged || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
			sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s: container security context = %+v", tc.mode, sc)
		}
		if sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
			!slices.Equal(sc.Capabilities.Add, []corev1.Capability{"DAC_OVERRIDE"}) {
			t.Errorf("%s: capabilities = %+v", tc.mode, sc.Capabilities)
		}
		if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
			t.Errorf("%s: termination message policy = %q", tc.mode, c.TerminationMessagePolicy)
		}
		if psc := spec.SecurityContext; psc == nil || psc.RunAsUser == nil || *psc.RunAsUser != 0 || psc.FSGroup != nil {
			t.Errorf("%s: pod security context = %+v", tc.mode, psc)
		}
		if b := job.Spec.BackoffLimit; b == nil || *b != 0 {
			t.Errorf("%s: backoffLimit = %v, want 0", tc.mode, b)
		}
		if d := job.Spec.ActiveDeadlineSeconds; d == nil || *d != 3600 {
			t.Errorf("%s: activeDeadlineSeconds = %v, want 3600", tc.mode, d)
		}
		if ttl := job.Spec.TTLSecondsAfterFinished; ttl == nil || *ttl != 300 {
			t.Errorf("%s: ttlSecondsAfterFinished = %v, want 300", tc.mode, ttl)
		}
	}
}

func TestExportJobRefusesIncompleteParams(t *testing.T) {
	for name, edit := range map[string]func(*JobParams){
		"no image":            func(p *JobParams) { p.Image = "" },
		"no token":            func(p *JobParams) { p.Token = "" },
		"no target":           func(p *JobParams) { p.TargetURL = "" },
		"no id":               func(p *JobParams) { p.ID = "" },
		"unknown mode":        func(p *JobParams) { p.Mode = "both" },
		"world without claim": func(p *JobParams) { p.WorldPVC = "" },
	} {
		p := params(ModeWorld)
		edit(&p)
		if _, err := ExportJob(p); err == nil {
			t.Errorf("%s: ExportJob accepted %+v", name, p)
		}
	}
	p := params(ModeBackup)
	p.BackupRef = ""
	if _, err := ExportJob(p); err == nil {
		t.Error("a backup export without a ref was accepted")
	}
	p = params(ModeFiles)
	p.Path = ""
	if _, err := ExportJob(p); err == nil {
		t.Error("a files export without a path was accepted")
	}
	p = params(ModeFiles)
	p.WorldPVC = ""
	if _, err := ExportJob(p); err == nil {
		t.Error("a files export without a world claim was accepted")
	}
}

// TestExportJobOptionalArgs: a backup with no recorded digest carries no
// --sha256, and a file download carries its path and no --dir.
func TestExportJobOptionalArgs(t *testing.T) {
	p := params(ModeBackup)
	p.BackupSHA256 = ""
	job, err := ExportJob(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--ref", "/backups/survival-1.tar.gz", "--backup-root", "/backups"}
	if args := job.Spec.Template.Spec.Containers[0].Args; !slices.Equal(args[len(args)-len(want):], want) || slices.Contains(args, "--sha256") {
		t.Errorf("backup args = %v", args)
	}
	p = params(ModeFiles)
	p.Dir = false
	if job, err = ExportJob(p); err != nil {
		t.Fatal(err)
	}
	want = []string{"--worlds-root", "/world", "--path", "plugins/Essentials"}
	if args := job.Spec.Template.Spec.Containers[0].Args; !slices.Equal(args[len(args)-len(want):], want) || slices.Contains(args, "--dir") {
		t.Errorf("file args = %v", args)
	}
}

// TestExportJobDefaults: a caller that leaves the deadline and the TTL unset
// still gets a Job that ends and is collected.
func TestExportJobDefaults(t *testing.T) {
	p := params(ModeWorld)
	p.Deadline, p.TTLAfterFinished = 0, 0
	job, err := ExportJob(p)
	if err != nil {
		t.Fatal(err)
	}
	if d := job.Spec.ActiveDeadlineSeconds; d == nil || *d != 7200 {
		t.Errorf("activeDeadlineSeconds = %v, want 7200", d)
	}
	if ttl := job.Spec.TTLSecondsAfterFinished; ttl == nil || *ttl != 600 {
		t.Errorf("ttlSecondsAfterFinished = %v, want 600", ttl)
	}
}

func TestStartCreatesTheJob(t *testing.T) {
	cs := fake.NewSimpleClientset()
	e := New(cs, Config{Image: "felis:1", BackupPVC: "felis-backups"})
	name, err := e.Start(context.Background(), Request{
		Server: "survival", Mode: ModeWorld, ID: "0011223344556677",
		TargetURL: "http://api:8081/api/v1/internal/exports/0011223344556677", Token: secretToken,
	})
	if err != nil || name != "export-survival-0011223344556677" {
		t.Fatalf("Start = %q, %v", name, err)
	}
	job, err := cs.BatchV1().Jobs("minecraft").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the Job is not in the minecraft namespace: %v", err)
	}
	if claim := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; claim != "world-survival-0" {
		t.Errorf("world claim = %q, want world-survival-0", claim)
	}
	if d := *job.Spec.ActiveDeadlineSeconds; d != int64(defaultDeadline/time.Second) {
		t.Errorf("deadline = %d, want the %s default", d, defaultDeadline)
	}
	if _, err := e.Start(context.Background(), Request{Server: "survival", Mode: ModeWorld, ID: "0011223344556677",
		TargetURL: "http://api:8081/x", Token: secretToken}); err == nil {
		t.Error("a second Job of the same name was reported as created")
	}

	// Each mode's own fields reach the Pod's arguments.
	for _, tc := range []struct {
		r    Request
		tail []string
	}{
		{Request{Server: "survival", Mode: ModeBackup, ID: "1111111111111111", BackupRef: "/backups/a.tar.gz", BackupSHA256: strings.Repeat("cd", 32)},
			[]string{"--ref", "/backups/a.tar.gz", "--backup-root", "/backups", "--sha256", strings.Repeat("cd", 32)}},
		{Request{Server: "survival", Mode: ModeFiles, ID: "2222222222222222", Path: "plugins/Essentials", Dir: true},
			[]string{"--worlds-root", "/world", "--path", "plugins/Essentials", "--dir"}},
	} {
		tc.r.TargetURL, tc.r.Token = "http://api:8081/x", secretToken
		name, err := e.Start(context.Background(), tc.r)
		if err != nil {
			t.Fatalf("%s: Start: %v", tc.r.Mode, err)
		}
		job, err := cs.BatchV1().Jobs("minecraft").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if args := job.Spec.Template.Spec.Containers[0].Args; !slices.Equal(args[len(args)-len(tc.tail):], tc.tail) {
			t.Errorf("%s: args = %v, want them to end %v", tc.r.Mode, args, tc.tail)
		}
	}
}
