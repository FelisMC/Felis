package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// Felis-nano: the multi-source hasJoined multiplexer (spec §B3 player game-login).
//
// Velocity's session verifier (authlib) is pointed here — via -Dmojang.sessionserver
// on Felis-managed proxies, or a thin login-pipeline hook on third-party servers. On
// login Velocity computes the serverId hash and GETs hasJoined; this endpoint fans that
// query out to the configured Yggdrasil roots in priority order (Mojang first, 正版优先)
// and returns the first source that validates. Each upstream Yggdrasil runs its own
// serverId-hash check — the multiplexer only relays, it computes no hashes.
//
// The one non-negotiable transform: a non-identity (third-party) source's UUID is
// self-asserted, so its profile is rewritten into a per-source namespace
// (canonical = UUIDv3(felisAuthNS, tag+":"+nativeID)) BEFORE it leaves the resolver.
// Mojang stays identity. This makes the reclaim invariant — "the genuine Mojang player
// has a DIFFERENT UUID from any squatter" — true by construction, not assumed: MD5
// preimage resistance means no third-party source can mint a Mojang-space UUID, and the
// per-tag namespace means two sources cannot collide onto one identity. Every downstream
// key (account_links, username_blacklist, owner checks) then sees one canonical UUID.

// felisAuthNS is the fixed UUIDv3 namespace every third-party profile is rewritten
// under (see the rewrite rationale above). Derived from the project name, not a magic
// literal, so its origin is self-documenting; the exact value only has to be stable.
var felisAuthNS = uuid.NewSHA1(uuid.NameSpaceURL, []byte("nano.felis.lolicon.best/auth-source"))

// authHTTPClient calls the upstream Yggdrasil roots. The timeout bounds one login
// against a hung source; the resolver moves on to the next source on any failure.
// ponytail: one shared client, sequential priority scan — a third-party login costs one
// wasted Mojang round-trip; add parallel fan-out only if login latency bites.
var authHTTPClient = &http.Client{Timeout: 5 * time.Second}

// AuthSource is one upstream Yggdrasil root in the multiplexer's priority list (config
// order = priority). URL is the full hasJoined endpoint the query string is appended to.
// Identity marks the authoritative source (Mojang) whose UUIDs are trusted as-is; every
// other source is rewritten into felisAuthNS.
type AuthSource struct {
	Tag      string
	URL      string
	Identity bool
}

// sessionProfile is the Mojang hasJoined contract. properties is relayed verbatim
// (json.RawMessage) so a source's signed textures survive the multiplexer untouched.
type sessionProfile struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Properties []json.RawMessage `json:"properties,omitempty"`
}

// handleHasJoined is the multi-source session verifier (Felis-nano). It is a Public
// internal-face route: authlib speaks the vanilla sessionserver protocol and sends no
// service token. A rejected login is 204 No Content — exactly what Mojang returns for an
// invalid session, which authlib maps to "failed to verify username".
func (a *API) handleHasJoined(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	username, serverID := q.Get("username"), q.Get("serverId")
	if username == "" || serverID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	prof, src := a.resolveHasJoined(r.Context(), username, serverID, q.Get("ip"))
	if prof == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Canonicalize identity. A trusted (Mojang) source keeps its UUID; a self-asserted
	// source is rewritten into felisAuthNS so it can never land in Mojang's UUID space
	// nor onto another source's. An unparseable identity UUID is not trustworthy → reject.
	var canonical uuid.UUID
	if src.Identity {
		id, err := uuid.Parse(prof.ID)
		if err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		canonical = id
	} else {
		canonical = uuid.NewMD5(felisAuthNS, []byte(src.Tag+":"+prof.ID))
	}

	// Bar gate at the single chokepoint every login crosses, so a reclaimed squatter
	// stays out even on a consumer with no limbo plugin. Keyed on the dashed canonical
	// UUID — the same form Repo.ReclaimUsername stores.
	barred, err := a.Repo.IsUsernameBlacklisted(r.Context(), canonical.String())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if barred {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Emit the canonical UUID undashed — the 32-hex form authlib's GameProfile expects.
	prof.ID = hex.EncodeToString(canonical[:])
	writeJSON(w, http.StatusOK, prof)
}

// resolveHasJoined queries each configured source in priority order and returns the
// first that validates the session (200 with a profile). A source that is down, answers
// non-200 (204 = "not my player"), or returns garbage is skipped.
func (a *API) resolveHasJoined(ctx context.Context, username, serverID, ip string) (*sessionProfile, AuthSource) {
	for _, src := range a.AuthSources {
		u := src.URL + "?username=" + url.QueryEscape(username) + "&serverId=" + url.QueryEscape(serverID)
		if ip != "" {
			u += "&ip=" + url.QueryEscape(ip)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		resp, err := authHTTPClient.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		var prof sessionProfile
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&prof)
		resp.Body.Close()
		if err != nil || prof.ID == "" {
			continue
		}
		return &prof, src
	}
	return nil, AuthSource{}
}
