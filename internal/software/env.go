package software

import "strings"

func IsReservedEnv(name string) bool {
	if strings.HasPrefix(name, "FELIS_") {
		return true
	}
	switch name {
	case "JAVA_MEMORY", "JAVA_FLAGS", "JAVA_HOME", "JRE_HOME", "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "CLASSPATH",
		"RCON_PORT", "RCON_PASSWORD", "FABRIC_PROXY_SECRET", "FABRIC_PROXY_HACK_MESSAGE_CHAIN",
		"PATH", "HOME", "BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "IFS", "CDPATH", "GLOBIGNORE",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "GCONV_PATH", "GLIBC_TUNABLES":
		return true
	}
	return false
}
