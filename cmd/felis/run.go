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
  files             List/read/write one file in a stopped server's world (internal Job entrypoint)
  fetch-context     Fetch and extract a submission's build context (internal Job entrypoint)
  manifests         Render the control-plane RBAC + NetworkPolicy install bundle as YAML
  apply             Create a MinecraftServer CRD (direct K8s write; use -f server.json)
  setup             Run host bootstrap + first-run setup console (TUI; requires root/sudo)
  converge          Fill in fields a newer desired spec added to already-installed system servers
  version           Print the build stamp of this binary
  update            Report which platform components have updates available
  breakGlass        Open the local break-glass emergency console (TUI; requires root/sudo)

Run "felis <command> -h" for command-specific flags.
`

// commands is the dispatch table. It is a map rather than a switch so the router's
// contents are DATA a test can compare against the usage text above: `version`
// shipped once as an implemented-but-unreachable command (cmdVersion existed with
// nothing routing to it), and a switch offers no way to notice that. Adding an entry
// here without documenting it in usage — or vice versa — now fails a test instead of
// shipping.
//
// The help aliases are deliberately NOT entries: they print usage rather than run a
// subcommand, and listing them would make the table disagree with the command list.
var commands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"migrate":          cmdMigrate,
	"operator":         cmdOperator,
	"api":              cmdAPI,
	"nano":             cmdNano,
	"reaper":           cmdReaper,
	"restore":          cmdRestore,
	"backup":           cmdBackup,
	"files":            cmdFiles,
	"fetch-context":    cmdFetchContext,
	"manifests":        cmdManifests,
	"apply":            cmdApply,
	"setup":            cmdSetup,
	"converge":         cmdConverge,
	"breakGlass":       cmdBreakGlass,
	"bootstrap-assets": cmdBootstrapAssets,
	"init-forwarding":  cmdInitForwarding,
	"version":          cmdVersion,
	"update":           cmdUpdate,
}

// run dispatches a subcommand. It is separate from main so the router is
// testable without spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	if fn, ok := commands[cmd]; ok {
		return fn(rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "felis: unknown command %q\n\n%s", cmd, usage)
	return 2
}
