package main

import (
	"errors"
	"testing"

	"felis.lolicon.best/internal/updates"
)

func TestDevelopmentBuildsFollowMainUnlessATargetIsNamed(t *testing.T) {
	for _, tc := range []struct {
		version      string
		dev          bool
		release, ref string
		want         string
	}{
		{version: "v1.2.3", want: ""},
		{version: "v1.0.0-earlyAccess", want: ""},
		{version: "dev", want: ""},
		{version: "v0.0.0+gunknown", want: ""},
		{version: "v1.2.3+g1a2b3c4", want: "main"},
		{version: "v1.0.0-earlyAccess-3-g1a2b3c4", want: "main"},
		{version: "v1.2.3+g1a2b3c4-dirty", want: "main"},
		{version: "v1.2.3+g1a2b3c4", release: "v0.2.0", want: ""},
		{version: "v1.2.3+g1a2b3c4", ref: "feature/x", want: "feature/x"},
		{version: "v1.2.3", dev: true, want: "main"},
	} {
		o := updateOptions{release: tc.release, ref: tc.ref}
		o.followMain(tc.dev, tc.version)
		if o.ref != tc.want || o.release != tc.release {
			t.Errorf("%s (dev %v, release %q, ref %q): target ref %q, release %q; want ref %q", tc.version, tc.dev, tc.release, tc.ref, o.ref, o.release, tc.want)
		}
	}
}

func TestMainCheckOffersTheNewestCommitOnlyWhenMainMoved(t *testing.T) {
	const stamp = "v0.0.0+g1a2b3c4"
	same := "1a2b3c4" + "0123456789abcdef0123456789abcdef0"
	moved := "fedcba9876543210fedcba9876543210fedcba98"
	for _, tc := range []struct {
		name string
		c    mainCheck
		want updates.ComponentStatus
	}{
		{"main is at the installed commit", mainCheck{commit: "1a2b3c4", head: same},
			updates.ComponentStatus{Name: "felis-api", Current: stamp, State: updates.StateCurrent, Selector: "panel"}},
		{"main moved", mainCheck{commit: "1a2b3c4", head: moved},
			updates.ComponentStatus{Name: "felis-api", Current: stamp, Latest: moved, State: updates.StateAvailable, Selector: "panel"}},
		{"main unreachable", mainCheck{commit: "1a2b3c4", err: errors.New("github: resolve ref \"main\": HTTP 503")},
			updates.ComponentStatus{Name: "felis-api", Current: stamp, State: updates.StateUnknown, Selector: "panel", Error: "github: resolve ref \"main\": HTTP 503"}},
	} {
		if got := tc.c.status(stamp); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
