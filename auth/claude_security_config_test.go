package auth

import "testing"

func TestNormalizeClaudeSecurityConfigUsesSafeDefaults(t *testing.T) {
	cfg := NormalizeClaudeSecurityConfig(ClaudeSecurityConfig{})
	if cfg.MaxOutputTokens != 0 || cfg.MaxToolCount != 0 || cfg.MaxToolSchemaBytes != 0 {
		t.Fatalf("zero values should mean no application cap: %+v", cfg)
	}
	if len(cfg.AllowedBetaHeaders) != 0 {
		t.Fatalf("empty beta allowlist should stay empty: %v", cfg.AllowedBetaHeaders)
	}
}

func TestNormalizeClaudeSecurityConfigCanonicalizesBetaAllowlistAndKeepsExplicitLimits(t *testing.T) {
	cfg := NormalizeClaudeSecurityConfig(ClaudeSecurityConfig{
		AllowedBetaHeaders: []string{" Foo-Bar ", "foo-bar", "bad value", "oauth-2025-04-20"},
		MaxOutputTokens:    999999,
		MaxToolCount:       999,
		MaxToolSchemaBytes: 99999999,
	})
	if len(cfg.AllowedBetaHeaders) != 2 || cfg.AllowedBetaHeaders[0] != "foo-bar" || cfg.AllowedBetaHeaders[1] != "oauth-2025-04-20" {
		t.Fatalf("normalized beta allowlist = %v", cfg.AllowedBetaHeaders)
	}
	if cfg.MaxOutputTokens != 999999 || cfg.MaxToolCount != 999 || cfg.MaxToolSchemaBytes != 99999999 {
		t.Fatalf("explicit compatibility limits should remain operator values: %+v", cfg)
	}
	unlimited := NormalizeClaudeSecurityConfig(ClaudeSecurityConfig{
		MaxOutputTokens:    -1,
		MaxToolCount:       -1,
		MaxToolSchemaBytes: -1,
	})
	if unlimited.MaxOutputTokens != 0 || unlimited.MaxToolCount != 0 || unlimited.MaxToolSchemaBytes != 0 {
		t.Fatalf("negative values should normalize to unlimited zero values: %+v", unlimited)
	}
}

func TestParseClaudeConfigKeepsLegacyFieldsAndSecurityDefaults(t *testing.T) {
	cfg := ParseClaudeConfig(`{"fingerprint_mode":"force","default_timezone":"Asia/Shanghai","session_window_limit":4,"allow_service_tier":true,"allowed_beta_headers":["beta-x"]}`)
	if cfg.FingerprintMode != ClaudeFingerprintModeForce || cfg.DefaultTimezone != "Asia/Shanghai" || cfg.SessionWindowLimit != 4 {
		t.Fatalf("legacy Claude config fields changed: %+v", cfg)
	}
	security := cfg.SecurityConfig()
	if !security.AllowServiceTier || len(security.AllowedBetaHeaders) != 1 || security.MaxOutputTokens != 0 || security.MaxToolCount != 0 || security.MaxToolSchemaBytes != 0 {
		t.Fatalf("security config parse = %+v", security)
	}
}
