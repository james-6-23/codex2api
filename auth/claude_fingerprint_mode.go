package auth

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"
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

// CLIVersionSyncEnabledValue 把缺失字段解释为开启，避免老配置静默关闭同步。
func (c ClaudeConfig) CLIVersionSyncEnabledValue() bool {
	return c.CLIVersionSyncEnabled == nil || *c.CLIVersionSyncEnabled
}

// DefaultClaudeFirstTokenTimeoutSeconds 是 Claude OAuth 路径首字超时的默认值：
// 长推理（effort xhigh、~150k 上下文）正常也会在 1~2 分钟内吐出首个 thinking delta，
// 超过这个时间基本是上游卡死，继续等只会让并发位被僵尸请求占住。
const DefaultClaudeFirstTokenTimeoutSeconds = 120

// MaxClaudeFirstTokenTimeoutSeconds 与全局 first_token_timeout_seconds 的上限保持一致。
const MaxClaudeFirstTokenTimeoutSeconds = 600

// NormalizeClaudeFirstTokenTimeoutSeconds 把配置值钳到 [0,600]；nil（老配置缺失）取默认 120，
// 负数视为 0（跟随全局）。
func NormalizeClaudeFirstTokenTimeoutSeconds(seconds *int) int {
	if seconds == nil {
		return DefaultClaudeFirstTokenTimeoutSeconds
	}
	if *seconds <= 0 {
		return 0
	}
	if *seconds > MaxClaudeFirstTokenTimeoutSeconds {
		return MaxClaudeFirstTokenTimeoutSeconds
	}
	return *seconds
}

// FirstTokenTimeoutSecondsValue 返回归一化后的 Claude 首字超时秒数（0=跟随全局）。
func (c ClaudeConfig) FirstTokenTimeoutSecondsValue() int {
	return NormalizeClaudeFirstTokenTimeoutSeconds(c.FirstTokenTimeoutSeconds)
}

// StreamKeepaliveEnabledValue 把缺失字段解释为开启。
func (c ClaudeConfig) StreamKeepaliveEnabledValue() bool {
	return c.StreamKeepaliveEnabled == nil || *c.StreamKeepaliveEnabled
}

// SetClaudeFirstTokenTimeoutSeconds 发布 Claude 路径首字超时（0=跟随全局）。
func (s *Store) SetClaudeFirstTokenTimeoutSeconds(seconds int) {
	if s == nil {
		return
	}
	if seconds < 0 {
		seconds = 0
	}
	if seconds > MaxClaudeFirstTokenTimeoutSeconds {
		seconds = MaxClaudeFirstTokenTimeoutSeconds
	}
	s.claudeFirstTokenTimeoutSec.Store(int64(seconds))
	s.claudeFirstTokenTimeoutSet.Store(true)
}

// ClaudeFirstTokenTimeoutSeconds 返回 Claude 路径首字超时秒数；从未设置时取默认 120。
func (s *Store) ClaudeFirstTokenTimeoutSeconds() int {
	if s == nil {
		return DefaultClaudeFirstTokenTimeoutSeconds
	}
	if !s.claudeFirstTokenTimeoutSet.Load() {
		return DefaultClaudeFirstTokenTimeoutSeconds
	}
	return int(s.claudeFirstTokenTimeoutSec.Load())
}

// ClaudeFirstTokenTimeout 返回 Claude 路径首字超时时长；0 表示跟随全局设置。
func (s *Store) ClaudeFirstTokenTimeout() time.Duration {
	return time.Duration(s.ClaudeFirstTokenTimeoutSeconds()) * time.Second
}

// SetClaudeStreamKeepaliveEnabled 发布 Claude 流式首字前 SSE 保活开关。
func (s *Store) SetClaudeStreamKeepaliveEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.claudeStreamKeepaliveDisabled.Store(!enabled)
}

// ClaudeStreamKeepaliveEnabled 报告 Claude 流式首字前 SSE 保活是否开启（零值=开启）。
func (s *Store) ClaudeStreamKeepaliveEnabled() bool {
	return s != nil && !s.claudeStreamKeepaliveDisabled.Load()
}

// NormalizeClaudeCLIVersionSyncIntervalHours 钳到 [1,720]，0/负数视为默认 12。
func NormalizeClaudeCLIVersionSyncIntervalHours(hours int) int {
	if hours <= 0 {
		return 12
	}
	if hours > 720 {
		return 720
	}
	return hours
}

func (s *Store) SetClaudeCLIVersionSync(enabled bool, intervalHours int) {
	if s == nil {
		return
	}
	s.claudeCLIVersionSyncDisabled.Store(!enabled)
	s.claudeCLIVersionSyncIntervalH.Store(int64(NormalizeClaudeCLIVersionSyncIntervalHours(intervalHours)))
}

func (s *Store) ClaudeCLIVersionSyncEnabled() bool {
	return s != nil && !s.claudeCLIVersionSyncDisabled.Load()
}

