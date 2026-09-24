// Package registryprune deletes the manifests in the platform registry that
// nothing uses any more, so the registry-gc sidecar's next sweep can free their
// layers. Without it the registry only grows: every rebuild of a tag leaves the
// previous manifest behind, a whitelist entry an admin removes keeps its image,
// and each installer run adds a platform image.
//
// A manifest is kept when any of these hold:
//
//   - an image reference the platform still depends on names it: a whitelist
//     entry (by tag, by wildcard tag, or by digest), a server's spec (servers are
//     pinned by digest, so a sleeping server keeps the exact build it was created
//     with even after its tag moved on), or the images the control plane and the
//     build Jobs run;
//   - it was pushed less than Grace ago, which covers a build that pushed but has
//     not been admitted to the whitelist yet;
//   - it lives under a platform-reserved root (felis/, mirror/), is tagged, and is
//     among the KeepTagged most recently pushed tagged manifests of its
//     repository. The installer owns those tags; the newest few stay for a
//     rollback, the older ones go.
//
// Everything else is deleted through the registry gate as the prune principal,
// which may do nothing but delete a manifest by digest.
package registryprune

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"felis.lolicon.best/internal/registrygate"
)

// Defaults for Pruner's zero fields.
const (
	DefaultGrace      = 24 * time.Hour
	DefaultKeepTagged = 5
)

// Registry is the pruner's view of the registry (Client in production).
type Registry interface {
	Repositories(ctx context.Context) ([]string, error)
	Index(ctx context.Context, repo string) (*registrygate.Index, error)
	Delete(ctx context.Context, repo, digest string) error
}

// Pruner decides and deletes.
type Pruner struct {
	Registry Registry
	// Host is the registry host[:port] image refs spell (registry.felis.svc:5000).
	// Refs under any other host are not this registry's and are ignored.
	Host string
	// Refs lists every image reference still in use. An error aborts the run: a
	// partial view could delete an image something depends on.
	Refs func(ctx context.Context) ([]string, error)
	// Grace keeps manifests pushed this recently. Zero means DefaultGrace.
	Grace time.Duration
	// KeepTagged is how many tagged manifests per reserved repository survive by
	// recency alone. Zero means DefaultKeepTagged.
	KeepTagged int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log receives one line per deletion and a summary. Nil discards.
	Log *slog.Logger
}

// Target is one manifest the pruner deletes.
type Target struct {
	Repo, Digest string
}

func (t Target) String() string { return t.Repo + "@" + t.Digest }

// Report summarizes one run.
type Report struct {
	Repositories int
	Manifests    int
	Deleted      []Target
	// Failed counts deletions the registry refused; they are retried next run.
	Failed int
}

// Run lists the registry, decides, and deletes.
func (p *Pruner) Run(ctx context.Context) (Report, error) {
	var rep Report
	if p.Registry == nil || p.Refs == nil || p.Host == "" {
		return rep, errors.New("registryprune: Registry, Refs and Host are required")
	}
	refs, err := p.Refs(ctx)
	if err != nil {
		return rep, fmt.Errorf("registryprune: list image references: %w", err)
	}
	repos, err := p.Registry.Repositories(ctx)
	if err != nil {
		return rep, fmt.Errorf("registryprune: list repositories: %w", err)
	}
	indexes := make(map[string]*registrygate.Index, len(repos))
	for _, repo := range repos {
		idx, err := p.Registry.Index(ctx, repo)
		if err != nil {
			return rep, fmt.Errorf("registryprune: index %s: %w", repo, err)
		}
		indexes[repo] = idx
		rep.Manifests += len(idx.Revisions)
	}
	rep.Repositories = len(repos)

	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	for _, t := range Plan(indexes, refs, p.Host, now(), p.grace(), p.keepTagged()) {
		if err := p.Registry.Delete(ctx, t.Repo, t.Digest); err != nil {
			rep.Failed++
			p.log().Warn("registry prune: delete failed", "manifest", t.String(), "err", err)
			continue
		}
		rep.Deleted = append(rep.Deleted, t)
		p.log().Info("registry prune: deleted manifest", "manifest", t.String())
	}
	return rep, nil
}

