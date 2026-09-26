package main

import (
	"os"
	"regexp"
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

// run is what the tests drive, so the PATH fix must sit in main, before it.
func TestMainFixesPathBeforeRunning(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`func main\(\) \{\n\tensureHostBinDirOnPath\(\)\n\tos\.Exit\(run\(`).Match(b) {
		t.Fatal("main does not call ensureHostBinDirOnPath before run")
	}
}
