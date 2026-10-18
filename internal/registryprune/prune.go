// Package registryprune deletes unused manifests while retaining dependencies, recent pushes, and rollback images.
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

const (
	DefaultGrace      = 24 * time.Hour
	DefaultKeepTagged = 5
)

type Registry interface {
	Repositories(ctx context.Context) ([]string, error)
	Index(ctx context.Context, repo string) (*registrygate.Index, error)
	Delete(ctx context.Context, repo, digest string) error
}

type Pruner struct {
	Registry Registry
	// Host selects image references belonging to this registry.
	Host string
	// Refs must return a complete view; an error aborts pruning.
	Refs func(ctx context.Context) ([]string, error)
	// Grace defaults to DefaultGrace.
	Grace time.Duration
	// KeepTagged retains recent tagged manifests per reserved repository; zero uses DefaultKeepTagged.
	KeepTagged int
	Now        func() time.Time
	Log        *slog.Logger
}

type Target struct {
	Repo, Digest string
}

func (t Target) String() string { return t.Repo + "@" + t.Digest }

type Report struct {
	Repositories int
	Manifests    int
	Deleted      []Target
	// Failed deletions are retried on the next run.
	Failed int
}

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

// Loop delays its first run by interval/10 to limit restart churn.
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
			if now.Sub(r.Pushed) < grace {
				keep[Target{repo, r.Digest}] = true
			}
		}
		keepChildren(repo, idx, keep)
		for _, r := range idx.Revisions {
			if t := (Target{repo, r.Digest}); !keep[t] {
				out = append(out, t)
			}
		}
	}
	return out
}

// Retain index descendants transitively, including platform manifests and attestations.
func keepChildren(repo string, idx *registrygate.Index, keep map[Target]bool) {
	for grew := true; grew; {
		grew = false
		for _, r := range idx.Revisions {
			if !keep[Target{repo, r.Digest}] {
				continue
			}
			for _, c := range r.Children {
				if t := (Target{repo, c}); !keep[t] {
					keep[t], grew = true, true
				}
			}
		}
	}
}

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

// ParseRef defaults references without a tag or digest to :latest.
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