// Loop runs Run every interval until ctx ends, starting after one interval's
// tenth so a restart loop does not hammer the registry.
func (p *Pruner) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTimer(every / 10)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rep, err := p.Run(ctx)
		if err != nil {
			p.log().Error("registry prune failed", "err", err)
		} else {
			p.log().Info("registry prune finished", "repositories", rep.Repositories,
				"manifests", rep.Manifests, "deleted", len(rep.Deleted), "failed", rep.Failed)
		}
		t.Reset(every)
	}
}

func (p *Pruner) grace() time.Duration {
	if p.Grace > 0 {
		return p.Grace
	}
	return DefaultGrace
}

func (p *Pruner) keepTagged() int {
	if p.KeepTagged > 0 {
		return p.KeepTagged
	}
	return DefaultKeepTagged
}

func (p *Pruner) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Plan decides which manifests to delete. It is pure so every keep rule is
// tested without a registry.
func Plan(indexes map[string]*registrygate.Index, refs []string, host string, now time.Time, grace time.Duration, keepTagged int) []Target {
	keep := map[Target]bool{}
	for _, ref := range refs {
		repo, tag, digest, ok := ParseRef(ref, host)
		if !ok {
			continue
		}
		idx := indexes[repo]
		switch {
		case digest != "":
			keep[Target{repo, digest}] = true
		case idx == nil:
		case tag == "*":
			for _, d := range idx.Tags {
				keep[Target{repo, d}] = true
			}
		default:
			if d, ok := idx.Tags[tag]; ok {
				keep[Target{repo, d}] = true
			}
		}
	}

	repos := make([]string, 0, len(indexes))
	for repo := range indexes {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	var out []Target
	for _, repo := range repos {
		idx := indexes[repo]
		pushed := make(map[string]time.Time, len(idx.Revisions))
		for _, r := range idx.Revisions {
			pushed[r.Digest] = r.Pushed
		}
		if reserved(repo) {
			for _, d := range newestTagged(idx, pushed, keepTagged) {
				keep[Target{repo, d}] = true
			}
		}
		for _, r := range idx.Revisions {
			t := Target{repo, r.Digest}
			if keep[t] || now.Sub(r.Pushed) < grace {
				continue
			}
			out = append(out, t)
		}
	}
	return out
}

// newestTagged returns the n most recently pushed distinct digests any tag in idx
// names.
func newestTagged(idx *registrygate.Index, pushed map[string]time.Time, n int) []string {
	seen := map[string]bool{}
	var tagged []string
	for _, d := range idx.Tags {
		if !seen[d] {
			seen[d] = true
			tagged = append(tagged, d)
		}
	}
	sort.Slice(tagged, func(i, j int) bool {
		pi, pj := pushed[tagged[i]], pushed[tagged[j]]
		if !pi.Equal(pj) {
			return pi.After(pj)
		}
		return tagged[i] < tagged[j]
	})
	if len(tagged) > n {
		tagged = tagged[:n]
	}
	return tagged
}

func reserved(repo string) bool {
	root, _, _ := strings.Cut(repo, "/")
	for _, r := range registrygate.ReservedRepoRoots {
		if root == r {
			return true
		}
	}
	return false
}

// ParseRef splits host/repo[:tag][@digest] for refs under host. A ref with
// neither tag nor digest means :latest, as it does for every image client.
func ParseRef(ref, host string) (repo, tag, digest string, ok bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(ref), host+"/")
	if !ok || rest == "" {
		return "", "", "", false
	}
	name, digest, _ := strings.Cut(rest, "@")
	repo, tag = name, ""
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		repo, tag = name[:colon], name[colon+1:]
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return repo, tag, digest, repo != ""
}
