package auth

// OpenCode（opencode.ai）账号在账号池中的运行时接线。
//
// 同时支持 OpenCode Go（订阅套餐，/zen/go/v1 + /zen/go/v1/usage）与
// OpenCode Zen（按量付费，/zen/v1）两种接入模式；api_key 复用通用凭据键，
// 自动享有静态加密与列表脱敏，其余专有配置落在 opencode_ 前缀键里。

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/codex2api/database"
)

// UpstreamOpenCode 是 OpenCode 账号的 upstream_type 判别值。
const UpstreamOpenCode = "opencode"

// OpenCode 账号模式。
const (
	OpenCodeModeGo  = "go"
	OpenCodeModeZen = "zen"
)

// OpenCode 支持的三种原生上游协议。
const (
	OpenCodeProtocolChatCompletions = "chat_completions"
	OpenCodeProtocolAnthropic       = "anthropic"
	OpenCodeProtocolResponses       = "responses"
)

// OpenCode 官方默认端点与内置常量。
const (
	DefaultOpenCodeGoBaseURL           = "https://opencode.ai/zen/go/v1"
	DefaultOpenCodeGoAnthropicBaseURL  = "https://opencode.ai/zen/go"
	DefaultOpenCodeZenBaseURL          = "https://opencode.ai/zen/v1"
	DefaultOpenCodeZenAnthropicBaseURL = "https://opencode.ai/zen"
	DefaultOpenCodeTestModel           = "glm-5.3"
	DefaultOpenCodeUserAgentVersion    = "1.19.6"

	OpenCodeUserAgentVersionEnv = "OPENCODE_USER_AGENT_VERSION"

	OpenCodeMaxProtocolRules             = 64
	OpenCodeMaxProtocolRulePatternLength = 128

	// OpenCodeUsageSchedulingTTL 防止陈旧的 Go 额度快照在过期后继续压低调度权重。
	OpenCodeUsageSchedulingTTL = 24 * time.Hour

	// OpenCodeGoUsageMinFetchInterval 是两次成功刷新同一 Go 账号额度的最小间隔。
	OpenCodeGoUsageMinFetchInterval = 5 * time.Minute
	// OpenCodeGoUsageDefaultMaxWait 是活跃账号的自动刷新最大等待窗口。
	OpenCodeGoUsageDefaultMaxWait = 15 * time.Minute
)

// OpenCode 专有凭据键（存于 accounts.credentials JSON）。
const (
	OpenCodeModeCredentialKey             = "opencode_mode"
	OpenCodeAnthropicBaseURLCredentialKey = "opencode_anthropic_base_url"
	OpenCodeProtocolRulesCredentialKey    = "opencode_protocol_rules"
	OpenCodeCatalogModelsCredentialKey    = "opencode_catalog_models"
	OpenCodeUsageCredentialKey            = "opencode_usage"
)

// OpenCode 用量快照状态值。
const (
	OpenCodeUsageStatusOK           = "ok"
	OpenCodeUsageStatusUnauthorized = "unauthorized"
	OpenCodeUsageStatusFailed       = "failed"
)

// OpenCodeProtocolRule 表示一条「模型模式 → 原生上游协议」分流规则。
// Pattern 支持精确模型名或单尾部 * 通配符（如 grok-*、*）；按顺序首条命中生效。
type OpenCodeProtocolRule struct {
	Pattern   string   `json:"pattern"`
	Protocol  string   `json:"protocol"`
	Protocols []string `json:"protocols,omitempty"`
}

// OpenCodeUsageWindow 表示 OpenCode Go 的单个额度窗口（rolling 5h / weekly / monthly）。
type OpenCodeUsageWindow struct {
	Status   string    `json:"status,omitempty"`
	Percent  float64   `json:"percent"`
	ResetsAt time.Time `json:"resets_at,omitempty"`
}

// OpenCodeUsageSnapshot 是持久化的 OpenCode Go 官方额度观测快照。
type OpenCodeUsageSnapshot struct {
	Status        string               `json:"status"`
	Rolling       *OpenCodeUsageWindow `json:"rolling,omitempty"`
	Weekly        *OpenCodeUsageWindow `json:"weekly,omitempty"`
	Monthly       *OpenCodeUsageWindow `json:"monthly,omitempty"`
	FetchedAt     time.Time            `json:"fetched_at,omitempty"`
	LastAttemptAt time.Time            `json:"last_attempt_at,omitempty"`
	HTTPStatus    int                  `json:"http_status,omitempty"`
	LastError     string               `json:"last_error,omitempty"`
}

