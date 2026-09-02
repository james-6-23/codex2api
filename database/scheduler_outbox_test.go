package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSchedulerOutboxRoundTripAndCleanup(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "scheduler-outbox.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertSchedulerOutboxEvent(ctx, SchedulerEntityAccount, 41, "upsert"); err != nil {
		t.Fatalf("InsertSchedulerOutboxEvent(account): %v", err)
	}
	firstHigh, err := db.SchedulerOutboxHighWatermark(ctx)
	if err != nil || firstHigh <= 0 {
		t.Fatalf("SchedulerOutboxHighWatermark = %d, err=%v", firstHigh, err)
	}
	if err := db.InsertSchedulerOutboxEvent(ctx, SchedulerEntityAPIKey, 7, "routing_changed"); err != nil {
		t.Fatalf("InsertSchedulerOutboxEvent(api_key): %v", err)
	}

	events, err := db.ListSchedulerOutboxEventsAfter(ctx, firstHigh, 100)
	if err != nil {
		t.Fatalf("ListSchedulerOutboxEventsAfter: %v", err)
	}
	if len(events) != 1 || events[0].EntityType != SchedulerEntityAPIKey || events[0].EntityID != 7 {
		t.Fatalf("events = %+v, want api_key/7", events)
	}

	if _, err := db.conn.ExecContext(ctx, `UPDATE scheduler_outbox SET created_at=$1`, sqliteTimeParam(time.Now().Add(-8*24*time.Hour))); err != nil {
		t.Fatalf("age scheduler outbox: %v", err)
	}
	removed, err := db.CleanupSchedulerOutbox(ctx, time.Now().Add(-7*24*time.Hour), 10000)
	if err != nil {
		t.Fatalf("CleanupSchedulerOutbox: %v", err)
	}
	if removed != 2 {
		t.Fatalf("CleanupSchedulerOutbox removed %d rows, want 2", removed)
	}
}

func TestSchedulerOutboxEventRollbackIsAtomic(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "scheduler-outbox-rollback.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := insertSchedulerOutboxEventTx(ctx, tx, SchedulerEntityAccount, 99, "upsert"); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("withWriteTx returned nil, want rollback error")
	}
	watermark, err := db.SchedulerOutboxHighWatermark(ctx)
	if err != nil {
		t.Fatalf("SchedulerOutboxHighWatermark: %v", err)
	}
	if watermark != 0 {
		t.Fatalf("watermark = %d, want 0 after rollback", watermark)
	}
}

func TestSchedulerOutboxTriggersExcludeAPIKeyUsageOnlyUpdates(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "scheduler-outbox-triggers.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	keyID, err := db.InsertAPIKey(ctx, "routing-key", "sk-routing")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	createdAt, err := db.SchedulerOutboxHighWatermark(ctx)
	if err != nil || createdAt == 0 {
		t.Fatalf("watermark after key insert = %d, err=%v", createdAt, err)
	}
	if err := db.UpdateAPIKeyQuotaLimit(ctx, keyID, 10); err != nil {
		t.Fatalf("UpdateAPIKeyQuotaLimit: %v", err)
	}
	afterQuota, err := db.SchedulerOutboxHighWatermark(ctx)
	if err != nil {
		t.Fatalf("watermark after quota update: %v", err)
	}
	if afterQuota != createdAt {
		t.Fatalf("usage-only API key update emitted routing event: %d -> %d", createdAt, afterQuota)
	}

	if err := db.UpdateAPIKeyAllowedGroups(ctx, keyID, []int64{9}); err != nil {
		t.Fatalf("UpdateAPIKeyAllowedGroups: %v", err)
	}
	events, err := db.ListSchedulerOutboxEventsAfter(ctx, createdAt, 10)
	if err != nil {
		t.Fatalf("ListSchedulerOutboxEventsAfter: %v", err)
	}
	if len(events) != 1 || events[0].EntityType != SchedulerEntityAPIKey || events[0].EntityID != keyID {
		t.Fatalf("routing events = %+v, want api_key/%d", events, keyID)
	}
}

