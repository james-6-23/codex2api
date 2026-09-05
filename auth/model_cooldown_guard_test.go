package auth

import (
	"context"
	"testing"
	"time"
)

func TestMarkModelCooldownWithBackoff_DoesNotShortenActiveLongerCooldown(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	acc := &Account{DBID: 251, UpstreamType: UpstreamClaude}

	long := store.MarkModelCooldownWithBackoff(acc, "claude-fable-5-1", 30*time.Minute, "credits_required", false)
	short := store.MarkModelCooldownWithBackoff(acc, "claude-fable-5-1", 2*time.Second, "rate_limited_model", true)

	if short.ResetAt.Before(long.ResetAt) {
		t.Fatalf("a later short cooldown must not shorten the active 30m one: short=%s long=%s", short.ResetAt, long.ResetAt)
	}
	if short.Reason != "credits_required" {
		t.Fatalf("reason must stay the longer cooldown's reason, got %q", short.Reason)
	}
	acc.mu.RLock()
	stored := acc.ModelCooldowns["claude-fable-5-1"]
	acc.mu.RUnlock()
	if stored.ResetAt.Before(long.ResetAt) || stored.Reason != "credits_required" {
		t.Fatalf("stored cooldown = %+v", stored)
	}
}

func TestMarkModelCooldownWithBackoff_ExtendsExpiredCooldown(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	acc := &Account{DBID: 251, UpstreamType: UpstreamClaude, ModelCooldowns: map[string]ModelCooldown{
		"claude-fable-5-1": {Model: "claude-fable-5-1", Reason: "credits_required", ResetAt: time.Now().Add(-time.Minute)},
	}}
	got := store.MarkModelCooldownWithBackoff(acc, "claude-fable-5-1", 2*time.Second, "rate_limited_model", true)
	if got.Reason != "rate_limited_model" || !got.ResetAt.After(time.Now()) {
		t.Fatalf("an expired cooldown must be replaced normally, got %+v", got)
	}
}

func TestDropAccountModel(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	acc := &Account{DBID: 251, UpstreamType: UpstreamClaude, Models: []string{"claude-fable-5-1", "claude-opus-5", "Claude-Fable-5"}}
	store.mu.Lock()
	store.accounts = []*Account{acc}
	store.mu.Unlock()

	removed, err := store.DropAccountModel(context.Background(), acc, "claude-fable-5-1")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	acc.mu.RLock()
	models := append([]string(nil), acc.Models...)
	acc.mu.RUnlock()
	if len(models) != 2 || models[0] != "claude-opus-5" || models[1] != "Claude-Fable-5" {
		t.Fatalf("models after drop = %v", models)
	}

	removed, err = store.DropAccountModel(context.Background(), acc, "claude-sonnet-5")
	if err != nil || removed {
		t.Fatalf("model not in whitelist must be a no-op: removed=%v err=%v", removed, err)
	}

	open := &Account{DBID: 252, UpstreamType: UpstreamClaude}
	removed, err = store.DropAccountModel(context.Background(), open, "claude-fable-5-1")
	if err != nil || removed {
		t.Fatalf("empty whitelist (allow-all) must not be rewritten: removed=%v err=%v", removed, err)
	}
}
