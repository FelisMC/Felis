//go:build pgint

package pgint

import (
	"context"
	"slices"
	"strings"
	"testing"

	"felis.lolicon.best/internal/watchdog"
)

// TestWatchdogOwnerEmails: the watchdog mails the verified address of every enabled
// owner account, in order, and no other account's.
func TestWatchdogOwnerEmails(t *testing.T) {
	ctx := context.Background()
	tag := suffix(t)
	addr := func(who string) string { return who + "-" + tag + "@example.com" }
	for _, u := range []struct {
		who, role, email string
		verified, off    bool
		deleted          bool
	}{
		{who: "zoe", role: "owner", email: addr("zoe"), verified: true},
		{who: "amy", role: "owner", email: addr("amy"), verified: true},
		{who: "unverified", role: "owner", email: addr("unverified")},
		{who: "disabled", role: "owner", email: addr("disabled"), verified: true, off: true},
		{who: "deleted", role: "owner", email: addr("deleted"), verified: true, deleted: true},
		{who: "admin", role: "admin", email: addr("admin"), verified: true},
		{who: "user", role: "user", email: addr("user"), verified: true},
		{who: "no-address", role: "owner", verified: true},
	} {
		var email any
		if u.email != "" {
			email = u.email
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO users (id, username, role, email, email_verified, disabled, deleted_at)
			 VALUES ($1, $1, $2, $3, $4, $5, CASE WHEN $6 THEN now() END)`,
			"pgint-"+u.who+"-"+tag, u.role, email, u.verified, u.off, u.deleted); err != nil {
			t.Fatalf("insert %s: %v", u.who, err)
		}
	}
	got, err := watchdog.OwnerEmails(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	// Other suites share the schema; their accounts carry other tags.
	mine := slices.DeleteFunc(got, func(e string) bool { return !strings.Contains(e, tag) })
	if want := []string{addr("amy"), addr("zoe")}; !slices.Equal(mine, want) {
		t.Fatalf("OwnerEmails = %q, want %q", mine, want)
	}
}