func TestSchedulerOutboxTriggersExcludeAccountUsageSnapshots(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "scheduler-account-trigger.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	accountID, err := db.InsertAccount(ctx, "snapshot-account", "rt", "")
	if err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	createdAt, err := db.SchedulerOutboxHighWatermark(ctx)
	if err != nil {
		t.Fatalf("SchedulerOutboxHighWatermark: %v", err)
	}
	if err := db.UpdateCredentials(ctx, accountID, map[string]interface{}{
		"codex_7d_used_percent":  42,
		"codex_usage_updated_at": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("UpdateCredentials(usage): %v", err)
	}
	afterUsage, err := db.SchedulerOutboxHighWatermark(ctx)
	if err != nil {
		t.Fatalf("watermark after usage: %v", err)
	}
	if afterUsage != createdAt {
		t.Fatalf("usage snapshot emitted routing event: %d -> %d", createdAt, afterUsage)
	}
	if err := db.UpdateCredentials(ctx, accountID, map[string]interface{}{"access_token": "rotated"}); err != nil {
		t.Fatalf("UpdateCredentials(access token): %v", err)
	}
	afterToken, err := db.SchedulerOutboxHighWatermark(ctx)
	if err != nil || afterToken <= afterUsage {
		t.Fatalf("routing credential update watermark = %d, previous=%d, err=%v", afterToken, afterUsage, err)
	}
}

func TestSchedulerBatchAccountProjectionHelpers(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "scheduler-batch-projection.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	firstID, err := db.InsertAccount(ctx, "batch-first", "rt-first", "")
	if err != nil {
		t.Fatalf("InsertAccount(first): %v", err)
	}
	secondID, err := db.InsertAccount(ctx, "batch-second", "rt-second", "")
	if err != nil {
		t.Fatalf("InsertAccount(second): %v", err)
	}
	firstGroup, err := db.CreateAccountGroup(ctx, "batch-group-a", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatalf("CreateAccountGroup(first): %v", err)
	}
	secondGroup, err := db.CreateAccountGroup(ctx, "batch-group-b", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatalf("CreateAccountGroup(second): %v", err)
	}
	if err := db.SetAccountGroups(ctx, firstID, []int64{secondGroup, firstGroup}); err != nil {
		t.Fatalf("SetAccountGroups(first): %v", err)
	}
	if err := db.SetAccountGroups(ctx, secondID, []int64{secondGroup}); err != nil {
		t.Fatalf("SetAccountGroups(second): %v", err)
	}
	if err := db.SetModelCooldown(ctx, firstID, "gpt-5.6", "rate_limited", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetModelCooldown(active): %v", err)
	}
	if err := db.SetModelCooldown(ctx, secondID, "expired", "rate_limited", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("SetModelCooldown(expired): %v", err)
	}

	memberships, err := db.ListAccountGroupMembershipsByAccountIDs(ctx, []int64{secondID, firstID, firstID, 0, 999999})
	if err != nil {
		t.Fatalf("ListAccountGroupMembershipsByAccountIDs: %v", err)
	}
	if got := memberships[firstID]; len(got) != 2 || got[0] != firstGroup || got[1] != secondGroup {
		t.Fatalf("first memberships = %v, want [%d %d]", got, firstGroup, secondGroup)
	}
	if got := memberships[secondID]; len(got) != 1 || got[0] != secondGroup {
		t.Fatalf("second memberships = %v, want [%d]", got, secondGroup)
	}

	cooldowns, err := db.ListActiveModelCooldownsForAccounts(ctx, []int64{secondID, firstID, firstID, 0, 999999})
	if err != nil {
		t.Fatalf("ListActiveModelCooldownsForAccounts: %v", err)
	}
	if len(cooldowns) != 1 || cooldowns[0].AccountID != firstID || cooldowns[0].Model != "gpt-5.6" {
		t.Fatalf("active cooldowns = %+v, want first account gpt-5.6 only", cooldowns)
	}
}

func TestCleanupSchedulerOutboxThroughRespectsWatermark(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "scheduler-outbox-through.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := db.InsertSchedulerOutboxEvent(ctx, SchedulerEntityAccount, int64(100+i), "upsert"); err != nil {
			t.Fatalf("InsertSchedulerOutboxEvent(%d): %v", i, err)
		}
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE scheduler_outbox SET created_at=$1`, sqliteTimeParam(time.Now().Add(-8*24*time.Hour))); err != nil {
		t.Fatalf("age scheduler outbox: %v", err)
	}

	// 只清到本实例已消费的水位:第 3 条虽然过期但未消费,必须保留。
	removed, err := db.CleanupSchedulerOutboxThrough(ctx, time.Now().Add(-7*24*time.Hour), 2, 10000)
	if err != nil {
		t.Fatalf("CleanupSchedulerOutboxThrough: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	events, err := db.ListSchedulerOutboxEventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListSchedulerOutboxEventsAfter: %v", err)
	}
	if len(events) != 1 || events[0].ID != 3 {
		t.Fatalf("surviving events = %+v, want only id 3", events)
	}
}

func TestListSchedulerOutboxEventsByIDs(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "scheduler-outbox-byids.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := db.InsertSchedulerOutboxEvent(ctx, SchedulerEntityAccount, int64(200+i), "upsert"); err != nil {
			t.Fatalf("InsertSchedulerOutboxEvent(%d): %v", i, err)
		}
	}
	events, err := db.ListSchedulerOutboxEventsByIDs(ctx, []int64{1, 3, 999})
	if err != nil {
		t.Fatalf("ListSchedulerOutboxEventsByIDs: %v", err)
	}
	if len(events) != 2 || events[0].ID != 1 || events[1].ID != 3 {
		t.Fatalf("events = %+v, want ids 1 and 3", events)
	}
}
