package main

import (
	"os"
	"path/filepath"
	"testing"

	"felis.lolicon.best/internal/config"
)

func TestContextMaxBytes(t *testing.T) {
	cases := []struct {
		name    string
		reg     config.RegistryConfig
		auth    config.AuthConfig
		want    int64
		wantErr bool
	}{
		{name: "direct install keeps the package default", want: 0},
		// The panel uploads in parts under the edge's 100 MB body limit, so the
		// Cloudflare edge keeps the full default.
		{name: "the Cloudflare edge keeps the package default", auth: config.AuthConfig{AccessJWTAud: "aud-1"}, want: 0},
		{name: "CF-Connecting-IP keeps the package default", auth: config.AuthConfig{ClientIPHeader: "cf-connecting-ip"}, want: 0},
		{name: "explicit value behind the edge", reg: config.RegistryConfig{ContextMaxBytes: "50Mi"}, auth: config.AuthConfig{AccessJWTAud: "aud-1"}, want: 52428800},
		{name: "explicit value on a direct install", reg: config.RegistryConfig{ContextMaxBytes: "2Gi"}, want: 2147483648},
		{name: "garbage is refused", reg: config.RegistryConfig{ContextMaxBytes: "lots"}, wantErr: true},
		{name: "zero is refused", reg: config.RegistryConfig{ContextMaxBytes: "0"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := contextMaxBytes(&config.Config{Registry: tc.reg, Auth: tc.auth})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("contextMaxBytes = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestUploadPartsDir(t *testing.T) {
	if got := uploadPartsDir("/var/lib/felis/uploads"); got != "/var/lib/felis/uploads/.parts" {
		t.Errorf("local store: parts dir = %q, want beside the contexts", got)
	}
	if got := uploadPartsDir("file:///srv/uploads"); got != "/srv/uploads/.parts" {
		t.Errorf("file:// store: parts dir = %q, want /srv/uploads/.parts", got)
	}
	// This machine has no /var/lib/felis/uploads mount, so an s3:// store falls
	// back to the temp dir.
	if _, err := os.Stat("/var/lib/felis/uploads"); err == nil {
		t.Skip("/var/lib/felis/uploads exists here")
	}
	if got := uploadPartsDir("s3://bucket/uploads"); got != filepath.Join(os.TempDir(), "felis-upload-parts") {
		t.Errorf("s3 store without the uploads mount: parts dir = %q", got)
	}
}
