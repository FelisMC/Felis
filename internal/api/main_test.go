package api

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	mojangProfileAPI = "http://127.0.0.1:1/"
	code := m.Run()
	if code == 0 {
		contractCalls.Lock()
		violations, err := checkContract("../../api/openapi.yaml", contractCalls.list)
		contractCalls.Unlock()
		if err != nil {
			fmt.Fprintln(os.Stderr, "openapi contract:", err)
			code = 1
		}
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "openapi contract:", v)
		}
		if len(violations) > 0 {
			fmt.Fprintf(os.Stderr, "FAIL: %d exchange(s) disagree with api/openapi.yaml\n", len(violations))
			code = 1
		}
	}
	os.Exit(code)
}
