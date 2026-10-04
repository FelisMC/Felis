package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"felis.lolicon.best/internal/updates"
)

// githubBaseURL is the GitHub REST API root. It is a field on the github source (not a
// hardcoded constant in the request) so discovery runs against an httptest server in
// tests without touching the network.
const githubBaseURL = "https://api.github.com"

// github discovers the latest STABLE release of a GitHub repository from the REST API's
// /repos/{repo}/releases/latest endpoint, which GitHub defines as the most recent
// non-draft, non-prerelease release. Felis tracks felis-api, k3s and cloudflared this
// way.
//
// Unlike the PaperMC Fill API, GitHub ENFORCES a User-Agent: a request without one is
// answered "403 Request forbidden by administrative rules. Please make sure your request
// has a User-Agent header" (verified against api.github.com on 2026-07-05 — a UA-less
// request 403'd while a plain browser UA got 200, so the gate is on presence, not on the
// UA's content). Felis always sends its descriptive UA, so the requirement is met.
//
// It uses /releases/latest (one request per repo) rather than listing releases: it is
// GitHub's own "newest stable" definition and is gentle on the unauthenticated 60-req/h
// rate limit a CronJob shares. The one semantic gap — GitHub sorts "latest" by the
// release's created_at (not by version), so a repo that back-ports a patch to an OLD
// line LAST would report that patch — does not bite Felis's tracked repos: felis-api
// and cloudflared are
// single-line (date order == version order), and k3s is Notify-only (a missed
// notification self-corrects on the next release, and never drives an apply).
type github struct {
	baseURL   string // e.g. "https://api.github.com"
	userAgent string // MUST be non-empty — GitHub 403s a UA-less request
	// token is an optional credential. Empty means unauthenticated, which is the right
	// posture for the public repos Felis tracks (felis-api, k3s, cloudflared) and is what
	// the 60-req/h note above is about. It is load-bearing only when felis-api is built
	// from a private fork, where its absence does not look like an auth failure:
	// GitHub answers 404 — not 401 or 403 — for a private repo the caller cannot see, so
	// "no token" is indistinguishable from "no release published yet" by status alone.
	// latestStable says both in the error rather than making an operator guess, for any
	// repository other than the official one, which is public.
	//
	// It is read from the environment and never compiled in. A constant would be
	// committed to the very repository it protects, ship inside every felis binary where
	// strings(1) recovers it, reach every node the image is imported onto, and need a
	// rebuild and a redeploy to rotate.
	token string
	hc    *http.Client
}

// tokenEnv names the environment variable holding the GitHub credential. deploy/bootstrap.sh
// reads the same variable to clone a private fork, so an operator sets one value once.
const tokenEnv = "FELIS_GITHUB_TOKEN"

// newGitHub builds a source pointed at the live GitHub REST API with sane defaults.
func newGitHub() github {
	return github{
		baseURL:   githubBaseURL,
		userAgent: defaultUserAgent,
		token:     os.Getenv(tokenEnv),
		hc:        &http.Client{Timeout: 15 * time.Second},
	}
}

// releaseResponse is the slice of GET /repos/{repo}/releases/latest that Felis reads.
// The live shape (2026-07-05) for both a CalVer project (cloudflared "2026.6.1") and a
// v-prefixed, build-tagged one (k3s "v1.36.2+k3s1") is:
//
//	{"tag_name":"v1.36.2+k3s1","prerelease":false,"draft":false, ...}
//
// updates.Parse tolerates the leading "v" and strips "+build" metadata, so both tag
// styles reduce to a comparable Version while String() keeps the raw for the report.
type releaseResponse struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
}

// latestStable returns the newest STABLE release of repo (e.g. "cloudflare/cloudflared").
//
// It fails closed: a transport error, a non-200 status (GitHub answers 404 when a repo
// has no non-prerelease release, so "no stable release" surfaces as an error, never a
// zero version), an undecodable body, a response that is somehow marked draft/prerelease,
// or an unparseable / prerelease-parsing tag all return an error. /releases/latest
// already excludes drafts and prereleases; the explicit re-checks are defense in depth so
// an upstream change can never silently promote a prerelease into a scheduled apply.
func (g github) latestStable(ctx context.Context, repo string) (updates.Version, error) {
	tag, err := g.latestTag(ctx, repo)
	if err != nil {
		return updates.Version{}, err
	}
	return parseStableTag(repo, tag)
}

// latestTemurin returns the newest stable Temurin build of one JDK feature release.
// Adoptium publishes each feature line from its own repository and tags every build
// "jdk-<version>" ("jdk-25.0.4.1+1"); the prefix is the only thing Parse cannot take.
func (g github) latestTemurin(ctx context.Context, feature int) (updates.Version, error) {
	repo := fmt.Sprintf("adoptium/temurin%d-binaries", feature)
	tag, err := g.latestTag(ctx, repo)
	if err != nil {
		return updates.Version{}, err
	}
	return parseStableTag(repo, strings.TrimPrefix(tag, "jdk-"))
}

func parseStableTag(repo, tag string) (updates.Version, error) {
	v, err := updates.Parse(tag)
	if err != nil {
		return updates.Version{}, fmt.Errorf("github: parse tag %q for %s: %w", tag, repo, err)
	}
	if v.IsPrerelease() {
		return updates.Version{}, fmt.Errorf("github: %s latest tag %q parses as a prerelease", repo, tag)
	}
	return v, nil
}

// latestTag fetches the tag of repo's /releases/latest.
func (g github) latestTag(ctx context.Context, repo string) (string, error) {
	return g.releaseTag(ctx, repo, "releases/latest")
}

func (g github) get(ctx context.Context, path, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	ua := g.userAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", accept)
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}

	return g.hc.Do(req)
}

func (g github) releaseTag(ctx context.Context, repo, endpoint string) (string, error) {
	resp, err := g.get(ctx, "/repos/"+repo+"/"+endpoint, "application/vnd.github+json")
	if err != nil {
		return "", fmt.Errorf("github: get %s: %w", repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 is the ambiguous one: GitHub hides a private repo behind it rather than
		// answering 401/403, so an unauthenticated miss and a repo with no stable release
		// are the same status. Name both causes, and name the fix for the one an operator
		// can act on. The official repository is public, so there only the first applies.
		if endpoint == "releases/latest" && resp.StatusCode == http.StatusNotFound && strings.EqualFold(repo, officialRepo) {
			return "", fmt.Errorf("github: %s releases/latest returned HTTP 404 — it has no published stable release", repo)
		}
		if resp.StatusCode == http.StatusNotFound && g.token == "" {
			return "", fmt.Errorf(
				"github: %s %s returned HTTP 404 — either no published stable release exists, or the repository is private and %s is unset",
				repo, endpoint, tokenEnv)
		}
		return "", fmt.Errorf("github: %s %s returned HTTP %d", repo, endpoint, resp.StatusCode)
	}

	var rr releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return "", fmt.Errorf("github: decode %s: %w", repo, err)
	}
	if rr.Draft || rr.Prerelease {
		return "", fmt.Errorf("github: %s %s is unexpectedly draft/prerelease (tag %q)", repo, endpoint, rr.TagName)
	}

	return rr.TagName, nil
}
