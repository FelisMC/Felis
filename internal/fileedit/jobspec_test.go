package fileedit

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func testParams(op string) JobParams {
	return JobParams{
		Server:           "survival",
		OpID:             "deadbeefcafe0001",
		Op:               op,
		Path:             "server.properties",
		WorldPVC:         "world-survival-0",
		Namespace:        "minecraft",
		ServiceAccount:   "felis-restore",
		Image:            "registry.example/felis:v1",
		WorldsRoot:       "/data",
		Deadline:         2 * time.Minute,
		CPULimit:         "500m",
		MemLimit:         "256Mi",
		RunAsUser:        0,
		RunAsGroup:       0,
		FSGroup:          0,
		TTLAfterFinished: 2 * time.Minute,
	}
}

// TestFilesJobIsolation asserts every isolation guarantee FilesJob documents. No
// cluster runs in this environment, so this pure-function test IS the enforcement:
// if someone loosens the Pod spec, this is what catches it.
func TestFilesJobIsolation(t *testing.T) {
	job, err := FilesJob(testParams(OpRead))
	if err != nil {
		t.Fatalf("FilesJob: %v", err)
	}
	spec := job.Spec.Template.Spec

	t.Run("runs under the weak SA with its token un-mounted", func(t *testing.T) {
		if spec.ServiceAccountName != "felis-restore" {
			t.Fatalf("SA = %q, want the weak felis-restore", spec.ServiceAccountName)
		}
		if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
			t.Fatal("the SA token MUST NOT be auto-mounted — the Pod must not reach the K8s API")
		}
	})

	// The four-power red line: a file-editor Pod holds no credential of any kind. It
	// is strictly blinder than the backup Pod, which does mount the config Secret.
	t.Run("mounts exactly one volume and no credential", func(t *testing.T) {
		if len(spec.Volumes) != 1 {
			t.Fatalf("volumes = %d, want exactly 1 (the world PVC)", len(spec.Volumes))
		}
		v := spec.Volumes[0]
		if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "world-survival-0" {
			t.Fatalf("the sole volume must be the world PVC, got %+v", v)
		}
		if v.Secret != nil || v.ConfigMap != nil || v.Projected != nil {
			t.Fatalf("no Secret/ConfigMap/Projected volume may be mounted, got %+v", v)
		}
	})

	t.Run("runs as root, the owner-matching identity for game-image worlds", func(t *testing.T) {
		sc := spec.SecurityContext
		if sc == nil || sc.RunAsNonRoot == nil || *sc.RunAsNonRoot {
			t.Fatal("RunAsNonRoot must be false: root is the owner-matching default for game-image worlds")
		}
		// Root because the world volume belongs to the game uid and Paper saves
		// mode-0600 files a different non-root editor uid cannot open.
		if sc.RunAsUser == nil || *sc.RunAsUser != 0 ||
			sc.RunAsGroup == nil || *sc.RunAsGroup != 0 {
			t.Fatalf("uid/gid must be 0:0 by default, got %+v", sc)
		}
		if sc.FSGroup != nil {
			t.Fatalf("fsGroup must stay unset when zero, got %+v", sc.FSGroup)
		}
	})

	t.Run("container drops every privilege", func(t *testing.T) {
		if len(spec.Containers) != 1 {
			t.Fatalf("containers = %d, want 1", len(spec.Containers))
		}
		sc := spec.Containers[0].SecurityContext
		if sc == nil {
			t.Fatal("the container needs a SecurityContext")
		}
		if sc.Privileged == nil || *sc.Privileged {
			t.Fatal("Privileged must be false")
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Fatal("AllowPrivilegeEscalation must be false")
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Fatal("ReadOnlyRootFilesystem must be true")
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Fatalf("capabilities must drop ALL, got %+v", sc.Capabilities)
		}
		if len(sc.Capabilities.Add) != 1 || sc.Capabilities.Add[0] != "DAC_OVERRIDE" {
			t.Fatalf("capabilities must add exactly DAC_OVERRIDE, got %+v", sc.Capabilities.Add)
		}
	})

	// Only a write creates a file it must hand back to the game uid, so only a
	// write keeps CHOWN; a read stays at DAC_OVERRIDE alone (asserted above).
	t.Run("a write also keeps CHOWN", func(t *testing.T) {
		w, err := FilesJob(testParams(OpWrite))
		if err != nil {
			t.Fatalf("FilesJob: %v", err)
		}
		add := w.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add
		if len(add) != 2 || add[0] != "CHOWN" || add[1] != "DAC_OVERRIDE" {
			t.Fatalf("write capabilities = %v, want [CHOWN DAC_OVERRIDE]", add)
		}
	})

	t.Run("is one-shot, deadlined, and self-collecting", func(t *testing.T) {
		if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
			t.Fatal("BackoffLimit must be 0 — a retried write is a second write")
		}
		if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 120 {
			t.Fatalf("ActiveDeadlineSeconds = %v, want 120", job.Spec.ActiveDeadlineSeconds)
		}
		// The TTL is the ONLY cleanup available: felis-api holds no jobs:delete.
		if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 120 {
			t.Fatalf("TTLSecondsAfterFinished = %v, want 120", job.Spec.TTLSecondsAfterFinished)
		}
		if spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Fatalf("RestartPolicy = %q, want Never", spec.RestartPolicy)
		}
	})

	t.Run("runs the files entrypoint with the op as arguments", func(t *testing.T) {
		c := spec.Containers[0]
		if len(c.Command) != 2 || c.Command[0] != felisBinaryPath || c.Command[1] != "files" {
			t.Fatalf("command = %v, want [%s files]", c.Command, felisBinaryPath)
		}
		args := strings.Join(c.Args, " ")
		for _, want := range []string{"--op read", "--path server.properties", "--worlds-root /data"} {
			if !strings.Contains(args, want) {
				t.Fatalf("args %q missing %q", args, want)
			}
		}
	})
}

