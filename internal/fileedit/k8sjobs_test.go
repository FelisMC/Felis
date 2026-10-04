package fileedit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestRunReadsResultBeforePodTermination(t *testing.T) {
	for _, op := range []string{OpList, OpRead} {
		t.Run(op, func(t *testing.T) {
			closed := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"files-test"}}`)
				case strings.HasSuffix(r.URL.Path, "/pods"):
					if !strings.Contains(r.URL.Query().Get("labelSelector"), LabelOpID+"=") {
						t.Error("pod lookup did not select this operation")
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"file-pod"},"status":{"phase":"Running","containerStatuses":[{"name":"files","state":{"running":{}}}]}}]}`)
				case strings.HasSuffix(r.URL.Path, "/log"):
					if r.URL.Query().Get("follow") != "true" {
						t.Error("result log was not streamed")
					}
					io.WriteString(w, "diagnostic line\n"+ResultPrefix+`{"entries":[]}`+"\n")
					w.(http.Flusher).Flush()
					// The log stays open and the Pod stays Running. Run must return
					// on the result line and close the stream, not await either EOF.
					<-r.Context().Done()
					close(closed)
				default:
					t.Errorf("unexpected cluster call: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			got, err := NewK8sRunner(cs).Run(ctx, testParams(op))
			if err != nil || string(got) != `{"entries":[]}` {
				t.Fatalf("Run = %s, %v", got, err)
			}
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("result stream was not closed")
			}
		})
	}
}

func TestAwaitPodKeepsWritesWaitingForTermination(t *testing.T) {
	p := testParams(OpWrite)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "file-pod", Namespace: p.Namespace, Labels: filesLabels(p)},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: containerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if got, err := NewK8sRunner(fake.NewSimpleClientset(pod)).awaitPod(ctx, p); err == nil || got != nil {
		t.Fatalf("a running writer was treated as finished: %v, %v", got, err)
	}
}

var (
	opCreated = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	opEnded   = opCreated.Add(3 * time.Minute)
)

func asyncJob(conds ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "files-survival-0a",
			CreationTimestamp: metav1.NewTime(opCreated),
			Labels:            map[string]string{LabelOpID: "0a", LabelMode: OpUnzip},
			Annotations:       map[string]string{AnnotationPath: "maps/world.zip"},
		},
		Status: batchv1.JobStatus{Conditions: conds},
	}
}

func cond(typ batchv1.JobConditionType, status corev1.ConditionStatus, reason string) batchv1.JobCondition {
	return batchv1.JobCondition{Type: typ, Status: status, Reason: reason, LastTransitionTime: metav1.NewTime(opEnded)}
}

