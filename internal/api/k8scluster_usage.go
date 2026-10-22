package api

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (k *K8sCluster) ReadPodUsage(ctx context.Context) (map[string]UsagePoint, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "metrics.k8s.io", Version: "v1beta1", Kind: "PodMetricsList"})
	if err := k.c.List(ctx, list, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	points := make(map[string]UsagePoint, len(list.Items))
	for _, item := range list.Items {
		var metric struct {
			Timestamp  metav1.Time `json:"timestamp"`
			Containers []struct {
				Usage corev1.ResourceList `json:"usage"`
			} `json:"containers"`
		}
		raw, err := json.Marshal(item.Object)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &metric); err != nil {
			return nil, fmt.Errorf("pod usage %s: %w", item.GetName(), err)
		}
		if metric.Timestamp.IsZero() || len(metric.Containers) == 0 {
			continue
		}
		point := UsagePoint{At: metric.Timestamp.Time.UTC()}
		for _, container := range metric.Containers {
			point.CPUMilli += container.Usage.Cpu().MilliValue()
			point.MemoryBytes += container.Usage.Memory().Value()
		}
		points[item.GetName()] = point
	}
	return points, nil
}
