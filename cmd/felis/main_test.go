package main

import (
	"os"
	"testing"
)

func TestEnsureHostBinDirOnPath(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/usr/sbin:/usr/bin", "/usr/sbin:/usr/bin:/usr/local/bin"},
		{"/usr/local/bin:/usr/bin", "/usr/local/bin:/usr/bin"},
		{"/opt/k3s:/usr/bin:/usr/local/bin", "/opt/k3s:/usr/bin:/usr/local/bin"},
		{"", "/usr/local/bin"},
	} {
		t.Setenv("PATH", c.in)
		ensureHostBinDirOnPath()
		if got := os.Getenv("PATH"); got != c.want {
			t.Errorf("PATH %q became %q, want %q", c.in, got, c.want)
		}
	}
}
