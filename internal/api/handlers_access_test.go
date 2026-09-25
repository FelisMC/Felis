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
		// kick has no action field — a single verb — so its label is "access.kick".
		{"kick", "/api/v1/servers/survival/access/kick",
			`{"player":"Griefer_99"}`, "kick Griefer_99", "access.kick"},
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
		{"kick player space", "/api/v1/servers/survival/access/kick", `{"player":"ev il"}`},
		{"kick player newline", "/api/v1/servers/survival/access/kick", `{"player":"ev\nop x"}`},
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

// TestAccessPlayers exercises the online-roster read projector: GET runs "list"
// and returns the online/max tally, the parsed names, and the raw reply, owner-
// gated like the writes and never auditing.
func TestAccessPlayers(t *testing.T) {
	t.Run("parses tally, names and raw output", func(t *testing.T) {
		api, repo, _, console := mkAccess(t)
		console.reply = "There are 3 of a max of 20 players online: alice, bob, carol"
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/access/players", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		var resp struct {
			Name    string   `json:"name"`
			Online  int      `json:"online"`
			Max     int      `json:"max"`
			Players []string `json:"players"`
			Output  string   `json:"output"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		if resp.Name != "survival" || resp.Online != 3 || resp.Max != 20 || resp.Output != console.reply {
			t.Fatalf("unexpected response %+v", resp)
		}
		if len(resp.Players) != 3 || resp.Players[0] != "alice" || resp.Players[2] != "carol" {
			t.Fatalf("players = %#v, want [alice bob carol]", resp.Players)
		}
		if console.gotCommand != "list" {
			t.Fatalf("console got %q, want %q", console.gotCommand, "list")
		}
		if len(repo.audits) != 0 {
			t.Fatalf("GET players must not audit: %+v", repo.audits)
		}
	})

	t.Run("empty server -> [] not null", func(t *testing.T) {
		api, _, _, console := mkAccess(t)
		console.reply = "There are 0 of a max of 20 players online:"
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/access/players", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if string(raw["players"]) != "[]" {
			t.Fatalf("players = %s, want []", raw["players"])
		}
	})

	t.Run("non-owner -> 403, no RCON call", func(t *testing.T) {
		api, _, _, console := mkAccess(t)
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/access/players", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if console.calls != 0 {
			t.Fatal("a forbidden caller must not reach RCON")
		}
	})
}

// TestParseListOutput unit-tests the "list" parser directly, including formats
// issueAccessCommand never produces but a real server might. Names parse from the
// colon tail independently of the count line, so both are pinned separately.
func TestParseListOutput(t *testing.T) {
	cases := []struct {
		in          string
		online, max int
		want        []string
	}{
		{"There are 3 of a max of 20 players online: alice, bob, carol", 3, 20, []string{"alice", "bob", "carol"}},
		{"There are 1 of a max of 20 players online: Steve", 1, 20, []string{"Steve"}},
		{"There are 0 of a max of 20 players online:", 0, 20, []string{}},
		{"There are 0 of a max of 20 players online", 0, 20, []string{}},
		{"", 0, 0, []string{}},
		// A colon tail with no recognised count line still yields names, tally 0.
		{"Online: a,  b ,c", 0, 0, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		online, max, got := parseListOutput(tc.in)
		if got == nil {
			t.Fatalf("parseListOutput(%q) names = nil, want non-nil slice", tc.in)
		}
		if online != tc.online || max != tc.max {
			t.Fatalf("parseListOutput(%q) = (%d,%d), want (%d,%d)", tc.in, online, max, tc.online, tc.max)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("parseListOutput(%q) names = %#v, want %#v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("parseListOutput(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// TestAccessBanList exercises the ban-list read projector: GET runs "banlist",
// returns the parsed names plus the raw reply, is owner-gated, and never audits.
func TestAccessBanList(t *testing.T) {
	t.Run("parses players and returns raw output", func(t *testing.T) {
		api, repo, _, console := mkAccess(t)
		console.reply = "There are 2 ban(s):\nSteve was banned by Server: Griefing\nAlex was banned by Server: Spam"
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/access/ban", "", nil)
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
		if console.gotCommand != "banlist" {
			t.Fatalf("console got %q, want %q", console.gotCommand, "banlist")
		}
		if len(repo.audits) != 0 {
			t.Fatalf("GET ban must not audit: %+v", repo.audits)
		}
	})

	t.Run("empty ban list -> [] not null", func(t *testing.T) {
		api, _, _, console := mkAccess(t)
		console.reply = "There are no bans."
		api.External = staticExternal{p: accessOwner}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/access/ban", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if string(raw["players"]) != "[]" {
			t.Fatalf("players = %s, want []", raw["players"])
		}
	})

	t.Run("non-owner -> 403, no RCON call", func(t *testing.T) {
		api, _, _, console := mkAccess(t)
		api.External = staticExternal{p: &Principal{UserID: "stranger", Role: "user"}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/access/ban", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if console.calls != 0 {
			t.Fatal("a forbidden caller must not reach RCON")
		}
	})
}

// TestParseBanlistOutput unit-tests the ban parser directly. The critical cases are
// the SEPARATOR variants: "banlist" emits one feedback message per ban and RCON's
// concatenation separator is version-dependent, so the parser must return the same
// names whether entries are newline-, space-, or non-separated — and must never let
// the header or a reason's own colon/spaces leak into a name (a corrupt name would
// arm an off-charset pardon).
func TestParseBanlistOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			"newline-separated",
			"There are 2 ban(s):\nSteve was banned by Server: Griefing\nAlex was banned by Server: Spam",
			[]string{"Steve", "Alex"},
		},
		{
			// The separator the wire format might actually use: header + entries
			// concatenated with spaces, reasons carrying spaces of their own.
			"space-concatenated with spaced reasons",
			"There are 2 ban(s): Steve was banned by Server: griefing spawn Alex was banned by Server: spam",
			[]string{"Steve", "Alex"},
		},
		{"single ban", "There are 1 ban(s):\nNotch was banned by Console: rude", []string{"Notch"}},
		{"no bans", "There are no bans.", []string{}},
		{"empty", "", []string{}},
	}
	for _, tc := range cases {
		got := parseBanlistOutput(tc.in)
		if got == nil {
			t.Fatalf("%s: parseBanlistOutput(%q) = nil, want non-nil slice", tc.name, tc.in)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: parseBanlistOutput(%q) = %#v, want %#v", tc.name, tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: parseBanlistOutput(%q)[%d] = %q, want %q", tc.name, tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// TestLuckPermsMissingIsAConflict: on a server without LuckPerms every lp command
// is answered by the server's unknown-command reply, and each LuckPerms door must
// say so (409 luckperms_missing) with no audit of a change that never happened (#4).
// The replies are what the servers send: Paper 1.21's captured live from a paper
// server over RCON, and a Spigot one colored the way its console renders it. The
// empty reply LuckPerms itself gives (it answers after RCON has flushed) and an
// LP message that mentions "unknown command" mid-line both stay successes.
func TestLuckPermsMissingIsAConflict(t *testing.T) {
	doors := []struct{ name, method, path, body string }{
		{"permission", "POST", "/api/v1/servers/survival/access/permission", `{"action":"set","player":"Steve","node":"essentials.fly"}`},
		{"group", "POST", "/api/v1/servers/survival/access/group", `{"action":"add","player":"Steve","group":"vip"}`},
		{"info", "GET", "/api/v1/servers/survival/access/luckperms/Steve", ""},
	}
	replies := []struct {
		name    string
		reply   string
		missing bool
	}{
		{"paper", "Unknown or incomplete command. See below for error\nlp user Steve permission info<--[HERE]", true},
		{"older vanilla", "Unknown or incomplete command, see below for error\nlp user Steve<--[HERE]", true},
		{"spigot colored", "§fUnknown command. Type \"/help\" for help.", true},
		{"luckperms silent", "", false},
		{"luckperms message", "§7[§b§lL§3§lP§7]§r §7Another command is being executed; unknown command queue", false},
	}
	for _, d := range doors {
		for _, rp := range replies {
			t.Run(d.name+"/"+rp.name, func(t *testing.T) {
				api, repo, _, console := mkAccess(t)
				api.External = staticExternal{p: accessOwner}
				console.reply = rp.reply
				w := do(api.ExternalHandler(), d.method, d.path, d.body, nil)
				if !rp.missing {
					if w.Code != http.StatusOK {
						t.Fatalf("code = %d (%s), want 200", w.Code, w.Body.String())
					}
					return
				}
				if w.Code != http.StatusConflict || decodeErr(t, w) != "luckperms_missing" {
					t.Fatalf("code = %d (%s), want 409 luckperms_missing", w.Code, w.Body.String())
				}
				if len(repo.audits) != 0 {
					t.Fatalf("audited a change LuckPerms never made: %+v", repo.audits)
				}
			})
		}
	}
}
