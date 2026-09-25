package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"felis.lolicon.best/internal/naming"
)

// Access / permissions domain (spec §7). These endpoints let an owner manage
// who may join and what they may do on their OWN claimed node, and let an admin
// do the same on ANY node, by translating a small set of STRUCTURED fields into
// the live server's runtime authority — vanilla's whitelist/ban and the
// LuckPerms plugin — over the exact same owner-gated RCON path as POST
// /servers/{name}/command (handleCommand).
//
// Source-of-truth note (spec §1): the live MC server + LuckPerms is a THIRD
// authority, neither the CRD (lifecycle) nor Postgres (business). felis-api is
// only a command-issuer and read-projector here; it stores none of this state.
//
// The security difference from handleCommand is the whole point of this file.
// handleCommand validates ONE free-form line and forbids control characters so a
// newline cannot smuggle a second command. Here the command is ASSEMBLED from
// structured fields (player, node, world, group); a space or newline in any
// field would splice a second RCON command just the same. So every field is
// validated against a strict allow-list charset BEFORE it is ever concatenated
// into a command, and there is NO free-text field anywhere (a ban carries no
// reason string — that would be the one free-text injection vector). The
// anchored allow-list regexes are strictly stronger than handleCommand's
// control-character scan: Go's `$` is `\z` (absolute end, not `\Z`), so a
// trailing newline cannot sit before it and "player\n…" is rejected outright.
var (
	// mcNameRe matches a Java-edition username: 1–16 of [A-Za-z0-9_]. No space,
	// separator, or control character can appear, so a validated name is safe to
	// concatenate directly into an RCON command word.
	mcNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)
	// lpNodeRe matches a LuckPerms permission node: dotted segments with the
	// wildcard, e.g. "essentials.fly" or "worldedit.*". Deliberately excludes
	// space/`=`/`/` so a node can never carry a second token or a `world=` context.
	lpNodeRe = regexp.MustCompile(`^[A-Za-z0-9_.*-]{1,64}$`)
	// lpCtxRe matches a world name or a LuckPerms group: [A-Za-z0-9_-], 1–48. Used
	// for both the optional `world=` context and a parent group.
	lpCtxRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)
)

var (
	errInvalidPlayer = newError(http.StatusBadRequest, "bad_request",
		"invalid player name (1–16 chars: letters, digits, underscore)")
	errInvalidAction = newError(http.StatusBadRequest, "bad_request", "unknown action")
	errInvalidNode   = newError(http.StatusBadRequest, "bad_request",
		"invalid permission node (allowed: letters, digits, . _ - *)")
	errInvalidWorld = newError(http.StatusBadRequest, "bad_request",
		"invalid world (allowed: letters, digits, _ -)")
	errInvalidGroup = newError(http.StatusBadRequest, "bad_request",
		"invalid group (allowed: letters, digits, _ -)")
	errLuckPermsMissing = newError(http.StatusConflict, "luckperms_missing",
		"LuckPerms is not installed on this server, so permission and group changes have no effect; "+
			"the lobby gets it back by re-running the installer, another server needs the LuckPerms plugin added")
)

// unknownCommandRe matches the server's reply to a command no plugin registered,
// after color stripping. Brigadier servers (Paper, vanilla 1.13+) answer
// "Unknown or incomplete command. See below for error" (older builds: ", see
// below"), Spigot and legacy Bukkit "Unknown command. Type "/help" for help.".
// With LuckPerms installed an lp command never gets this: LuckPerms answers
// asynchronously, after RCON has flushed the reply, so the body is empty.
var unknownCommandRe = regexp.MustCompile(`(?i)^\s*unknown (or incomplete )?command\b`)

