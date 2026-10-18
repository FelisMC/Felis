package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Access state belongs to Minecraft and LuckPerms; validate every structured field before assembling RCON commands.
var (
	// mcNameRe restricts usernames to one RCON command token.
	mcNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)
	// lpNodeRe excludes separators and context syntax to prevent extra RCON arguments.
	lpNodeRe = regexp.MustCompile(`^[A-Za-z0-9_.*-]{1,64}$`)
	lpCtxRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)
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

// unknownCommandRe detects missing plugins; installed LuckPerms may return an empty asynchronous RCON reply.
var unknownCommandRe = regexp.MustCompile(`(?i)^\s*unknown (or incomplete )?command\b`)

// issueLuckPermsCommand maps unregistered commands to luckperms_missing instead of reporting a successful no-op.
func (a *API) issueLuckPermsCommand(w http.ResponseWriter, r *http.Request, name, command string) (string, bool) {
	out, ok := a.issueAccessCommand(w, r, name, command)
	if ok && unknownCommandRe.MatchString(lpColorRe.ReplaceAllString(out, "")) {
		writeError(w, r, errLuckPermsMissing)
		return "", false
	}
	return out, ok
}

// issueAccessCommand requires charset-validated command fields; it writes errors but leaves auditing to the caller.
func (a *API) issueAccessCommand(w http.ResponseWriter, r *http.Request, name, command string) (string, bool) {
	p := principalFromContext(r.Context())
	if err := validateManagedServerName(r, name); err != nil {
		writeError(w, r, newError(http.StatusBadRequest, "bad_name", "invalid server name: %v", err))
		return "", false
	}

	rec, err := a.managedServerRecord(r.Context(), name)
	if err != nil {
		a.writeLookupError(w, r, err)
		return "", false
	}
	if !a.isOwnerOrAdmin(p, rec) {
		writeError(w, r, errForbidden)
		return "", false
	}

	// Readiness may change after this check; the RCON call still maps connection failures to 503.
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

type whitelistRequest struct {
	Action string `json:"action"`
	Player string `json:"player"`
}

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

// handleAccessWhitelistList returns raw output alongside the best-effort vanilla parse.
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

// handleAccessPlayers returns raw output alongside best-effort counts and player names.
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

// handleAccessBanList returns raw output because plugins and localized servers may use other formats.
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

// banRequest omits free-text reasons to prevent RCON argument injection.
type banRequest struct {
	Action string `json:"action"`
	Player string `json:"player"`
}

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

// kickRequest omits free-text reasons to prevent RCON argument injection.
type kickRequest struct {
	Player string `json:"player"`
}

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

type permissionRequest struct {
	Action string `json:"action"`
	Player string `json:"player"`
	Node   string `json:"node"`
	Value  *bool  `json:"value,omitempty"` // nil grants; false explicitly denies
	World  string `json:"world,omitempty"` // empty means global
}

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
	if body.World != "" && !lpCtxRe.MatchString(body.World) {
		writeError(w, r, errInvalidWorld)
		return
	}

	var cmd string
	switch body.Action {
	case "set":
		value := true
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

type groupRequest struct {
	Action string `json:"action"`
	Player string `json:"player"`
	Group  string `json:"group"`
}

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

// lpPermissionView derives permission values from color codes and exposes only the world context.
type lpPermissionView struct {
	Node  string `json:"node"`
	Value bool   `json:"value"`
	World string `json:"world,omitempty"`
}

// maxLPInfoPages bounds pagination to roughly 150 permission entries per request.
const maxLPInfoPages = 10

// handleAccessLuckPermsInfo preserves earlier pages if a later fetch fails.
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
			break
		}
		raw += "\n" + more
		e, _ := parseLuckPermsInfo(more)
		entries = append(entries, e...)
	}

	// Keep negated or world-scoped group entries in permissions so their value and scope are preserved.
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
	// lpEntryRe anchors on entry markers because RCON message separators vary; retain colors to distinguish grants and denials.
	lpEntryRe = regexp.MustCompile(`>\s*((?:§[0-9a-fk-or])*)([A-Za-z0-9_.*-]{1,64})`)
	lpPageRe  = regexp.MustCompile(`page\s+(\d+)\s+of\s+(\d+)`)
	lpColorRe = regexp.MustCompile(`§[0-9a-fk-or]`)
	lpWorldRe = regexp.MustCompile(`world=([A-Za-z0-9_-]{1,48})`)
)

// parseLuckPermsInfo defaults uncolored entries to granted and unparsed page counts to zero.
func parseLuckPermsInfo(out string) (entries []lpPermissionView, pages int) {
	matches := lpEntryRe.FindAllStringSubmatchIndex(out, -1)
	for i, m := range matches {
		colors := out[m[2]:m[3]]
		node := out[m[4]:m[5]]
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

// parseWhitelistOutput returns an empty slice for unrecognized replies; callers also return the raw output.
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

var listCountRe = regexp.MustCompile(`There are (\d+) of a max of (\d+) players online`)

// parseListOutput returns zero counts and an empty slice for unrecognized replies; callers also return raw output.
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

// banEntryRe uses name markers because RCON separators and reason colons make line or colon splitting unreliable.
var banEntryRe = regexp.MustCompile(`([A-Za-z0-9_]{1,16}) was banned by`)

// parseBanlistOutput parses player bans, not IP bans, and returns an empty slice for unrecognized replies.
func parseBanlistOutput(out string) []string {
	players := []string{}
	for _, m := range banEntryRe.FindAllStringSubmatch(out, -1) {
		players = append(players, m[1])
	}
	return players
}
