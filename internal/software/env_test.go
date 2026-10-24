package software

import "testing"

func TestIsReservedEnv(t *testing.T) {
	for _, name := range []string{
		"FELIS_SOFTWARE", "FELIS_ARTIFACT_CACHE", "FELIS_SERVICE_TOKEN", "FELIS_FORWARDING_SECRET", "FELIS_API_BASE_URL",
		"FELIS_ROOT_DOMAIN", "FELIS_PANEL_HOSTNAME", "FELIS_LOBBY_SERVER", "FELIS_GAME_PORT", "FELIS_MANAGED_FORWARDING",
		"JAVA_MEMORY", "JAVA_FLAGS", "JAVA_HOME", "JAVA_TOOL_OPTIONS", "JRE_HOME",
		"JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "CLASSPATH", "RCON_PORT", "RCON_PASSWORD",
		"FABRIC_PROXY_SECRET", "FABRIC_PROXY_HACK_MESSAGE_CHAIN", "PATH", "HOME",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "GCONV_PATH", "GLIBC_TUNABLES",
		"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "IFS", "CDPATH", "GLOBIGNORE",
	} {
		t.Run(name, func(t *testing.T) {
			if !IsReservedEnv(name) {
				t.Fatal("runtime variable was left editable")
			}
		})
	}
	for _, name := range []string{"PLUGIN_MODE", "DISCORD_WEBHOOK", "TZ", "LANG", "LC_ALL", "CUSTOM_PATH", "MY_JAVA_MEMORY", "felis_custom", "JAVA_OPTS", "JAVA_OPTIONS", "JAVA_PLUGIN_MODE", "JDK_CUSTOM", "RCON_PLUGIN", "FABRIC_PROXY_CUSTOM", "LD_PLUGIN_MODE"} {
		t.Run(name, func(t *testing.T) {
			if IsReservedEnv(name) {
				t.Fatal("application variable was treated as a runtime variable")
			}
		})
	}
}
