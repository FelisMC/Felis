//go:build pgint

package pgint

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/submit"
)

// ---- list pages and batched lookups (api-surface-7) ------------------------------

// GetBuilds answers a page's worth of build lookups in one query: the rows that
// exist, with their outcome and without the Dockerfile, and nothing for an id
// that has no row or for no ids at all.
func TestBuildStoreGetBuilds(t *testing.T) {
	ctx := context.Background()
	s := build.NewPGStore(db)
	tag := suffix(t)
	ok, failed := "bld-ok-"+tag, "bld-failed-"+tag
	for _, id := range []string{ok, failed} {
		if err := s.CreateBuild(ctx, &build.Build{ID: id, ImageRef: "registry.felis.svc:5000/x/" + id + ":1",
			Status: build.StatusPending, RequestedBy: "pgint", Dockerfile: "FROM scratch\n", CreatedAt: mustNow()}); err != nil {
			t.Fatalf("CreateBuild(%s): %v", id, err)
		}
	}
	if err := s.FinishBuild(ctx, failed, build.StatusFailed, "trivy: CRITICAL", mustNow()); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}
	got, err := s.GetBuilds(ctx, []string{ok, failed, "bld-gone-" + tag})
	if err != nil {
		t.Fatalf("GetBuilds: %v", err)
	}
	byID := map[string]build.Build{}
	for _, b := range got {
		byID[b.ID] = b
	}
	if len(got) != 2 || byID[ok].Status != build.StatusPending ||
		byID[failed].Status != build.StatusFailed || byID[failed].Error != "trivy: CRITICAL" {
		t.Fatalf("GetBuilds = %+v, want %s pending and %s failed with its error", got, ok, failed)
	}
	if byID[ok].Dockerfile != "" || byID[ok].RequestedBy != "pgint" {
		t.Fatalf("row = %+v, want the requester and no Dockerfile", byID[ok])
	}
	if got, err := s.GetBuilds(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("GetBuilds(nil) = %v, %v; want nothing", got, err)
	}
}

// PageSubmissions pages one scope newest first (id breaking a created_at tie),
// filters by status and by any part of the id, submitter or name, reports the
// filtered total, and counts the scope's statuses regardless of the filters.
func TestSubmitStorePageSubmissions(t *testing.T) {
	ctx := context.Background()
	s := submit.NewPGStore(db)
	tag := suffix(t)
	alice, bob := "pgint-alice-"+tag, "pgint-bob-"+tag
	base := mustNow().Add(-time.Hour).Truncate(time.Second)
	// sub-0 oldest … sub-4 newest; sub-2 and sub-3 share a timestamp.
	at := []time.Duration{0, time.Minute, 2 * time.Minute, 2 * time.Minute, 3 * time.Minute}
	ids := make([]string, len(at))
	for i := range at {
		ids[i] = fmt.Sprintf("sub-%s-%d", tag, i)
		if _, err := s.CreateSubmission(ctx, &submit.Submission{ID: ids[i], SubmittedBy: alice,
			DisplayName: fmt.Sprintf("Pack %d of %s", i, tag), ContextRef: "s3://b/" + ids[i],
			Status: submit.StatusPendingReview, CreatedAt: base.Add(at[i])}, 100); err != nil {
			t.Fatalf("CreateSubmission(%d): %v", i, err)
		}
	}
	bobs := "sub-" + tag + "-bob"
	if _, err := s.CreateSubmission(ctx, &submit.Submission{ID: bobs, SubmittedBy: bob,
		DisplayName: "Bob's pack " + tag, ContextRef: "s3://b/" + bobs,
		Status: submit.StatusPendingReview, CreatedAt: base}, 100); err != nil {
		t.Fatalf("CreateSubmission(bob): %v", err)
	}
	if won, err := s.ApproveSubmission(ctx, ids[1], "admin", "registry/x", "", mustNow()); err != nil || !won {
		t.Fatalf("approve = %v, %v", won, err)
	}
	if won, err := s.RejectSubmission(ctx, ids[0], "admin", "no", mustNow()); err != nil || !won {
		t.Fatalf("reject = %v, %v", won, err)
	}
	idsOf := func(p submit.Page) string {
		var out []string
		for _, sub := range p.Submissions {
			out = append(out, sub.ID)
		}
		return strings.Join(out, ",")
	}
	page := func(opts submit.ListOpts) submit.Page {
		t.Helper()
		p, err := s.PageSubmissions(ctx, opts)
		if err != nil {
			t.Fatalf("PageSubmissions(%+v): %v", opts, err)
		}
		return p
	}

	p := page(submit.ListOpts{SubmittedBy: alice, Limit: 2})
	if want := ids[4] + "," + ids[3]; idsOf(p) != want || p.Total != 5 {
		t.Fatalf("first page = %s of %d, want %s of 5", idsOf(p), p.Total, want)
	}
	if p.Counts[submit.StatusPendingReview] != 3 || p.Counts[submit.StatusApproved] != 1 ||
		p.Counts[submit.StatusRejected] != 1 || len(p.Counts) != 3 {
		t.Fatalf("counts = %v, want 3 pending, 1 approved, 1 rejected", p.Counts)
	}
	p = page(submit.ListOpts{SubmittedBy: alice, Limit: 2, Offset: 2})
	if want := ids[2] + "," + ids[1]; idsOf(p) != want {
		t.Fatalf("second page = %s, want %s (the tie broken by id)", idsOf(p), want)
	}
	p = page(submit.ListOpts{SubmittedBy: alice, Status: submit.StatusApproved, Limit: 10})
	if idsOf(p) != ids[1] || p.Total != 1 || p.Counts[submit.StatusPendingReview] != 3 {
		t.Fatalf("approved = %s of %d with counts %v, want %s of 1 and the scope's counts", idsOf(p), p.Total, p.Counts, ids[1])
	}
	p = page(submit.ListOpts{SubmittedBy: alice, Query: strings.ToUpper("pack 2 of " + tag), Limit: 10})
	if idsOf(p) != ids[2] || p.Total != 1 {
		t.Fatalf("name query = %s of %d, want %s", idsOf(p), p.Total, ids[2])
	}
	p = page(submit.ListOpts{SubmittedBy: alice, Query: tag + "-3", Limit: 10})
	if idsOf(p) != ids[3] {
		t.Fatalf("id query = %s, want %s", idsOf(p), ids[3])
	}
	p = page(submit.ListOpts{SubmittedBy: bob, Limit: 10})
	if idsOf(p) != bobs || p.Total != 1 || p.Counts[submit.StatusPendingReview] != 1 || len(p.Counts) != 1 {
		t.Fatalf("bob's scope = %s of %d with %v, want only %s", idsOf(p), p.Total, p.Counts, bobs)
	}
	// The admin queue spans submitters; the tag keeps the shared schema out.
	p = page(submit.ListOpts{Query: tag, Limit: 10})
	if p.Total != 6 || !containsSubmission(p.Submissions, bobs) || !containsSubmission(p.Submissions, ids[0]) {
		t.Fatalf("admin queue = %s of %d, want all 6", idsOf(p), p.Total)
	}
	p = page(submit.ListOpts{Query: "pgint-bob-" + tag, Limit: 10})
	if idsOf(p) != bobs {
		t.Fatalf("submitter query = %s, want %s", idsOf(p), bobs)
	}
}

