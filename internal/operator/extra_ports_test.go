package operator_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func allocatedExtraClient(c client.Client) client.WithWatch {
	next := int32(31000)
	allocate := func(object client.Object) {
		service, ok := object.(*corev1.Service)
		if !ok || service.Spec.Type != corev1.ServiceTypeNodePort {
			return
		}
		for i := range service.Spec.Ports {
			if service.Spec.Ports[i].NodePort == 0 {
				service.Spec.Ports[i].NodePort = next
				next++
			}
		}
	}
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			allocate(object)
			return cl.Create(ctx, object, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			allocate(object)
			return cl.Update(ctx, object, opts...)
		},
	})
}

func extraResources(t *testing.T, c client.Client) (*corev1.Service, *networkingv1.NetworkPolicy) {
	t.Helper()
	var services corev1.ServiceList
	if err := c.List(context.Background(), &services, client.InNamespace("minecraft")); err != nil {
		t.Fatal(err)
	}
	var service *corev1.Service
	for i := range services.Items {
		if services.Items[i].Spec.Type == corev1.ServiceTypeNodePort {
			if service != nil {
				t.Fatal("more than one extra port Service")
			}
			service = &services.Items[i]
		}
	}
	var policies networkingv1.NetworkPolicyList
	if err := c.List(context.Background(), &policies, client.InNamespace("minecraft")); err != nil {
		t.Fatal(err)
	}
	if service == nil || len(policies.Items) != 1 {
		t.Fatal("expected one extra port Service and NetworkPolicy")
	}
	return service, &policies.Items[0]
}

func extraReady(t *testing.T, server *v1alpha1.MinecraftServer, ready bool) {
	t.Helper()
	condition := meta.FindStatusCondition(server.Status.Conditions, v1alpha1.ConditionExtraPortsReady)
	want := metav1.ConditionFalse
	if ready {
		want = metav1.ConditionTrue
	}
	if condition == nil || condition.Status != want || condition.ObservedGeneration != server.Generation {
		t.Fatalf("extra port condition = %+v, want %s at generation %d", condition, want, server.Generation)
	}
	if !ready && len(server.Status.ExtraPorts) != 0 {
		t.Fatal("failed reconciliation published allocated ports")
	}
}

func TestExtraPortsAllocatePreserveAndRemoveWhileStopped(t *testing.T) {
	server := runningServer()
	server.UID, server.Spec.DesiredState = "server-1", v1alpha1.DesiredStopped
	server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolTCP, Port: 24454}, {Protocol: corev1.ProtocolUDP, Port: 24454}}
	r, c := newReconciler(t, fakeProber{}, server)
	r.Client = allocatedExtraClient(c)
	reconcile(t, r, server.Name)
	service, policy := extraResources(t, c)
	if service.Name == server.Name || service.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Fatal("extra ports must use a separate, node-local Service")
	}
	for _, object := range []client.Object{service, policy} {
		owner := metav1.GetControllerOf(object)
		if owner == nil || owner.UID != server.UID || owner.Name != server.Name {
			t.Fatal("extra port resource has no server controller owner")
		}
	}
	current := getServer(t, c, server.Name)
	extraReady(t, current, true)
	want := []v1alpha1.AllocatedExtraPort{{Protocol: corev1.ProtocolTCP, Port: 24454, NodePort: 31000}, {Protocol: corev1.ProtocolUDP, Port: 24454, NodePort: 31001}}
	if current.Status.Phase != v1alpha1.PhaseStopped || !slices.Equal(current.Status.ExtraPorts, want) {
		t.Fatalf("stopped allocations = %v", current.Status.ExtraPorts)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 0 || len(policy.Spec.Ingress[0].Ports) != 2 {
		t.Fatal("extra ingress must admit exactly the configured ports")
	}
	for i, port := range policy.Spec.Ingress[0].Ports {
		if port.Port == nil || port.Port.IntVal != 24454 || port.EndPort != nil || port.Protocol == nil || *port.Protocol != want[i].Protocol {
			t.Fatal("extra ingress changed an internal port or protocol")
		}
	}
	selector := map[string]string{v1alpha1.LabelServer: server.Name, v1alpha1.LabelComponent: operator.ComponentValue}
	if !reflect.DeepEqual(service.Spec.Selector, selector) || !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, selector) {
		t.Fatal("extra ports must select only this server's game workload")
	}
	current.Generation++
	current.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}, {Protocol: corev1.ProtocolTCP, Port: 8123}}
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, server.Name)
	current = getServer(t, c, server.Name)
	extraReady(t, current, true)
	want = []v1alpha1.AllocatedExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454, NodePort: 31001}, {Protocol: corev1.ProtocolTCP, Port: 8123, NodePort: 31002}}
	if !slices.Equal(current.Status.ExtraPorts, want) {
		t.Fatalf("changed allocations = %v, want %v", current.Status.ExtraPorts, want)
	}
	service, policy = extraResources(t, c)
	if len(service.Spec.Ports) != 2 || len(policy.Spec.Ingress[0].Ports) != 2 || policy.Spec.Ingress[0].Ports[1].Port.IntVal != 8123 {
		t.Fatal("removed port remained exposed or new port was not applied")
	}
	current.Generation++
	current.Spec.ExtraPorts = []v1alpha1.ExtraPort{}
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, server.Name)
	current = getServer(t, c, server.Name)
	extraReady(t, current, true)
	if len(current.Status.ExtraPorts) != 0 {
		t.Fatal("clearing ports retained allocation status")
	}
	for _, object := range []client.Object{&corev1.Service{}, &networkingv1.NetworkPolicy{}} {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(service), object); !apierrors.IsNotFound(err) {
			t.Fatalf("cleared extra port resource still exists: %v", err)
		}
	}
}

