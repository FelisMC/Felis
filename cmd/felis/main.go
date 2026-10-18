// Command felis dispatches the platform subcommands from one binary.
package main

import (
	"os"
	"path/filepath"
)

func main() {
	ensureHostBinDirOnPath()
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// hostBinDir is where deploy/bootstrap.sh installs felis, k3s and cloudflared.
const hostBinDir = "/usr/local/bin"

// Append the host binary directory to handle sudo secure_path while preserving PATH precedence.
func ensureHostBinDirOnPath() {
	path := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == hostBinDir {
			return
		}
	}
	if path == "" {
		os.Setenv("PATH", hostBinDir)
		return
	}
	os.Setenv("PATH", path+string(os.PathListSeparator)+hostBinDir)
}