// issueLuckPermsCommand runs an lp command through issueAccessCommand and turns
// the reply of a server without LuckPerms into 409 luckperms_missing (#4). Without
// it that reply went back as a success whose output nobody reads, and the change
// silently did nothing.
func (a *API) issueLuckPermsCommand(w http.ResponseWriter, r *http.Request, name, command string) (string, bool) {
	out, ok := a.issueAccessCommand(w, r, name, command)
	if ok && unknownCommandRe.MatchString(lpColorRe.ReplaceAllString(out, "")) {
		writeError(w, r, errLuckPermsMissing)
		return "", false
	}
	return out, ok
}

// issueAccessCommand is the shared spine of every §7 access mutation: resolve the
// named server, enforce owner-or-admin, require readiness, and run ONE
// already-validated RCON command, returning its reply. It centralises the
// gate / readiness / console-failure surface in exactly one reviewed place,
// mirroring handleCommand step-for-step, so each handler's only job is to
// validate its structured fields and assemble the command string.
//
// It writes the HTTP error and returns ok=false on any failure, so a caller just
// `return`s. It does NOT audit — the caller audits with a structured action
// label (e.g. "access.whitelist.add") so the trail records intent, not a raw
// "console.command". The RCON password is resolved inside the Console
// implementation and never appears in `command`, the reply, or any log (§286).
//
// command MUST be assembled only from charset-validated fields; a space or
// newline in it would splice a second RCON command. `name` (the one field from
// the path, not the body) is validated here.
func (a *API) issueAccessCommand(w http.ResponseWriter, r *http.Request, name, command string) (string, bool) {
	p := principalFromContext(r.Context())
	if err := naming.ValidateServerName(name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return "", false
	}

	rec, err := a.Repo.ServerByName(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return "", false
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return "", false
	}

	// Readiness pre-check: RCON cannot reach a stopped server (§141 Ready ⟺ RCON
	// reachable), so changing access requires a Running node. This is a specific
	// 409 rather than a blind dial; the RunCommand below still maps an unreachable
	// channel to 503 because the invariant can drop between here and the dial.
	info, err := a.Cluster.GetServer(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return "", false
	}
	if !info.Ready {
		writeError(w, r, newError(http.StatusConflict, "not_running",
			"server is not running; wake it before changing access"))
		return "", false
	}

	if a.Console == nil {
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"console subsystem is not configured"))
		return "", false
	}

	out, err := a.Console.RunCommand(r.Context(), name, command)
	switch {
	case errors.Is(err, ErrConsoleUnavailable):
		writeError(w, r, newError(http.StatusServiceUnavailable, "console_unavailable",
			"server console is currently unreachable; wake the server and retry"))
		return "", false
	case err != nil:
		a.writeLookupError(w, r, err)
		return "", false
	}
	return out, true
}

// whitelistRequest is the body of POST .../access/whitelist (allow / disallow a
// player to join, spec §7). decodeJSON rejects unknown fields so no extra knob
// can smuggle in.
type whitelistRequest struct {
	Action string `json:"action"` // add | remove
	Player string `json:"player"`
}

// handleAccessWhitelist adds or removes a player from the live whitelist via
// "whitelist add|remove <player>". App-tier, owner/admin-gated inside
// issueAccessCommand.
func (a *API) handleAccessWhitelist(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body whitelistRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !mcNameRe.MatchString(body.Player) {
		writeError(w, r, errInvalidPlayer)
		return
	}
	switch body.Action {
	case "add", "remove":
	default:
		writeError(w, r, errInvalidAction)
		return
	}

	out, ok := a.issueAccessCommand(w, r, name, "whitelist "+body.Action+" "+body.Player)
	if !ok {
		return
	}
	a.audit(r, "access.whitelist."+body.Action, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "action": body.Action, "player": body.Player, "output": out,
	})
}

// handleAccessWhitelistList is the read projector for the whitelist: it runs
// "whitelist list" and returns a best-effort parse PLUS the raw reply. The parse
// is vanilla-specific (INTEGRATION-ONLY against a real server); the raw output is
// always returned so the client has ground truth when the format differs.
func (a *API) handleAccessWhitelistList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	out, ok := a.issueAccessCommand(w, r, name, "whitelist list")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "players": parseWhitelistOutput(out), "output": out,
	})
}