func TestExtraPortsDoNotRestartOrChangeGameAndRconServices(t *testing.T) {
	h := newRunHost(t, func(server *v1alpha1.MinecraftServer) { server.UID = "running-server" })
	h.r.Client = allocatedExtraClient(h.c)
	before := getSTS(t, h.c, "survival")
	var game corev1.Service
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: "minecraft", Name: "survival"}, &game); err != nil {
		t.Fatal(err)
	}
	current := getServer(t, h.c, "survival")
	current.Generation++
	current.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}}
	if err := h.c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	current = h.pass()
	extraReady(t, current, true)
	after := getSTS(t, h.c, "survival")
	if !reflect.DeepEqual(before.Spec, after.Spec) || before.Annotations[operator.PodTemplateAnnotation] != after.Annotations[operator.PodTemplateAnnotation] {
		t.Fatal("extra ports changed the StatefulSet template or startup fingerprint")
	}
	var afterGame corev1.Service
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(&game), &afterGame); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(game.Spec, afterGame.Spec) || afterGame.Spec.Type != corev1.ServiceTypeClusterIP && afterGame.Spec.Type != "" {
		t.Fatal("extra ports changed the private game/RCON Service")
	}
	var pod corev1.Pod
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: "minecraft", Name: "survival-0"}, &pod); err != nil || pod.UID != "pod-1" {
		t.Fatal("extra ports replaced the running Pod")
	}
}

func TestExtraPortsRejectForeignResourcesOnApplyAndCleanup(t *testing.T) {
	for _, kind := range []string{"Service", "NetworkPolicy"} {
		t.Run(kind, func(t *testing.T) {
			server := runningServer()
			server.UID, server.Spec.DesiredState = "server-1", v1alpha1.DesiredStopped
			server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}}
			r, c := newReconciler(t, fakeProber{}, server)
			r.Client = allocatedExtraClient(c)
			reconcile(t, r, server.Name)
			service, policy := extraResources(t, c)
			var foreign client.Object = service
			if kind == "NetworkPolicy" {
				foreign = policy
			}
			foreign.GetOwnerReferences()[0].UID = "other-server"
			if err := c.Update(context.Background(), foreign); err != nil {
				t.Fatal(err)
			}
			for _, ports := range [][]v1alpha1.ExtraPort{{{Protocol: corev1.ProtocolTCP, Port: 8123}}, {}} {
				current := getServer(t, c, server.Name)
				current.Generation++
				current.Spec.ExtraPorts = ports
				if err := c.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
				if result := reconcile(t, r, server.Name); result.RequeueAfter <= 0 || result.RequeueAfter > 10*time.Second {
					t.Fatal("ownership conflict did not schedule a bounded retry")
				}
				extraReady(t, getServer(t, c, server.Name), false)
				stored := foreign.DeepCopyObject().(client.Object)
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), stored); err != nil || !reflect.DeepEqual(foreign, stored) {
					t.Fatal("extra port reconciliation modified or removed a foreign resource")
				}
			}
		})
	}
}

func TestExtraPortsInvalidDirectCRRemovesPublicMapping(t *testing.T) {
	for _, port := range []int32{25565, 25575, 8080} {
		server := runningServer()
		server.UID, server.Spec.DesiredState = "server-1", v1alpha1.DesiredStopped
		server.Spec.Startup.HealthHTTPPort = 8080
		server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolTCP, Port: 8123}}
		r, c := newReconciler(t, fakeProber{}, server)
		r.Client = allocatedExtraClient(c)
		reconcile(t, r, server.Name)
		service, _ := extraResources(t, c)
		current := getServer(t, c, server.Name)
		current.Generation++
		current.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolTCP, Port: port}}
		if err := c.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		reconcile(t, r, server.Name)
		current = getServer(t, c, server.Name)
		extraReady(t, current, false)
		if condition := meta.FindStatusCondition(current.Status.Conditions, v1alpha1.ConditionExtraPortsReady); condition.Reason != "InvalidExtraPorts" {
			t.Fatal("invalid direct CR did not report its configuration error")
		}
		for _, object := range []client.Object{&corev1.Service{}, &networkingv1.NetworkPolicy{}} {
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(service), object); !apierrors.IsNotFound(err) {
				t.Fatalf("invalid direct CR retained an extra port resource: %v", err)
			}
		}
	}
}