func (s *Store) ClaudeCLIVersionSyncIntervalHours() int {
	if s == nil {
		return 12
	}
	return NormalizeClaudeCLIVersionSyncIntervalHours(int(s.claudeCLIVersionSyncIntervalH.Load()))
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
	FingerprintMode             string `json:"fingerprint_mode"`                          // preserve / force(空=preserve)
	DefaultTimezone             string `json:"default_timezone"`                          // 导入账号默认 IANA 时区
	SessionWindowLimit          int64  `json:"session_window_limit"`                      // 默认并发会话窗口数(0=跟随全局 maxConcurrency)
	CLIVersionSyncEnabled       *bool  `json:"cli_version_sync_enabled,omitempty"`        // 缺失=true
	CLIVersionSyncIntervalHours int    `json:"cli_version_sync_interval_hours,omitempty"` // 0=12，钳 [1,720]
	// FirstTokenTimeoutSeconds 是 Claude OAuth 路径专用的首字超时（秒）。缺失=默认
	// DefaultClaudeFirstTokenTimeoutSeconds；显式 0=跟随全局 first_token_timeout_seconds。
	FirstTokenTimeoutSeconds *int `json:"first_token_timeout_seconds,omitempty"`
	// StreamKeepaliveEnabled 控制 Claude 流式请求在首字前是否向下游发 SSE 保活注释。缺失=开启。
	StreamKeepaliveEnabled *bool `json:"stream_keepalive_enabled,omitempty"`
	ClaudeClientPolicy
	ClaudeSecurityConfig
}

// SetClaudeClientPolicy publishes the global Claude Code platform/version
// policy as one snapshot so readers never observe a half-updated policy.
// Invalid values fall back to any/passthrough so malformed legacy settings
// cannot break startup; the admin endpoint performs strict validation first.
func (s *Store) SetClaudeClientPolicy(policy ClaudeClientPolicy) {
	if s == nil {
		return
	}
	normalized, err := NormalizeClaudeClientPolicy(policy)
	if err != nil {
		normalized = DefaultClaudeClientPolicy()
	}
	s.claudeClientPolicy.Store(normalized)
}

// ClaudeClientPolicy returns the normalized global policy.
func (s *Store) ClaudeClientPolicy() ClaudeClientPolicy {
	if s != nil {
		if value, ok := s.claudeClientPolicy.Load().(ClaudeClientPolicy); ok {
			if normalized, err := NormalizeClaudeClientPolicy(value); err == nil {
				return normalized
			}
		}
	}
	return DefaultClaudeClientPolicy()
}

func (s *Store) ClaudeClientPlatform() ClaudeClientPlatform {
	return s.ClaudeClientPolicy().Platform
}

func (s *Store) ClaudeVersionPolicy() ClaudeVersionPolicy {
	return s.ClaudeClientPolicy().VersionPolicy
}

func (s *Store) ClaudeClientVersion() string {
	return s.ClaudeClientPolicy().ClientVersion
}

// ClaudeClientPolicyForAccount merges an account override over the global
// policy. Empty account fields intentionally inherit global settings. When the
// merged result is invalid (for example a stale minimum/fixed override whose
// version was cleared) the account keeps the already-valid global policy
// instead of silently dropping a platform restriction.
func (s *Store) ClaudeClientPolicyForAccount(account *Account) ClaudeClientPolicy {
	global := s.ClaudeClientPolicy()
	if account == nil {
		return global
	}
	policy := global
	account.mu.RLock()
	if account.ClaudeClientPlatformOverride != "" {
		policy.Platform = ClaudeClientPlatform(account.ClaudeClientPlatformOverride)
	}
	if account.ClaudeVersionPolicyOverride != "" {
		policy.VersionPolicy = ClaudeVersionPolicy(account.ClaudeVersionPolicyOverride)
	}
	if account.ClaudeClientVersionOverride != "" {
		policy.ClientVersion = account.ClaudeClientVersionOverride
	}
	account.mu.RUnlock()
	if normalized, err := NormalizeClaudeClientPolicy(policy); err == nil {
		return normalized
	}
	return global
}

// ApplyAccountClaudeClientPolicy updates an in-memory override after the
// credentials mutation has been committed.
func (s *Store) ApplyAccountClaudeClientPolicy(dbID int64, policy ClaudeClientPolicy) bool {
	account := s.FindByID(dbID)
	if account == nil {
		return false
	}
	account.mu.Lock()
	account.ClaudeClientPlatformOverride = string(policy.Platform)
	account.ClaudeVersionPolicyOverride = string(policy.VersionPolicy)
	account.ClaudeClientVersionOverride = policy.ClientVersion
	account.mu.Unlock()
	return true
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
	cfg.CLIVersionSyncIntervalHours = NormalizeClaudeCLIVersionSyncIntervalHours(cfg.CLIVersionSyncIntervalHours)
	normalizedTimeout := NormalizeClaudeFirstTokenTimeoutSeconds(cfg.FirstTokenTimeoutSeconds)
	cfg.FirstTokenTimeoutSeconds = &normalizedTimeout
	if clientPolicy, err := NormalizeClaudeClientPolicy(cfg.ClaudeClientPolicy); err == nil {
		cfg.ClaudeClientPolicy = clientPolicy
	} else {
		cfg.ClaudeClientPolicy = DefaultClaudeClientPolicy()
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
	s.SetClaudeCLIVersionSync(cfg.CLIVersionSyncEnabledValue(), cfg.CLIVersionSyncIntervalHours)
	s.SetClaudeFirstTokenTimeoutSeconds(cfg.FirstTokenTimeoutSecondsValue())
	s.SetClaudeStreamKeepaliveEnabled(cfg.StreamKeepaliveEnabledValue())
	s.SetClaudeClientPolicy(cfg.ClaudeClientPolicy)
	s.SetClaudeSecurityConfig(cfg.SecurityConfig())
}
