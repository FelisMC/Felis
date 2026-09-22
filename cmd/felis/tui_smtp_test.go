package main

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestSMTPSecretManifestCarriesTargetNamespace pins the fix for the
// workload-namespace replica: kubectl refuses a manifest whose namespace
// conflicts with -n ("the namespace from the provided object ... does not
// match"), so the mirror must render felis-smtp with the TARGET namespace —
// otherwise the "configure email" refresh fails on the first apply and the
// felis-config mirror never runs at all.
func TestSMTPSecretManifestCarriesTargetNamespace(t *testing.T) {
	for _, ns := range []string{"felis", "minecraft"} {
		b, err := smtpSecretManifest("pw", ns)
		if err != nil {
			t.Fatalf("render for %s: %v", ns, err)
		}
		var got struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal for %s: %v", ns, err)
		}
		if got.Metadata.Namespace != ns {
			t.Fatalf("manifest namespace = %q, want %q", got.Metadata.Namespace, ns)
		}
		if !strings.Contains(string(b), "name: felis-smtp") {
			t.Fatalf("manifest must still name felis-smtp: %s", b)
		}
	}
}
