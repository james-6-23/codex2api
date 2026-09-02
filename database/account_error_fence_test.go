package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOwnedAccountErrorDoesNotOverwriteOrClearUnrelatedFence(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "owned-error.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "account", map[string]any{"access_token": "token"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetError(ctx, id, "administrator quarantine"); err != nil {
		t.Fatal(err)
	}
	if applied, err := db.SetOwnedAccountError(ctx, id, "provider fence", "provider fence: denied"); err != nil || applied {
		t.Fatalf("SetOwnedAccountError applied=%v err=%v, want unrelated error preserved", applied, err)
	}
	if cleared, err := db.ClearOwnedAccountError(ctx, id, "provider fence"); err != nil || cleared {
		t.Fatalf("ClearOwnedAccountError cleared=%v err=%v, want unrelated error preserved", cleared, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "error" || row.ErrorMessage != "administrator quarantine" {
		t.Fatalf("unrelated fence changed: status=%q error=%q", row.Status, row.ErrorMessage)
	}
}

func TestClearOwnedAccountErrorDoesNotEraseNewerCooldown(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "owned-error-cooldown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "account", map[string]any{"access_token": "token"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := db.SetOwnedAccountError(ctx, id, "provider fence", "provider fence: denied"); err != nil || !applied {
		t.Fatalf("SetOwnedAccountError applied=%v err=%v", applied, err)
	}
	cooldownUntil := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := db.SetCooldown(ctx, id, "rate_limited", cooldownUntil); err != nil {
		t.Fatal(err)
	}
	if cleared, err := db.ClearOwnedAccountError(ctx, id, "provider fence"); err != nil || !cleared {
		t.Fatalf("ClearOwnedAccountError cleared=%v err=%v, want owned error cleared", cleared, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "active" || row.ErrorMessage != "" || row.CooldownReason != "rate_limited" || !row.CooldownUntil.Valid || !row.CooldownUntil.Time.Equal(cooldownUntil) {
		t.Fatalf("newer cooldown changed: status=%q error=%q reason=%q until=%v", row.Status, row.ErrorMessage, row.CooldownReason, row.CooldownUntil)
	}
}

func TestOwnedAccountErrorCannotReviveSoftDeletedAccount(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "owned-error-deleted.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "account", map[string]any{"access_token": "token"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SoftDeleteAccount(ctx, id); err != nil {
		t.Fatal(err)
	}
	if applied, err := db.SetOwnedAccountError(ctx, id, "provider fence", "provider fence: denied"); err != nil || applied {
		t.Fatalf("SetOwnedAccountError applied=%v err=%v, want deleted account untouched", applied, err)
	}
	if cleared, err := db.ClearOwnedAccountError(ctx, id, "provider fence"); err != nil || cleared {
		t.Fatalf("ClearOwnedAccountError cleared=%v err=%v, want deleted account untouched", cleared, err)
	}
	row, err := db.GetAccountByIDIncludingDeleted(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "deleted" {
		t.Fatalf("soft-deleted account was revived: status=%q", row.Status)
	}
}

func TestOwnedAccountErrorCanUpdateAndClearItsOwnFence(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "owned-error-update.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "account", map[string]any{"access_token": "token"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := db.SetOwnedAccountError(ctx, id, "provider fence", "provider fence: denied"); err != nil || !applied {
		t.Fatalf("first SetOwnedAccountError applied=%v err=%v", applied, err)
	}
	if applied, err := db.SetOwnedAccountError(ctx, id, "provider fence", "provider fence: still denied"); err != nil || !applied {
		t.Fatalf("owned update applied=%v err=%v", applied, err)
	}
	if cleared, err := db.ClearOwnedAccountError(ctx, id, "provider fence"); err != nil || !cleared {
		t.Fatalf("owned clear cleared=%v err=%v", cleared, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "active" || row.ErrorMessage != "" {
		t.Fatalf("owned fence not cleared: status=%q error=%q", row.Status, row.ErrorMessage)
	}
}

func TestOwnedAccountErrorPreservesExistingCooldown(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "owned-error-existing-cooldown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "account", map[string]any{"access_token": "token"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cooldownUntil := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := db.SetCooldown(ctx, id, "rate_limited", cooldownUntil); err != nil {
		t.Fatal(err)
	}
	if applied, err := db.SetOwnedAccountError(ctx, id, "provider fence", "provider fence: denied"); err != nil || !applied {
		t.Fatalf("SetOwnedAccountError applied=%v err=%v", applied, err)
	}
	if cleared, err := db.ClearOwnedAccountError(ctx, id, "provider fence"); err != nil || !cleared {
		t.Fatalf("ClearOwnedAccountError cleared=%v err=%v", cleared, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "active" || row.ErrorMessage != "" || row.CooldownReason != "rate_limited" || !row.CooldownUntil.Valid || !row.CooldownUntil.Time.Equal(cooldownUntil) {
		t.Fatalf("existing cooldown changed: status=%q error=%q reason=%q until=%v", row.Status, row.ErrorMessage, row.CooldownReason, row.CooldownUntil)
	}
}
