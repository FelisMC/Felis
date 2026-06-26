package main

import (
	"fmt"
	"io"
)

const usage = `felis — Kubernetes-native Minecraft server orchestration

Usage:
  felis <command> [flags]

Commands:
  migrate up        Apply embedded database migrations under an advisory lock
  operator          Run the MinecraftServer controller-manager
  api               Run the felis-api HTTP server
  reaper            Run the world reaper / backup batch
  restore           Extract a world archive into a world volume (internal Job entrypoint)
  manifests         Render the control-plane RBAC + NetworkPolicy install bundle as YAML
  apply             Apply a MinecraftServer manifest

Run "felis <command> -h" for command-specific flags.
`

// run dispatches a subcommand. It is separate from main so the router is
// testable without spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "migrate":
		return cmdMigrate(rest, stdout, stderr)
	case "operator":
		return cmdOperator(rest, stdout, stderr)
	case "api":
		return cmdAPI(rest, stdout, stderr)
	case "reaper":
		return cmdReaper(rest, stdout, stderr)
	case "restore":
		return cmdRestore(rest, stdout, stderr)
	case "manifests":
		return cmdManifests(rest, stdout, stderr)
	case "apply":
		return notImplemented("apply", "MinecraftServer manifest apply", stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "felis: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// notImplemented reports a subcommand that is wired into the CLI surface but
// whose implementation lands in a later phase. It fails loudly rather than
// pretending to do work.
func notImplemented(name, desc string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "felis %s: not implemented yet — %s\n", name, desc)
	return 3
}
