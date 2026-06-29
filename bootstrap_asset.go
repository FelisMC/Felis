package felis

import "embed"

//go:embed deploy/bootstrap.sh
var bootstrapScript string

//go:embed deploy/crd/*.yaml
var bootstrapAssets embed.FS

// BootstrapScript returns the host bootstrap installer embedded in the felis binary.
func BootstrapScript() string {
	return bootstrapScript
}

// MinecraftServerCRD returns the embedded MinecraftServer CRD YAML used by the
// host bootstrap path that runs without a source checkout.
func MinecraftServerCRD() ([]byte, error) {
	return bootstrapAssets.ReadFile("deploy/crd/felis.lolicon.best_minecraftservers.yaml")
}
