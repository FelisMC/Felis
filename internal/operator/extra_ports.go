package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const extraPortsRetry = 10 * time.Second

var errExtraPortsPending = errors.New("extra port resources have not finished changing")
var errInvalidExtraPorts = errors.New("extra ports are invalid")

func extraPortsName(server *v1alpha1.MinecraftServer) string {
	sum := sha256.Sum256([]byte(string(server.UID) + "\x00" + server.Namespace + "\x00" + server.Name))
	return "extras-" + hex.EncodeToString(sum[:8])
}

func extraPortsSelector(server *v1alpha1.MinecraftServer) map[string]string {
	return map[string]string{v1alpha1.LabelServer: server.Name, v1alpha1.LabelComponent: ComponentValue}
}

func controlsExtraResource(server *v1alpha1.MinecraftServer, object client.Object) bool {
	owner := metav1.GetControllerOf(object)
	return owner != nil && owner.UID == server.UID && owner.Name == server.Name && owner.Kind == "MinecraftServer" && owner.APIVersion == v1alpha1.GroupVersion.String()
}

func (r *Reconciler) extraResources() client.Reader {
	if r.ExtraResources != nil {
		return r.ExtraResources
	}
	return r.Client
}

func buildExtraService(server *v1alpha1.MinecraftServer, existing *corev1.Service) *corev1.Service {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: extraPortsName(server), Namespace: server.Namespace, Labels: labelsFor(server)},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Selector: extraPortsSelector(server), ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal}}
	for _, port := range server.Spec.ExtraPorts {
		nodePort := int32(0)
		if existing != nil {
			for _, allocated := range existing.Spec.Ports {
				if allocated.Protocol == port.Protocol && allocated.Port == port.Port {
					nodePort = allocated.NodePort
				}
			}
		}
		service.Spec.Ports = append(service.Spec.Ports, corev1.ServicePort{
			Name:     strings.ToLower(string(port.Protocol)) + "-" + fmt.Sprint(port.Port),
			Protocol: port.Protocol, Port: port.Port, TargetPort: intstr.FromInt32(port.Port), NodePort: nodePort,
		})
	}
	return service
}

func buildExtraPolicy(server *v1alpha1.MinecraftServer) *networkingv1.NetworkPolicy {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: extraPortsName(server), Namespace: server.Namespace, Labels: labelsFor(server)},
		Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: extraPortsSelector(server)}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}}
	ports := make([]networkingv1.NetworkPolicyPort, 0, len(server.Spec.ExtraPorts))
	for _, requested := range server.Spec.ExtraPorts {
		protocol, port := requested.Protocol, intstr.FromInt32(requested.Port)
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &port})
	}
	policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{Ports: ports}}
	return policy
}

func (r *Reconciler) reconcileExtraPorts(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	allocated, err := r.syncExtraPorts(ctx, server)
	status, reason, message := metav1.ConditionTrue, "Ready", "extra ports are ready"
	if err != nil {
		allocated = nil
		status, reason, message = metav1.ConditionFalse, "ApplyFailed", "extra ports could not be applied; reconciliation will retry"
		if errors.Is(err, errInvalidExtraPorts) {
			reason, message = "InvalidExtraPorts", "extra ports are invalid and cannot be exposed"
		} else if errors.Is(err, errExtraPortsPending) {
			reason, message = "Pending", "waiting for extra port resources to finish changing"
		}
	}
	previous := meta.FindStatusCondition(server.Status.Conditions, v1alpha1.ConditionExtraPortsReady)
	changed := !slices.Equal(server.Status.ExtraPorts, allocated) || previous == nil || previous.Status != status || previous.Reason != reason || previous.ObservedGeneration != server.Generation
	if !changed {
		return err
	}
	server.Status.ExtraPorts = allocated
	r.setCondition(server, v1alpha1.ConditionExtraPortsReady, status, reason, message)
	return errors.Join(err, r.patchStatus(ctx, server))
}

