package api

import (
	"os"
	"testing"
)

// TestMain points the premium-name lookup at an address nothing listens on before any test
// runs. A test that forgets stubMojangNames then fails the same way everywhere (lookup
// error, fail closed, rename) instead of asking the real api.mojang.com, whose answer
// changes the day someone buys the name and which CI may not reach at all.
func TestMain(m *testing.M) {
	mojangProfileAPI = "http://127.0.0.1:1/"
	os.Exit(m.Run())
}
