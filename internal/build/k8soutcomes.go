package build

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// maxScanLogBytes bounds the scan-gate log read: the base64 envelope plus the verdict lines.
const maxScanLogBytes = MaxEnvelopeBytes/3*4 + 1<<20

// K8sOutcomes reads pod statuses and scan-gate logs through the typed client, uncached, to avoid a cluster-wide pod informer.
type K8sOutcomes struct {
	cs        kubernetes.Interface
	namespace string
}

// NewK8sOutcomes reads build pods in cfg's namespace.
func NewK8sOutcomes(cs kubernetes.Interface, cfg Config) *K8sOutcomes {
	return &K8sOutcomes{cs: cs, namespace: cfg.withDefaults().Namespace}
}

// Return retryable API errors only; unreadable scan logs become ScanErr without preventing settlement.
func (k *K8sOutcomes) Outcome(ctx context.Context, buildID string) (Outcome, error) {
	var out Outcome
	job, err := k.cs.BatchV1().Jobs(k.namespace).Get(ctx, BuildJobName(buildID), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return out, err
	default:
		out.DeadlineExceeded = jobDeadlineExceeded(job)
	}
	pods, err := k.cs.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelBuildID + "=" + buildID})
	if err != nil {
		return out, err
	}
	if len(pods.Items) == 0 {
		return out, nil
	}
	// backoffLimit 0 and restartPolicy Never: the Job makes at most one pod.
	pod := &pods.Items[0]
	gateRan := false
	for _, st := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		t := st.State.Terminated
		if t == nil {
			continue
		}
		if st.Name == ContainerScanGate {
			gateRan = true
		}
		if t.ExitCode != 0 && out.FailedStep == "" {
			out.FailedStep, out.ExitCode, out.Message = st.Name, t.ExitCode, t.Message
		}
	}
	if !gateRan {
		return out, nil
	}
	limit := int64(maxScanLogBytes)
	stream, err := k.cs.CoreV1().Pods(k.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: ContainerScanGate, LimitBytes: &limit,
	}).Stream(ctx)
	if err != nil {
		out.ScanErr = fmt.Errorf("open the scan-gate log: %w", err)
		return out, nil
	}
	defer stream.Close()
	env, err := ReadScanEnvelope(stream)
	switch {
	case errors.Is(err, ErrNoScanEnvelope):
		out.ScanErr = errors.New("the scan-gate log holds no scan envelope")
	case err != nil:
		out.ScanErr = err
	default:
		out.Scan = env
	}
	return out, nil
}

// jobDeadlineExceeded reports a Job failed for exceeding activeDeadlineSeconds; the pod is deleted, so only the Job says so.
func jobDeadlineExceeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Reason == batchv1.JobReasonDeadlineExceeded {
			return true
		}
	}
	return false
}
