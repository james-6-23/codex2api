package auth

import (
	"context"
	"testing"
	"time"
)

// TestAccount_IsLocked 测试原子读取锁定状态
func TestAccount_IsLocked(t *testing.T) {
	acc := &Account{DBID: 1, Locked: 0}
	if acc.IsLocked() {
		t.Error("未锁定账号应该返回false")
	}

	acc.Locked = 1
	if !acc.IsLocked() {
		t.Error("已锁定账号应该返回true")
	}
}

// TestStore_SetLocked 测试设置锁定状态
func TestStore_SetLocked(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{DBID: 1, AccessToken: "test", Status: StatusReady}
	store.AddAccount(acc)

	// 测试锁定
	err := store.SetLocked(1, true)
	if err != nil {
		t.Fatalf("锁定失败: %v", err)
	}
	if !acc.IsLocked() {
		t.Error("锁定后IsLocked应返回true")
	}

	// 测试解锁
	err = store.SetLocked(1, false)
	if err != nil {
		t.Fatalf("解锁失败: %v", err)
	}
	if acc.IsLocked() {
		t.Error("解锁后IsLocked应返回false")
	}
}

// TestStore_SetLocked_NotFound 测试不存在的账号
func TestStore_SetLocked_NotFound(t *testing.T) {
	store := NewStore(nil, nil, nil)
	err := store.SetLocked(999, true)
	if err == nil {
		t.Error("不存在的账号应该返回错误")
	}
}

// TestRemoveAccounts_SkipLocked 测试RemoveAccounts跳过锁定账号
func TestRemoveAccounts_SkipLocked(t *testing.T) {
	store := NewStore(nil, nil, nil)

	acc1 := &Account{DBID: 1, AccessToken: "unlocked", Status: StatusReady}
	acc2 := &Account{DBID: 2, AccessToken: "locked", Status: StatusReady, Locked: 1}
	acc3 := &Account{DBID: 3, AccessToken: "unlocked2", Status: StatusReady}

	store.AddAccount(acc1)
	store.AddAccount(acc2)
	store.AddAccount(acc3)

	// 尝试删除所有账号
	store.RemoveAccounts([]int64{1, 2, 3})

	// 验证锁定账号未被删除
	accounts := store.Accounts()
	if len(accounts) != 1 {
		t.Errorf("应该保留1个锁定账号，实际保留%d个", len(accounts))
	}
	if len(accounts) > 0 && accounts[0].DBID != 2 {
		t.Errorf("保留的应该是锁定账号DBID=2，实际是DBID=%d", accounts[0].DBID)
	}
}

// TestCleanExpiredAccounts_SkipLocked 测试过期清理跳过锁定账号
func TestCleanExpiredAccounts_SkipLocked(t *testing.T) {
	store := NewStore(nil, nil, nil)
	ctx := context.Background()

	// 添加过期账号
	now := time.Now()
	expiredTime := now.Add(-2 * time.Hour).UnixNano()

	acc1 := &Account{DBID: 1, AccessToken: "expired_unlocked", Status: StatusReady, AddedAt: expiredTime}
	acc2 := &Account{DBID: 2, AccessToken: "expired_locked", Status: StatusReady, AddedAt: expiredTime, Locked: 1}

	store.AddAccount(acc1)
	store.AddAccount(acc2)

	// 清理1小时前的账号
	cleaned := store.CleanExpiredAccounts(ctx, 1*time.Hour)

	// 验证只清理了未锁定账号
	if cleaned != 1 {
		t.Errorf("应该清理1个未锁定账号，实际清理%d个", cleaned)
	}

	accounts := store.Accounts()
	if len(accounts) != 1 {
		t.Errorf("应该保留1个锁定账号，实际保留%d个", len(accounts))
	}
	if len(accounts) > 0 && !accounts[0].IsLocked() {
		t.Error("保留的账号应该是锁定状态")
	}
}
