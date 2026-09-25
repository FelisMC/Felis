package api

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the premium-name lookup at an address nothing listens on before any test
// runs. A test that forgets stubMojangNames then fails the same way everywhere (lookup
// error, fail closed, rename) instead of asking the real api.mojang.com, whose answer
// changes the day someone buys the name and which CI may not reach at all.
func TestMain(m *testing.M) {
	mojangProfileAPI = "http://127.0.0.1:1/"
	code := m.Run()
	// Every exchange the handler tests made is then held to docs/openapi.yaml
	// (openapi_contract_test.go).
	if code == 0 {
		contractCalls.Lock()
		violations, err := checkContract("../../docs/openapi.yaml", contractCalls.list)
		contractCalls.Unlock()
		if err != nil {
			fmt.Fprintln(os.Stderr, "openapi contract:", err)
			code = 1
		}
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "openapi contract:", v)
		}
		if len(violations) > 0 {
			fmt.Fprintf(os.Stderr, "FAIL: %d exchange(s) disagree with docs/openapi.yaml\n", len(violations))
			code = 1
		}
	}
	os.Exit(code)
}
