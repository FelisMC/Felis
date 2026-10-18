package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// Stamp development commits as +gSHA metadata so they do not sort below their release tag.
var version = "dev"

// cmdVersion prints the build stamp. It takes no flags and never touches the
// cluster, so it is safe to run as any user (unlike setup/breakGlass).
func cmdVersion(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "felis %s\n", resolvedVersion())
	fmt.Fprintf(stdout, "  go:       %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if rev, ok := vcsRevision(); ok {
		fmt.Fprintf(stdout, "  revision: %s\n", rev)
	}
	return 0
}

// resolvedVersion prefers the ldflag stamp, then the module version recorded by
// `go install`, and only reports "unknown" when neither is present.
func resolvedVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

// Use Go VCS stamping for local builds; Docker builds obtain identity from ldflags.
func vcsRevision() (string, bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value, true
		}
	}
	return "", false
}