// OpenCodeCredential 是 OpenCode 账号除通用字段外的专有投影。
type OpenCodeCredential struct {
	Mode             string
	AnthropicBaseURL string
	ProtocolRules    []OpenCodeProtocolRule
	HasCustomRules   bool
	CatalogModels    []string
	Usage            *OpenCodeUsageSnapshot
}

var openCodeUserAgentVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// NormalizeOpenCodeUserAgentVersion 校验三段 semver 版本号，非法返回空串。
func NormalizeOpenCodeUserAgentVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || !openCodeUserAgentVersionPattern.MatchString(version) {
		return ""
	}
	return version
}

// OpenCodeUserAgent 返回发往 opencode.ai 的规范客户端 User-Agent，避免触发 Cloudflare 1010/403。
func OpenCodeUserAgent() string {
	version := NormalizeOpenCodeUserAgentVersion(os.Getenv(OpenCodeUserAgentVersionEnv))
	if version == "" {
		version = DefaultOpenCodeUserAgentVersion
	}
	return "opencode/" + version
}

// NormalizeOpenCodeMode 归一化 OpenCode 账号模式（默认 go）。
func NormalizeOpenCodeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case OpenCodeModeZen:
		return OpenCodeModeZen
	default:
		return OpenCodeModeGo
	}
}

// IsValidOpenCodeMode 判断模式字符串是否合法（空串按默认 go 允许）。
func IsValidOpenCodeMode(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", OpenCodeModeGo, OpenCodeModeZen:
		return true
	default:
		return false
	}
}

// DefaultOpenCodeBaseURL 返回指定模式默认的 Chat Completions / Responses / Models 基址。
func DefaultOpenCodeBaseURL(mode string) string {
	if NormalizeOpenCodeMode(mode) == OpenCodeModeZen {
		return DefaultOpenCodeZenBaseURL
	}
	return DefaultOpenCodeGoBaseURL
}

// DefaultOpenCodeAnthropicBaseURL 返回指定模式默认的 Anthropic Messages 基址。
func DefaultOpenCodeAnthropicBaseURL(mode string) string {
	if NormalizeOpenCodeMode(mode) == OpenCodeModeZen {
		return DefaultOpenCodeZenAnthropicBaseURL
	}
	return DefaultOpenCodeGoAnthropicBaseURL
}

// NormalizeOpenCodeBaseURL 校验并归一 OpenCode 上游基址（空串允许，代表使用官方默认）。
func NormalizeOpenCodeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("base_url 不能包含空白字符")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("base_url 必须是合法的 HTTP(S) 地址且不含凭据、查询串或锚点")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// NormalizeOpenCodeModelID 归一 OpenCode 模型名并剥离 opencode-go/ / opencode_go/ / opencode/ 前缀。
func NormalizeOpenCodeModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"opencode-go/", "opencode_go/", "opencode/"} {
		model = strings.TrimPrefix(model, prefix)
	}
	return model
}

// StripOpenCodeModelPrefix 保留原始大小写仅剥离厂商前缀，便于透传或展示。
func StripOpenCodeModelPrefix(model string) string {
	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{"opencode-go/", "opencode_go/", "opencode/"} {
		if strings.HasPrefix(lower, prefix) {
			return trimmed[len(prefix):]
		}
	}
	return trimmed
}

// IsOpenCodeUnsupportedModel 判断模型是否属于 OpenCode 专有协议端点模型。
// gemini-* 仅走 Google SDK 端点，jev-* 仅走 System One 端点，标准网关不支持。
func IsOpenCodeUnsupportedModel(model string) bool {
	normalized := NormalizeOpenCodeModelID(model)
	return strings.HasPrefix(normalized, "gemini-") || strings.HasPrefix(normalized, "jev-")
}

