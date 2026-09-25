package main

import (
	"testing"

	"felis.lolicon.best/internal/config"
)

func TestContextMaxBytesFollowsTheEdge(t *testing.T) {
	cases := []struct {
		name    string
		reg     config.RegistryConfig
		auth    config.AuthConfig
		want    int64
		wantErr bool
	}{
		{name: "direct install keeps the package default", want: 0},
		{name: "access audience means the Cloudflare edge", auth: config.AuthConfig{AccessJWTAud: "aud-1"}, want: 99614720},
		{name: "CF-Connecting-IP header means the Cloudflare edge", auth: config.AuthConfig{ClientIPHeader: "cf-connecting-ip"}, want: 99614720},
		{name: "an operator proxy keeps the package default", auth: config.AuthConfig{ClientIPHeader: "X-Forwarded-For"}, want: 0},
		{name: "explicit value wins over the edge default", reg: config.RegistryConfig{ContextMaxBytes: "50Mi"}, auth: config.AuthConfig{AccessJWTAud: "aud-1"}, want: 52428800},
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
