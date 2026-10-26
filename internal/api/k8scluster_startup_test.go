package api

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type startupPatchClient struct {
	client.Client
	patches     int
	beforePatch func(context.Context) error
}

func (c *startupPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	if c.beforePatch != nil {
		if err := c.beforePatch(ctx); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func startupTestSpec() v1alpha1.MinecraftServerSpec {
	return v1alpha1.MinecraftServerSpec{
		Subdomain: "login", DesiredState: v1alpha1.DesiredRunning, JavaMemory: "512M",
		JavaFlags: []string{"-Dold=kept"}, Args: []string{"--old"},
		Env:       []v1alpha1.EnvVar{{Name: "FELIS_API_BASE_URL", Value: "http://fake-platform.internal"}, {Name: "PLUGIN_OLD", Value: "old"}},
		Lifecycle: v1alpha1.LifecycleSpec{StopCommands: []string{"say old"}, TerminationGracePeriodSeconds: 120},
	}
}

func newStartupSettingsCluster(t *testing.T, spec v1alpha1.MinecraftServerSpec) (*K8sCluster, *startupPatchClient, *v1alpha1.MinecraftServer) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ms := &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{Name: "login", Namespace: "minecraft", Annotations: map[string]string{"unrelated": "kept"}},
		Spec:       spec, Status: v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseRunning},
	}
	c := &startupPatchClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ms).WithStatusSubresource(ms).Build()}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ms), ms); err != nil {
		t.Fatal(err)
	}
	return NewK8sCluster(c, ms.Namespace), c, ms
}

func TestGetStartupSettingsReadsLiteralCRWithoutPodOrSecretAccess(t *testing.T) {
	spec := startupTestSpec()
	spec.JavaFlags = []string{"-Xmx4G"}
	spec.Args = []string{"", "literal $(unchanged)", "  two words  "}
	spec.Env = []v1alpha1.EnvVar{
		{Name: "FELIS_SERVICE_TOKEN", Value: "fake-managed-secret"},
		{Name: "RCON_PASSWORD", Value: "fake-rcon-secret"},
		{Name: "JAVA_TOOL_OPTIONS", Value: "fake-managed-options"},
		{Name: "PLUGIN_LITERAL", Value: "${FELIS_SERVICE_TOKEN}"},
		{Name: "PLUGIN_EMPTY", Value: ""},
	}
	// Only MinecraftServer is registered, so the settings must come from the spec.
	k, _, _ := newStartupSettingsCluster(t, spec)
	got, err := k.GetStartupSettings(context.Background(), "login")
	if err != nil {
		t.Fatal(err)
	}
	want := &ServerStartupSettings{
		JavaFlags: []string{"-Xmx4G"}, Args: []string{"", "literal $(unchanged)", "  two words  "},
		Env:          []StartupEnvVar{{Name: "PLUGIN_LITERAL", Value: "${FELIS_SERVICE_TOKEN}"}, {Name: "PLUGIN_EMPTY", Value: ""}},
		StopCommands: []string{"say old"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("private projection validated legacy flags, expanded values or exposed managed settings")
	}
	got.JavaFlags[0], got.Args[0], got.Env[0].Value, got.StopCommands[0] = "changed", "changed", "changed", "changed"
	if reread, err := k.GetStartupSettings(context.Background(), "login"); err != nil || !reflect.DeepEqual(reread, want) {
		t.Fatal("changing the returned projection modified the server")
	}
	if _, err := k.GetStartupSettings(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing server error=%v", err)
	}
}

func TestPatchStartupSettingsPreservesManagedEnvAndUnrelatedSpec(t *testing.T) {
	k, c, before := newStartupSettingsCluster(t, startupTestSpec())
	flags, args := []string{"-XX:+UseG1GC", "-Dlabel=世界"}, []string{"--motd", "hello world", "", "  unchanged  "}
	env := []v1alpha1.EnvVar{{Name: "PLUGIN_MODE", Value: ""}}
	commands := []string{" /say 正在停止 ", "stop"}
	err := k.PatchServerSpec(context.Background(), "login", ServerSpecPatch{JavaFlags: &flags, Args: &args, Env: &env, StopCommands: &commands})
	if err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Spec.JavaFlags, flags) || !reflect.DeepEqual(got.Spec.Args, args) || !reflect.DeepEqual(got.Spec.Lifecycle.StopCommands, []string{"say 正在停止", "stop"}) {
		t.Fatal("startup arguments or stop-command order changed")
	}
	wantEnv := []v1alpha1.EnvVar{{Name: "FELIS_API_BASE_URL", Value: "http://fake-platform.internal"}, {Name: "PLUGIN_MODE", Value: ""}}
	if !reflect.DeepEqual(got.Spec.Env, wantEnv) || got.Spec.JavaMemory != before.Spec.JavaMemory || got.Spec.Lifecycle.TerminationGracePeriodSeconds != 120 || !reflect.DeepEqual(got.Status, before.Status) || !reflect.DeepEqual(got.Annotations, before.Annotations) || c.patches != 1 {
		t.Fatalf("managed settings or status changed, patches=%d", c.patches)
	}
	if commands[0] != " /say 正在停止 " {
		t.Fatal("normalization modified the caller's slice")
	}
}

