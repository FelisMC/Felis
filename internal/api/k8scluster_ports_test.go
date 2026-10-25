package api

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPatchExtraPortsPreservesLifecycleAndStartupSettings(t *testing.T) {
	for _, state := range []v1alpha1.DesiredState{v1alpha1.DesiredRunning, v1alpha1.DesiredStopped} {
		t.Run(string(state), func(t *testing.T) {
			spec := startupTestSpec()
			spec.DesiredState = state
			spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 19132}}
			k, c, before := newStartupSettingsCluster(t, spec)
			ports := []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}, {Protocol: corev1.ProtocolTCP, Port: 8123}}
			if err := k.PatchServerSpec(context.Background(), before.Name, ServerSpecPatch{ExtraPorts: &ports}); err != nil {
				t.Fatal(err)
			}
			var got v1alpha1.MinecraftServer
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
				t.Fatal(err)
			}
			want := *before.Spec.DeepCopy()
			want.ExtraPorts = ports
			if !reflect.DeepEqual(got.Spec, want) || !reflect.DeepEqual(got.Status, before.Status) || !reflect.DeepEqual(got.Annotations, before.Annotations) {
				t.Fatal("port configuration changed startup settings, lifecycle, status or annotations")
			}
			name := "Renamed"
			if err := k.PatchServerSpec(context.Background(), before.Name, ServerSpecPatch{DisplayName: &name}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Spec.ExtraPorts, ports) {
				t.Fatal("omitting extraPorts discarded the mappings")
			}
			empty := []v1alpha1.ExtraPort{}
			if err := k.PatchServerSpec(context.Background(), before.Name, ServerSpecPatch{ExtraPorts: &empty}); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Spec.ExtraPorts) != 0 || got.Spec.DisplayName != name {
				t.Fatal("explicit empty ports failed to clear only the requested configuration")
			}
		})
	}
}

func TestPatchExtraPortsRevalidatesManagedPortsAfterConflict(t *testing.T) {
	spec := startupTestSpec()
	k, c, before := newStartupSettingsCluster(t, spec)
	c.beforePatch = func(ctx context.Context) error {
		if c.patches != 1 {
			return nil
		}
		var current v1alpha1.MinecraftServer
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(before), &current); err != nil {
			return err
		}
		current.Spec.Startup.HealthHTTPPort = 8123
		return c.Client.Update(ctx, &current)
	}
	ports := []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolTCP, Port: 8123}}
	name := "Must not apply"
	err := k.PatchServerSpec(context.Background(), before.Name, ServerSpecPatch{ExtraPorts: &ports, DisplayName: &name})
	var failure *apiError
	if !errors.As(err, &failure) || failure.code != "bad_extra_ports" {
		t.Fatalf("error=%v, want bad_extra_ports", err)
	}
	var got v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Startup.HealthHTTPPort != 8123 || len(got.Spec.ExtraPorts) != 0 || got.Spec.DisplayName != before.Spec.DisplayName {
		t.Fatal("retry exposed a newly managed port or partially applied the rejected patch")
	}
}

func TestExtraPortsProjectionRequiresCurrentCompleteConfirmation(t *testing.T) {
	for _, mode := range []string{"ready", "pending", "failed", "old generation", "different protocol", "missing mapping", "zero nodePort", "clearing"} {
		t.Run(mode, func(t *testing.T) {
			server := stoppedServer()
			server.Generation = 4
			server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}, {Protocol: corev1.ProtocolTCP, Port: 8123}}
			server.Status.ExtraPorts = []v1alpha1.AllocatedExtraPort{{Protocol: corev1.ProtocolTCP, Port: 8123, NodePort: 30101}, {Protocol: corev1.ProtocolUDP, Port: 24454, NodePort: 30100}}
			server.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionExtraPortsReady, Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: 4}}
			switch mode {
			case "pending":
				server.Status.Conditions = nil
			case "failed":
				server.Status.Conditions[0].Status = metav1.ConditionFalse
			case "old generation":
				server.Status.Conditions[0].ObservedGeneration = 3
			case "different protocol":
				server.Status.ExtraPorts[1].Protocol = corev1.ProtocolTCP
			case "missing mapping":
				server.Status.ExtraPorts = server.Status.ExtraPorts[:1]
			case "zero nodePort":
				server.Status.ExtraPorts[0].NodePort = 0
			case "clearing":
				server.Spec.ExtraPorts = nil
			}
			info := serverInfo(server, time.Now())
			if info.ExtraPortsPending != (mode != "ready") || len(info.ExtraPorts) != len(server.Spec.ExtraPorts) {
				t.Fatalf("projection=%+v pending=%v", info.ExtraPorts, info.ExtraPortsPending)
			}
			if mode == "ready" {
				want := []ServerExtraPort{{Protocol: "UDP", Port: 24454, NodePort: 30100}, {Protocol: "TCP", Port: 8123, NodePort: 30101}}
				if !reflect.DeepEqual(info.ExtraPorts, want) {
					t.Fatal("status did not match the requested protocol and port")
				}
			} else {
				for _, port := range info.ExtraPorts {
					if port.NodePort != 0 || port.Address != "" {
						t.Fatal("unconfirmed mapping was reported as active")
					}
				}
			}
		})
	}
}

