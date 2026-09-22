package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
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
var authHTTPClient = &http.Client{
	Timeout: 5 * time.Second,
	// A redirect is not a hasJoined answer. Following one would let a configured root point
	// this host at any URL it can reach — this listener included, where each hop re-runs the
	// whole source scan inside the same login's timeout. The 3xx is returned as-is and the
	// resolver skips that source like any other non-200.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// AuthSource is one upstream Yggdrasil root in the multiplexer's priority list (config
// order = priority). URL is the full hasJoined endpoint the query string is appended to.
// Identity marks the authoritative source (Mojang) whose UUIDs are trusted as-is; every
// other source is rewritten into felisAuthNS. Prefix is the in-game rename applied to a
// player of this source who is holding a Mojang player's name (see prefixedName); it is
// unused on the identity source, whose players are never renamed.
type AuthSource struct {
	Tag      string
	Prefix   string
	URL      string
	Identity bool
}

// sessionProfile is the Mojang hasJoined contract. properties is relayed verbatim
// (json.RawMessage) so a source's signed textures survive the multiplexer untouched, and
// it is always emitted as an array: Velocity's GameProfile parser throws on a missing or
// null properties key, while a Yggdrasil root may legitimately send [] or omit it for a
// player with no skin.
type sessionProfile struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Properties []json.RawMessage `json:"properties"`
}

// HasJoinedHandler returns an http.Handler serving only the Felis-nano hasJoined
// multiplexer route (GET /session/minecraft/hasJoined), for a standalone host that
// federates logins without standing up the full felis-api. sources is the priority list
// (put the Mojang identity source first for 正版优先); repo backs the reclaim blacklist
// gate — a stub that never bars is fine for a host without the reclaim DB. The full
// felis-api mounts the same handler through its internal-face route table instead.
func HasJoinedHandler(sources []AuthSource, repo Repo) http.Handler {
	a := &API{AuthSources: sources, Repo: repo}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /session/minecraft/hasJoined", a.handleHasJoined)
	return mux
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
		// A third-party source is untrusted input, its name included: nothing stops a
		// hostile or sloppy root from answering with "§4admin", an empty string, or 200
		// characters, all of which this handler would otherwise relay straight into the
		// proxy's player list.
		if !mcUsernameRe.MatchString(prof.Name) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		canonical = uuid.NewMD5(felisAuthNS, []byte(src.Tag+":"+prof.ID))

		// Give a Mojang player's name back to the Mojang player. The UUID rewrite above
		// already keeps the two apart as identities, but the proxy's player registry is
		// keyed on the NAME (Velocity: "You are already connected to this proxy!"), so
		// without this they cannot even be online at the same time. Renaming only on an
		// actual collision leaves the ordinary third-party player's name untouched.
		if isPremiumName(r.Context(), prof.Name) {
			prof.Name = prefixedName(src.Prefix, prof.Name)
		}
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
	if prof.Properties == nil {
		prof.Properties = []json.RawMessage{} // a nil slice would marshal as null
	}
	writeJSON(w, http.StatusOK, prof)
}

// mcUsernameRe is Minecraft's username charset — the trust boundary on a third-party
// source's self-asserted profile name.
var mcUsernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

// mcUsernameMax is the protocol's username length ceiling, which prefixedName must respect.
const mcUsernameMax = 16

// prefixedName is the squatter rename: ("LS", "steve") → "LS_steve". The base name is
// TRUNCATED to fit rather than the rename being skipped when it would not fit — skipping is
// what would silently hand a 14-character premium name back to the squatter.
//
// ponytail: two players of one source whose names agree on their first mcUsernameMax-len(prefix)-1
// characters truncate onto the same in-game name, as does a prefixed name that happens to be
// a premium name itself. Both cost an "already connected" bounce, not an identity: the UUID
// rewrite is what keeps players apart, and it does not depend on the name at all. Add a
// disambiguating suffix only if real players actually collide.
func prefixedName(prefix, name string) string {
	p := prefix + "_"
	if keep := mcUsernameMax - len(p); len(name) > keep {
		name = name[:keep]
	}
	return p + name
}

// mojangProfileAPI answers the one question that decides a rename: is this username
// registered to a Mojang account? A var, not a const, so a test can point it at a stub
// instead of the real Mojang.
var mojangProfileAPI = "https://api.mojang.com/users/profiles/minecraft/"

// profileHTTPClient is deliberately more impatient than authHTTPClient: the name lookup is a
// SECOND Mojang round-trip on a third-party login (the identity leg already spent one), and
// api.mojang.com is exactly what is unreliable from the networks these servers sit on. A
// slow answer falls back to the cache instead of holding the login open.
var profileHTTPClient = &http.Client{Timeout: 2 * time.Second}

// A name's premium status changes on human timescales, not per login, so it is cached — but
// asymmetrically, because the two directions have very different costs. "Taken" is nearly
// permanent (Mojang does not recycle names), while "free" can stop being true the moment
// someone buys that name, and a stale "free" is the dangerous one: it leaves a squatter
// holding a name its real owner has just bought. So a "free" answer is trusted for minutes
// and a "taken" answer for a day.
const (
	premiumTakenTTL = 24 * time.Hour
	premiumFreeTTL  = 10 * time.Minute
	premiumCacheMax = 4096
)

type premiumEntry struct {
	taken bool
	at    time.Time
}

var premiumNames = struct {
	sync.Mutex
	m map[string]premiumEntry
}{m: make(map[string]premiumEntry)}

// isPremiumName reports whether username belongs to a real Mojang account — which is what
// makes a third-party player holding it a squatter. On a lookup failure it prefers a stale
// cached answer, and with nothing cached it fails CLOSED (assume premium → rename the
// third-party player): a Mojang outage must not let a squatter keep a name the real owner is
// about to log in with. Being wrong that way costs a cosmetic prefix; being wrong the other
// way bounces the name's actual owner off the proxy.
func isPremiumName(ctx context.Context, username string) bool {
	key := strings.ToLower(username)

	premiumNames.Lock()
	cached, hit := premiumNames.m[key]
	premiumNames.Unlock()
	if hit && time.Since(cached.at) < premiumTTL(cached.taken) {
		return cached.taken
	}

	taken, err := lookupPremiumName(ctx, username)
	if err != nil {
		if hit {
			return cached.taken
		}
		return true
	}

	premiumNames.Lock()
	// ponytail: bounded by dropping the whole map rather than evicting LRU — entries are
	// only minted by players who actually authenticated somewhere, so this is a backstop
	// against an unbounded map, not a cache policy worth tuning.
	if len(premiumNames.m) >= premiumCacheMax {
		clear(premiumNames.m)
	}
	premiumNames.m[key] = premiumEntry{taken: taken, at: time.Now()}
	premiumNames.Unlock()
	return taken
}

func premiumTTL(taken bool) time.Duration {
	if taken {
		return premiumTakenTTL
	}
	return premiumFreeTTL
}

// lookupPremiumName asks Mojang whether a name is registered: 200 = it is, 404 (204 on the
// legacy endpoint) = it is free. Anything else is an ERROR, never a "no" — a 429 or a 503
// must not read as "this name is unowned"; see isPremiumName's fail-closed rule.
func lookupPremiumName(ctx context.Context, username string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mojangProfileAPI+url.PathEscape(username), nil)
	if err != nil {
		return false, err
	}
	resp, err := profileHTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound, http.StatusNoContent:
		return false, nil
	}
	return false, fmt.Errorf("mojang profile api: %s", resp.Status)
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
