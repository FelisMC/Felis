package api

import (
	"context"
	"errors"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestEmergencyStopIndependentOfOperator(t *testing.T) {
	for _, mode := range []string{"success", "wrong owner", "scale unavailable", "missing workload"} {
		t.Run(mode, func(t *testing.T) {
			scheme := runtime.NewScheme()
			v1alpha1.AddToScheme(scheme)
			appsv1.AddToScheme(scheme)
			corev1.AddToScheme(scheme)
			ms := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft", UID: "server-uid"}, Spec: v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredRunning}, Status: v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseRunning}}
			replicas := int32(1)
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: ms.Name, Namespace: ms.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, v1alpha1.GroupVersion.WithKind("MinecraftServer"))}}, Spec: appsv1.StatefulSetSpec{Replicas: &replicas}}
			if mode == "wrong owner" {
				sts.OwnerReferences[0].UID = "unrelated"
			}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ms)
			if mode != "missing workload" {
				builder = builder.WithObjects(sts)
			}
			if mode == "scale unavailable" {
				builder = builder.WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return errors.New("kubernetes unavailable")
				}})
			}
			c := builder.Build()
			k := NewK8sCluster(c, "minecraft")
			ctx := context.Background()
			err := k.EmergencyStop(ctx, ms.Name)
			if (err != nil) != (mode == "wrong owner" || mode == "scale unavailable") {
				t.Fatal(mode, err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(ms), ms); err != nil {
				t.Fatal(err)
			}
			if ms.Spec.DesiredState != v1alpha1.DesiredStopped || ms.Status.Phase != v1alpha1.PhaseRunning {
				t.Fatal("stop intent missing or status falsely rewritten", ms)
			}
			if mode != "success" && mode != "missing workload" {
				if err := c.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
					t.Fatal(err)
				}
				if *sts.Spec.Replicas != 1 {
					t.Fatal("unsafe scaling")
				}
				return
			}
			if mode == "success" {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: ms.Name + "-0", Namespace: ms.Namespace, Labels: map[string]string{v1alpha1.LabelServer: ms.Name, v1alpha1.LabelComponent: gamePodComponent}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
				if err := c.Create(ctx, pod); err != nil {
					t.Fatal(err)
				}
				info, err := k.GetServer(ctx, ms.Name)
				if err != nil || info.Phase != "Stopping" {
					t.Fatal("running process falsely stopped", info, err)
				}
				if err := c.Delete(ctx, pod); err != nil {
					t.Fatal(err)
				}
				if err := c.Get(ctx, types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name}, sts); err != nil || *sts.Spec.Replicas != 0 {
					t.Fatal(sts, err)
				}
			}
			info, err := k.GetServer(ctx, ms.Name)
			if err != nil || info.Phase != "Stopped" {
				t.Fatal("operator-independent confirmation missing", info, err)
			}
		})
	}
}

type emergencyFakeCluster struct {
	*fakeCluster
	calls int
}

func (c *emergencyFakeCluster) EmergencyStop(context.Context, string) error { c.calls++; return nil }
func TestEmergencyStopAuthorization(t *testing.T) {
	cl := &emergencyFakeCluster{fakeCluster: newFakeCluster()}
	a := newTestAPI(newFakeRepo(), cl)
	path := "/api/v1/servers/survival/emergency-stop"
	for _, p := range []*Principal{nil, {UserID: "admin", Role: "admin", ViaAdminAccess: true}, {UserID: "owner", Role: "owner"}} {
		a.External = staticExternal{p: p}
		w := do(a.ExternalHandler(), "POST", path, `{"confirm":"survival"}`, jsonHeader)
		if w.Code != 401 && w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	a.External = staticExternal{p: p}
	a.Repo.(*fakeRepo).passkeyCreds["key"] = PasskeyCredential{ID: "key", UserID: p.UserID, UserVerified: true}
	if w := do(a.ExternalHandler(), "POST", path, `{"confirm":"survival"}`, jsonHeader); w.Code != 403 || decodeErr(t, w) != "reauth_required" {
		t.Fatal(w.Code, w.Body.String())
	}
	p.ReauthAt = a.now()
	if w := do(a.ExternalHandler(), "POST", path, `{"confirm":"wrong"}`, jsonHeader); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if cl.calls != 0 {
		t.Fatal("unconfirmed stop ran")
	}
	if w := do(a.ExternalHandler(), "POST", path, `{"confirm":"survival"}`, jsonHeader); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if cl.calls != 1 {
		t.Fatal(cl.calls)
	}
}
