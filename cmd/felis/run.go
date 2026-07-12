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
  nano              Run the Felis-nano hasJoined multiplexer (multi-Yggdrasil, no control plane)
  reaper            Run the world reaper / backup batch
  restore           Extract a world archive into a world volume (internal Job entrypoint)
  backup            Archive a world into the backup store and record it (internal Job entrypoint)
  manifests         Render the control-plane RBAC + NetworkPolicy install bundle as YAML
  apply             Create a MinecraftServer CRD (direct K8s write; use -f server.json)
  setup             Run host bootstrap + first-run setup console (TUI; requires root/sudo)
  breakGlass        Open the local break-glass emergency console (TUI; requires root/sudo)

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
	case "nano":
		return cmdNano(rest, stdout, stderr)
	case "reaper":
		return cmdReaper(rest, stdout, stderr)
	case "restore":
		return cmdRestore(rest, stdout, stderr)
	case "backup":
		return cmdBackup(rest, stdout, stderr)
	case "manifests":
		return cmdManifests(rest, stdout, stderr)
	case "apply":
		return cmdApply(rest, stdout, stderr)
	case "setup":
		return cmdSetup(rest, stdout, stderr)
	case "breakGlass":
		return cmdBreakGlass(rest, stdout, stderr)
	case "bootstrap-assets":
		return cmdBootstrapAssets(rest, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "felis: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}
