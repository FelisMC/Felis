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

// gameStackAssets carries everything deploy/bootstrap.sh needs to build the three
// game images (login limbo, lobby, plain Paper) and the Velocity plugin, for the TUI
// install path — which pipes the embedded bootstrap.sh into bash and therefore has
// NO source checkout on disk to build from.
//
// The patterns are file- and directory-explicit rather than a bare `plugins`: a
// developer's working tree carries gradle output (plugins/*/build, plugins/*/bin,
// and for the modded loaders a decompiled Minecraft under build/) which would
// otherwise be baked into every felis binary. Keep them explicit — add a source
// directory here, never a parent.
//
//go:embed deploy/game-stack.lock
//go:embed deploy/limbo/Dockerfile deploy/limbo/entrypoint.sh
//go:embed deploy/lobby/Dockerfile deploy/lobby/entrypoint.sh
//go:embed deploy/paper/Dockerfile deploy/paper/entrypoint.sh
//go:embed plugins/limbo/build.gradle plugins/limbo/settings.gradle plugins/limbo/src
//go:embed plugins/paper/build.gradle plugins/paper/settings.gradle plugins/paper/src
//go:embed plugins/velocity/build.gradle plugins/velocity/settings.gradle plugins/velocity/src
//go:embed plugins/shared/src
var gameStackAssets embed.FS

// GameStackTar streams the embedded game-stack sources as a tar, rooted so that
// `tar -x` reproduces the repo-relative layout the two Dockerfiles expect
// (deploy/limbo/..., plugins/shared/...). bootstrap.sh extracts it into a temp dir
// and uses that as the docker build context when it has no source checkout.
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
		// Mode 0644 for everything: entrypoint.sh is invoked as `sh <file>` by all
		// three Dockerfiles precisely because the +x bit does not survive a Windows
		// checkout, so nothing here needs to be executable.
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
