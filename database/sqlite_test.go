package database

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewSQLiteInitializesFreshDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	if got := db.Driver(); got != "sqlite" {
		t.Fatalf("Driver() = %q, want %q", got, "sqlite")
	}
}

func TestSQLiteAPIKeyLookupAndCount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	key := "sk-test-lookup-1234567890"
	id, err := db.InsertAPIKey(ctx, "lookup", key)
	if err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}
	count, err := db.CountAPIKeys(ctx)
	if err != nil {
		t.Fatalf("CountAPIKeys 返回错误: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountAPIKeys = %d, want 1", count)
	}
	row, err := db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		t.Fatalf("GetAPIKeyByValue 返回错误: %v", err)
	}
	if row.ID != id || row.Name != "lookup" || row.Key != key {
		t.Fatalf("API key row = %#v, want id=%d name=lookup key=%s", row, id, key)
	}
}

func TestSQLiteAPIKeyReadDoesNotWaitBehindAccountWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := db.InsertAPIKey(ctx, "lookup", "sk-test-lookup-1234567890"); err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}
	accountID, err := db.InsertAccount(ctx, "writer", "rt-writer", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx 返回错误: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET updated_at = CURRENT_TIMESTAMP WHERE id = $1`, accountID); err != nil {
		t.Fatalf("hold write transaction: %v", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	count, err := db.CountAPIKeys(readCtx)
	if err != nil {
		t.Fatalf("CountAPIKeys while account write is open 返回错误: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountAPIKeys = %d, want 1", count)
	}
}

func TestSQLiteQueuedAccountWritesDoNotBlockAPIKeyReads(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := db.InsertAPIKey(ctx, "lookup", "sk-test-lookup-1234567890"); err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}
	accountIDs := make([]int64, 0, maxSQLiteOpenConns*2)
	for i := 0; i < maxSQLiteOpenConns*2; i++ {
		id, err := db.InsertAccount(ctx, "writer", "rt-writer", "")
		if err != nil {
			t.Fatalf("InsertAccount 返回错误: %v", err)
		}
		accountIDs = append(accountIDs, id)
	}

	db.sqliteWriteSem <- struct{}{}
	var wg sync.WaitGroup
	for _, accountID := range accountIDs {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			_ = db.UpdateCredentials(writeCtx, id, map[string]interface{}{"codex_7d_used_percent": 1})
		}(accountID)
	}

	readCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	count, err := db.CountAPIKeys(readCtx)
	<-db.sqliteWriteSem
	wg.Wait()
	if err != nil {
		t.Fatalf("CountAPIKeys while account writes are queued 返回错误: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountAPIKeys = %d, want 1", count)
	}
}

func TestSQLiteUpdateCredentialsMergesAtomically(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	accountID, err := db.InsertAccountWithCredentials(ctx, "merge", map[string]interface{}{
		"refresh_token": "rt-merge",
		"email":         "old@example.com",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials 返回错误: %v", err)
	}
	if err := db.UpdateCredentials(ctx, accountID, map[string]interface{}{
		"codex_7d_used_percent": 42.5,
		"email":                 "new@example.com",
	}); err != nil {
		t.Fatalf("UpdateCredentials 返回错误: %v", err)
	}

	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("GetAccountByID 返回错误: %v", err)
	}
	if got := row.GetCredential("refresh_token"); got != "rt-merge" {
		t.Fatalf("refresh_token = %q, want rt-merge", got)
	}
	if got := row.GetCredential("email"); got != "new@example.com" {
		t.Fatalf("email = %q, want new@example.com", got)
	}
	if got := row.GetCredential("codex_7d_used_percent"); got != "42.5" {
		t.Fatalf("codex_7d_used_percent = %q, want 42.5", got)
	}
}

func TestFindActiveAccountByOAuthIdentity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "identity", map[string]interface{}{
		"refresh_token":      "rt-identity",
		"email":              "User@Example.COM",
		"chatgpt_account_id": "acc-identity",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials 返回错误: %v", err)
	}

	got, err := db.FindActiveAccountByOAuthIdentity(ctx, " user@example.com ", "acc-identity")
	if err != nil {
		t.Fatalf("FindActiveAccountByOAuthIdentity 返回错误: %v", err)
	}
	if got != id {
		t.Fatalf("matched id = %d, want %d", got, id)
	}

	otherID, err := db.InsertAccountWithCredentials(ctx, "identity-other", map[string]interface{}{
		"refresh_token": "rt-identity-other",
		"email":         "user@example.com",
		"account_id":    "acc-identity",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials other 返回错误: %v", err)
	}
	got, err = db.FindActiveAccountByOAuthIdentity(ctx, "user@example.com", "acc-identity", id)
	if err != nil {
		t.Fatalf("FindActiveAccountByOAuthIdentity with exclude 返回错误: %v", err)
	}
	if got != otherID {
		t.Fatalf("matched id with exclude = %d, want %d", got, otherID)
	}

	if err := db.SoftDeleteAccount(ctx, id); err != nil {
		t.Fatalf("SoftDeleteAccount 返回错误: %v", err)
	}
	if err := db.SoftDeleteAccount(ctx, otherID); err != nil {
		t.Fatalf("SoftDeleteAccount other 返回错误: %v", err)
	}
	if _, err := db.FindActiveAccountByOAuthIdentity(ctx, "user@example.com", "acc-identity"); err == nil {
		t.Fatal("FindActiveAccountByOAuthIdentity 应该排除已删除账号")
	} else if err != sql.ErrNoRows {
		t.Fatalf("FindActiveAccountByOAuthIdentity err = %v, want sql.ErrNoRows", err)
	}
}

// 个人账号 JWT 可能没有工作区 account_id，只有 user_id（user-...）；此外旧版
// wham 回填曾把 user_id 写进 account_id 字段。身份匹配必须两个键都认，
// 否则 AT 轮换后同一账号会被重复导入。
func TestFindActiveAccountByOAuthIdentityMatchesUserID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// 账号 A：credentials 里存的是 user_id 键
	idA, err := db.InsertAccountWithCredentials(ctx, "uid-key", map[string]interface{}{
		"access_token": "at-uid-key",
		"email":        "solo@example.com",
		"user_id":      "user-abc123",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials A 返回错误: %v", err)
	}
	got, err := db.FindActiveAccountByOAuthIdentity(ctx, "solo@example.com", "user-abc123")
	if err != nil {
		t.Fatalf("match by user_id key 返回错误: %v", err)
	}
	if got != idA {
		t.Fatalf("matched id = %d, want %d", got, idA)
	}

	// 账号 B：旧版 wham 回填把 user_id 污染进了 account_id 字段
	idB, err := db.InsertAccountWithCredentials(ctx, "polluted", map[string]interface{}{
		"access_token": "at-polluted",
		"email":        "legacy@example.com",
		"account_id":   "user-def456", // 实为 user_id
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials B 返回错误: %v", err)
	}
	got, err = db.FindActiveAccountByOAuthIdentity(ctx, "legacy@example.com", "user-def456")
	if err != nil {
		t.Fatalf("match polluted account_id 返回错误: %v", err)
	}
	if got != idB {
		t.Fatalf("matched id = %d, want %d", got, idB)
	}
}

// v2 迁移：user_id 也是身份别名——个人账号（credentials 只有 user_id）和被旧版
// wham 回填污染（user_id 写进了 account_id）的账号必须合并为一组。
// 勾选"允许重复添加"强制导入的副本（allow_duplicate 标记）不参与合并。
func TestSQLiteDataMigrationV2DedupesByUserID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	ctx := context.Background()

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	if _, err := db.conn.ExecContext(ctx, `DELETE FROM data_migrations WHERE version = $1`, dataMigrationOAuthIdentityDedupeV2); err != nil {
		t.Fatalf("清理 v2 data migration 标记返回错误: %v", err)
	}

	// 旧账号：account_id 字段被污染成 user_id（旧版 wham 回填）
	pollutedID, err := db.InsertAccountWithCredentials(ctx, "polluted", map[string]interface{}{
		"access_token": "at-old-rotation",
		"email":        "solo@example.com",
		"account_id":   "user-dup999",
	}, "")
	if err != nil {
		t.Fatalf("Insert polluted 返回错误: %v", err)
	}
	// 新账号：正确存在 user_id 键（新导入路径）
	freshID, err := db.InsertAccountWithCredentials(ctx, "fresh", map[string]interface{}{
		"access_token": "at-new-rotation",
		"email":        "solo@example.com",
		"user_id":      "user-dup999",
	}, "")
	if err != nil {
		t.Fatalf("Insert fresh 返回错误: %v", err)
	}
	// 强制重复副本：allow_duplicate 标记，身份与上面相同，但必须保留
	forcedID, err := db.InsertAccountWithCredentials(ctx, "forced-dup", map[string]interface{}{
		"access_token":    "at-forced-copy",
		"email":           "solo@example.com",
		"user_id":         "user-dup999",
		"allow_duplicate": "true",
	}, "")
	if err != nil {
		t.Fatalf("Insert forced 返回错误: %v", err)
	}

	if err := db.runDataMigrationsWithTimeout(); err != nil {
		t.Fatalf("runDataMigrations 返回错误: %v", err)
	}

	var remaining int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id IN ($1, $2) AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`, pollutedID, freshID).Scan(&remaining); err != nil {
		t.Fatalf("查询存活账号数返回错误: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("v2 迁移后存活账号 = %d, want 1（user_id 重复应被合并）", remaining)
	}

	var forcedAlive int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`, forcedID).Scan(&forcedAlive); err != nil {
		t.Fatalf("查询强制副本返回错误: %v", err)
	}
	if forcedAlive != 1 {
		t.Fatal("allow_duplicate 副本被迁移误删，应保留")
	}

	// 强制副本也不作为身份判重锚点：按身份查找应命中主账号而非副本
	if got, err := db.FindActiveAccountByOAuthIdentity(ctx, "solo@example.com", "user-dup999"); err != nil {
		t.Fatalf("FindActiveAccountByOAuthIdentity 返回错误: %v", err)
	} else if got == forcedID {
		t.Fatal("身份判重不应命中 allow_duplicate 副本")
	}
}

func TestSQLiteDataMigrationDedupesOAuthIdentityOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	ctx := context.Background()

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}

	if _, err := db.conn.ExecContext(ctx, `DELETE FROM data_migrations WHERE version = $1`, dataMigrationOAuthIdentityDedupeV1); err != nil {
		t.Fatalf("清理 data migration 标记返回错误: %v", err)
	}

	oldTime := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	midTime := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	newTime := time.Now().UTC().Truncate(time.Second)

	oldID, err := db.InsertAccountWithCredentials(ctx, "old-duplicate", map[string]interface{}{
		"refresh_token": "rt-old-duplicate",
		"email":         "User@Example.com",
		"account_id":    "acc-dedupe",
	}, "")
	if err != nil {
		t.Fatalf("Insert old duplicate 返回错误: %v", err)
	}
	midID, err := db.InsertAccountWithCredentials(ctx, "mid-duplicate", map[string]interface{}{
		"access_token":        "at-mid-duplicate",
		"email":               "user@example.com",
		"chatgpt_account_id":  "acc-dedupe",
		"codex_7d_reset_at":   newTime.Format(time.RFC3339),
		"codex_5h_reset_at":   newTime.Format(time.RFC3339),
		"codex_usage_marker":  "keep-credentials-intact",
		"codex_usage_updated": "true",
	}, "")
	if err != nil {
		t.Fatalf("Insert mid duplicate 返回错误: %v", err)
	}
	winnerID, err := db.InsertAccountWithCredentials(ctx, "new-duplicate", map[string]interface{}{
		"session_token": "st-new-duplicate",
		"email":         " user@example.com ",
		"account_id":    "acc-dedupe",
	}, "")
	if err != nil {
		t.Fatalf("Insert winner 返回错误: %v", err)
	}
	otherID, err := db.InsertAccountWithCredentials(ctx, "other-workspace", map[string]interface{}{
		"refresh_token": "rt-other-workspace",
		"email":         "user@example.com",
		"account_id":    "acc-other",
	}, "")
	if err != nil {
		t.Fatalf("Insert other 返回错误: %v", err)
	}
	bridgeOldID, err := db.InsertAccountWithCredentials(ctx, "bridge-old", map[string]interface{}{
		"refresh_token":      "rt-bridge-old",
		"email":              "bridge@example.com",
		"account_id":         "acc-bridge-old",
		"chatgpt_account_id": "acc-bridge",
	}, "")
	if err != nil {
		t.Fatalf("Insert bridge old 返回错误: %v", err)
	}
	bridgeWinnerID, err := db.InsertAccountWithCredentials(ctx, "bridge-winner", map[string]interface{}{
		"access_token": "at-bridge-winner",
		"email":        "Bridge@Example.com",
		"account_id":   "acc-bridge",
	}, "")
	if err != nil {
		t.Fatalf("Insert bridge winner 返回错误: %v", err)
	}
	deletedID, err := db.InsertAccountWithCredentials(ctx, "deleted-duplicate", map[string]interface{}{
		"refresh_token": "rt-deleted-duplicate",
		"email":         "user@example.com",
		"account_id":    "acc-dedupe",
	}, "")
	if err != nil {
		t.Fatalf("Insert deleted 返回错误: %v", err)
	}
	if err := db.SoftDeleteAccount(ctx, deletedID); err != nil {
		t.Fatalf("SoftDeleteAccount 返回错误: %v", err)
	}

	for id, ts := range map[int64]time.Time{
		oldID:          oldTime,
		midID:          midTime,
		winnerID:       newTime,
		otherID:        midTime,
		bridgeOldID:    oldTime,
		bridgeWinnerID: newTime,
	} {
		if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET updated_at = $1 WHERE id = $2`, sqliteTimeParam(ts), id); err != nil {
			t.Fatalf("设置账号 %d updated_at 返回错误: %v", id, err)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	db, err = New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen New(sqlite) 返回错误: %v", err)
	}

	activeRows, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	activeIDs := map[int64]bool{}
	for _, row := range activeRows {
		activeIDs[row.ID] = true
	}
	if !activeIDs[winnerID] || !activeIDs[otherID] || !activeIDs[bridgeWinnerID] {
		t.Fatalf("active ids = %v, want winner %d, other %d and bridge winner %d", activeIDs, winnerID, otherID, bridgeWinnerID)
	}
	if activeIDs[oldID] || activeIDs[midID] || activeIDs[bridgeOldID] {
		t.Fatalf("active ids = %v, want duplicates %d/%d/%d soft-deleted", activeIDs, oldID, midID, bridgeOldID)
	}

	deletedRows, err := db.ListDeleted(ctx)
	if err != nil {
		t.Fatalf("ListDeleted 返回错误: %v", err)
	}
	deletedIDs := map[int64]bool{}
	for _, row := range deletedRows {
		deletedIDs[row.ID] = true
	}
	for _, id := range []int64{oldID, midID, bridgeOldID, deletedID} {
		if !deletedIDs[id] {
			t.Fatalf("deleted ids = %v, want id %d", deletedIDs, id)
		}
	}

	var migrationCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_migrations WHERE version = $1`, dataMigrationOAuthIdentityDedupeV1).Scan(&migrationCount); err != nil {
		t.Fatalf("查询 data_migrations 返回错误: %v", err)
	}
	if migrationCount != 1 {
		t.Fatalf("migration count = %d, want 1", migrationCount)
	}

	var eventCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_events WHERE source = 'oauth_identity_dedupe_v1' AND account_id IN ($1, $2, $3)`, oldID, midID, bridgeOldID).Scan(&eventCount); err != nil {
		t.Fatalf("查询 account_events 返回错误: %v", err)
	}
	if eventCount != 3 {
		t.Fatalf("dedupe event count = %d, want 3", eventCount)
	}

	postMigrationDuplicateID, err := db.InsertAccountWithCredentials(ctx, "post-migration-duplicate", map[string]interface{}{
		"refresh_token": "rt-post-migration-duplicate",
		"email":         "user@example.com",
		"account_id":    "acc-dedupe",
	}, "")
	if err != nil {
		t.Fatalf("Insert post migration duplicate 返回错误: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close after post migration duplicate 返回错误: %v", err)
	}

	db, err = New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("second reopen New(sqlite) 返回错误: %v", err)
	}

	if _, err := db.GetAccountByID(ctx, postMigrationDuplicateID); err != nil {
		t.Fatalf("post migration duplicate should remain active because migration is one-shot: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("final Close 返回错误: %v", err)
	}
}

