package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// mkAccess builds an API whose "survival" server is owned by owner1 and Ready,
// with a fresh fakeConsole wired — the shared fixture for the §7 access tests.
func mkAccess(t *testing.T) (*API, *fakeRepo, *fakeCluster, *fakeConsole) {
	t.Helper()
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Running", Ready: true}
	console := &fakeConsole{reply: "ok"}
	api := newTestAPI(repo, cl)
	api.Console = console
	return api, repo, cl, console
}

var accessOwner = &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}

// TestAccessTranslation pins the structured-field → RCON-command translation for
// every §7 action. The exact gotCommand is the contract LuckPerms / vanilla will
// receive, so each variant that hides a bug is checked: a permission grant with
// an OMITTED value must become "... true" (not the *bool zero "... false", which
// would be a silent DENY), an explicit false must stay false, and an optional
// world must append " world=<w>" only when present.
func TestAccessTranslation(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		body    string
		wantCmd string
		wantLbl string // audit action label
	}{
		{"whitelist add", "/api/v1/servers/survival/access/whitelist",
			`{"action":"add","player":"Steve"}`, "whitelist add Steve", "access.whitelist.add"},
		{"whitelist remove", "/api/v1/servers/survival/access/whitelist",
			`{"action":"remove","player":"Steve"}`, "whitelist remove Steve", "access.whitelist.remove"},
		{"ban", "/api/v1/servers/survival/access/ban",
			`{"action":"ban","player":"Griefer_99"}`, "ban Griefer_99", "access.ban.ban"},
		{"pardon", "/api/v1/servers/survival/access/ban",
			`{"action":"pardon","player":"Griefer_99"}`, "pardon Griefer_99", "access.ban.pardon"},
		// The *bool trap: omitted value defaults to true (grant), NOT false (deny).
		{"permission set default grant", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"Steve","node":"essentials.fly"}`,
			"lp user Steve permission set essentials.fly true", "access.permission.set"},
		{"permission set explicit false", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"Steve","node":"essentials.fly","value":false}`,
			"lp user Steve permission set essentials.fly false", "access.permission.set"},
		{"permission set with world", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"Steve","node":"essentials.fly","world":"nether","value":true}`,
			"lp user Steve permission set essentials.fly true world=nether", "access.permission.set"},
		{"permission unset", "/api/v1/servers/survival/access/permission",
			`{"action":"unset","player":"Steve","node":"worldedit.*"}`,
			"lp user Steve permission unset worldedit.*", "access.permission.unset"},
		{"permission unset with world", "/api/v1/servers/survival/access/permission",
			`{"action":"unset","player":"Steve","node":"essentials.fly","world":"nether"}`,
			"lp user Steve permission unset essentials.fly world=nether", "access.permission.unset"},
		{"group add", "/api/v1/servers/survival/access/group",
			`{"action":"add","player":"Steve","group":"vip"}`, "lp user Steve parent add vip", "access.group.add"},
		{"group remove", "/api/v1/servers/survival/access/group",
			`{"action":"remove","player":"Steve","group":"vip"}`, "lp user Steve parent remove vip", "access.group.remove"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, repo, _, console := mkAccess(t)
			api.External = staticExternal{p: accessOwner}
			w := do(api.ExternalHandler(), "POST", tc.path, tc.body, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d body %s", w.Code, w.Body.String())
			}
			if console.calls != 1 {
				t.Fatalf("console calls = %d, want 1", console.calls)
			}
			if console.gotName != "survival" || console.gotCommand != tc.wantCmd {
				t.Fatalf("console saw (%q,%q), want (survival,%q)", console.gotName, console.gotCommand, tc.wantCmd)
			}
			if len(repo.audits) != 1 || repo.audits[0].Action != tc.wantLbl || repo.audits[0].Actor != "owner1@example.net" {
				t.Fatalf("audit = %+v, want action %q", repo.audits, tc.wantLbl)
			}
		})
	}
}

// TestAccessInjectionRejected is the security verification: a space, newline, or
// other off-charset character in ANY structured field must be rejected with 400
// AND must never reach RCON. Without per-field validation each of these inputs
// would splice a second command into the assembled line — so this matrix, not the
// happy-path gotCommand assert, is what proves the injection defence.
func TestAccessInjectionRejected(t *testing.T) {
	cases := []struct {
		name, path, body string
	}{
		// player field, across the routes that take one.
		{"whitelist player space", "/api/v1/servers/survival/access/whitelist", `{"action":"add","player":"ev il"}`},
		{"whitelist player newline", "/api/v1/servers/survival/access/whitelist", `{"action":"add","player":"ev\nop x"}`},
		{"ban player semicolon", "/api/v1/servers/survival/access/ban", `{"action":"ban","player":"ev;il"}`},
		{"ban player space", "/api/v1/servers/survival/access/ban", `{"action":"ban","player":"ev il"}`},
		{"permission player space", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"ev il","node":"essentials.fly"}`},
		{"group player newline", "/api/v1/servers/survival/access/group",
			`{"action":"add","player":"ev\nx","group":"vip"}`},
		// node field.
		{"permission node space", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"Steve","node":"essentials fly"}`},
		{"permission node newline", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"Steve","node":"essentials.fly\nop x"}`},
		{"permission node equals", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"Steve","node":"a=b"}`},
		// world field (optional — but when present, still validated).
		{"permission world space", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"Steve","node":"essentials.fly","world":"ne ther"}`},
		{"permission world newline", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"Steve","node":"essentials.fly","world":"a\nb"}`},
		// group field.
		{"group group space", "/api/v1/servers/survival/access/group",
			`{"action":"add","player":"Steve","group":"vi p"}`},
		{"group group newline", "/api/v1/servers/survival/access/group",
			`{"action":"add","player":"Steve","group":"a\nb"}`},
		// Trailing / embedded control characters: the anchored regex uses Go's `$`
		// (= \z, absolute end), so even a SINGLE trailing newline after an otherwise
		// valid name is rejected — a Perl `$` (\Z) would have let "Steve\n" through.
		{"whitelist player trailing newline", "/api/v1/servers/survival/access/whitelist",
			`{"action":"add","player":"Steve\n"}`},
		{"whitelist player carriage return", "/api/v1/servers/survival/access/whitelist",
			`{"action":"add","player":"Steve\r"}`},
		{"whitelist player tab", "/api/v1/servers/survival/access/whitelist",
			`{"action":"add","player":"Ste\tve"}`},
		// Length bounds: a name past 16 chars / node past 64 chars must be rejected,
		// not truncated, so an over-long field can never carry a smuggled tail.
		{"whitelist player over 16 chars", "/api/v1/servers/survival/access/whitelist",
			`{"action":"add","player":"AAAAAAAAAAAAAAAAA"}`},
		{"permission node over 64 chars", "/api/v1/servers/survival/access/permission",
			`{"action":"set","player":"Steve","node":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, _, _, console := mkAccess(t)
			api.External = staticExternal{p: accessOwner}
			w := do(api.ExternalHandler(), "POST", tc.path, tc.body, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d body %s, want 400", w.Code, w.Body.String())
			}
			if console.calls != 0 {
				t.Fatalf("an off-charset field must not reach RCON (calls = %d)", console.calls)
			}
		})
	}
}

