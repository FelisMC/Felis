//go:build pgint

package pgint

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/archivepolicy"
	"felis.lolicon.best/internal/mailsettings"
	"felis.lolicon.best/internal/reaper"
)

func TestSettingsCompareAndSet(t *testing.T) {
	ctx := context.Background()
	key := "auth-sources-" + suffix(t)
	defer db.ExecContext(ctx, "DELETE FROM platform_settings WHERE key=$1", key)
	first := []byte(`[{"tag":"custom","enabled":true}]`)
	next := []byte(`[{"tag":"custom","enabled":false}]`)
	if err := repo.CompareAndSetSetting(ctx, key, first, next); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("missing update = %v", err)
	}
	if err := repo.CompareAndSetSetting(ctx, key, nil, first); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompareAndSetSetting(ctx, key, nil, next); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("concurrent insert = %v", err)
	}
	// jsonb equality must survive PostgreSQL's different spacing and key order.
	raw, err := repo.GetSetting(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- repo.CompareAndSetSetting(ctx, key, raw, next) }()
	}
	wg.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, api.ErrConflict):
			conflicted++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent update: success=%d conflict=%d", succeeded, conflicted)
	}
	if err := repo.CompareAndSetSetting(ctx, key, []byte(`[ { "enabled" : false, "tag" : "custom" } ]`), first); err != nil {
		t.Fatalf("JSON equality = %v", err)
	}
}

// TestMailSettingsRow pins the platform_settings row the panel's mail relay lives in as the
// senders outside felis-api read it: a saved relay comes back with its password, and a
// row cleared to JSON null reads as no relay.
func TestMailSettingsRow(t *testing.T) {
	ctx := context.Background()
	defer db.ExecContext(ctx, "DELETE FROM platform_settings WHERE key=$1", mailsettings.Key)
	db.ExecContext(ctx, "DELETE FROM platform_settings WHERE key=$1", mailsettings.Key)
	if s, err := mailsettings.Read(ctx, db); s != nil || err != nil {
		t.Fatalf("no row = %+v, %v", s, err)
	}
	value, _ := json.Marshal(mailsettings.Settings{Host: "smtp.example.com", Port: 587, From: "a@example.com", Username: "a@example.com", Password: "pw", RequireTLS: true})
	if err := repo.CompareAndSetSetting(ctx, mailsettings.Key, nil, value); err != nil {
		t.Fatal(err)
	}
	s, err := mailsettings.Read(ctx, db)
	if err != nil || s == nil || s.Host != "smtp.example.com" || s.Password != "pw" {
		t.Fatalf("saved row = %+v, %v", s, err)
	}
	raw, err := repo.GetSetting(ctx, mailsettings.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CompareAndSetSetting(ctx, mailsettings.Key, raw, []byte("null")); err != nil {
		t.Fatal(err)
	}
	if s, err := mailsettings.Read(ctx, db); s != nil || err != nil {
		t.Fatalf("cleared row = %+v, %v", s, err)
	}

	// felis setup hands the relay back the same way.
	if err := repo.CompareAndSetSetting(ctx, mailsettings.Key, []byte("null"), value); err != nil {
		t.Fatal(err)
	}
	if err := mailsettings.Clear(ctx, db); err != nil {
		t.Fatal(err)
	}
	if s, err := mailsettings.Read(ctx, db); s != nil || err != nil {
		t.Fatalf("row after felis setup = %+v, %v", s, err)
	}
}

// TestArchivePolicyRow pins the retention policy's round trip through jsonb: what the
// panel saves is what the backup Job and the reaper apply, and JSON null hands retention
// back to felis.toml.
func TestArchivePolicyRow(t *testing.T) {
	ctx := context.Background()
	defer db.ExecContext(ctx, "DELETE FROM platform_settings WHERE key=$1", archivepolicy.Key)
	db.ExecContext(ctx, "DELETE FROM platform_settings WHERE key=$1", archivepolicy.Key)
	rc := reaper.DefaultConfig()
	if err := archivepolicy.ApplyFrom(ctx, db, &rc); err != nil || rc.ManualKeep != reaper.DefaultConfig().ManualKeep {
		t.Fatalf("no row: %+v, %v", rc, err)
	}
	p := archivepolicy.Policy{ManualKeep: 9, ManualRetentionDays: 40, ManualCooldownMinutes: 0, ScheduledEveryHours: 12, ScheduledKeep: 3, ScheduledRetentionDays: 20, ReleasedRetentionDays: 60, MaxStoreGiB: 50, AuditRetentionDays: 0}
	value, _ := json.Marshal(p)
	if err := repo.CompareAndSetSetting(ctx, archivepolicy.Key, nil, value); err != nil {
		t.Fatal(err)
	}
	if err := archivepolicy.ApplyFrom(ctx, db, &rc); err != nil || rc.ManualKeep != 9 || rc.ScheduledEvery != 12*time.Hour || rc.MaxLocalBytes != 50<<30 {
		t.Fatalf("saved row applied: %+v, %v", rc, err)
	}
	raw, err := repo.GetSetting(ctx, archivepolicy.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CompareAndSetSetting(ctx, archivepolicy.Key, raw, []byte("null")); err != nil {
		t.Fatal(err)
	}
	if got, err := archivepolicy.Read(ctx, db); got != nil || err != nil {
		t.Fatalf("cleared row = %+v, %v", got, err)
	}
}
