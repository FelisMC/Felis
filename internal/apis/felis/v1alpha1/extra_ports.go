package v1alpha1

import (
	"errors"

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