func TestPatchStartupSettingsClearsOnlyPresentArrays(t *testing.T) {
	for _, field := range []string{"java_flags", "args", "env", "stop_commands"} {
		t.Run(field, func(t *testing.T) {
			k, c, before := newStartupSettingsCluster(t, startupTestSpec())
			empty, emptyEnv := []string{}, []v1alpha1.EnvVar{}
			patch := ServerSpecPatch{}
			want := before.Spec.DeepCopy()
			switch field {
			case "java_flags":
				patch.JavaFlags = &empty
				want.JavaFlags = nil
			case "args":
				patch.Args = &empty
				want.Args = nil
			case "env":
				patch.Env = &emptyEnv
				want.Env = []v1alpha1.EnvVar{before.Spec.Env[0]}
			case "stop_commands":
				patch.StopCommands = &empty
				want.Lifecycle.StopCommands = nil
			}
			if err := k.PatchServerSpec(context.Background(), "login", patch); err != nil {
				t.Fatal(err)
			}
			var got v1alpha1.MinecraftServer
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Spec, *want) {
				t.Fatal("an omitted field changed or an explicit empty array was ignored")
			}
		})
	}
}

func TestPatchStartupEnvLimitExcludesManagedEntries(t *testing.T) {
	k, c, before := newStartupSettingsCluster(t, startupTestSpec())
	env := startupEnvs(64)
	if err := k.PatchServerSpec(context.Background(), "login", ServerSpecPatch{Env: &env}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Env) != 65 || !reflect.DeepEqual(got.Spec.Env[0], before.Spec.Env[0]) || !reflect.DeepEqual(userStartupEnv(got.Spec.Env), env) {
		t.Fatal("the user limit removed or counted the managed environment")
	}
}

func TestPatchStartupSettingsRejectsChangesAtomically(t *testing.T) {
	for _, mode := range []string{"heap flag", "reserved env", "duplicate env", "multiple commands", "aggregate"} {
		t.Run(mode, func(t *testing.T) {
			spec := startupTestSpec()
			if mode == "aggregate" {
				spec.JavaFlags, spec.Env, spec.Lifecycle.StopCommands = nil, nil, nil
				spec.Args = startupStrings(32, strings.Repeat("x", 2048))
			}
			k, c, before := newStartupSettingsCluster(t, spec)
			display := "must not be saved"
			flags, env, commands := []string{"-Xmx2G"}, []v1alpha1.EnvVar{{Name: "FELIS_SERVICE_TOKEN", Value: "fake-private-input"}}, []string{"say hi\nop fake"}
			patch := ServerSpecPatch{DisplayName: &display}
			switch mode {
			case "heap flag":
				patch.JavaFlags = &flags
			case "reserved env":
				patch.Env = &env
			case "duplicate env":
				env = []v1alpha1.EnvVar{{Name: "PLUGIN_MODE"}, {Name: "PLUGIN_MODE", Value: "fake-private-input"}}
				patch.Env = &env
			case "multiple commands":
				patch.StopCommands = &commands
			case "aggregate":
				flags = []string{"-ea"}
				patch.JavaFlags = &flags
			}
			err := k.PatchServerSpec(context.Background(), "login", patch)
			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.code != "bad_startup_settings" || strings.Contains(err.Error(), "fake-private-input") || c.patches != 0 {
				t.Fatalf("invalid patch reached Kubernetes or exposed a value: error=%v patches=%d", err, c.patches)
			}
			var got v1alpha1.MinecraftServer
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Spec, before.Spec) || !reflect.DeepEqual(got.Status, before.Status) {
				t.Fatal("a rejected patch left a partial update")
			}
		})
	}
}

