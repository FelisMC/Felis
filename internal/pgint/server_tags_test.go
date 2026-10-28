//go:build pgint

package pgint

import (
	"context"
	"errors"
	"slices"
	"testing"

	"felis.lolicon.best/internal/api"
)

func TestServerTagsReachTheirReaders(t *testing.T) {
	ctx := context.Background()
	owner := newUser(t, "user", "tags")
	sfx := suffix(t)
	mine, free, bare := "tg-"+sfx, "tgf-"+sfx, "tgb-"+sfx
	seedOwnedServer(t, mine, owner.ID, false)
	seedOwnedServer(t, bare, owner.ID, false)
	mustExec(t, `INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 100, 128, 1)`, free)

	mineTags, freeTags := []string{"PvP", "生存"}, []string{"Hub"}
	for name, tags := range map[string][]string{mine: mineTags, free: freeTags} {
		if err := repo.SetServerTags(ctx, name, tags); err != nil {
			t.Fatalf("SetServerTags(%s): %v", name, err)
		}
	}

	all, err := repo.ServerTags(ctx)
	if err != nil {
		t.Fatalf("ServerTags: %v", err)
	}
	if !slices.Equal(all[mine], mineTags) || !slices.Equal(all[free], freeTags) {
		t.Fatalf("ServerTags = %v, want %s: %v and %s: %v", all, mine, mineTags, free, freeTags)
	}
	if tags, listed := all[bare]; listed {
		t.Fatalf("ServerTags lists the untagged %s with %v", bare, tags)
	}

	views, err := repo.MyServers(ctx, owner.ID)
	if err != nil {
		t.Fatalf("MyServers: %v", err)
	}
	got := map[string][]string{}
	for _, v := range views {
		got[v.Name] = v.Tags
	}
	if !slices.Equal(got[mine], mineTags) {
		t.Errorf("own row tags = %v, want %v", got[mine], mineTags)
	}
	if tags, listed := got[free]; !listed || len(tags) != 0 {
		t.Errorf("claimable row tags = %v (listed %v), want the row without tags", tags, listed)
	}

	rec, err := repo.ServerByName(ctx, mine)
	if err != nil || !slices.Equal(rec.Tags, mineTags) {
		t.Fatalf("ServerByName = %+v, %v; want tags %v", rec, err, mineTags)
	}

	if err := repo.SetServerTags(ctx, mine, nil); err != nil {
		t.Fatalf("clearing the tags: %v", err)
	}
	if rec, err := repo.ServerByName(ctx, mine); err != nil || len(rec.Tags) != 0 {
		t.Fatalf("after clearing: ServerByName = %+v, %v", rec, err)
	}
}

func TestServerTagsRefuseWhatTheyCannotHold(t *testing.T) {
	ctx := context.Background()
	owner := newUser(t, "user", "tagsmax")
	sfx := suffix(t)
	name, gone := "tgm-"+sfx, "tgd-"+sfx
	seedOwnedServer(t, name, owner.ID, false)
	seedOwnedServer(t, gone, owner.ID, true)

	six := []string{"a", "b", "c", "d", "e", "f"}
	if err := repo.SetServerTags(ctx, name, six); err != nil {
		t.Fatalf("six tags: %v", err)
	}
	if err := repo.SetServerTags(ctx, name, append(slices.Clone(six), "g")); err == nil {
		t.Fatal("seven tags were stored")
	}
	if rec, err := repo.ServerByName(ctx, name); err != nil || !slices.Equal(rec.Tags, six) {
		t.Fatalf("after the refused write: ServerByName = %+v, %v; want %v", rec, err, six)
	}
	for _, n := range []string{gone, "tgn-" + sfx} {
		if err := repo.SetServerTags(ctx, n, []string{"x"}); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("SetServerTags(%s) = %v, want ErrNotFound", n, err)
		}
	}
}

func TestServerTagsResetWhenAServerChangesHands(t *testing.T) {
	ctx := context.Background()
	owner := newUser(t, "user", "tagsend")
	next := newUser(t, "user", "tagsnext")
	sfx := suffix(t)
	released, recreated := "tge-"+sfx, "tgr-"+sfx
	seedOwnedServer(t, released, owner.ID, false)
	if err := repo.SeedServer(ctx, recreated, "tgrs-"+sfx, 100, 128, 1); err != nil {
		t.Fatalf("SeedServer: %v", err)
	}
	for _, n := range []string{released, recreated} {
		if err := repo.SetServerTags(ctx, n, []string{"old"}); err != nil {
			t.Fatalf("SetServerTags(%s): %v", n, err)
		}
	}

	mustExec(t, `UPDATE servers SET owner_id = NULL WHERE name = $1`, released)
	if ok, err := repo.ClaimServer(ctx, released, next.ID); err != nil || !ok {
		t.Fatalf("ClaimServer = (%v, %v)", ok, err)
	}
	if err := repo.SeedServer(ctx, recreated, "tgrs-"+sfx, 100, 128, 1); err != nil {
		t.Fatalf("SeedServer again: %v", err)
	}
	for _, n := range []string{released, recreated} {
		if rec, err := repo.ServerByName(ctx, n); err != nil || len(rec.Tags) != 0 {
			t.Errorf("%s: ServerByName = %+v, %v; want no tags", n, rec, err)
		}
	}
}
