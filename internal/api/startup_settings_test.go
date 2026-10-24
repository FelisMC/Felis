package api

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

func startupStrings(count int, value string) []string {
	values := make([]string, count)
	for i := range values {
		values[i] = value
	}
	return values
}

func startupEnvs(count int) []v1alpha1.EnvVar {
	values := make([]v1alpha1.EnvVar, count)
	for i := range values {
		values[i].Name = fmt.Sprintf("PLUGIN_%d", i)
	}
	return values
}

func TestStartupSettingsLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spec  v1alpha1.MinecraftServerSpec
		valid bool
	}{
		{"empty", v1alpha1.MinecraftServerSpec{}, true},
		{"128 JVM flags", v1alpha1.MinecraftServerSpec{JavaFlags: startupStrings(128, "-ea")}, true},
		{"129 JVM flags", v1alpha1.MinecraftServerSpec{JavaFlags: startupStrings(129, "-ea")}, false},
		{"1024 byte JVM flag", v1alpha1.MinecraftServerSpec{JavaFlags: []string{"-Dkey=" + strings.Repeat("x", 1018)}}, true},
		{"1025 byte JVM flag", v1alpha1.MinecraftServerSpec{JavaFlags: []string{"-Dkey=" + strings.Repeat("x", 1019)}}, false},
		{"128 args", v1alpha1.MinecraftServerSpec{Args: startupStrings(128, "a")}, true},
		{"129 args", v1alpha1.MinecraftServerSpec{Args: startupStrings(129, "a")}, false},
		{"2048 byte arg", v1alpha1.MinecraftServerSpec{Args: []string{strings.Repeat("x", 2048)}}, true},
		{"2049 byte arg", v1alpha1.MinecraftServerSpec{Args: []string{strings.Repeat("x", 2049)}}, false},
		{"multibyte arg within limit", v1alpha1.MinecraftServerSpec{Args: []string{strings.Repeat("界", 682)}}, true},
		{"multibyte arg beyond limit", v1alpha1.MinecraftServerSpec{Args: []string{strings.Repeat("界", 683)}}, false},
		{"spaces and empty argv", v1alpha1.MinecraftServerSpec{Args: []string{"hello world", "", "  unchanged  "}}, true},
		{"64 env entries", v1alpha1.MinecraftServerSpec{Env: startupEnvs(64)}, true},
		{"65 env entries", v1alpha1.MinecraftServerSpec{Env: startupEnvs(65)}, false},
		{"128 byte env name", v1alpha1.MinecraftServerSpec{Env: []v1alpha1.EnvVar{{Name: strings.Repeat("A", 128)}}}, true},
		{"129 byte env name", v1alpha1.MinecraftServerSpec{Env: []v1alpha1.EnvVar{{Name: strings.Repeat("A", 129)}}}, false},
		{"4096 byte env value", v1alpha1.MinecraftServerSpec{Env: []v1alpha1.EnvVar{{Name: "PLUGIN_MODE", Value: strings.Repeat("x", 4096)}}}, true},
		{"4097 byte env value", v1alpha1.MinecraftServerSpec{Env: []v1alpha1.EnvVar{{Name: "PLUGIN_MODE", Value: strings.Repeat("x", 4097)}}}, false},
		{"16 commands", v1alpha1.MinecraftServerSpec{Lifecycle: v1alpha1.LifecycleSpec{StopCommands: startupStrings(16, "say goodbye")}}, true},
		{"17 commands", v1alpha1.MinecraftServerSpec{Lifecycle: v1alpha1.LifecycleSpec{StopCommands: startupStrings(17, "say goodbye")}}, false},
		{"1024 byte command", v1alpha1.MinecraftServerSpec{Lifecycle: v1alpha1.LifecycleSpec{StopCommands: []string{"say " + strings.Repeat("x", 1020)}}}, true},
		{"1025 byte command", v1alpha1.MinecraftServerSpec{Lifecycle: v1alpha1.LifecycleSpec{StopCommands: []string{"say " + strings.Repeat("x", 1021)}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStartupSettings(&tc.spec)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if err != nil {
				var apiErr *apiError
				if !errors.As(err, &apiErr) || apiErr.status != 400 || apiErr.code != "bad_startup_settings" {
					t.Fatalf("validation error was not safe for a future API caller: %v", err)
				}
			}
		})
	}
}

