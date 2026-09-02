package auth

import (
	"encoding/json"
	"strings"
)

const (
	GrokQualityGuardOnExhaustedFailClosed = "fail_closed"
	GrokQualityGuardOnExhaustedFailOpen   = "fail_open"

	GrokQualityGuardDefaultMaxAttempts    = 6
	GrokQualityGuardDefaultHoldTimeoutSec = 30
	GrokQualityGuardDefaultCooldownHours  = 12

	GrokQualityGuardMaxAttemptsCap    = 20
	GrokQualityGuardHoldTimeoutSecCap = 300
	GrokQualityGuardCooldownHoursCap  = 168
)

// GrokQualityGuardConfig 是 Grok 降智检测(缺失思考换号重试)的系统设置。
// 默认关闭;开启后仅对推理模型的流式请求生效,判定降智即换号并冷却账号。
type GrokQualityGuardConfig struct {
	Enabled              bool   `json:"quality_guard_enabled"`
	MaxAttempts          int    `json:"quality_guard_max_attempts"`
	HoldTimeoutSec       int    `json:"quality_guard_hold_timeout_sec"`
	OnExhausted          string `json:"quality_guard_on_exhausted"`
	AccountCooldownHours int    `json:"quality_guard_account_cooldown_hours"`
}

func DefaultGrokQualityGuardConfig() GrokQualityGuardConfig {
	return GrokQualityGuardConfig{
		Enabled:              false,
		MaxAttempts:          GrokQualityGuardDefaultMaxAttempts,
		HoldTimeoutSec:       GrokQualityGuardDefaultHoldTimeoutSec,
		OnExhausted:          GrokQualityGuardOnExhaustedFailClosed,
		AccountCooldownHours: GrokQualityGuardDefaultCooldownHours,
	}
}

func NormalizeGrokQualityGuardConfig(cfg GrokQualityGuardConfig) GrokQualityGuardConfig {
	defaults := DefaultGrokQualityGuardConfig()
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaults.MaxAttempts
	}
	if cfg.MaxAttempts > GrokQualityGuardMaxAttemptsCap {
		cfg.MaxAttempts = GrokQualityGuardMaxAttemptsCap
	}
	if cfg.HoldTimeoutSec <= 0 {
		cfg.HoldTimeoutSec = defaults.HoldTimeoutSec
	}
	if cfg.HoldTimeoutSec > GrokQualityGuardHoldTimeoutSecCap {
		cfg.HoldTimeoutSec = GrokQualityGuardHoldTimeoutSecCap
	}
	if cfg.AccountCooldownHours <= 0 {
		cfg.AccountCooldownHours = defaults.AccountCooldownHours
	}
	if cfg.AccountCooldownHours > GrokQualityGuardCooldownHoursCap {
		cfg.AccountCooldownHours = GrokQualityGuardCooldownHoursCap
	}
	if strings.EqualFold(strings.TrimSpace(cfg.OnExhausted), GrokQualityGuardOnExhaustedFailOpen) {
		cfg.OnExhausted = GrokQualityGuardOnExhaustedFailOpen
	} else {
		cfg.OnExhausted = GrokQualityGuardOnExhaustedFailClosed
	}
	return cfg
}

func GrokQualityGuardConfigFromJSON(raw string) GrokQualityGuardConfig {
	cfg := DefaultGrokQualityGuardConfig()
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return cfg
	}
	var parsed struct {
		Enabled              *bool  `json:"quality_guard_enabled"`
		MaxAttempts          *int   `json:"quality_guard_max_attempts"`
		HoldTimeoutSec       *int   `json:"quality_guard_hold_timeout_sec"`
		OnExhausted          string `json:"quality_guard_on_exhausted"`
		AccountCooldownHours *int   `json:"quality_guard_account_cooldown_hours"`
	}
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return cfg
	}
	if parsed.Enabled != nil {
		cfg.Enabled = *parsed.Enabled
	}
	if parsed.MaxAttempts != nil {
		cfg.MaxAttempts = *parsed.MaxAttempts
	}
	if parsed.HoldTimeoutSec != nil {
		cfg.HoldTimeoutSec = *parsed.HoldTimeoutSec
	}
	if strings.TrimSpace(parsed.OnExhausted) != "" {
		cfg.OnExhausted = parsed.OnExhausted
	}
	if parsed.AccountCooldownHours != nil {
		cfg.AccountCooldownHours = *parsed.AccountCooldownHours
	}
	return NormalizeGrokQualityGuardConfig(cfg)
}