func TestSQLiteAPIKeyQuotaAndExpiration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	key := "sk-test-limited-1234567890"
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	id, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{
		Name:       "limited",
		Key:        key,
		QuotaLimit: 0.01,
		ExpiresAt:  sql.NullTime{Time: expiresAt, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertAPIKeyWithOptions 返回错误: %v", err)
	}

	row, err := db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		t.Fatalf("GetAPIKeyByValue 返回错误: %v", err)
	}
	if row.ID != id || row.QuotaLimit != 0.01 || !row.ExpiresAt.Valid {
		t.Fatalf("API key row = %#v, want quota and expiration", row)
	}
	if !row.ExpiresAt.Time.Equal(expiresAt) {
		t.Fatalf("ExpiresAt = %s, want %s", row.ExpiresAt.Time, expiresAt)
	}

	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		APIKeyID:     id,
		Endpoint:     "/v1/responses",
		Model:        "gpt-5.4",
		StatusCode:   200,
		InputTokens:  1000,
		OutputTokens: 0,
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	row, err = db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		t.Fatalf("GetAPIKeyByValue after usage 返回错误: %v", err)
	}
	if row.QuotaUsed != 0.0025 {
		t.Fatalf("QuotaUsed = %.12f, want %.12f", row.QuotaUsed, 0.0025)
	}
}

func TestSQLiteUpdateAPIKeyPatchesSelectedFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	key := "sk-test-patch-1234567890"
	expiresAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	id, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{
		Name:            "patch",
		Key:             key,
		QuotaLimit:      1,
		ExpiresAt:       sql.NullTime{Time: expiresAt, Valid: true},
		AllowedGroupIDs: []int64{1, 2},
	})
	if err != nil {
		t.Fatalf("InsertAPIKeyWithOptions 返回错误: %v", err)
	}

	if err := db.UpdateAPIKey(ctx, id, APIKeyUpdate{Name: "patched", NameSet: true}); err != nil {
		t.Fatalf("UpdateAPIKey name 返回错误: %v", err)
	}
	row, err := db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		t.Fatalf("GetAPIKeyByValue 返回错误: %v", err)
	}
	if row.Name != "patched" || row.QuotaLimit != 1 || !row.ExpiresAt.Valid || len(row.AllowedGroupIDs) != 2 {
		t.Fatalf("row = %#v, want only name patched", row)
	}

	if err := db.UpdateAPIKey(ctx, id, APIKeyUpdate{
		QuotaLimitSet:      true,
		QuotaLimit:         0,
		ExpiresAtSet:       true,
		ExpiresAt:          sql.NullTime{},
		AllowedGroupIDsSet: true,
		AllowedGroupIDs:    []int64{3},
	}); err != nil {
		t.Fatalf("UpdateAPIKey limits 返回错误: %v", err)
	}
	row, err = db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		t.Fatalf("GetAPIKeyByValue after patch 返回错误: %v", err)
	}
	if row.Name != "patched" || row.QuotaLimit != 0 || row.ExpiresAt.Valid || len(row.AllowedGroupIDs) != 1 || row.AllowedGroupIDs[0] != 3 {
		t.Fatalf("row = %#v, want limits/groups patched", row)
	}
}

func TestSQLiteMigratesLegacyAPIKeysColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy sqlite: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE api_keys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		key TEXT UNIQUE NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create legacy api_keys: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO api_keys (name, key) VALUES ('legacy', 'sk-legacy-1234567890')`); err != nil {
		t.Fatalf("insert legacy api key: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy sqlite: %v", err)
	}

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite legacy) 返回错误: %v", err)
	}
	defer db.Close()

	row, err := db.GetAPIKeyByValue(context.Background(), "sk-legacy-1234567890")
	if err != nil {
		t.Fatalf("GetAPIKeyByValue legacy 返回错误: %v", err)
	}
	if row.Name != "legacy" || row.QuotaLimit != 0 || row.QuotaUsed != 0 || row.ExpiresAt.Valid || len(row.AllowedGroupIDs) != 0 {
		t.Fatalf("legacy row = %#v, want migrated defaults", row)
	}
}

func TestSQLiteAccountsEnabledDefaultsAndCanToggle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "test", "rt", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}

	rows, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListActive 返回 %d 条，want 1", len(rows))
	}
	if !rows[0].Enabled {
		t.Fatal("new account Enabled = false, want true")
	}

	if err := db.SetAccountEnabled(ctx, id, false); err != nil {
		t.Fatalf("SetAccountEnabled 返回错误: %v", err)
	}
	rows, err = db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListActive 返回 %d 条，want 1", len(rows))
	}
	if rows[0].Enabled {
		t.Fatal("disabled account Enabled = true, want false")
	}

	if err := db.SetAccountEnabled(ctx, id+1, false); err != sql.ErrNoRows {
		t.Fatalf("SetAccountEnabled missing account error = %v, want sql.ErrNoRows", err)
	}
}

func TestSQLiteUsageLogsHasAPIKeyColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	columns, err := db.sqliteTableColumns(ctx, "usage_logs")
	if err != nil {
		t.Fatalf("sqliteTableColumns 返回错误: %v", err)
	}

	for _, name := range []string{"api_key_id", "api_key_name", "api_key_masked", "client_ip", "client_user_agent", "upstream_user_agent", "user_agent_overridden", "image_count", "image_width", "image_height", "image_bytes", "image_format", "image_size", "effective_model", "compact", "account_billed", "user_billed", "is_retry_attempt", "attempt_index", "upstream_error_kind", "error_message"} {
		if _, ok := columns[name]; !ok {
			t.Fatalf("usage_logs 缺少列 %q", name)
		}
	}

	rows, err := db.conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'usage_logs'`)
	if err != nil {
		t.Fatalf("查询 usage_logs 索引返回错误: %v", err)
	}
	defer rows.Close()

	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("读取 usage_logs 索引返回错误: %v", err)
		}
		indexes[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 usage_logs 索引返回错误: %v", err)
	}
	if !indexes["idx_usage_logs_account_created_at"] {
		t.Fatal("usage_logs 缺少索引 idx_usage_logs_account_created_at")
	}
}

func TestUsageLogModeErrorsSkipsSuccessfulLogsButChargesQuota(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()
	db.SetUsageLogConfig(UsageLogModeErrors, 10, 5)

	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "mode-errors", "sk-mode-errors-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:   1,
		APIKeyID:    apiKeyID,
		Endpoint:    "/v1/responses",
		Model:       "gpt-5.4",
		StatusCode:  200,
		InputTokens: 1000,
	}); err != nil {
		t.Fatalf("InsertUsageLog success 返回错误: %v", err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:    1,
		Endpoint:     "/v1/responses",
		Model:        "gpt-5.4",
		StatusCode:   500,
		ErrorMessage: "upstream failed",
	}); err != nil {
		t.Fatalf("InsertUsageLog error 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	if logs[0].StatusCode != 500 {
		t.Fatalf("StatusCode = %d, want 500", logs[0].StatusCode)
	}

	want := calculateCost(1000, 0, 0, "gpt-5.4", "")
	var quotaUsed, totalUsed float64
	if err := db.conn.QueryRowContext(ctx, `SELECT quota_used, total_used FROM api_keys WHERE id = $1`, apiKeyID).Scan(&quotaUsed, &totalUsed); err != nil {
		t.Fatalf("查询 API Key 用量返回错误: %v", err)
	}
	if math.Abs(quotaUsed-want) > 1e-12 || math.Abs(totalUsed-want) > 1e-12 {
		t.Fatalf("API Key usage = quota %.12f total %.12f, want %.12f", quotaUsed, totalUsed, want)
	}
}