// DefaultOpenCodeGoModelIDs 是 OpenCode Go 公开的默认模型目录，供尚未同步上游前使用。
func DefaultOpenCodeGoModelIDs() []string {
	return []string{
		"grok-4.7",
		"grok-4.6",
		"gpt-5.6-luna",
		"glm-5.3-flash",
		"glm-5.3",
		"glm-5.2",
		"glm-5.1",
		"kimi-k3",
		"kimi-k2.7-code",
		"kimi-k2.6",
		"longcat-2.0",
		"deepseek-v4-pro",
		"deepseek-v4-flash",
		"deepseek-v4-flash-vision-exp",
		"mimo-v2.5",
		"mimo-v2.5-pro",
		"minimax-m3",
		"minimax-m2.7",
		"minimax-m2.5",
		"muse-spark-1.3-contributor",
		"muse-spark-1.2-contributor",
		"qwen3.8-max",
		"qwen3.8-flash",
		"qwen3.7-max",
		"qwen3.7-plus",
		"qwen3.6-plus",
		"hy4-preview",
		"hy3",
		"omen-alpha",
	}
}

// DefaultOpenCodeGoProtocolRules 是 Go 模式内置协议路由表（对齐 sub2api）。
func DefaultOpenCodeGoProtocolRules() []OpenCodeProtocolRule {
	return []OpenCodeProtocolRule{
		{Pattern: "grok-*", Protocol: OpenCodeProtocolResponses},
		{Pattern: "gpt-*", Protocol: OpenCodeProtocolResponses},
		{Pattern: "muse-spark-*", Protocol: OpenCodeProtocolResponses},
		{Pattern: "minimax-*", Protocol: OpenCodeProtocolAnthropic},
		{Pattern: "qwen*", Protocol: OpenCodeProtocolAnthropic},
	}
}

// DefaultOpenCodeZenProtocolRules 是 Zen 模式内置协议路由表（对齐 opencode.ai/docs/zen/）。
func DefaultOpenCodeZenProtocolRules() []OpenCodeProtocolRule {
	return []OpenCodeProtocolRule{
		{Pattern: "grok-*", Protocol: OpenCodeProtocolResponses},
		{Pattern: "gpt-*", Protocol: OpenCodeProtocolResponses},
		{Pattern: "muse-spark-*", Protocol: OpenCodeProtocolResponses},
		{Pattern: "claude-*", Protocol: OpenCodeProtocolAnthropic},
		{Pattern: "qwen3.8-max", Protocol: OpenCodeProtocolChatCompletions},
		{Pattern: "qwen*", Protocol: OpenCodeProtocolAnthropic},
	}
}

// DefaultOpenCodeProtocolRulesForMode 返回指定模式的内置协议分流规则。
func DefaultOpenCodeProtocolRulesForMode(mode string) []OpenCodeProtocolRule {
	if NormalizeOpenCodeMode(mode) == OpenCodeModeZen {
		return DefaultOpenCodeZenProtocolRules()
	}
	return DefaultOpenCodeGoProtocolRules()
}

// IsOpenCodeNativeProtocol 判断协议名是否为 OpenCode 支持的三种原生协议之一。
func IsOpenCodeNativeProtocol(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case OpenCodeProtocolChatCompletions, OpenCodeProtocolAnthropic, OpenCodeProtocolResponses:
		return true
	default:
		return false
	}
}

func normalizeOpenCodeProtocolSet(preferred string, protocols []string) []string {
	preferred = strings.ToLower(strings.TrimSpace(preferred))
	set := []string{preferred}
	for _, p := range protocols {
		p = strings.ToLower(strings.TrimSpace(p))
		if !IsOpenCodeNativeProtocol(p) || slices.Contains(set, p) {
			continue
		}
		set = append(set, p)
	}
	return set
}

func normalizeOpenCodeRulePattern(pattern string) (string, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	if len(pattern) > OpenCodeMaxProtocolRulePatternLength {
		return "", fmt.Errorf("pattern 长度不能超过 %d", OpenCodeMaxProtocolRulePatternLength)
	}
	if strings.ContainsAny(pattern, " \t\r\n") {
		return "", fmt.Errorf("pattern 不能包含空白字符")
	}
	star := strings.Count(pattern, "*")
	if star > 1 || (star == 1 && !strings.HasSuffix(pattern, "*")) {
		return "", fmt.Errorf("pattern 仅允许在末尾使用单个 * 通配符")
	}
	return pattern, nil
}

