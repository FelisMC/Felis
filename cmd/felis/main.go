// Command felis is the single multi-call binary for the platform (spec §25):
// it dispatches to the api, operator, migrate, reaper, and apply subcommands.
// Building one binary keeps the shared packages (scheme, store, config) linked
// once and shipped in a single image.
package main

import (
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
