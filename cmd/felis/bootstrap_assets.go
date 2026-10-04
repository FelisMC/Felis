package main

import (
	"fmt"
	"io"

	felis "felis.lolicon.best"
)

func cmdBootstrapAssets(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "felis bootstrap-assets: usage: felis bootstrap-assets crd|game-stack|config-keys")
		return 2
	}
	switch args[0] {
	case "config-keys":
		// Optional keys the installer may emit; older binaries reject this verb.
		_, err := fmt.Fprintln(stdout, "velocity.game_version")
		if err != nil {
			fmt.Fprintf(stderr, "felis bootstrap-assets: write: %v\n", err)
			return 1
		}
	case "crd":
		crd, err := felis.MinecraftServerCRD()
		if err != nil {
			fmt.Fprintf(stderr, "felis bootstrap-assets: %v\n", err)
			return 1
		}
		if _, err := stdout.Write(crd); err != nil {
			fmt.Fprintf(stderr, "felis bootstrap-assets: write: %v\n", err)
			return 1
		}
	case "game-stack":
		// A tar on stdout, not a directory on disk: the caller (bootstrap.sh) is the
		// one that knows where a build context may live, and piping keeps this command
		// side-effect-free.
		if err := felis.GameStackTar(stdout); err != nil {
			fmt.Fprintf(stderr, "felis bootstrap-assets: %v\n", err)
			return 1
		}
	default:
		fmt.Fprintln(stderr, "felis bootstrap-assets: usage: felis bootstrap-assets crd|game-stack|config-keys")
		return 2
	}
	return 0
}
