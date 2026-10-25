package v1alpha1_test

import (
	"slices"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestValidateExtraPorts(t *testing.T) {
	tcp := func(port int32) v1alpha1.ExtraPort {
		return v1alpha1.ExtraPort{Protocol: corev1.ProtocolTCP, Port: port}
	}
	udp := func(port int32) v1alpha1.ExtraPort {
		return v1alpha1.ExtraPort{Protocol: corev1.ProtocolUDP, Port: port}
	}
	cases := []struct {
		name   string
		ports  []v1alpha1.ExtraPort
		rcon   int32
		health int32
		valid  bool
	}{
		{name: "omitted", valid: true},
		{name: "empty", ports: []v1alpha1.ExtraPort{}, valid: true},
		{name: "both protocols", ports: []v1alpha1.ExtraPort{tcp(24454), udp(24454)}, valid: true},
		{name: "boundaries and five entries", ports: []v1alpha1.ExtraPort{tcp(1), udp(65535), tcp(24454), udp(24454), tcp(8123)}, valid: true},
		{name: "too many", ports: []v1alpha1.ExtraPort{tcp(1), tcp(2), tcp(3), tcp(4), tcp(5), tcp(6)}},
		{name: "duplicate", ports: []v1alpha1.ExtraPort{tcp(24454), tcp(24454)}},
		{name: "missing protocol", ports: []v1alpha1.ExtraPort{{Port: 24454}}},
		{name: "other protocol", ports: []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolSCTP, Port: 24454}}},
		{name: "zero", ports: []v1alpha1.ExtraPort{tcp(0)}},
		{name: "negative", ports: []v1alpha1.ExtraPort{tcp(-1)}},
		{name: "too high", ports: []v1alpha1.ExtraPort{tcp(65536)}},
		{name: "game", ports: []v1alpha1.ExtraPort{tcp(25565)}},
		{name: "disabled default RCON", ports: []v1alpha1.ExtraPort{tcp(25575)}},
		{name: "custom RCON", ports: []v1alpha1.ExtraPort{tcp(26000)}, rcon: 26000},
		{name: "health", ports: []v1alpha1.ExtraPort{tcp(8080)}, health: 8080},
		{name: "same UDP numbers", ports: []v1alpha1.ExtraPort{udp(25565), udp(25575), udp(8080)}, health: 8080, valid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := v1alpha1.MinecraftServerSpec{ExtraPorts: tc.ports, Rcon: v1alpha1.RconSpec{Port: tc.rcon}, Startup: v1alpha1.StartupSpec{HealthHTTPPort: tc.health}}
			if err := v1alpha1.ValidateExtraPorts(spec); (err == nil) != tc.valid {
				t.Fatalf("validation error = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestDeepCopyPreservesIndependentExtraPorts(t *testing.T) {
	for _, ports := range [][]v1alpha1.ExtraPort{nil, {}, {{Protocol: corev1.ProtocolUDP, Port: 24454}}} {
		var allocated []v1alpha1.AllocatedExtraPort
		if ports != nil {
			allocated = make([]v1alpha1.AllocatedExtraPort, 0, len(ports))
		}
		for _, port := range ports {
			allocated = append(allocated, v1alpha1.AllocatedExtraPort{Protocol: port.Protocol, Port: port.Port, NodePort: 31000})
		}
		source := &v1alpha1.MinecraftServer{Spec: v1alpha1.MinecraftServerSpec{ExtraPorts: ports}, Status: v1alpha1.MinecraftServerStatus{ExtraPorts: allocated}}
		copied := source.DeepCopy()
		if (copied.Spec.ExtraPorts == nil) != (ports == nil) || !slices.Equal(copied.Spec.ExtraPorts, ports) ||
			(copied.Status.ExtraPorts == nil) != (allocated == nil) || !slices.Equal(copied.Status.ExtraPorts, allocated) {
			t.Fatal("deep copy changed extra port list contents or shape")
		}
		if len(ports) == 0 {
			continue
		}
		copied.Spec.ExtraPorts[0].Port = 8123
		copied.Status.ExtraPorts[0].NodePort = 31001
		if source.Spec.ExtraPorts[0].Port != 24454 || source.Status.ExtraPorts[0].NodePort != 31000 {
			t.Fatal("changing copied extra ports modified the source")
		}
	}
}