func TestUsageErrorSummaryAndFilters(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for _, usageLog := range []*UsageLogInput{
		{
			AccountID:         1,
			Endpoint:          "/v1/responses",
			InboundEndpoint:   "/v1/responses",
			UpstreamEndpoint:  "/backend-api/codex/responses",
			Model:             "gpt-5.4",
			StatusCode:        500,
			DurationMs:        1200,
			IsRetryAttempt:    true,
			AttemptIndex:      1,
			UpstreamErrorKind: "upstream_timeout",
			ErrorMessage:      "upstream timeout",
		},
		{
			AccountID:         2,
			Endpoint:          "/v1/messages",
			InboundEndpoint:   "/v1/messages",
			Model:             "claude-sonnet-4.5",
			StatusCode:        401,
			DurationMs:        80,
			UpstreamErrorKind: "unauthorized",
			ErrorMessage:      "invalid access token",
		},
		{
			AccountID:    3,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.4",
			StatusCode:   499,
			DurationMs:   30,
			ErrorMessage: "client canceled",
		},
		{
			AccountID:  4,
			Endpoint:   "/v1/responses",
			Model:      "gpt-5.4",
			StatusCode: 200,
			DurationMs: 90,
		},
	} {
		if err := db.InsertUsageLog(ctx, usageLog); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	db.flushLogs()

	now := time.Now()
	filter := UsageLogFilter{
		Start:           now.Add(-1 * time.Hour),
		End:             now.Add(1 * time.Hour),
		Page:            1,
		PageSize:        10,
		ErrorOnly:       true,
		IncludeCanceled: true,
	}
	page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged 返回错误: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("page.Total = %d, want 3", page.Total)
	}

	foundRetry := false
	for _, usageLog := range page.Logs {
		if usageLog.UpstreamErrorKind == "upstream_timeout" {
			foundRetry = true
			if !usageLog.IsRetryAttempt {
				t.Fatal("IsRetryAttempt = false, want true")
			}
			if usageLog.AttemptIndex != 1 {
				t.Fatalf("AttemptIndex = %d, want 1", usageLog.AttemptIndex)
			}
		}
	}
	if !foundRetry {
		t.Fatal("未找到 upstream_timeout 错误日志")
	}

	summary, err := db.GetUsageErrorSummary(ctx, filter)
	if err != nil {
		t.Fatalf("GetUsageErrorSummary 返回错误: %v", err)
	}
	if summary.TotalErrors != 3 {
		t.Fatalf("TotalErrors = %d, want 3", summary.TotalErrors)
	}
	if summary.Status5xx != 1 || summary.Unauthorized != 1 || summary.Canceled != 1 || summary.Timeouts != 1 || summary.RetryAttempts != 1 {
		t.Fatalf("summary = %+v, want one 5xx/401/499/timeout/retry", summary)
	}

	charts, err := db.GetChartAggregation(ctx, filter.Start, filter.End, 5)
	if err != nil {
		t.Fatalf("GetChartAggregation 返回错误: %v", err)
	}
	var chart4xx, chart5xx int64
	for _, point := range charts.Timeline {
		chart4xx += point.Errors4xx
		chart5xx += point.Errors5xx
	}
	if chart4xx != 1 || chart5xx != 1 {
		t.Fatalf("chart errors = 4xx:%d 5xx:%d, want 1/1", chart4xx, chart5xx)
	}

	filter.StatusFamily = "5xx"
	page, err = db.ListUsageLogsByTimeRangePaged(ctx, filter)
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged status family 返回错误: %v", err)
	}
	if page.Total != 1 || len(page.Logs) != 1 || page.Logs[0].StatusCode != 500 {
		t.Fatalf("5xx page = total %d len %d first %+v", page.Total, len(page.Logs), page.Logs)
	}
}

func TestUsageLogModeOffSkipsAllLogsButChargesQuota(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()
	db.SetUsageLogConfig(UsageLogModeOff, 10, 5)

	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "mode-off", "sk-mode-off-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:   1,
		APIKeyID:    apiKeyID,
		Endpoint:    "/v1/responses",
		Model:       "gpt-5.4",
		StatusCode:  200,
		InputTokens: 1000,
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("len(logs) = %d, want 0", len(logs))
	}

	want := calculateCost(1000, 0, 0, "gpt-5.4", "")
	var quotaUsed, totalUsed float64
	if err := db.conn.QueryRowContext(ctx, `SELECT quota_used, total_used FROM api_keys WHERE id = $1`, apiKeyID).Scan(&quotaUsed, &totalUsed); err != nil {
		t.Fatalf("查询 API Key 用量返回错误: %v", err)
	}
	if math.Abs(quotaUsed-want) > 1e-12 || math.Abs(totalUsed-want) > 1e-12 {
		t.Fatalf("API Key usage = quota %.12f total %.12f, want %.12f", quotaUsed, totalUsed, want)
	}
}

func TestUsageLogModesPreserveCanceledQuotaExemption(t *testing.T) {
	for _, mode := range []string{UsageLogModeErrors, UsageLogModeOff} {
		t.Run(mode, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "codex2api.db")
			db, err := New("sqlite", dbPath)
			if err != nil {
				t.Fatalf("New(sqlite) 返回错误: %v", err)
			}
			defer db.Close()
			db.SetUsageLogConfig(mode, 10, 5)

			ctx := context.Background()
			apiKeyID, err := db.InsertAPIKey(ctx, "mode-499-"+mode, "sk-mode-499-"+mode+"-1234567890")
			if err != nil {
				t.Fatalf("InsertAPIKey 返回错误: %v", err)
			}
			if err := db.InsertUsageLog(ctx, &UsageLogInput{
				APIKeyID:    apiKeyID,
				Endpoint:    "/v1/responses",
				Model:       "gpt-5.4",
				StatusCode:  499,
				InputTokens: 1000,
			}); err != nil {
				t.Fatalf("InsertUsageLog 返回错误: %v", err)
			}
			db.flushLogs()

			var quotaUsed, totalUsed float64
			if err := db.conn.QueryRowContext(ctx, `SELECT quota_used, total_used FROM api_keys WHERE id = $1`, apiKeyID).Scan(&quotaUsed, &totalUsed); err != nil {
				t.Fatalf("查询 API Key 用量返回错误: %v", err)
			}
			if quotaUsed != 0 || totalUsed != 0 {
				t.Fatalf("API Key usage = quota %.12f total %.12f, want 0", quotaUsed, totalUsed)
			}
		})
	}
}

func TestSQLiteModelCooldownPersistence(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	resetAt := time.Now().Add(15 * time.Minute).Truncate(time.Second)
	if err := db.SetModelCooldown(ctx, 42, "gpt-5.4", "model_capacity", resetAt); err != nil {
		t.Fatalf("SetModelCooldown 返回错误: %v", err)
	}

	rows, err := db.ListActiveModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("ListActiveModelCooldowns 返回错误: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListActiveModelCooldowns 返回 %d 条，want 1", len(rows))
	}
	if rows[0].AccountID != 42 || rows[0].Model != "gpt-5.4" || rows[0].Reason != "model_capacity" {
		t.Fatalf("cooldown row = %#v", rows[0])
	}

	if err := db.ClearModelCooldown(ctx, 42, "gpt-5.4"); err != nil {
		t.Fatalf("ClearModelCooldown 返回错误: %v", err)
	}
	rows, err = db.ListActiveModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("ListActiveModelCooldowns 返回错误: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("ListActiveModelCooldowns 返回 %d 条，want 0", len(rows))
	}
}

func TestAccountRequestCountsSeparateRetryAttempts(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	logs := []*UsageLogInput{
		{AccountID: 7, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 200},
		{AccountID: 7, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 429, IsRetryAttempt: true, AttemptIndex: 1, UpstreamErrorKind: "model_capacity"},
		{AccountID: 7, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 500, IsRetryAttempt: false, AttemptIndex: 2, UpstreamErrorKind: "server"},
	}
	for _, usageLog := range logs {
		if err := db.InsertUsageLog(ctx, usageLog); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	db.flushLogs()

	counts, err := db.GetAccountRequestCounts(ctx)
	if err != nil {
		t.Fatalf("GetAccountRequestCounts 返回错误: %v", err)
	}
	got := counts[7]
	if got == nil {
		t.Fatal("account 7 counts missing")
	}
	if got.SuccessCount != 1 || got.ErrorCount != 1 || got.RetryErrorCount != 1 || got.RateLimitAttemptCount != 1 {
		t.Fatalf("counts = %#v, want success=1 error=1 retry=1 rateLimit=1", got)
	}
}

func TestSQLiteUsageStatsBaselineHasBillingColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	columns, err := db.sqliteTableColumns(context.Background(), "usage_stats_baseline")
	if err != nil {
		t.Fatalf("sqliteTableColumns 返回错误: %v", err)
	}

	for _, name := range []string{"account_billed", "user_billed", "cache_hit_requests", "first_token_ms_sum", "first_token_samples"} {
		if _, ok := columns[name]; !ok {
			t.Fatalf("usage_stats_baseline 缺少列 %q", name)
		}
	}
}

func TestSQLiteSystemSettingsPersistsFirstTokenTimeoutSeconds(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.UpdateSystemSettings(ctx, &SystemSettings{
		SiteName:                          "CodexProxy",
		MaxConcurrency:                    2,
		GlobalRPM:                         0,
		TestModel:                         "gpt-5.4",
		TestContent:                       "say pong",
		TestConcurrency:                   50,
		BackgroundRefreshIntervalMinutes:  2,
		UsageProbeMaxAgeMinutes:           10,
		UsageProbeConcurrency:             16,
		RecoveryProbeIntervalMinutes:      30,
		PgMaxConns:                        50,
		RedisPoolSize:                     30,
		MaxRetries:                        2,
		MaxRateLimitRetries:               1,
		ModelMapping:                      "{}",
		CodexModelMapping:                 `{"gpt-5.2":"gpt-5.5"}`,
		ReasoningEffortModels:             `[{"model":"gpt-5.5","effort":"xhigh"}]`,
		PromptFilterMode:                  "monitor",
		PromptFilterThreshold:             50,
		PromptFilterStrictThreshold:       90,
		PromptFilterStrictTerminalEnabled: true,
		PromptFilterAdvancedConfig:        `{"normalization":{"enabled":true}}`,
		PromptFilterLogMatches:            true,
		PromptFilterMaxTextLength:         81920,
		PromptFilterCustomPatterns:        "[]",
		PromptFilterDisabledPatterns:      "[]",
		PromptFilterReviewEnabled:         true,
		PromptFilterReviewAPIKey:          "sk-review-test",
		PromptFilterReviewBaseURL:         "https://review.example.com",
		PromptFilterReviewModel:           "review-model",
		PromptFilterReviewTimeoutSeconds:  7,
		PromptFilterReviewFailClosed:      false,
		ClientCompatMode:                  "preserve",
		CodexMinCLIVersion:                "0.118.0",
		CodexUserAgentConfig:              `{"terminal":"xterm-256color","os_name":"Linux","os_version":"Unknown"}`,
		UsageLogMode:                      "full",
		UsageLogBatchSize:                 200,
		UsageLogFlushIntervalSeconds:      5,
		StreamFlushPolicy:                 "immediate",
		StreamFlushIntervalMS:             20,
		FirstTokenMode:                    "loose",
		FirstTokenTimeoutSeconds:          17,
		BillingTierPolicy:                 "requested",
		ImageStorageConfig:                "{}",
		SchedulerMode:                     "round_robin",
		AffinityMode:                      "bounded",
		BackgroundConfig:                  "{}",
		ShowFullUsageNumbers:              true,
		PublicKeyUsagePageEnabled:         true,
		PublicImageStudioPageEnabled:      true,
		CodexWSHideUpstreamErrors:         true,
		CodexWSSilentRetryEnabled:         true,
		CodexWSSilentMaxRetries:           4,
		IgnoreUsageLimitStatus:            true,
		AutoResetCreditsEnabled:           true,
		AutoResetCreditsBeforeExpiryMin:   75,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings 返回错误: %v", err)
	}

	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings 返回错误: %v", err)
	}
	if settings == nil {
		t.Fatal("GetSystemSettings 返回 nil")
	}
	if settings.FirstTokenTimeoutSeconds != 17 {
		t.Fatalf("FirstTokenTimeoutSeconds = %d, want 17", settings.FirstTokenTimeoutSeconds)
	}
	if !settings.PromptFilterStrictTerminalEnabled {
		t.Fatal("PromptFilterStrictTerminalEnabled = false, want true")
	}
	if settings.PromptFilterAdvancedConfig != `{"normalization":{"enabled":true}}` {
		t.Fatalf("PromptFilterAdvancedConfig = %q", settings.PromptFilterAdvancedConfig)
	}
	if !settings.IgnoreUsageLimitStatus {
		t.Fatal("IgnoreUsageLimitStatus = false, want true")
	}
	if !settings.AutoResetCreditsEnabled {
		t.Fatal("AutoResetCreditsEnabled = false, want true")
	}
	if settings.AutoResetCreditsBeforeExpiryMin != 75 {
		t.Fatalf("AutoResetCreditsBeforeExpiryMin = %d, want 75", settings.AutoResetCreditsBeforeExpiryMin)
	}
	if settings.TestContent != "say pong" {
		t.Fatalf("TestContent = %q, want say pong", settings.TestContent)
	}
	if settings.FirstTokenMode != "loose" {
		t.Fatalf("FirstTokenMode = %q, want loose", settings.FirstTokenMode)
	}
	if !settings.ShowFullUsageNumbers {
		t.Fatal("ShowFullUsageNumbers = false, want true")
	}
	if !settings.PublicKeyUsagePageEnabled {
		t.Fatal("PublicKeyUsagePageEnabled = false, want true")
	}
	if !settings.PublicImageStudioPageEnabled {
		t.Fatal("PublicImageStudioPageEnabled = false, want true")
	}
	if settings.BillingTierPolicy != "requested" {
		t.Fatalf("BillingTierPolicy = %q, want requested", settings.BillingTierPolicy)
	}
	if settings.CodexModelMapping != `{"gpt-5.2":"gpt-5.5"}` {
		t.Fatalf("CodexModelMapping = %q, want gpt-5.2 mapping", settings.CodexModelMapping)
	}
	if settings.CodexUserAgentConfig != `{"terminal":"xterm-256color","os_name":"Linux","os_version":"Unknown"}` {
		t.Fatalf("CodexUserAgentConfig = %q, want custom UA config", settings.CodexUserAgentConfig)
	}
	if settings.ReasoningEffortModels != `[{"model":"gpt-5.5","effort":"xhigh"}]` {
		t.Fatalf("ReasoningEffortModels = %q, want gpt-5.5 xhigh entry", settings.ReasoningEffortModels)
	}
	if !settings.PromptFilterReviewEnabled {
		t.Fatal("PromptFilterReviewEnabled = false, want true")
	}
	if settings.PromptFilterReviewAPIKey != "sk-review-test" {
		t.Fatalf("PromptFilterReviewAPIKey = %q, want sk-review-test", settings.PromptFilterReviewAPIKey)
	}
	if settings.PromptFilterReviewBaseURL != "https://review.example.com" {
		t.Fatalf("PromptFilterReviewBaseURL = %q, want https://review.example.com", settings.PromptFilterReviewBaseURL)
	}
	if settings.PromptFilterReviewModel != "review-model" {
		t.Fatalf("PromptFilterReviewModel = %q, want review-model", settings.PromptFilterReviewModel)
	}
	if settings.PromptFilterReviewTimeoutSeconds != 7 {
		t.Fatalf("PromptFilterReviewTimeoutSeconds = %d, want 7", settings.PromptFilterReviewTimeoutSeconds)
	}
	if settings.PromptFilterReviewFailClosed {
		t.Fatal("PromptFilterReviewFailClosed = true, want false")
	}
	if !settings.CodexWSHideUpstreamErrors {
		t.Fatal("CodexWSHideUpstreamErrors = false, want true")
	}
	if !settings.CodexWSSilentRetryEnabled {
		t.Fatal("CodexWSSilentRetryEnabled = false, want true")
	}
	if settings.CodexWSSilentMaxRetries != 4 {
		t.Fatalf("CodexWSSilentMaxRetries = %d, want 4", settings.CodexWSSilentMaxRetries)
	}

	settings.PublicKeyUsagePageEnabled = false
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings false PublicKeyUsagePageEnabled 返回错误: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings after false PublicKeyUsagePageEnabled 返回错误: %v", err)
	}
	if settings.PublicKeyUsagePageEnabled {
		t.Fatal("PublicKeyUsagePageEnabled = true, want false")
	}

	settings.PublicImageStudioPageEnabled = false
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings false PublicImageStudioPageEnabled 返回错误: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings after false PublicImageStudioPageEnabled 返回错误: %v", err)
	}
	if settings.PublicImageStudioPageEnabled {
		t.Fatal("PublicImageStudioPageEnabled = true, want false")
	}
}

