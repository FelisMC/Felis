package felis

import (
	"archive/tar"
	"embed"
	"io"
	"io/fs"
)

//go:embed deploy/bootstrap.sh
var bootstrapScript string

//go:embed deploy/crd/*.yaml
var bootstrapAssets embed.FS

// Embed explicit source directories and verification metadata; exclude generated output and Gradle wrappers.
//
//go:embed deploy/game-stack.lock
//go:embed deploy/limbo/Dockerfile deploy/limbo/entrypoint.sh
//go:embed deploy/lobby/Dockerfile deploy/lobby/entrypoint.sh
//go:embed deploy/paper/Dockerfile deploy/paper/entrypoint.sh
//go:embed plugins/limbo/build.gradle plugins/limbo/settings.gradle plugins/limbo/src plugins/limbo/gradle/verification-metadata.xml
//go:embed plugins/paper/build.gradle plugins/paper/settings.gradle plugins/paper/src plugins/paper/gradle/verification-metadata.xml
//go:embed plugins/velocity/build.gradle plugins/velocity/settings.gradle plugins/velocity/src plugins/velocity/gradle/verification-metadata.xml
//go:embed plugins/shared/src plugins/shared/build-progress.gradle
var gameStackAssets embed.FS

// Preserve repository-relative layout when streaming embedded game-stack sources.
// bootstrap.sh extracts it into a temp dir and uses that as the docker build context when it has no source checkout.
func GameStackTar(w io.Writer) error {
	tw := tar.NewWriter(w)
	err := fs.WalkDir(gameStackAssets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == "." {
			return err
		}
		data, err := gameStackAssets.ReadFile(path)
		if err != nil {
			return err
		}
		// Use mode 0644 because Dockerfiles invoke entrypoints through sh, including Windows checkouts.
		if err := tw.WriteHeader(&tar.Header{
			Name:     path,
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// BootstrapScript returns the host bootstrap installer embedded in the felis binary.
func BootstrapScript() string {
	return bootstrapScript
}

// MinecraftServerCRD returns the embedded MinecraftServer CRD YAML used by the
// host bootstrap path that runs without a source checkout.
func MinecraftServerCRD() ([]byte, error) {
	return bootstrapAssets.ReadFile("deploy/crd/felis.lolicon.best_minecraftservers.yaml")
}