// handleAccessPlayers is the read projector for the online roster: it runs the
// vanilla "list" command and returns the online/max tally, a best-effort parse of
// the online player names, and the raw reply. Like the whitelist read, the parse
// is vanilla-specific (the names arrive after the count line's colon) and the raw
// output is always returned so a differing format never loses information. This is
// the only place the panel can learn WHO is online — Status.Players carries the
// count alone (§141), so this reuses the same RCON reply the prober already sees.
func (a *API) handleAccessPlayers(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	out, ok := a.issueAccessCommand(w, r, name, "list")
	if !ok {
		return
	}
	online, max, players := parseListOutput(out)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "online": online, "max": max, "players": players, "output": out,
	})
}

// handleAccessBanList is the read projector for the ban list: it runs "banlist"
// and returns a best-effort parse of the banned names PLUS the raw reply, like the
// whitelist / players reads. Unlike them the parse cannot key on a colon tail — a
// ban entry reads "<name> was banned by <source>: <reason>" and the reason carries
// its own colon — so parseBanlistOutput anchors on the ban marker + name charset
// instead. The raw reply is always returned so a plugin or localised format never
// loses information. No audit (a read).
func (a *API) handleAccessBanList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	out, ok := a.issueAccessCommand(w, r, name, "banlist")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "players": parseBanlistOutput(out), "output": out,
	})
}

// banRequest is the body of POST .../access/ban (deny / restore a player's
// ability to join, spec §7). It carries NO reason field on purpose: a free-text
// reason would be the one place a structured request could splice a second RCON
// command, and it buys nothing the audit log does not already record.
type banRequest struct {
	Action string `json:"action"` // ban | pardon
	Player string `json:"player"`
}

// handleAccessBan bans or pardons a player via "ban|pardon <player>".
func (a *API) handleAccessBan(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body banRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !mcNameRe.MatchString(body.Player) {
		writeError(w, r, errInvalidPlayer)
		return
	}
	switch body.Action {
	case "ban", "pardon":
	default:
		writeError(w, r, errInvalidAction)
		return
	}

	out, ok := a.issueAccessCommand(w, r, name, body.Action+" "+body.Player)
	if !ok {
		return
	}
	a.audit(r, "access.ban."+body.Action, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "action": body.Action, "player": body.Player, "output": out,
	})
}

// kickRequest is the body of POST .../access/kick (remove a player from the
// server right now, spec §7). Like ban it carries NO reason field — a free-text
// reason is the one place a structured request could splice a second RCON command,
// and it buys nothing the audit log does not already record.
type kickRequest struct {
	Player string `json:"player"`
}

// handleAccessKick kicks a player off the running server via "kick <player>".
// Unlike ban it does not block rejoining; it is the immediate "get out now" that
// pairs with the online roster. Single-action, so the body carries only a player.
func (a *API) handleAccessKick(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body kickRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !mcNameRe.MatchString(body.Player) {
		writeError(w, r, errInvalidPlayer)
		return
	}

	out, ok := a.issueAccessCommand(w, r, name, "kick "+body.Player)
	if !ok {
		return
	}
	a.audit(r, "access.kick", name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "player": body.Player, "output": out,
	})
}

// permissionRequest is the body of POST .../access/permission: a fine-grained
// LuckPerms permission grant/deny on a single node, optionally scoped to a world
// (spec §7 细致的权限调整 + world 范围).
//
// Value is a *bool, NOT a bool, and this matters: a plain bool's zero value is
// false, so an OMITTED value would silently mean "permission set <node> false",
// which is an explicit LuckPerms DENY — the exact opposite of the grant a caller
// who omits the field intends. nil therefore means "default to true (grant)";
// an explicit false is a deliberate deny.
type permissionRequest struct {
	Action string `json:"action"` // set | unset
	Player string `json:"player"`
	Node   string `json:"node"`
	Value  *bool  `json:"value,omitempty"` // set only; nil => true (grant)
	World  string `json:"world,omitempty"` // optional context; "" => global
}

