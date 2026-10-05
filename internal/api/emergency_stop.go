package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EmergencyStop records the stop intent before scaling the owned workload. It
// bypasses RCON and operator reconciliation, retaining normal Pod shutdown grace.
func (k *K8sCluster) EmergencyStop(ctx context.Context, name string) error {
	if err := k.SetDesiredState(ctx, name, v1alpha1.DesiredStopped); err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.getServer(ctx, name, &ms); err != nil {
			return err
		}
		if ms.Spec.DesiredState != v1alpha1.DesiredStopped {
			return newError(http.StatusConflict, "conflict", "stop intent changed; retry emergency stop")
		}
		var sts appsv1.StatefulSet
		if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &sts); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		owner := metav1.GetControllerOf(&sts)
		if owner == nil || ms.UID == "" || owner.UID != ms.UID || owner.Kind != "MinecraftServer" || owner.APIVersion != v1alpha1.GroupVersion.String() {
			return fmt.Errorf("stop intent saved, but workload ownership could not be verified")
		}
		scale := &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.namespace, ResourceVersion: sts.ResourceVersion}, Spec: autoscalingv1.ScaleSpec{Replicas: 0}}
		if err := k.c.SubResource("scale").Update(ctx, &sts, client.WithSubResourceBody(scale)); err != nil {
			return fmt.Errorf("stop intent saved, but workload scale-down was not confirmed: %w", err)
		}
		return nil
	})
}

func (a *API) handleEmergencyStop(w http.ResponseWriter, r *http.Request) {
	if !a.requireReauth(w, r, principalFromContext(r.Context())) {
		return
	}
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name"))
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeError(w, r, err)
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Confirm != name {
		writeError(w, r, newError(http.StatusBadRequest, "bad_request", "type the exact server name to confirm"))
		return
	}
	stopper, ok := a.Cluster.(interface {
		EmergencyStop(context.Context, string) error
	})
	if !ok {
		writeError(w, r, newError(http.StatusServiceUnavailable, "unavailable", "this cluster does not support direct emergency shutdown"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := stopper.EmergencyStop(ctx, name); err != nil {
		a.audit(r, "emergency_stop.failed", name)
		a.writeLookupError(w, r, err)
		return
	}
	a.audit(r, "emergency_stop", name)
	writeJSON(w, http.StatusAccepted, map[string]any{"name": name, "desiredState": "Stopped"})
}
