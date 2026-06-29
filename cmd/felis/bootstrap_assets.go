package main

import (
	"fmt"
	"io"

	felis "felis.lolicon.best"
)

func cmdBootstrapAssets(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "crd" {
		fmt.Fprintln(stderr, "felis bootstrap-assets: usage: felis bootstrap-assets crd")
		return 2
	}
	crd, err := felis.MinecraftServerCRD()
	if err != nil {
		fmt.Fprintf(stderr, "felis bootstrap-assets: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(crd); err != nil {
		fmt.Fprintf(stderr, "felis bootstrap-assets: write: %v\n", err)
		return 1
	}
	return 0
}