func TestExtraPortAddressesUseOnlyDirectPublicIP(t *testing.T) {
	for _, tc := range []struct {
		name, configured, external, want string
		distributed, running             bool
	}{
		{"single IPv4", "203.0.113.8", "", "203.0.113.8:30100", false, false},
		{"single IPv6", "2001:db8::8", "", "[2001:db8::8]:30100", false, false},
		{"tunnel hostname", "console.example.com", "", "", false, false},
		{"worker IPv4", "203.0.113.99", "203.0.113.9", "203.0.113.9:30100", true, false},
		{"worker IPv6", "203.0.113.99", "2001:db8::9", "[2001:db8::9]:30100", true, false},
		{"worker without external address", "203.0.113.99", "", "", true, false},
		{"actual running node", "203.0.113.99", "203.0.113.9", "203.0.113.9:30100", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := stoppedServer()
			server.Spec.NodeName = "worker"
			if tc.running {
				server.Spec.DesiredState, server.Status.Phase, server.Status.NodeName = v1alpha1.DesiredRunning, v1alpha1.PhaseRunning, "actual-worker"
			}
			server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}}
			server.Status.ExtraPorts = []v1alpha1.AllocatedExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454, NodePort: 30100}}
			server.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionExtraPortsReady, Status: metav1.ConditionTrue, Reason: "Ready"}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.9"}}}}
			if tc.running {
				node.Name = "actual-worker"
			}
			if tc.external != "" {
				node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: tc.external})
			}
			k, c := lockCluster(t, server, node)
			k.WithDistributed(tc.distributed, "controller")
			k.ExtraPortsPublicIP = tc.configured
			nodeReads := 0
			k.c = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Node); ok {
					nodeReads++
				}
				return cl.Get(ctx, key, obj, opts...)
			}})
			info, err := k.GetServer(context.Background(), server.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(info.ExtraPorts) != 1 || info.ExtraPorts[0].Address != tc.want {
				t.Fatalf("mapping=%+v want address=%q", info.ExtraPorts, tc.want)
			}
			before := nodeReads
			list, err := k.ListServers(context.Background())
			if err != nil || len(list) != 1 || nodeReads != before || len(list[0].ExtraPorts) != 1 || list[0].ExtraPorts[0].Address != "" {
				t.Fatal("fleet listing performed per-server node lookups or lost the mapping")
			}
		})
	}
}

func TestClearExtraPortsWaitsForCurrentEmptyConfirmation(t *testing.T) {
	for _, mode := range []string{"unobserved", "failed", "stale", "allocated", "ready", "different UID", "missing CR"} {
		t.Run(mode, func(t *testing.T) {
			server := stoppedServer()
			server.UID, server.Generation = "ports-server", 4
			server.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionExtraPortsReady, Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: 4}}
			switch mode {
			case "unobserved":
				server.Status.Conditions = nil
			case "failed":
				server.Status.Conditions[0].Status = metav1.ConditionFalse
			case "stale":
				server.Status.Conditions[0].ObservedGeneration = 3
			case "allocated":
				server.Status.ExtraPorts = []v1alpha1.AllocatedExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454, NodePort: 30100}}
			}
			k, c := lockCluster(t, server)
			if mode == "missing CR" {
				if err := c.Delete(context.Background(), server); err != nil {
					t.Fatal(err)
				}
			}
			uid := "ports-server"
			if mode == "different UID" {
				uid = "old-server"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := k.ClearExtraPorts(ctx, server.Name, uid)
			if mode == "ready" || mode == "missing CR" {
				if err != nil {
					t.Fatal(err)
				}
			} else if mode == "different UID" {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("identity error=%v", err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cleanup passed without current empty confirmation: %v", err)
			}
		})
	}
}

func TestClearExtraPortsCASDoesNotFinishBeforeOperatorCleanup(t *testing.T) {
	server := stoppedServer()
	server.UID, server.Generation = "ports-server", 4
	server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}}
	server.Status.ExtraPorts = []v1alpha1.AllocatedExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454, NodePort: 30100}}
	server.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionExtraPortsReady, Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: 4}}
	k, c := lockCluster(t, server)
	patched := make(chan struct{}, 1)
	k.c = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		err := cl.Patch(ctx, obj, patch, opts...)
		if err == nil {
			patched <- struct{}{}
		}
		return err
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- k.ClearExtraPorts(ctx, server.Name, string(server.UID)) }()
	select {
	case <-patched:
	case <-ctx.Done():
		t.Fatal("cleanup did not clear the desired port list")
	}
	select {
	case err := <-done:
		t.Fatalf("cleanup completed before service removal confirmation: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	var current v1alpha1.MinecraftServer
	if err := c.Get(ctx, client.ObjectKeyFromObject(server), &current); err != nil {
		t.Fatal(err)
	}
	if len(current.Spec.ExtraPorts) != 0 {
		t.Fatal("desired ports were not cleared")
	}
	current.Status.ExtraPorts = nil
	if err := c.Status().Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
