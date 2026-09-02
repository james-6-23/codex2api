package auth

import (
	"encoding/json"
	"strings"
	"sync/atomic"
)

// Claude Code 出站请求的指纹收敛模式(账号级;空值 = 跟随全局默认):
//
//	preserve — 入站真实客户端身份头优先,缺失才用账号绑定指纹补齐(历史默认行为)。
//	force    — 无条件用账号绑定指纹覆盖入站身份头(强制替换,保证同一账号
//	           对 Anthropic 始终呈现同一套 Claude Code 身份)。
const (
	ClaudeFingerprintModePreserve = "preserve"
	ClaudeFingerprintModeForce    = "force"
)

// ClaudeFingerprintModeCredentialKey 是该模式在账号 credentials 中的存储键。
const ClaudeFingerprintModeCredentialKey = "claude_fingerprint_mode"

// ClaudeSecurityConfig 是 ClaudeCode 出站请求的安全边界。
// 布尔字段默认 false（默认过滤敏感字段）；数值字段为 0 时表示不设置
// Codex2API 应用层上限，仍受请求体、整数和 Anthropic 上游能力约束。
// AllowedBetaHeaders 只允许额外的 Beta token，OAuth 必需 token 由 proxy 始终注入。
type ClaudeSecurityConfig struct {
	AllowServiceTier      bool     `json:"allow_service_tier"`
	AllowInferenceGeo     bool     `json:"allow_inference_geo"`
	AllowSpeed            bool     `json:"allow_speed"`
	AllowSafetyIdentifier bool     `json:"allow_safety_identifier"`
	AllowedBetaHeaders    []string `json:"allowed_beta_headers"`
	MaxOutputTokens       int64    `json:"max_output_tokens"`
	MaxToolCount          int      `json:"max_tool_count"`
	MaxToolSchemaBytes    int64    `json:"max_tool_schema_bytes"`
}

// DefaultClaudeSecurityConfig returns compatibility-safe defaults used when an
// older installation has no Claude resource-limit fields persisted yet.
func DefaultClaudeSecurityConfig() ClaudeSecurityConfig {
	return ClaudeSecurityConfig{}
}

func validClaudeBetaToken(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (i > 0 && strings.ContainsRune("._-", r)) {
			continue
		}
		return false
	}
	return true
}

// NormalizeClaudeSecurityConfig canonicalizes operator-provided values and
// keeps zero as the explicit "no application cap" sentinel. Negative values
// are never meaningful and normalize to that same sentinel. Integer and body
// size guards remain enforced at the request boundary.
func NormalizeClaudeSecurityConfig(cfg ClaudeSecurityConfig) ClaudeSecurityConfig {
	if cfg.MaxOutputTokens < 0 {
		cfg.MaxOutputTokens = 0
	}
	if cfg.MaxToolCount < 0 {
		cfg.MaxToolCount = 0
	}
	if cfg.MaxToolSchemaBytes < 0 {
		cfg.MaxToolSchemaBytes = 0
	}
	allowed := make([]string, 0, len(cfg.AllowedBetaHeaders))
	seen := make(map[string]struct{}, len(cfg.AllowedBetaHeaders))
	for _, raw := range cfg.AllowedBetaHeaders {
		token := strings.ToLower(strings.TrimSpace(raw))
		if !validClaudeBetaToken(token) {
			continue
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		allowed = append(allowed, token)
	}
	cfg.AllowedBetaHeaders = allowed
	return cfg
}

// NormalizeClaudeFingerprintMode 归一化模式取值;空/非法值归一为空串(跟随全局)。
func NormalizeClaudeFingerprintMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ClaudeFingerprintModePreserve:
		return ClaudeFingerprintModePreserve
	case ClaudeFingerprintModeForce:
		return ClaudeFingerprintModeForce
	}
	return ""
}

// IsValidClaudeFingerprintMode 报告取值是否合法(空串=跟随全局,亦视为合法)。
func IsValidClaudeFingerprintMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ClaudeFingerprintModePreserve, ClaudeFingerprintModeForce:
		return true
	}
	return false
}

// EffectiveClaudeFingerprintMode 返回账号生效模式:账号级覆盖 > 全局默认 > preserve。
func (a *Account) EffectiveClaudeFingerprintMode(globalDefault string) string {
	if a != nil {
		a.mu.RLock()
		mode := a.ClaudeFingerprintMode
		a.mu.RUnlock()
		if m := NormalizeClaudeFingerprintMode(mode); m != "" {
			return m
		}
	}
	if m := NormalizeClaudeFingerprintMode(globalDefault); m != "" {
		return m
	}
	return ClaudeFingerprintModePreserve
}

// ── Claude 全局配置访问器(来自系统设置 claude_config,ApplySystemSettings 注入) ──

// SetClaudeFingerprintModeDefault 设置 Claude 指纹模式全局默认。
func (s *Store) SetClaudeFingerprintModeDefault(mode string) {
	s.claudeFingerprintDefault.Store(NormalizeClaudeFingerprintMode(mode))
}

// ClaudeFingerprintModeDefault 返回 Claude 指纹模式全局默认(空=preserve)。
func (s *Store) ClaudeFingerprintModeDefault() string {
	if v, ok := s.claudeFingerprintDefault.Load().(string); ok {
		return v
	}
	return ""
}

