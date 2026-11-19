package v1alpha1

import (
	"errors"

	"felis.lolicon.best/internal/software"
	corev1 "k8s.io/api/core/v1"
)

const MaxExtraPorts = 5
const ConditionExtraPortsReady = "ExtraPortsReady"

type ExtraPort struct {
	// +kubebuilder:validation:Enum=TCP;UDP
	Protocol corev1.Protocol `json:"protocol"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

type AllocatedExtraPort struct {
	// +kubebuilder:validation:Enum=TCP;UDP
	Protocol corev1.Protocol `json:"protocol"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	NodePort int32 `json:"nodePort"`
}

func ClearOwnerSettings(spec *MinecraftServerSpec) bool {
	var kept []EnvVar
	for _, entry := range spec.Env {
		if software.IsReservedEnv(entry.Name) {
			kept = append(kept, entry)
		}
	}
	dirty := len(spec.ExtraPorts) > 0 ||
		len(spec.JavaFlags) > 0 ||
		len(spec.Args) > 0 ||
		len(kept) != len(spec.Env) ||
		len(spec.Lifecycle.StopCommands) > 0 ||
		(spec.AutostartPolicy != "" && spec.AutostartPolicy != AutostartOwnerOnly)
	if !dirty {
		return false
	}
	spec.ExtraPorts = nil
	spec.JavaFlags = nil
	spec.Args = nil
	spec.Env = kept
	spec.Lifecycle.StopCommands = nil
	spec.AutostartPolicy = AutostartOwnerOnly
	return true
}

func ValidateExtraPorts(spec MinecraftServerSpec) error {
	if len(spec.ExtraPorts) > MaxExtraPorts {
		return errors.New("at most five extra ports are allowed")
	}
	seen := make(map[ExtraPort]struct{}, len(spec.ExtraPorts))
	for _, port := range spec.ExtraPorts {
		if port.Protocol != corev1.ProtocolTCP && port.Protocol != corev1.ProtocolUDP || port.Port < 1 || port.Port > 65535 {
			return errors.New("extra ports require TCP or UDP and a port from 1 to 65535")
		}
		if port.Protocol == corev1.ProtocolTCP && (port.Port == 25565 || port.Port == 25575 || port.Port == spec.Rcon.Port || port.Port == spec.Startup.HealthHTTPPort) {
			return errors.New("extra ports cannot expose a managed TCP port")
		}
		if _, ok := seen[port]; ok {
			return errors.New("extra ports cannot repeat a protocol and port")
		}
		seen[port] = struct{}{}
	}
	return nil
}
