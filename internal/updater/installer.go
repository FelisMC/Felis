package updater

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
)

var releaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// InstallTarget pins both the installer and the source tree to one revision.
// Release is set only when installing checksum-verified published assets.
type InstallTarget struct {
	Release  string
	Revision string
	Script   string
}

// InstallerSource uses the same GitHub client and token as release discovery.
type InstallerSource struct {
	github github
	repo   string
}

func NewInstallerSource(repoURL string) (InstallerSource, error) {
	repo := officialRepo
	if repoURL != "" {
		if strings.HasPrefix(repoURL, "git@github.com:") {
			repo = strings.TrimPrefix(repoURL, "git@github.com:")
		} else {
			u, err := url.Parse(repoURL)
			if err != nil || u.Hostname() != "github.com" || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" {
				return InstallerSource{}, fmt.Errorf("FELIS_REPO_URL must name a GitHub repository over HTTPS or SSH")
			}
			repo = strings.TrimPrefix(u.Path, "/")
		}
		repo = strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
		if !githubRepoPattern.MatchString(repo) || strings.Contains(repo, "..") {
			return InstallerSource{}, fmt.Errorf("FELIS_REPO_URL must name a GitHub owner/repository")
		}
	}
	return InstallerSource{github: newGitHub(), repo: repo}, nil
}

func (s InstallerSource) RepoURL() string { return "https://github.com/" + s.repo + ".git" }

// Prepare downloads the complete script before any host changes. A moving ref
// (including main) is resolved once; the installer receives that same full SHA.
func (s InstallerSource) Prepare(ctx context.Context, release, ref string) (InstallTarget, error) {
	if ref == "" {
		endpoint := "releases/latest"
		if release != "" {
			endpoint = "releases/tags/" + url.PathEscape(release)
		}
		tag, err := s.github.releaseTag(ctx, s.repo, endpoint)
		if err != nil {
			return InstallTarget{}, err
		}
		if !releaseTagPattern.MatchString(tag) || (release != "" && tag != release) {
			return InstallTarget{}, fmt.Errorf("unexpected Felis release tag %q", tag)
		}
		release, ref = tag, tag
	}
	resp, err := s.github.get(ctx, "/repos/"+s.repo+"/commits/"+url.PathEscape(ref), "application/vnd.github+json")
	if err != nil {
		return InstallTarget{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return InstallTarget{}, fmt.Errorf("github: resolve ref %q: HTTP %d", ref, resp.StatusCode)
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commit); err != nil {
		return InstallTarget{}, fmt.Errorf("github: decode commit: %w", err)
	}
	if _, err := hex.DecodeString(commit.SHA); err != nil || len(commit.SHA) != 40 {
		return InstallTarget{}, fmt.Errorf("github: ref %q did not resolve to a full commit SHA", ref)
	}
	commit.SHA = strings.ToLower(commit.SHA)
	if _, err := hex.DecodeString(ref); err == nil && len(ref) == 40 && !strings.EqualFold(ref, commit.SHA) {
		return InstallTarget{}, fmt.Errorf("github: resolved commit %s differs from requested %s", commit.SHA, ref)
	}
	script, err := s.script(ctx, commit.SHA)
	if err != nil {
		return InstallTarget{}, err
	}
	return InstallTarget{Release: release, Revision: commit.SHA, Script: script}, nil
}

func (s InstallerSource) script(ctx context.Context, revision string) (string, error) {
	resp, err := s.github.get(ctx, "/repos/"+s.repo+"/contents/deploy/bootstrap.sh?ref="+revision, "application/vnd.github.raw+json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: download installer at %s: HTTP %d", revision, resp.StatusCode)
	}
	const limit = 2 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", fmt.Errorf("github: read installer: %w", err)
	}
	if len(raw) > limit || !strings.HasPrefix(string(raw), "#!/") {
		return "", fmt.Errorf("github: installer is oversized or is not a shell script")
	}
	return string(raw), nil
}
