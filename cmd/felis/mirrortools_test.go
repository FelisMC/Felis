package main

import (
	"context"
	"slices"
	"testing"

	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/config"
)

func TestSelectToolsTrimsBuildToolsWhenDisabled(t *testing.T) {
	runtimesOnly, err := selectTools("", false)
	if err != nil {
		t.Fatalf("selectTools(runtimes only): %v", err)
	}
	if len(runtimesOnly) != len(build.RuntimeImages) {
		t.Fatalf("got %d tools, want %d runtime images", len(runtimesOnly), len(build.RuntimeImages))
	}
	for _, tool := range runtimesOnly {
		if slices.ContainsFunc(build.Tools, func(bt build.Tool) bool { return bt.Name == tool.Name }) {
			t.Errorf("build tool %q must not be included when custom images are off", tool.Name)
		}
	}

	allTools, err := selectTools("", true)
	if err != nil {
		t.Fatalf("selectTools(all): %v", err)
	}
	if len(allTools) != len(build.Tools)+len(build.RuntimeImages) {
		t.Fatalf("got %d tools, want %d", len(allTools), len(build.Tools)+len(build.RuntimeImages))
	}

	explicit, err := selectTools("trivy", false)
	if err != nil || len(explicit) != 1 || explicit[0].Name != "trivy" {
		t.Fatalf("explicit -only trivy = %+v, err=%v", explicit, err)
	}
}

func TestResolveIncludeBuildTools(t *testing.T) {
	ctx := context.Background()
	f, tr := false, true
	cfgOff := &config.Config{Database: config.DatabaseConfig{URL: "postgres://db"}, Features: config.FeaturesConfig{CustomImages: &f}}
	cfgOn := &config.Config{Database: config.DatabaseConfig{URL: "postgres://db"}, Features: config.FeaturesConfig{CustomImages: &tr}}
	noDB := func(context.Context, string) (bool, bool) { return false, false }
	dbOn := func(context.Context, string) (bool, bool) { return true, true }
	dbOff := func(context.Context, string) (bool, bool) { return false, true }

	for _, tc := range []struct {
		name string
		mode string
		cfg  *config.Config
		read func(context.Context, string) (bool, bool)
		want bool
	}{
		{"auto without config keeps full mirror", "auto", nil, noDB, true},
		{"auto with config off", "auto", cfgOff, noDB, false},
		{"auto with config on", "auto", cfgOn, noDB, true},
		{"db true overrides config off", "auto", cfgOff, dbOn, true},
		{"db false overrides config on", "auto", cfgOn, dbOff, false},
		{"explicit true overrides config off", "true", cfgOff, dbOff, true},
		{"explicit false overrides config on", "false", cfgOn, dbOn, false},
	} {
		got, err := resolveIncludeBuildTools(ctx, tc.mode, tc.cfg, tc.read)
		if err != nil || got != tc.want {
			t.Errorf("%s: got (%v, %v), want (%v, nil)", tc.name, got, err, tc.want)
		}
	}

	if _, err := resolveIncludeBuildTools(ctx, "maybe", cfgOff, noDB); err == nil {
		t.Error("expected error for invalid -include-build-tools value")
	}
}
