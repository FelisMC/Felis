//go:build pgint

package pgint

import (
	"context"
	"errors"
	"sync"
	"testing"

	"felis.lolicon.best/internal/api"
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
