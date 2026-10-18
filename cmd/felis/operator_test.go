package main

import (
	"context"
	"net/http/httptest"
	"testing"
)

type fakeCache bool

func (f fakeCache) WaitForCacheSync(ctx context.Context) bool {
	if !f {
		<-ctx.Done()
	}
	return bool(f)
}

func TestCacheSynced(t *testing.T) {
	req := httptest.NewRequest("GET", "/readyz", nil)
	if err := cacheSynced(fakeCache(true))(req); err != nil {
		t.Errorf("synced: %v", err)
	}
	if err := cacheSynced(fakeCache(false))(req); err == nil {
		t.Error("unsynced caches reported ready")
	}
}