func TestSystemSettingsNormalizeBlankBillingTierPolicy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings (id, billing_tier_policy) VALUES (1, '')`); err != nil {
		t.Fatalf("插入空 billing_tier_policy 失败: %v", err)
	}

	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings 返回错误: %v", err)
	}
	if settings == nil {
		t.Fatal("GetSystemSettings 返回 nil")
	}
	if settings.BillingTierPolicy != "actual" {
		t.Fatalf("BillingTierPolicy = %q, want actual", settings.BillingTierPolicy)
	}

	settings.BillingTierPolicy = ""
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings 返回错误: %v", err)
	}
	var stored string
	if err := db.conn.QueryRowContext(ctx, `SELECT billing_tier_policy FROM system_settings WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatalf("读取 billing_tier_policy 返回错误: %v", err)
	}
	if stored != "actual" {
		t.Fatalf("stored billing_tier_policy = %q, want actual", stored)
	}
}

func TestSQLitePartialBackgroundSettingsUpdatesPreserveAutoResetCredits(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	settings := &SystemSettings{
		AutoResetCreditsEnabled:         true,
		AutoResetCreditsBeforeExpiryMin: 90,
		ModelPricingOverrides:           `{"old":{"input":1}}`,
		ModelPricingSyncURL:             "https://old.example/pricing.json",
	}
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if err := db.UpdateCodexSyncedCLIVersion(ctx, "9.9.9"); err != nil {
		t.Fatalf("UpdateCodexSyncedCLIVersion: %v", err)
	}
	if err := db.UpdateModelPricingSettings(ctx, `{"new":{"input":2}}`, "https://new.example/pricing.json"); err != nil {
		t.Fatalf("UpdateModelPricingSettings: %v", err)
	}

	got, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if got == nil {
		t.Fatal("GetSystemSettings returned nil")
	}
	// 模拟管理员从旧快照保存无关设置；后台刚写入的模型定价不能被回滚。
	staleFullUpdate := *got
	staleFullUpdate.SiteName = "Concurrent Admin Save"
	staleFullUpdate.CodexSyncedCLIVersion = "0.0.1"
	staleFullUpdate.ModelPricingOverrides = `{"old":{"input":1}}`
	staleFullUpdate.ModelPricingSyncURL = "https://old.example/pricing.json"
	if err := db.UpdateSystemSettings(ctx, &staleFullUpdate); err != nil {
		t.Fatalf("UpdateSystemSettings(stale full update): %v", err)
	}
	got, err = db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings after stale full update: %v", err)
	}
	if got == nil {
		t.Fatal("GetSystemSettings after stale full update returned nil")
	}
	if !got.AutoResetCreditsEnabled || got.AutoResetCreditsBeforeExpiryMin != 90 {
		t.Fatalf("auto reset settings = (%v,%d), want (true,90)", got.AutoResetCreditsEnabled, got.AutoResetCreditsBeforeExpiryMin)
	}
	if got.CodexSyncedCLIVersion != "9.9.9" {
		t.Fatalf("CodexSyncedCLIVersion = %q, want 9.9.9", got.CodexSyncedCLIVersion)
	}
	if got.ModelPricingOverrides != `{"new":{"input":2}}` || got.ModelPricingSyncURL != "https://new.example/pricing.json" {
		t.Fatalf("model pricing = %q / %q", got.ModelPricingOverrides, got.ModelPricingSyncURL)
	}
}

func TestSQLiteMigratesAccountGroupBaseConcurrencyOverride(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open legacy sqlite 返回错误: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE account_groups (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL)`); err != nil {
		legacy.Close()
		t.Fatalf("创建旧 account_groups 表返回错误: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("关闭旧 SQLite 返回错误: %v", err)
	}

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 迁移旧库返回错误: %v", err)
	}
	defer db.Close()

	var columnCount int
	if err := db.conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pragma_table_info('account_groups') WHERE name = 'base_concurrency_override'`).Scan(&columnCount); err != nil {
		t.Fatalf("查询迁移列返回错误: %v", err)
	}
	if columnCount != 1 {
		t.Fatalf("base_concurrency_override column count = %d, want 1", columnCount)
	}
}

func TestAccountGroupBaseConcurrencyOverrideCRUD(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	groupID, err := db.CreateAccountGroup(ctx, "Concurrency", "", "", 0, 0, sql.NullInt64{Int64: 6, Valid: true})
	if err != nil {
		t.Fatalf("CreateAccountGroup 返回错误: %v", err)
	}

	assertOverride := func(want sql.NullInt64) {
		t.Helper()
		groups, err := db.ListAccountGroups(ctx)
		if err != nil {
			t.Fatalf("ListAccountGroups 返回错误: %v", err)
		}
		if len(groups) != 1 || groups[0].ID != groupID {
			t.Fatalf("groups = %#v, want group %d", groups, groupID)
		}
		got := groups[0].BaseConcurrencyOverride
		if got.Valid != want.Valid || (got.Valid && got.Int64 != want.Int64) {
			t.Fatalf("BaseConcurrencyOverride = %+v, want %+v", got, want)
		}
	}

	assertOverride(sql.NullInt64{Int64: 6, Valid: true})
	if err := db.UpdateAccountGroup(ctx, groupID, nil, nil, nil, &UpdateAccountGroupOpts{
		BaseConcurrencyOverride: OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: 3, Valid: true}},
	}); err != nil {
		t.Fatalf("UpdateAccountGroup set override 返回错误: %v", err)
	}
	assertOverride(sql.NullInt64{Int64: 3, Valid: true})

	name := "Concurrency renamed"
	if err := db.UpdateAccountGroup(ctx, groupID, &name, nil, nil, nil); err != nil {
		t.Fatalf("UpdateAccountGroup unrelated field 返回错误: %v", err)
	}
	assertOverride(sql.NullInt64{Int64: 3, Valid: true})

	if err := db.UpdateAccountGroup(ctx, groupID, nil, nil, nil, &UpdateAccountGroupOpts{
		BaseConcurrencyOverride: OptionalNullInt64{Set: true},
	}); err != nil {
		t.Fatalf("UpdateAccountGroup clear override 返回错误: %v", err)
	}
	assertOverride(sql.NullInt64{})
}

func TestDeleteAccountGroupDoesNotBroadenScopedAPIKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	groupA, err := db.CreateAccountGroup(ctx, "Group A", "", "#2563eb", 0, 0, sql.NullInt64{}, 0)
	if err != nil {
		t.Fatalf("CreateAccountGroup A 返回错误: %v", err)
	}
	groupB, err := db.CreateAccountGroup(ctx, "Group B", "", "#16a34a", 0, 0, sql.NullInt64{}, 1)
	if err != nil {
		t.Fatalf("CreateAccountGroup B 返回错误: %v", err)
	}

	keyOnlyA, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{
		Name:            "Only A",
		Key:             "sk-only-a-1234567890",
		AllowedGroupIDs: []int64{groupA},
	})
	if err != nil {
		t.Fatalf("InsertAPIKeyWithOptions only-a 返回错误: %v", err)
	}
	keyAB, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{
		Name:            "A and B",
		Key:             "sk-a-b-1234567890",
		AllowedGroupIDs: []int64{groupA, groupB},
	})
	if err != nil {
		t.Fatalf("InsertAPIKeyWithOptions a-b 返回错误: %v", err)
	}

	if err := db.DeleteAccountGroup(ctx, groupA, true); err != nil {
		t.Fatalf("DeleteAccountGroup 返回错误: %v", err)
	}

	rows, err := db.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("ListAPIKeys 返回错误: %v", err)
	}

	got := make(map[int64][]int64)
	for _, row := range rows {
		got[row.ID] = row.AllowedGroupIDs
	}

	if actual := got[keyOnlyA]; len(actual) != 1 || actual[0] != groupA {
		t.Fatalf("keyOnlyA allowed groups = %v, want stale [%d] to preserve deny-all semantics", actual, groupA)
	}
	if actual := got[keyAB]; len(actual) != 1 || actual[0] != groupB {
		t.Fatalf("keyAB allowed groups = %v, want [%d]", actual, groupB)
	}
}

func TestUsageLogsPersistEffectiveModel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:        1,
		Endpoint:         "/v1/messages",
		InboundEndpoint:  "/v1/messages",
		UpstreamEndpoint: "/v1/responses",
		Model:            "claude-haiku-4-5-20251001",
		EffectiveModel:   "gpt-5.4",
		StatusCode:       200,
		ReasoningEffort:  "high",
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	if logs[0].Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("Model = %q, want claude-haiku-4-5-20251001", logs[0].Model)
	}
	if logs[0].EffectiveModel != "gpt-5.4" {
		t.Fatalf("EffectiveModel = %q, want gpt-5.4", logs[0].EffectiveModel)
	}
	if logs[0].ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", logs[0].ReasoningEffort)
	}
}

