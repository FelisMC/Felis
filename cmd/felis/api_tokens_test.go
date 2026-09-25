package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"felis.lolicon.best/internal/api"
)

// felis-api maps each env var onto the caller the Deployment feeds it for, so the
// build Job's token (FELIS_BUILD_TOKEN) authenticates as build and only as build.
func TestInternalCallerTokensReadEachCallersEnv(t *testing.T) {
	env := map[string]string{
		"FELIS_SERVICE_TOKEN": "v-tok",
		"FELIS_LIMBO_TOKEN":   "l-tok",
		"FELIS_BUILD_TOKEN":   " b-tok\n",
		"FELIS_OPS_TOKEN":     "o-tok",
	}
	auth, err := internalCallerTokens(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	for tok, want := range map[string]api.Caller{"v-tok": api.CallerVelocity, "l-tok": api.CallerLimbo, "b-tok": api.CallerBuild, "o-tok": api.CallerOps} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		if got, err := auth.Authenticate(r); err != nil || got != want {
			t.Errorf("%s: got (%q, %v), want %q", tok, got, err, want)
		}
	}

	// An install where the build namespace still holds a copy of the proxy's token
	// would let that copy act as the proxy; the api refuses to start on it.
	env["FELIS_BUILD_TOKEN"] = "v-tok"
	if _, err := internalCallerTokens(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "same value") {
		t.Fatalf("shared token: err = %v, want a same-value refusal", err)
	}
}
