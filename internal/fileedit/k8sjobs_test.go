package fileedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	for _, op := range []string{OpList, OpRead, OpDiskUsage} {
		t.Run(op, func(t *testing.T) {
			want := `{"entries":[]}`
			if op == OpDiskUsage {
				want = `{"used_bytes":4096}`
			}
			closed := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs"):
					if op != OpDiskUsage {
						t.Error("a regular file operation checked the disk sampling queue")
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"apiVersion":"batch/v1","kind":"JobList","items":[]}`)
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
					io.WriteString(w, "diagnostic line\n"+ResultPrefix+want+"\n")
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
			runner := NewK8sRunner(cs)
			if op == OpDiskUsage {
				runner.Browser = &Browser{}
			}
			got, err := runner.Run(ctx, opParams(op))
			if err != nil || string(got) != want {
				t.Fatalf("Run = %s, %v", got, err)
			}
			if runner.Browser != nil && len(runner.Browser.sessions) != 0 {
				t.Fatal("disk sampling consumed a browser slot")
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

func diskQueueJob(t *testing.T, status batchv1.JobStatus) *batchv1.Job {
	t.Helper()
	p := opParams(OpDiskUsage)
	p.Server, p.OpID, p.WorldPVC = "other-server", "previous-leader", "world-other-server-0"
	job, err := FilesJob(p)
	if err != nil {
		t.Fatal(err)
	}
	job.Status = status
	return job
}

func TestDiskSamplingWaitsForPreviousJobAcrossServers(t *testing.T) {
	terminating := int32(1)
	for _, tc := range []struct {
		name    string
		status  batchv1.JobStatus
		blocked bool
	}{
		{"pending before controller observation", batchv1.JobStatus{}, true},
		{"active", batchv1.JobStatus{Active: 1}, true},
		{"success count without terminal condition", batchv1.JobStatus{Succeeded: 1}, true},
		{"failure count without terminal condition", batchv1.JobStatus{Failed: 1}, true},
		{"success criteria met", batchv1.JobStatus{Conditions: []batchv1.JobCondition{cond(batchv1.JobSuccessCriteriaMet, corev1.ConditionTrue, "")}}, true},
		{"failure target", batchv1.JobStatus{Conditions: []batchv1.JobCondition{cond(batchv1.JobFailureTarget, corev1.ConditionTrue, "")}}, true},
		{"false complete condition", batchv1.JobStatus{Conditions: []batchv1.JobCondition{cond(batchv1.JobComplete, corev1.ConditionFalse, "")}}, true},
		{"complete with active pod", batchv1.JobStatus{Active: 1, Conditions: []batchv1.JobCondition{cond(batchv1.JobComplete, corev1.ConditionTrue, "")}}, true},
		{"failed with terminating pod", batchv1.JobStatus{Terminating: &terminating, Conditions: []batchv1.JobCondition{cond(batchv1.JobFailed, corev1.ConditionTrue, "")}}, true},
		{"complete", batchv1.JobStatus{Conditions: []batchv1.JobCondition{cond(batchv1.JobComplete, corev1.ConditionTrue, "")}}, false},
		{"failed", batchv1.JobStatus{Conditions: []batchv1.JobCondition{cond(batchv1.JobFailed, corev1.ConditionTrue, "")}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"Run", "Start"} {
				t.Run(method, func(t *testing.T) {
					cs := fake.NewSimpleClientset(diskQueueJob(t, tc.status))
					createReached := errors.New("create reached")
					cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, createReached
					})
					runner := NewK8sRunner(cs)
					p := opParams(OpDiskUsage)
					budget := time.Second
					if tc.blocked {
						budget = 10 * time.Millisecond
					}
					ctx, cancel := context.WithTimeout(context.Background(), budget)
					defer cancel()
					var payload []byte
					var err error
					if method == "Start" {
						err = runner.Start(ctx, p)
					} else {
						payload, err = runner.Run(ctx, p)
					}
					want := createReached
					if tc.blocked {
						want = context.DeadlineExceeded
					}
					if !errors.Is(err, want) || payload != nil {
						t.Fatalf("queue admission: payload=%s error=%v want=%v", payload, err, want)
					}
				})
			}
		})
	}
}

func TestDiskSamplingQueueIgnoresOtherNamespacesAndOperations(t *testing.T) {
	for _, alter := range []func(*batchv1.Job){
		func(job *batchv1.Job) { job.Namespace = "another-namespace" },
		func(job *batchv1.Job) { job.Labels[LabelMode] = OpRead },
		func(job *batchv1.Job) { job.Labels[LabelManagedBy] = "another-controller" },
	} {
		old := diskQueueJob(t, batchv1.JobStatus{Active: 1})
		alter(old)
		cs := fake.NewSimpleClientset(old)
		createReached := errors.New("create reached")
		cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, createReached
		})
		payload, err := NewK8sRunner(cs).Run(context.Background(), opParams(OpDiskUsage))
		if !errors.Is(err, createReached) || payload != nil {
			t.Fatalf("unrelated Job blocked disk sampling: payload=%s error=%v", payload, err)
		}
	}
}

func TestDiskSamplingQueueListFailureCreatesNoJob(t *testing.T) {
	cs := fake.NewSimpleClientset()
	unavailable := errors.New("job listing unavailable")
	cs.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, unavailable
	})
	payload, err := NewK8sRunner(cs).Run(context.Background(), opParams(OpDiskUsage))
	if !errors.Is(err, unavailable) || payload != nil {
		t.Fatalf("list failure: payload=%s error=%v", payload, err)
	}
	for _, action := range cs.Actions() {
		if action.GetVerb() == "create" {
			t.Fatal("an unverifiable queue admitted a new Job")
		}
	}
}

func TestDiskSamplingQueueChecksEveryJobPage(t *testing.T) {
	var pages atomic.Int32
	old := diskQueueJob(t, batchv1.JobStatus{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/batch/v1/namespaces/minecraft/jobs" {
			t.Error("queue inspection requested another resource")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		selector := r.URL.Query().Get("labelSelector")
		if strings.Contains(selector, LabelServer) || strings.Contains(selector, LabelOpID) || !strings.Contains(selector, LabelMode+"="+OpDiskUsage) || r.URL.Query().Get("limit") != "100" {
			t.Error("queue inspection did not cover every server with bounded pages")
		}
		page := pages.Add(1)
		job := old.DeepCopy()
		list := &batchv1.JobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "JobList"}}
		if page == 1 {
			job.Status.Conditions = []batchv1.JobCondition{cond(batchv1.JobComplete, corev1.ConditionTrue, "")}
			list.Continue = "next-page"
		} else if r.URL.Query().Get("continue") != "next-page" {
			t.Error("queue inspection omitted its continuation token")
		}
		list.Items = []batchv1.Job{*job}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
		if page == 2 {
			cancel()
		}
	}))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewK8sRunner(cs).checkDiskUsageJobs(ctx, "minecraft"); !errors.Is(err, context.Canceled) || pages.Load() != 2 {
		t.Fatalf("unfinished Job on a later page was missed: pages=%d err=%v", pages.Load(), err)
	}
}

func TestDiskSamplingLogFailureDoesNotOverlapNextLeader(t *testing.T) {
	var mu sync.Mutex
	var old *batchv1.Job
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs"):
			list := &batchv1.JobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "JobList"}, Items: []batchv1.Job{}}
			if old != nil {
				list.Items = append(list.Items, *old)
			}
			json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			job := &batchv1.Job{}
			if err := json.NewDecoder(r.Body).Decode(job); err != nil {
				t.Error("invalid Job body")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			old = job
			creates++
			job.TypeMeta = metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}
			json.NewEncoder(w).Encode(job)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			io.WriteString(w, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"sampling-pod"},"status":{"phase":"Running","containerStatuses":[{"name":"files","state":{"running":{}}}]}}]}`)
		case strings.HasSuffix(r.URL.Path, "/log"):
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Error("unexpected cluster request")
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
	p := opParams(OpDiskUsage)
	if payload, err := NewK8sRunner(cs).Run(ctx, p); err == nil || payload != nil {
		t.Fatal("unreadable result was treated as a successful sample")
	}
	p.Server, p.OpID, p.WorldPVC = "another-server", "new-leader", "world-another-server-0"
	wait, release := context.WithTimeout(ctx, 10*time.Millisecond)
	if payload, err := NewK8sRunner(cs).Run(wait, p); !errors.Is(err, context.DeadlineExceeded) || payload != nil {
		release()
		t.Fatalf("replacement leader overlapped an unfinished Job: payload=%s err=%v", payload, err)
	}
	release()
	mu.Lock()
	if creates != 1 || old == nil {
		mu.Unlock()
		t.Fatal("more than one sampling Job was created before termination")
	}
	old.Status.Conditions = []batchv1.JobCondition{cond(batchv1.JobFailed, corev1.ConditionTrue, "")}
	mu.Unlock()
	if payload, err := NewK8sRunner(cs).Run(ctx, p); err == nil || errors.Is(err, context.DeadlineExceeded) || payload != nil {
		t.Fatalf("finished Job prevented the next attempt: payload=%s err=%v", payload, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 2 {
		t.Fatalf("created %d Jobs after the first terminated, want 2", creates)
	}
}

func TestDiskSamplingQueueContinuesAfterCrossServerTermination(t *testing.T) {
	old := diskQueueJob(t, batchv1.JobStatus{Active: 1})
	cs := fake.NewSimpleClientset(old.DeepCopy())
	observed := make(chan struct{})
	created := make(chan struct{}, 1)
	snapshot := &batchv1.JobList{Items: []batchv1.Job{*old.DeepCopy()}}
	first := true
	cs.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if first {
			first = false
			close(observed)
			return true, snapshot, nil
		}
		return false, nil, nil
	})
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		created <- struct{}{}
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- NewK8sRunner(cs).Start(ctx, opParams(OpDiskUsage)) }()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("the prior Job was not inspected")
	}
	select {
	case <-created:
		t.Fatal("a second Job was created before the first terminated")
	default:
	}
	old.Status.Active = 0
	old.Status.Conditions = []batchv1.JobCondition{cond(batchv1.JobComplete, corev1.ConditionTrue, "")}
	if _, err := cs.BatchV1().Jobs(old.Namespace).UpdateStatus(ctx, old, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("healthy queue did not advance after termination: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("healthy queue stalled after the first Job terminated")
	}
	select {
	case <-created:
	default:
		t.Fatal("the next server's sampling Job was not created")
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