func TestUsageLogsPersistUserAgentAudit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:           1,
		Endpoint:            "/v1/responses",
		Model:               "gpt-5.4",
		StatusCode:          200,
		ClientUserAgent:     "curl/8.7.1",
		UpstreamUserAgent:   "codex-tui/0.151.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.151.0)",
		UserAgentOverridden: true,
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	if got := logs[0].ClientUserAgent; got != "curl/8.7.1" {
		t.Fatalf("ClientUserAgent = %q, want curl/8.7.1", got)
	}
	if got := logs[0].UpstreamUserAgent; !strings.HasPrefix(got, "codex-tui/0.151.0 ") {
		t.Fatalf("UpstreamUserAgent = %q, want generated codex-tui UA", got)
	}
	if !logs[0].UserAgentOverridden {
		t.Fatal("UserAgentOverridden = false, want true")
	}
}

func TestUsageLogsPersistImageMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:        1,
		Endpoint:         "/v1/images/generations",
		InboundEndpoint:  "/v1/images/generations",
		UpstreamEndpoint: "/v1/responses",
		Model:            "gpt-image-2-4k",
		StatusCode:       200,
		DurationMs:       1200,
		ImageCount:       1,
		ImageWidth:       3840,
		ImageHeight:      2160,
		ImageBytes:       2457600,
		ImageFormat:      "png",
		ImageSize:        "3840x2160",
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	got := logs[0]
	if got.ImageCount != 1 || got.ImageWidth != 3840 || got.ImageHeight != 2160 || got.ImageBytes != 2457600 || got.ImageFormat != "png" || got.ImageSize != "3840x2160" {
		t.Fatalf("image metadata = %#v", got)
	}
}

func TestUsageLogsReturnBillingFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:          1,
		Endpoint:           "/v1/responses",
		InboundEndpoint:    "/v1/responses",
		UpstreamEndpoint:   "/v1/responses",
		Model:              "gpt-5.5",
		StatusCode:         200,
		InputTokens:        476,
		OutputTokens:       252,
		TotalTokens:        728,
		ServiceTier:        "default",
		ActualServiceTier:  "default",
		BillingServiceTier: "default",
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}

	got := logs[0]
	want := calculateCost(476, 252, 0, "gpt-5.5", "default")
	if got.AccountBilled != want || got.UserBilled != want {
		t.Fatalf("billing = account %.12f user %.12f, want %.12f", got.AccountBilled, got.UserBilled, want)
	}
	if got.InputCost <= 0 || got.OutputCost <= 0 || got.TotalCost != want {
		t.Fatalf("billing breakdown = input %.12f output %.12f total %.12f, want total %.12f", got.InputCost, got.OutputCost, got.TotalCost, want)
	}
	if got.ActualServiceTier != "default" || got.BillingServiceTier != "default" {
		t.Fatalf("tiers actual=%q billing=%q, want default/default", got.ActualServiceTier, got.BillingServiceTier)
	}
}

func TestUsageLogsBillFastByActualServiceTier(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:            1,
		Endpoint:             "/v1/responses",
		Model:                "gpt-5.4",
		StatusCode:           200,
		InputTokens:          1000,
		OutputTokens:         500,
		CachedTokens:         200,
		ServiceTier:          "fast",
		RequestedServiceTier: "priority",
		ActualServiceTier:    "default",
		BillingServiceTier:   "default",
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:            1,
		Endpoint:             "/v1/responses",
		Model:                "gpt-5.4",
		StatusCode:           200,
		InputTokens:          1000,
		OutputTokens:         500,
		CachedTokens:         200,
		ServiceTier:          "fast",
		RequestedServiceTier: "priority",
		ActualServiceTier:    "priority",
		BillingServiceTier:   "priority",
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("len(logs) = %d, want 2", len(logs))
	}

	wantPriority := calculateCost(1000, 500, 200, "gpt-5.4", "priority")
	wantDefault := calculateCost(1000, 500, 200, "gpt-5.4", "default")
	seenPriority := false
	seenDefault := false
	for _, log := range logs {
		if log.ServiceTier != "fast" {
			t.Fatalf("log tier = %q, want fast", log.ServiceTier)
		}
		switch log.AccountBilled {
		case wantPriority:
			seenPriority = true
		case wantDefault:
			seenDefault = true
		default:
			t.Fatalf("unexpected billed amount %.12f, want %.12f or %.12f", log.AccountBilled, wantPriority, wantDefault)
		}
	}
	if !seenPriority || !seenDefault {
		t.Fatalf("billing tiers seen priority=%v default=%v, want both", seenPriority, seenDefault)
	}
}

func TestUsageLogsReturnErrorMessage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:    1,
		Endpoint:     "/v1/responses",
		Model:        "gpt-5.4",
		StatusCode:   429,
		ErrorMessage: "rate_limit_exceeded · Too many requests",
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	if got := logs[0].ErrorMessage; got != "rate_limit_exceeded · Too many requests" {
		t.Fatalf("ErrorMessage = %q", got)
	}
}

func TestUsageStatsIncludeBillingTotals(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for _, usageLog := range []*UsageLogInput{
		{
			AccountID:    1,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.5",
			StatusCode:   200,
			InputTokens:  1000,
			OutputTokens: 500,
			TotalTokens:  1500,
		},
		{
			AccountID:    1,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.5",
			StatusCode:   499,
			InputTokens:  1000,
			OutputTokens: 500,
			TotalTokens:  1500,
		},
	} {
		if err := db.InsertUsageLog(ctx, usageLog); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	db.flushLogs()

	stats, err := db.GetUsageStats(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetUsageStats 返回错误: %v", err)
	}

	want := calculateCost(1000, 500, 0, "gpt-5.5", "")
	if stats.TotalAccountBilled != want || stats.TotalUserBilled != want {
		t.Fatalf("total billing = account %.12f user %.12f, want %.12f", stats.TotalAccountBilled, stats.TotalUserBilled, want)
	}
	if stats.TodayAccountBilled != want || stats.TodayUserBilled != want {
		t.Fatalf("today billing = account %.12f user %.12f, want %.12f", stats.TodayAccountBilled, stats.TodayUserBilled, want)
	}
	if stats.AvgAccountBilled != want || stats.AvgUserBilled != want {
		t.Fatalf("avg billing = account %.12f user %.12f, want %.12f", stats.AvgAccountBilled, stats.AvgUserBilled, want)
	}
	if len(stats.ModelStats) != 1 {
		t.Fatalf("ModelStats len = %d, want 1: %+v", len(stats.ModelStats), stats.ModelStats)
	}
	modelStats := stats.ModelStats[0]
	if modelStats.Model != "gpt-5.5" || modelStats.Requests != 1 || modelStats.Tokens != 1500 {
		t.Fatalf("ModelStats[0] = %+v, want gpt-5.5 requests=1 tokens=1500", modelStats)
	}
	if modelStats.AccountBilled != want || modelStats.UserBilled != want {
		t.Fatalf("model billing = account %.12f user %.12f, want %.12f", modelStats.AccountBilled, modelStats.UserBilled, want)
	}
}

func TestUsageStatsIncludeCodex2APIBreakdowns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	logs := []*UsageLogInput{
		{
			AccountID:       1,
			Endpoint:        "/v1/responses",
			InboundEndpoint: "/v1/responses",
			Model:           "gpt-5.5",
			StatusCode:      200,
			InputTokens:     1000,
			OutputTokens:    500,
			TotalTokens:     1500,
			Stream:          true,
			ServiceTier:     "fast",
			CachedTokens:    128,
			FirstTokenMs:    820,
			ReasoningTokens: 32,
			APIKeyID:        7,
			APIKeyName:      "Claude Code",
			APIKeyMasked:    "sk-...1111",
		},
		{
			AccountID:       1,
			Endpoint:        "/v1/images/generations",
			InboundEndpoint: "/v1/images/generations",
			Model:           "gpt-image-2",
			StatusCode:      200,
			ImageCount:      1,
			APIKeyID:        7,
			APIKeyName:      "Claude Code",
			APIKeyMasked:    "sk-...1111",
		},
		{
			AccountID:      2,
			Endpoint:       "/v1/chat/completions",
			Model:          "gpt-5.4",
			StatusCode:     500,
			InputTokens:    100,
			OutputTokens:   20,
			TotalTokens:    120,
			APIKeyID:       8,
			APIKeyName:     "Cherry Studio",
			APIKeyMasked:   "sk-...2222",
			IsRetryAttempt: true,
			AttemptIndex:   1,
		},
		{
			AccountID:       3,
			Endpoint:        "/v1/responses",
			InboundEndpoint: "/v1/responses",
			Model:           "gpt-5.4",
			StatusCode:      499,
			Stream:          true,
			APIKeyID:        9,
			APIKeyName:      "Canceled",
		},
	}
	for _, usageLog := range logs {
		if err := db.InsertUsageLog(ctx, usageLog); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	db.flushLogs()

	stats, err := db.GetUsageStats(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetUsageStats 返回错误: %v", err)
	}
	if stats.TotalRequests != 3 {
		t.Fatalf("TotalRequests = %d, want 3", stats.TotalRequests)
	}
	if stats.TodayCachedTokens != 128 {
		t.Fatalf("TodayCachedTokens = %d, want 128", stats.TodayCachedTokens)
	}
	if stats.TodayCacheRate < 33.3 || stats.TodayCacheRate > 33.4 {
		t.Fatalf("TodayCacheRate = %.4f, want about 33.33", stats.TodayCacheRate)
	}
	if stats.TotalCacheRate < 33.3 || stats.TotalCacheRate > 33.4 {
		t.Fatalf("TotalCacheRate = %.4f, want about 33.33", stats.TotalCacheRate)
	}
	if stats.AvgFirstTokenMs != 820 {
		t.Fatalf("AvgFirstTokenMs = %.2f, want 820", stats.AvgFirstTokenMs)
	}
	features := stats.FeatureStats
	if features.StreamRequests != 1 || features.SyncRequests != 2 || features.FastRequests != 1 ||
		features.CacheHitRequests != 1 || features.ReasoningRequests != 1 || features.ImageRequests != 1 ||
		features.RetryRequests != 1 || features.ErrorRequests != 1 {
		t.Fatalf("FeatureStats = %+v, want stream/sync/fast/cache/reasoning/image/retry/error = 1/2/1/1/1/1/1/1", features)
	}

	endpoints := make(map[string]UsageEndpointStat)
	for _, item := range stats.EndpointStats {
		endpoints[item.Endpoint] = item
	}
	if endpoints["/v1/responses"].Requests != 1 || endpoints["/v1/images/generations"].Requests != 1 || endpoints["/v1/chat/completions"].ErrorCount != 1 {
		t.Fatalf("EndpointStats = %+v", stats.EndpointStats)
	}

	apiKeys := make(map[int64]UsageAPIKeyStat)
	for _, item := range stats.APIKeyStats {
		apiKeys[item.APIKeyID] = item
	}
	if apiKeys[7].Requests != 2 || apiKeys[7].Label != "Claude Code" {
		t.Fatalf("APIKeyStats[7] = %+v, want Claude Code requests=2", apiKeys[7])
	}
	if apiKeys[8].Requests != 1 || apiKeys[8].ErrorCount != 1 {
		t.Fatalf("APIKeyStats[8] = %+v, want requests=1 errors=1", apiKeys[8])
	}
}

func TestAPIKeySelfUsageReportScopesToSingleKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	keyID, err := db.InsertAPIKey(ctx, "Client A", "sk-self-usage-a-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey A 返回错误: %v", err)
	}
	otherKeyID, err := db.InsertAPIKey(ctx, "Client B", "sk-self-usage-b-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey B 返回错误: %v", err)
	}
	now := time.Now()
	insertUsage := func(apiKeyID int64, model string, endpoint string, statusCode int, totalTokens int, userBilled float64) {
		t.Helper()
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO usage_logs (
				api_key_id, api_key_name, api_key_masked, endpoint, inbound_endpoint, model,
				status_code, total_tokens, input_tokens, output_tokens, user_billed, created_at
			)
			VALUES ($1, 'client', 'sk-...test', $2, $2, $3, $4, $5, $6, $7, $8, $9)
		`, apiKeyID, endpoint, model, statusCode, totalTokens, totalTokens/2, totalTokens/2, userBilled, sqliteTimeParam(now)); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
	}

	insertUsage(keyID, "gpt-5.5", "/v1/responses", 200, 120, 0.12)
	insertUsage(keyID, "gpt-5.5", "/v1/responses", 500, 80, 0.08)
	insertUsage(keyID, "cancelled", "/v1/responses", 499, 1000, 9.99)
	insertUsage(otherKeyID, "other", "/v1/messages", 200, 900, 7.77)

	report, err := db.GetAPIKeySelfUsageReport(ctx, keyID, now.Add(-time.Hour), now.Add(time.Hour), 1, 10)
	if err != nil {
		t.Fatalf("GetAPIKeySelfUsageReport 返回错误: %v", err)
	}
	if report.Summary.Requests != 2 || report.Summary.Tokens != 200 || report.Summary.ErrorCount != 1 {
		t.Fatalf("summary = %+v, want requests=2 tokens=200 errors=1", report.Summary)
	}
	if math.Abs(report.Summary.UserBilled-0.20) > 0.000001 {
		t.Fatalf("summary.UserBilled = %.6f, want 0.20", report.Summary.UserBilled)
	}
	if report.Windows.Last30d.Requests != 2 || report.Windows.Last30d.Tokens != 200 {
		t.Fatalf("last30d = %+v, want current key only", report.Windows.Last30d)
	}
	if len(report.Models) != 1 || report.Models[0].Name != "gpt-5.5" || report.Models[0].Requests != 2 {
		t.Fatalf("models = %+v, want one gpt-5.5 bucket", report.Models)
	}
	if len(report.RecentLogs) != 2 {
		t.Fatalf("recent logs = %d, want 2", len(report.RecentLogs))
	}
	if report.RecentLogsTotal != 2 || report.RecentLogsPage != 1 || report.RecentLogsPageSize != 10 {
		t.Fatalf("recent log pagination = total %d page %d page_size %d, want 2/1/10", report.RecentLogsTotal, report.RecentLogsPage, report.RecentLogsPageSize)
	}
	for _, logRow := range report.RecentLogs {
		if logRow.Model == "other" || logRow.StatusCode == 499 {
			t.Fatalf("recent log leaked unrelated/cancelled row: %+v", logRow)
		}
	}
}

func TestAPIKeySelfUsageReportPaginatesRecentLogs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	keyID, err := db.InsertAPIKey(ctx, "Client A", "sk-self-usage-page-a-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey A 返回错误: %v", err)
	}
	otherKeyID, err := db.InsertAPIKey(ctx, "Client B", "sk-self-usage-page-b-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey B 返回错误: %v", err)
	}

	now := time.Now()
	insertUsage := func(apiKeyID int64, model string, statusCode int, totalTokens int) {
		t.Helper()
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO usage_logs (
				api_key_id, api_key_name, api_key_masked, endpoint, inbound_endpoint, model,
				status_code, total_tokens, input_tokens, output_tokens, user_billed, created_at
			)
			VALUES ($1, 'client', 'sk-...test', '/v1/responses', '/v1/responses', $2, $3, $4, $5, $6, 0, $7)
		`, apiKeyID, model, statusCode, totalTokens, totalTokens/2, totalTokens/2, sqliteTimeParam(now)); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
	}

	for i := 1; i <= 5; i++ {
		insertUsage(keyID, fmt.Sprintf("model-%d", i), 200, i*10)
	}
	insertUsage(keyID, "cancelled", 499, 999)
	insertUsage(otherKeyID, "other", 200, 777)

	report, err := db.GetAPIKeySelfUsageReport(ctx, keyID, now.Add(-time.Hour), now.Add(time.Hour), 2, 2)
	if err != nil {
		t.Fatalf("GetAPIKeySelfUsageReport 返回错误: %v", err)
	}
	if report.RecentLogsTotal != 5 || report.RecentLogsPage != 2 || report.RecentLogsPageSize != 2 {
		t.Fatalf("recent log pagination = total %d page %d page_size %d, want 5/2/2", report.RecentLogsTotal, report.RecentLogsPage, report.RecentLogsPageSize)
	}
	if len(report.RecentLogs) != 2 {
		t.Fatalf("recent logs = %d, want 2", len(report.RecentLogs))
	}
	if report.RecentLogs[0].Model != "model-3" || report.RecentLogs[1].Model != "model-2" {
		t.Fatalf("page 2 logs = %+v, want model-3 then model-2", report.RecentLogs)
	}

	clamped, err := db.GetAPIKeySelfUsageReport(ctx, keyID, now.Add(-time.Hour), now.Add(time.Hour), 99, 2)
	if err != nil {
		t.Fatalf("GetAPIKeySelfUsageReport clamped 返回错误: %v", err)
	}
	if clamped.RecentLogsPage != 3 || len(clamped.RecentLogs) != 1 || clamped.RecentLogs[0].Model != "model-1" {
		t.Fatalf("clamped page logs = page %d rows %+v, want page 3 with model-1", clamped.RecentLogsPage, clamped.RecentLogs)
	}
}