func TestOrdinaryPatchDoesNotValidateLegacyStartupSettings(t *testing.T) {
	spec := startupTestSpec()
	spec.JavaFlags = []string{"-Xmx4G"}
	spec.Args = []string{strings.Repeat("x", 2049)}
	k, c, before := newStartupSettingsCluster(t, spec)
	display := "new display"
	if err := k.PatchServerSpec(context.Background(), "login", ServerSpecPatch{DisplayName: &display}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.DisplayName != display || !reflect.DeepEqual(got.Spec.JavaFlags, spec.JavaFlags) || !reflect.DeepEqual(got.Spec.Args, spec.Args) {
		t.Fatal("ordinary edit rewrote or validated unchanged legacy startup settings")
	}
}

func TestPatchStartupEnvRetriesAgainstCurrentManagedValues(t *testing.T) {
	k, c, before := newStartupSettingsCluster(t, startupTestSpec())
	c.beforePatch = func(ctx context.Context) error {
		if c.patches != 1 {
			return nil
		}
		var current v1alpha1.MinecraftServer
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(before), &current); err != nil {
			return err
		}
		current.Spec.Env[0].Value = "http://new-platform.internal"
		current.Spec.DisplayName = "concurrent display"
		return c.Client.Update(ctx, &current)
	}
	env := []v1alpha1.EnvVar{{Name: "PLUGIN_MODE", Value: "custom"}}
	if err := k.PatchServerSpec(context.Background(), "login", ServerSpecPatch{Env: &env}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
		t.Fatal(err)
	}
	if c.patches != 2 || got.Spec.Env[0].Value != "http://new-platform.internal" || got.Spec.DisplayName != "concurrent display" || !reflect.DeepEqual(userStartupEnv(got.Spec.Env), env) {
		t.Fatalf("concurrent managed values were overwritten or the patch was not retried: patches=%d", c.patches)
	}
}

func TestPatchStartupSettingsRechecksAggregateAfterConflict(t *testing.T) {
	spec := startupTestSpec()
	spec.JavaFlags, spec.Env, spec.Lifecycle.StopCommands = nil, nil, nil
	spec.Args = startupStrings(31, strings.Repeat("x", 2048))
	k, c, before := newStartupSettingsCluster(t, spec)
	c.beforePatch = func(ctx context.Context) error {
		if c.patches != 1 {
			return nil
		}
		var current v1alpha1.MinecraftServer
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(before), &current); err != nil {
			return err
		}
		current.Spec.JavaFlags = startupStrings(2, "-Dkey="+strings.Repeat("x", 1018))
		return c.Client.Update(ctx, &current)
	}
	env := []v1alpha1.EnvVar{{Name: "PLUGIN", Value: strings.Repeat("x", 1018)}}
	err := k.PatchServerSpec(context.Background(), "login", ServerSpecPatch{Env: &env})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.code != "bad_startup_settings" || c.patches != 1 {
		t.Fatalf("a retry bypassed the combined byte limit: error=%v patches=%d", err, c.patches)
	}
	var got v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.JavaFlags) != 2 || len(got.Spec.Env) != 0 {
		t.Fatal("a rejected retry changed the concurrent settings")
	}
}

func TestPatchStartupSettingsConflictsDoNotOverwriteConcurrentChanges(t *testing.T) {
	k, c, before := newStartupSettingsCluster(t, startupTestSpec())
	c.beforePatch = func(ctx context.Context) error {
		var current v1alpha1.MinecraftServer
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(before), &current); err != nil {
			return err
		}
		current.Spec.DisplayName += "changed"
		return c.Client.Update(ctx, &current)
	}
	args := []string{"--new"}
	err := k.PatchServerSpec(context.Background(), "login", ServerSpecPatch{Args: &args})
	if !errors.Is(err, ErrConflict) || c.patches < 2 {
		t.Fatalf("continuous conflicting writes did not produce a bounded conflict: error=%v patches=%d", err, c.patches)
	}
	var got v1alpha1.MinecraftServer
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(before), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Spec.Args, before.Spec.Args) || got.Spec.DisplayName == "" {
		t.Fatal("an exhausted retry overwrote the concurrent server state")
	}
}
