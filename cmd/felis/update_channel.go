package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"felis.lolicon.best/internal/updater"
	"felis.lolicon.best/internal/updates"
)

// devStamp matches the commit a development build is stamped with: +g<sha> or git-describe's -<n>-g<sha>.
var devStamp = regexp.MustCompile(`(?:\+g|-[0-9]+-g)([0-9a-f]{7,40})$`)

// devBuildCommit returns the commit of a development build; a host running one follows main.
func devBuildCommit(stamp string) (string, bool) {
	m := devStamp.FindStringSubmatch(strings.TrimSuffix(strings.TrimSpace(stamp), "-dirty"))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// followMain targets main when --dev is given, or when a development build names no other target.
func (o *updateOptions) followMain(dev bool, stamp string) {
	if _, devBuild := devBuildCommit(stamp); dev || (devBuild && o.release == "" && o.ref == "") {
		o.ref = "main"
	}
}

// channelTopology leaves felis-api out of the release lookup on a development build, which checkMain covers.
func channelTopology(dev bool) []updater.Spec {
	topo := updater.Topology()
	if !dev {
		return topo
	}
	return slices.DeleteFunc(topo, func(s updater.Spec) bool { return s.Name == "felis-api" })
}

// mainCheck places a development build's commit against the newest commit on main.
type mainCheck struct {
	commit, head string
	err          error
}

func checkMain(ctx context.Context, commit string) mainCheck {
	src, err := updater.NewInstallerSource(os.Getenv("FELIS_REPO_URL"))
	if err != nil {
		return mainCheck{commit: commit, err: err}
	}
	head, err := src.Resolve(ctx, "main")
	return mainCheck{commit: commit, head: head, err: err}
}

func (c mainCheck) moved() bool {
	return c.err == nil && !strings.HasPrefix(c.head, c.commit)
}

// status is the felis-api line a development build records for the panel; Latest is main's full SHA.
func (c mainCheck) status(stamp string) updates.ComponentStatus {
	cs := updates.ComponentStatus{Name: "felis-api", Current: stamp, State: updates.StateCurrent, Selector: "panel"}
	switch {
	case c.err != nil:
		cs.State, cs.Error = updates.StateUnknown, c.err.Error()
	case c.moved():
		cs.State, cs.Latest = updates.StateAvailable, c.head
	}
	return cs
}

func (c mainCheck) render(stamp string) string {
	switch {
	case c.err != nil:
		return fmt.Sprintf("%-13s newest main commit undiscoverable: %v\n", "felis-api", c.err)
	case c.moved():
		return fmt.Sprintf("%-13s %s -> main %s  newer commits on main (development build)\n", "felis-api", stamp, c.head[:12])
	default:
		return fmt.Sprintf("%-13s %s  up to date with main (development build)\n", "felis-api", stamp)
	}
}