func TestUsageStatsBreakdownsRespectExplicitRange(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	rangeStart := time.Now().Add(-2 * time.Hour)
	rangeEnd := time.Now().Add(1 * time.Hour)
	logs := []*UsageLogInput{
		{
			AccountID:          1,
			Endpoint:           "/v1/responses",
			InboundEndpoint:    "/v1/responses",
			Model:              "range-model",
			StatusCode:         200,
			PromptTokens:       70,
			CompletionTokens:   30,
			InputTokens:        70,
			OutputTokens:       30,
			TotalTokens:        100,
			Stream:             true,
			BillingServiceTier: "fast",
			APIKeyID:           10,
			APIKeyName:         "Range Key",
			APIKeyMasked:       "sk-...range",
		},
		{
			AccountID:        2,
			Endpoint:         "/v1/old",
			InboundEndpoint:  "/v1/old",
			Model:            "old-model",
			StatusCode:       200,
			PromptTokens:     40,
			CompletionTokens: 10,
			InputTokens:      40,
			OutputTokens:     10,
			TotalTokens:      50,
			APIKeyID:         11,
			APIKeyName:       "Old Key",
			APIKeyMasked:     "sk-...old",
		},
	}
	for _, usageLog := range logs {
		if err := db.InsertUsageLog(ctx, usageLog); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	db.flushLogs()

	oldCreatedAt := rangeStart.Add(-24 * time.Hour)
	if _, err := db.conn.ExecContext(ctx, `UPDATE usage_logs SET created_at = $1 WHERE model = $2`, sqliteTimeParam(oldCreatedAt), "old-model"); err != nil {
		t.Fatalf("更新旧日志时间失败: %v", err)
	}

	stats, err := db.GetUsageStats(ctx, rangeStart, rangeEnd)
	if err != nil {
		t.Fatalf("GetUsageStats 返回错误: %v", err)
	}
	if stats.TodayRequests != 1 || stats.TodayTokens != 100 || stats.TodayPrompt != 70 || stats.TodayCompletion != 30 {
		t.Fatalf("range stats = requests %d tokens %d prompt %d completion %d, want 1/100/70/30",
			stats.TodayRequests, stats.TodayTokens, stats.TodayPrompt, stats.TodayCompletion)
	}
	if stats.TotalRequests != 2 || stats.TotalTokens != 150 {
		t.Fatalf("total stats = requests %d tokens %d, want cumulative 2/150", stats.TotalRequests, stats.TotalTokens)
	}
	if len(stats.ModelStats) != 1 || stats.ModelStats[0].Model != "range-model" || stats.ModelStats[0].Requests != 1 {
		t.Fatalf("ModelStats = %+v, want only range-model", stats.ModelStats)
	}
	if stats.FeatureStats.StreamRequests != 1 || stats.FeatureStats.SyncRequests != 0 || stats.FeatureStats.FastRequests != 1 {
		t.Fatalf("FeatureStats = %+v, want selected range only", stats.FeatureStats)
	}
	endpoints := make(map[string]UsageEndpointStat)
	for _, item := range stats.EndpointStats {
		endpoints[item.Endpoint] = item
	}
	if _, ok := endpoints["/v1/old"]; ok || endpoints["/v1/responses"].Requests != 1 {
		t.Fatalf("EndpointStats = %+v, want selected range only", stats.EndpointStats)
	}
	apiKeys := make(map[int64]UsageAPIKeyStat)
	for _, item := range stats.APIKeyStats {
		apiKeys[item.APIKeyID] = item
	}
	if _, ok := apiKeys[11]; ok || apiKeys[10].Requests != 1 {
		t.Fatalf("APIKeyStats = %+v, want selected range only", stats.APIKeyStats)
	}
}

func TestUsageStatsBaselinePreservesCacheRateAndFirstTokenAfterClear(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for _, usageLog := range []*UsageLogInput{
		{
			AccountID:    1,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.5",
			StatusCode:   200,
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
			CachedTokens: 32,
			FirstTokenMs: 600,
		},
		{
			AccountID:    1,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.5",
			StatusCode:   200,
			InputTokens:  80,
			OutputTokens: 20,
			TotalTokens:  100,
			FirstTokenMs: 300,
		},
	} {
		if err := db.InsertUsageLog(ctx, usageLog); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	db.flushLogs()

	if err := db.ClearUsageLogs(ctx); err != nil {
		t.Fatalf("ClearUsageLogs 返回错误: %v", err)
	}

	stats, err := db.GetUsageStats(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetUsageStats 返回错误: %v", err)
	}
	if stats.TotalRequests != 2 {
		t.Fatalf("TotalRequests = %d, want 2", stats.TotalRequests)
	}
	if stats.TotalCacheRate < 49.9 || stats.TotalCacheRate > 50.1 {
		t.Fatalf("TotalCacheRate = %.4f, want about 50.00", stats.TotalCacheRate)
	}
	if stats.AvgFirstTokenMs < 449.9 || stats.AvgFirstTokenMs > 450.1 {
		t.Fatalf("AvgFirstTokenMs = %.4f, want about 450.00", stats.AvgFirstTokenMs)
	}
}

func TestSoftDeleteAccountMarksDeletedStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "delete-me", "rt-delete-me", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}
	if err := db.SoftDeleteAccount(ctx, id); err != nil {
		t.Fatalf("SoftDeleteAccount 返回错误: %v", err)
	}

	active, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive 返回 %d 条，want 0", len(active))
	}
	if _, err := db.GetAccountByID(ctx, id); err == nil {
		t.Fatal("GetAccountByID 应该排除已删除账号")
	}

	var status string
	var errorMessage string
	var deletedAt sql.NullString
	if err := db.conn.QueryRowContext(ctx, `SELECT status, error_message, deleted_at FROM accounts WHERE id = $1`, id).Scan(&status, &errorMessage, &deletedAt); err != nil {
		t.Fatalf("查询账号状态返回错误: %v", err)
	}
	if status != "deleted" {
		t.Fatalf("status = %q, want deleted", status)
	}
	if errorMessage != "" {
		t.Fatalf("error_message = %q, want empty", errorMessage)
	}
	if !deletedAt.Valid || deletedAt.String == "" {
		t.Fatal("deleted_at 未写入")
	}
}

func TestSQLiteMigratesLegacyDeletedAccounts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	ctx := context.Background()

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	id, err := db.InsertAccount(ctx, "legacy-delete", "rt-legacy-delete", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}
	if err := db.SetError(ctx, id, "deleted"); err != nil {
		t.Fatalf("SetError 返回错误: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	db, err = New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	var status string
	var errorMessage string
	var deletedAt sql.NullString
	if err := db.conn.QueryRowContext(ctx, `SELECT status, error_message, deleted_at FROM accounts WHERE id = $1`, id).Scan(&status, &errorMessage, &deletedAt); err != nil {
		t.Fatalf("查询迁移后账号返回错误: %v", err)
	}
	if status != "deleted" {
		t.Fatalf("status = %q, want deleted", status)
	}
	if errorMessage != "" {
		t.Fatalf("error_message = %q, want empty", errorMessage)
	}
	if !deletedAt.Valid || deletedAt.String == "" {
		t.Fatal("deleted_at 未迁移")
	}
}

func TestListActiveIncludesErrorAccounts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "error-account", "rt-error", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}
	if err := db.SetError(ctx, id, "batch test failed"); err != nil {
		t.Fatalf("SetError 返回错误: %v", err)
	}

	rows, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListActive 返回 %d 条，want 1", len(rows))
	}
	if rows[0].Status != "error" {
		t.Fatalf("status = %q, want error", rows[0].Status)
	}
	if rows[0].ErrorMessage != "batch test failed" {
		t.Fatalf("error_message = %q, want batch test failed", rows[0].ErrorMessage)
	}
}

