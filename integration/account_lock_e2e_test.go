package integration

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

// TestAccountLock_E2E 测试锁定账号完整生命周期
func TestAccountLock_E2E(t *testing.T) {
	// 1. 创建内存Store
	store := auth.NewStore(nil, nil, nil)
	ctx := context.Background()

	// 2. 添加账号
	acc := &auth.Account{DBID: 1, AccessToken: "important_account", Status: auth.StatusReady}
	store.AddAccount(acc)

	// 3. 锁定账号
	err := store.SetLocked(1, true)
	if err != nil {
		t.Fatalf("锁定失败: %v", err)
	}

	// 4. 验证锁定状态
	if !acc.IsLocked() {
		t.Fatal("账号应该已被锁定")
	}

	// 5. 模拟过期清理
	acc.AddedAt = time.Now().Add(-2 * time.Hour).UnixNano()
	cleaned := store.CleanExpiredAccounts(ctx, 1*time.Hour)

	// 6. 验证锁定账号未被清理
	if cleaned != 0 {
		t.Error("锁定账号不应该被清理")
	}

	accounts := store.Accounts()
	if len(accounts) != 1 {
		t.Errorf("应该保留1个锁定账号，实际%d个", len(accounts))
	}

	// 7. 解锁账号
	err = store.SetLocked(1, false)
	if err != nil {
		t.Fatalf("解锁失败: %v", err)
	}

	// 8. 再次触发清理
	cleaned = store.CleanExpiredAccounts(ctx, 1*time.Hour)

	// 9. 验证解锁账号被清理
	if cleaned != 1 {
		t.Error("解锁账号应该被清理")
	}

	accounts = store.Accounts()
	if len(accounts) != 0 {
		t.Errorf("账号池应该为空，实际%d个账号", len(accounts))
	}
}

// TestBatchLock_E2E 测试批量锁定完整流程
func TestBatchLock_E2E(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	ctx := context.Background()

	// 添加多个账号
	for i := 1; i <= 5; i++ {
		acc := &auth.Account{DBID: int64(i), AccessToken: "test", Status: auth.StatusReady}
		acc.AddedAt = time.Now().Add(-2 * time.Hour).UnixNano()
		store.AddAccount(acc)
	}

	// 批量锁定账号1, 2, 3
	for _, id := range []int64{1, 2, 3} {
		err := store.SetLocked(id, true)
		if err != nil {
			t.Fatalf("锁定账号%d失败: %v", id, err)
		}
	}

	// 触发过期清理
	cleaned := store.CleanExpiredAccounts(ctx, 1*time.Hour)

	// 验证只清理了未锁定账号
	if cleaned != 2 {
		t.Errorf("应该清理2个未锁定账号，实际%d个", cleaned)
	}

	accounts := store.Accounts()
	if len(accounts) != 3 {
		t.Errorf("应该保留3个锁定账号，实际%d个", len(accounts))
	}

	// 验证保留的都是锁定账号
	for _, acc := range accounts {
		if !acc.IsLocked() {
			t.Error("保留的账号应该是锁定状态")
		}
	}
}
