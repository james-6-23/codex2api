package auth

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestApplyOpenAIResponsesIdentityChangeRecoversRuntimeAccount(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	acc := &Account{
		DBID:                 1,
		CredentialGeneration: 1,
		UpstreamType:         UpstreamOpenAIResponses,
		BaseURL:              "https://relay.example",
		APIKey:               "sk-old",
		Models:               []string{"gpt-5.6"},
		PlanType:             "api",
		Status:               StatusError,
		ErrorMsg:             "old credential rejected",
		HealthTier:           HealthTierBanned,
		FailureStreak:        3,
		LastUnauthorizedAt:   time.Now(),
	}
	atomic.StoreInt32(&acc.Disabled, 1)
	store.AddAccount(acc)
	store.MarkModelCooldown(acc, "gpt-5.6", time.Hour, "rate_limited")

	if !store.ApplyOpenAIResponsesConfig(acc.DBID, acc.BaseURL, "", []string{"gpt-5.6", "gpt-5.6-mini"}, "", "auto", "") {
		t.Fatal("config-only ApplyOpenAIResponsesConfig returned false")
	}
	if atomic.LoadInt32(&acc.Disabled) == 0 || acc.IsAvailable() {
		t.Fatal("config-only update unexpectedly cleared old identity failure state")
	}

	if !store.ApplyOpenAIResponsesConfig(acc.DBID, acc.BaseURL, "sk-new", acc.Models, "", "auto", "") {
		t.Fatal("identity ApplyOpenAIResponsesConfig returned false")
	}
	if atomic.LoadInt32(&acc.Disabled) != 0 || !acc.IsAvailable() {
		t.Fatal("identity update did not restore runtime availability")
	}
	if got := len(acc.ActiveModelCooldowns()); got != 0 {
		t.Fatalf("active model cooldown count = %d, want 0", got)
	}
	baseURL, apiKey := acc.OpenAIResponsesCredentials()
	if baseURL != "https://relay.example" || apiKey != "sk-new" {
		t.Fatalf("runtime credentials = (%q, %q), want corrected endpoint", baseURL, apiKey)
	}
	selected := store.Next()
	if selected == nil || selected.ID() != acc.DBID {
		t.Fatal("corrected endpoint was not immediately schedulable")
	}
	store.Release(selected)
}

func TestReconcileDispatchStateReloadsChangedResponsesIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "responses-reconcile.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "relay", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://relay.example",
		"api_key":       "sk-old",
		"models":        []string{"gpt-5.6"},
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	// Init 启动的 outbox 消费者会异步投影下面这条更新,抢在显式对账之前把
	// 变更吃掉。这里测的是对账路径本身,先停掉后台消费者保证判定确定。
	store.Stop()
	acc := store.FindByID(accountID)
	if acc == nil {
		t.Fatal("runtime account missing after Init")
	}
	store.MarkCooldownWithError(acc, time.Hour, "unauthorized", "old credential rejected")
	atomic.StoreInt32(&acc.Disabled, 1)
	if err := db.UpdateOpenAIResponsesAccount(ctx, accountID, "relay", map[string]interface{}{"api_key": "sk-new"}, ""); err != nil {
		t.Fatalf("UpdateOpenAIResponsesAccount: %v", err)
	}

	_, err = store.ReconcileDispatchState(ctx)
	if err != nil {
		t.Fatalf("ReconcileDispatchState: %v", err)
	}
	baseURL, apiKey := acc.OpenAIResponsesCredentials()
	if baseURL != "https://relay.example" || apiKey != "sk-new" {
		t.Fatalf("reconciled credentials = (%q, %q), want corrected endpoint", baseURL, apiKey)
	}
	selected := store.Next()
	if selected == nil || selected.ID() != accountID {
		t.Fatal("reconciled endpoint was not schedulable")
	}
	store.Release(selected)
}

func TestApplyOpenAIResponsesConfigUsesPersistedAPIKeySemantics(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "responses-empty-key.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "relay", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://relay.example",
		"api_key":       "sk-old",
		"models":        []string{"gpt-5.6"},
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	// 清空 api_key 后该行不再能构造运行时账号,Init 启动的 outbox 消费者一旦
	// 先于下面的调用轮询到这条更新就会把账号摘掉,ApplyOpenAIResponsesConfig
	// 随之返回 false。这里测的是同步应用路径,先停掉后台消费者。
	store.Stop()
	if err := db.UpdateOpenAIResponsesAccount(ctx, accountID, "relay", map[string]interface{}{
		"models": []string{"gpt-5.6", "gpt-5.6-mini"},
	}, ""); err != nil {
		t.Fatalf("config-only UpdateOpenAIResponsesAccount: %v", err)
	}
	if !store.ApplyOpenAIResponsesConfig(accountID, "https://relay.example", "", []string{"gpt-5.6", "gpt-5.6-mini"}, "", "auto", "") {
		t.Fatal("config-only ApplyOpenAIResponsesConfig returned false")
	}
	acc := store.FindByID(accountID)
	if acc == nil {
		t.Fatal("runtime account missing")
	}
	_, apiKey := acc.OpenAIResponsesCredentials()
	if apiKey != "sk-old" {
		t.Fatalf("runtime API key after omitted-key update = %q, want sk-old", apiKey)
	}

	if err := db.UpdateOpenAIResponsesAccount(ctx, accountID, "relay", map[string]interface{}{"api_key": ""}, ""); err != nil {
		t.Fatalf("UpdateOpenAIResponsesAccount: %v", err)
	}
	if !store.ApplyOpenAIResponsesConfig(accountID, "https://relay.example", "", []string{"gpt-5.6"}, "", "auto", "") {
		t.Fatal("ApplyOpenAIResponsesConfig returned false")
	}

	_, apiKey = acc.OpenAIResponsesCredentials()
	if apiKey != "" {
		t.Fatalf("runtime API key = %q, want empty persisted value", apiKey)
	}
	if got := store.Next(); got != nil {
		store.Release(got)
		t.Fatalf("account with cleared API key remained schedulable: %d", got.ID())
	}
}

func TestOpenAIResponsesAccountIgnoresPersistedCodexUsageSnapshot(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "responses-stale-usage.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "relay", map[string]interface{}{
		"upstream_type":             UpstreamOpenAIResponses,
		"base_url":                  "https://relay.example",
		"api_key":                   "sk-relay",
		"models":                    []string{"gpt-5.6"},
		"codex_7d_used_percent":     88,
		"codex_7d_reset_at":         time.Now().Add(7 * 24 * time.Hour).Format(time.RFC3339),
		"codex_7d_window_seconds":   604800,
		"codex_usage_updated_at":    time.Now().Format(time.RFC3339),
		"codex_5h_used_percent":     77,
		"codex_5h_reset_at":         time.Now().Add(5 * time.Hour).Format(time.RFC3339),
		"codex_5h_usage_updated_at": time.Now().Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	account := store.FindByID(accountID)
	if account == nil {
		t.Fatal("runtime relay account missing")
	}
	if _, ok := account.GetUsagePercent7d(); ok {
		t.Fatal("historical relay 7d snapshot was hydrated")
	}
	if _, ok := account.GetUsagePercent5h(); ok {
		t.Fatal("historical relay 5h snapshot was hydrated")
	}
	if !account.GetReset7dAt().IsZero() || account.GetWindow7dSeconds() != 0 {
		t.Fatal("historical relay reset metadata was hydrated")
	}
}