// handleAccessPermission sets or unsets a LuckPerms permission node for a player,
// optionally within a world context:
//
//	set:   lp user <player> permission set <node> <true|false> [world=<world>]
//	unset: lp user <player> permission unset <node> [world=<world>]
func (a *API) handleAccessPermission(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body permissionRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !mcNameRe.MatchString(body.Player) {
		writeError(w, r, errInvalidPlayer)
		return
	}
	if !lpNodeRe.MatchString(body.Node) {
		writeError(w, r, errInvalidNode)
		return
	}
	// World is optional: validate ONLY when present, else an omitted world would
	// fail the charset check and the optional field would become mandatory.
	if body.World != "" && !lpCtxRe.MatchString(body.World) {
		writeError(w, r, errInvalidWorld)
		return
	}

	var cmd string
	switch body.Action {
	case "set":
		value := true // nil => grant; see permissionRequest.Value.
		if body.Value != nil {
			value = *body.Value
		}
		cmd = fmt.Sprintf("lp user %s permission set %s %t", body.Player, body.Node, value)
	case "unset":
		cmd = fmt.Sprintf("lp user %s permission unset %s", body.Player, body.Node)
	default:
		writeError(w, r, errInvalidAction)
		return
	}
	if body.World != "" {
		cmd += " world=" + body.World
	}

	out, ok := a.issueLuckPermsCommand(w, r, name, cmd)
	if !ok {
		return
	}
	a.audit(r, "access.permission."+body.Action, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "action": body.Action, "player": body.Player,
		"node": body.Node, "output": out,
	})
}

// groupRequest is the body of POST .../access/group: add or remove a LuckPerms
// parent group for a player (spec §7 给其他玩家权限的管理 via group membership).
type groupRequest struct {
	Action string `json:"action"` // add | remove
	Player string `json:"player"`
	Group  string `json:"group"`
}

// handleAccessGroup adds or removes a player's LuckPerms parent group via
// "lp user <player> parent add|remove <group>".
func (a *API) handleAccessGroup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body groupRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	if !mcNameRe.MatchString(body.Player) {
		writeError(w, r, errInvalidPlayer)
		return
	}
	if !lpCtxRe.MatchString(body.Group) {
		writeError(w, r, errInvalidGroup)
		return
	}
	switch body.Action {
	case "add", "remove":
	default:
		writeError(w, r, errInvalidAction)
		return
	}

	out, ok := a.issueLuckPermsCommand(w, r, name, "lp user "+body.Player+" parent "+body.Action+" "+body.Group)
	if !ok {
		return
	}
	a.audit(r, "access.group."+body.Action, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "action": body.Action, "player": body.Player, "group": body.Group, "output": out,
	})
}

// lpPermissionView is one parsed LuckPerms permission entry returned by the
// luckperms read projector. World is surfaced only when the entry carries a
// world= context (the one context the panel renders); Value comes from the
// entry's color code (LuckPerms renders granted nodes green, negated red).
type lpPermissionView struct {
	Node  string `json:"node"`
	Value bool   `json:"value"`
	World string `json:"world,omitempty"`
}

// maxLPInfoPages bounds how many "permission info" pages the read projector
// chases per request. LuckPerms paginates its reply, so one command shows only
// the first page; we follow the header's page count up to this cap.
// 10 pages ≈ 150 entries — raise if a real user outgrows it.
const maxLPInfoPages = 10