// TestAccessUnknownAction rejects an action enum value no handler understands,
// before any RCON contact.
func TestAccessUnknownAction(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"whitelist", "/api/v1/servers/survival/access/whitelist", `{"action":"frobnicate","player":"Steve"}`},
		{"ban", "/api/v1/servers/survival/access/ban", `{"action":"frobnicate","player":"Steve"}`},
		{"permission", "/api/v1/servers/survival/access/permission",
			`{"action":"frobnicate","player":"Steve","node":"essentials.fly"}`},
		{"group", "/api/v1/servers/survival/access/group", `{"action":"frobnicate","player":"Steve","group":"vip"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, _, _, console := mkAccess(t)
			api.External = staticExternal{p: accessOwner}
			w := do(api.ExternalHandler(), "POST", tc.path, tc.body, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400", w.Code)
			}
			if console.calls != 0 {
				t.Fatalf("an unknown action must not reach RCON (calls = %d)", console.calls)
			}
		})
	}
}

// TestAccessUnknownField proves decodeJSON's strict mode locks the body shape for
// every mutating access route — no extra knob can ride in.
func TestAccessUnknownField(t *testing.T) {
	api, _, _, console := mkAccess(t)
	api.External = staticExternal{p: accessOwner}
	w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/access/whitelist",
		`{"action":"add","player":"Steve","extra":1}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	if console.calls != 0 {
		t.Fatal("a body with an unknown field must not reach RCON")
	}
}

// TestAccessOwnerGate proves the shared issueAccessCommand gate: an owner and an
// admin pass (on their own / on any node), a non-owner non-admin is rejected with
// 403 and never reaches RCON. The gate is the same one handleCommand uses, so one
// route exercises it for all five.
func TestAccessOwnerGate(t *testing.T) {
	body := `{"action":"add","player":"Steve"}`
	path := "/api/v1/servers/survival/access/whitelist"

	t.Run("owner -> 200", func(t *testing.T) {
		api, _, _, console := mkAccess(t)
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "POST", path, body, nil)
		if w.Code != http.StatusOK || console.calls != 1 {
			t.Fatalf("code = %d calls = %d", w.Code, console.calls)
		}
	})

	t.Run("admin on another's node -> 200", func(t *testing.T) {
		api, _, _, console := mkAccess(t)
		api.External = staticExternal{p: &Principal{UserID: "admin1", Email: "admin1@example.net",
			Role: "admin", ViaAdminAccess: true}}
		w := do(api.ExternalHandler(), "POST", path, body, nil)
		if w.Code != http.StatusOK || console.calls != 1 {
			t.Fatalf("code = %d calls = %d", w.Code, console.calls)
		}
	})

	t.Run("non-owner -> 403, no RCON call", func(t *testing.T) {
		api, _, _, console := mkAccess(t)
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "POST", path, body, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if console.calls != 0 {
			t.Fatal("a forbidden caller must not reach RCON")
		}
	})
}

