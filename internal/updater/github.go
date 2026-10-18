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

const githubBaseURL = "https://api.github.com"

// GitHub latest follows release creation time rather than maximum semantic version; this source reports that selected stable release.
type github struct {
	baseURL   string
	userAgent string // GitHub requires a nonempty User-Agent
	// token is optional and environment-provided; private-repository 404 responses can also mean missing credentials.
	token string
	hc    *http.Client
}

// tokenEnv must match the credential variable used by bootstrap for private-repository cloning.
const tokenEnv = "FELIS_GITHUB_TOKEN"

func newGitHub() github {
	return github{
		baseURL:   githubBaseURL,
		userAgent: defaultUserAgent,
		token:     os.Getenv(tokenEnv),
		hc:        &http.Client{Timeout: 15 * time.Second},
	}
}

type releaseResponse struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
}

// latestStable rejects malformed, draft or prerelease responses even if the latest endpoint returns them.
func (g github) latestStable(ctx context.Context, repo string) (updates.Version, error) {
	tag, err := g.latestTag(ctx, repo)
	if err != nil {
		return updates.Version{}, err
	}
	return parseStableTag(repo, tag)
}

// latestTemurin restricts discovery to the current JDK feature and strips the jdk- tag prefix before parsing.
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
		// For private forks a 404 may mean missing credentials or no stable release; report both possibilities.
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