// handleAccessLuckPermsInfo is the read projector for a player's LuckPerms
// state: it runs "lp user <player> permission info" over the same owner-gated
// RCON spine as every access mutation and returns a best-effort parse — parent
// groups split out from plain permission nodes — PLUS the raw reply, like the
// whitelist/players/banlist reads. Page 1 goes through issueAccessCommand (the
// gate); further pages are fetched best-effort directly, so a mid-fetch failure
// keeps what was already read instead of erroring a half-served response.
// No audit (a read).
func (a *API) handleAccessLuckPermsInfo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	player := r.PathValue("player")
	if !mcNameRe.MatchString(player) {
		writeError(w, r, errInvalidPlayer)
		return
	}

	out, ok := a.issueLuckPermsCommand(w, r, name, "lp user "+player+" permission info")
	if !ok {
		return
	}
	raw := out
	entries, pages := parseLuckPermsInfo(out)
	for page := 2; page <= pages && page <= maxLPInfoPages; page++ {
		more, err := a.Console.RunCommand(r.Context(), name,
			fmt.Sprintf("lp user %s permission info %d", player, page))
		if err != nil {
			break // best-effort: keep the pages we have
		}
		raw += "\n" + more
		e, _ := parseLuckPermsInfo(more)
		entries = append(entries, e...)
	}

	// Split parent groups ("group.<name>", granted, no context) from plain
	// permission nodes. A negated or world-scoped group.* entry stays in
	// permissions — folding it into groups would lose the negation/scope.
	groups := []string{}
	permissions := []lpPermissionView{}
	for _, e := range entries {
		if g, isGroup := strings.CutPrefix(e.Node, "group."); isGroup && e.Value && e.World == "" && lpCtxRe.MatchString(g) {
			groups = append(groups, g)
			continue
		}
		permissions = append(permissions, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"player": player, "groups": groups, "permissions": permissions, "output": raw,
	})
}

var (
	// lpEntryRe matches one "permission info" entry: the "> " marker, then any
	// legacy color codes, then the node (lpNodeRe's charset). Anchoring on the
	// marker rather than lines follows banEntryRe's rationale: RCON concatenates
	// multi-message replies with a server-dependent separator, so a line split is
	// unreliable. Group 1 keeps the color codes so the entry's value survives the
	// later color strip (§a = granted, §c = negated).
	lpEntryRe = regexp.MustCompile(`>\s*((?:§[0-9a-fk-or])*)([A-Za-z0-9_.*-]{1,64})`)
	// lpPageRe reads the pagination header ("page 1 of 3") AFTER color stripping.
	lpPageRe = regexp.MustCompile(`page\s+(\d+)\s+of\s+(\d+)`)
	// lpColorRe strips legacy §-color codes.
	lpColorRe = regexp.MustCompile(`§[0-9a-fk-or]`)
	// lpWorldRe reads a world= context from an entry's color-stripped tail.
	lpWorldRe = regexp.MustCompile(`world=([A-Za-z0-9_-]{1,48})`)
)

// parseLuckPermsInfo extracts permission entries and the total page count from
// one "lp user <player> permission info" reply. Best-effort and
// LuckPerms-specific (INTEGRATION-ONLY against a real server) — the caller
// always returns the raw reply alongside, so an unrecognised format loses
// nothing. An entry's value defaults to granted when no color code precedes the
// node (a color-stripping RCON transport); pages is 0 when no header parses.
func parseLuckPermsInfo(out string) (entries []lpPermissionView, pages int) {
	matches := lpEntryRe.FindAllStringSubmatchIndex(out, -1)
	for i, m := range matches {
		colors := out[m[2]:m[3]]
		node := out[m[4]:m[5]]
		// The entry's tail (up to the next marker) carries its contexts.
		tailEnd := len(out)
		if i+1 < len(matches) {
			tailEnd = matches[i+1][0]
		}
		tail := lpColorRe.ReplaceAllString(out[m[5]:tailEnd], "")
		e := lpPermissionView{Node: node, Value: !strings.Contains(colors, "§c")}
		if wm := lpWorldRe.FindStringSubmatch(tail); wm != nil {
			e.World = wm[1]
		}
		entries = append(entries, e)
	}
	if pm := lpPageRe.FindStringSubmatch(lpColorRe.ReplaceAllString(out, "")); pm != nil {
		pages, _ = strconv.Atoi(pm[2])
	}
	return entries, pages
}