// TestOpState pins how a background Job and the tail of its log read as an
// OpState: the printed result decides the outcome whatever the Job's condition
// says, and a Job that ended without one failed for the condition's reason.
func TestOpState(t *testing.T) {
	progress := ProgressPrefix + `{"done":10,"total":100}` + "\n" +
		"a stderr line\n" +
		ProgressPrefix + `{"done":40,"total":100}` + "\n"
	ok := ResultPrefix + `{"files":3,"bytes":40}` + "\n"
	conflict := ResultPrefix + `{"code":"exists","conflicts":["a.txt"],"conflict_count":1}` + "\n"
	complete := cond(batchv1.JobComplete, corev1.ConditionTrue, "")
	backoff := cond(batchv1.JobFailed, corev1.ConditionTrue, "BackoffLimitExceeded")
	killed := func(container, reason string) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			Name: container, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: reason}},
		}}}}
	}

	cases := []struct {
		name       string
		conds      []batchv1.JobCondition
		pod        *corev1.Pod
		log        string
		state      string
		reason     string
		code       string
		files      int
		done       int64
		finished   bool
		wantResult bool
	}{
		{name: "running, at its latest progress", log: progress, state: OpRunning, done: 40},
		{name: "running, before any progress", state: OpRunning},
		{name: "a condition not yet true is still running",
			conds: []batchv1.JobCondition{cond(batchv1.JobFailed, corev1.ConditionFalse, "DeadlineExceeded")},
			log:   progress, state: OpRunning, done: 40},
		{name: "complete with a clean result",
			conds: []batchv1.JobCondition{complete}, log: progress + ok,
			state: OpSucceeded, files: 3, done: 40, finished: true, wantResult: true},
		{name: "success criteria met before complete",
			conds: []batchv1.JobCondition{cond(batchv1.JobSuccessCriteriaMet, corev1.ConditionTrue, "")}, log: ok,
			state: OpSucceeded, files: 3, finished: true, wantResult: true},
		{name: "killed at its deadline after printing a clean result",
			conds: []batchv1.JobCondition{cond(batchv1.JobFailed, corev1.ConditionTrue, "DeadlineExceeded")}, log: ok,
			state: OpSucceeded, files: 3, finished: true, wantResult: true},
		{name: "complete with a refusal",
			conds: []batchv1.JobCondition{complete}, log: conflict,
			state: OpFailed, code: CodeExists, finished: true, wantResult: true},
		{name: "killed at its deadline without a result",
			conds: []batchv1.JobCondition{cond(batchv1.JobFailed, corev1.ConditionTrue, "DeadlineExceeded")}, log: progress,
			state: OpFailed, reason: "DeadlineExceeded", done: 40, finished: true},
		{name: "failure target before failed",
			conds: []batchv1.JobCondition{cond(batchv1.JobFailureTarget, corev1.ConditionTrue, "BackoffLimitExceeded")},
			state: OpFailed, reason: "BackoffLimitExceeded", finished: true},
		{name: "failed without a reason",
			conds: []batchv1.JobCondition{cond(batchv1.JobFailed, corev1.ConditionTrue, "")},
			state: OpFailed, reason: "Failed", finished: true},
		{name: "complete but its log is gone",
			conds: []batchv1.JobCondition{complete},
			state: OpFailed, reason: "ResultUnavailable", finished: true},
		{name: "complete with a result that does not parse",
			conds: []batchv1.JobCondition{complete}, log: ResultPrefix + "{\n",
			state: OpFailed, reason: "ResultUnavailable", finished: true},
		{name: "killed for memory without a result",
			conds: []batchv1.JobCondition{backoff}, pod: killed(containerName, "OOMKilled"), log: progress,
			state: OpFailed, reason: "OOMKilled", done: 40, finished: true},
		{name: "killed for memory after printing a clean result",
			conds: []batchv1.JobCondition{backoff}, pod: killed(containerName, "OOMKilled"), log: ok,
			state: OpSucceeded, files: 3, finished: true, wantResult: true},
		{name: "killed another way",
			conds: []batchv1.JobCondition{backoff}, pod: killed(containerName, "Error"),
			state: OpFailed, reason: "BackoffLimitExceeded", finished: true},
		{name: "another container killed for memory",
			conds: []batchv1.JobCondition{backoff}, pod: killed("sidecar", "OOMKilled"),
			state: OpFailed, reason: "BackoffLimitExceeded", finished: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := opState(asyncJob(tc.conds...), tc.pod, tc.log)
			if st.ID != "0a" || st.Op != OpUnzip || st.Path != "maps/world.zip" || !st.Started.Equal(opCreated) {
				t.Fatalf("identity = %q %q %q %v", st.ID, st.Op, st.Path, st.Started)
			}
			if st.State != tc.state || st.Reason != tc.reason {
				t.Fatalf("state = %q reason %q, want %q reason %q", st.State, st.Reason, tc.state, tc.reason)
			}
			if st.Done != tc.done || (tc.done != 0 && st.Total != 100) {
				t.Fatalf("progress = %d/%d, want %d/100", st.Done, st.Total, tc.done)
			}
			if want := map[bool]time.Time{true: opEnded}[tc.finished]; !st.Finished.Equal(want) {
				t.Fatalf("finished = %v, want %v", st.Finished, want)
			}
			if (st.Result != nil) != tc.wantResult {
				t.Fatalf("result = %+v, want one: %v", st.Result, tc.wantResult)
			}
			if st.Result != nil && (st.Result.Code != tc.code || st.Result.Files != tc.files) {
				t.Fatalf("result = %+v, want code %q and %d files", st.Result, tc.code, tc.files)
			}
		})
	}
}