// TestAccessReadinessAndFailures covers the non-happy lifecycle states the shared
// helper maps: unknown server → 404, stopped server → 409 (no RCON), unreachable
// console → 503 (no audit), nil console → 503.
func TestAccessReadinessAndFailures(t *testing.T) {
	body := `{"action":"add","player":"Steve"}`
	path := "/api/v1/servers/survival/access/whitelist"

	t.Run("unknown server -> 404", func(t *testing.T) {
		api, _, _, _ := mkAccess(t)
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/missing/access/whitelist", body, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})

	t.Run("not running -> 409 not_running, no RCON call", func(t *testing.T) {
		api, _, cl, console := mkAccess(t)
		cl.byName["survival"].Ready = false
		cl.byName["survival"].Phase = "Stopped"
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "POST", path, body, nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "not_running" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if console.calls != 0 {
			t.Fatal("a non-ready server must not reach RCON")
		}
	})

	t.Run("console unreachable -> 503, no audit", func(t *testing.T) {
		api, repo, _, console := mkAccess(t)
		console.err = ErrConsoleUnavailable
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "POST", path, body, nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "console_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if len(repo.audits) != 0 {
			t.Fatalf("a failed command must not audit: %+v", repo.audits)
		}
	})

	t.Run("nil Console -> 503", func(t *testing.T) {
		api, _, _, _ := mkAccess(t)
		api.Console = nil
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "POST", path, body, nil)
		if w.Code != http.StatusServiceUnavailable || decodeErr(t, w) != "console_unavailable" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
}

// TestAccessWhitelistList exercises the read projector: GET returns the parsed
// player list plus the raw reply, and is owner-gated like the writes.
func TestAccessWhitelistList(t *testing.T) {
	t.Run("parses players and returns raw output", func(t *testing.T) {
		api, repo, _, console := mkAccess(t)
		console.reply = "There are 2 whitelisted player(s): Steve, Alex"
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/access/whitelist", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		var resp struct {
			Name    string   `json:"name"`
			Players []string `json:"players"`
			Output  string   `json:"output"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if resp.Name != "survival" || resp.Output != console.reply {
			t.Fatalf("unexpected response %+v", resp)
		}
		if len(resp.Players) != 2 || resp.Players[0] != "Steve" || resp.Players[1] != "Alex" {
			t.Fatalf("players = %#v, want [Steve Alex]", resp.Players)
		}
		if console.gotCommand != "whitelist list" {
			t.Fatalf("console got %q, want %q", console.gotCommand, "whitelist list")
		}
		// A read must never write the audit trail — only the mutating routes audit.
		if len(repo.audits) != 0 {
			t.Fatalf("GET whitelist must not audit: %+v", repo.audits)
		}
	})

	t.Run("empty whitelist -> [] not null", func(t *testing.T) {
		api, repo, _, console := mkAccess(t)
		console.reply = "There are no whitelisted players"
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/access/whitelist", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		// players must render as [] (non-nil) so the client never sees null.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if string(raw["players"]) != "[]" {
			t.Fatalf("players = %s, want []", raw["players"])
		}
		if len(repo.audits) != 0 {
			t.Fatalf("GET whitelist must not audit: %+v", repo.audits)
		}
	})

	t.Run("non-owner -> 403, no RCON call", func(t *testing.T) {
		api, _, _, console := mkAccess(t)
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/access/whitelist", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if console.calls != 0 {
			t.Fatal("a forbidden caller must not reach RCON")
		}
	})
}

// TestParseWhitelistOutput unit-tests the vanilla parser directly, including the
// formats issueAccessCommand never produces but a real server might.
func TestParseWhitelistOutput(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"There are 2 whitelisted player(s): Steve, Alex", []string{"Steve", "Alex"}},
		{"There are 1 whitelisted player(s): Steve", []string{"Steve"}},
		{"There are no whitelisted players", []string{}},
		{"", []string{}},
		{"There are 0 whitelisted player(s):", []string{}},
		{"Names: a,  b ,c", []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		got := parseWhitelistOutput(tc.in)
		if got == nil {
			t.Fatalf("parseWhitelistOutput(%q) = nil, want non-nil slice", tc.in)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("parseWhitelistOutput(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("parseWhitelistOutput(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
