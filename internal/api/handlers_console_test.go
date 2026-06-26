package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestConsoleCommand exercises the §8 RCON write handler end-to-end through the
// external app-tier router: ownership (owner/admin), the readiness gate, input
// validation (single line, bounded, no control chars), and the failure surface
// (unreachable console → 503). The real RCON dial is integration-only; this
// drives the handler against a fakeConsole.
func TestConsoleCommand(t *testing.T) {
	owner := &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

	// mk builds an API whose "survival" server is owned by owner1 and Ready, with a
	// fresh fakeConsole wired. Subtests override only what they need.
	mk := func() (*API, *fakeRepo, *fakeCluster, *fakeConsole) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Running", Ready: true}
		console := &fakeConsole{reply: "There are 3 of a max of 20 players online"}
		api := newTestAPI(repo, cl)
		api.Console = console
		return api, repo, cl, console
	}

	t.Run("owner runs command -> 200 + reply + audit", func(t *testing.T) {
		api, repo, _, console := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		var resp struct {
			Name   string `json:"name"`
			Output string `json:"output"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if resp.Name != "survival" || resp.Output != console.reply {
			t.Fatalf("unexpected response %+v", resp)
		}
		if console.gotName != "survival" || console.gotCommand != "list" {
			t.Fatalf("console saw (%q,%q), want (survival,list)", console.gotName, console.gotCommand)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "console.command" || repo.audits[0].Actor != "owner1@example.net" {
			t.Fatalf("audit not written as expected: %+v", repo.audits)
		}
	})

	t.Run("leading slash stripped before RCON", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"/say hi"}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if console.gotCommand != "say hi" {
			t.Fatalf("console got %q, want %q", console.gotCommand, "say hi")
		}
	})

	t.Run("admin runs command on another's server -> 200", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: &Principal{UserID: "admin1", Email: "admin1@example.net",
			Role: "admin", ViaAdminAccess: true}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if console.calls != 1 {
			t.Fatalf("admin command reached RCON %d times, want 1", console.calls)
		}
	})

	t.Run("non-owner -> 403, no RCON call", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if console.calls != 0 {
			t.Fatal("a forbidden caller must not reach RCON")
		}
	})

	t.Run("unknown server -> 404", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/missing/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("not ready -> 409 not_running, no RCON call", func(t *testing.T) {
		api, _, cl, console := mk()
		cl.byName["survival"].Ready = false
		cl.byName["survival"].Phase = "Stopped"
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_running" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if console.calls != 0 {
			t.Fatal("a non-ready server must not reach RCON")
		}
	})

	t.Run("empty command -> 400, no RCON call", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"   "}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
		if console.calls != 0 {
			t.Fatal("an empty command must not reach RCON")
		}
	})

	t.Run("slash-only command -> 400", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"/"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})

	t.Run("control character (newline) -> 400, no RCON call", func(t *testing.T) {
		api, _, _, console := mk()
		api.External = staticExternal{p: owner}
		// The \n is a JSON escape that decodes to a real newline; the handler must
		// reject it so one request cannot smuggle a second command.
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command",
			`{"command":"say hi\nop attacker"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if console.calls != 0 {
			t.Fatal("a control-char command must not reach RCON")
		}
	})

	t.Run("over-long command -> 400", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		long := strings.Repeat("a", maxConsoleCommandLen+1)
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command",
			`{"command":"`+long+`"}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})

	t.Run("unknown field -> 400", func(t *testing.T) {
		api, _, _, _ := mk()
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command",
			`{"command":"list","extra":1}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})

	t.Run("console unreachable -> 503 console_unavailable", func(t *testing.T) {
		api, repo, _, console := mk()
		console.err = ErrConsoleUnavailable
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "console_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		// A failed write must not be audited as a successful command.
		if len(repo.audits) != 0 {
			t.Fatalf("unreachable console must not audit: %+v", repo.audits)
		}
	})

	t.Run("nil Console -> 503", func(t *testing.T) {
		api, _, _, _ := mk()
		api.Console = nil
		api.External = staticExternal{p: owner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/command", `{"command":"list"}`, nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "console_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
}
