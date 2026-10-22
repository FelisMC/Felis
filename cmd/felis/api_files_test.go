package main

import (
	"testing"

	"felis.lolicon.best/internal/config"
)

func TestFileEditorRunsAsGameUser(t *testing.T) {
	cfg := fileEditConfig(&config.Config{}, "felis:test")
	if cfg.RunAsUser != 1000 || cfg.RunAsGroup != 1000 || cfg.FSGroup != 0 {
		t.Fatalf("file editor identity: uid=%d gid=%d fsGroup=%d", cfg.RunAsUser, cfg.RunAsGroup, cfg.FSGroup)
	}
}
