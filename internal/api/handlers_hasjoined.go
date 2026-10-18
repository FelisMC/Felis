package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"felis.lolicon.best/internal/netguard"
)

// Third-party profiles use source-namespaced UUIDs; chat signatures over their native UUIDs require unsigned-chat support.

// felisAuthNS must remain stable to preserve third-party player identities.
var felisAuthNS = uuid.NewSHA1(uuid.NameSpaceURL, []byte("nano.felis.lolicon.best/auth-source"))

// authHTTPClient bounds each source request; sources are queried sequentially in priority order.
var authHTTPClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: upstreamTransport,
	// Reject redirects to prevent configured sources from targeting arbitrary URLs or recursively calling this listener.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// upstreamTransport separately bounds headers and rejects link-local addresses; the body limit does not cover headers.
var upstreamTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxResponseHeaderBytes = 16 << 10
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: netguard.RefuseLinkLocal}).DialContext
	return t
}()

// AuthSource order sets priority; Identity trusts native UUIDs, while other sources are rewritten into felisAuthNS.
type AuthSource struct {
	Tag      string
	Prefix   string
	URL      string
	APIURL   string
	Identity bool
}

// sessionProfile preserves signed properties verbatim and emits an array because Velocity rejects missing or null properties.
type sessionProfile struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Properties []json.RawMessage `json:"properties"`
}

// HasJoinedHandler expects priority-ordered sources with Mojang first; repo enforces the reclaim blacklist.
func HasJoinedHandler(sources []AuthSource, repo Repo) http.Handler {
	a := &API{AuthSources: sources, Repo: repo}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /session/minecraft/hasJoined", a.handleHasJoined)
	return mux
}

// handleHasJoined accepts the tokenless sessionserver protocol; rejected sessions return 204.
func (a *API) handleHasJoined(w http.ResponseWriter, r *http.Request) {
	// Close requests with unexpected bodies so net/http cannot block draining a body that never arrives.
	if r.ContentLength != 0 {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	username, serverID, ip := q.Get("username"), q.Get("serverId"), q.Get("ip")
	if username == "" || serverID == "" ||
		len(username) > maxHasJoinedParam || len(serverID) > maxHasJoinedParam || len(ip) > maxHasJoinedParam {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	prof, src, failed := a.resolveHasJoined(r.Context(), username, serverID, ip)
	if prof == nil {
		// An unavailable source is not a rejected session; return 503 so Velocity reports an authentication outage.
		if failed {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Keep Mojang UUIDs unchanged and namespace third-party UUIDs before returning them downstream.
	canonical, err := canonicalProfileUUID(src, prof.ID)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !src.Identity {
		// Treat third-party profile names as untrusted even after upstream resolution.
		if !mcUsernameRe.MatchString(prof.Name) {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Velocity keys connected players by name; rename third-party collisions so the Mojang player can connect.
		if isPremiumName(r.Context(), prof.Name) {
			prof.Name = prefixedName(src.Prefix, prof.Name)
		}
	}

	// Apply the reclaim blacklist to the canonical dashed UUID on every login, including clients without the limbo plugin.
	barred, err := a.Repo.IsUsernameBlacklisted(r.Context(), canonical.String())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if barred {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Velocity expects the canonical UUID as 32 undashed hex characters.
	prof.ID = hex.EncodeToString(canonical[:])
	if prof.Properties == nil {
		prof.Properties = []json.RawMessage{} // encode an array instead of null
	}
	writeJSON(w, http.StatusOK, prof)
}

const maxHasJoinedParam = 64

var mcUsernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)

const mcUsernameMax = 16

// prefixedName truncates to the protocol limit; collisions may reject a connection but do not merge UUID identities.
func prefixedName(prefix, name string) string {
	p := prefix + "_"
	if keep := mcUsernameMax - len(p); len(name) > keep {
		name = name[:keep]
	}
	return p + name
}

// canonicalProfileUUID preserves the source-native ID byte-for-byte to maintain existing third-party identities.
func canonicalProfileUUID(src AuthSource, nativeID string) (uuid.UUID, error) {
	if src.Identity {
		return uuid.Parse(nativeID)
	}
	return uuid.NewMD5(felisAuthNS, []byte(src.Tag+":"+nativeID)), nil
}

var mojangProfileAPI = "https://api.mojang.com/users/profiles/minecraft/"

// profileHTTPClient uses a shorter timeout for the second Mojang lookup; failures conservatively reserve the name.
var profileHTTPClient = &http.Client{Timeout: 2 * time.Second, Transport: upstreamTransport}

// Cache free names briefly because purchase can invalidate them; stale reserved names cost only a prefix.
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

// isPremiumName treats lookup failures as taken, including after an expired free-name cache entry.
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
		return true
	}

	premiumNames.Lock()
	// Bound cache memory by clearing it; additional lookups still fail closed when Mojang is unavailable.
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

// lookupPremiumName treats only 404 or legacy 204 as free; outages and throttling are errors.
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

// resolveHasJoined returns the first valid profile in priority order and distinguishes rejection from upstream failure.
func (a *API) resolveHasJoined(ctx context.Context, username, serverID, ip string) (prof *sessionProfile, src AuthSource, failed bool) {
	sources, err := a.currentAuthSources(ctx)
	if err != nil {
		log.Printf("hasJoined: read authentication sources: %v", err)
		return nil, AuthSource{}, true
	}
	for _, src := range sources {
		u := src.URL + "?username=" + url.QueryEscape(username) + "&serverId=" + url.QueryEscape(serverID)
		if ip != "" {
			u += "&ip=" + url.QueryEscape(ip)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			log.Printf("hasJoined: source %q: %v", src.Tag, err)
			failed = true
			continue
		}
		resp, err := authHTTPClient.Do(req)
		if err != nil {
			log.Printf("hasJoined: source %q: %v", src.Tag, err)
			failed = true
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusNoContent:
			case resp.StatusCode >= 300 && resp.StatusCode < 400:
				log.Printf("hasJoined: source %q answered %s with Location %q; redirects are not followed, so set its url to the final endpoint", src.Tag, resp.Status, resp.Header.Get("Location"))
				failed = true
			default:
				log.Printf("hasJoined: source %q answered %s", src.Tag, resp.Status)
				failed = true
			}
			continue
		}
		var p sessionProfile
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&p)
		resp.Body.Close()
		if err != nil || p.ID == "" {
			log.Printf("hasJoined: source %q answered 200 without a usable profile (err=%v)", src.Tag, err)
			failed = true
			continue
		}
		// Validate a profile before accepting its source so an unusable response cannot hide later valid sources.
		if src.Identity {
			if _, perr := uuid.Parse(p.ID); perr != nil {
				log.Printf("hasJoined: identity source %q answered 200 with an unparseable profile id %q", src.Tag, p.ID)
				failed = true
				continue
			}
		} else if !mcUsernameRe.MatchString(p.Name) {
			log.Printf("hasJoined: source %q answered 200 with an unusable profile name %q", src.Tag, p.Name)
			failed = true
			continue
		}
		return &p, src, failed
	}
	return nil, AuthSource{}, failed
}