// TestFilesJobWorldMountIsReadOnlyExceptForWrite pins the guarantee that only a
// write can mutate a world. For list and read the kernel refuses the write, not
// merely the code — a defence that survives a bug in the entrypoint.
func TestFilesJobWorldMountIsReadOnlyExceptForWrite(t *testing.T) {
	cases := []struct {
		op           string
		wantReadOnly bool
	}{
		{OpList, true},
		{OpRead, true},
		{OpWrite, false},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			job, err := FilesJob(testParams(tc.op))
			if err != nil {
				t.Fatalf("FilesJob: %v", err)
			}
			spec := job.Spec.Template.Spec
			gotMount := spec.Containers[0].VolumeMounts[0].ReadOnly
			gotVol := spec.Volumes[0].PersistentVolumeClaim.ReadOnly
			if gotMount != tc.wantReadOnly || gotVol != tc.wantReadOnly {
				t.Fatalf("op %s: mount.readOnly=%v volume.readOnly=%v, want %v",
					tc.op, gotMount, gotVol, tc.wantReadOnly)
			}
		})
	}
}

// TestFilesJobContentEnv pins the write channel: content rides the Job spec
// base64-encoded, and ONLY for a write — a list or read Job spec must carry no
// caller content at all.
func TestFilesJobContentEnv(t *testing.T) {
	t.Run("write carries base64 content", func(t *testing.T) {
		p := testParams(OpWrite)
		p.Content = []byte("motd=hello\n\x00\xff")
		job, err := FilesJob(p)
		if err != nil {
			t.Fatalf("FilesJob: %v", err)
		}
		env := job.Spec.Template.Spec.Containers[0].Env
		if len(env) != 1 || env[0].Name != ContentEnv {
			t.Fatalf("env = %+v, want exactly %s", env, ContentEnv)
		}
		got, err := base64.StdEncoding.DecodeString(env[0].Value)
		if err != nil {
			t.Fatalf("env value is not base64: %v", err)
		}
		if string(got) != string(p.Content) {
			t.Fatalf("decoded %q, want %q — arbitrary bytes must survive", got, p.Content)
		}
		// The content must never leak into argv, which is world-readable on the node.
		if strings.Contains(strings.Join(job.Spec.Template.Spec.Containers[0].Args, " "), "motd=hello") {
			t.Fatal("content must not appear in the container arguments")
		}
	})

	for _, op := range []string{OpList, OpRead} {
		t.Run(op+" carries no content env", func(t *testing.T) {
			job, err := FilesJob(testParams(op))
			if err != nil {
				t.Fatalf("FilesJob: %v", err)
			}
			if env := job.Spec.Template.Spec.Containers[0].Env; len(env) != 0 {
				t.Fatalf("env = %+v, want none for a %s", env, op)
			}
		})
	}
}

// TestFilesJobNameIsPerInvocation is the RBAC-forced property documented on
// FilesJobName. felis-api holds jobs:create and NOTHING else — no jobs:delete — so
// a deterministic name would let the first completed Job squat it for a whole TTL
// window and wedge every subsequent operation. Two operations on the same server
// must therefore never collide.
func TestFilesJobNameIsPerInvocation(t *testing.T) {
	a := testParams(OpRead)
	b := testParams(OpRead)
	b.OpID = "deadbeefcafe0002"

	ja, err := FilesJob(a)
	if err != nil {
		t.Fatalf("FilesJob: %v", err)
	}
	jb, err := FilesJob(b)
	if err != nil {
		t.Fatalf("FilesJob: %v", err)
	}
	if ja.Name == jb.Name {
		t.Fatalf("two operations on one server share the Job name %q — the editor would wedge", ja.Name)
	}
	if !strings.Contains(ja.Name, "survival") || !strings.Contains(ja.Name, a.OpID) {
		t.Fatalf("job name %q should carry the server and the op id", ja.Name)
	}
	// The op id must also label the Pod, or the runner could not select THIS
	// operation's Pod to read its result from.
	if got := ja.Spec.Template.ObjectMeta.Labels[LabelOpID]; got != a.OpID {
		t.Fatalf("pod label %s = %q, want %q", LabelOpID, got, a.OpID)
	}
}

// TestFilesJobRejectsBadParams checks the renderer fails loudly rather than
// producing a Job that cannot run or that would be refused by etcd.
func TestFilesJobRejectsBadParams(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*JobParams)
	}{
		{"no image", func(p *JobParams) { p.Image = "" }},
		{"no world PVC", func(p *JobParams) { p.WorldPVC = "" }},
		{"no op id", func(p *JobParams) { p.OpID = "" }},
		{"unknown op", func(p *JobParams) { p.Op = "delete" }},
		{"oversized content", func(p *JobParams) {
			p.Op, p.Content = OpWrite, make([]byte, MaxWriteBytes+1)
		}},
		{"bad cpu limit", func(p *JobParams) { p.CPULimit = "half" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testParams(OpRead)
			tc.mutate(&p)
			if _, err := FilesJob(p); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
