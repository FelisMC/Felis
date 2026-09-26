// Command felis is the single multi-call binary for the platform (spec §25):
// it dispatches to the api, operator, migrate, reaper, and apply subcommands.
// Building one binary keeps the shared packages (scheme, store, config) linked
// once and shipped in a single image.
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

// ensureHostBinDirOnPath appends hostBinDir to PATH when it is missing, so the
// k3s and cloudflared this binary execs are found beside it. sudo's secure_path
// on EL leaves /usr/local/bin out: `sudo /usr/local/bin/felis db backup` would
// otherwise run with no k3s to reach the database's pod through. Appended, so a
// PATH that names another k3s first keeps it.
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
