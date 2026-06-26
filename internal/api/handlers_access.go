package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
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
)

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
	a.audit(r, principalFromContext(r.Context()).Email, "access.whitelist."+body.Action, name)
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
	a.audit(r, principalFromContext(r.Context()).Email, "access.ban."+body.Action, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "action": body.Action, "player": body.Player, "output": out,
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
	Action string `json:"action"`          // set | unset
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

	out, ok := a.issueAccessCommand(w, r, name, cmd)
	if !ok {
		return
	}
	a.audit(r, principalFromContext(r.Context()).Email, "access.permission."+body.Action, name)
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

	out, ok := a.issueAccessCommand(w, r, name, "lp user "+body.Player+" parent "+body.Action+" "+body.Group)
	if !ok {
		return
	}
	a.audit(r, principalFromContext(r.Context()).Email, "access.group."+body.Action, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "action": body.Action, "player": body.Player, "group": body.Group, "output": out,
	})
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