// parseWhitelistOutput extracts player names from vanilla's "whitelist list"
// reply, whose format is "There are N whitelisted player(s): a, b, c" (and "There
// are no whitelisted players" / a trailing colon for the empty case). The parse
// is best-effort and vanilla-specific — the raw reply is always returned
// alongside, so a different format (a plugin, a localised or future server) never
// loses information. Returns a non-nil empty slice so the JSON renders [] not null.
func parseWhitelistOutput(out string) []string {
	players := []string{}
	i := strings.LastIndex(out, ":")
	if i < 0 {
		return players
	}
	for _, part := range strings.Split(out[i+1:], ",") {
		if p := strings.TrimSpace(part); p != "" {
			players = append(players, p)
		}
	}
	return players
}

// listCountRe matches the count line of vanilla's "list" reply, e.g.
// "There are 3 of a max of 20 players online: alice, bob, carol". It mirrors the
// operator prober's listReplyPattern; kept local so the api package does not
// depend on operator internals for a read it already has the reply for.
var listCountRe = regexp.MustCompile(`There are (\d+) of a max of (\d+) players online`)

// parseListOutput extracts (online, max, names) from vanilla's "list" reply. The
// count comes from the "N of a max of M" line; the names come from the tail after
// the colon, comma-separated (each name is [A-Za-z0-9_], so it never contains a
// colon or comma of its own). Best-effort and vanilla-specific — the raw reply is
// always returned alongside — so a plugin or localised format loses nothing. names
// is non-nil so the JSON renders [] not null; online/max are 0 when the count line
// does not match (e.g. an empty or unrecognised reply).
func parseListOutput(out string) (online, max int, players []string) {
	players = []string{}
	if i := strings.Index(out, ":"); i >= 0 {
		for _, part := range strings.Split(out[i+1:], ",") {
			if p := strings.TrimSpace(part); p != "" {
				players = append(players, p)
			}
		}
	}
	if m := listCountRe.FindStringSubmatch(out); m != nil {
		online, _ = strconv.Atoi(m[1])
		max, _ = strconv.Atoi(m[2])
	}
	return online, max, players
}

// banEntryRe matches one player-ban entry in vanilla's "banlist" reply, anchored on
// the "<name> was banned by" marker with the name pinned to mcNameRe's charset. The
// anchoring is deliberate and NOT interchangeable with the whitelist/list tail
// parse: "banlist" emits one command-feedback message PER ban, and RCON concatenates
// them with a separator that is server/version-dependent (newline, space, or none),
// so a line- or colon-split parser could run the "There are N ban(s):" header into
// the first entry and emit a non-name. Keying only on the marker + name charset
// yields the SAME names under every separator and structurally cannot return a
// non-name (group 1 IS the charset), so a corrupt entry can never reach the one-tap
// pardon button. It also sidesteps the reason's own colon, which the tail parse can't.
//
// We issue plain "banlist", which in vanilla lists PLAYER bans only (IP bans are the
// separate "banlist ips", which nothing here ever issues), so a "1.2.3.4 was banned
// by ..." line — whose trailing octet the charset would otherwise capture as a bogus
// short name — never reaches this parser.
var banEntryRe = regexp.MustCompile(`([A-Za-z0-9_]{1,16}) was banned by`)

// parseBanlistOutput extracts banned player names from vanilla's "banlist" reply,
// whose entries read "<name> was banned by <source>: <reason>". Best-effort and
// vanilla-specific — the raw reply is always returned alongside — so a plugin or
// localised format loses nothing; the "There are no ban(s)." / header lines carry no
// marker and are skipped. Returns a non-nil empty slice so the JSON renders [] not null.
func parseBanlistOutput(out string) []string {
	players := []string{}
	for _, m := range banEntryRe.FindAllStringSubmatch(out, -1) {
		players = append(players, m[1])
	}
	return players
}
