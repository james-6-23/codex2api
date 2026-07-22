package admin

import (
	"context"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
)

// TestRefreshAccountByIDTriggersUsageProbe 验证 issue #300：手动刷新账号后，
// 会顺带触发一次用量探针（wham），从服务端权威数据同步订阅到期时间，
// 而不是仅依赖可能滞后的 token JWT。
func TestRefreshAccountByIDTriggersUsageProbe(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)

	id, err := db.InsertAccountWithCredentials(context.Background(), "renew", map[string]interface{}{
		"refresh_token": "rt-renew",
		"access_token":  "at-renew",
		"email":         "renew@example.com",
		"workspace_id":  "acc-renew",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}

	refreshed := false
	probedID := int64(0)
	handler := &Handler{
		db:    db,
		store: store,
		refreshAccount: func(context.Context, int64) error {
			refreshed = true
			return nil
		},
		probeUsage: func(_ context.Context, acc *auth.Account) error {
			if acc != nil {
				probedID = acc.DBID
			}
			return nil
		},
	}

	if err := handler.refreshAccountByID(context.Background(), id); err != nil {
		t.Fatalf("refreshAccountByID: %v", err)
	}
	if !refreshed {
		t.Fatal("token refresh was not invoked")
	}
	if probedID != id {
		t.Fatalf("usage probe ran for account %d, want %d (subscription expiry sync after refresh)", probedID, id)
	}
}

// TestRefreshAccountByIDSkipsProbeOnRefreshFailure 验证刷新失败时不再触发探针。
func TestRefreshAccountByIDSkipsProbeOnRefreshFailure(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)

	probed := false
	handler := &Handler{
		db:    db,
		store: store,
		refreshAccount: func(context.Context, int64) error {
			return context.DeadlineExceeded
		},
		probeUsage: func(context.Context, *auth.Account) error {
			probed = true
			return nil
		},
	}

	if err := handler.refreshAccountByID(context.Background(), 1); err == nil {
		t.Fatal("refreshAccountByID should return the refresh error")
	}
	if probed {
		t.Fatal("usage probe should not run when token refresh fails")
	}
}