func TestExtraPortsCleanupWaitsForActualDeletion(t *testing.T) {
	for _, kind := range []string{"Service", "NetworkPolicy"} {
		t.Run(kind, func(t *testing.T) {
			server := runningServer()
			server.UID, server.Spec.DesiredState = "server-1", v1alpha1.DesiredStopped
			server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}}
			r, c := newReconciler(t, fakeProber{}, server)
			r.Client = allocatedExtraClient(c)
			reconcile(t, r, server.Name)
			service, policy := extraResources(t, c)
			var blocked client.Object = service
			if kind == "NetworkPolicy" {
				blocked = policy
			}
			blocked.SetFinalizers([]string{"test.felis/hold"})
			if err := c.Update(context.Background(), blocked); err != nil {
				t.Fatal(err)
			}
			current := getServer(t, c, server.Name)
			current.Generation++
			current.Spec.ExtraPorts = nil
			if err := c.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			r.ExtraResources = c
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					switch object.(type) {
					case *corev1.Service, *networkingv1.NetworkPolicy:
						return apierrors.NewNotFound(schema.GroupResource{Resource: "extra-resource"}, key.Name)
					}
					return cl.Get(ctx, key, object, opts...)
				},
			})
			if result := reconcile(t, r, server.Name); result.RequeueAfter <= 0 || result.RequeueAfter > 10*time.Second {
				t.Fatal("terminating resources did not schedule cleanup retry")
			}
			extraReady(t, getServer(t, c, server.Name), false)
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(blocked), blocked); err != nil || blocked.GetDeletionTimestamp() == nil {
				t.Fatal("cleanup did not request deletion before reporting pending")
			}
			blocked.SetFinalizers(nil)
			if err := c.Update(context.Background(), blocked); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r, server.Name)
			extraReady(t, getServer(t, c, server.Name), true)
		})
	}
}

func TestExtraPortsLegacyEmptyPublishesReady(t *testing.T) {
	server := runningServer()
	server.UID, server.Spec.DesiredState = "legacy-server", v1alpha1.DesiredStopped
	r, c := newReconciler(t, fakeProber{}, server)
	reconcile(t, r, server.Name)
	extraReady(t, getServer(t, c, server.Name), true)
	var services corev1.ServiceList
	if err := c.List(context.Background(), &services); err != nil || len(services.Items) != 0 {
		t.Fatal("an empty legacy server created an extra Service")
	}
}

func TestExtraPortsDoNotPublishUnallocatedNodePorts(t *testing.T) {
	server := runningServer()
	server.UID, server.Spec.DesiredState = "server-1", v1alpha1.DesiredStopped
	server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}}
	r, c := newReconciler(t, fakeProber{}, server)
	if result := reconcile(t, r, server.Name); result.RequeueAfter <= 0 || result.RequeueAfter > 10*time.Second {
		t.Fatal("an unallocated NodePort must wait for reconciliation")
	}
	extraReady(t, getServer(t, c, server.Name), false)
}

func TestExtraPortsNetworkFailureDoesNotBlockStopOrKill(t *testing.T) {
	for _, kill := range []bool{false, true} {
		server, workload := stopWorkload()
		server.UID = "server-1"
		server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}}
		if kill {
			server.Annotations = map[string]string{v1alpha1.AnnotationKill: "request"}
		}
		var steps []string
		r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Known: true}, steps: &steps}, server, workload, gamePod(), rconSecret())
		r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, object client.Object, opts ...client.CreateOption) error {
				if _, ok := object.(*networkingv1.NetworkPolicy); ok {
					steps = append(steps, "extra")
					return errors.New("network policy API unavailable")
				}
				return cl.Create(ctx, object, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
				if sts, ok := object.(*appsv1.StatefulSet); ok && sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
					steps = append(steps, "scale")
				}
				return cl.Update(ctx, object, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
				if _, ok := object.(*corev1.Pod); ok {
					steps = append(steps, "kill")
				}
				return cl.Delete(ctx, object, opts...)
			},
		})
		result := reconcile(t, r, server.Name)
		want := []string{"save", "commands", "scale", "extra"}
		if kill {
			want = []string{"scale", "kill", "extra"}
		}
		if !slices.Equal(steps, want) || *getSTS(t, c, server.Name).Spec.Replicas != 0 || result.RequeueAfter <= 0 || result.RequeueAfter > 10*time.Second {
			t.Fatalf("stop order = %v; retry = %v", steps, result.RequeueAfter)
		}
		current := getServer(t, c, server.Name)
		extraReady(t, current, false)
		if current.Status.Phase != v1alpha1.PhaseStopping {
			t.Fatal("network failure prevented stop progress")
		}
	}
}