// SetClaudeDefaultTimezone 设置导入 Claude 账号的默认时区。
func (s *Store) SetClaudeDefaultTimezone(tz string) {
	s.claudeDefaultTimezone.Store(strings.TrimSpace(tz))
}

// ClaudeDefaultTimezone 返回导入 Claude 账号的默认时区(空=不指定)。
func (s *Store) ClaudeDefaultTimezone() string {
	if v, ok := s.claudeDefaultTimezone.Load().(string); ok {
		return v
	}
	return ""
}

// SetClaudeSecurityConfig publishes an immutable copy of the Claude egress
// policy to request handlers without taking a lock on the first-token path.
func (s *Store) SetClaudeSecurityConfig(cfg ClaudeSecurityConfig) {
	if s == nil {
		return
	}
	cfg = NormalizeClaudeSecurityConfig(cfg)
	cfg.AllowedBetaHeaders = append([]string(nil), cfg.AllowedBetaHeaders...)
	s.claudeSecurityConfig.Store(cfg)
}

// ClaudeSecurityConfig returns the current Claude egress policy. A missing
// legacy setting is treated as the secure default configuration.
func (s *Store) ClaudeSecurityConfig() ClaudeSecurityConfig {
	if s == nil {
		return DefaultClaudeSecurityConfig()
	}
	if value, ok := s.claudeSecurityConfig.Load().(ClaudeSecurityConfig); ok {
		value.AllowedBetaHeaders = append([]string(nil), value.AllowedBetaHeaders...)
		return NormalizeClaudeSecurityConfig(value)
	}
	return DefaultClaudeSecurityConfig()
}

// SetClaudeSessionWindowLimit 设置 Claude 账号默认并发会话窗口数(<=0 归 0=跟随全局)。
func (s *Store) SetClaudeSessionWindowLimit(n int64) {
	if n < 0 {
		n = 0
	}
	atomic.StoreInt64(&s.claudeSessionWindowLimit, n)
}

// ClaudeSessionWindowLimit 返回 Claude 账号默认并发会话窗口数(0=跟随全局 maxConcurrency)。
func (s *Store) ClaudeSessionWindowLimit() int64 {
	return atomic.LoadInt64(&s.claudeSessionWindowLimit)
}

// ApplyAccountClaudeFingerprintMode 更新内存态账号的 Claude 指纹模式。
func (s *Store) ApplyAccountClaudeFingerprintMode(dbID int64, mode string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.ClaudeFingerprintMode = NormalizeClaudeFingerprintMode(mode)
	acc.mu.Unlock()
	return true
}

// claudeSessionWindowForRow 仅对 Claude 账号返回全局并发会话窗口默认;其它渠道返回 0。
func claudeSessionWindowForRow(upstreamType string, globalWindow int64) int64 {
	if globalWindow > 0 && strings.EqualFold(strings.TrimSpace(upstreamType), UpstreamClaude) {
		return globalWindow
	}
	return 0
}

// ClaudeConfig 是 ClaudeCode 全局配置(系统设置 claude_config 列反序列化目标)。
// 全体 Claude 账号默认遵守;个体账号可通过编辑覆盖。
type ClaudeConfig struct {
	FingerprintMode    string `json:"fingerprint_mode"`     // preserve / force(空=preserve)
	DefaultTimezone    string `json:"default_timezone"`     // 导入账号默认 IANA 时区
	SessionWindowLimit int64  `json:"session_window_limit"` // 默认并发会话窗口数(0=跟随全局 maxConcurrency)
	ClaudeSecurityConfig
}

// SecurityConfig extracts the flattened Claude security fields from the
// persisted system setting while keeping the legacy top-level fields intact.
func (c ClaudeConfig) SecurityConfig() ClaudeSecurityConfig {
	return NormalizeClaudeSecurityConfig(c.ClaudeSecurityConfig)
}

// ParseClaudeConfig 解析 claude_config JSON;空/非法回落到零值(即全部默认)。
func ParseClaudeConfig(raw string) ClaudeConfig {
	var cfg ClaudeConfig
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return cfg
	}
	_ = json.Unmarshal([]byte(raw), &cfg)
	cfg.FingerprintMode = NormalizeClaudeFingerprintMode(cfg.FingerprintMode)
	cfg.DefaultTimezone = strings.TrimSpace(cfg.DefaultTimezone)
	if cfg.SessionWindowLimit < 0 {
		cfg.SessionWindowLimit = 0
	}
	cfg.ClaudeSecurityConfig = NormalizeClaudeSecurityConfig(cfg.ClaudeSecurityConfig)
	return cfg
}

// applyClaudeConfigToStore 把解析后的 ClaudeCode 全局配置写入 Store 的运行时访问器。
func applyClaudeConfigToStore(s *Store, raw string) {
	cfg := ParseClaudeConfig(raw)
	s.SetClaudeFingerprintModeDefault(cfg.FingerprintMode)
	s.SetClaudeDefaultTimezone(cfg.DefaultTimezone)
	s.SetClaudeSessionWindowLimit(cfg.SessionWindowLimit)
	s.SetClaudeSecurityConfig(cfg.SecurityConfig())
}
