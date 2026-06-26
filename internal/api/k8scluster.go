package api

import (
	"context"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sCluster is the production Cluster backed by a controller-runtime client
// (spec §4). It reads the MinecraftServer CRD and performs the API's spec writes
// — the app-tier desiredState lever, the admin-tier create, and the admin-tier
// spec patch — each via a merge patch. It is integration-tested against a live
// cluster, not the hermetic api_test.go suite.
type K8sCluster struct {
	c         client.Client
	namespace string
}

// NewK8sCluster builds a Cluster over c, scoped to namespace.
func NewK8sCluster(c client.Client, namespace string) *K8sCluster {
	return &K8sCluster{c: c, namespace: namespace}
}

func (k *K8sCluster) GetServer(ctx context.Context, name string) (*ServerInfo, error) {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return serverInfo(&ms), nil
}

func (k *K8sCluster) GetBySubdomain(ctx context.Context, subdomain string) (*ServerInfo, error) {
	var list v1alpha1.MinecraftServerList
	if err := k.c.List(ctx, &list, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.Subdomain == subdomain {
			return serverInfo(&list.Items[i]), nil
		}
	}
	return nil, ErrNotFound
}

func (k *K8sCluster) ListServers(ctx context.Context) ([]ServerInfo, error) {
	var list v1alpha1.MinecraftServerList
	if err := k.c.List(ctx, &list, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	out := make([]ServerInfo, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, *serverInfo(&list.Items[i]))
	}
	return out, nil
}

// CreateServer creates a MinecraftServer CRD from the validated §15 form. The
// server starts DesiredState=Stopped (created cold, woken later) and unowned —
// ownership is established by a later claim (spec §9.3). felis-api has already
// guaranteed the §22 memory ceiling lives in in.Resources, so the operator
// never has to derive a cgroup limit from JavaMemory. An existing name maps to
// ErrConflict so the handler returns 409.
func (k *K8sCluster) CreateServer(ctx context.Context, in CreateServerInput) error {
	ms := &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Name,
			Namespace: k.namespace,
		},
		Spec: v1alpha1.MinecraftServerSpec{
			Subdomain:       in.Subdomain,
			DisplayName:     in.DisplayName,
			Image:           in.Image,
			JavaMemory:      in.JavaMemory,
			DesiredState:    v1alpha1.DesiredStopped,
			AutostartPolicy: in.AutostartPolicy,
			Storage:         v1alpha1.StorageSpec{Size: in.StorageSize},
			Resources:       in.Resources,
		},
	}
	if err := k.c.Create(ctx, ms); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

// SetDesiredState patches spec.desiredState with a merge patch so concurrent
// status writes by the operator are never clobbered (spec §9.1).
func (k *K8sCluster) SetDesiredState(ctx context.Context, name string, state v1alpha1.DesiredState) error {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	patch := client.MergeFrom(ms.DeepCopy())
	ms.Spec.DesiredState = state
	return k.c.Patch(ctx, &ms, patch)
}

// PatchServerSpec applies the admin-tier spec mutation (spec §7) with the same
// merge-patch discipline as SetDesiredState: read, copy, mutate only the fields
// the admin set, patch — so an operator status write racing in parallel survives.
// felis-api has already validated every field and resolved the §22 ceiling, so
// here we only translate the non-nil patch fields onto the live spec.
func (k *K8sCluster) PatchServerSpec(ctx context.Context, name string, p ServerSpecPatch) error {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	patch := client.MergeFrom(ms.DeepCopy())
	if p.DisplayName != nil {
		ms.Spec.DisplayName = *p.DisplayName
	}
	if p.AutostartPolicy != nil {
		ms.Spec.AutostartPolicy = *p.AutostartPolicy
	}
	if p.Image != nil {
		ms.Spec.Image = *p.Image
	}
	if p.JavaMemory != nil {
		ms.Spec.JavaMemory = *p.JavaMemory
	}
	if p.Resources != nil {
		ms.Spec.Resources = *p.Resources
	}
	return k.c.Patch(ctx, &ms, patch)
}

// serverInfo projects a MinecraftServer onto the API's lifecycle view.
func serverInfo(ms *v1alpha1.MinecraftServer) *ServerInfo {
	return &ServerInfo{
		Name:            ms.Name,
		Subdomain:       ms.Spec.Subdomain,
		Phase:           string(ms.Status.Phase),
		Ready:           ms.Status.Ready,
		AutostartPolicy: string(ms.Spec.AutostartPolicy),
		DesiredState:    string(ms.Spec.DesiredState),
		EndpointMode:    string(ms.Status.Endpoint.Mode),
		EndpointAddress: ms.Status.Endpoint.Address,
		PlayersOnline:   ms.Status.Players.Online,
		PlayersMax:      ms.Status.Players.Max,
	}
}