// TestK8sRunnerOps checks what Ops lists: this server's background Jobs only,
// newest first and at most maxOps of them, with a log read for each started Pod
// and none for a Pod still waiting to run.
func TestK8sRunnerOps(t *testing.T) {
	job := func(server, id string, age time.Duration, async bool) *batchv1.Job {
		p := testParams(OpUnzip)
		p.Server, p.OpID, p.Path, p.Async = server, id, "maps/"+id+".zip", async
		j, err := FilesJob(p)
		if err != nil {
			t.Fatal(err)
		}
		j.CreationTimestamp = metav1.NewTime(opCreated.Add(-age))
		return j
	}
	pod := func(j *batchv1.Job, phase corev1.PodPhase, age time.Duration) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-%d", j.Name, age), Namespace: "minecraft", Labels: j.Spec.Template.Labels,
				CreationTimestamp: metav1.NewTime(opCreated.Add(-age)),
			},
			Status: corev1.PodStatus{Phase: phase},
		}
	}

	var objs []runtime.Object
	for i := range maxOps + 2 {
		j := job("survival", fmt.Sprintf("%02d", i), time.Duration(i)*time.Minute, true)
		objs = append(objs, j, pod(j, corev1.PodSucceeded, time.Duration(i)*time.Minute))
	}
	// The newest Job's newest Pod has not started, so its log is not read; the
	// older Pod beside it is not the one Ops reports on.
	newest := job("survival", "new", -time.Minute, true)
	objs = append(objs, newest,
		pod(newest, corev1.PodPending, -time.Minute), pod(newest, corev1.PodFailed, 0))
	objs = append(objs,
		job("creative", "other", -2*time.Minute, true),
		job("survival", "sync", -3*time.Minute, false))
	cs := fake.NewSimpleClientset(objs...)

	ops, err := NewK8sRunner(cs).Ops(context.Background(), "minecraft", "survival")
	if err != nil {
		t.Fatalf("Ops: %v", err)
	}
	var ids []string
	for _, op := range ops {
		ids = append(ids, op.ID)
	}
	want := []string{"new", "00", "01", "02", "03", "04", "05", "06", "07", "08"}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("ops = %v, want %v", ids, want)
	}
	if ops[0].Path != "maps/new.zip" || ops[0].Op != OpUnzip {
		t.Fatalf("newest op = %+v", ops[0])
	}

	logs := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "get" && a.GetSubresource() == "log" {
			logs++
			opts := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
			if opts.Container != containerName || opts.TailLines == nil || *opts.TailLines != opLogLines {
				t.Fatalf("log options = %+v", opts)
			}
		}
	}
	if logs != maxOps-1 {
		t.Fatalf("read %d logs, want %d: one per listed op whose Pod has started", logs, maxOps-1)
	}
}

// TestK8sRunnerOpsReadsAMemoryKill checks Ops hands each Job's Pod to opState: a
// Pod the kernel killed for memory is what names an op's reason OOMKilled.
func TestK8sRunnerOpsReadsAMemoryKill(t *testing.T) {
	p := testParams(OpUnzip)
	p.Server, p.OpID, p.Path, p.Async = "survival", "oom", "maps/tiles.zip", true
	j, err := FilesJob(p)
	if err != nil {
		t.Fatal(err)
	}
	j.Status.Conditions = []batchv1.JobCondition{cond(batchv1.JobFailed, corev1.ConditionTrue, "BackoffLimitExceeded")}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-x", Namespace: "minecraft", Labels: j.Spec.Template.Labels},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			Name: containerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
		}}},
	}
	ops, err := NewK8sRunner(fake.NewSimpleClientset(j, pod)).Ops(context.Background(), "minecraft", "survival")
	if err != nil || len(ops) != 1 || ops[0].State != OpFailed || ops[0].Reason != "OOMKilled" {
		t.Fatalf("Ops = %+v, %v; want the one op failed for OOMKilled", ops, err)
	}
}

// TestK8sRunnerOpsFailsLoudly checks a listing the cluster refused is an error,
// never an empty list that would read as nothing running.
func TestK8sRunnerOpsFailsLoudly(t *testing.T) {
	for _, resource := range []string{"jobs", "pods"} {
		t.Run(resource, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("forbidden")
			})
			ops, err := NewK8sRunner(cs).Ops(context.Background(), "minecraft", "survival")
			if err == nil || ops != nil {
				t.Fatalf("Ops = %v, %v; want the refusal", ops, err)
			}
		})
	}
}

// TestK8sRunnerStart checks Start creates the rendered Job and nothing else,
// and refuses params the renderer refuses without touching the cluster.
func TestK8sRunnerStart(t *testing.T) {
	cs := fake.NewSimpleClientset()
	p := testParams(OpUnzip)
	p.Path, p.Async = "maps/world.zip", true
	if err := NewK8sRunner(cs).Start(context.Background(), p); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want, _ := FilesJob(p)
	got, err := cs.BatchV1().Jobs("minecraft").Get(context.Background(), want.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the Job was not created: %v", err)
	}
	if got.Labels[LabelAsync] != "true" || got.Annotations[AnnotationPath] != "maps/world.zip" {
		t.Fatalf("created %+v", got.ObjectMeta)
	}
	if n := len(cs.Actions()); n != 2 { // the create, and this test's get
		t.Fatalf("%d calls to the cluster, want the one create", n-1)
	}

	cs = fake.NewSimpleClientset()
	p.Op = OpRead
	if err := NewK8sRunner(cs).Start(context.Background(), p); err == nil || len(cs.Actions()) != 0 {
		t.Fatalf("err %v after %d calls, want a refusal before any", err, len(cs.Actions()))
	}

	cs = fake.NewSimpleClientset()
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("quota exceeded")
	})
	p.Op = OpUnzip
	if err := NewK8sRunner(cs).Start(context.Background(), p); err == nil {
		t.Fatal("a refused create must be an error")
	}
}
