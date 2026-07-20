package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// version is the build stamp injected at link time via
//
//	-ldflags "-X main.version=v1.2.3"        (release channel: the tag verbatim)
//	-ldflags "-X main.version=v1.2.3+g1a2b3c4" (dev channel: tag + build metadata)
//
// deploy/bootstrap.sh computes it per install channel (FELIS_VERSION_BOOTSTRAP):
// the release channel stamps the resolved tag verbatim (v1.2.3), the dev channel
// stamps "<latest-tag>+g<short-sha>". It stays "dev" for an un-stamped local
// `go build`, where ReadBuildInfo below still surfaces the vcs revision.
//
// NOT `git describe`, for two reasons that both bite. Its "<tag>-<n>-g<sha>" form
// puts the distance in the PRERELEASE field, which sorts BELOW the bare tag, so a
// dev build ahead of v1.2.3 would compare as older than v1.2.3 and `felis update`
// would propose "upgrading" onto the release it already contains — hence "+", which
// is build metadata and ignored for ordering. And bootstrap's primary clone is
// --depth 1, which carries no tags, so describe would fall back to a bare SHA that
// updates.Parse rejects outright.
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

// vcsRevision returns the git commit the binary was built from when the build
// carried VCS stamping (local `go build` in a checkout; the docker build strips
// .git, so there the ldflag version carries the identity instead).
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