// The backups list pages newest first inside the caller's scope, narrows to one
// server on request, counts the matches, and never lists a backup that is gone.
func TestBackupListPaging(t *testing.T) {
	ctx := context.Background()
	tag := suffix(t)
	s1, s2 := "pg-a-"+tag, "pg-b-"+tag
	uid, other := "owner-"+tag, "other-"+tag
	base := mustNow().Add(-time.Hour).Truncate(time.Second)
	seed := func(id, server, owner, status string, at time.Duration) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO world_backups (id, server_name, former_owner, backup_ref, size_bytes, reason, status, created_at, expires_at)
			 VALUES ($1, $2, $3, $4, 1, 'manual', $5, $6, $7)`,
			id, server, owner, "/archives/"+id, status, base.Add(at), base.Add(90*24*time.Hour)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	b := func(n string) string { return "bk-" + tag + "-" + n }
	seed(b("1"), s1, uid, "present", 0)
	seed(b("2"), s1, other, "present", time.Minute)
	seed(b("3"), s1, uid, "present", time.Minute) // ties with 2
	seed(b("4"), s1, uid, "deleted", 2*time.Minute)
	seed(b("5"), s2, uid, "present", 3*time.Minute)
	idsOf := func(vs []api.BackupView) string {
		var out []string
		for _, v := range vs {
			out = append(out, v.ID)
		}
		return strings.Join(out, ",")
	}

	vs, total, err := repo.AllBackups(ctx, api.BackupListOpts{Server: s1, Limit: 2})
	if err != nil || idsOf(vs) != b("3")+","+b("2") || total != 3 {
		t.Fatalf("s1 first page = %s of %d (%v), want %s,%s of 3", idsOf(vs), total, err, b("3"), b("2"))
	}
	vs, total, err = repo.AllBackups(ctx, api.BackupListOpts{Server: s1, Limit: 2, Offset: 2})
	if err != nil || idsOf(vs) != b("1") || total != 3 {
		t.Fatalf("s1 second page = %s of %d (%v), want %s of 3", idsOf(vs), total, err, b("1"))
	}
	vs, total, err = repo.BackupsForUser(ctx, uid, api.BackupListOpts{Server: s1, Limit: 10})
	if err != nil || idsOf(vs) != b("3")+","+b("1") || total != 2 {
		t.Fatalf("owner's s1 = %s of %d (%v), want %s,%s of 2", idsOf(vs), total, err, b("3"), b("1"))
	}
	vs, total, err = repo.BackupsForUser(ctx, uid, api.BackupListOpts{Limit: 10})
	if err != nil || idsOf(vs) != b("5")+","+b("3")+","+b("1") || total != 3 {
		t.Fatalf("owner's worlds = %s of %d (%v), want %s,%s,%s", idsOf(vs), total, err, b("5"), b("3"), b("1"))
	}
	vs, total, err = repo.BackupsForUser(ctx, other, api.BackupListOpts{Server: s2, Limit: 10})
	if err != nil || len(vs) != 0 || total != 0 {
		t.Fatalf("other's s2 = %s of %d (%v), want nothing", idsOf(vs), total, err)
	}
}