func (r *Reconciler) syncExtraPorts(ctx context.Context, server *v1alpha1.MinecraftServer) ([]v1alpha1.AllocatedExtraPort, error) {
	if err := v1alpha1.ValidateExtraPorts(server.Spec); err != nil {
		return nil, errors.Join(errInvalidExtraPorts, err, r.clearExtraPorts(ctx, server))
	}
	if len(server.Spec.ExtraPorts) == 0 {
		return nil, r.clearExtraPorts(ctx, server)
	}
	if server.UID == "" {
		return nil, errors.New("extra ports require the server's persistent identity")
	}
	var old corev1.Service
	key := client.ObjectKey{Namespace: server.Namespace, Name: extraPortsName(server)}
	err := r.extraResources().Get(ctx, key, &old)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	existing := &old
	if apierrors.IsNotFound(err) {
		existing = nil
	} else if !controlsExtraResource(server, &old) {
		return nil, errors.New("the extra port Service belongs to another owner")
	} else if old.DeletionTimestamp != nil {
		return nil, errExtraPortsPending
	}
	desired := buildExtraService(server, existing)
	if err := controllerutil.SetControllerReference(server, desired, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.applyExtraPolicy(ctx, server); err != nil {
		return nil, err
	}
	if existing == nil {
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
	} else {
		updated := existing.DeepCopy()
		updated.Labels, updated.OwnerReferences = desired.Labels, desired.OwnerReferences
		updated.Spec.Type, updated.Spec.Selector, updated.Spec.Ports = desired.Spec.Type, desired.Spec.Selector, desired.Spec.Ports
		updated.Spec.ExternalTrafficPolicy, updated.Spec.ExternalIPs = desired.Spec.ExternalTrafficPolicy, nil
		if !equality.Semantic.DeepEqual(existing, updated) {
			if err := r.Update(ctx, updated); err != nil {
				return nil, err
			}
		}
		desired = updated
	}
	allocated := make([]v1alpha1.AllocatedExtraPort, 0, len(desired.Spec.Ports))
	for _, port := range desired.Spec.Ports {
		if port.NodePort < 1 || port.NodePort > 65535 {
			return nil, errExtraPortsPending
		}
		allocated = append(allocated, v1alpha1.AllocatedExtraPort{Protocol: port.Protocol, Port: port.Port, NodePort: port.NodePort})
	}
	return allocated, nil
}

func (r *Reconciler) applyExtraPolicy(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	desired := buildExtraPolicy(server)
	if err := controllerutil.SetControllerReference(server, desired, r.Scheme); err != nil {
		return err
	}
	var existing networkingv1.NetworkPolicy
	err := r.extraResources().Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !controlsExtraResource(server, &existing) {
		return errors.New("the extra port NetworkPolicy belongs to another owner")
	}
	if existing.DeletionTimestamp != nil {
		return errExtraPortsPending
	}
	updated := existing.DeepCopy()
	updated.Labels, updated.OwnerReferences, updated.Spec = desired.Labels, desired.OwnerReferences, desired.Spec
	if equality.Semantic.DeepEqual(&existing, updated) {
		return nil
	}
	return r.Update(ctx, updated)
}

func (r *Reconciler) clearExtraPorts(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	metadata := metav1.ObjectMeta{Name: extraPortsName(server), Namespace: server.Namespace}
	serviceErr := r.deleteExtraResource(ctx, server, &corev1.Service{ObjectMeta: metadata})
	policyErr := r.deleteExtraResource(ctx, server, &networkingv1.NetworkPolicy{ObjectMeta: metadata})
	return errors.Join(serviceErr, policyErr)
}

func (r *Reconciler) deleteExtraResource(ctx context.Context, server *v1alpha1.MinecraftServer, object client.Object) error {
	key := client.ObjectKeyFromObject(object)
	if err := r.extraResources().Get(ctx, key, object); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !controlsExtraResource(server, object) {
		return errors.New("an extra port resource belongs to another owner")
	}
	if object.GetDeletionTimestamp() == nil {
		uid, version := object.GetUID(), object.GetResourceVersion()
		if err := r.Delete(ctx, object, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if err := r.extraResources().Get(ctx, key, object); err != nil {
		return client.IgnoreNotFound(err)
	}
	return errExtraPortsPending
}
