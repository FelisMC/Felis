package fileedit

import (
	"bytes"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// opParams is testParams with what each op needs to render.
func opParams(op string) JobParams {
	p := testParams(op)
	switch op {
	case OpRename:
		p.To = "server.properties.bak"
	case OpUpload:
		p.SourceURL, p.UploadToken = "http://felis-api-internal.felis.svc:8081/api/v1/internal/file-uploads/0a", "tok"
	}
	return p
}

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

	// The ops that create a file or folder hand it back to the game uid, so they
	// keep CHOWN; the rest stay at DAC_OVERRIDE alone.
	t.Run("only the creating ops keep CHOWN", func(t *testing.T) {
		for _, tc := range []struct {
			op    string
			chown bool
		}{
			{OpList, false}, {OpRead, false}, {OpDelete, false}, {OpRename, false},
			{OpWrite, true}, {OpMkdir, true}, {OpUpload, true}, {OpUnzip, true},
		} {
			j, err := FilesJob(opParams(tc.op))
			if err != nil {
				t.Fatalf("%s: FilesJob: %v", tc.op, err)
			}
			add := j.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add
			want := []corev1.Capability{"DAC_OVERRIDE"}
			if tc.chown {
				want = []corev1.Capability{"CHOWN", "DAC_OVERRIDE"}
			}
			if !slices.Equal(add, want) {
				t.Errorf("%s capabilities = %v, want %v", tc.op, add, want)
			}
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

// TestFilesJobExpectArg: a write's precondition hash reaches the entrypoint as a
// flag, and only a write carries one.
func TestFilesJobExpectArg(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for _, tc := range []struct {
		op     string
		expect string
		want   bool
	}{
		{OpWrite, sum, true},
		{OpWrite, "", false},
		{OpRead, sum, false},
	} {
		p := testParams(tc.op)
		p.Expect = tc.expect
		job, err := FilesJob(p)
		if err != nil {
			t.Fatalf("FilesJob: %v", err)
		}
		args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
		if got := strings.Contains(args, "--expect-sha256 "+sum); got != tc.want {
			t.Fatalf("op %s expect %q: args %q, want flag present = %v", tc.op, tc.expect, args, tc.want)
		}
	}
}

// TestFilesJobWorldMountIsReadOnlyForReads pins the guarantee that list and read
// cannot mutate a world. For them the kernel refuses the write, not
// merely the code — a defence that survives a bug in the entrypoint.
func TestFilesJobWorldMountIsReadOnlyForReads(t *testing.T) {
	cases := []struct {
		op           string
		wantReadOnly bool
	}{
		{OpList, true},
		{OpRead, true},
		{OpWrite, false},
		{OpMkdir, false},
		{OpDelete, false},
		{OpRename, false},
		{OpUpload, false},
		{OpUnzip, false},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			job, err := FilesJob(opParams(tc.op))
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

// envLookup reads a rendered container's environment the way the Job's process
// sees it.
func envLookup(env []corev1.EnvVar) func(string) (string, bool) {
	return func(name string) (string, bool) {
		for _, e := range env {
			if e.Name == name {
				return e.Value, true
			}
		}
		return "", false
	}
}

// TestFilesJobContentEnv pins the write channel: content rides the Job spec
// base64-encoded, and ONLY for a write — no other Job spec carries caller content.
func TestFilesJobContentEnv(t *testing.T) {
	t.Run("write carries the content, reassembled by the entrypoint", func(t *testing.T) {
		p := testParams(OpWrite)
		p.Content = []byte("motd=hello\n\x00\xff")
		job, err := FilesJob(p)
		if err != nil {
			t.Fatalf("FilesJob: %v", err)
		}
		env := job.Spec.Template.Spec.Containers[0].Env
		want := []corev1.EnvVar{
			{Name: ContentPartsEnv, Value: "1"},
			{Name: ContentEnv + "_0", Value: base64.StdEncoding.EncodeToString(p.Content)},
		}
		if !slices.Equal(env, want) {
			t.Fatalf("env = %+v, want %+v", env, want)
		}
		got, err := ContentFromEnv(envLookup(env))
		if err != nil || string(got) != string(p.Content) {
			t.Fatalf("reassembled %q, %v; want %q — arbitrary bytes must survive", got, err, p.Content)
		}
		// The content must never leak into argv, which is world-readable on the node.
		if strings.Contains(strings.Join(job.Spec.Template.Spec.Containers[0].Args, " "), "motd=hello") {
			t.Fatal("content must not appear in the container arguments")
		}
	})

	// execve refuses one environment string over 128 KiB and the container never
	// starts, so the largest write must arrive in parts each well under it.
	t.Run("the largest write is split under the kernel's per-variable limit", func(t *testing.T) {
		p := testParams(OpWrite)
		p.Content = make([]byte, MaxWriteBytes)
		for i := range p.Content {
			p.Content[i] = byte(i * 7)
		}
		job, err := FilesJob(p)
		if err != nil {
			t.Fatalf("FilesJob: %v", err)
		}
		env := job.Spec.Template.Spec.Containers[0].Env
		if len(env) != 1+maxContentParts || env[0].Value != strconv.Itoa(maxContentParts) {
			t.Fatalf("%d variables, count %q; want the count and %d parts", len(env), env[0].Value, maxContentParts)
		}
		for _, e := range env {
			if len(e.Value) > contentChunk || len(e.Name)+1+len(e.Value) >= 128<<10 {
				t.Fatalf("%s is %d bytes; each part must fit in %d", e.Name, len(e.Value), contentChunk)
			}
		}
		got, err := ContentFromEnv(envLookup(env))
		if err != nil || !bytes.Equal(got, p.Content) {
			t.Fatalf("reassembled %d bytes, %v; want the %d written", len(got), err, len(p.Content))
		}
	})

	t.Run("an empty write is zero parts", func(t *testing.T) {
		p := testParams(OpWrite)
		p.Content = []byte{}
		job, err := FilesJob(p)
		if err != nil {
			t.Fatalf("FilesJob: %v", err)
		}
		env := job.Spec.Template.Spec.Containers[0].Env
		if want := []corev1.EnvVar{{Name: ContentPartsEnv, Value: "0"}}; !slices.Equal(env, want) {
			t.Fatalf("env = %+v, want %+v", env, want)
		}
		if got, err := ContentFromEnv(envLookup(env)); err != nil || len(got) != 0 {
			t.Fatalf("reassembled %q, %v; want empty", got, err)
		}
	})

	for _, op := range []string{OpList, OpRead, OpMkdir, OpDelete, OpRename} {
		t.Run(op+" carries no env", func(t *testing.T) {
			job, err := FilesJob(opParams(op))
			if err != nil {
				t.Fatalf("FilesJob: %v", err)
			}
			if env := job.Spec.Template.Spec.Containers[0].Env; len(env) != 0 {
				t.Fatalf("env = %+v, want none for a %s", env, op)
			}
		})
	}
}

// TestFilesJobOpArgs pins what each op hands the entrypoint beyond --op and
// --path, and that the upload token rides the environment, never argv.
func TestFilesJobOpArgs(t *testing.T) {
	args := func(p JobParams) []string {
		t.Helper()
		j, err := FilesJob(p)
		if err != nil {
			t.Fatalf("FilesJob(%s): %v", p.Op, err)
		}
		return j.Spec.Template.Spec.Containers[0].Args[6:]
	}
	read, err := FilesJob(testParams(OpRead))
	if err != nil {
		t.Fatalf("FilesJob: %v", err)
	}
	if got, want := read.Spec.Template.Spec.Containers[0].Args, []string{"--op", "read", "--path", "server.properties", "--worlds-root", "/data"}; !slices.Equal(got, want) {
		t.Fatalf("read args = %v, want %v", got, want)
	}

	create := testParams(OpWrite)
	create.CreateOnly = true
	if got := args(create); !slices.Equal(got, []string{"--create-only"}) {
		t.Errorf("create-only write args = %v", got)
	}
	readCreate := testParams(OpRead)
	readCreate.CreateOnly = true
	if got := args(readCreate); len(got) != 0 {
		t.Errorf("a read carries write flags: %v", got)
	}
	if got := args(opParams(OpRename)); !slices.Equal(got, []string{"--to", "server.properties.bak"}) {
		t.Errorf("rename args = %v", got)
	}

	up := opParams(OpUpload)
	up.UploadSize, up.UploadSHA256 = 1234, strings.Repeat("c", 64)
	want := []string{"--source-url", up.SourceURL, "--size", "1234", "--sha256", strings.Repeat("c", 64)}
	if got := args(up); !slices.Equal(got, want) {
		t.Errorf("upload args = %v, want %v", got, want)
	}
	up.Overwrite = true
	if got := args(up); !slices.Equal(got, append(want, "--overwrite")) {
		t.Errorf("overwriting upload args = %v", got)
	}
	j, err := FilesJob(up)
	if err != nil {
		t.Fatalf("FilesJob: %v", err)
	}
	c := j.Spec.Template.Spec.Containers[0]
	if want := []corev1.EnvVar{{Name: UploadTokenEnv, Value: "tok"}}; !slices.Equal(c.Env, want) {
		t.Errorf("upload env = %+v, want %+v", c.Env, want)
	}
	if slices.Contains(c.Args, "tok") {
		t.Errorf("the upload token is in argv: %v", c.Args)
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
		{"unknown op", func(p *JobParams) { p.Op = "chmod" }},
		{"upload without a source", func(p *JobParams) { p.Op, p.UploadToken = OpUpload, "tok" }},
		{"upload without a token", func(p *JobParams) { p.Op, p.SourceURL = OpUpload, "http://x" }},
		{"oversized content", func(p *JobParams) {
			p.Op, p.Content = OpWrite, make([]byte, MaxWriteBytes+1)
		}},
		{"bad cpu limit", func(p *JobParams) { p.CPULimit = "half" }},
		{"a read in the background", func(p *JobParams) { p.Async = true }},
		{"a write in the background", func(p *JobParams) { p.Op, p.Async = OpWrite, true }},
		{"a delete in the background", func(p *JobParams) { p.Op, p.Async = OpDelete, true }},
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

// TestFilesJobAsync checks a background Job is marked so Ops finds it, on the
// Job and on its Pod, and carries the path Ops shows; a Job felis-api waits on
// carries neither, so Ops never reports it.
func TestFilesJobAsync(t *testing.T) {
	for _, op := range []string{OpUpload, OpUnzip} {
		t.Run(op, func(t *testing.T) {
			p := opParams(op)
			p.Path, p.Async = "maps/world.zip", true
			j, err := FilesJob(p)
			if err != nil {
				t.Fatalf("FilesJob: %v", err)
			}
			if j.Labels[LabelAsync] != "true" || j.Spec.Template.Labels[LabelAsync] != "true" {
				t.Fatalf("job labels %v, pod labels %v, want %s=true on both", j.Labels, j.Spec.Template.Labels, LabelAsync)
			}
			if len(j.Annotations) != 1 || j.Annotations[AnnotationPath] != "maps/world.zip" {
				t.Fatalf("annotations = %v, want only %s", j.Annotations, AnnotationPath)
			}

			p.Async = false
			j, err = FilesJob(p)
			if err != nil {
				t.Fatalf("FilesJob: %v", err)
			}
			_, onJob := j.Labels[LabelAsync]
			_, onPod := j.Spec.Template.Labels[LabelAsync]
			if onJob || onPod || j.Annotations != nil {
				t.Fatalf("a Job waited on is labelled %v / %v and annotated %v", j.Labels, j.Spec.Template.Labels, j.Annotations)
			}
		})
	}
}