func TestSetCooldownWithErrorPersistsMessage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "cooldown-account", "rt-cooldown", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}
	until := time.Now().Add(time.Hour)
	if err := db.SetCooldownWithError(ctx, id, "unauthorized", until, "上游返回 401: token_invalidated"); err != nil {
		t.Fatalf("SetCooldownWithError 返回错误: %v", err)
	}

	var reason string
	var errorMessage string
	var cooldownUntil sql.NullTime
	if err := db.conn.QueryRowContext(ctx, `SELECT cooldown_reason, error_message, cooldown_until FROM accounts WHERE id = $1`, id).Scan(&reason, &errorMessage, &cooldownUntil); err != nil {
		t.Fatalf("查询账号冷却状态返回错误: %v", err)
	}
	if reason != "unauthorized" {
		t.Fatalf("cooldown_reason = %q, want unauthorized", reason)
	}
	if errorMessage != "上游返回 401: token_invalidated" {
		t.Fatalf("error_message = %q, want recorded upstream error", errorMessage)
	}
	if !cooldownUntil.Valid {
		t.Fatal("cooldown_until 未写入")
	}
}

func TestUsageLogsFilterByAPIKeyID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	targetAPIKeyID := int64(7)

	logs := []*UsageLogInput{
		{
			AccountID:    1,
			Endpoint:     "/v1/chat/completions",
			Model:        "gpt-5.4",
			StatusCode:   200,
			DurationMs:   120,
			APIKeyID:     targetAPIKeyID,
			APIKeyName:   "Team A",
			APIKeyMasked: "sk-a****...****1111",
		},
		{
			AccountID:    1,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.4",
			StatusCode:   200,
			DurationMs:   220,
			Compact:      true,
			APIKeyID:     targetAPIKeyID,
			APIKeyName:   "Team A",
			APIKeyMasked: "sk-a****...****1111",
		},
		{
			AccountID:    2,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.4-mini",
			StatusCode:   200,
			DurationMs:   320,
			APIKeyID:     8,
			APIKeyName:   "Team B",
			APIKeyMasked: "sk-b****...****2222",
		},
	}

	for _, usageLog := range logs {
		if err := db.InsertUsageLog(ctx, usageLog); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	db.flushLogs()

	recentLogs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(recentLogs) != len(logs) {
		t.Fatalf("recentLogs 长度 = %d, want %d", len(recentLogs), len(logs))
	}

	foundSnapshot := false
	foundCompact := false
	for _, usageLog := range recentLogs {
		if usageLog.APIKeyID == targetAPIKeyID {
			foundSnapshot = true
			if usageLog.APIKeyName != "Team A" {
				t.Fatalf("APIKeyName = %q, want %q", usageLog.APIKeyName, "Team A")
			}
			if usageLog.APIKeyMasked != "sk-a****...****1111" {
				t.Fatalf("APIKeyMasked = %q, want %q", usageLog.APIKeyMasked, "sk-a****...****1111")
			}
			if usageLog.Endpoint == "/v1/responses" {
				foundCompact = true
				if !usageLog.Compact {
					t.Fatal("Compact = false, want true for compact usage log")
				}
			}
			if usageLog.Endpoint == "/v1/chat/completions" && usageLog.Compact {
				t.Fatal("Compact = true, want false for normal usage log")
			}
		}
	}
	if !foundSnapshot {
		t.Fatal("未找到带 API 密钥快照的最近日志")
	}
	if !foundCompact {
		t.Fatal("未找到 compact 使用日志")
	}

	page, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{
		Start:    now.Add(-1 * time.Hour),
		End:      now.Add(1 * time.Hour),
		Page:     1,
		PageSize: 10,
		APIKeyID: &targetAPIKeyID,
	})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged 返回错误: %v", err)
	}

	if page.Total != 2 {
		t.Fatalf("page.Total = %d, want %d", page.Total, 2)
	}
	if len(page.Logs) != 2 {
		t.Fatalf("len(page.Logs) = %d, want %d", len(page.Logs), 2)
	}
	for _, usageLog := range page.Logs {
		if usageLog.APIKeyID != targetAPIKeyID {
			t.Fatalf("APIKeyID = %d, want %d", usageLog.APIKeyID, targetAPIKeyID)
		}
		if usageLog.APIKeyName != "Team A" {
			t.Fatalf("APIKeyName = %q, want %q", usageLog.APIKeyName, "Team A")
		}
		if usageLog.Endpoint == "/v1/responses" && !usageLog.Compact {
			t.Fatal("Compact = false, want true in paged usage logs")
		}
	}

	targetAccountID := int64(1)
	page, err = db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{
		Start:     now.Add(-1 * time.Hour),
		End:       now.Add(1 * time.Hour),
		Page:      1,
		PageSize:  10,
		AccountID: &targetAccountID,
	})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged account filter 返回错误: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("account filter page.Total = %d, want %d", page.Total, 2)
	}
	for _, usageLog := range page.Logs {
		if usageLog.AccountID != targetAccountID {
			t.Fatalf("AccountID = %d, want %d", usageLog.AccountID, targetAccountID)
		}
	}
}

func TestUsageLogsIncludeAccountNameForOpenAIResponsesAccount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "API 别名", map[string]interface{}{
		"base_url": "https://api.example.com",
		"email":    "https://api.example.com",
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount 返回错误: %v", err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:  accountID,
		Endpoint:   "/v1/responses",
		Model:      "gpt-4.1",
		StatusCode: 200,
		DurationMs: 120,
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	recentLogs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(recentLogs) != 1 {
		t.Fatalf("recentLogs 长度 = %d, want 1", len(recentLogs))
	}
	if recentLogs[0].AccountName != "API 别名" {
		t.Fatalf("AccountName = %q, want API 别名", recentLogs[0].AccountName)
	}
	if recentLogs[0].AccountEmail != "https://api.example.com" {
		t.Fatalf("AccountEmail = %q, want base URL", recentLogs[0].AccountEmail)
	}

	page, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{
		Start:    now.Add(-1 * time.Hour),
		End:      now.Add(1 * time.Hour),
		Page:     1,
		PageSize: 10,
		Email:    "API 别名",
	})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged 返回错误: %v", err)
	}
	if page.Total != 1 || len(page.Logs) != 1 {
		t.Fatalf("page = total %d len %d, want 1/1", page.Total, len(page.Logs))
	}
	if page.Logs[0].AccountName != "API 别名" {
		t.Fatalf("paged AccountName = %q, want API 别名", page.Logs[0].AccountName)
	}

	logs, err := db.ListUsageLogsByFilter(ctx, UsageLogFilter{
		Start: now.Add(-1 * time.Hour),
		End:   now.Add(1 * time.Hour),
		Query: "API 别名",
	})
	if err != nil {
		t.Fatalf("ListUsageLogsByFilter 返回错误: %v", err)
	}
	if len(logs) != 1 || logs[0].AccountName != "API 别名" {
		t.Fatalf("filter logs = %+v, want one account name match", logs)
	}
}

func TestSQLiteUsageLogsTimeRangeUsesUTCStorage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	createdUTC := time.Date(2026, 4, 23, 20, 6, 0, 0, time.UTC)
	if _, err := db.conn.ExecContext(ctx, `
		INSERT INTO usage_logs (
			account_id, endpoint, inbound_endpoint, upstream_endpoint, model,
			status_code, total_tokens, input_tokens, output_tokens, created_at
		)
		VALUES (1, '/v1/images/generations', '/v1/images/generations', '/v1/responses', 'gpt-image-2',
			200, 1790, 34, 1756, $1)
	`, sqliteTimeParam(createdUTC)); err != nil {
		t.Fatalf("insert usage log 返回错误: %v", err)
	}

	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	localCreated := createdUTC.In(shanghai)
	page, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{
		Start:    localCreated.Add(-1 * time.Hour),
		End:      localCreated.Add(1 * time.Hour),
		Page:     1,
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged 返回错误: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("page.Total = %d, want %d", page.Total, 1)
	}
	if len(page.Logs) != 1 {
		t.Fatalf("len(page.Logs) = %d, want %d", len(page.Logs), 1)
	}
	if got := page.Logs[0].InboundEndpoint; got != "/v1/images/generations" {
		t.Fatalf("InboundEndpoint = %q, want /v1/images/generations", got)
	}
	if got := page.Logs[0].Model; got != "gpt-image-2" {
		t.Fatalf("Model = %q, want gpt-image-2", got)
	}
}

