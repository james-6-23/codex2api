package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
)

const schedulerOutboxClaudeConfigFixture = `{"fingerprint_mode":"force","session_window_limit":7,"client_platform":"claude_code_cli_only","version_policy":"minimum","client_version":"2.1.251"}`

func persistSchedulerOutboxRuntimeSettings(t *testing.T, db *database.DB, engine string) {
	t.Helper()
	settings := &database.SystemSettings{
		MaxConcurrency:               1,
		TestConcurrency:              1,
		TestModel:                    "gpt-5.4",
		SchedulerEngine:              engine,
		SessionWindowBalanceEnabled:  true,
		PassiveInternalModelsEnabled: true,
		ClaudeConfig:                 schedulerOutboxClaudeConfigFixture,
	}
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	if err := db.UpdateClaudeConfig(context.Background(), schedulerOutboxClaudeConfigFixture); err != nil {
		t.Fatalf("UpdateClaudeConfig: %v", err)
	}
}

func assertSchedulerOutboxRuntimeSettings(t *testing.T, store *Store) {
	t.Helper()
	if !store.SessionWindowBalanceEnabled() || !store.PassiveInternalModelsEnabled() {
		t.Fatalf("runtime flags = balance:%t passive:%t, want true/true", store.SessionWindowBalanceEnabled(), store.PassiveInternalModelsEnabled())
	}
	policy := store.ClaudeClientPolicy()
	if policy.Platform != ClaudeClientPlatformCLIOnly || policy.VersionPolicy != ClaudeVersionPolicyMinimum || policy.ClientVersion != "2.1.251" {
		t.Fatalf("Claude client policy = %+v", policy)
	}
	if got := store.ClaudeFingerprintModeDefault(); got != ClaudeFingerprintModeForce {
		t.Fatalf("Claude fingerprint mode = %q, want %q", got, ClaudeFingerprintModeForce)
	}
	if got := store.ClaudeSessionWindowLimit(); got != 7 {
		t.Fatalf("Claude session window = %d, want 7", got)
	}
}

func TestReloadSchedulerSettingsAppliesPersistedRuntimePolicies(t *testing.T) {
	t.Setenv("CODEX_SCHEDULER_ENGINE", "")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "scheduler-settings.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	persistSchedulerOutboxRuntimeSettings(t, db, "indexed")

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "legacy"})
	t.Cleanup(store.Stop)
	if err := store.reloadSchedulerSettings(context.Background()); err != nil {
		t.Fatalf("reloadSchedulerSettings: %v", err)
	}
	if got := store.SchedulerEngine(); got != "indexed" {
		t.Fatalf("scheduler engine = %q, want indexed", got)
	}
	assertSchedulerOutboxRuntimeSettings(t, store)
}

func TestReloadSchedulerSettingsEngineOverrideStillReloadsOtherPolicies(t *testing.T) {
	t.Setenv("CODEX_SCHEDULER_ENGINE", "legacy")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "scheduler-settings-env.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	persistSchedulerOutboxRuntimeSettings(t, db, "indexed")

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "legacy"})
	t.Cleanup(store.Stop)
	if err := store.reloadSchedulerSettings(context.Background()); err != nil {
		t.Fatalf("reloadSchedulerSettings: %v", err)
	}
	if got := store.SchedulerEngine(); got != "legacy" {
		t.Fatalf("scheduler engine = %q, want env override legacy", got)
	}
	assertSchedulerOutboxRuntimeSettings(t, store)
}
