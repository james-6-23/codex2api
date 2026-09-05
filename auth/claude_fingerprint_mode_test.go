package auth

import (
	"testing"
)

func TestParseClaudeConfig_CLIVersionSyncDefaults(t *testing.T) {
	cfg := ParseClaudeConfig(`{"fingerprint_mode":"force"}`)
	if !cfg.CLIVersionSyncEnabledValue() {
		t.Fatal("missing cli_version_sync_enabled must default to true")
	}
	if cfg.CLIVersionSyncIntervalHours != 12 {
		t.Fatalf("interval = %d, want 12", cfg.CLIVersionSyncIntervalHours)
	}
	cfg = ParseClaudeConfig(`{"cli_version_sync_enabled":false,"cli_version_sync_interval_hours":9999}`)
	if cfg.CLIVersionSyncEnabledValue() {
		t.Fatal("explicit false must be honored")
	}
	if cfg.CLIVersionSyncIntervalHours != 720 {
		t.Fatalf("interval = %d, want 720 clamp", cfg.CLIVersionSyncIntervalHours)
	}
}

func TestStore_ClaudeCLIVersionSyncAccessors(t *testing.T) {
	s := NewStore(nil, nil, nil)
	defer s.Stop()
	if !s.ClaudeCLIVersionSyncEnabled() || s.ClaudeCLIVersionSyncIntervalHours() != 12 {
		t.Fatalf("defaults: enabled=%v hours=%d", s.ClaudeCLIVersionSyncEnabled(), s.ClaudeCLIVersionSyncIntervalHours())
	}
	s.SetClaudeCLIVersionSync(false, 0)
	if s.ClaudeCLIVersionSyncEnabled() || s.ClaudeCLIVersionSyncIntervalHours() != 12 {
		t.Fatal("disabled + zero interval should read false/12")
	}
	applyClaudeConfigToStore(s, `{"cli_version_sync_enabled":true,"cli_version_sync_interval_hours":6}`)
	if !s.ClaudeCLIVersionSyncEnabled() || s.ClaudeCLIVersionSyncIntervalHours() != 6 {
		t.Fatal("applyClaudeConfigToStore must publish sync settings")
	}
}
