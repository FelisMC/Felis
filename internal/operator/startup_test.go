package operator_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestStartupSettingsWaitForNextStart(t *testing.T) {
	for _, starting := range []bool{false, true} {
		for _, clear := range []bool{false, true} {
			t.Run(map[bool]string{false: "running", true: "starting"}[starting]+map[bool]string{false: "/replace", true: "/clear"}[clear], func(t *testing.T) {
				server := runningServer()
				server.Spec.JavaFlags = []string{"-Dmode=old"}
				server.Spec.Args = []string{"--universe", "old world"}
				server.Spec.Env = []v1alpha1.EnvVar{{Name: "PLUGIN_MODE", Value: "old"}}
				r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Known: true}}, server, rconSecret())
				reconcile(t, r, server.Name)
				if !starting {
					markPodReady(t, c, server.Name)
					reconcile(t, r, server.Name)
				}
				before := getSTS(t, c, server.Name)
				server = getServer(t, c, server.Name)
				server.Spec.JavaFlags = []string{"-Dmode=new"}
				server.Spec.Args = []string{"--universe", "new world with spaces"}
				server.Spec.Env = []v1alpha1.EnvVar{{Name: "PLUGIN_MODE", Value: "new value"}}
				if clear {
					server.Spec.JavaFlags, server.Spec.Args, server.Spec.Env = nil, nil, nil
				}
				if err := c.Update(context.Background(), server); err != nil {
					t.Fatal(err)
				}
				reconcile(t, r, server.Name)
				after := getSTS(t, c, server.Name)
				if !reflect.DeepEqual(before.Spec.Template, after.Spec.Template) || before.Annotations[operator.PodTemplateAnnotation] != after.Annotations[operator.PodTemplateAnnotation] {
					t.Fatal("saving startup settings changed the current workload or its applied fingerprint")
				}
				stopRunningServer(t, r, c)
				markPodTerminated(t, c, server.Name)
				reconcile(t, r, server.Name)
				server = getServer(t, c, server.Name)
				server.Spec.DesiredState = v1alpha1.DesiredRunning
				if err := c.Update(context.Background(), server); err != nil {
					t.Fatal(err)
				}
				reconcile(t, r, server.Name)
				main := getSTS(t, c, server.Name).Spec.Template.Spec.Containers[0]
				if clear {
					if len(main.Args) != 0 || startupEnvValue(main.Env, "JAVA_FLAGS") != "" || startupEnvValue(main.Env, "PLUGIN_MODE") != "" {
						t.Fatal("the next start retained cleared settings")
					}
				} else if !reflect.DeepEqual(main.Args, []string{"--universe", "new world with spaces"}) || startupEnvValue(main.Env, "JAVA_FLAGS") != "-Dmode=new" || startupEnvValue(main.Env, "PLUGIN_MODE") != "new value" {
					t.Fatal("the next start did not use the saved settings")
				}
			})
		}
	}
}

func startupEnvValue(env []corev1.EnvVar, name string) string {
	for _, entry := range env {
		if entry.Name == name {
			return entry.Value
		}
	}
	return ""
}

func TestOtherPodChangesStillRollWithPendingStartupSettings(t *testing.T) {
	for _, change := range []string{"memory", "image", "resources", "managed env"} {
		t.Run(change, func(t *testing.T) {
			r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Known: true}}, runningServer(), rconSecret())
			reconcile(t, r, "survival")
			before := getSTS(t, c, "survival")
			server := getServer(t, c, "survival")
			server.Spec.JavaFlags = []string{"-Dmode=new"}
			switch change {
			case "memory":
				server.Spec.JavaMemory = "6G"
			case "image":
				server.Spec.Image = "registry.internal/felis/paper:new"
			case "resources":
				server.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
			case "managed env":
				server.Spec.Env = []v1alpha1.EnvVar{{Name: "FELIS_MANAGED_VALUE", Value: "new"}}
			}
			if err := c.Update(context.Background(), server); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r, "survival")
			after := getSTS(t, c, "survival")
			if reflect.DeepEqual(before.Spec.Template, after.Spec.Template) || before.Annotations[operator.PodTemplateAnnotation] == after.Annotations[operator.PodTemplateAnnotation] {
				t.Fatal("startup settings suppressed a separate pod configuration change")
			}
		})
	}
}

func TestRuntimeArgumentBoundaryUpgradeWaitsForNextStart(t *testing.T) {
	server := runningServer()
	server.Spec.Software = &v1alpha1.SoftwareSpec{Type: "paper", MinecraftVersion: "26.3", Java: 25}
	r, c := newReconciler(t, fakeProber{players: operator.PlayerCount{Known: true}}, server, rconSecret())
	r.FelisImage = felisV1
	reconcile(t, r, server.Name)
	legacy := getSTS(t, c, server.Name)
	legacy.Spec.Template.Spec.Containers[0].Command = []string{"/felis/bin/felis", "launch", "--data", "/data"}
	projection := legacy.Spec.Template.DeepCopy()
	projection.Spec.Containers[0].Stdin = false
	for i := range projection.Spec.InitContainers {
		if projection.Spec.InitContainers[i].Image == felisV1 {
			projection.Spec.InitContainers[i].Image = ""
		}
	}
	raw, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	legacy.Annotations[operator.PodTemplateAnnotation] = hex.EncodeToString(sum[:8])
	if err := c.Update(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	r.FelisImage = felisV2
	reconcile(t, r, server.Name)
	if current := getSTS(t, c, server.Name); !reflect.DeepEqual(current.Spec.Template, legacy.Spec.Template) || current.Annotations[operator.PodTemplateAnnotation] != legacy.Annotations[operator.PodTemplateAnnotation] {
		t.Fatal("the runtime argument-boundary upgrade changed an active template")
	}
}
