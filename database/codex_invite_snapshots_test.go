package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func newInviteSnapshotTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "invite-snapshots.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestCodexInviteSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newInviteSnapshotTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	t.Run("missing returns nil without error", func(t *testing.T) {
		// 「还没查过」是正常状态，不该冒出 sql.ErrNoRows 让调用方去分辨。
		snap, err := db.GetCodexInviteSnapshot(ctx, 1, CodexInviteSnapshotEligibility, "p|persistent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if snap != nil {
			t.Fatalf("expected nil snapshot, got %+v", snap)
		}
	})

	t.Run("upsert then read back", func(t *testing.T) {
		payload := json.RawMessage(`{"ok":true,"remaining_send_capacity":3}`)
		if err := db.UpsertCodexInviteSnapshot(ctx, &CodexInviteSnapshot{
			AccountID:            7,
			Kind:                 CodexInviteSnapshotEligibility,
			Scope:                "prog|persistent",
			CredentialGeneration: 2,
			HTTPStatus:           200,
			Payload:              payload,
			ObservedAt:           now,
			ExpiresAt:            now.Add(15 * time.Minute),
		}); err != nil {
			t.Fatalf("UpsertCodexInviteSnapshot: %v", err)
		}

		snap, err := db.GetCodexInviteSnapshot(ctx, 7, CodexInviteSnapshotEligibility, "prog|persistent")
		if err != nil {
			t.Fatalf("GetCodexInviteSnapshot: %v", err)
		}
		if snap == nil {
			t.Fatal("expected snapshot, got nil")
		}
		if snap.CredentialGeneration != 2 || snap.HTTPStatus != 200 {
			t.Fatalf("generation/status mismatch: %+v", snap)
		}
		var decoded struct {
			OK        bool `json:"ok"`
			Remaining int  `json:"remaining_send_capacity"`
		}
		if err := json.Unmarshal(snap.Payload, &decoded); err != nil {
			t.Fatalf("payload not round-tripped: %v (raw=%s)", err, snap.Payload)
		}
		if !decoded.OK || decoded.Remaining != 3 {
			t.Fatalf("payload mismatch: %+v", decoded)
		}
		if snap.Expired(now) {
			t.Fatal("snapshot should not be expired at observation time")
		}
		if !snap.Expired(now.Add(16 * time.Minute)) {
			t.Fatal("snapshot should be expired past its TTL")
		}
	})
}

// 作用域必须区分 limit 不同的跟踪查询，否则 limit=10 写下的快照会被 limit=100
// 的请求当成自己的结果返回。
func TestCodexInviteSnapshotScopeIsolation(t *testing.T) {
	ctx := context.Background()
	db := newInviteSnapshotTestDB(t)
	now := time.Now().UTC()

	write := func(scope string, count int) {
		t.Helper()
		if err := db.UpsertCodexInviteSnapshot(ctx, &CodexInviteSnapshot{
			AccountID:            9,
			Kind:                 CodexInviteSnapshotTracking,
			Scope:                scope,
			CredentialGeneration: 1,
			Payload:              json.RawMessage(`{"count":` + string(rune('0'+count)) + `}`),
			ObservedAt:           now,
			ExpiresAt:            now.Add(time.Minute),
		}); err != nil {
			t.Fatalf("upsert %s: %v", scope, err)
		}
	}
	write("prog|past_90_days|10", 1)
	write("prog|past_90_days|100", 2)

	small, err := db.GetCodexInviteSnapshot(ctx, 9, CodexInviteSnapshotTracking, "prog|past_90_days|10")
	if err != nil || small == nil {
		t.Fatalf("read small scope: %v %+v", err, small)
	}
	large, err := db.GetCodexInviteSnapshot(ctx, 9, CodexInviteSnapshotTracking, "prog|past_90_days|100")
	if err != nil || large == nil {
		t.Fatalf("read large scope: %v %+v", err, large)
	}
	if string(small.Payload) == string(large.Payload) {
		t.Fatalf("scopes collided: both returned %s", small.Payload)
	}
}

// 重新授权后仍在途的旧探针回来时，不能压掉新一代凭据写下的快照。
func TestCodexInviteSnapshotGenerationFence(t *testing.T) {
	ctx := context.Background()
	db := newInviteSnapshotTestDB(t)
	now := time.Now().UTC()

	newer := &CodexInviteSnapshot{
		AccountID: 3, Kind: CodexInviteSnapshotEligibility, Scope: "s",
		CredentialGeneration: 5, Payload: json.RawMessage(`{"gen":5}`),
		ObservedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := db.UpsertCodexInviteSnapshot(ctx, newer); err != nil {
		t.Fatalf("upsert newer: %v", err)
	}

	stale := &CodexInviteSnapshot{
		AccountID: 3, Kind: CodexInviteSnapshotEligibility, Scope: "s",
		CredentialGeneration: 4, Payload: json.RawMessage(`{"gen":4}`),
		ObservedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := db.UpsertCodexInviteSnapshot(ctx, stale); err != nil {
		t.Fatalf("upsert stale should be a no-op, not an error: %v", err)
	}

	snap, err := db.GetCodexInviteSnapshot(ctx, 3, CodexInviteSnapshotEligibility, "s")
	if err != nil || snap == nil {
		t.Fatalf("read back: %v %+v", err, snap)
	}
	if snap.CredentialGeneration != 5 || string(snap.Payload) != `{"gen":5}` {
		t.Fatalf("stale generation overwrote newer snapshot: %+v payload=%s", snap, snap.Payload)
	}
}

func TestCodexInviteSnapshotDeleteAndPurge(t *testing.T) {
	ctx := context.Background()
	db := newInviteSnapshotTestDB(t)
	now := time.Now().UTC()

	seed := func(accountID int64, expiresAt time.Time) {
		t.Helper()
		if err := db.UpsertCodexInviteSnapshot(ctx, &CodexInviteSnapshot{
			AccountID: accountID, Kind: CodexInviteSnapshotEligibility, Scope: "s",
			CredentialGeneration: 1, Payload: json.RawMessage(`{}`),
			ObservedAt: now.Add(-time.Hour), ExpiresAt: expiresAt,
		}); err != nil {
			t.Fatalf("seed %d: %v", accountID, err)
		}
	}
	seed(11, now.Add(time.Hour))
	seed(12, now.Add(-time.Minute)) // 已过期

	if err := db.DeleteCodexInviteSnapshots(ctx, 11); err != nil {
		t.Fatalf("DeleteCodexInviteSnapshots: %v", err)
	}
	snap, err := db.GetCodexInviteSnapshot(ctx, 11, CodexInviteSnapshotEligibility, "s")
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}
	if snap != nil {
		t.Fatalf("expected row deleted, got %+v", snap)
	}

	purged, err := db.PurgeExpiredCodexInviteSnapshots(ctx, now)
	if err != nil {
		t.Fatalf("PurgeExpiredCodexInviteSnapshots: %v", err)
	}
	if purged != 1 {
		t.Fatalf("expected 1 purged row, got %d", purged)
	}
	if snap, err = db.GetCodexInviteSnapshot(ctx, 12, CodexInviteSnapshotEligibility, "s"); err != nil || snap != nil {
		t.Fatalf("expired row should be gone: %v %+v", err, snap)
	}
}