// ParseOpenCodeProtocolRules 校验并规范化协议分流规则（支持 JSON 字符串、[]OpenCodeProtocolRule 或 []any）。
func ParseOpenCodeProtocolRules(raw any) ([]OpenCodeProtocolRule, error) {
	if raw == nil {
		return nil, nil
	}
	var decoded []OpenCodeProtocolRule
	switch typed := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil, nil
		}
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			return nil, fmt.Errorf("protocol_rules JSON 解析失败: %w", err)
		}
	case []OpenCodeProtocolRule:
		decoded = typed
	default:
		b, err := json.Marshal(typed)
		if err != nil {
			return nil, fmt.Errorf("protocol_rules 格式无效: %w", err)
		}
		if err := json.Unmarshal(b, &decoded); err != nil {
			return nil, fmt.Errorf("protocol_rules 必须是规则数组: %w", err)
		}
	}
	if len(decoded) > OpenCodeMaxProtocolRules {
		return nil, fmt.Errorf("protocol_rules 最多支持 %d 条规则", OpenCodeMaxProtocolRules)
	}
	out := make([]OpenCodeProtocolRule, 0, len(decoded))
	for i, item := range decoded {
		pattern, err := normalizeOpenCodeRulePattern(item.Pattern)
		if err != nil {
			return nil, fmt.Errorf("protocol_rules[%d]: %w", i, err)
		}
		extra := make([]string, 0, len(item.Protocols))
		for _, p := range item.Protocols {
			p = strings.ToLower(strings.TrimSpace(p))
			if !IsOpenCodeNativeProtocol(p) {
				return nil, fmt.Errorf("protocol_rules[%d]: protocols 仅支持 chat_completions / anthropic / responses", i)
			}
			extra = append(extra, p)
		}
		protocol := strings.ToLower(strings.TrimSpace(item.Protocol))
		if protocol == "" && len(extra) > 0 {
			protocol = extra[0]
		}
		if !IsOpenCodeNativeProtocol(protocol) {
			return nil, fmt.Errorf("protocol_rules[%d]: protocol 仅支持 chat_completions / anthropic / responses", i)
		}
		rule := OpenCodeProtocolRule{Pattern: pattern, Protocol: protocol}
		if set := normalizeOpenCodeProtocolSet(protocol, extra); len(set) > 1 {
			rule.Protocols = set
		}
		out = append(out, rule)
	}
	return out, nil
}

// EncodeOpenCodeProtocolRules 将规则序列化成存入 credentials JSON 的字符串。
func EncodeOpenCodeProtocolRules(rules []OpenCodeProtocolRule) string {
	if len(rules) == 0 {
		return ""
	}
	encoded, err := json.Marshal(rules)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func openCodePatternMatches(pattern, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	model = NormalizeOpenCodeModelID(model)
	if pattern == "" || model == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	}
	return model == pattern
}

func matchOpenCodeRuleSet(model string, rules []OpenCodeProtocolRule) ([]string, bool) {
	for _, rule := range rules {
		if !IsOpenCodeNativeProtocol(rule.Protocol) {
			continue
		}
		if openCodePatternMatches(rule.Pattern, model) {
			return normalizeOpenCodeProtocolSet(rule.Protocol, rule.Protocols), true
		}
	}
	return nil, false
}

// ResolveOpenCodeProtocol 根据模型名、入站协议、账号模式与自定义规则决定上游协议。
//   - 若账号显式配置了 protocol_rules：按自定义规则匹配；未命中回落 chat_completions（不再套用内置表）。
//   - 否则按账号模式内置规则匹配；未命中回落 chat_completions。
//   - 命中规则允许多个协议且包含 inbound 时，直接选 inbound 以免去协议转换。
func ResolveOpenCodeProtocol(model, inbound, mode string, customRules []OpenCodeProtocolRule, hasCustom bool) string {
	inbound = strings.ToLower(strings.TrimSpace(inbound))
	var set []string
	var matched bool
	if hasCustom {
		set, matched = matchOpenCodeRuleSet(model, customRules)
	} else {
		set, matched = matchOpenCodeRuleSet(model, DefaultOpenCodeProtocolRulesForMode(mode))
	}
	if !matched || len(set) == 0 {
		return OpenCodeProtocolChatCompletions
	}
	if inbound != "" && slices.Contains(set, inbound) {
		return inbound
	}
	return set[0]
}