func TestStartupSettingsRejectHeapOverridesAndArgumentFiles(t *testing.T) {
	for _, flag := range []string{
		"-Xmx", "-Xmx4G", "-Xms256M", "-mx4G", "-ms256M",
		"-XX:MaxHeapSize=4294967296", "-XX:InitialHeapSize=268435456", "-XX:MinHeapSize=268435456",
		"@user-jvm-args.txt", "-XX:VMOptionsFile=user.conf", "-XX:Flags=user.conf",
		"-Dfake=one -Xmx4G", "-Dfake=one\t-Xms4G", "-Dfake=one\u00a0-Xmx4G", "",
	} {
		t.Run(flag, func(t *testing.T) {
			if err := validateStartupSettings(&v1alpha1.MinecraftServerSpec{JavaFlags: []string{flag}}); err == nil {
				t.Fatal("heap override or multiple JVM arguments were accepted")
			}
		})
	}
	if err := validateStartupSettings(&v1alpha1.MinecraftServerSpec{JavaFlags: []string{"-XX:+UseG1GC", "-Dexample=-Xmx", "-Dlabel=世界"}}); err != nil {
		t.Fatalf("ordinary JVM options were refused: %v", err)
	}
}

func TestStartupSettingsRejectInvalidTextAndEnvNames(t *testing.T) {
	invalid := string([]byte{0xff})
	for _, tc := range []struct {
		name string
		spec v1alpha1.MinecraftServerSpec
	}{
		{"JVM UTF8", v1alpha1.MinecraftServerSpec{JavaFlags: []string{"-Dname=" + invalid}}},
		{"JVM NUL", v1alpha1.MinecraftServerSpec{JavaFlags: []string{"-Dname=\x00"}}},
		{"arg UTF8", v1alpha1.MinecraftServerSpec{Args: []string{invalid}}},
		{"arg NUL", v1alpha1.MinecraftServerSpec{Args: []string{"fake\x00secret"}}},
		{"env UTF8", v1alpha1.MinecraftServerSpec{Env: []v1alpha1.EnvVar{{Name: "PLUGIN_KEY", Value: invalid}}}},
		{"env NUL", v1alpha1.MinecraftServerSpec{Env: []v1alpha1.EnvVar{{Name: "PLUGIN_KEY", Value: "fake\x00secret"}}}},
		{"command UTF8", v1alpha1.MinecraftServerSpec{Lifecycle: v1alpha1.LifecycleSpec{StopCommands: []string{"say " + invalid}}}},
		{"command newline", v1alpha1.MinecraftServerSpec{Lifecycle: v1alpha1.LifecycleSpec{StopCommands: []string{"say hi\nop fake"}}}},
		{"command empty", v1alpha1.MinecraftServerSpec{Lifecycle: v1alpha1.LifecycleSpec{StopCommands: []string{" / "}}}},
		{"duplicate env", v1alpha1.MinecraftServerSpec{Env: []v1alpha1.EnvVar{{Name: "PLUGIN_KEY", Value: "fake-secret-value"}, {Name: "PLUGIN_KEY"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStartupSettings(&tc.spec)
			if err == nil || strings.Contains(err.Error(), "fake-secret-value") || strings.Contains(err.Error(), "fake\x00secret") {
				t.Fatalf("invalid input accepted or echoed: %v", err)
			}
		})
	}
	for _, name := range []string{"", "1PLUGIN", "PLUGIN-KEY", "PLUGIN.KEY", "PLUGIN=KEY", "PLUGIN KEY", "插件", "KEY\x00"} {
		if err := validateStartupSettings(&v1alpha1.MinecraftServerSpec{Env: []v1alpha1.EnvVar{{Name: name}}}); err == nil {
			t.Fatalf("invalid env name accepted: %q", name)
		}
	}
}

func TestStartupSettingsAggregateByteLimit(t *testing.T) {
	spec := v1alpha1.MinecraftServerSpec{
		Args: startupStrings(31, strings.Repeat("x", 2048)),
		Env:  []v1alpha1.EnvVar{{Name: "CUSTOM", Value: strings.Repeat("x", 2042)}, {Name: "FELIS_API_BASE_URL", Value: strings.Repeat("p", 70000)}},
	}
	if err := validateStartupSettings(&spec); err != nil {
		t.Fatalf("exactly 65536 user bytes were refused: %v", err)
	}
	spec.Env[0].Name = "CUSTOMA"
	if err := validateStartupSettings(&spec); err == nil {
		t.Fatal("65537 user bytes were accepted")
	}
}

func TestUserStartupEnvFiltersAndCopiesManagedValues(t *testing.T) {
	env := []v1alpha1.EnvVar{{Name: "FELIS_SERVICE_TOKEN", Value: "fake-platform-credential"}, {Name: "RCON_PASSWORD", Value: "fake-rcon-credential"}, {Name: "PLUGIN_MODE", Value: "custom"}}
	got := userStartupEnv(env)
	if !reflect.DeepEqual(got, []v1alpha1.EnvVar{{Name: "PLUGIN_MODE", Value: "custom"}}) {
		t.Fatal("managed runtime variables escaped the filter")
	}
	got[0].Value = "changed"
	if env[2].Value != "custom" {
		t.Fatal("editing a filtered result changed the stored environment")
	}
	if got := userStartupEnv(nil); got == nil || len(got) != 0 {
		t.Fatal("empty user environment must be a non-nil empty list")
	}
}