func TestGetAccountUsageStatsAggregatesRecentAccountSummary(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	insertUsage := func(accountID int64, model string, statusCode int, totalTokens, inputTokens, outputTokens, reasoningTokens, cachedTokens, durationMs int, accountBilled, userBilled float64, createdAt time.Time) {
		t.Helper()
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO usage_logs (
				account_id, model, effective_model, status_code, total_tokens,
				input_tokens, output_tokens, reasoning_tokens, cached_tokens,
				duration_ms, account_billed, user_billed, created_at
			)
			VALUES ($1, $2, '', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, accountID, model, statusCode, totalTokens, inputTokens, outputTokens, reasoningTokens, cachedTokens, durationMs, accountBilled, userBilled, sqliteTimeParam(createdAt)); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
	}

	insertUsage(7, "gpt-5.5", 200, 1000, 700, 300, 50, 100, 1000, 1.25, 0.50, todayStart.Add(1*time.Hour))
	insertUsage(7, "gpt-5.5", 200, 2000, 1500, 500, 100, 0, 2000, 2.50, 1.00, todayStart.AddDate(0, 0, -1).Add(1*time.Hour))
	insertUsage(7, "gpt-4.1", 500, 3000, 2000, 1000, 150, 250, 3000, 3.75, 1.50, todayStart.AddDate(0, 0, -1).Add(2*time.Hour))
	insertUsage(7, "old-model", 200, 9000, 9000, 0, 0, 0, 5000, 9.99, 9.99, todayStart.AddDate(0, 0, -40))
	insertUsage(7, "cancelled", 499, 8000, 8000, 0, 0, 0, 5000, 8.88, 8.88, todayStart.Add(2*time.Hour))
	insertUsage(8, "other-account", 200, 7000, 7000, 0, 0, 0, 5000, 7.77, 7.77, todayStart.Add(2*time.Hour))
	if _, err := db.conn.ExecContext(ctx, `UPDATE usage_logs SET first_token_ms = 500, stream = 1 WHERE account_id = 7 AND total_tokens = 1000`); err != nil {
		t.Fatalf("update first usage quality fields: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE usage_logs SET first_token_ms = 1500, compact = 1 WHERE account_id = 7 AND total_tokens = 2000`); err != nil {
		t.Fatalf("update second usage quality fields: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE usage_logs SET first_token_ms = 2500, stream = 1, compact = 1, is_retry_attempt = 1, attempt_index = 1 WHERE account_id = 7 AND model = 'gpt-4.1'`); err != nil {
		t.Fatalf("update third usage quality fields: %v", err)
	}

	got, err := db.GetAccountUsageStats(ctx, 7, 30)
	if err != nil {
		t.Fatalf("GetAccountUsageStats 返回错误: %v", err)
	}

	if got.PeriodDays != 30 {
		t.Fatalf("PeriodDays = %d, want 30", got.PeriodDays)
	}
	if got.TotalRequests != 3 {
		t.Fatalf("TotalRequests = %d, want 3", got.TotalRequests)
	}
	if got.TotalTokens != 6000 {
		t.Fatalf("TotalTokens = %d, want 6000", got.TotalTokens)
	}
	if got.InputTokens != 4200 || got.OutputTokens != 1800 || got.ReasoningTokens != 300 || got.CachedTokens != 350 {
		t.Fatalf("token breakdown = input %d output %d reasoning %d cached %d", got.InputTokens, got.OutputTokens, got.ReasoningTokens, got.CachedTokens)
	}
	if math.Abs(got.TotalAccountBilled-7.50) > 0.000001 {
		t.Fatalf("TotalAccountBilled = %.4f, want 7.5000", got.TotalAccountBilled)
	}
	if math.Abs(got.TotalUserBilled-3.00) > 0.000001 {
		t.Fatalf("TotalUserBilled = %.4f, want 3.0000", got.TotalUserBilled)
	}
	if got.Today.Requests != 1 || got.Today.Tokens != 1000 {
		t.Fatalf("Today = %+v, want requests 1 tokens 1000", got.Today)
	}
	if got.ActiveDays < 2 {
		t.Fatalf("ActiveDays = %d, want at least 2", got.ActiveDays)
	}
	if got.HighestCostDay == nil || math.Abs(got.HighestCostDay.AccountBilled-6.25) > 0.000001 {
		t.Fatalf("HighestCostDay = %+v, want account billed 6.25", got.HighestCostDay)
	}
	if got.HighestRequestDay == nil || got.HighestRequestDay.Requests != 2 {
		t.Fatalf("HighestRequestDay = %+v, want requests 2", got.HighestRequestDay)
	}
	if len(got.History) != 2 {
		t.Fatalf("len(History) = %d, want 2", len(got.History))
	}
	if len(got.Models) != 2 {
		t.Fatalf("len(Models) = %d, want 2", len(got.Models))
	}
	if got.Models[0].Model != "gpt-5.5" || got.Models[0].Requests != 2 {
		t.Fatalf("Models[0] = %+v, want gpt-5.5 requests 2", got.Models[0])
	}
	if got.Models[0].Tokens != 3000 || got.Models[0].InputTokens != 2200 || got.Models[0].OutputTokens != 800 || got.Models[0].ReasoningTokens != 150 || got.Models[0].CachedTokens != 100 {
		t.Fatalf("Models[0] token breakdown = %+v, want tokens 3000 input 2200 output 800 reasoning 150 cached 100", got.Models[0])
	}
	if math.Abs(got.Models[0].AccountBilled-3.75) > 0.000001 || math.Abs(got.Models[0].UserBilled-1.50) > 0.000001 {
		t.Fatalf("Models[0] billed = account %.4f user %.4f, want 3.7500/1.5000", got.Models[0].AccountBilled, got.Models[0].UserBilled)
	}
	if got.ErrorRequests != 1 || math.Abs(got.ErrorRate-33.333333) > 0.001 {
		t.Fatalf("error quality = requests %d rate %.4f, want 1/33.33", got.ErrorRequests, got.ErrorRate)
	}
	if got.RetryRequests != 1 {
		t.Fatalf("RetryRequests = %d, want 1", got.RetryRequests)
	}
	if got.FirstTokenSamples != 3 || math.Abs(got.AvgFirstTokenMs-1500) > 0.001 {
		t.Fatalf("first token quality = samples %d avg %.2f, want 3/1500", got.FirstTokenSamples, got.AvgFirstTokenMs)
	}
	if math.Abs(got.P95DurationMs-3000) > 0.001 {
		t.Fatalf("P95DurationMs = %.2f, want 3000", got.P95DurationMs)
	}
	if got.StreamRequests != 2 || math.Abs(got.StreamRate-66.666667) > 0.001 {
		t.Fatalf("stream quality = requests %d rate %.4f, want 2/66.67", got.StreamRequests, got.StreamRate)
	}
	if got.CompactRequests != 2 || math.Abs(got.CompactRate-66.666667) > 0.001 {
		t.Fatalf("compact quality = requests %d rate %.4f, want 2/66.67", got.CompactRequests, got.CompactRate)
	}

	todayOnly, err := db.GetAccountUsageStats(ctx, 7, 1)
	if err != nil {
		t.Fatalf("GetAccountUsageStats today 返回错误: %v", err)
	}
	if todayOnly.TotalRequests != 1 || todayOnly.TotalTokens != 1000 {
		t.Fatalf("todayOnly = requests %d tokens %d, want 1/1000", todayOnly.TotalRequests, todayOnly.TotalTokens)
	}

	allTime, err := db.GetAccountUsageStats(ctx, 7, 0)
	if err != nil {
		t.Fatalf("GetAccountUsageStats all-time 返回错误: %v", err)
	}
	if allTime.PeriodDays != 0 {
		t.Fatalf("allTime.PeriodDays = %d, want 0", allTime.PeriodDays)
	}
	if allTime.TotalRequests != 4 || allTime.TotalTokens != 15000 {
		t.Fatalf("allTime = requests %d tokens %d, want 4/15000", allTime.TotalRequests, allTime.TotalTokens)
	}
}

func TestGetAccountsBilledSinceUsesPerAccountWindows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	insertUsage := func(accountID int64, statusCode int, billed float64, createdAt time.Time) {
		t.Helper()
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO usage_logs (account_id, status_code, account_billed, created_at)
			VALUES ($1, $2, $3, $4)
		`, accountID, statusCode, billed, sqliteTimeParam(createdAt)); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
	}

	insertUsage(1, 200, 1.25, now.Add(-4*time.Hour))
	insertUsage(1, 200, 9.99, now.Add(-6*time.Hour))
	insertUsage(1, 499, 7.77, now.Add(-30*time.Minute))
	insertUsage(2, 200, 2.50, now.AddDate(0, 0, -6))
	insertUsage(2, 200, 8.88, now.AddDate(0, 0, -8))

	got, err := db.GetAccountsBilledSince(ctx, map[int64]time.Time{
		1: now.Add(-5 * time.Hour),
		2: now.AddDate(0, 0, -7),
		3: now.Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("GetAccountsBilledSince 返回错误: %v", err)
	}

	if got[1] != 1.25 {
		t.Fatalf("account 1 billed = %.2f, want 1.25", got[1])
	}
	if got[2] != 2.50 {
		t.Fatalf("account 2 billed = %.2f, want 2.50", got[2])
	}
	if got[3] != 0 {
		t.Fatalf("account 3 billed = %.2f, want 0", got[3])
	}
}

func TestFlushLogsRequeuesBatchWhenSQLiteBeginFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	close(db.logStop)
	db.logWg.Wait()

	ctx := context.Background()
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions"} {
		if err := db.InsertUsageLog(ctx, &UsageLogInput{
			Endpoint:    endpoint,
			Model:       "gpt-5.4",
			StatusCode:  200,
			InputTokens: 1000,
		}); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	if err := db.conn.Close(); err != nil {
		t.Fatalf("关闭 sqlite 连接返回错误: %v", err)
	}

	db.flushLogs()

	db.logMu.Lock()
	defer db.logMu.Unlock()
	if len(db.logBuf) != 2 {
		t.Fatalf("len(logBuf) = %d, want 2", len(db.logBuf))
	}
	if db.logBuf[0].Endpoint != "/v1/responses" || db.logBuf[1].Endpoint != "/v1/chat/completions" {
		t.Fatalf("requeued endpoints = %q, %q", db.logBuf[0].Endpoint, db.logBuf[1].Endpoint)
	}
}

func TestFlushLogsRollsBackAndRequeuesWhenQuotaUpdateFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	close(db.logStop)
	db.logWg.Wait()
	defer db.conn.Close()

	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `ALTER TABLE api_keys RENAME TO api_keys_broken`); err != nil {
		t.Fatalf("rename api_keys 返回错误: %v", err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		APIKeyID:    123,
		Endpoint:    "/v1/responses",
		Model:       "gpt-5.4",
		StatusCode:  200,
		InputTokens: 1000,
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}

	db.flushLogs()

	db.logMu.Lock()
	if len(db.logBuf) != 1 {
		db.logMu.Unlock()
		t.Fatalf("len(logBuf) = %d, want 1", len(db.logBuf))
	}
	if db.logBuf[0].APIKeyID != 123 {
		db.logMu.Unlock()
		t.Fatalf("requeued APIKeyID = %d, want 123", db.logBuf[0].APIKeyID)
	}
	db.logMu.Unlock()

	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs`).Scan(&count); err != nil {
		t.Fatalf("count usage_logs 返回错误: %v", err)
	}
	if count != 0 {
		t.Fatalf("usage_logs count = %d, want 0 after rolled-back flush", count)
	}
}

func TestFlushLogsRetriesQuotaOnlyBatchWithoutDoubleCharge(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	close(db.logStop)
	db.logWg.Wait()
	defer db.conn.Close()
	db.SetUsageLogConfig(UsageLogModeOff, 10, 5)

	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "quota-only-retry", "sk-quota-only-retry-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `ALTER TABLE api_keys RENAME TO api_keys_broken`); err != nil {
		t.Fatalf("rename api_keys 返回错误: %v", err)
	}
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		APIKeyID:    apiKeyID,
		Endpoint:    "/v1/responses",
		Model:       "gpt-5.4",
		StatusCode:  200,
		InputTokens: 1000,
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}

	db.flushLogs()
	db.logMu.Lock()
	if len(db.logBuf) != 1 {
		db.logMu.Unlock()
		t.Fatalf("len(logBuf) = %d, want 1", len(db.logBuf))
	}
	if db.logBuf[0].StoreUsageLog {
		db.logMu.Unlock()
		t.Fatal("quota-only event unexpectedly marked for usage log storage")
	}
	db.logMu.Unlock()

	if _, err := db.conn.ExecContext(ctx, `ALTER TABLE api_keys_broken RENAME TO api_keys`); err != nil {
		t.Fatalf("restore api_keys 返回错误: %v", err)
	}
	db.flushLogs()
	db.flushLogs()

	want := calculateCost(1000, 0, 0, "gpt-5.4", "")
	var quotaUsed, totalUsed float64
	if err := db.conn.QueryRowContext(ctx, `SELECT quota_used, total_used FROM api_keys WHERE id = $1`, apiKeyID).Scan(&quotaUsed, &totalUsed); err != nil {
		t.Fatalf("查询 API Key 用量返回错误: %v", err)
	}
	if math.Abs(quotaUsed-want) > 1e-12 || math.Abs(totalUsed-want) > 1e-12 {
		t.Fatalf("API Key usage = quota %.12f total %.12f, want exactly one charge %.12f", quotaUsed, totalUsed, want)
	}

	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_logs`).Scan(&count); err != nil {
		t.Fatalf("count usage_logs 返回错误: %v", err)
	}
	if count != 0 {
		t.Fatalf("usage_logs count = %d, want 0", count)
	}
}

func TestPromptFilterLogsPersistReviewMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
		Source:          "local_filter",
		Endpoint:        "/v1/responses",
		Model:           "gpt-5.4",
		Action:          "allow",
		Mode:            "block",
		Score:           100,
		Threshold:       50,
		MatchedPatterns: `[{"name":"credential_theft","weight":100}]`,
		TextPreview:     "preview",
		ReviewModel:     "omni-moderation-latest",
		ReviewFlagged:   false,
		ReviewError:     "temporary failure",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog 返回错误: %v", err)
	}

	logs, total, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Page: 1, PageSize: 10, Query: "temporary"})
	if err != nil {
		t.Fatalf("ListPromptFilterLogsPage 返回错误: %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("logs total=%d len=%d, want 1", total, len(logs))
	}
	got := logs[0]
	if got.ReviewModel != "omni-moderation-latest" || got.ReviewFlagged || got.ReviewError != "temporary failure" {
		t.Fatalf("review metadata = %+v", got)
	}
}

func TestSQLiteSystemSettingsContinueThinkingRoundtrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// 播种默认行（全新库无 system_settings 行），并确认续想默认关闭、轮数 8。
	seed := &SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		CodexContinueMaxRounds: 8,
	}
	if err := db.UpdateSystemSettings(ctx, seed); err != nil {
		t.Fatalf("UpdateSystemSettings(seed): %v", err)
	}
	got, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if got.CodexContinueThinkingEnabled {
		t.Errorf("默认应关闭续想, got enabled")
	}
	if got.CodexContinueMaxRounds != 8 {
		t.Errorf("默认轮数 = %d, want 8", got.CodexContinueMaxRounds)
	}

	// 写入后读回。
	got.CodexContinueThinkingEnabled = true
	got.CodexContinueMaxRounds = 15
	if err := db.UpdateSystemSettings(ctx, got); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	after, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings(2): %v", err)
	}
	if !after.CodexContinueThinkingEnabled || after.CodexContinueMaxRounds != 15 {
		t.Fatalf("往返后 = {enabled=%v rounds=%d}, want {true 15}", after.CodexContinueThinkingEnabled, after.CodexContinueMaxRounds)
	}

	// 越界轮数落库时归一到上界 32。
	after.CodexContinueMaxRounds = 100
	if err := db.UpdateSystemSettings(ctx, after); err != nil {
		t.Fatalf("UpdateSystemSettings(clamp): %v", err)
	}
	clamped, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings(3): %v", err)
	}
	if clamped.CodexContinueMaxRounds != 32 {
		t.Errorf("越界轮数应归一到 32, got %d", clamped.CodexContinueMaxRounds)
	}
}
