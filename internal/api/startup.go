package api

import (
	"fmt"
	"slices"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

type StartupStatus struct {
	Stage         string `json:"stage"`
	Reason        string `json:"reason,omitempty"`
	Message       string `json:"message,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	LogsAvailable bool   `json:"logsAvailable"`
}

func startupStatus(ms *v1alpha1.MinecraftServer, pods []corev1.Pod) *StartupStatus {
	s := &StartupStatus{Stage: "creating"}
	if ms.Status.Phase == v1alpha1.PhaseFailed {
		s.Stage = "failed"
	}
	if ms.Status.StartRequestedAt != nil {
		s.StartedAt = ms.Status.StartRequestedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if c := meta.FindStatusCondition(ms.Status.Conditions, v1alpha1.ConditionReady); c != nil {
		s.Reason, s.Message = c.Reason, c.Message
	}
	var p *corev1.Pod
	for i := range pods {
		if pods[i].DeletionTimestamp == nil && (p == nil || pods[i].CreationTimestamp.After(p.CreationTimestamp.Time)) {
			p = &pods[i]
		}
	}
	if p == nil {
		if ms.Status.Phase == v1alpha1.PhaseRunning {
			s.Stage, s.Reason, s.Message = "failed", "GamePodMissing", "game Pod is missing; recorded Running status is no longer confirmed"
		}
		return s
	}
	s.Stage = "preparing"
	if p.Status.Phase == corev1.PodFailed {
		s.Stage, s.Reason, s.Message = "failed", p.Status.Reason, p.Status.Message
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && (c.Status == corev1.ConditionUnknown || (ms.Status.Phase == v1alpha1.PhaseRunning && c.Status == corev1.ConditionFalse && p.Status.Phase == corev1.PodRunning)) {
			s.Stage, s.Reason, s.Message = "failed", c.Reason, c.Message
			return s
		}
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			s.Stage, s.Reason, s.Message = "scheduling", c.Reason, c.Message
			return s
		}
	}
	s.LogsAvailable = p.Status.Phase == corev1.PodRunning
	for _, c := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
		if c.State.Waiting != nil {
			switch c.State.Waiting.Reason {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "RunContainerError":
				s.Stage, s.Reason, s.Message = "failed", c.State.Waiting.Reason, c.State.Waiting.Message
			default:
				if s.Stage != "failed" {
					s.Reason, s.Message = c.State.Waiting.Reason, c.State.Waiting.Message
				}
			}
		}
		if c.State.Terminated != nil && (c.State.Terminated.ExitCode != 0 || c.Name == serverLogContainer) {
			s.Stage, s.Reason, s.Message = "failed", c.State.Terminated.Reason, c.State.Terminated.Message
			if s.Message == "" {
				s.Message = fmt.Sprintf("Container %s exited with code %d", c.Name, c.State.Terminated.ExitCode)
			}
		}
		if c.Name == serverLogContainer && c.State.Running != nil {
			if s.Stage != "failed" {
				s.Stage = "booting"
			}
		}
	}
	return s
}