// openCodeReservedHeaders 是网关拥有的认证与传输帧头，不允许 custom_headers 覆盖。
var openCodeReservedHeaders = map[string]struct{}{
	"authorization":     {},
	"x-api-key":         {},
	"content-type":      {},
	"content-length":    {},
	"host":              {},
	"transfer-encoding": {},
	"connection":        {},
	"accept-encoding":   {},
}

// IsOpenCodeReservedHeader 判断请求头是否属于网关保留头（不可由 custom_headers 覆写）。
func IsOpenCodeReservedHeader(name string) bool {
	_, reserved := openCodeReservedHeaders[strings.ToLower(strings.TrimSpace(name))]
	return reserved
}

// OpenCodeCredentialFromRow 从数据库行解析 OpenCodeCredential。
func OpenCodeCredentialFromRow(row *database.AccountRow) OpenCodeCredential {
	if row == nil {
		return OpenCodeCredential{Mode: OpenCodeModeGo}
	}
	mode := NormalizeOpenCodeMode(row.GetCredential(OpenCodeModeCredentialKey))
	cred := OpenCodeCredential{
		Mode:             mode,
		AnthropicBaseURL: strings.TrimRight(strings.TrimSpace(row.GetCredential(OpenCodeAnthropicBaseURLCredentialKey)), "/"),
	}
	if rawRules := strings.TrimSpace(row.GetCredential(OpenCodeProtocolRulesCredentialKey)); rawRules != "" {
		if rules, err := ParseOpenCodeProtocolRules(rawRules); err == nil {
			cred.ProtocolRules = rules
			cred.HasCustomRules = true
		}
	}
	if rawCatalog := strings.TrimSpace(row.GetCredential(OpenCodeCatalogModelsCredentialKey)); rawCatalog != "" {
		var models []string
		if err := json.Unmarshal([]byte(rawCatalog), &models); err == nil {
			cred.CatalogModels = normalizeModelList(models)
		}
	}
	if rawUsage := strings.TrimSpace(row.GetCredential(OpenCodeUsageCredentialKey)); rawUsage != "" {
		var snap OpenCodeUsageSnapshot
		if err := json.Unmarshal([]byte(rawUsage), &snap); err == nil && snap.Status != "" {
			cred.Usage = &snap
		}
	}
	return cred
}

// applyOpenCodeRowLocked 在装载账号时将 OpenCode 专有字段投影到运行时 Account。调用方持有 a.mu 或账号尚未发布。
func (a *Account) applyOpenCodeRowLocked(row *database.AccountRow) {
	a.OpenCode = OpenCodeCredentialFromRow(row)
	a.Email = strings.TrimSpace(row.GetCredential("email"))
	if a.PlanType == "" {
		if a.OpenCode.Mode == OpenCodeModeZen {
			a.PlanType = "opencode zen"
		} else {
			a.PlanType = "opencode go"
		}
	}
	if strings.TrimSpace(a.APIKey) != "" {
		a.HealthTier = HealthTierHealthy
	}
	a.projectOpenCodeUsageWindowsLocked()
}

// projectOpenCodeUsageWindowsLocked 将 Go 额度快照的 rolling(5h) / weekly(7d) 同步到通用用量字段，
// 便于已有统计与重置倒计时逻辑复用。
func (a *Account) projectOpenCodeUsageWindowsLocked() {
	snap := a.OpenCode.Usage
	if a.OpenCode.Mode != OpenCodeModeGo || snap == nil || snap.Status != OpenCodeUsageStatusOK {
		return
	}
	if snap.Rolling != nil {
		a.UsagePercent5h = clampSchedulingPercent(snap.Rolling.Percent)
		a.UsagePercent5hValid = true
		if !snap.Rolling.ResetsAt.IsZero() {
			a.Reset5hAt = snap.Rolling.ResetsAt
		}
	}
	if snap.Weekly != nil {
		a.UsagePercent7d = clampSchedulingPercent(snap.Weekly.Percent)
		a.UsagePercent7dValid = true
		if !snap.Weekly.ResetsAt.IsZero() {
			a.Reset7dAt = snap.Weekly.ResetsAt
		}
	}
}

func (a *Account) isOpenCodeLocked() bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamOpenCode) {
		return false
	}
	return strings.TrimSpace(a.APIKey) != ""
}

