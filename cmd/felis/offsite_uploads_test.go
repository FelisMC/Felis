package main

import (
	"os"
	"path/filepath"
	"testing"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/platform"
)

func TestARebuildAdoptsTheUploadsVolumeOnlyWithoutAnS3Store(t *testing.T) {
	// The [registry] a fresh install writes: nothing about uploads, so the default applies.
	path := filepath.Join(t.TempDir(), "felis.toml")
	if err := os.WriteFile(path, []byte("[database]\nurl = \"postgres://felis@127.0.0.1/felis\"\n[server]\nroot_domain = \"example.com\"\n[registry]\nurl = \"registry.felis.svc:5000\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		reg  config.RegistryConfig
		want bool
	}{
		{"a fresh install, whose placeholder store answers 503", fresh.Registry, true},
		{"uploads already on the volume", config.RegistryConfig{UserUploadsContext: platform.UploadsLocalPath}, false},
		{"an S3 store set up in setup", config.RegistryConfig{UserUploadsContext: "s3://uploads", S3: config.RegistryS3Config{Endpoint: "https://s3.example", AccessKeyRef: platform.UploadsS3AccessKeyEnv, SecretKeyRef: platform.UploadsS3SecretKeyEnv}}, false},
		{"AWS S3, which needs no endpoint", config.RegistryConfig{UserUploadsContext: "s3://uploads", S3: config.RegistryS3Config{Region: "eu-west-1", AccessKeyRef: platform.UploadsS3AccessKeyEnv, SecretKeyRef: platform.UploadsS3SecretKeyEnv}}, false},
	}
	for _, c := range cases {
		if got := adoptsUploadsVolume(c.reg); got != c.want {
			t.Errorf("%s: adopts the volume = %v, want %v", c.name, got, c.want)
		}
	}
}
