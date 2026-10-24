package api

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/software"
)

const (
	maxStartupJavaFlags     = 128
	maxStartupJavaFlagBytes = 1024
	maxStartupArgs          = 128
	maxStartupArgBytes      = 2048
	maxStartupEnv           = 64
	maxStartupEnvNameBytes  = 128
	maxStartupEnvValueBytes = 4096
	maxStartupStopCommands  = 16
	maxStartupSettingBytes  = 64 << 10
)

var startupEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type ServerStartupSettings struct {
	JavaFlags    []string        `json:"java_flags"`
	Args         []string        `json:"args"`
	Env          []StartupEnvVar `json:"env"`
	StopCommands []string        `json:"stop_commands"`
}

type StartupEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func serverStartupSettings(spec *v1alpha1.MinecraftServerSpec) *ServerStartupSettings {
	settings := &ServerStartupSettings{
		JavaFlags:    append([]string{}, spec.JavaFlags...),
		Args:         append([]string{}, spec.Args...),
		Env:          []StartupEnvVar{},
		StopCommands: append([]string{}, spec.Lifecycle.StopCommands...),
	}
	for _, entry := range userStartupEnv(spec.Env) {
		settings.Env = append(settings.Env, StartupEnvVar{Name: entry.Name, Value: entry.Value})
	}
	return settings
}

func (a *API) handleStartupSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name := r.PathValue("name")
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return
	}
	p := principalFromContext(r.Context())
	if !p.IsAdmin() {
		rec, err := a.Repo.ServerByName(r.Context(), name)
		if err != nil {
			a.writeLookupError(w, r, err)
			return
		}
		if !a.isOwnerOrAdmin(p, rec) {
			writeError(w, r, errForbidden)
			return
		}
	}
	settings, err := a.Cluster.GetStartupSettings(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (p ServerSpecPatch) setsStartupSettings() bool {
	return p.JavaFlags != nil || p.Args != nil || p.Env != nil || p.StopCommands != nil
}

func startupSettingsError(message string) error {
	return newError(http.StatusBadRequest, "bad_startup_settings", "%s", message)
}

func userStartupEnv(env []v1alpha1.EnvVar) []v1alpha1.EnvVar {
	out := []v1alpha1.EnvVar{}
	for _, entry := range env {
		if !software.IsReservedEnv(entry.Name) {
			out = append(out, entry)
		}
	}
	return out
}

func applyStartupSettings(spec *v1alpha1.MinecraftServerSpec, p ServerSpecPatch) error {
	if p.JavaFlags != nil {
		spec.JavaFlags = append([]string(nil), (*p.JavaFlags)...)
	}
	if p.Args != nil {
		spec.Args = append([]string(nil), (*p.Args)...)
	}
	if p.Env != nil {
		for _, entry := range *p.Env {
			if software.IsReservedEnv(entry.Name) {
				return startupSettingsError("env cannot replace a reserved runtime variable")
			}
		}
		env := []v1alpha1.EnvVar{}
		for _, entry := range spec.Env {
			if software.IsReservedEnv(entry.Name) {
				env = append(env, entry)
			}
		}
		spec.Env = append(env, (*p.Env)...)
	}
	if p.StopCommands != nil {
		commands := append([]string(nil), (*p.StopCommands)...)
		if len(commands) > maxStartupStopCommands {
			return startupSettingsError("stop_commands exceeds 16 commands")
		}
		for i, command := range commands {
			if !startupTextValid(command, maxConsoleCommandLen) {
				return startupSettingsError("stop_commands must contain valid single-line commands of at most 1024 bytes")
			}
			normalized, err := normalizeConsoleCommand(command)
			if err != nil {
				return startupSettingsError("stop_commands must contain valid single-line commands of at most 1024 bytes")
			}
			commands[i] = normalized
		}
		spec.Lifecycle.StopCommands = commands
	}
	return validateStartupSettings(spec)
}

func startupTextValid(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func forbiddenJavaFlag(flag string) bool {
	for _, prefix := range []string{"-Xmx", "-Xms", "-mx", "-ms", "-XX:MaxHeapSize", "-XX:InitialHeapSize", "-XX:MinHeapSize", "-XX:VMOptionsFile", "-XX:Flags", "@"} {
		if strings.HasPrefix(flag, prefix) {
			return true
		}
	}
	return false
}

func validateStartupSettings(spec *v1alpha1.MinecraftServerSpec) error {
	env := userStartupEnv(spec.Env)
	if len(spec.JavaFlags) > maxStartupJavaFlags || len(spec.Args) > maxStartupArgs || len(env) > maxStartupEnv || len(spec.Lifecycle.StopCommands) > maxStartupStopCommands {
		return startupSettingsError("startup settings contain too many entries")
	}
	total := 0
	for _, flag := range spec.JavaFlags {
		if !startupTextValid(flag, maxStartupJavaFlagBytes) || flag == "" || strings.IndexFunc(flag, unicode.IsSpace) >= 0 || strings.IndexFunc(flag, unicode.IsControl) >= 0 || forbiddenJavaFlag(flag) {
			return startupSettingsError("java_flags must contain single JVM arguments of at most 1024 bytes without heap sizing or argument files")
		}
		total += len(flag)
	}
	for _, arg := range spec.Args {
		if !startupTextValid(arg, maxStartupArgBytes) {
			return startupSettingsError("args must contain valid UTF-8 arguments of at most 2048 bytes without NUL")
		}
		total += len(arg)
	}
	names := make(map[string]bool, len(env))
	for _, entry := range env {
		if len(entry.Name) > maxStartupEnvNameBytes || !startupEnvName.MatchString(entry.Name) || names[entry.Name] {
			return startupSettingsError("env names must be unique portable names of at most 128 bytes")
		}
		if !startupTextValid(entry.Value, maxStartupEnvValueBytes) {
			return startupSettingsError("env values must be valid UTF-8 of at most 4096 bytes without NUL")
		}
		names[entry.Name] = true
		total += len(entry.Name) + len(entry.Value)
	}
	for _, command := range spec.Lifecycle.StopCommands {
		if !startupTextValid(command, maxConsoleCommandLen) {
			return startupSettingsError("stop_commands must contain valid single-line commands of at most 1024 bytes")
		}
		if _, err := normalizeConsoleCommand(command); err != nil {
			return startupSettingsError("stop_commands must contain valid single-line commands of at most 1024 bytes")
		}
		total += len(command)
	}
	if total > maxStartupSettingBytes {
		return startupSettingsError("startup settings exceed 65536 bytes in total")
	}
	return nil
}