// IsOpenCode 判断账号是否为 OpenCode 上游账号。
func (a *Account) IsOpenCode() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isOpenCodeLocked()
}

// OpenCodeMode 返回账号模式（go / zen）。非 OpenCode 账号返回空串。
func (a *Account) OpenCodeMode() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isOpenCodeLocked() {
		return ""
	}
	return NormalizeOpenCodeMode(a.OpenCode.Mode)
}

// IsOpenCodeGoMode 判断是否为 OpenCode Go 订阅模式账号。
func (a *Account) IsOpenCodeGoMode() bool {
	return a.OpenCodeMode() == OpenCodeModeGo
}

// OpenCodeSnapshot 返回线程安全的 OpenCodeCredential 副本及通用连接信息。
type OpenCodeAccountConfig struct {
	Mode             string
	BaseURL          string
	AnthropicBaseURL string
	APIKey           string
	ProxyURL         string
	CustomHeaders    map[string]string
	Models           []string
	CatalogModels    []string
	ProtocolRules    []OpenCodeProtocolRule
	HasCustomRules   bool
	Usage            *OpenCodeUsageSnapshot
}

// OpenCodeConfig 返回线程安全的 OpenCode 配置快照。
func (a *Account) OpenCodeConfig() OpenCodeAccountConfig {
	if a == nil {
		return OpenCodeAccountConfig{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	mode := NormalizeOpenCodeMode(a.OpenCode.Mode)
	baseURL := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultOpenCodeBaseURL(mode)
	}
	anthropicBaseURL := strings.TrimRight(strings.TrimSpace(a.OpenCode.AnthropicBaseURL), "/")
	if anthropicBaseURL == "" {
		if strings.TrimSpace(a.BaseURL) != "" {
			anthropicBaseURL = strings.TrimSuffix(baseURL, "/v1")
		} else {
			anthropicBaseURL = DefaultOpenCodeAnthropicBaseURL(mode)
		}
	}
	cfg := OpenCodeAccountConfig{
		Mode:             mode,
		BaseURL:          baseURL,
		AnthropicBaseURL: anthropicBaseURL,
		APIKey:           strings.TrimSpace(a.APIKey),
		ProxyURL:         strings.TrimSpace(a.ProxyURL),
		CustomHeaders:    cloneStringMap(a.CustomHeaders),
		Models:           cloneStringSlice(a.Models),
		CatalogModels:    cloneStringSlice(a.OpenCode.CatalogModels),
		HasCustomRules:   a.OpenCode.HasCustomRules,
	}
	if len(a.OpenCode.ProtocolRules) > 0 {
		cfg.ProtocolRules = make([]OpenCodeProtocolRule, len(a.OpenCode.ProtocolRules))
		for i, r := range a.OpenCode.ProtocolRules {
			copied := r
			if len(r.Protocols) > 0 {
				copied.Protocols = append([]string(nil), r.Protocols...)
			}
			cfg.ProtocolRules[i] = copied
		}
	}
	if a.OpenCode.Usage != nil {
		snapCopy := *a.OpenCode.Usage
		if snapCopy.Rolling != nil {
			w := *snapCopy.Rolling
			snapCopy.Rolling = &w
		}
		if snapCopy.Weekly != nil {
			w := *snapCopy.Weekly
			snapCopy.Weekly = &w
		}
		if snapCopy.Monthly != nil {
			w := *snapCopy.Monthly
			snapCopy.Monthly = &w
		}
		cfg.Usage = &snapCopy
	}
	return cfg
}

// openCodeUsagePercentForSchedulingLocked 返回 OpenCode 账号的调度排序键（已用百分比）。
// 仅 Go 模式且快照在 TTL 内有效时返回三窗口最大已用比例，否则返回 0。
func (a *Account) openCodeUsagePercentForSchedulingLocked(now time.Time) float64 {
	if a.OpenCode.Mode != OpenCodeModeGo {
		return 0
	}
	snap := a.OpenCode.Usage
	if snap == nil || snap.Status != OpenCodeUsageStatusOK {
		return 0
	}
	if !snap.FetchedAt.IsZero() && now.Sub(snap.FetchedAt) > OpenCodeUsageSchedulingTTL {
		return 0
	}
	maxPct := 0.0
	for _, w := range []*OpenCodeUsageWindow{snap.Rolling, snap.Weekly, snap.Monthly} {
		if w == nil || math.IsNaN(w.Percent) {
			continue
		}
		if pct := clampSchedulingPercent(w.Percent); pct > maxPct {
			maxPct = pct
		}
	}
	return maxPct
}

// OpenCodeEarliestFutureReset 返回 Go 额度快照中最早一个尚未到期的窗口重置时间。
func OpenCodeEarliestFutureReset(snap *OpenCodeUsageSnapshot, now time.Time) *time.Time {
	if snap == nil || snap.Status != OpenCodeUsageStatusOK {
		return nil
	}
	var earliest *time.Time
	for _, w := range []*OpenCodeUsageWindow{snap.Rolling, snap.Weekly, snap.Monthly} {
		if w == nil || w.ResetsAt.IsZero() || !w.ResetsAt.After(now) {
			continue
		}
		t := w.ResetsAt.UTC()
		if earliest == nil || t.Before(*earliest) {
			earliest = &t
		}
	}
	return earliest
}

// OpenCodeUsageNearExhausted 判断 Go 额度快照中是否已有窗口达到给定百分比阈值。
// 快照缺失时返回 true（保守按窗口耗尽处理，避免把真实耗尽错当短频控）。
func OpenCodeUsageNearExhausted(snap *OpenCodeUsageSnapshot, threshold float64) bool {
	if snap == nil || snap.Status != OpenCodeUsageStatusOK {
		return true
	}
	seen := false
	for _, w := range []*OpenCodeUsageWindow{snap.Rolling, snap.Weekly, snap.Monthly} {
		if w == nil {
			continue
		}
		seen = true
		if w.Percent >= threshold {
			return true
		}
	}
	return !seen
}

// ApplyOpenCodeAccountRow 把同步或编辑写库后的行更新到内存运行态账号。
func (s *Store) ApplyOpenCodeAccountRow(row *database.AccountRow) bool {
	if s == nil || row == nil {
		return false
	}
	acc := s.FindByID(row.ID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	if !acc.isOpenCodeLocked() {
		acc.mu.Unlock()
		return false
	}
	if apiKey := strings.TrimSpace(row.GetCredential("api_key")); apiKey != "" {
		acc.APIKey = apiKey
	}
	acc.BaseURL = strings.TrimRight(strings.TrimSpace(row.GetCredential("base_url")), "/")
	acc.Models = normalizeModelList(row.GetCredentialStringSlice("models"))
	acc.ModelMapping = strings.TrimSpace(row.GetCredential("model_mapping"))
	acc.CustomHeaders = row.GetCredentialStringMap("custom_headers")
	acc.ProxyURL = strings.TrimSpace(row.ProxyURL)
	acc.Email = strings.TrimSpace(row.GetCredential("email"))
	acc.OpenCode = OpenCodeCredentialFromRow(row)
	if acc.OpenCode.Mode == OpenCodeModeZen {
		acc.PlanType = "opencode zen"
	} else {
		acc.PlanType = "opencode go"
	}
	acc.projectOpenCodeUsageWindowsLocked()
	acc.mu.Unlock()
	s.rebuildFastScheduler()
	return true
}

// OpenCodeGoUsageAutoRefreshDue 判断活跃的 Go 账号是否到达请求触发的额度刷新时机。
func OpenCodeGoUsageAutoRefreshDue(snap *OpenCodeUsageSnapshot, lastUsedAt, now time.Time) bool {
	if lastUsedAt.IsZero() {
		return false
	}
	if snap == nil {
		return true
	}
	if !snap.LastAttemptAt.IsZero() && now.Sub(snap.LastAttemptAt) < OpenCodeGoUsageMinFetchInterval {
		return false
	}
	if snap.Status == OpenCodeUsageStatusOK {
		if snap.FetchedAt.IsZero() {
			return true
		}
		if !lastUsedAt.After(snap.FetchedAt) {
			return false
		}
		return now.Sub(snap.FetchedAt) >= OpenCodeGoUsageDefaultMaxWait || now.Sub(lastUsedAt) >= time.Minute
	}
	return now.Sub(snap.LastAttemptAt) >= OpenCodeGoUsageDefaultMaxWait
}
