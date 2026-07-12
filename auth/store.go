package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
)

// AccountStatus 账号状态
type AccountStatus int

const (
	StatusReady    AccountStatus = iota // 可用
	StatusCooldown                      // 冷却中（被限速）
	StatusError                         // 不可用（RT 失效等）
)

// AccountHealthTier 账号健康层级（仅用于调度优先级，不直接暴露给外部 API）
type AccountHealthTier string

const (
	HealthTierHealthy AccountHealthTier = "healthy"
	HealthTierWarm    AccountHealthTier = "warm"
	HealthTierRisky   AccountHealthTier = "risky"
	HealthTierBanned  AccountHealthTier = "banned"
)

const UpstreamOpenAIResponses = "openai_responses"

const (
	CodexClientMetadataModeAuto   = "auto"
	CodexClientMetadataModeAlways = "always"
	CodexClientMetadataModeOff    = "off"
)

func NormalizeCodexClientMetadataMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case CodexClientMetadataModeAlways:
		return CodexClientMetadataModeAlways
	case CodexClientMetadataModeOff:
		return CodexClientMetadataModeOff
	default:
		return CodexClientMetadataModeAuto
	}
}

func IsValidCodexClientMetadataMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case CodexClientMetadataModeAuto, CodexClientMetadataModeAlways, CodexClientMetadataModeOff:
		return true
	default:
		return false
	}
}

const (
	DefaultTestContent  = "hi"
	MaxTestContentRunes = 8192
)

// NormalizeTestContent returns the prompt text used by connection tests.
// Empty content keeps the historical minimal probe behavior.
func NormalizeTestContent(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return DefaultTestContent
	}
	return content
}

// Account 运行时账号状态
type Account struct {
	mu                      sync.RWMutex
	DBID                    int64 // 数据库 ID
	RefreshToken            string
	SessionToken            string
	AccessToken             string
	ExpiresAt               time.Time
	AccountID               string
	Email                   string
	PlanType                string
	ProxyURL                string
	CustomHeaders           map[string]string
	UpstreamType            string
	BaseURL                 string
	APIKey                  string
	Models                  []string
	ModelMapping            string
	CodexClientMetadataMode string
	Status                  AccountStatus
	CooldownUtil            time.Time
	CooldownReason          string // rate_limited / unauthorized / 空
	ErrorMsg                string

	// 用量进度（从 Codex 响应头被动解析）
	UsagePercent7d      float64 // 7d 窗口使用率 0-100+
	UsagePercent7dValid bool
	Reset7dAt           time.Time // 7d 窗口重置时间
	// Window7dSeconds 是「长窗口」(即 7d 槽)的真实周期秒数：plus/pro 通常为 7d(604800)，
	// team plan 实为 monthly(约 2592000)。0 = 未知(按 7d 默认处理)。智能配速的自然速率
	// 需按真实周期计算，否则 team 的月窗被当成 7 天 → natural 速率偏大 → 过度限流。
	Window7dSeconds     int64
	UsagePercent5h      float64 // 5h 窗口使用率 0-100+
	UsagePercent5hValid bool
	Reset5hAt           time.Time // 5h 窗口重置时间
	UsageUpdatedAt      time.Time // 7d 用量快照刷新时间
	UsageUpdatedAt5h    time.Time // 5h 用量快照刷新时间

	// RateLimitResetCredits 是 OpenAI 官方账号剩余的「主动重置次数」，来自
	// /backend-api/wham/usage 响应的 rate_limit_reset_credits.available_count。
	// -1 表示尚未探测过（未知）；>=0 为已知次数。
	RateLimitResetCredits      int
	RateLimitResetCreditsValid bool
	// resetCreditsProbedAt 记录最近一次成功 wham 用量探针的时间。
	// 「主动重置次数」只能通过 wham 探针刷新（普通 /responses 流量不携带该字段），
	// 因此用它独立判断重置次数是否过期，避免活跃账号因用量快照一直被流量刷新而长期不探针。
	resetCreditsProbedAt time.Time

	usageProbeInFlight          bool
	recoveryProbeInFlight       bool
	lastAuthVerifyAt            time.Time // WS 上游异常关闭后触发的鉴权验证探针节流时间戳
	AutoPause5hThreshold        float64   // 0..1, 0 = disabled
	AutoPause7dThreshold        float64   // 0..1, 0 = disabled
	AutoPause5hDisabled         bool
	AutoPause7dDisabled         bool
	effectiveAutoPause5h        float64 // resolved: account > group > global
	effectiveAutoPause7d        float64
	autoPause5hGuardBandPercent float64 // percentage points, 0 = disabled
	autoPause5hGuardConcurrency int     // 0 = disabled; otherwise guard-band concurrency cap
	// 智能配速（issue #312）：按剩余配额/剩余时间把用量匀速摊到窗口重置，
	// 燃烧过快时按可持续速率缩放并发。参数由 Store 全局设置快照而来。
	smartPacingEnabled        bool
	smartPacingMinConcurrency int
	smartPacingWindows5h      bool
	smartPacingWindows7d      bool
	DispatchCountLimit        int64 // 0 = disabled; per-reset-window dispatch cap
	dispatchCountMu           sync.Mutex
	dispatchWindowUsed        int64
	dispatchWindowResetAt     time.Time
	// SchedulerPriority 账号调度优先级（issue #358）：数值大者严格先调度，
	// 同优先级内才按健康档位与调度分竞争。0 为默认；负值可把账号压为兜底渠道。
	SchedulerPriority int64

	// 调度健康信号
	HealthTier               AccountHealthTier
	SchedulerScore           float64
	DispatchScore            float64
	ScoreBiasEffective       int64
	BaseConcurrencyEffective int64
	groupBaseConcurrency     int64 // resolved from memberships; 0 means no group override
	DynamicConcurrencyLimit  int64
	LatencyEWMA              float64
	SuccessStreak            int
	FailureStreak            int
	LastSuccessAt            time.Time
	LastFailureAt            time.Time
	LastUnauthorizedAt       time.Time
	LastRateLimitedAt        time.Time
	LastTimeoutAt            time.Time
	LastServerErrorAt        time.Time
	LastRecoveryProbeAt      time.Time

	// 滑动窗口成功率（最近 N 次请求）
	RecentResults    [20]uint8 // 1=成功, 0=失败
	RecentResultsIdx int       // 环形缓冲区写入位置
	RecentResultsCnt int       // 已记录数量（最大 20）

	// 高并发调度指标（原子操作，无需锁）
	ActiveRequests int64 // 当前并发请求数
	TotalRequests  int64 // 累计总请求数
	LastUsedAt     int64 // 最后使用时间（UnixNano）
	Disabled       int32 // 原子标志，1 = 立即不可调度（401 时瞬间置位，无需等锁）
	AddedAt        int64 // 加入号池的时间（UnixNano），用于过期清理
	Locked         int32 // 原子标志，1 = 锁定，自动清理跳过此账号
	DispatchPaused int32 // 原子标志，1 = 禁用调度选择，不影响刷新/探针/清理

	// per-account 调度配置（nil = 跟随默认）
	ScoreBiasOverride       *int64
	BaseConcurrencyOverride *int64
	CreditEnabled           bool // 信用账号标记
	CreditSkipUsageWindow   bool // 跳过用量窗口惩罚和本地限流标记
	// IgnoreUsageLimitStatusOverride 为 nil 时跟随全局设置；effective 值由 Store 解析。
	IgnoreUsageLimitStatusOverride *bool
	ignoreUsageLimitStatus         bool
	SkipWarmTier                   bool // 跳过 warm 层级降级
	AllowedAPIKeyIDs               []int64
	allowedAPIKeySet               map[int64]struct{}
	Tags                           []string
	GroupIDs                       []int64
	ModelCooldowns                 map[string]ModelCooldown

	SubscriptionExpiresAt time.Time
}

type ModelCooldown struct {
	Model        string
	Reason       string
	ResetAt      time.Time
	UpdatedAt    time.Time
	BackoffLevel int
}

// AccountFilter 用于请求级调度约束，例如按模型限制账号套餐。
type AccountFilter func(*Account) bool

const (
	defaultBackgroundRefreshInterval = 2 * time.Minute
	defaultUsageProbeMaxAge          = 10 * time.Minute
	defaultUsageProbeConcurrency     = 16
	defaultRecoveryProbeInterval     = 30 * time.Minute
	// probeBoundaryLag 是「到点即探」定时器相对边界时刻的滞后量：稍晚于重置/冷却
	// 结束再探，确保 NeedsUsageProbe 里 `!ResetAt.After(now)` 已成立，并给上游与
	// 本地之间的时钟偏差留出余量，避免探早了仍拿到重置前的旧数据。
	probeBoundaryLag                 = 2 * time.Second
	premium5hUrgencyWindow           = 4 * time.Hour
	premium5hUrgencyMaxBonus         = 25.0
	premium5hUrgencyMinRemainingPct  = 5.0
	premium5hUrgencyFullRemainingPct = 50.0
	premium7dUrgencyWindow           = 72 * time.Hour
	premium7dUrgencyMaxBonus         = 80.0
	premium7dUrgencyMinRemainingPct  = 5.0
	premium7dUrgencyFullRemainingPct = 70.0
	expiryUrgencyUrgentDays          = 3
	expiryUrgencyWarnDays            = 7
	expiryUrgencyUrgentBonus         = 60.0
	expiryUrgencyWarnBonus           = 25.0
)

// SchedulerBreakdown 调度评分拆解
type SchedulerBreakdown struct {
	UnauthorizedPenalty float64
	RateLimitPenalty    float64
	TimeoutPenalty      float64
	ServerPenalty       float64
	FailurePenalty      float64
	SuccessBonus        float64
	ProvenBonus         float64 // 经过验证的账号（TotalRequests > 10）加分
	UsagePenalty7d      float64
	UsageUrgencyBonus5h float64
	UsageUrgencyBonus7d float64
	ExpiryUrgencyBonus  float64
	LatencyPenalty      float64
	SuccessRatePenalty  float64 // 滑动窗口成功率惩罚
}

// SchedulerDebugSnapshot 调度调试快照
type SchedulerDebugSnapshot struct {
	HealthTier               string
	SchedulerScore           float64
	DispatchScore            float64
	ScoreBiasOverride        *int64
	ScoreBiasEffective       int64
	BaseConcurrencyOverride  *int64
	BaseConcurrencyEffective int64
	DynamicConcurrencyLimit  int64
	Breakdown                SchedulerBreakdown
	LastUnauthorizedAt       time.Time
	LastRateLimitedAt        time.Time
	LastTimeoutAt            time.Time
	LastServerErrorAt        time.Time
}

// ID 返回数据库 ID
func (a *Account) ID() int64 {
	return a.DBID
}

// Mu 返回读写锁（供外部包安全读取字段）
func (a *Account) Mu() *sync.RWMutex {
	return &a.mu
}

func (a *Account) isOpenAIResponsesAPILocked() bool {
	if a == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamOpenAIResponses) &&
		strings.TrimSpace(a.BaseURL) != "" &&
		strings.TrimSpace(a.APIKey) != ""
}

func (a *Account) hasDispatchCredentialLocked() bool {
	if a == nil {
		return false
	}
	if a.isOpenAIResponsesAPILocked() {
		return true
	}
	return strings.TrimSpace(a.AccessToken) != ""
}

func (a *Account) IsOpenAIResponsesAPI() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isOpenAIResponsesAPILocked()
}

func (a *Account) SupportsOpenAIResponsesModel(model string) bool {
	if a == nil {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isOpenAIResponsesAPILocked() || len(a.Models) == 0 {
		return false
	}
	for _, candidate := range a.Models {
		if strings.EqualFold(strings.TrimSpace(candidate), model) {
			return true
		}
	}
	return false
}

func (a *Account) OpenAIResponsesModels() []string {
	if a == nil {
		return []string{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isOpenAIResponsesAPILocked() {
		return []string{}
	}
	return cloneStringSlice(a.Models)
}

func (a *Account) OpenAIResponsesModelMapping() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isOpenAIResponsesAPILocked() {
		return ""
	}
	return strings.TrimSpace(a.ModelMapping)
}

func (a *Account) OpenAIResponsesCodexClientMetadataMode() string {
	if a == nil {
		return CodexClientMetadataModeAuto
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return NormalizeCodexClientMetadataMode(a.CodexClientMetadataMode)
}

func (a *Account) OpenAIResponsesCredentials() (baseURL, apiKey string) {
	if a == nil {
		return "", ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isOpenAIResponsesAPILocked() {
		return "", ""
	}
	return strings.TrimRight(strings.TrimSpace(a.BaseURL), "/"), strings.TrimSpace(a.APIKey)
}

func (a *Account) GetProxyURL() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.ProxyURL)
}

func (a *Account) GetAccessToken() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.AccessToken)
}

func (a *Account) GetCustomHeaders() map[string]string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneStringMap(a.CustomHeaders)
}

// EffectiveAccountID 返回实际用于上游路由的工作区 ID:自定义请求头覆盖了
// Chatgpt-Account-Id 时以覆盖值为准(与 proxy/wsrelay 转发行为一致),
// 额度探测等旁路请求必须用它,否则统计的是与流量不同的空间。
func (a *Account) EffectiveAccountID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if v := strings.TrimSpace(a.CustomHeaders["Chatgpt-Account-Id"]); v != "" {
		return v
	}
	return strings.TrimSpace(a.AccountID)
}

// AccountIDOverridden 判断自定义请求头是否把流量导向了与 OAuth 身份不同的空间。
func (a *Account) AccountIDOverridden() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	v := strings.TrimSpace(a.CustomHeaders["Chatgpt-Account-Id"])
	return v != "" && v != strings.TrimSpace(a.AccountID)
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func cloneInt64Ptr(v *int64) *int64 {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

func cloneInt64Slice(values []int64) []int64 {
	if len(values) == 0 {
		return []int64{}
	}
	cloned := make([]int64, len(values))
	copy(cloned, values)
	return cloned
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func normalizeModelList(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return []string{}
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i]) < strings.ToLower(result[j])
	})
	return result
}

func NormalizeOpenAIResponsesModels(values []string) []string {
	return normalizeModelList(values)
}

func NormalizeOpenAIResponsesBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "https://api.openai.com"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base_url 必须是完整的 http/https URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("base_url 仅支持 http/https")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func OpenAIResponsesEndpoint(baseURL, suffix string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return baseURL
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	if strings.HasSuffix(strings.ToLower(baseURL), "/v1") && strings.HasPrefix(strings.ToLower(suffix), "/v1/") {
		return baseURL + strings.TrimPrefix(suffix, "/v1")
	}
	return baseURL + suffix
}

func normalizeAllowedAPIKeyIDs(values []int64) []int64 {
	if len(values) == 0 {
		return []int64{}
	}
	unique := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, exists := unique[value]; exists {
			continue
		}
		unique[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})
	if len(result) == 0 {
		return []int64{}
	}
	return result
}

func reflectOptionalInt64Field(src any, fieldName string) *int64 {
	if src == nil || fieldName == "" {
		return nil
	}

	v := reflect.ValueOf(src)
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return nil
	}

	field := v.FieldByName(fieldName)
	if !field.IsValid() {
		return nil
	}

	if field.Kind() == reflect.Pointer {
		if field.IsNil() {
			return nil
		}
		field = field.Elem()
	}

	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := field.Int()
		return &value
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value := int64(field.Uint())
		return &value
	case reflect.Float32, reflect.Float64:
		value := int64(field.Float())
		return &value
	case reflect.Struct:
		validField := field.FieldByName("Valid")
		if validField.IsValid() && validField.Kind() == reflect.Bool && !validField.Bool() {
			return nil
		}
		int64Field := field.FieldByName("Int64")
		if int64Field.IsValid() && int64Field.Kind() == reflect.Int64 {
			value := int64Field.Int()
			return &value
		}
	}

	return nil
}

// fastRandN 轻量级随机数（用于调度公平性，无需加密安全）
func fastRandN(n int) int {
	if n <= 0 {
		return 0
	}
	return rand.Intn(n)
}

func concurrencyLimitForTier(baseLimit int64, tier AccountHealthTier) int64 {
	if baseLimit <= 0 {
		baseLimit = 1
	}

	switch tier {
	case HealthTierHealthy:
		return baseLimit
	case HealthTierWarm:
		half := baseLimit / 2
		if half < 1 {
			return 1
		}
		return half
	case HealthTierRisky:
		return 1
	case HealthTierBanned:
		return 0
	default:
		if baseLimit >= 2 {
			return 2
		}
		return 1
	}
}

func defaultScoreBiasForPlan(planType string) int64 {
	switch NormalizePlanType(planType) {
	// k12 是教育版 team 工作区，行为与 team 一致 (issue #282)
	case "pro", "plus", "team", "k12":
		return 50
	default:
		return 0
	}
}

func tierPriority(tier AccountHealthTier) int {
	switch tier {
	case HealthTierHealthy:
		return 3
	case HealthTierWarm:
		return 2
	case HealthTierRisky:
		return 1
	default:
		return 0
	}
}

func (a *Account) healthTierLocked() AccountHealthTier {
	if a.HealthTier != "" {
		return a.HealthTier
	}
	if a.hasDispatchCredentialLocked() {
		return HealthTierHealthy
	}
	return HealthTierWarm
}

func (a *Account) recordLatencyLocked(latency time.Duration) {
	if latency <= 0 {
		return
	}

	latencyMs := float64(latency.Milliseconds())
	if latencyMs <= 0 {
		return
	}
	if a.LatencyEWMA == 0 {
		a.LatencyEWMA = latencyMs
		return
	}
	a.LatencyEWMA = a.LatencyEWMA*0.8 + latencyMs*0.2
}

// recordResultLocked 记录一次请求结果到滑动窗口（必须持有锁）
func (a *Account) recordResultLocked(success bool) {
	if success {
		a.RecentResults[a.RecentResultsIdx] = 1
	} else {
		a.RecentResults[a.RecentResultsIdx] = 0
	}
	a.RecentResultsIdx = (a.RecentResultsIdx + 1) % len(a.RecentResults)
	if a.RecentResultsCnt < len(a.RecentResults) {
		a.RecentResultsCnt++
	}
}

// recentSuccessRateLocked 计算滑动窗口成功率 (0.0 ~ 1.0)
func (a *Account) recentSuccessRateLocked() float64 {
	if a.RecentResultsCnt == 0 {
		return 1.0 // 无数据时返回 100%
	}
	var sum int
	for i := 0; i < a.RecentResultsCnt; i++ {
		sum += int(a.RecentResults[i])
	}
	return float64(sum) / float64(a.RecentResultsCnt)
}

// linearDecay 线性衰减：返回 base × max(0, 1 - elapsed/window)
func linearDecay(base float64, elapsed, window time.Duration) float64 {
	if elapsed >= window || window <= 0 {
		return 0
	}
	return base * (1.0 - float64(elapsed)/float64(window))
}

func (a *Account) schedulerBreakdownLocked(now time.Time) SchedulerBreakdown {
	breakdown := SchedulerBreakdown{}
	premium5hLimited := a.premium5hRateLimitedLocked(now)

	// 线性衰减惩罚：随时间平滑更无突变
	if !a.LastUnauthorizedAt.IsZero() {
		elapsed := now.Sub(a.LastUnauthorizedAt)
		breakdown.UnauthorizedPenalty = linearDecay(50, elapsed, 24*time.Hour)
	}
	if !a.LastRateLimitedAt.IsZero() {
		elapsed := now.Sub(a.LastRateLimitedAt)
		breakdown.RateLimitPenalty = linearDecay(22, elapsed, time.Hour)
	}
	if !a.LastTimeoutAt.IsZero() {
		elapsed := now.Sub(a.LastTimeoutAt)
		breakdown.TimeoutPenalty = linearDecay(18, elapsed, 15*time.Minute)
	}
	if !a.LastServerErrorAt.IsZero() {
		elapsed := now.Sub(a.LastServerErrorAt)
		breakdown.ServerPenalty = linearDecay(12, elapsed, 15*time.Minute)
	}

	breakdown.FailurePenalty = float64(clampInt(a.FailureStreak*6, 0, 24))
	if !premium5hLimited {
		breakdown.SuccessBonus = float64(clampInt(a.SuccessStreak*2, 0, 12))
	}

	// 经过验证的账号（累计请求 > 10 次）优先调度
	if !premium5hLimited && atomic.LoadInt64(&a.TotalRequests) > 10 {
		breakdown.ProvenBonus = 20
	}

	// 滑动窗口成功率惩罚
	if a.RecentResultsCnt >= 5 { // 至少 5 次请求才统计
		rate := a.recentSuccessRateLocked()
		switch {
		case rate < 0.5:
			breakdown.SuccessRatePenalty = 15
		case rate < 0.75:
			breakdown.SuccessRatePenalty = 8
		}
	}

	if !(a.CreditEnabled && a.CreditSkipUsageWindow) && a.UsagePercent7dValid && strings.EqualFold(a.PlanType, "free") {
		switch {
		case a.UsagePercent7d >= 100:
			breakdown.UsagePenalty7d = 40
		case a.UsagePercent7d >= 95:
			breakdown.UsagePenalty7d = 30
		case a.UsagePercent7d >= 85:
			breakdown.UsagePenalty7d = 18
		case a.UsagePercent7d >= 70:
			breakdown.UsagePenalty7d = 8
		}
	}

	switch {
	case a.LatencyEWMA >= 20000:
		breakdown.LatencyPenalty = 15
	case a.LatencyEWMA >= 10000:
		breakdown.LatencyPenalty = 8
	case a.LatencyEWMA >= 5000:
		breakdown.LatencyPenalty = 4
	}

	return breakdown
}

func (a *Account) premium5hUsageUrgencyBonusLocked(now time.Time) float64 {
	if !isPremium5hPlan(a.PlanType) {
		return 0
	}
	if !a.UsagePercent5hValid || a.Reset5hAt.IsZero() {
		return 0
	}
	if a.UsagePercent5h >= 100 || a.premium5hRateLimitedLocked(now) {
		return 0
	}
	if a.AccessToken == "" || a.Status == StatusError || a.HealthTier == HealthTierBanned {
		return 0
	}
	if atomic.LoadInt32(&a.DispatchPaused) != 0 {
		return 0
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		return 0
	}
	if a.usageExhaustedLocked() {
		return 0
	}

	timeRemaining := a.Reset5hAt.Sub(now)
	if timeRemaining <= 0 || timeRemaining > premium5hUrgencyWindow {
		return 0
	}

	quotaRemaining := 100 - a.UsagePercent5h
	if quotaRemaining <= premium5hUrgencyMinRemainingPct {
		return 0
	}

	timeFactor := 1 - float64(timeRemaining)/float64(premium5hUrgencyWindow)
	quotaFactor := quotaRemaining / premium5hUrgencyFullRemainingPct
	if quotaFactor > 1 {
		quotaFactor = 1
	}
	if quotaFactor < 0 {
		quotaFactor = 0
	}

	return premium5hUrgencyMaxBonus * timeFactor * quotaFactor
}

func (a *Account) premium7dUsageUrgencyBonusLocked(now time.Time) float64 {
	if !IsPlusOrHigherPlan(a.PlanType) {
		return 0
	}
	if !a.UsagePercent7dValid || a.Reset7dAt.IsZero() {
		return 0
	}
	if a.UsagePercent7d >= 100 {
		return 0
	}
	if a.AccessToken == "" || a.Status == StatusError || a.HealthTier == HealthTierBanned {
		return 0
	}
	if atomic.LoadInt32(&a.DispatchPaused) != 0 {
		return 0
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		return 0
	}

	timeRemaining := a.Reset7dAt.Sub(now)
	if timeRemaining <= 0 || timeRemaining > premium7dUrgencyWindow {
		return 0
	}

	quotaRemaining := 100 - a.UsagePercent7d
	if quotaRemaining <= premium7dUrgencyMinRemainingPct {
		return 0
	}

	timeFactor := 1 - float64(timeRemaining)/float64(premium7dUrgencyWindow)
	quotaFactor := quotaRemaining / premium7dUrgencyFullRemainingPct
	if quotaFactor > 1 {
		quotaFactor = 1
	}
	if quotaFactor < 0 {
		quotaFactor = 0
	}
	weightedQuotaFactor := 0.6 + 0.4*quotaFactor

	return premium7dUrgencyMaxBonus * timeFactor * weightedQuotaFactor
}

func (a *Account) effectiveBaseConcurrencyLocked(storeBaseLimit int64) int64 {
	if a.BaseConcurrencyOverride != nil && *a.BaseConcurrencyOverride > 0 {
		return *a.BaseConcurrencyOverride
	}
	if a.groupBaseConcurrency > 0 {
		return a.groupBaseConcurrency
	}
	if storeBaseLimit <= 0 {
		return 1
	}
	return storeBaseLimit
}

func (a *Account) dispatchBonusEligibleLocked(now time.Time, tier AccountHealthTier) bool {
	if tier != HealthTierHealthy && tier != HealthTierWarm {
		return false
	}
	if a.Status == StatusError {
		return false
	}
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) {
		return false
	}
	if a.healthTierLocked() == HealthTierBanned {
		return false
	}
	if a.usageExhaustedLocked() {
		return false
	}
	if a.quotaAutoPausedLocked(now) {
		return false
	}
	if !a.hasDispatchCredentialLocked() {
		return false
	}
	return true
}

func (a *Account) effectiveScoreBiasLocked(now time.Time, tier AccountHealthTier) int64 {
	if !a.dispatchBonusEligibleLocked(now, tier) {
		return 0
	}
	if a.ScoreBiasOverride != nil {
		return *a.ScoreBiasOverride
	}
	return defaultScoreBiasForPlan(a.PlanType)
}

// expiryUrgencyBonusLocked 在订阅快到期时给账号加分,促使调度器优先消耗它。
// <= 3d 紧急(+60) / <= 7d 警告(+25) / 其它(0)。已过期/free/api 不加分。
func (a *Account) expiryUrgencyBonusLocked(now time.Time) float64 {
	if a.SubscriptionExpiresAt.IsZero() {
		return 0
	}
	plan := strings.ToLower(strings.TrimSpace(a.PlanType))
	if plan == "" || plan == "free" || plan == "api" {
		return 0
	}
	remaining := a.SubscriptionExpiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}
	days := remaining.Hours() / 24
	switch {
	case days <= expiryUrgencyUrgentDays:
		return expiryUrgencyUrgentBonus
	case days <= expiryUrgencyWarnDays:
		return expiryUrgencyWarnBonus
	}
	return 0
}

func (a *Account) recomputeSchedulerLocked(baseLimit int64) {
	now := time.Now()
	breakdown := a.schedulerBreakdownLocked(now)
	score := 100.0 -
		breakdown.UnauthorizedPenalty -
		breakdown.RateLimitPenalty -
		breakdown.TimeoutPenalty -
		breakdown.ServerPenalty -
		breakdown.FailurePenalty -
		breakdown.UsagePenalty7d -
		breakdown.LatencyPenalty -
		breakdown.SuccessRatePenalty +
		breakdown.SuccessBonus +
		breakdown.ProvenBonus

	tier := HealthTierHealthy
	switch {
	case score < 60:
		tier = HealthTierRisky
	case score < 85:
		tier = HealthTierWarm
	}

	if a.LastFailureAt.After(a.LastSuccessAt) && !a.LastFailureAt.IsZero() && tier == HealthTierHealthy {
		tier = HealthTierWarm
	}
	if !a.LastUnauthorizedAt.IsZero() && now.Sub(a.LastUnauthorizedAt) < 24*time.Hour && tier == HealthTierHealthy {
		tier = HealthTierWarm
	}
	if !(a.CreditEnabled && a.CreditSkipUsageWindow) && a.UsagePercent7dValid && strings.EqualFold(a.PlanType, "free") {
		switch {
		case a.UsagePercent7d >= 95:
			tier = HealthTierRisky
		case a.UsagePercent7d >= 85 && tier == HealthTierHealthy:
			tier = HealthTierWarm
		}
	}
	if a.HealthTier == HealthTierBanned {
		tier = HealthTierBanned
	}
	if a.premium5hRateLimitedLocked(now) && tier != HealthTierBanned {
		tier = HealthTierRisky
	}
	if a.SkipWarmTier && tier == HealthTierWarm {
		tier = HealthTierHealthy
	}

	baseConcurrencyEffective := a.effectiveBaseConcurrencyLocked(baseLimit)
	scoreBiasEffective := a.effectiveScoreBiasLocked(now, tier)
	if a.dispatchBonusEligibleLocked(now, tier) {
		breakdown.UsageUrgencyBonus5h = a.premium5hUsageUrgencyBonusLocked(now)
		breakdown.UsageUrgencyBonus7d = a.premium7dUsageUrgencyBonusLocked(now)
		breakdown.ExpiryUrgencyBonus = a.expiryUrgencyBonusLocked(now)
	}
	dispatchScore := score + float64(scoreBiasEffective) + breakdown.UsageUrgencyBonus5h + breakdown.UsageUrgencyBonus7d + breakdown.ExpiryUrgencyBonus - a.quotaAutoPause5hGuardDispatchPenaltyLocked(now)

	a.HealthTier = tier
	a.SchedulerScore = score
	a.DispatchScore = dispatchScore
	a.ScoreBiasEffective = scoreBiasEffective
	a.BaseConcurrencyEffective = baseConcurrencyEffective
	a.DynamicConcurrencyLimit = a.quotaAutoPause5hGuardConcurrencyLimitLocked(concurrencyLimitForTier(baseConcurrencyEffective, tier), now)
	a.DynamicConcurrencyLimit = a.smartPacingConcurrencyLimitLocked(a.DynamicConcurrencyLimit, now)
	if a.premium5hRateLimitedLocked(now) && a.DynamicConcurrencyLimit > 1 {
		a.DynamicConcurrencyLimit = 1
	}
}

func (a *Account) schedulerSnapshot(baseLimit int64) (AccountHealthTier, float64, float64, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recomputeSchedulerLocked(baseLimit)
	return a.HealthTier, a.SchedulerScore, a.DispatchScore, a.DynamicConcurrencyLimit
}

// IsAvailable 检查账号是否可用
func (a *Account) IsAvailable() bool {
	// 原子标志优先：401 时瞬间置位，无需等锁即可拦截并发请求
	if atomic.LoadInt32(&a.Disabled) != 0 {
		return false
	}
	if atomic.LoadInt32(&a.DispatchPaused) != 0 {
		return false
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.Status == StatusError {
		return false
	}
	if a.healthTierLocked() == HealthTierBanned {
		return false
	}
	// Free 账号 7d 用量 >= 100%，视为不可用
	if a.usageExhaustedLocked() {
		return false
	}
	if a.Status == StatusCooldown && time.Now().Before(a.CooldownUtil) {
		return false
	}
	if a.premium5hRateLimitedLocked(time.Now()) {
		return false
	}
	now := time.Now()
	if a.quotaAutoPausedLocked(now) {
		return false
	}
	// 冷却期过了自动恢复
	if a.Status == StatusCooldown && !now.Before(a.CooldownUtil) {
		return a.hasDispatchCredentialLocked()
	}
	return a.hasDispatchCredentialLocked()
}

func normalizeQuotaAutoPauseThreshold(value float64) float64 {
	switch {
	case value <= 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

const (
	defaultAutoPause5hGuardBandPercent = 5.0
	defaultAutoPause5hGuardConcurrency = 1
	maxAutoPause5hGuardDispatchPenalty = 50.0

	defaultSmartPacingMinConcurrency = 1
	smartPacingWindow5h              = 5 * time.Hour
	smartPacingWindow7d              = 7 * 24 * time.Hour
)

func normalizeSmartPacingMinConcurrency(value int) int {
	if value < 1 {
		return 1
	}
	if value > 1000 {
		return 1000
	}
	return value
}

// parseSmartPacingWindows 解析 "5h,7d" 形式，返回是否对 5h / 7d 窗口配速。
// 空或非法一律回退为两个窗口都启用。
func parseSmartPacingWindows(raw string) (bool, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return true, true
	}
	var w5h, w7d bool
	for _, part := range strings.Split(raw, ",") {
		switch strings.TrimSpace(part) {
		case "5h":
			w5h = true
		case "7d":
			w7d = true
		}
	}
	if !w5h && !w7d {
		return true, true
	}
	return w5h, w7d
}

// 长窗口(7d 槽)周期识别：free/team plan 的限流长窗口实为月窗(约 30 天 = 2592000s)，
// 而非 plus/pro 的周窗(7 天 = 604800s)。用 28–31 天容差兼容服务端的轻微抖动。
const (
	monthlyWindowMinSeconds int64 = 28 * 24 * 60 * 60
	monthlyWindowMaxSeconds int64 = 31 * 24 * 60 * 60
)

func isMonthlyWindowSeconds(sec int64) bool {
	return sec >= monthlyWindowMinSeconds && sec <= monthlyWindowMaxSeconds
}

// IsMonthlyWindowSeconds 判断窗口周期是否属月窗(28–31 天，含 2592000 精确值)。
// 导出供 proxy 层的 wham/header 窗口分类复用，保证判据单一真源。
func IsMonthlyWindowSeconds(sec int64) bool {
	return isMonthlyWindowSeconds(sec)
}

// normalizeSmartPacingWindows 归一化为规范字符串（用于持久化与展示）。
func normalizeSmartPacingWindows(raw string) string {
	w5h, w7d := parseSmartPacingWindows(raw)
	switch {
	case w5h && w7d:
		return "5h,7d"
	case w5h:
		return "5h"
	default:
		return "7d"
	}
}

// smartPacingRatio 计算某窗口的"配速比" = 可持续速率 / 自然速率。
//
//	可持续速率 = 剩余配额% / 剩余时间
//	自然速率   = 100% / 窗口长度（把整窗配额均匀铺满整段窗口的速率）
//	ratio      = 剩余配额% × 窗口长度 / (100 × 剩余时间)
//
// ratio >= 1 表示未超前燃烧（无需限速）；ratio < 1 表示烧太快，需按比例压并发。
// ok=false 表示用量/重置信号无效或窗口已翻新，此时不介入。
func smartPacingRatio(usage float64, valid bool, resetAt time.Time, window time.Duration, now time.Time) (float64, bool) {
	if !valid || resetAt.IsZero() || window <= 0 {
		return 0, false
	}
	remainingTime := resetAt.Sub(now)
	if remainingTime <= 0 {
		return 0, false
	}
	remainingPct := 100 - usage
	if remainingPct <= 0 {
		// 已耗尽，交给限流/自动暂停逻辑处理，配速不越权。
		return 0, false
	}
	sustainable := remainingPct / remainingTime.Seconds()
	natural := 100.0 / window.Seconds()
	if natural <= 0 {
		return 0, false
	}
	return sustainable / natural, true
}

// window7dDurationLocked 返回长窗口(7d 槽)用于配速的周期时长：已知真实长度(team 月窗)时
// 用真实值，否则回退到默认 7 天。调用方须持有 a.mu。
func (a *Account) window7dDurationLocked() time.Duration {
	if a.Window7dSeconds > 0 {
		return time.Duration(a.Window7dSeconds) * time.Second
	}
	return smartPacingWindow7d
}

func normalizeAutoPause5hGuardBandPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func normalizeAutoPause5hGuardConcurrency(value int) int {
	if value < 0 {
		return 0
	}
	if value > 1000 {
		return 1000
	}
	return value
}

func quotaAutoPausedByWindow(usage float64, valid bool, resetAt time.Time, threshold float64, disabled bool, now time.Time) bool {
	if disabled || threshold <= 0 || !valid {
		return false
	}
	if !resetAt.IsZero() && !now.Before(resetAt) {
		return false
	}
	return usage/100 >= threshold
}

func (a *Account) quotaAutoPause5hGuardConcurrencyLimitLocked(limit int64, now time.Time) int64 {
	if limit <= 1 || a.AutoPause5hDisabled || a.effectiveAutoPause5h <= 0 || !a.UsagePercent5hValid || a.autoPause5hGuardBandPercent <= 0 || a.autoPause5hGuardConcurrency <= 0 {
		return limit
	}
	if !a.Reset5hAt.IsZero() && !now.Before(a.Reset5hAt) {
		return limit
	}

	remainingPercent := a.effectiveAutoPause5h*100 - a.UsagePercent5h
	if remainingPercent <= 0 {
		return 0
	}
	if remainingPercent <= a.autoPause5hGuardBandPercent && limit > int64(a.autoPause5hGuardConcurrency) {
		return int64(a.autoPause5hGuardConcurrency)
	}
	return limit
}

// smartPacingConcurrencyLimitLocked 智能配速（issue #312）：当账号在某个用量窗口内
// "燃烧过快"（已用比例超过按时间匀速的应用比例）时，按可持续速率与自然速率之比缩放
// 并发上限，让剩余配额平滑用到窗口重置那一刻，而不是提前撞到 5h/7d 限流。
// 只在有有效用量信号 + 重置时间时介入；5h/7d 两个窗口取更严格（更小）的比值。
func (a *Account) smartPacingConcurrencyLimitLocked(limit int64, now time.Time) int64 {
	if !a.smartPacingEnabled || limit <= 1 {
		return limit
	}
	floor := int64(a.smartPacingMinConcurrency)
	if floor < 1 {
		floor = 1
	}
	if floor >= limit {
		return limit
	}

	ratio := 1.0
	if a.smartPacingWindows5h {
		if r, ok := smartPacingRatio(a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt, smartPacingWindow5h, now); ok && r < ratio {
			ratio = r
		}
	}
	if a.smartPacingWindows7d {
		// 用长窗口的真实周期(team 为月窗)算自然速率，避免月窗被当 7 天导致过度限流。
		if r, ok := smartPacingRatio(a.UsagePercent7d, a.UsagePercent7dValid, a.Reset7dAt, a.window7dDurationLocked(), now); ok && r < ratio {
			ratio = r
		}
	}
	if ratio >= 1 {
		return limit
	}

	scaled := int64(math.Ceil(float64(limit) * ratio))
	if scaled < floor {
		scaled = floor
	}
	if scaled > limit {
		scaled = limit
	}
	return scaled
}

func (a *Account) quotaAutoPause5hGuardDispatchPenaltyLocked(now time.Time) float64 {
	if a.AutoPause5hDisabled || a.effectiveAutoPause5h <= 0 || !a.UsagePercent5hValid || a.autoPause5hGuardBandPercent <= 0 || a.autoPause5hGuardConcurrency <= 0 {
		return 0
	}
	if !a.Reset5hAt.IsZero() && !now.Before(a.Reset5hAt) {
		return 0
	}

	remainingPercent := a.effectiveAutoPause5h*100 - a.UsagePercent5h
	if remainingPercent <= 0 || remainingPercent > a.autoPause5hGuardBandPercent {
		return 0
	}
	progress := (a.autoPause5hGuardBandPercent - remainingPercent) / a.autoPause5hGuardBandPercent
	return progress * maxAutoPause5hGuardDispatchPenalty
}

func (a *Account) quotaAutoPausedLocked(now time.Time) bool {
	if quotaAutoPausedByWindow(a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt, a.effectiveAutoPause5h, a.AutoPause5hDisabled, now) {
		return true
	}
	return quotaAutoPausedByWindow(a.UsagePercent7d, a.UsagePercent7dValid, a.Reset7dAt, a.effectiveAutoPause7d, a.AutoPause7dDisabled, now)
}

func (a *Account) recomputeEffectiveAutoPause(s *Store) {
	a.effectiveAutoPause5h = resolveEffectiveThreshold(a.AutoPause5hThreshold, a.GroupIDs, s, true)
	a.effectiveAutoPause7d = resolveEffectiveThreshold(a.AutoPause7dThreshold, a.GroupIDs, s, false)
	if s != nil {
		a.autoPause5hGuardBandPercent = s.GetAutoPause5hGuardBandPercent()
		a.autoPause5hGuardConcurrency = s.GetAutoPause5hGuardConcurrency()
		a.smartPacingEnabled = s.GetSmartPacingEnabled()
		a.smartPacingMinConcurrency = s.GetSmartPacingMinConcurrency()
		a.smartPacingWindows5h, a.smartPacingWindows7d = parseSmartPacingWindows(s.GetSmartPacingWindows())
	} else {
		a.autoPause5hGuardBandPercent = defaultAutoPause5hGuardBandPercent
		a.autoPause5hGuardConcurrency = defaultAutoPause5hGuardConcurrency
		a.smartPacingEnabled = false
		a.smartPacingMinConcurrency = defaultSmartPacingMinConcurrency
		a.smartPacingWindows5h = true
		a.smartPacingWindows7d = true
	}
}

func resolveEffectiveThreshold(accountThreshold float64, groupIDs []int64, s *Store, is5h bool) float64 {
	if accountThreshold > 0 {
		return accountThreshold
	}
	if s == nil {
		return 0
	}
	var best float64
	for _, gid := range groupIDs {
		t5h, t7d := s.getGroupAutoPauseThresholds(gid)
		var t float64
		if is5h {
			t = t5h
		} else {
			t = t7d
		}
		if t > 0 && (best == 0 || t < best) {
			best = t
		}
	}
	if best > 0 {
		return best
	}
	if is5h {
		return s.GetGlobalAutoPause5hThreshold()
	}
	return s.GetGlobalAutoPause7dThreshold()
}

func (a *Account) recomputeEffectiveGroupBaseConcurrency(s *Store) {
	a.groupBaseConcurrency = resolveGroupBaseConcurrency(a.GroupIDs, s)
}

func resolveGroupBaseConcurrency(groupIDs []int64, s *Store) int64 {
	if s == nil {
		return 0
	}
	var best int64
	for _, groupID := range groupIDs {
		value, ok := s.getGroupBaseConcurrencyOverride(groupID)
		if ok && value > 0 && (best == 0 || value < best) {
			best = value
		}
	}
	return best
}

func (a *Account) creditSkipsUsageWindowLocked() bool {
	return a.CreditEnabled && a.CreditSkipUsageWindow
}

func (a *Account) recomputeEffectiveIgnoreUsageLimitStatus(global bool) {
	if a.IgnoreUsageLimitStatusOverride != nil {
		a.ignoreUsageLimitStatus = *a.IgnoreUsageLimitStatusOverride
		return
	}
	a.ignoreUsageLimitStatus = global
}

func (a *Account) skipsUsageWindowLimitsLocked() bool {
	return a.creditSkipsUsageWindowLocked() || a.ignoreUsageLimitStatus
}

// IgnoresUsageLimitStatus reports whether usage-window percentages are
// informational for this account and Responses outcomes decide availability.
func (a *Account) IgnoresUsageLimitStatus() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ignoreUsageLimitStatus
}

// GetIgnoreUsageLimitStatusOverride returns the account override. nil means
// the account follows the global setting.
func (a *Account) GetIgnoreUsageLimitStatusOverride() *bool {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.IgnoreUsageLimitStatusOverride == nil {
		return nil
	}
	value := *a.IgnoreUsageLimitStatusOverride
	return &value
}

// SkipsUsageWindowLimits 判断账号是否应跳过 5h/7d 用量窗口触发的本地限流。
func (a *Account) SkipsUsageWindowLimits() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.skipsUsageWindowLimitsLocked()
}

// usageExhaustedLocked 判断 Free 账号 7d 用量是否已耗尽（需持有 mu 读锁）
func (a *Account) usageExhaustedLocked() bool {
	if a.skipsUsageWindowLimitsLocked() {
		return false
	}
	return a.UsagePercent7dValid && strings.EqualFold(a.PlanType, "free") && a.UsagePercent7d >= 100
}

// NeedsRefresh 检查 AT 是否需要刷新（过期前 5 分钟刷新）
func (a *Account) NeedsRefresh() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Until(a.ExpiresAt) < 5*time.Minute
}

// SetCooldown 设置冷却时间
func (a *Account) SetCooldown(duration time.Duration) {
	a.SetCooldownUntil(time.Now().Add(duration), "")
}

// SetCooldownWithReason 设置冷却时间（带原因）
func (a *Account) SetCooldownWithReason(duration time.Duration, reason string) {
	a.SetCooldownUntil(time.Now().Add(duration), reason)
}

// SetCooldownUntil 设置冷却结束时间（带原因）
func (a *Account) SetCooldownUntil(until time.Time, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Status = StatusCooldown
	a.CooldownUtil = until
	a.CooldownReason = reason
	switch reason {
	case "unauthorized":
		a.HealthTier = HealthTierBanned
	case "rate_limited":
		if a.healthTierLocked() == HealthTierHealthy {
			a.HealthTier = HealthTierWarm
		} else {
			a.HealthTier = HealthTierRisky
		}
	default:
		if a.HealthTier == "" {
			a.HealthTier = HealthTierWarm
		}
	}
}

// GetCooldownReason 获取冷却原因
func (a *Account) GetCooldownReason() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CooldownReason
}

func (a *Account) GetCooldownSnapshot() (string, time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CooldownReason, a.CooldownUtil
}

// HasActiveCooldown 检查账号是否仍处于冷却期
func (a *Account) HasActiveCooldown() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Status == StatusCooldown && time.Now().Before(a.CooldownUtil)
}

// IsBanned 检查账号是否处于强隔离状态
func (a *Account) IsBanned() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.healthTierLocked() == HealthTierBanned
}

// RuntimeStatus 返回运行时状态字符串（供 admin API 使用）
func (a *Account) RuntimeStatus() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	if a.healthTierLocked() == HealthTierBanned {
		return "unauthorized"
	}
	// Free 账号 7d 用量耗尽，优先于冷却状态展示
	if a.usageExhaustedLocked() {
		return "usage_exhausted"
	}
	if a.premium5hRateLimitedLocked(now) {
		return "rate_limited"
	}
	switch a.Status {
	case StatusError:
		return "error"
	case StatusCooldown:
		if now.Before(a.CooldownUtil) {
			if a.CooldownReason != "" {
				return a.CooldownReason
			}
			return "cooldown"
		}
		if a.hasDispatchCredentialLocked() {
			if a.quotaAutoPausedLocked(now) {
				return "quota_paused"
			}
			return "active" // 冷却过期，已恢复
		}
		if a.RefreshToken != "" {
			return "refreshing"
		}
		return "error"
	default:
		if a.hasDispatchCredentialLocked() && a.quotaAutoPausedLocked(now) {
			return "quota_paused"
		}
		if a.hasDispatchCredentialLocked() {
			return "active"
		}
		if a.RefreshToken != "" && a.ErrorMsg == "" {
			return "refreshing"
		}
		return "error"
	}
}

// SetUsagePercent7d 更新 7d 用量百分比
func (a *Account) SetUsagePercent7d(pct float64) {
	a.SetUsageSnapshot(pct, time.Now())
}

// SetUsageSnapshot 更新用量快照及时间
func (a *Account) SetUsageSnapshot(pct float64, updatedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent7d = pct
	a.UsagePercent7dValid = true
	a.UsageUpdatedAt = updatedAt
}

// GetUsagePercent7d 获取 7d 用量百分比
func (a *Account) GetUsagePercent7d() (float64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsagePercent7d, a.UsagePercent7dValid
}

// MarkUsage7dRateLimited marks an account as rate-limited when its active 7d
// usage window is exhausted. A future reset time is preferred; missing reset
// metadata falls back to a full 7d cooldown, while stale reset times are ignored.
func (s *Store) MarkUsage7dRateLimited(acc *Account) bool {
	if s == nil || acc == nil || acc.IsBanned() {
		return false
	}
	if acc.SkipsUsageWindowLimits() {
		return false
	}

	pct, ok := acc.GetUsagePercent7d()
	if !ok || pct < 100 {
		return false
	}

	duration := 7 * 24 * time.Hour
	if resetAt := acc.GetReset7dAt(); !resetAt.IsZero() {
		untilReset := time.Until(resetAt)
		if untilReset <= 0 {
			return false
		}
		duration = untilReset
	}

	s.MarkCooldown(acc, duration, "rate_limited")
	return true
}

// usagePercentForScheduling 返回调度排序用的用量百分比（7d 窗口有效则返回，否则 0）。
func (a *Account) usagePercentForScheduling() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.UsagePercent7dValid {
		return a.UsagePercent7d
	}
	return 0
}

// SetUsageSnapshot5h 更新 5h 用量快照
func (a *Account) SetUsageSnapshot5h(pct float64, resetAt time.Time) {
	a.SetUsageSnapshot5hAt(pct, resetAt, time.Now())
}

// SetUsageSnapshot5hAt 更新 5h 用量快照及刷新时间
func (a *Account) SetUsageSnapshot5hAt(pct float64, resetAt time.Time, updatedAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent5h = pct
	a.UsagePercent5hValid = true
	a.Reset5hAt = resetAt
	a.UsageUpdatedAt5h = updatedAt
}

// GetUsagePercent5h 获取 5h 用量百分比
func (a *Account) GetUsagePercent5h() (float64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsagePercent5h, a.UsagePercent5hValid
}

// SetRateLimitResetCredits 记录账号剩余的「主动重置次数」。
func (a *Account) SetRateLimitResetCredits(count int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if count < 0 {
		count = 0
	}
	a.RateLimitResetCredits = count
	a.RateLimitResetCreditsValid = true
}

// GetRateLimitResetCredits 返回账号剩余的「主动重置次数」及其是否已探测过。
func (a *Account) GetRateLimitResetCredits() (int, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RateLimitResetCredits, a.RateLimitResetCreditsValid
}

// MarkResetCreditsProbed 记录最近一次成功 wham 用量探针的时间。
// 调用方应在 wham 探针成功（拿到 usage）后调用，无论本次响应是否带 reset_credits 字段，
// 因为「能成功拉到 wham」本身就代表重置次数已是最新。
func (a *Account) MarkResetCreditsProbed(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resetCreditsProbedAt = t
}

// ClearUsageCache 清除内存中的用量缓存，下次请求时从上游重新获取
func (a *Account) ClearUsageCache() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.UsagePercent7d = 0
	a.UsagePercent7dValid = false
	a.Reset7dAt = time.Time{}
	a.UsagePercent5h = 0
	a.UsagePercent5hValid = false
	a.Reset5hAt = time.Time{}
	a.UsageUpdatedAt = time.Time{}
	a.UsageUpdatedAt5h = time.Time{}
}

// SetReset7dAt 设置 7d 窗口重置时间
func (a *Account) SetReset7dAt(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Reset7dAt = t
}

// SetWindow7dSeconds 记录长窗口(7d 槽)的真实周期秒数。仅在拿到有效长度(>0)时写入，
// 避免不知道长度的路径(载入/种子)用 0 覆盖已探测到的真实值。
func (a *Account) SetWindow7dSeconds(sec int64) {
	if sec <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Window7dSeconds = sec
}

// GetWindow7dSeconds 返回长窗口(7d 槽)的真实周期秒数(0=未知)。
func (a *Account) GetWindow7dSeconds() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Window7dSeconds
}

// Window7dKind 返回长窗口(7d 槽)的类型标签："monthly"(free/team 月窗)/"weekly"/""(未知)，
// 供管理端把进度条标成「30天」而非误标「7天」(issue #324)。判据与 wham 分类的月窗容差一致。
func (a *Account) Window7dKind() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case a.Window7dSeconds <= 0:
		return ""
	case isMonthlyWindowSeconds(a.Window7dSeconds):
		return "monthly"
	default:
		return "weekly"
	}
}

// GetReset5hAt 获取 5h 窗口重置时间
func (a *Account) GetReset5hAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Reset5hAt
}

// GetReset7dAt 获取 7d 窗口重置时间
func (a *Account) GetReset7dAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Reset7dAt
}

// GetUsageUpdatedAt 获取 7d 用量快照刷新时间
func (a *Account) GetUsageUpdatedAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsageUpdatedAt
}

// GetUsageUpdatedAt5h 获取 5h 用量快照刷新时间
func (a *Account) GetUsageUpdatedAt5h() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UsageUpdatedAt5h
}

// GetPlanType 获取账号套餐类型
func (a *Account) GetPlanType() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.PlanType
}

// applyRefreshedPlanTypeLocked applies a plan parsed from refreshed tokens.
// Caller must hold a.mu.
func (a *Account) applyRefreshedPlanTypeLocked(planType string, now time.Time) (string, bool) {
	plan := strings.ToLower(strings.TrimSpace(planType))
	if plan == "" {
		return "", false
	}
	if plan != "free" &&
		strings.EqualFold(a.PlanType, "free") &&
		a.UsagePercent7dValid &&
		a.Reset7dAt.After(now) {
		return plan, false
	}
	a.PlanType = plan
	return plan, true
}

// GetHealthTier 获取当前健康层级
func (a *Account) GetHealthTier() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return string(a.HealthTier)
}

// GetSchedulerScore 获取当前调度分
func (a *Account) GetSchedulerScore() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.SchedulerScore
}

// GetDispatchScore 获取当前用于排序的调度分
func (a *Account) GetDispatchScore() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.DispatchScore
}

// GetScoreBiasOverride 获取账号级分数 override
func (a *Account) GetScoreBiasOverride() (int64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.ScoreBiasOverride == nil {
		return 0, false
	}
	return *a.ScoreBiasOverride, true
}

// GetScoreBiasEffective 获取当前实际生效的 bonus
func (a *Account) GetScoreBiasEffective() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ScoreBiasEffective
}

// GetBaseConcurrencyOverride 获取账号级并发 override
func (a *Account) GetBaseConcurrencyOverride() (int64, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.BaseConcurrencyOverride == nil {
		return 0, false
	}
	return *a.BaseConcurrencyOverride, true
}

// GetBaseConcurrencyEffective 获取当前实际基础并发
func (a *Account) GetBaseConcurrencyEffective() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.BaseConcurrencyEffective
}

func (a *Account) setAllowedAPIKeyIDsLocked(values []int64) {
	normalized := normalizeAllowedAPIKeyIDs(values)
	a.AllowedAPIKeyIDs = cloneInt64Slice(normalized)
	if len(normalized) == 0 {
		a.allowedAPIKeySet = nil
		return
	}
	a.allowedAPIKeySet = make(map[int64]struct{}, len(normalized))
	for _, value := range normalized {
		a.allowedAPIKeySet[value] = struct{}{}
	}
}

func (a *Account) SetAllowedAPIKeyIDs(values []int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setAllowedAPIKeyIDsLocked(values)
}

func (a *Account) GetAllowedAPIKeyIDs() []int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneInt64Slice(a.AllowedAPIKeyIDs)
}

func (a *Account) AllowsAPIKey(apiKeyID int64) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if len(a.AllowedAPIKeyIDs) == 0 {
		return true
	}
	if apiKeyID <= 0 {
		return false
	}
	_, ok := a.allowedAPIKeySet[apiKeyID]
	return ok
}

func normalizeModelCooldownKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func (a *Account) SetModelCooldownUntil(model, reason string, resetAt time.Time) ModelCooldown {
	key := normalizeModelCooldownKey(model)
	if key == "" || resetAt.IsZero() {
		return ModelCooldown{}
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "rate_limited"
	}
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ModelCooldowns == nil {
		a.ModelCooldowns = make(map[string]ModelCooldown)
	}
	current := a.ModelCooldowns[key]
	level := current.BackoffLevel
	if current.ResetAt.After(now) {
		level++
	}
	if level < 0 {
		level = 0
	}
	cooldown := ModelCooldown{
		Model:        key,
		Reason:       reason,
		ResetAt:      resetAt,
		UpdatedAt:    now,
		BackoffLevel: level,
	}
	a.ModelCooldowns[key] = cooldown
	return cooldown
}

func (a *Account) RestoreModelCooldown(model, reason string, resetAt, updatedAt time.Time) {
	key := normalizeModelCooldownKey(model)
	if key == "" || resetAt.IsZero() || !resetAt.After(time.Now()) {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "rate_limited"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ModelCooldowns == nil {
		a.ModelCooldowns = make(map[string]ModelCooldown)
	}
	a.ModelCooldowns[key] = ModelCooldown{
		Model:     key,
		Reason:    reason,
		ResetAt:   resetAt,
		UpdatedAt: updatedAt,
	}
}

func (a *Account) IsModelRateLimited(model string) bool {
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	cooldown, ok := a.ModelCooldowns[key]
	return ok && cooldown.ResetAt.After(time.Now())
}

func (a *Account) ModelCooldownRemaining(model string) time.Duration {
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return 0
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	cooldown, ok := a.ModelCooldowns[key]
	if !ok {
		return 0
	}
	remaining := time.Until(cooldown.ResetAt)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func (a *Account) ActiveModelCooldowns() []ModelCooldown {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.ModelCooldowns) == 0 {
		return nil
	}
	now := time.Now()
	result := make([]ModelCooldown, 0, len(a.ModelCooldowns))
	for _, cooldown := range a.ModelCooldowns {
		if cooldown.ResetAt.After(now) {
			result = append(result, cooldown)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Model < result[j].Model
	})
	return result
}

func (a *Account) ClearModelCooldown(model string) bool {
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.ModelCooldowns) == 0 {
		return false
	}
	if _, ok := a.ModelCooldowns[key]; !ok {
		return false
	}
	delete(a.ModelCooldowns, key)
	return true
}

// GetDynamicConcurrencyLimit 获取当前动态并发上限
func (a *Account) GetDynamicConcurrencyLimit() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.DynamicConcurrencyLimit
}

// GetSchedulerDebugSnapshot 获取调度调试快照
func (a *Account) GetSchedulerDebugSnapshot(baseLimit int64) SchedulerDebugSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.recomputeSchedulerLocked(baseLimit)
	now := time.Now()
	breakdown := a.schedulerBreakdownLocked(now)
	if a.dispatchBonusEligibleLocked(now, a.HealthTier) {
		breakdown.UsageUrgencyBonus5h = a.premium5hUsageUrgencyBonusLocked(now)
		breakdown.UsageUrgencyBonus7d = a.premium7dUsageUrgencyBonusLocked(now)
		breakdown.ExpiryUrgencyBonus = a.expiryUrgencyBonusLocked(now)
	}
	return SchedulerDebugSnapshot{
		HealthTier:               string(a.HealthTier),
		SchedulerScore:           a.SchedulerScore,
		DispatchScore:            a.DispatchScore,
		ScoreBiasOverride:        cloneInt64Ptr(a.ScoreBiasOverride),
		ScoreBiasEffective:       a.ScoreBiasEffective,
		BaseConcurrencyOverride:  cloneInt64Ptr(a.BaseConcurrencyOverride),
		BaseConcurrencyEffective: a.BaseConcurrencyEffective,
		DynamicConcurrencyLimit:  a.DynamicConcurrencyLimit,
		Breakdown:                breakdown,
		LastUnauthorizedAt:       a.LastUnauthorizedAt,
		LastRateLimitedAt:        a.LastRateLimitedAt,
		LastTimeoutAt:            a.LastTimeoutAt,
		LastServerErrorAt:        a.LastServerErrorAt,
	}
}

// NeedsUsageProbe 判断是否需要主动探针刷新用量
func (a *Account) NeedsUsageProbe(maxAge time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()

	if a.usageProbeInFlight || a.AccessToken == "" || a.Status == StatusError {
		return false
	}
	if a.Status == StatusCooldown && a.CooldownReason == "unauthorized" && (a.CooldownUtil.IsZero() || now.Before(a.CooldownUtil)) {
		return false // token 失效，wham 也会 401，探针无意义
	}

	// 「主动重置次数」只能由 wham 探针刷新（普通 /responses 流量不携带该字段），
	// 因此用独立的 resetCreditsProbedAt 判断它是否过期。否则活跃账号的用量快照被
	// 业务流量持续刷新，会让用量看起来一直"新鲜"，从而长期不触发 wham 探针、
	// 重置次数迟迟探测不出来。
	resetCreditsStale := a.resetCreditsProbedAt.IsZero() || now.Sub(a.resetCreditsProbedAt) > maxAge

	if a.premium5hRateLimitedLocked(now) {
		// premium 5h 限流期间不发 /responses 探活，但 wham 零成本，仍允许其刷新重置次数。
		return resetCreditsStale
	}
	if a.Status == StatusCooldown && a.CooldownReason == "rate_limited" && (a.CooldownUtil.IsZero() || now.Before(a.CooldownUtil)) {
		// 429 冷却期间不发 /responses 探活（避免加重限流），但允许 wham-only 探针刷新重置次数——
		// 这正是用户最需要看到"还剩几次主动重置"的时刻。
		return resetCreditsStale
	}
	if resetCreditsStale {
		return true
	}
	if !a.UsagePercent7dValid || a.UsageUpdatedAt.IsZero() || now.Sub(a.UsageUpdatedAt) > maxAge {
		return true
	}
	// 5h / 7d 窗口重置时刻一到就立即探测一次（issue：倒计时归零后账号看似恢复"可用"，
	// 但上游可能已被封禁）。判据：用量快照的采集时间早于重置时刻 => 展示的是重置前的
	// 过期数据 => 需要一次 wham 探测确认真实状态，而不是盲目放行。探测成功后
	// UsageUpdatedAt* 会晚于（旧）重置时刻，条件自然不再成立——每个重置边界只探一次，
	// 不受 maxAge 延迟影响（比下面 maxAge 限速的兜底更及时）。
	if a.UsagePercent5hValid && !a.Reset5hAt.IsZero() && !a.Reset5hAt.After(now) && a.UsageUpdatedAt5h.Before(a.Reset5hAt) {
		return true
	}
	if a.UsagePercent7dValid && !a.Reset7dAt.IsZero() && !a.Reset7dAt.After(now) && a.UsageUpdatedAt.Before(a.Reset7dAt) {
		return true
	}
	if a.effectiveAutoPause5h > 0 && !a.AutoPause5hDisabled {
		if !a.UsagePercent5hValid || a.UsageUpdatedAt5h.IsZero() {
			return true
		}
		if a.Reset5hAt.IsZero() || a.Reset5hAt.After(now) {
			return now.Sub(a.UsageUpdatedAt5h) > maxAge
		}
	}
	// 5h 用量窗口的重置时间已过、但快照仍停留在重置前采集的高用量（展示的是过期数据）→
	// 触发一次 wham 刷新，让 5h 进度条与 premium 5h 限流冷却跟随官方窗口重置而恢复。
	// （7d 窗口的过期数据已被上面的 7d 新鲜度检查覆盖；5h 检查此前仅在 Reset5hAt 未过期时生效，
	// 重置后会一直停在旧值，这里补上。）
	// now.Sub(UsageUpdatedAt5h) > maxAge 既能在窗口重置后尽快触发，也能在上游偶尔不返回该窗口时
	// 限制探测频率，避免反复探针。
	if a.UsagePercent5hValid && a.UsagePercent5h > 0 && !a.Reset5hAt.IsZero() &&
		!a.Reset5hAt.After(now) && now.Sub(a.UsageUpdatedAt5h) > maxAge {
		return true
	}
	return false
}

// nextProbeBoundary 返回该账号「到点即应触发 wham 探针」的最近未来时刻：
//   - 5h / 7d 窗口重置：快照仍停在重置前采集的数据，窗口一翻新就该刷新进度条；
//   - 限流冷却结束（非 unauthorized——那类探针会 401 无意义）：恢复可用的瞬间确认真实用量/状态。
//
// 只返回严格晚于 now 的时刻；这些时刻正是 NeedsUsageProbe 的重置/冷却判据会翻转为
// true 的边界，因此 Store 在此刻精确探针一次即可命中，无需等巡检周期。
func (a *Account) nextProbeBoundary(now time.Time) (time.Time, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.AccessToken == "" || a.Status == StatusError {
		return time.Time{}, false
	}
	var next time.Time
	consider := func(t time.Time) {
		if t.IsZero() || !t.After(now) {
			return
		}
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	if a.UsagePercent5hValid && a.UsageUpdatedAt5h.Before(a.Reset5hAt) {
		consider(a.Reset5hAt)
	}
	if a.UsagePercent7dValid && a.UsageUpdatedAt.Before(a.Reset7dAt) {
		consider(a.Reset7dAt)
	}
	if a.Status == StatusCooldown && a.CooldownReason != "unauthorized" {
		consider(a.CooldownUtil)
	}
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}

// InLimitedState 报告账号是否处于"应避免 /responses 探活"的限流/冷却状态
// （429 冷却或 premium 5h 限流）。此时用量探针应只走 wham（零成本），
// 失败也不回退 /responses，避免加重限流或消耗额度。
// 注意：unauthorized 冷却不在此列——那类账号 NeedsUsageProbe 已直接跳过。
func (a *Account) InLimitedState() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	now := time.Now()
	if a.premium5hRateLimitedLocked(now) {
		return true
	}
	if a.Status == StatusCooldown && a.CooldownReason == "rate_limited" && (a.CooldownUtil.IsZero() || now.Before(a.CooldownUtil)) {
		return true
	}
	return false
}

// TryBeginUsageProbe 尝试开始一次用量探针
func (a *Account) TryBeginUsageProbe() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usageProbeInFlight {
		return false
	}
	a.usageProbeInFlight = true
	return true
}

// FinishUsageProbe 结束一次用量探针
func (a *Account) FinishUsageProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.usageProbeInFlight = false
}

// NeedsRecoveryProbe 判断是否需要对被封禁账号做低频恢复探测
func (a *Account) NeedsRecoveryProbe(minInterval time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.recoveryProbeInFlight || a.healthTierLocked() != HealthTierBanned {
		return false
	}
	if a.RefreshToken == "" {
		return false
	}
	if a.Status == StatusCooldown && time.Now().Before(a.CooldownUtil) {
		return false
	}
	if !a.LastRecoveryProbeAt.IsZero() && time.Since(a.LastRecoveryProbeAt) < minInterval {
		return false
	}
	return true
}

// TryBeginRecoveryProbe 尝试开始一次恢复探测
func (a *Account) TryBeginRecoveryProbe() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recoveryProbeInFlight {
		return false
	}
	a.recoveryProbeInFlight = true
	a.LastRecoveryProbeAt = time.Now()
	return true
}

// FinishRecoveryProbe 结束一次恢复探测
func (a *Account) FinishRecoveryProbe() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recoveryProbeInFlight = false
}

// GetActiveRequests 获取当前并发数
func (a *Account) GetActiveRequests() int64 {
	return atomic.LoadInt64(&a.ActiveRequests)
}

// GetTotalRequests 获取累计请求数
func (a *Account) GetTotalRequests() int64 {
	return atomic.LoadInt64(&a.TotalRequests)
}

// GetLastUsedAt 获取最后使用时间
func (a *Account) GetLastUsedAt() time.Time {
	nano := atomic.LoadInt64(&a.LastUsedAt)
	if nano == 0 {
		return time.Time{}
	}
	return time.Unix(0, nano)
}

// Store 多账号管理器（数据库 + Token 缓存）
type Store struct {
	mu                                 sync.RWMutex
	accounts                           []*Account
	accountsByID                       map[int64]*Account // DBID -> Account 索引，与 accounts 同步维护，供 O(1) 查找
	globalProxy                        string
	maxConcurrency                     int64        // 每账号最大并发数
	testConcurrency                    int64        // 批量测试并发数
	testModel                          atomic.Value // 测试连接使用的模型（string）
	testContent                        atomic.Value // 测试连接使用的输入内容（string）
	db                                 *database.DB
	tokenCache                         cache.TokenCache
	apiKeyGroupsMu                     sync.RWMutex
	apiKeyAllowedGroups                map[int64][]int64
	apiKeyAllowedGroupSets             map[int64]map[int64]struct{}
	apiKeyAllowedPlans                 map[int64][]string
	apiKeyAllowedPlanSets              map[int64]map[string]struct{}
	usageProbeMu                       sync.RWMutex
	usageProbe                         func(context.Context, *Account) error
	usageProbeBatch                    atomic.Bool
	recoveryProbeBatch                 atomic.Bool
	autoCleanUnauthorized              atomic.Bool
	autoCleanRateLimited               atomic.Bool
	autoCleanFullUsage                 atomic.Bool
	autoCleanError                     atomic.Bool
	autoCleanExpired                   atomic.Bool
	lazyMode                           atomic.Bool
	autoCleanupBatch                   atomic.Bool
	maxRetries                         int64 // 请求失败最大重试次数（换号重试）
	maxRateLimitRetries                int64 // 429 最大换号重试次数
	backgroundRefreshInterval          int64 // 后台刷新/探针巡检间隔（ns）
	usageProbeMaxAge                   int64 // 用量探针快照最大缓存时长（ns）
	usageProbeConcurrency              int64 // 用量探针并行度
	usageProbeResponsesFallbackEnabled atomic.Bool
	recoveryProbeInterval              int64 // 恢复探测最小间隔（ns）
	backgroundRefreshWakeCh            chan struct{}
	// 到点即探：限流冷却 / 5h·7d 窗口重置的倒计时归零那一刻，精确唤醒一次 wham 探针，
	// 让用量进度条随官方窗口翻新立即刷新，而不是干等下一个巡检周期。
	// boundaryProbeWakeCh 由 wakeBoundaryProbe 非阻塞写入（任何锁下都安全），
	// 后台 goroutine 收到后全量扫描各账号最近边界并重排单个定时器。
	// armedBoundaryAt 记录当前已武装的最近边界（UnixNano，0=未武装），
	// 供 wakeBoundaryProbe 判断「新边界是否更早、值不值得打扰」。
	boundaryProbeWakeCh chan struct{}
	armedBoundaryAt     int64
	lazyRefreshInFlight sync.Map
	stopCh              chan struct{}
	stopOnce            sync.Once
	wg                  sync.WaitGroup

	// 代理池
	proxyPool        []string // 已启用的代理 URL 列表
	proxyPoolEnabled bool     // 代理池是否开启
	proxyRoundRobin  uint64   // 轮询计数器

	// Fast scheduler POC（默认关闭，通过环境变量启用）
	fastScheduler        atomic.Pointer[FastScheduler]
	fastSchedulerEnabled atomic.Bool

	// Codex 上游 WebSocket 相关（默认全部关闭，不影响现有 HTTP 路径）
	codexForceWebsocket         atomic.Bool  // 强制 Codex 上游走 WebSocket（复用连接池）
	codexWSKeepaliveEnabled     atomic.Bool  // 启用上游 WS 空闲连接保活（仅 Ping）
	codexWSKeepaliveIntervalSec atomic.Int64 // WS 保活 Ping 间隔（秒），默认 60
	codexWSHideUpstreamErrors   atomic.Bool  // 隐藏上游 WS 原始错误，默认开启
	codexWSSilentRetryEnabled   atomic.Bool  // 首包前上游 WS 错误静默换号重试，默认开启
	codexWSSilentMaxRetries     atomic.Int64 // WS 静默换号最大重试次数，默认 2

	// Codex 思考截断自动续想（默认关闭，不影响现有路径）
	codexContinueThinkingEnabled atomic.Bool  // 检测到上游截断思考时自动续想并折叠成单响应
	codexContinueMaxRounds       atomic.Int64 // 单次请求最大续想轮数（含首轮），默认 8
	codexCLIVersionSyncEnabled   atomic.Bool  // 后台定时同步 Codex CLI 模拟版本，默认 true
	codexCLIVersionSyncInterval  atomic.Int64 // 定时同步间隔（小时），默认 12
	ignoreUsageLimitStatus       atomic.Bool  // 用量窗口只记录，不作为账号不可用证据

	// 重试间隔与传输错误重试策略（issue #331）
	retryIntervalMS      atomic.Int64 // 重试间隔毫秒，0 = 立即重试（旧行为）
	transportRetryPolicy atomic.Value // 传输错误重试策略: rotate / sticky

	// 智能刷新调度器
	refreshScheduler atomic.Pointer[RefreshSchedulerIntegration]

	allowRemoteMigration  atomic.Bool  // 是否允许远程迁移拉取账号
	modelMapping          atomic.Value // 模型映射 JSON 字符串
	codexModelMapping     atomic.Value // Codex 模型映射 JSON 字符串
	reasoningEffortModels atomic.Value // 带思考强度的模型别名 JSON 数组
	schedulerMode         atomic.Value // string: "round_robin" or "remaining_quota"
	affinityMode          atomic.Value // string: "bounded" / "off" / "strict"
	promptFilterConfig    atomic.Value // promptfilter.Config
	sessionMu             sync.RWMutex
	sessionBindings       map[string]sessionAffinity

	globalAutoPause5hThreshold    float64  // protected by mu
	globalAutoPause7dThreshold    float64  // protected by mu
	autoPause5hGuardBandPercent   float64  // protected by mu, percentage points
	autoPause5hGuardConcurrency   int      // protected by mu, 0 = disabled
	smartPacingEnabled            bool     // protected by mu; issue #312 智能配速总开关
	smartPacingMinConcurrency     int      // protected by mu, 配速并发下限
	smartPacingWindows            string   // protected by mu, "5h,7d" / "5h" / "7d"
	groupAutoPauseThresholds      sync.Map // int64 -> [2]float64 {5h, 7d}
	groupBaseConcurrencyOverrides sync.Map // int64 -> int64; missing means inherit global
}

// sessionAffinity 记录某个 sessionKey 当前粘附到哪个账号/代理。
//
// boundAt / requestCount 用于 bounded affinity 的逃逸条件:
//   - 累计请求超过 maxAffinityRequests 后强制解绑,避免单账号被一直薅
//   - 绑定时长超过 maxAffinityDuration 后同样解绑
//   - 上层在选号时还会检查"绑定账号当前是否还健康",非 healthy 直接换号
//
// strict 模式不读这些字段(行为退化为旧实现);off 模式根本不进入这条路径。
type sessionAffinity struct {
	accountID    int64
	proxyURL     string
	boundAt      time.Time
	requestCount int64
	expiresAt    time.Time
}

const defaultSessionAffinityTTL = time.Hour

// maxSessionBindings 会话粘性表的软上限。超限时在 bind 路径全量清一轮过期项。
const maxSessionBindings = 65536

// Bounded affinity 默认阈值。命中任一即触发解绑下次走完整挑号策略。
const (
	defaultMaxAffinityRequests = 50
	defaultMaxAffinityDuration = 5 * time.Minute
)

// Affinity 模式常量。affinity_mode 系统设置使用以下值。
const (
	AffinityModeBounded = "bounded" // 默认。粘性但有逃逸条件
	AffinityModeOff     = "off"     // 关闭粘性。每次都按调度策略重新挑号
	AffinityModeStrict  = "strict"  // 旧行为。粘到底,直到 TTL 过期或账号失败
)

const (
	accountCooldownCacheNamespace = "account-cooldown"
	modelCooldownCacheNamespace   = "model-cooldown"
	runtimeCooldownCacheTimeout   = 300 * time.Millisecond
)

type runtimeCooldownRecord struct {
	Model        string    `json:"model,omitempty"`
	Reason       string    `json:"reason"`
	ResetAt      time.Time `json:"reset_at"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
	BackoffLevel int       `json:"backoff_level,omitempty"`
}

func sessionAffinityTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CODEX_SESSION_AFFINITY_TTL"))
	if raw == "" {
		return defaultSessionAffinityTTL
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultSessionAffinityTTL
}

func cooldownRuntimeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), runtimeCooldownCacheTimeout)
}

func accountCooldownRuntimeKey(accountID int64) string {
	return strconv.FormatInt(accountID, 10)
}

func modelCooldownRuntimeKey(accountID int64, model string) string {
	return fmt.Sprintf("%d:%s", accountID, normalizeModelCooldownKey(model))
}

func normalizeCooldownReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "rate_limited"
	}
	return reason
}

func cooldownTTL(resetAt time.Time) (time.Duration, bool) {
	if resetAt.IsZero() {
		return 0, false
	}
	ttl := time.Until(resetAt)
	if ttl <= 0 {
		return 0, false
	}
	return ttl, true
}

func (s *Store) setCachedAccountCooldown(accountID int64, reason string, resetAt time.Time) {
	// 所有冷却设置（429 / premium 5h / usage_limit）都经此漏斗——在这里挂「到点即探」唤醒：
	// 冷却倒计时归零那一刻精确探针一次，刷新用量进度条。unauthorized 除外（探针必 401，无意义）。
	if normalizeCooldownReason(reason) != "unauthorized" {
		s.WakeBoundaryProbe(resetAt)
	}
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return
	}
	ttl, ok := cooldownTTL(resetAt)
	if !ok {
		return
	}
	payload, err := json.Marshal(runtimeCooldownRecord{
		Reason:    normalizeCooldownReason(reason),
		ResetAt:   resetAt,
		UpdatedAt: time.Now(),
	})
	if err != nil {
		log.Printf("[账号 %d] 序列化账号冷却缓存失败: %v", accountID, err)
		return
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	if err := s.tokenCache.SetRuntime(ctx, accountCooldownCacheNamespace, accountCooldownRuntimeKey(accountID), payload, ttl); err != nil {
		log.Printf("[账号 %d] 写入账号冷却缓存失败: %v", accountID, err)
	}
}

func (s *Store) getCachedAccountCooldown(accountID int64) (runtimeCooldownRecord, bool) {
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return runtimeCooldownRecord{}, false
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	payload, ok, err := s.tokenCache.GetRuntime(ctx, accountCooldownCacheNamespace, accountCooldownRuntimeKey(accountID))
	if err != nil {
		log.Printf("[账号 %d] 读取账号冷却缓存失败: %v", accountID, err)
		return runtimeCooldownRecord{}, false
	}
	if !ok || len(payload) == 0 {
		return runtimeCooldownRecord{}, false
	}
	var record runtimeCooldownRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		log.Printf("[账号 %d] 解析账号冷却缓存失败: %v", accountID, err)
		s.deleteCachedAccountCooldown(accountID)
		return runtimeCooldownRecord{}, false
	}
	if !record.ResetAt.After(time.Now()) {
		s.deleteCachedAccountCooldown(accountID)
		return runtimeCooldownRecord{}, false
	}
	record.Reason = normalizeCooldownReason(record.Reason)
	return record, true
}

func (s *Store) deleteCachedAccountCooldown(accountID int64) {
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	if err := s.tokenCache.DeleteRuntime(ctx, accountCooldownCacheNamespace, accountCooldownRuntimeKey(accountID)); err != nil {
		log.Printf("[账号 %d] 删除账号冷却缓存失败: %v", accountID, err)
	}
}

func (s *Store) applyCachedAccountCooldown(acc *Account, record runtimeCooldownRecord) {
	if s == nil || acc == nil || !record.ResetAt.After(time.Now()) {
		return
	}
	reason := normalizeCooldownReason(record.Reason)
	baseLimit := atomic.LoadInt64(&s.maxConcurrency)
	acc.mu.Lock()
	acc.Status = StatusCooldown
	acc.CooldownUtil = record.ResetAt
	acc.CooldownReason = reason
	now := time.Now()
	switch reason {
	case "unauthorized":
		acc.LastUnauthorizedAt = now
		acc.LastFailureAt = now
		acc.HealthTier = HealthTierBanned
	case "rate_limited", "usage_limited", "usage_limit":
		acc.LastRateLimitedAt = now
		acc.LastFailureAt = now
		if acc.healthTierLocked() == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else if acc.HealthTier != HealthTierBanned {
			acc.HealthTier = HealthTierRisky
		}
	}
	acc.recomputeSchedulerLocked(baseLimit)
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
}

func (s *Store) accountHasCachedCooldown(acc *Account) bool {
	if acc == nil {
		return false
	}
	record, ok := s.getCachedAccountCooldown(acc.DBID)
	if !ok {
		return false
	}
	s.applyCachedAccountCooldown(acc, record)
	return true
}

func (s *Store) setCachedModelCooldown(accountID int64, cooldown ModelCooldown) {
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return
	}
	key := normalizeModelCooldownKey(cooldown.Model)
	if key == "" {
		return
	}
	ttl, ok := cooldownTTL(cooldown.ResetAt)
	if !ok {
		return
	}
	payload, err := json.Marshal(runtimeCooldownRecord{
		Model:        key,
		Reason:       normalizeCooldownReason(cooldown.Reason),
		ResetAt:      cooldown.ResetAt,
		UpdatedAt:    cooldown.UpdatedAt,
		BackoffLevel: cooldown.BackoffLevel,
	})
	if err != nil {
		log.Printf("[账号 %d] 序列化模型冷却缓存失败 model=%s: %v", accountID, key, err)
		return
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	if err := s.tokenCache.SetRuntime(ctx, modelCooldownCacheNamespace, modelCooldownRuntimeKey(accountID, key), payload, ttl); err != nil {
		log.Printf("[账号 %d] 写入模型冷却缓存失败 model=%s: %v", accountID, key, err)
	}
}

func (s *Store) getCachedModelCooldown(accountID int64, model string) (runtimeCooldownRecord, bool) {
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return runtimeCooldownRecord{}, false
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return runtimeCooldownRecord{}, false
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	payload, ok, err := s.tokenCache.GetRuntime(ctx, modelCooldownCacheNamespace, modelCooldownRuntimeKey(accountID, key))
	if err != nil {
		log.Printf("[账号 %d] 读取模型冷却缓存失败 model=%s: %v", accountID, key, err)
		return runtimeCooldownRecord{}, false
	}
	if !ok || len(payload) == 0 {
		return runtimeCooldownRecord{}, false
	}
	var record runtimeCooldownRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		log.Printf("[账号 %d] 解析模型冷却缓存失败 model=%s: %v", accountID, key, err)
		s.deleteCachedModelCooldown(accountID, key)
		return runtimeCooldownRecord{}, false
	}
	if !record.ResetAt.After(time.Now()) {
		s.deleteCachedModelCooldown(accountID, key)
		return runtimeCooldownRecord{}, false
	}
	record.Model = key
	record.Reason = normalizeCooldownReason(record.Reason)
	return record, true
}

func (s *Store) deleteCachedModelCooldown(accountID int64, model string) {
	if s == nil || s.tokenCache == nil || accountID == 0 {
		return
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return
	}
	ctx, cancel := cooldownRuntimeContext()
	defer cancel()
	if err := s.tokenCache.DeleteRuntime(ctx, modelCooldownCacheNamespace, modelCooldownRuntimeKey(accountID, key)); err != nil {
		log.Printf("[账号 %d] 删除模型冷却缓存失败 model=%s: %v", accountID, key, err)
	}
}

func (s *Store) applyCachedModelCooldown(acc *Account, model string, record runtimeCooldownRecord) {
	if acc == nil || !record.ResetAt.After(time.Now()) {
		return
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		key = normalizeModelCooldownKey(record.Model)
	}
	if key == "" {
		return
	}
	acc.mu.Lock()
	if acc.ModelCooldowns == nil {
		acc.ModelCooldowns = make(map[string]ModelCooldown)
	}
	acc.ModelCooldowns[key] = ModelCooldown{
		Model:        key,
		Reason:       normalizeCooldownReason(record.Reason),
		ResetAt:      record.ResetAt,
		UpdatedAt:    updatedAt,
		BackoffLevel: record.BackoffLevel,
	}
	acc.mu.Unlock()
}

func (s *Store) accountHasCachedModelCooldown(acc *Account, model string) bool {
	if acc == nil {
		return false
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return false
	}
	if acc.IsModelRateLimited(key) {
		return true
	}
	record, ok := s.getCachedModelCooldown(acc.DBID, key)
	if !ok {
		return false
	}
	s.applyCachedModelCooldown(acc, key, record)
	return true
}

// WithModelCooldownFilter wraps a request model filter with Redis-backed model cooldown checks.
func (s *Store) WithModelCooldownFilter(model string, filter AccountFilter) AccountFilter {
	key := normalizeModelCooldownKey(model)
	if s == nil || key == "" {
		return filter
	}
	return func(acc *Account) bool {
		if acc == nil {
			return false
		}
		if filter != nil && !filter(acc) {
			return false
		}
		return !s.accountHasCachedModelCooldown(acc, key)
	}
}

func fastSchedulerEnabledFromEnv() bool {
	for _, key := range []string{"FAST_SCHEDULER_ENABLED", "CODEX_FAST_SCHEDULER"} {
		if truthyEnv(os.Getenv(key)) {
			return true
		}
	}
	return false
}

func truthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	default:
		return false
	}
}

// NewStore 创建账号管理器
func NewStore(db *database.DB, tc cache.TokenCache, settings *database.SystemSettings) *Store {
	if settings == nil {
		settings = &database.SystemSettings{
			MaxConcurrency:                     2,
			TestConcurrency:                    50,
			TestModel:                          "gpt-5.4",
			TestContent:                        DefaultTestContent,
			BackgroundRefreshIntervalMinutes:   2,
			UsageProbeMaxAgeMinutes:            10,
			UsageProbeConcurrency:              defaultUsageProbeConcurrency,
			UsageProbeResponsesFallbackEnabled: true,
			RecoveryProbeIntervalMinutes:       30,
			LazyMode:                           false,
			ProxyURL:                           "",
			MaxRateLimitRetries:                1,
			SchedulerMode:                      "round_robin",
			CodexWSHideUpstreamErrors:          true,
			CodexWSSilentRetryEnabled:          true,
			CodexWSSilentMaxRetries:            2,
			CodexContinueMaxRounds:             8,
			AutoPause5hGuardBandPercent:        defaultAutoPause5hGuardBandPercent,
			AutoPause5hGuardConcurrency:        defaultAutoPause5hGuardConcurrency,
			SmartPacingMinConcurrency:          defaultSmartPacingMinConcurrency,
			SmartPacingWindows:                 "5h,7d",
		}
	}
	s := &Store{
		globalProxy:             settings.ProxyURL,
		maxConcurrency:          int64(settings.MaxConcurrency),
		testConcurrency:         int64(settings.TestConcurrency),
		db:                      db,
		tokenCache:              tc,
		backgroundRefreshWakeCh: make(chan struct{}, 1),
		boundaryProbeWakeCh:     make(chan struct{}, 1),
		stopCh:                  make(chan struct{}),
		proxyPoolEnabled:        settings.ProxyPoolEnabled,
		sessionBindings:         make(map[string]sessionAffinity),
	}
	s.testModel.Store(settings.TestModel)
	s.testContent.Store(NormalizeTestContent(settings.TestContent))
	s.SetBackgroundRefreshInterval(time.Duration(settings.BackgroundRefreshIntervalMinutes) * time.Minute)
	s.SetUsageProbeMaxAge(time.Duration(settings.UsageProbeMaxAgeMinutes) * time.Minute)
	s.SetUsageProbeConcurrency(settings.UsageProbeConcurrency)
	s.SetUsageProbeResponsesFallbackEnabled(settings.UsageProbeResponsesFallbackEnabled)
	s.SetRecoveryProbeInterval(time.Duration(settings.RecoveryProbeIntervalMinutes) * time.Minute)
	s.autoCleanUnauthorized.Store(settings.AutoCleanUnauthorized)
	s.autoCleanRateLimited.Store(settings.AutoCleanRateLimited)
	s.autoCleanFullUsage.Store(settings.AutoCleanFullUsage)
	s.autoCleanError.Store(settings.AutoCleanError)
	s.autoCleanExpired.Store(settings.AutoCleanExpired)
	s.lazyMode.Store(settings.LazyMode)
	retries := int64(settings.MaxRetries)
	if retries <= 0 {
		retries = 2 // 默认重试 2 次
	}
	atomic.StoreInt64(&s.maxRetries, retries)
	rateLimitRetries := int64(settings.MaxRateLimitRetries)
	if rateLimitRetries < 0 {
		rateLimitRetries = 0
	}
	atomic.StoreInt64(&s.maxRateLimitRetries, rateLimitRetries)
	s.allowRemoteMigration.Store(settings.AllowRemoteMigration)
	s.schedulerMode.Store(settings.SchedulerMode)
	s.SetAffinityMode(settings.AffinityMode)
	if settings.ModelMapping != "" {
		s.modelMapping.Store(settings.ModelMapping)
	}
	if settings.CodexModelMapping != "" {
		s.codexModelMapping.Store(settings.CodexModelMapping)
	}
	if settings.ReasoningEffortModels != "" {
		s.reasoningEffortModels.Store(settings.ReasoningEffortModels)
	}
	s.SetPromptFilterConfig(promptFilterConfigFromSettings(settings))
	// 环境变量优先，否则读数据库设置
	fastEnabled := fastSchedulerEnabledFromEnv() || settings.FastSchedulerEnabled
	s.fastSchedulerEnabled.Store(fastEnabled)
	if fastEnabled {
		scheduler := NewFastScheduler(int64(settings.MaxConcurrency), s.GetSchedulerMode())
		s.configureFastScheduler(scheduler)
		s.fastScheduler.Store(scheduler)
		log.Printf("快速调度器已启用（请求热路径将优先走本地内存调度器）")
	}

	// Codex 上游 WebSocket 相关设置（默认关闭，不影响现有路径）
	s.codexForceWebsocket.Store(settings.CodexForceWebsocket)
	s.codexWSKeepaliveEnabled.Store(settings.CodexWSKeepaliveEnabled)
	s.codexWSKeepaliveIntervalSec.Store(normalizeWSKeepaliveInterval(settings.CodexWSKeepaliveIntervalSec))
	s.codexWSHideUpstreamErrors.Store(settings.CodexWSHideUpstreamErrors)
	s.codexWSSilentRetryEnabled.Store(settings.CodexWSSilentRetryEnabled)
	s.codexWSSilentMaxRetries.Store(normalizeWSSilentMaxRetries(settings.CodexWSSilentMaxRetries))
	s.codexContinueThinkingEnabled.Store(settings.CodexContinueThinkingEnabled)
	s.codexContinueMaxRounds.Store(int64(database.NormalizeCodexContinueMaxRounds(settings.CodexContinueMaxRounds)))
	s.codexCLIVersionSyncEnabled.Store(settings.CodexCLIVersionSyncEnabled)
	s.codexCLIVersionSyncInterval.Store(int64(database.NormalizeCodexCLIVersionSyncIntervalHours(settings.CodexCLIVersionSyncIntervalHours)))
	s.ignoreUsageLimitStatus.Store(settings.IgnoreUsageLimitStatus)
	s.retryIntervalMS.Store(int64(normalizeRetryIntervalMS(settings.RetryIntervalMS)))
	s.transportRetryPolicy.Store(database.NormalizeTransportRetryPolicy(settings.TransportRetryPolicy))

	s.globalAutoPause5hThreshold = normalizeQuotaAutoPauseThreshold(settings.AutoPause5hThreshold)
	s.globalAutoPause7dThreshold = normalizeQuotaAutoPauseThreshold(settings.AutoPause7dThreshold)
	s.autoPause5hGuardBandPercent = normalizeAutoPause5hGuardBandPercent(settings.AutoPause5hGuardBandPercent)
	s.autoPause5hGuardConcurrency = normalizeAutoPause5hGuardConcurrency(settings.AutoPause5hGuardConcurrency)
	s.smartPacingEnabled = settings.SmartPacingEnabled
	s.smartPacingMinConcurrency = normalizeSmartPacingMinConcurrency(settings.SmartPacingMinConcurrency)
	s.smartPacingWindows = normalizeSmartPacingWindows(settings.SmartPacingWindows)

	// 加载代理池
	if settings.ProxyPoolEnabled {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if proxies, err := db.ListEnabledProxies(ctx); err == nil {
			urls := make([]string, 0, len(proxies))
			for _, p := range proxies {
				urls = append(urls, p.URL)
			}
			s.proxyPool = urls
			log.Printf("代理池已加载: %d 个活跃代理", len(urls))
		}
	}

	return s
}

func (s *Store) getFastScheduler() *FastScheduler {
	if s == nil || !s.fastSchedulerEnabled.Load() {
		return nil
	}
	return s.fastScheduler.Load()
}

func (s *Store) configureFastScheduler(scheduler *FastScheduler) {
	if s == nil || scheduler == nil {
		return
	}
	scheduler.SetGroupCheck(s.APIKeyAllowsAccount)
	scheduler.SetAcquireFunc(func(acc *Account, concurrencyLimit int64) bool {
		return s.tryAcquireAccount(acc, concurrencyLimit, false)
	})
}

func (s *Store) rebuildFastScheduler() {
	if s == nil || !s.fastSchedulerEnabled.Load() {
		return
	}
	scheduler := s.BuildFastScheduler()
	s.configureFastScheduler(scheduler)
	s.fastScheduler.Store(scheduler)
}

func (s *Store) recomputeAllAccountSchedulerState() {
	if s == nil {
		return
	}
	baseLimit := atomic.LoadInt64(&s.maxConcurrency)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, acc := range s.accounts {
		if acc == nil {
			continue
		}
		acc.mu.Lock()
		acc.recomputeSchedulerLocked(baseLimit)
		acc.mu.Unlock()
	}
}

func (s *Store) fastSchedulerUpdate(acc *Account) {
	if s == nil || acc == nil {
		return
	}
	scheduler := s.getFastScheduler()
	if scheduler == nil {
		return
	}
	scheduler.Update(acc)
}

func (s *Store) fastSchedulerRemove(dbID int64) {
	if s == nil || dbID == 0 {
		return
	}
	scheduler := s.getFastScheduler()
	if scheduler == nil {
		return
	}
	scheduler.Remove(dbID)
}

func (s *Store) SetFastSchedulerEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.fastSchedulerEnabled.Store(enabled)
	if enabled {
		s.recomputeAllAccountSchedulerState()
		s.rebuildFastScheduler()
		return
	}
	s.fastScheduler.Store(nil)
}

func (s *Store) FastSchedulerEnabled() bool {
	if s == nil {
		return false
	}
	return s.fastSchedulerEnabled.Load()
}

// normalizeWSKeepaliveInterval 把 WS 保活间隔(秒)归一,非正值 → 默认 60。
func normalizeWSKeepaliveInterval(sec int) int64 {
	if sec <= 0 {
		return 60
	}
	return int64(sec)
}

// normalizeWSSilentMaxRetries 把 WS 静默重试次数限制在 0-10。
func normalizeWSSilentMaxRetries(retries int) int64 {
	if retries < 0 {
		return 0
	}
	if retries > 10 {
		return 10
	}
	return int64(retries)
}

// SetCodexForceWebsocket 设置"强制 Codex 上游走 WebSocket"开关（运行时热更新）。
func (s *Store) SetCodexForceWebsocket(enabled bool) {
	if s == nil {
		return
	}
	s.codexForceWebsocket.Store(enabled)
}

// CodexForceWebsocket 返回是否强制 Codex 上游走 WebSocket。
func (s *Store) CodexForceWebsocket() bool {
	if s == nil {
		return false
	}
	return s.codexForceWebsocket.Load()
}

// SetCodexWSKeepaliveEnabled 设置上游 WS 空闲连接保活开关（运行时热更新）。
func (s *Store) SetCodexWSKeepaliveEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexWSKeepaliveEnabled.Store(enabled)
}

// CodexWSKeepaliveEnabled 返回是否启用上游 WS 连接保活。
func (s *Store) CodexWSKeepaliveEnabled() bool {
	if s == nil {
		return false
	}
	return s.codexWSKeepaliveEnabled.Load()
}

// SetCodexWSKeepaliveIntervalSec 设置 WS 保活 Ping 间隔（秒）。
func (s *Store) SetCodexWSKeepaliveIntervalSec(sec int) {
	if s == nil {
		return
	}
	s.codexWSKeepaliveIntervalSec.Store(normalizeWSKeepaliveInterval(sec))
}

// CodexWSKeepaliveIntervalSec 返回 WS 保活 Ping 间隔（秒），最小 60。
func (s *Store) CodexWSKeepaliveIntervalSec() int {
	if s == nil {
		return 60
	}
	v := s.codexWSKeepaliveIntervalSec.Load()
	if v <= 0 {
		return 60
	}
	return int(v)
}

// SetCodexWSHideUpstreamErrors 设置是否向客户端隐藏上游 WS 原始错误。
func (s *Store) SetCodexWSHideUpstreamErrors(enabled bool) {
	if s == nil {
		return
	}
	s.codexWSHideUpstreamErrors.Store(enabled)
}

// CodexWSHideUpstreamErrors 返回是否向客户端隐藏上游 WS 原始错误。
func (s *Store) CodexWSHideUpstreamErrors() bool {
	if s == nil {
		return true
	}
	return s.codexWSHideUpstreamErrors.Load()
}

// SetCodexWSSilentRetryEnabled 设置首包前 WS 上游错误是否静默换号重试。
func (s *Store) SetCodexWSSilentRetryEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexWSSilentRetryEnabled.Store(enabled)
}

// CodexWSSilentRetryEnabled 返回首包前 WS 上游错误是否静默换号重试。
func (s *Store) CodexWSSilentRetryEnabled() bool {
	if s == nil {
		return true
	}
	return s.codexWSSilentRetryEnabled.Load()
}

// SetCodexWSSilentMaxRetries 设置 WS 静默换号最大重试次数。
func (s *Store) SetCodexWSSilentMaxRetries(retries int) {
	if s == nil {
		return
	}
	s.codexWSSilentMaxRetries.Store(normalizeWSSilentMaxRetries(retries))
}

// CodexWSSilentMaxRetries 返回 WS 静默换号最大重试次数。
func (s *Store) CodexWSSilentMaxRetries() int {
	if s == nil {
		return 2
	}
	return int(s.codexWSSilentMaxRetries.Load())
}

// SetCodexContinueThinkingEnabled 设置是否在上游截断思考时自动续想。
func (s *Store) SetCodexContinueThinkingEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexContinueThinkingEnabled.Store(enabled)
}

// CodexContinueThinkingEnabled 返回是否在上游截断思考时自动续想。
func (s *Store) CodexContinueThinkingEnabled() bool {
	if s == nil {
		return false
	}
	return s.codexContinueThinkingEnabled.Load()
}

// SetCodexContinueMaxRounds 设置单次请求最大续想轮数（含首轮）。
func (s *Store) SetCodexContinueMaxRounds(rounds int) {
	if s == nil {
		return
	}
	s.codexContinueMaxRounds.Store(int64(database.NormalizeCodexContinueMaxRounds(rounds)))
}

// CodexContinueMaxRounds 返回单次请求最大续想轮数（含首轮）。
func (s *Store) CodexContinueMaxRounds() int {
	if s == nil {
		return 8
	}
	return int(s.codexContinueMaxRounds.Load())
}

// SetCodexCLIVersionSyncEnabled 设置是否后台定时同步 Codex CLI 模拟版本。
func (s *Store) SetCodexCLIVersionSyncEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.codexCLIVersionSyncEnabled.Store(enabled)
}

// CodexCLIVersionSyncEnabled 返回是否后台定时同步 Codex CLI 模拟版本。
func (s *Store) CodexCLIVersionSyncEnabled() bool {
	if s == nil {
		return true
	}
	return s.codexCLIVersionSyncEnabled.Load()
}

// SetCodexCLIVersionSyncIntervalHours 设置定时同步间隔（小时，钳到 1-720）。
func (s *Store) SetCodexCLIVersionSyncIntervalHours(hours int) {
	if s == nil {
		return
	}
	s.codexCLIVersionSyncInterval.Store(int64(database.NormalizeCodexCLIVersionSyncIntervalHours(hours)))
}

// CodexCLIVersionSyncIntervalHours 返回定时同步间隔（小时）。
func (s *Store) CodexCLIVersionSyncIntervalHours() int {
	if s == nil {
		return 12
	}
	return int(s.codexCLIVersionSyncInterval.Load())
}

// GetProxyURL 获取全局代理地址
func (s *Store) GetProxyURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.globalProxy
}

// SetProxyURL 更新全局代理地址
func (s *Store) SetProxyURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalProxy = url
}

// NextProxy 轮询获取下一个代理 URL
func (s *Store) NextProxy() string {
	s.mu.RLock()
	enabled := s.proxyPoolEnabled
	pool := s.proxyPool
	s.mu.RUnlock()

	if !enabled || len(pool) == 0 {
		return s.GetProxyURL() // fallback 全局单代理
	}
	idx := atomic.AddUint64(&s.proxyRoundRobin, 1)
	return pool[idx%uint64(len(pool))]
}

// ResolveProxyForAccount returns the effective proxy for account-bound internal calls.
// Priority: account proxy > sticky proxy pool > global proxy > direct.
func (s *Store) ResolveProxyForAccount(acc *Account) string {
	if s == nil {
		return ""
	}

	var accountID int64
	if acc != nil {
		acc.mu.RLock()
		accountID = acc.DBID
		if proxy := strings.TrimSpace(acc.ProxyURL); proxy != "" {
			acc.mu.RUnlock()
			return proxy
		}
		acc.mu.RUnlock()
	}

	return s.resolveFallbackProxyForAccount(accountID)
}

func (s *Store) resolveFallbackProxyForAccount(accountID int64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.proxyPoolEnabled && len(s.proxyPool) > 0 {
		start := stickyProxyIndex(accountID, len(s.proxyPool))
		for i := 0; i < len(s.proxyPool); i++ {
			if proxy := strings.TrimSpace(s.proxyPool[(start+i)%len(s.proxyPool)]); proxy != "" {
				return proxy
			}
		}
	}

	return strings.TrimSpace(s.globalProxy)
}

func stickyProxyIndex(accountID int64, poolSize int) int {
	if poolSize <= 1 {
		return 0
	}
	if accountID <= 0 {
		return 0
	}
	return int((accountID - 1) % int64(poolSize))
}

// GetProxyPoolEnabled 获取代理池开关状态
func (s *Store) GetProxyPoolEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxyPoolEnabled
}

// SetProxyPoolEnabled 设置代理池开关
func (s *Store) SetProxyPoolEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxyPoolEnabled = enabled
}

// ReloadProxyPool 从数据库重新加载代理池
func (s *Store) ReloadProxyPool() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxies, err := s.db.ListEnabledProxies(ctx)
	if err != nil {
		return err
	}
	urls := make([]string, 0, len(proxies))
	for _, p := range proxies {
		urls = append(urls, p.URL)
	}
	s.mu.Lock()
	s.proxyPool = urls
	s.mu.Unlock()
	log.Printf("代理池已重新加载: %d 个活跃代理", len(urls))
	return nil
}

// GetAutoCleanUnauthorized 获取是否自动清理 401 账号
func (s *Store) GetAutoCleanUnauthorized() bool {
	return s.autoCleanUnauthorized.Load()
}

// SetAutoCleanUnauthorized 设置是否自动清理 401 账号
func (s *Store) SetAutoCleanUnauthorized(enabled bool) {
	s.autoCleanUnauthorized.Store(enabled)
}

// GetAutoCleanRateLimited 获取是否自动清理 429 账号
func (s *Store) GetAutoCleanRateLimited() bool {
	return s.autoCleanRateLimited.Load()
}

// SetAutoCleanRateLimited 设置是否自动清理 429 账号
func (s *Store) SetAutoCleanRateLimited(enabled bool) {
	s.autoCleanRateLimited.Store(enabled)
}

// GetAutoCleanFullUsage 获取是否自动清理用量满的账号
func (s *Store) GetAutoCleanFullUsage() bool {
	return s.autoCleanFullUsage.Load()
}

// SetAutoCleanFullUsage 设置是否自动清理用量满的账号
func (s *Store) SetAutoCleanFullUsage(enabled bool) {
	s.autoCleanFullUsage.Store(enabled)
}

// GetAutoCleanError 获取是否自动清理 error 账号
func (s *Store) GetAutoCleanError() bool {
	return s.autoCleanError.Load()
}

// SetAutoCleanError 设置是否自动清理 error 账号
func (s *Store) SetAutoCleanError(enabled bool) {
	s.autoCleanError.Store(enabled)
}

// GetAutoCleanExpired 获取是否自动清理过期账号
func (s *Store) GetAutoCleanExpired() bool {
	return s.autoCleanExpired.Load()
}

// SetAutoCleanExpired 设置是否自动清理过期账号
func (s *Store) SetAutoCleanExpired(enabled bool) {
	s.autoCleanExpired.Store(enabled)
}

// GetLazyMode 获取是否启用惰性模式。
func (s *Store) GetLazyMode() bool {
	return s.lazyMode.Load()
}

// SetLazyMode 设置惰性模式。启用后不主动刷新/探测账号，只在调度命中时刷新 AT。
func (s *Store) SetLazyMode(enabled bool) {
	s.lazyMode.Store(enabled)
	s.rebuildFastScheduler()
}

// SetBackgroundRefreshInterval 设置后台刷新/探针巡检间隔。
func (s *Store) SetBackgroundRefreshInterval(d time.Duration) {
	if d <= 0 {
		d = defaultBackgroundRefreshInterval
	}
	atomic.StoreInt64(&s.backgroundRefreshInterval, int64(d))
	select {
	case s.backgroundRefreshWakeCh <- struct{}{}:
	default:
	}
}

// GetBackgroundRefreshInterval 获取后台刷新/探针巡检间隔。
func (s *Store) GetBackgroundRefreshInterval() time.Duration {
	d := time.Duration(atomic.LoadInt64(&s.backgroundRefreshInterval))
	if d <= 0 {
		return defaultBackgroundRefreshInterval
	}
	return d
}

// SetUsageProbeMaxAge 设置用量探针最大缓存时长。
func (s *Store) SetUsageProbeMaxAge(d time.Duration) {
	if d <= 0 {
		d = defaultUsageProbeMaxAge
	}
	atomic.StoreInt64(&s.usageProbeMaxAge, int64(d))
}

// GetUsageProbeMaxAge 获取用量探针最大缓存时长。
func (s *Store) GetUsageProbeMaxAge() time.Duration {
	d := time.Duration(atomic.LoadInt64(&s.usageProbeMaxAge))
	if d <= 0 {
		return defaultUsageProbeMaxAge
	}
	return d
}

// SetUsageProbeConcurrency 设置用量探针并行度。
func (s *Store) SetUsageProbeConcurrency(n int) {
	if n <= 0 {
		n = defaultUsageProbeConcurrency
	}
	if n > 128 {
		n = 128
	}
	atomic.StoreInt64(&s.usageProbeConcurrency, int64(n))
}

// GetUsageProbeConcurrency 获取用量探针并行度。
func (s *Store) GetUsageProbeConcurrency() int {
	n := int(atomic.LoadInt64(&s.usageProbeConcurrency))
	if n <= 0 {
		return defaultUsageProbeConcurrency
	}
	return n
}

// SetUsageProbeResponsesFallbackEnabled 设置 wham 失败后是否允许发送真实 /responses 探针。
func (s *Store) SetUsageProbeResponsesFallbackEnabled(enabled bool) {
	s.usageProbeResponsesFallbackEnabled.Store(enabled)
}

// UsageProbeResponsesFallbackEnabled 获取 wham 失败后是否允许发送真实 /responses 探针。
func (s *Store) UsageProbeResponsesFallbackEnabled() bool {
	if s == nil {
		return true
	}
	return s.usageProbeResponsesFallbackEnabled.Load()
}

// UsageProbeRunning reports whether a batch usage probe is currently active.
func (s *Store) UsageProbeRunning() bool {
	if s == nil {
		return false
	}
	return s.usageProbeBatch.Load()
}

// SetRecoveryProbeInterval 设置恢复探测最小间隔。
func (s *Store) SetRecoveryProbeInterval(d time.Duration) {
	if d <= 0 {
		d = defaultRecoveryProbeInterval
	}
	atomic.StoreInt64(&s.recoveryProbeInterval, int64(d))
}

// GetRecoveryProbeInterval 获取恢复探测最小间隔。
func (s *Store) GetRecoveryProbeInterval() time.Duration {
	d := time.Duration(atomic.LoadInt64(&s.recoveryProbeInterval))
	if d <= 0 {
		return defaultRecoveryProbeInterval
	}
	return d
}

// RecoveryProbeRunning reports whether a batch recovery probe is currently active.
func (s *Store) RecoveryProbeRunning() bool {
	if s == nil {
		return false
	}
	return s.recoveryProbeBatch.Load()
}

// AutoCleanupRunning reports whether an automatic cleanup pass is currently active.
func (s *Store) AutoCleanupRunning() bool {
	if s == nil {
		return false
	}
	return s.autoCleanupBatch.Load()
}

// CleanExpiredNow 立即执行一次过期清理，返回清理数量
func (s *Store) CleanExpiredNow() int {
	return s.CleanExpiredAccounts(context.Background(), 30*time.Minute)
}

// Init 初始化：从数据库加载账号
func (s *Store) Init(ctx context.Context) error {
	// 1. 从数据库加载账号到内存
	if err := s.loadFromDB(ctx); err != nil {
		return err
	}

	if len(s.accounts) == 0 {
		log.Println("⚠ 数据库中暂无账号，请通过管理后台添加")
		return nil
	}

	s.rebuildFastScheduler()

	// 2. 统计可用账号，RT 账号的刷新交给 StartBackgroundRefresh 处理
	available := 0
	for _, acc := range s.accounts {
		if acc.IsAvailable() {
			available++
		}
	}
	log.Printf("账号初始化完成: %d/%d 可用", available, len(s.accounts))
	return nil
}

// loadFromDB 从数据库加载账号
func (s *Store) loadFromDB(ctx context.Context) error {
	rows, err := s.db.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("从数据库加载账号失败: %w", err)
	}
	modelCooldowns := make(map[int64][]*database.AccountModelCooldownRow)
	if cooldownRows, err := s.db.ListActiveModelCooldowns(ctx); err == nil {
		for _, row := range cooldownRows {
			modelCooldowns[row.AccountID] = append(modelCooldowns[row.AccountID], row)
		}
	} else {
		log.Printf("加载模型冷却状态失败: %v", err)
	}
	if err := s.db.ClearExpiredModelCooldowns(ctx); err != nil {
		log.Printf("清理过期模型冷却状态失败: %v", err)
	}

	for _, row := range rows {
		account := s.buildAccountFromRow(ctx, row, modelCooldowns)
		if account == nil {
			continue
		}
		s.accounts = append(s.accounts, account)
	}

	s.rebuildAccountIndex()
	log.Printf("从数据库加载了 %d 个账号", len(s.accounts))
	if groups, err := s.db.ListAccountGroups(ctx); err == nil {
		for _, g := range groups {
			if g.AutoPause5hThreshold > 0 || g.AutoPause7dThreshold > 0 {
				s.groupAutoPauseThresholds.Store(g.ID, [2]float64{g.AutoPause5hThreshold, g.AutoPause7dThreshold})
			}
			if g.BaseConcurrencyOverride.Valid {
				s.groupBaseConcurrencyOverrides.Store(g.ID, g.BaseConcurrencyOverride.Int64)
			}
		}
	}
	if memberships, err := s.db.ListAccountGroupMemberships(ctx); err == nil {
		s.ApplyAccountGroupMemberships(memberships)
	} else {
		log.Printf("加载账号分组失败: %v", err)
	}
	if err := s.LoadAPIKeyAllowedGroups(ctx); err != nil {
		log.Printf("加载 API Key 分组限制失败: %v", err)
	}
	return nil
}

// buildAccountFromRow 将数据库账号行转换为运行时账号；凭据缺失或不可用时返回 nil。
func (s *Store) buildAccountFromRow(ctx context.Context, row *database.AccountRow, modelCooldowns map[int64][]*database.AccountModelCooldownRow) *Account {
	rt := row.GetCredential("refresh_token")
	st := row.GetCredential("session_token")
	at := row.GetCredential("access_token")
	upstreamType := row.GetCredential("upstream_type")
	baseURL := row.GetCredential("base_url")
	apiKey := row.GetCredential("api_key")
	models := normalizeModelList(row.GetCredentialStringSlice("models"))
	modelMapping := strings.TrimSpace(row.GetCredential("model_mapping"))
	codexClientMetadataMode := NormalizeCodexClientMetadataMode(row.GetCredential("codex_client_metadata_mode"))
	isOpenAIResponsesAccount := strings.EqualFold(strings.TrimSpace(upstreamType), UpstreamOpenAIResponses) && strings.TrimSpace(baseURL) != "" && strings.TrimSpace(apiKey) != ""
	if rt == "" && st == "" && at == "" && !isOpenAIResponsesAccount {
		log.Printf("[账号 %d] 缺少 refresh_token、session_token 和 access_token，跳过", row.ID)
		return nil
	}

	account := &Account{
		DBID:                    row.ID,
		RefreshToken:            rt,
		SessionToken:            st,
		ProxyURL:                strings.TrimSpace(row.ProxyURL),
		CustomHeaders:           row.GetCredentialStringMap("custom_headers"),
		HealthTier:              HealthTierWarm,
		AddedAt:                 row.CreatedAt.UnixNano(),
		UpstreamType:            upstreamType,
		BaseURL:                 strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:                  strings.TrimSpace(apiKey),
		Models:                  models,
		ModelMapping:            modelMapping,
		CodexClientMetadataMode: codexClientMetadataMode,
	}
	if isOpenAIResponsesAccount {
		account.HealthTier = HealthTierHealthy
		if account.PlanType == "" {
			account.PlanType = "api"
		}
	}
	account.ScoreBiasOverride = reflectOptionalInt64Field(row, "ScoreBiasOverride")
	account.BaseConcurrencyOverride = reflectOptionalInt64Field(row, "BaseConcurrencyOverride")
	account.setAllowedAPIKeyIDsLocked(row.GetCredentialInt64Slice("allowed_api_key_ids"))
	account.Tags = cloneStringSlice(row.Tags)
	if row.Locked {
		atomic.StoreInt32(&account.Locked, 1)
	}
	if !row.Enabled {
		atomic.StoreInt32(&account.DispatchPaused, 1)
	}
	account.CreditEnabled = row.CreditEnabled
	account.CreditSkipUsageWindow = row.CreditSkipUsageWindow
	account.IgnoreUsageLimitStatusOverride = row.GetCredentialOptionalBool("ignore_usage_limit_status_override")
	account.recomputeEffectiveIgnoreUsageLimitStatus(s.IgnoreUsageLimitStatus())
	account.SkipWarmTier = row.SkipWarmTier
	if row.Status == "error" {
		account.Status = StatusError
		account.ErrorMsg = row.ErrorMessage
		account.HealthTier = HealthTierRisky
	}

	// 尝试从 credentials 恢复已有的 AT
	if at != "" {
		account.AccessToken = at
		account.AccountID = row.GetCredential("account_id")
		account.Email = row.GetCredential("email")
		account.PlanType = row.GetCredential("plan_type")
		if account.Status != StatusError {
			account.HealthTier = HealthTierHealthy
		}
		if expiresAt := row.GetCredential("expires_at"); expiresAt != "" {
			if parsed, err := time.Parse(time.RFC3339, expiresAt); err == nil {
				account.ExpiresAt = parsed
			} else {
				log.Printf("[账号 %d] 解析 expires_at 失败: %v", row.ID, err)
			}
		}
	}
	if subExp := row.GetCredential("subscription_expires_at"); subExp != "" {
		if parsed, err := time.Parse(time.RFC3339, subExp); err == nil {
			account.SubscriptionExpiresAt = parsed
		}
	}
	if row.CooldownUntil.Valid {
		if time.Now().Before(row.CooldownUntil.Time) {
			account.SetCooldownUntil(row.CooldownUntil.Time, row.CooldownReason)
		} else if row.CooldownReason != "" {
			if err := s.db.ClearCooldown(ctx, row.ID); err != nil {
				log.Printf("[账号 %d] 清理过期冷却状态失败: %v", row.ID, err)
			}
		}
	}
	if usagePct := row.GetCredential("codex_7d_used_percent"); usagePct != "" {
		if parsed, err := strconv.ParseFloat(usagePct, 64); err == nil {
			updatedAt := time.Time{}
			if usageUpdatedAt := row.GetCredential("codex_usage_updated_at"); usageUpdatedAt != "" {
				if parsedTime, err := time.Parse(time.RFC3339, usageUpdatedAt); err == nil {
					updatedAt = parsedTime
				} else {
					log.Printf("[账号 %d] 解析 codex_usage_updated_at 失败: %v", row.ID, err)
				}
			}
			account.SetUsageSnapshot(parsed, updatedAt)
			// 恢复 7d 重置时间
			if resetAt := row.GetCredential("codex_7d_reset_at"); resetAt != "" {
				if t, err := time.Parse(time.RFC3339, resetAt); err == nil {
					account.SetReset7dAt(t)
				}
			}
		} else {
			log.Printf("[账号 %d] 解析 codex_7d_used_percent 失败: %v", row.ID, err)
		}
	}
	// 恢复 5h 用量快照
	if usagePct5h := row.GetCredential("codex_5h_used_percent"); usagePct5h != "" {
		if parsed, err := strconv.ParseFloat(usagePct5h, 64); err == nil {
			resetAt := time.Time{}
			if r := row.GetCredential("codex_5h_reset_at"); r != "" {
				if t, err := time.Parse(time.RFC3339, r); err == nil {
					resetAt = t
				}
			}
			updatedAt := time.Time{}
			if usageUpdatedAt5h := row.GetCredential("codex_5h_usage_updated_at"); usageUpdatedAt5h != "" {
				if parsedTime, err := time.Parse(time.RFC3339, usageUpdatedAt5h); err == nil {
					updatedAt = parsedTime
				} else {
					log.Printf("[账号 %d] 解析 codex_5h_usage_updated_at 失败: %v", row.ID, err)
				}
			}
			account.SetUsageSnapshot5hAt(parsed, resetAt, updatedAt)
		}
	}
	if threshold, ok := row.GetCredentialFloat64("auto_pause_5h_threshold"); ok {
		account.AutoPause5hThreshold = normalizeQuotaAutoPauseThreshold(threshold)
	}
	if threshold, ok := row.GetCredentialFloat64("auto_pause_7d_threshold"); ok {
		account.AutoPause7dThreshold = normalizeQuotaAutoPauseThreshold(threshold)
	}
	account.AutoPause5hDisabled = row.GetCredentialBool("auto_pause_5h_disabled")
	account.AutoPause7dDisabled = row.GetCredentialBool("auto_pause_7d_disabled")
	if limit, ok := row.GetCredentialInt64("dispatch_count_limit"); ok {
		account.SetDispatchCountLimit(limit)
	}
	if priority, ok := row.GetCredentialInt64("scheduler_priority"); ok {
		account.SetSchedulerPriority(priority)
	}
	account.recomputeEffectiveAutoPause(s)
	for _, cooldown := range modelCooldowns[row.ID] {
		account.RestoreModelCooldown(cooldown.Model, cooldown.Reason, cooldown.ResetAt, cooldown.UpdatedAt)
	}
	account.mu.Lock()
	account.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	account.mu.Unlock()
	return account
}

// BuildTransientAccountByID 从数据库构建一个临时账号（包含回收站中的已删除账号），
// 不加入运行时池、不参与调度，用于回收站的连通性测试。
func (s *Store) BuildTransientAccountByID(ctx context.Context, dbID int64) (*Account, error) {
	row, err := s.db.GetAccountByIDIncludingDeleted(ctx, dbID)
	if err != nil {
		return nil, err
	}
	account := s.buildAccountFromRow(ctx, row, nil)
	if account == nil {
		return nil, fmt.Errorf("账号 %d 缺少可用凭据", dbID)
	}
	return account, nil
}

// LoadAccountByID 从数据库加载单个账号并加入运行时池（用于回收站恢复等场景）。
func (s *Store) LoadAccountByID(ctx context.Context, dbID int64) error {
	if s.FindByID(dbID) != nil {
		return nil
	}
	row, err := s.db.GetAccountByID(ctx, dbID)
	if err != nil {
		return err
	}
	account := s.buildAccountFromRow(ctx, row, nil)
	if account == nil {
		return fmt.Errorf("账号 %d 缺少可用凭据", dbID)
	}
	s.AddAccount(account)
	return nil
}

// StartBackgroundRefresh 启动后台定期刷新
func (s *Store) StartBackgroundRefresh() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		refreshTimer := time.NewTimer(s.GetBackgroundRefreshInterval())
		autoCleanupTicker := time.NewTicker(30 * time.Second)
		fullUsageCleanupTicker := time.NewTicker(5 * time.Minute)
		expiredCleanupTicker := time.NewTicker(15 * time.Minute)
		// 添加定时重建 FastScheduler 以优化性能
		rebuildSchedulerTicker := time.NewTicker(10 * time.Minute)
		// 到点即探定时器：始终武装到「最近的限流冷却/窗口重置边界」，倒计时归零即探针刷新。
		boundaryProbeTimer := time.NewTimer(time.Hour)
		if !boundaryProbeTimer.Stop() {
			<-boundaryProbeTimer.C
		}
		defer refreshTimer.Stop()
		defer autoCleanupTicker.Stop()
		defer fullUsageCleanupTicker.Stop()
		defer expiredCleanupTicker.Stop()
		defer rebuildSchedulerTicker.Stop()
		defer boundaryProbeTimer.Stop()

		resetRefreshTimer := func() {
			if !refreshTimer.Stop() {
				select {
				case <-refreshTimer.C:
				default:
				}
			}
			refreshTimer.Reset(s.GetBackgroundRefreshInterval())
		}

		// 启动时先武装一次；此后每次巡检/唤醒/到点后都会重排，保证始终盯住最近边界。
		s.armNextBoundaryProbe(boundaryProbeTimer)

		for {
			select {
			case <-refreshTimer.C:
				if s.GetLazyMode() {
					s.TriggerUsageProbeAsync()
				} else {
					s.parallelRefreshAll(context.Background())
					s.TriggerUsageProbeAsync()
					s.TriggerRecoveryProbeAsync()
				}
				refreshTimer.Reset(s.GetBackgroundRefreshInterval())
				// 巡检可能刷新了各账号的重置时间，顺带重排「到点即探」定时器，
				// 兜底那些两次唤醒之间未显式 WakeBoundaryProbe 的边界变化。
				s.armNextBoundaryProbe(boundaryProbeTimer)
			case <-boundaryProbeTimer.C:
				// 某账号的限流冷却/窗口重置刚归零：立即探针刷新真实用量，再武装下一个边界。
				s.TriggerUsageProbeAsync()
				s.armNextBoundaryProbe(boundaryProbeTimer)
			case <-s.boundaryProbeWakeCh:
				// 有更早的新边界出现（如刚吃到 429 冷却），重排到该时刻。
				s.armNextBoundaryProbe(boundaryProbeTimer)
			case <-s.backgroundRefreshWakeCh:
				resetRefreshTimer()
			case <-autoCleanupTicker.C:
				s.TriggerAutoCleanupAsync()
			case <-fullUsageCleanupTicker.C:
				if s.GetAutoCleanFullUsage() && !s.GetLazyMode() {
					go s.CleanFullUsageAccounts(context.Background())
				}
			case <-expiredCleanupTicker.C:
				// 每 15 分钟清理加入超过 30 分钟的账号（需开启开关）
				if s.GetAutoCleanExpired() {
					go s.CleanExpiredAccounts(context.Background(), 30*time.Minute)
				}
			case <-rebuildSchedulerTicker.C:
				// 定期重建调度器以优化内存和性能
				if s.FastSchedulerEnabled() {
					s.rebuildFastScheduler()
				}
			case <-s.stopCh:
				return
			}
		}
	}()
}

// Stop 停止后台刷新
func (s *Store) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

// CleanByRuntimeStatus 按运行时状态清理账号（用于自动清理流程）
// premium 5h 限流账号会被跳过，因为它们会在 5h 内自然恢复，无需删除。
// 手动一键清理请改用 CleanRateLimitedManual——它会清掉所有限流账号。
func (s *Store) CleanByRuntimeStatus(ctx context.Context, targetStatus string) int {
	accounts := s.Accounts()
	cleaned := 0

	for _, acc := range accounts {
		if acc == nil || acc.RuntimeStatus() != targetStatus {
			continue
		}
		if targetStatus == "rate_limited" && acc.IsPremium5hRateLimited() {
			continue
		}

		// 锁定账号跳过自动清理
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}

		if s.db != nil {
			if err := s.db.SoftDeleteAccount(ctx, acc.DBID); err != nil {
				log.Printf("[账号 %d] 清理 %s 状态失败: %v", acc.DBID, targetStatus, err)
				continue
			}
		}

		s.RemoveAccount(acc.DBID)
		cleaned++
		if s.db != nil {
			s.db.InsertAccountEventAsync(acc.DBID, "deleted", "auto_clean")
		}
	}

	return cleaned
}

// CleanRateLimitedManual 清理所有"限流"含义下的账号（用于手动一键清理）。
// 与 CleanByRuntimeStatus("rate_limited") 的区别：
//   - 涵盖 RuntimeStatus 的全部限流相关值：rate_limited / usage_exhausted
//   - 不跳过 premium 5h 限流：手动触发即代表用户明确意图删除
//   - 锁定账号依然跳过（与所有清理流程一致）
func (s *Store) CleanRateLimitedManual(ctx context.Context) int {
	accounts := s.Accounts()
	cleaned := 0

	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		status := acc.RuntimeStatus()
		if status != "rate_limited" && status != "usage_exhausted" {
			continue
		}

		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}

		if s.db != nil {
			if err := s.db.SoftDeleteAccount(ctx, acc.DBID); err != nil {
				log.Printf("[账号 %d] 手动清理限流账号失败: %v", acc.DBID, err)
				continue
			}
		}

		s.RemoveAccount(acc.DBID)
		cleaned++
		if s.db != nil {
			s.db.InsertAccountEventAsync(acc.DBID, "deleted", "manual_clean")
		}
	}

	return cleaned
}

// ==================== 最少连接调度 ====================

// Next 获取下一个可用账号（健康优先 + 低负载择优 + warm 公平调度）
func (s *Store) Next() *Account {
	return s.NextExcluding(0, nil)
}

// NextExcluding 获取下一个可用账号，排除指定的账号 ID 集合
// 用于重试时避免再次选到已失败（如 401）的账号
func (s *Store) NextExcluding(apiKeyID int64, exclude map[int64]bool) *Account {
	return s.NextExcludingWithFilter(apiKeyID, exclude, nil)
}

func (s *Store) tryAcquireAccount(acc *Account, limit int64, updateSchedulerOnLimit bool) bool {
	if acc == nil || limit <= 0 {
		return false
	}
	for {
		current := atomic.LoadInt64(&acc.ActiveRequests)
		if current >= limit {
			return false
		}
		if atomic.CompareAndSwapInt64(&acc.ActiveRequests, current, current+1) {
			now := time.Now()
			reservation := acc.reserveDispatchCount(now)
			if !reservation.Allowed {
				atomic.AddInt64(&acc.ActiveRequests, -1)
				s.markDispatchCountLimitCooldown(acc, reservation.ResetAt, updateSchedulerOnLimit)
				return false
			}
			atomic.AddInt64(&acc.TotalRequests, 1)
			atomic.StoreInt64(&acc.LastUsedAt, now.UnixNano())
			if reservation.HitLimit {
				s.markDispatchCountLimitCooldown(acc, reservation.ResetAt, updateSchedulerOnLimit)
			}
			return true
		}
	}
}

// NextExcludingWithFilter 获取下一个可用账号，并应用请求级账号过滤器。
func (s *Store) NextExcludingWithFilter(apiKeyID int64, exclude map[int64]bool, filter AccountFilter) *Account {
	if s.GetLazyMode() {
		return s.nextExcludingWithFilterLazy(apiKeyID, exclude, filter)
	}
	if scheduler := s.getFastScheduler(); scheduler != nil {
		for attempts := 0; attempts < 16; attempts++ {
			acc := scheduler.AcquireExcludingWithFilter(apiKeyID, exclude, filter)
			if acc == nil {
				break
			}
			if s.accountHasCachedCooldown(acc) {
				scheduler.Release(acc)
				continue
			}
			return acc
		}
	}

	for attempts := 0; attempts < 16; attempts++ {
		s.mu.RLock()

		var best *Account
		bestSchedulerPriority := minSchedulerPriority - 1
		bestPriority := -1
		bestDispatchScore := -math.MaxFloat64
		var bestLoad int64 = math.MaxInt64
		var bestLimit int64
		maxConcurrency := atomic.LoadInt64(&s.maxConcurrency)

		for _, acc := range s.accounts {
			if exclude != nil && exclude[acc.DBID] {
				continue
			}
			if !acc.IsAvailable() {
				continue
			}
			if !s.accountAllowedForAPIKey(acc, apiKeyID) {
				continue
			}
			if filter != nil && !filter(acc) {
				continue
			}

			load := atomic.LoadInt64(&acc.ActiveRequests)
			tier, _, dispatchScore, limit := acc.schedulerSnapshot(maxConcurrency)
			if limit <= 0 || load >= limit {
				continue
			}

			// 账号调度优先级严格先于健康档位与调度分（issue #358）
			schedulerPriority := acc.schedulerPriority()
			priority := tierPriority(tier)
			if schedulerPriority > bestSchedulerPriority ||
				(schedulerPriority == bestSchedulerPriority && (priority > bestPriority ||
					(priority == bestPriority && (dispatchScore > bestDispatchScore ||
						(dispatchScore == bestDispatchScore && load < bestLoad) ||
						(dispatchScore == bestDispatchScore && load == bestLoad && fastRandN(2) == 0))))) {
				bestSchedulerPriority = schedulerPriority
				bestPriority = priority
				bestDispatchScore = dispatchScore
				bestLoad = load
				bestLimit = limit
				best = acc
			}
		}
		s.mu.RUnlock()

		if best == nil {
			return nil
		}
		if s.accountHasCachedCooldown(best) {
			continue
		}
		if s.tryAcquireAccount(best, bestLimit, true) {
			return best
		}
	}
	return nil
}

func (s *Store) accountLazySelectable(acc *Account) bool {
	if acc == nil {
		return false
	}
	if atomic.LoadInt32(&acc.Disabled) != 0 || atomic.LoadInt32(&acc.DispatchPaused) != 0 {
		return false
	}

	acc.mu.RLock()
	defer acc.mu.RUnlock()
	now := time.Now()
	if acc.Status == StatusError {
		return false
	}
	if acc.healthTierLocked() == HealthTierBanned {
		return false
	}
	if acc.usageExhaustedLocked() {
		return false
	}
	if acc.Status == StatusCooldown && now.Before(acc.CooldownUtil) {
		return false
	}
	if acc.premium5hRateLimitedLocked(now) {
		return false
	}
	if acc.quotaAutoPausedLocked(now) {
		return false
	}
	if acc.isOpenAIResponsesAPILocked() {
		return true
	}
	return strings.TrimSpace(acc.AccessToken) != "" ||
		strings.TrimSpace(acc.RefreshToken) != "" ||
		strings.TrimSpace(acc.SessionToken) != ""
}

func (s *Store) ensureLazyDispatchReady(acc *Account) bool {
	if acc == nil {
		return false
	}
	if s.lazyNeedsDispatchRefresh(acc) {
		s.triggerLazyRefreshAsync(acc)
		return false
	}
	return acc.IsAvailable()
}

func (s *Store) lazyNeedsDispatchRefresh(acc *Account) bool {
	if acc == nil {
		return false
	}
	acc.mu.RLock()
	openAIResponses := acc.isOpenAIResponsesAPILocked()
	hasRefreshCredential := strings.TrimSpace(acc.RefreshToken) != "" || strings.TrimSpace(acc.SessionToken) != ""
	acc.mu.RUnlock()
	return !openAIResponses && hasRefreshCredential && acc.NeedsRefresh()
}

func (s *Store) triggerLazyRefreshAsync(acc *Account) {
	if acc == nil || acc.DBID == 0 {
		return
	}
	dbID := acc.DBID
	if _, loaded := s.lazyRefreshInFlight.LoadOrStore(dbID, struct{}{}); loaded {
		return
	}
	go func() {
		defer s.lazyRefreshInFlight.Delete(dbID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.refreshAccount(ctx, acc); err != nil {
			log.Printf("[账号 %d] lazy mode 预热刷新失败: %v", dbID, err)
		}
	}()
}

func (s *Store) lazyCanRefreshForMetadata(acc *Account) bool {
	if acc == nil {
		return false
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if acc.isOpenAIResponsesAPILocked() {
		return false
	}
	return acc.AccessToken == "" &&
		(strings.TrimSpace(acc.RefreshToken) != "" || strings.TrimSpace(acc.SessionToken) != "") &&
		acc.Status != StatusError &&
		acc.healthTierLocked() != HealthTierBanned
}

func (s *Store) acquireLazyCandidate(acc *Account, maxConcurrency int64) bool {
	if !s.ensureLazyDispatchReady(acc) {
		return false
	}
	_, _, _, limit := acc.schedulerSnapshot(maxConcurrency)
	if limit <= 0 {
		return false
	}
	return s.tryAcquireAccount(acc, limit, true)
}

func (s *Store) nextExcludingWithFilterLazy(apiKeyID int64, exclude map[int64]bool, filter AccountFilter) *Account {
	for attempts := 0; attempts < 16; attempts++ {
		s.mu.RLock()

		var best *Account
		var metadataRefreshCandidate *Account
		bestSchedulerPriority := minSchedulerPriority - 1
		bestPriority := -1
		bestDispatchScore := -math.MaxFloat64
		var bestLoad int64 = math.MaxInt64
		maxConcurrency := atomic.LoadInt64(&s.maxConcurrency)

		for _, acc := range s.accounts {
			if exclude != nil && exclude[acc.DBID] {
				continue
			}
			if !s.accountLazySelectable(acc) {
				continue
			}
			if !s.accountAllowedForAPIKey(acc, apiKeyID) {
				continue
			}
			if filter != nil && !filter(acc) {
				if metadataRefreshCandidate == nil && s.lazyCanRefreshForMetadata(acc) {
					metadataRefreshCandidate = acc
				}
				continue
			}
			if s.lazyNeedsDispatchRefresh(acc) {
				s.triggerLazyRefreshAsync(acc)
				continue
			}

			load := atomic.LoadInt64(&acc.ActiveRequests)
			tier, _, dispatchScore, limit := acc.schedulerSnapshot(maxConcurrency)
			if limit <= 0 || load >= limit {
				continue
			}

			// 账号调度优先级严格先于健康档位与调度分（issue #358）
			schedulerPriority := acc.schedulerPriority()
			priority := tierPriority(tier)
			if schedulerPriority > bestSchedulerPriority ||
				(schedulerPriority == bestSchedulerPriority && (priority > bestPriority ||
					(priority == bestPriority && (dispatchScore > bestDispatchScore ||
						(dispatchScore == bestDispatchScore && load < bestLoad) ||
						(dispatchScore == bestDispatchScore && load == bestLoad && fastRandN(2) == 0))))) {
				bestSchedulerPriority = schedulerPriority
				bestPriority = priority
				bestDispatchScore = dispatchScore
				bestLoad = load
				best = acc
			}
		}
		s.mu.RUnlock()

		if best == nil {
			if metadataRefreshCandidate != nil && s.ensureLazyDispatchReady(metadataRefreshCandidate) {
				continue
			}
			return nil
		}
		if s.accountHasCachedCooldown(best) {
			continue
		}
		if s.acquireLazyCandidate(best, maxConcurrency) {
			return best
		}
	}
	return nil
}

// BindSessionAffinity 记录会话与账号/代理的亲和关系。
func (s *Store) BindSessionAffinity(key string, account *Account, proxyURL string) {
	s.bindSessionAffinity(key, account, proxyURL)
}

func (s *Store) bindSessionAffinity(key string, account *Account, proxyURL string) {
	if s == nil || account == nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	ttl := sessionAffinityTTL()
	now := time.Now()
	binding := sessionAffinity{
		accountID:    account.DBID,
		proxyURL:     strings.TrimSpace(proxyURL),
		boundAt:      now,
		requestCount: 0,
		expiresAt:    now.Add(ttl),
	}

	s.sessionMu.Lock()
	if s.sessionBindings == nil {
		s.sessionBindings = make(map[string]sessionAffinity)
	}
	// 有界保护：过期绑定只在同 key 再次命中时才被动删除，对话结束后的绑定
	// 永远不会再被查询、会静默泄漏。粘性键按内容种子派生（每段对话一个）后
	// 键数量随对话数增长，超限时全量清一轮过期项。
	if len(s.sessionBindings) >= maxSessionBindings {
		for k, b := range s.sessionBindings {
			if !b.expiresAt.After(now) {
				delete(s.sessionBindings, k)
			}
		}
	}
	// 同账号的连续 Bind 视为复用,沿用 boundAt 与 requestCount 以保持 bounded 上限计数;
	// 换账号时则按新绑定从 0 开始计。
	if existing, ok := s.sessionBindings[key]; ok && existing.accountID == account.DBID {
		binding.boundAt = existing.boundAt
		binding.requestCount = existing.requestCount
	}
	s.sessionBindings[key] = binding
	s.sessionMu.Unlock()

	if s.tokenCache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := s.tokenCache.SetSessionAffinity(ctx, key, cache.SessionAffinityBinding{
			AccountID: binding.accountID,
			ProxyURL:  binding.proxyURL,
		}, ttl); err != nil {
			log.Printf("写入缓存会话粘性失败: account=%d err=%v", binding.accountID, err)
		}
	}
}

// UnbindSessionAffinity removes a session binding when it still points to the failed account.
func (s *Store) UnbindSessionAffinity(key string, accountID int64) {
	if s == nil || accountID == 0 {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}

	s.sessionMu.Lock()
	if binding, ok := s.sessionBindings[key]; ok && binding.accountID == accountID {
		delete(s.sessionBindings, key)
	}
	s.sessionMu.Unlock()

	if s.tokenCache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := s.tokenCache.DeleteSessionAffinity(ctx, key, accountID); err != nil {
			log.Printf("删除缓存会话粘性失败: account=%d err=%v", accountID, err)
		}
	}
}

// NextForSession 优先复用已绑定的账号和代理，失败时回退到普通选号。
func (s *Store) NextForSession(key string, apiKeyID int64, exclude map[int64]bool) (*Account, string) {
	return s.NextForSessionWithFilter(key, apiKeyID, exclude, nil)
}

// NextForSessionWithFilter 优先复用已绑定的账号和代理，并应用请求级账号过滤器。
//
// affinity_mode 决定粘性强度:
//   - off:     永不读绑定,每次都走完整挑号策略
//   - bounded (默认): 绑定有效但被以下任一条件解除
//   - 累计请求超过 defaultMaxAffinityRequests (50)
//   - 绑定时长超过 defaultMaxAffinityDuration (5min)
//   - 绑定账号当前已不属于 healthy 桶 (warm/risky/banned)
//   - strict:  完全沿用旧行为,只在 TTL 过期或显式 Unbind 时换号
//
// 解除发生时绕过 binding 走完整挑号策略(NextExcludingWithFilter),后续 BindSessionAffinity
// 会重新建立绑定。
func (s *Store) NextForSessionWithFilter(key string, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string) {
	if s == nil {
		return nil, ""
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return s.NextExcludingWithFilter(apiKeyID, exclude, filter), ""
	}

	mode := s.GetAffinityMode()
	if mode == AffinityModeOff {
		return s.NextExcludingWithFilter(apiKeyID, exclude, filter), ""
	}

	now := time.Now()
	s.sessionMu.RLock()
	binding, ok := s.sessionBindings[key]
	s.sessionMu.RUnlock()

	if ok {
		expired := !binding.expiresAt.After(now)
		// bounded 模式下追加逃逸条件检查
		escape := false
		if mode == AffinityModeBounded {
			if binding.requestCount >= defaultMaxAffinityRequests {
				escape = true
			} else if !binding.boundAt.IsZero() && now.Sub(binding.boundAt) >= defaultMaxAffinityDuration {
				escape = true
			} else if !s.affinityAccountStillHealthy(binding.accountID) {
				escape = true
			}
		}

		if expired || escape {
			s.sessionMu.Lock()
			if current, exists := s.sessionBindings[key]; exists && current.accountID == binding.accountID {
				delete(s.sessionBindings, key)
			}
			s.sessionMu.Unlock()
		} else if acc := s.takeByIDExcluding(binding.accountID, apiKeyID, exclude, filter); acc != nil {
			// 命中粘性,记一次复用
			s.sessionMu.Lock()
			if current, exists := s.sessionBindings[key]; exists && current.accountID == binding.accountID {
				current.requestCount++
				s.sessionBindings[key] = current
			}
			s.sessionMu.Unlock()
			return acc, binding.proxyURL
		}
	}
	if binding, ok := s.getCachedSessionAffinity(key); ok {
		// 跨进程缓存的 binding 也按 bounded 逻辑校验账号健康
		if mode == AffinityModeBounded && !s.affinityAccountStillHealthy(binding.accountID) {
			// 不复用,落到完整挑号
		} else if acc := s.takeByIDExcluding(binding.accountID, apiKeyID, exclude, filter); acc != nil {
			s.sessionMu.Lock()
			if s.sessionBindings == nil {
				s.sessionBindings = make(map[string]sessionAffinity)
			}
			s.sessionBindings[key] = binding
			s.sessionMu.Unlock()
			return acc, binding.proxyURL
		}
	}

	return s.NextExcludingWithFilter(apiKeyID, exclude, filter), ""
}

// affinityAccountStillHealthy 检查一个粘性绑定的账号是否仍处于 healthy 桶。
// 若已掉到 warm/risky/banned 或不可调度,则 bounded 模式会逃逸并重新挑号。
func (s *Store) affinityAccountStillHealthy(accountID int64) bool {
	if s == nil || accountID == 0 {
		return false
	}
	s.mu.RLock()
	target := s.lookupByIDLocked(accountID)
	s.mu.RUnlock()
	if target == nil {
		return false
	}
	if atomic.LoadInt32(&target.Disabled) != 0 || atomic.LoadInt32(&target.DispatchPaused) != 0 {
		return false
	}
	target.mu.RLock()
	defer target.mu.RUnlock()
	if target.Status == StatusError || target.Status == StatusCooldown {
		return false
	}
	tier := target.healthTierLocked()
	return tier == HealthTierHealthy
}

func (s *Store) getCachedSessionAffinity(key string) (sessionAffinity, bool) {
	if s == nil || s.tokenCache == nil {
		return sessionAffinity{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	binding, ok, err := s.tokenCache.GetSessionAffinity(ctx, key)
	if err != nil {
		log.Printf("读取缓存会话粘性失败: %v", err)
		return sessionAffinity{}, false
	}
	if !ok || binding.AccountID == 0 {
		return sessionAffinity{}, false
	}
	return sessionAffinity{
		accountID: binding.AccountID,
		proxyURL:  strings.TrimSpace(binding.ProxyURL),
		expiresAt: time.Now().Add(sessionAffinityTTL()),
	}, true
}

func (s *Store) takeByIDExcluding(id int64, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) *Account {
	if s == nil || id == 0 {
		return nil
	}
	if exclude != nil && exclude[id] {
		return nil
	}

	s.mu.RLock()
	target := s.lookupByIDLocked(id)
	s.mu.RUnlock()
	if target == nil {
		return nil
	}
	if s.GetLazyMode() {
		if !s.accountLazySelectable(target) {
			return nil
		}
	} else if !target.IsAvailable() {
		return nil
	}
	if s.accountHasCachedCooldown(target) {
		return nil
	}
	if !s.accountAllowedForAPIKey(target, apiKeyID) {
		return nil
	}
	if filter != nil && !filter(target) {
		return nil
	}

	maxConcurrency := atomic.LoadInt64(&s.maxConcurrency)
	now := time.Now()
	if s.GetLazyMode() {
		if !s.acquireLazyCandidate(target, maxConcurrency) {
			return nil
		}
		return target
	}

	_, _, limit, _, available := target.fastSchedulerSnapshot(maxConcurrency, now)
	if !available || limit <= 0 {
		return nil
	}
	if !s.tryAcquireAccount(target, limit, true) {
		return nil
	}
	return target
}

// WaitForAvailable 等待可用账号（带超时的请求排队）
func (s *Store) WaitForAvailable(ctx context.Context, timeout time.Duration, apiKeyID int64) *Account {
	acc, _ := s.WaitForSessionAvailable(ctx, "", timeout, apiKeyID, nil)
	return acc
}

// WaitForSessionAvailable waits for a session-preferred account and proxy pair.
func (s *Store) WaitForSessionAvailable(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool) (*Account, string) {
	return s.WaitForSessionAvailableWithFilter(ctx, key, timeout, apiKeyID, exclude, nil)
}

func (s *Store) hasDispatchCandidateWithFilter(apiKeyID int64, exclude map[int64]bool, filter AccountFilter) bool {
	if s == nil {
		return false
	}

	maxConcurrency := atomic.LoadInt64(&s.maxConcurrency)
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, acc := range s.accounts {
		if acc == nil {
			continue
		}
		if exclude != nil && exclude[acc.DBID] {
			continue
		}
		if s.GetLazyMode() {
			if !s.accountLazySelectable(acc) {
				continue
			}
		} else if !acc.IsAvailable() {
			continue
		}
		if s.accountHasCachedCooldown(acc) {
			continue
		}
		if !s.accountAllowedForAPIKey(acc, apiKeyID) {
			continue
		}
		if filter != nil && !filter(acc) {
			continue
		}

		_, _, _, limit := acc.schedulerSnapshot(maxConcurrency)
		if limit > 0 {
			return true
		}
	}
	return false
}

// WaitForSessionAvailableWithFilter waits for an account that satisfies the request-level filter.
func (s *Store) WaitForSessionAvailableWithFilter(ctx context.Context, key string, timeout time.Duration, apiKeyID int64, exclude map[int64]bool, filter AccountFilter) (*Account, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.hasDispatchCandidateWithFilter(apiKeyID, exclude, filter) {
		return nil, ""
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	backoff := 50 * time.Millisecond
	backoffTimer := time.NewTimer(backoff)
	if !backoffTimer.Stop() {
		select {
		case <-backoffTimer.C:
		default:
		}
	}
	defer backoffTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ""
		case <-deadline.C:
			return nil, ""
		default:
			acc, proxyURL := s.NextForSessionWithFilter(key, apiKeyID, exclude, filter)
			if acc != nil {
				return acc, proxyURL
			}
			if !s.hasDispatchCandidateWithFilter(apiKeyID, exclude, filter) {
				return nil, ""
			}
			// 等待一下再重试（指数退避，最大 500ms）
			backoffTimer.Reset(backoff)
			select {
			case <-backoffTimer.C:
				if backoff < 500*time.Millisecond {
					backoff *= 2
				}
			case <-ctx.Done():
				return nil, ""
			case <-deadline.C:
				return nil, ""
			}
		}
	}
}

// Release 释放账号（请求完成后调用，递减并发计数）
func (s *Store) Release(acc *Account) {
	if acc == nil {
		return
	}
	if scheduler := s.getFastScheduler(); scheduler != nil {
		scheduler.Release(acc)
		return
	}
	atomic.AddInt64(&acc.ActiveRequests, -1)
}

// SetMaxConcurrency 动态更新每账号并发上限
func (s *Store) SetMaxConcurrency(n int) {
	atomic.StoreInt64(&s.maxConcurrency, int64(n))
	// Update existing scheduler's base limit in-place before full rebuild.
	if scheduler := s.getFastScheduler(); scheduler != nil {
		scheduler.SetBaseLimit(int64(n))
	}
	s.recomputeAllAccountSchedulerState()
	s.rebuildFastScheduler()
}

// GetMaxConcurrency 获取当前每账号并发上限
func (s *Store) GetMaxConcurrency() int {
	return int(atomic.LoadInt64(&s.maxConcurrency))
}

// SetMaxRetries 动态更新最大重试次数
func (s *Store) SetMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	atomic.StoreInt64(&s.maxRetries, int64(n))
}

// GetMaxRetries 获取当前最大重试次数
func (s *Store) GetMaxRetries() int {
	return int(atomic.LoadInt64(&s.maxRetries))
}

func (s *Store) SetMaxRateLimitRetries(n int) {
	if n < 0 {
		n = 0
	}
	atomic.StoreInt64(&s.maxRateLimitRetries, int64(n))
}

func (s *Store) GetMaxRateLimitRetries() int {
	return int(atomic.LoadInt64(&s.maxRateLimitRetries))
}

// normalizeRetryIntervalMS 把重试间隔限制在 0-30000ms(0 = 立即重试)。
func normalizeRetryIntervalMS(ms int) int {
	if ms < 0 {
		return 0
	}
	if ms > 30000 {
		return 30000
	}
	return ms
}

// SetRetryIntervalMS 动态更新重试间隔（毫秒）。
func (s *Store) SetRetryIntervalMS(ms int) {
	if s == nil {
		return
	}
	s.retryIntervalMS.Store(int64(normalizeRetryIntervalMS(ms)))
}

// GetRetryIntervalMS 获取当前重试间隔（毫秒），0 = 立即重试。
func (s *Store) GetRetryIntervalMS() int {
	if s == nil {
		return 0
	}
	return int(s.retryIntervalMS.Load())
}

// SetTransportRetryPolicy 动态更新传输错误重试策略（rotate / sticky）。
func (s *Store) SetTransportRetryPolicy(policy string) {
	if s == nil {
		return
	}
	s.transportRetryPolicy.Store(database.NormalizeTransportRetryPolicy(policy))
}

// GetTransportRetryPolicy 获取传输错误重试策略，缺省 rotate（换号，旧行为）。
func (s *Store) GetTransportRetryPolicy() string {
	if s == nil {
		return "rotate"
	}
	if v, ok := s.transportRetryPolicy.Load().(string); ok && v != "" {
		return v
	}
	return "rotate"
}

// GetAllowRemoteMigration 获取是否允许远程迁移
func (s *Store) GetAllowRemoteMigration() bool {
	return s.allowRemoteMigration.Load()
}

// SetAllowRemoteMigration 设置是否允许远程迁移
func (s *Store) SetAllowRemoteMigration(enabled bool) {
	s.allowRemoteMigration.Store(enabled)
}

// SetTestModel 动态更新测试连接模型
func (s *Store) SetTestModel(m string) {
	s.testModel.Store(m)
}

// GetTestModel 获取当前测试连接模型
func (s *Store) GetTestModel() string {
	if v, ok := s.testModel.Load().(string); ok && v != "" {
		return v
	}
	return "gpt-5.4"
}

// SetTestContent dynamically updates connection test input text.
func (s *Store) SetTestContent(content string) {
	s.testContent.Store(NormalizeTestContent(content))
}

// GetTestContent returns the input text used by connection tests.
func (s *Store) GetTestContent() string {
	if v, ok := s.testContent.Load().(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return DefaultTestContent
}

// SetTestConcurrency 动态更新批量测试并发数
func (s *Store) SetTestConcurrency(n int) {
	atomic.StoreInt64(&s.testConcurrency, int64(n))
}

// GetTestConcurrency 获取当前批量测试并发数
func (s *Store) GetTestConcurrency() int {
	return int(atomic.LoadInt64(&s.testConcurrency))
}

// GetBackgroundRefreshIntervalMinutes 获取后台巡检间隔（分钟）。
func (s *Store) GetBackgroundRefreshIntervalMinutes() int {
	return int(s.GetBackgroundRefreshInterval() / time.Minute)
}

// GetUsageProbeMaxAgeMinutes 获取用量探针最大缓存时长（分钟）。
func (s *Store) GetUsageProbeMaxAgeMinutes() int {
	return int(s.GetUsageProbeMaxAge() / time.Minute)
}

// GetRecoveryProbeIntervalMinutes 获取恢复探测最小间隔（分钟）。
func (s *Store) GetRecoveryProbeIntervalMinutes() int {
	return int(s.GetRecoveryProbeInterval() / time.Minute)
}

// SetModelMapping 动态更新模型映射 JSON
func (s *Store) SetModelMapping(mapping string) {
	s.modelMapping.Store(mapping)
}

// GetModelMapping 获取当前模型映射 JSON
func (s *Store) GetModelMapping() string {
	if v, ok := s.modelMapping.Load().(string); ok && v != "" {
		return v
	}
	return "{}"
}

// SetCodexModelMapping 动态更新 Codex 模型映射 JSON
func (s *Store) SetCodexModelMapping(mapping string) {
	s.codexModelMapping.Store(mapping)
}

// GetCodexModelMapping 获取当前 Codex 模型映射 JSON
func (s *Store) GetCodexModelMapping() string {
	if v, ok := s.codexModelMapping.Load().(string); ok && v != "" {
		return v
	}
	return "{}"
}

// SetReasoningEffortModels 动态更新带思考强度的模型别名 JSON 数组。
func (s *Store) SetReasoningEffortModels(value string) {
	s.reasoningEffortModels.Store(value)
}

// GetReasoningEffortModels 获取当前带思考强度的模型别名 JSON 数组。
func (s *Store) GetReasoningEffortModels() string {
	if v, ok := s.reasoningEffortModels.Load().(string); ok && v != "" {
		return v
	}
	return "[]"
}

// GetSchedulerMode 获取当前调度模式
func (s *Store) GetSchedulerMode() string {
	if v, ok := s.schedulerMode.Load().(string); ok {
		return v
	}
	return "round_robin"
}

// SetSchedulerMode 设置调度模式并传播到 FastScheduler
func (s *Store) SetSchedulerMode(mode string) {
	switch mode {
	case "round_robin", "remaining_quota":
		// ok
	default:
		mode = "round_robin"
	}
	s.schedulerMode.Store(mode)
	if scheduler := s.getFastScheduler(); scheduler != nil {
		scheduler.SetSchedulerMode(mode)
	}
}

// GetAffinityMode 获取当前 session affinity 模式 (bounded / off / strict)
func (s *Store) GetAffinityMode() string {
	if v, ok := s.affinityMode.Load().(string); ok && v != "" {
		return v
	}
	return AffinityModeBounded
}

// SetAffinityMode 设置 session affinity 模式
func (s *Store) SetAffinityMode(mode string) {
	switch mode {
	case AffinityModeBounded, AffinityModeOff, AffinityModeStrict:
		// ok
	default:
		mode = AffinityModeBounded
	}
	s.affinityMode.Store(mode)
}

func promptFilterConfigFromSettings(settings *database.SystemSettings) promptfilter.Config {
	cfg := promptfilter.DefaultConfig()
	if settings == nil {
		return cfg
	}
	cfg.Enabled = settings.PromptFilterEnabled
	cfg.Mode = settings.PromptFilterMode
	cfg.Threshold = settings.PromptFilterThreshold
	cfg.StrictThreshold = settings.PromptFilterStrictThreshold
	cfg.LogMatches = settings.PromptFilterLogMatches
	cfg.MaxTextLength = settings.PromptFilterMaxTextLength
	cfg.SensitiveWords = settings.PromptFilterSensitiveWords
	if patterns, err := promptfilter.ParseCustomPatterns(settings.PromptFilterCustomPatterns); err == nil {
		cfg.CustomPatterns = patterns
	}
	if disabled, err := promptfilter.ParseDisabledPatterns(settings.PromptFilterDisabledPatterns); err == nil {
		cfg.DisabledPatterns = disabled
	}
	cfg.Review = promptfilter.ReviewConfig{
		Enabled:        settings.PromptFilterReviewEnabled,
		APIKey:         settings.PromptFilterReviewAPIKey,
		BaseURL:        settings.PromptFilterReviewBaseURL,
		Model:          settings.PromptFilterReviewModel,
		TimeoutSeconds: settings.PromptFilterReviewTimeoutSeconds,
		FailClosed:     settings.PromptFilterReviewFailClosed,
	}
	return promptfilter.NormalizeConfig(cfg)
}

func (s *Store) SetPromptFilterConfig(cfg promptfilter.Config) {
	s.promptFilterConfig.Store(promptfilter.NormalizeConfig(cfg))
}

func (s *Store) GetPromptFilterConfig() promptfilter.Config {
	if v, ok := s.promptFilterConfig.Load().(promptfilter.Config); ok {
		return promptfilter.NormalizeConfig(v)
	}
	return promptfilter.DefaultConfig()
}

// SetIgnoreUsageLimitStatus updates the global default and immediately
// recomputes accounts that inherit it. Existing explicit cooldowns are kept
// until a real Responses success confirms recovery.
func (s *Store) SetIgnoreUsageLimitStatus(enabled bool) {
	if s == nil {
		return
	}
	s.ignoreUsageLimitStatus.Store(enabled)
	for _, acc := range s.Accounts() {
		acc.mu.Lock()
		acc.recomputeEffectiveIgnoreUsageLimitStatus(enabled)
		acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
		acc.mu.Unlock()
		s.fastSchedulerUpdate(acc)
	}
}

func (s *Store) IgnoreUsageLimitStatus() bool {
	return s != nil && s.ignoreUsageLimitStatus.Load()
}

func (s *Store) SetGlobalAutoPauseThresholds(t5h, t7d float64) {
	s.mu.Lock()
	s.globalAutoPause5hThreshold = normalizeQuotaAutoPauseThreshold(t5h)
	s.globalAutoPause7dThreshold = normalizeQuotaAutoPauseThreshold(t7d)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetGlobalAutoPause5hThreshold() float64 {
	s.mu.RLock()
	v := s.globalAutoPause5hThreshold
	s.mu.RUnlock()
	return v
}

func (s *Store) GetGlobalAutoPause7dThreshold() float64 {
	s.mu.RLock()
	v := s.globalAutoPause7dThreshold
	s.mu.RUnlock()
	return v
}

func (s *Store) SetAutoPause5hGuardBandPercent(value float64) {
	s.mu.Lock()
	s.autoPause5hGuardBandPercent = normalizeAutoPause5hGuardBandPercent(value)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetAutoPause5hGuardBandPercent() float64 {
	s.mu.RLock()
	v := s.autoPause5hGuardBandPercent
	s.mu.RUnlock()
	return v
}

func (s *Store) SetAutoPause5hGuardConcurrency(value int) {
	s.mu.Lock()
	s.autoPause5hGuardConcurrency = normalizeAutoPause5hGuardConcurrency(value)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetAutoPause5hGuardConcurrency() int {
	s.mu.RLock()
	v := s.autoPause5hGuardConcurrency
	s.mu.RUnlock()
	return v
}

func (s *Store) SetSmartPacingEnabled(value bool) {
	s.mu.Lock()
	s.smartPacingEnabled = value
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetSmartPacingEnabled() bool {
	s.mu.RLock()
	v := s.smartPacingEnabled
	s.mu.RUnlock()
	return v
}

func (s *Store) SetSmartPacingMinConcurrency(value int) {
	s.mu.Lock()
	s.smartPacingMinConcurrency = normalizeSmartPacingMinConcurrency(value)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetSmartPacingMinConcurrency() int {
	s.mu.RLock()
	v := s.smartPacingMinConcurrency
	s.mu.RUnlock()
	if v < 1 {
		v = defaultSmartPacingMinConcurrency
	}
	return v
}

func (s *Store) SetSmartPacingWindows(value string) {
	s.mu.Lock()
	s.smartPacingWindows = normalizeSmartPacingWindows(value)
	s.mu.Unlock()
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) GetSmartPacingWindows() string {
	s.mu.RLock()
	v := s.smartPacingWindows
	s.mu.RUnlock()
	if v == "" {
		return "5h,7d"
	}
	return v
}

func (s *Store) SetGroupAutoPauseThresholds(groupID int64, t5h, t7d float64) {
	s.groupAutoPauseThresholds.Store(groupID, [2]float64{
		normalizeQuotaAutoPauseThreshold(t5h),
		normalizeQuotaAutoPauseThreshold(t7d),
	})
	s.recomputeAllEffectiveAutoPause()
}

func (s *Store) DeleteGroupAutoPauseThresholds(groupID int64) {
	s.groupAutoPauseThresholds.Delete(groupID)
}

func (s *Store) GetGroupAutoPauseThresholds(groupID int64) (float64, float64) {
	return s.getGroupAutoPauseThresholds(groupID)
}

func (s *Store) getGroupAutoPauseThresholds(groupID int64) (float64, float64) {
	if v, ok := s.groupAutoPauseThresholds.Load(groupID); ok {
		t := v.([2]float64)
		return t[0], t[1]
	}
	return 0, 0
}

// SetGroupBaseConcurrencyOverride updates a group's inherited per-account base
// concurrency. A nil value clears the group override and falls back to other
// memberships or the global setting.
func (s *Store) SetGroupBaseConcurrencyOverride(groupID int64, value *int64) {
	if s == nil || groupID <= 0 {
		return
	}
	if value == nil {
		s.groupBaseConcurrencyOverrides.Delete(groupID)
	} else {
		s.groupBaseConcurrencyOverrides.Store(groupID, *value)
	}
	s.recomputeAllGroupBaseConcurrency()
}

func (s *Store) DeleteGroupBaseConcurrencyOverride(groupID int64) {
	s.SetGroupBaseConcurrencyOverride(groupID, nil)
}

func (s *Store) GetGroupBaseConcurrencyOverride(groupID int64) (int64, bool) {
	return s.getGroupBaseConcurrencyOverride(groupID)
}

func (s *Store) getGroupBaseConcurrencyOverride(groupID int64) (int64, bool) {
	if s == nil || groupID <= 0 {
		return 0, false
	}
	value, ok := s.groupBaseConcurrencyOverrides.Load(groupID)
	if !ok {
		return 0, false
	}
	return value.(int64), true
}

func (s *Store) recomputeAllGroupBaseConcurrency() {
	if s == nil {
		return
	}
	baseLimit := atomic.LoadInt64(&s.maxConcurrency)
	for _, acc := range s.Accounts() {
		if acc == nil {
			continue
		}
		acc.mu.Lock()
		acc.recomputeEffectiveGroupBaseConcurrency(s)
		acc.recomputeSchedulerLocked(baseLimit)
		acc.mu.Unlock()
		s.fastSchedulerUpdate(acc)
	}
}

func (s *Store) recomputeAllEffectiveAutoPause() {
	for _, acc := range s.Accounts() {
		acc.mu.Lock()
		acc.recomputeEffectiveAutoPause(s)
		acc.mu.Unlock()
	}
}

// AddAccount 热加载新账号到内存池（前端添加后即刻生效）
func (s *Store) AddAccount(acc *Account) {
	if acc == nil {
		return
	}
	// 记录加入时间（用于过期清理）
	if atomic.LoadInt64(&acc.AddedAt) == 0 {
		atomic.StoreInt64(&acc.AddedAt, time.Now().UnixNano())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc.mu.Lock()
	acc.recomputeEffectiveIgnoreUsageLimitStatus(s.IgnoreUsageLimitStatus())
	acc.recomputeEffectiveGroupBaseConcurrency(s)
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.accounts = append(s.accounts, acc)
	s.rebuildAccountIndex()
	s.fastSchedulerUpdate(acc)
}

// RemoveAccount 从内存池移除账号
func (s *Store) RemoveAccount(dbID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, acc := range s.accounts {
		if acc.DBID == dbID {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			s.rebuildAccountIndex()
			s.fastSchedulerRemove(dbID)
			// 清理 RefreshScheduler 中可能残留的任务
			if scheduler := s.GetRefreshScheduler(); scheduler != nil {
				scheduler.CancelTask(dbID)
			}
			return
		}
	}
}

// FindByID 通过数据库 ID 查找运行时账号
func (s *Store) FindByID(dbID int64) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lookupByIDLocked(dbID)
}

// lookupByIDLocked 通过索引 O(1) 查找账号；索引缺失时回退到线性扫描。
// 调用方必须持有 s.mu(读或写锁)。
func (s *Store) lookupByIDLocked(dbID int64) *Account {
	if s.accountsByID != nil {
		return s.accountsByID[dbID]
	}
	for _, acc := range s.accounts {
		if acc.DBID == dbID {
			return acc
		}
	}
	return nil
}

// rebuildAccountIndex 根据当前 s.accounts 重建 DBID 索引。
// 调用方必须持有 s.mu 写锁；在任何修改 s.accounts 的地方调用以保持同步。
func (s *Store) rebuildAccountIndex() {
	idx := make(map[int64]*Account, len(s.accounts))
	for _, acc := range s.accounts {
		if acc != nil {
			idx[acc.DBID] = acc
		}
	}
	s.accountsByID = idx
}

// ApplyAccountSchedulerOverrides 更新运行时账号的调度 override 并立即重算。
func (s *Store) ApplyAccountSchedulerOverrides(dbID int64, scoreBiasOverride, baseConcurrencyOverride *int64, skipWarmTier *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	acc.ScoreBiasOverride = cloneInt64Ptr(scoreBiasOverride)
	acc.BaseConcurrencyOverride = cloneInt64Ptr(baseConcurrencyOverride)
	if skipWarmTier != nil {
		acc.SkipWarmTier = *skipWarmTier
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountSchedulerOverridePatch(dbID int64, scoreBiasSet bool, scoreBiasOverride *int64, baseConcurrencySet bool, baseConcurrencyOverride *int64, skipWarmTier *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	if scoreBiasSet {
		acc.ScoreBiasOverride = cloneInt64Ptr(scoreBiasOverride)
	}
	if baseConcurrencySet {
		acc.BaseConcurrencyOverride = cloneInt64Ptr(baseConcurrencyOverride)
	}
	if skipWarmTier != nil {
		acc.SkipWarmTier = *skipWarmTier
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountAllowedAPIKeys(dbID int64, allowedAPIKeyIDs []int64) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	acc.setAllowedAPIKeyIDsLocked(allowedAPIKeyIDs)
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

// ApplyAccountIgnoreUsageLimitStatus updates a nullable account override.
// override=nil means follow the global setting.
func (s *Store) ApplyAccountIgnoreUsageLimitStatus(dbID int64, override *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	if override == nil {
		acc.IgnoreUsageLimitStatusOverride = nil
	} else {
		value := *override
		acc.IgnoreUsageLimitStatusOverride = &value
	}
	acc.recomputeEffectiveIgnoreUsageLimitStatus(s.IgnoreUsageLimitStatus())
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountQuotaAutoPauseConfig(dbID int64, threshold5h, threshold7d *float64, disabled5h, disabled7d *bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	if threshold5h != nil {
		acc.AutoPause5hThreshold = normalizeQuotaAutoPauseThreshold(*threshold5h)
	}
	if threshold7d != nil {
		acc.AutoPause7dThreshold = normalizeQuotaAutoPauseThreshold(*threshold7d)
	}
	if disabled5h != nil {
		acc.AutoPause5hDisabled = *disabled5h
	}
	if disabled7d != nil {
		acc.AutoPause7dDisabled = *disabled7d
	}
	acc.recomputeEffectiveAutoPause(s)
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountDispatchCountLimit(dbID int64, limit *int64) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	if limit == nil {
		acc.SetDispatchCountLimit(0)
	} else {
		acc.SetDispatchCountLimit(*limit)
	}
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountTags(dbID int64, tags []string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.Tags = cloneStringSlice(tags)
	acc.mu.Unlock()
	return true
}

func (s *Store) ApplyAccountGroups(dbID int64, groupIDs []int64) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.GroupIDs = cloneInt64Slice(groupIDs)
	acc.recomputeEffectiveGroupBaseConcurrency(s)
	acc.recomputeEffectiveAutoPause(s)
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

// UpdateAccountCredit 更新账号信用设置
// 传入 nil 表示不修改该字段。
func (s *Store) UpdateAccountCredit(dbID int64, creditEnabled, creditSkipUsageWindow *bool) error {
	acc := s.FindByID(dbID)
	if acc == nil {
		return fmt.Errorf("账号 %d 不存在", dbID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.db.UpdateAccountCredit(ctx, dbID, creditEnabled, creditSkipUsageWindow); err != nil {
		return err
	}
	acc.mu.Lock()
	if creditEnabled != nil {
		acc.CreditEnabled = *creditEnabled
	}
	if creditSkipUsageWindow != nil {
		acc.CreditSkipUsageWindow = *creditSkipUsageWindow
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return nil
}

func (s *Store) ApplyAccountGroupMemberships(memberships map[int64][]int64) {
	for _, acc := range s.Accounts() {
		acc.mu.Lock()
		acc.GroupIDs = cloneInt64Slice(memberships[acc.DBID])
		acc.recomputeEffectiveGroupBaseConcurrency(s)
		acc.recomputeEffectiveAutoPause(s)
		acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
		acc.mu.Unlock()
		s.fastSchedulerUpdate(acc)
	}
}

func (s *Store) SetAPIKeyAllowedGroups(apiKeyID int64, groupIDs []int64) {
	if apiKeyID <= 0 {
		return
	}
	normalized := normalizeAllowedGroupIDs(groupIDs)
	s.apiKeyGroupsMu.Lock()
	if s.apiKeyAllowedGroups == nil {
		s.apiKeyAllowedGroups = make(map[int64][]int64)
	}
	if s.apiKeyAllowedGroupSets == nil {
		s.apiKeyAllowedGroupSets = make(map[int64]map[int64]struct{})
	}
	if int64SliceEqual(s.apiKeyAllowedGroups[apiKeyID], normalized) {
		s.apiKeyGroupsMu.Unlock()
		return
	}
	if len(normalized) == 0 {
		delete(s.apiKeyAllowedGroups, apiKeyID)
		delete(s.apiKeyAllowedGroupSets, apiKeyID)
	} else {
		s.apiKeyAllowedGroups[apiKeyID] = cloneInt64Slice(normalized)
		s.apiKeyAllowedGroupSets[apiKeyID] = int64Set(normalized)
	}
	s.apiKeyGroupsMu.Unlock()
	s.rebuildFastScheduler()
}

// SetAPIKeyAllowedPlans 设置某 API Key 的账号套餐白名单。plans 归一(小写、去空白、去重)
// 后落入内存集合;为空表示不限套餐。仅当集合真正变化时才重建调度器,以免鉴权热路径
// 每次请求都触发重建。
func (s *Store) SetAPIKeyAllowedPlans(apiKeyID int64, plans []string) {
	if apiKeyID <= 0 {
		return
	}
	normalized := normalizeAllowedPlans(plans)
	s.apiKeyGroupsMu.Lock()
	if s.apiKeyAllowedPlans == nil {
		s.apiKeyAllowedPlans = make(map[int64][]string)
	}
	if s.apiKeyAllowedPlanSets == nil {
		s.apiKeyAllowedPlanSets = make(map[int64]map[string]struct{})
	}
	if stringSliceEqual(s.apiKeyAllowedPlans[apiKeyID], normalized) {
		s.apiKeyGroupsMu.Unlock()
		return
	}
	if len(normalized) == 0 {
		delete(s.apiKeyAllowedPlans, apiKeyID)
		delete(s.apiKeyAllowedPlanSets, apiKeyID)
	} else {
		s.apiKeyAllowedPlans[apiKeyID] = append([]string(nil), normalized...)
		s.apiKeyAllowedPlanSets[apiKeyID] = stringSet(normalized)
	}
	s.apiKeyGroupsMu.Unlock()
	s.rebuildFastScheduler()
}

func (s *Store) GetAPIKeyAllowedGroups(apiKeyID int64) []int64 {
	if apiKeyID <= 0 {
		return nil
	}
	s.apiKeyGroupsMu.RLock()
	defer s.apiKeyGroupsMu.RUnlock()
	return cloneInt64Slice(s.apiKeyAllowedGroups[apiKeyID])
}

func (s *Store) LoadAPIKeyAllowedGroups(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	keys, err := s.db.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	s.apiKeyGroupsMu.Lock()
	s.apiKeyAllowedGroups = make(map[int64][]int64, len(keys))
	s.apiKeyAllowedGroupSets = make(map[int64]map[int64]struct{}, len(keys))
	s.apiKeyAllowedPlans = make(map[int64][]string, len(keys))
	s.apiKeyAllowedPlanSets = make(map[int64]map[string]struct{}, len(keys))
	for _, key := range keys {
		normalized := normalizeAllowedGroupIDs(key.AllowedGroupIDs)
		if len(normalized) > 0 {
			s.apiKeyAllowedGroups[key.ID] = cloneInt64Slice(normalized)
			s.apiKeyAllowedGroupSets[key.ID] = int64Set(normalized)
		}
		plans := normalizeAllowedPlans(key.Limits.PlanAllow)
		if len(plans) > 0 {
			s.apiKeyAllowedPlans[key.ID] = append([]string(nil), plans...)
			s.apiKeyAllowedPlanSets[key.ID] = stringSet(plans)
		}
	}
	s.apiKeyGroupsMu.Unlock()
	s.rebuildFastScheduler()
	return nil
}

// APIKeyAllowsAccount 判断某 API Key 是否允许调度到该账号。分组白名单与套餐白名单
// 各自非空时都必须命中(AND 语义);任一为空表示该维度不限。
func (s *Store) APIKeyAllowsAccount(apiKeyID int64, acc *Account) bool {
	if s == nil || apiKeyID <= 0 || acc == nil {
		return true
	}
	s.apiKeyGroupsMu.RLock()
	allowedGroups := s.apiKeyAllowedGroupSets[apiKeyID]
	allowedPlans := s.apiKeyAllowedPlanSets[apiKeyID]
	s.apiKeyGroupsMu.RUnlock()
	if len(allowedGroups) == 0 && len(allowedPlans) == 0 {
		return true
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if len(allowedPlans) > 0 {
		if _, ok := allowedPlans[lowerTrimPlan(acc.PlanType)]; !ok {
			return false
		}
	}
	if len(allowedGroups) == 0 {
		return true
	}
	for _, id := range acc.GroupIDs {
		if _, ok := allowedGroups[id]; ok {
			return true
		}
	}
	return false
}

func normalizeAllowedGroupIDs(groupIDs []int64) []int64 {
	out := make([]int64, 0, len(groupIDs))
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func int64Set(values []int64) map[int64]struct{} {
	out := make(map[int64]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func int64SliceEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lowerTrimPlan 归一单个套餐名用于匹配:小写去空白。刻意不折叠 prolite→pro,
// 使 API Key 的套餐过滤与账号列表(Accounts 页)按原始 plan_type 精确匹配的语义一致。
func lowerTrimPlan(plan string) string {
	return strings.ToLower(strings.TrimSpace(plan))
}

// normalizeAllowedPlans 归一账号套餐白名单:小写去空白、去重并排序,保证
// SetAPIKeyAllowedPlans 的变化检测稳定。匹配时账号侧同样走 lowerTrimPlan。
func normalizeAllowedPlans(plans []string) []string {
	out := make([]string, 0, len(plans))
	seen := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		normalized := lowerTrimPlan(plan)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out
}

func (s *Store) accountAllowedForAPIKey(acc *Account, apiKeyID int64) bool {
	if acc == nil {
		return false
	}
	return acc.AllowsAPIKey(apiKeyID) && s.APIKeyAllowsAccount(apiKeyID, acc)
}

func (s *Store) ApplyOpenAIResponsesConfig(dbID int64, baseURL, apiKey string, models []string, modelMapping, codexClientMetadataMode, proxyURL string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}

	acc.mu.Lock()
	acc.UpstreamType = UpstreamOpenAIResponses
	acc.BaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.TrimSpace(apiKey) != "" {
		acc.APIKey = strings.TrimSpace(apiKey)
	}
	acc.Models = normalizeModelList(models)
	acc.ModelMapping = strings.TrimSpace(modelMapping)
	acc.CodexClientMetadataMode = NormalizeCodexClientMetadataMode(codexClientMetadataMode)
	acc.ProxyURL = strings.TrimSpace(proxyURL)
	acc.Email = acc.BaseURL
	acc.PlanType = "api"
	if acc.Status != StatusError {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true
}

func (s *Store) ApplyAccountProxyURL(dbID int64, proxyURL string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.ProxyURL = strings.TrimSpace(proxyURL)
	acc.mu.Unlock()
	return true
}

func (s *Store) ApplyAccountCustomHeaders(dbID int64, headers map[string]string) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	acc.CustomHeaders = cloneStringMap(headers)
	acc.mu.Unlock()
	return true
}

func (s *Store) ApplyAccountEnabled(dbID int64, enabled bool) bool {
	acc := s.FindByID(dbID)
	if acc == nil {
		return false
	}
	if enabled {
		atomic.StoreInt32(&acc.DispatchPaused, 0)
	} else {
		atomic.StoreInt32(&acc.DispatchPaused, 1)
	}
	s.fastSchedulerUpdate(acc)
	return true
}

func normalizeAccountErrorMessage(errorMsg string, fallback string) string {
	errorMsg = strings.TrimSpace(errorMsg)
	if errorMsg == "" {
		errorMsg = strings.TrimSpace(fallback)
	}
	if len(errorMsg) > 500 {
		errorMsg = errorMsg[:500]
	}
	return errorMsg
}

// MarkCooldown 标记账号进入冷却，并持久化到数据库
func (s *Store) MarkCooldown(acc *Account, duration time.Duration, reason string) {
	s.markCooldown(acc, duration, reason, "")
}

// MarkCooldownWithError 标记账号进入冷却，并同时记录本次上游错误详情。
func (s *Store) MarkCooldownWithError(acc *Account, duration time.Duration, reason string, errorMsg string) {
	s.markCooldown(acc, duration, reason, errorMsg)
}

func (s *Store) markDispatchCountLimitCooldown(acc *Account, resetAt time.Time, updateScheduler bool) {
	if s == nil || acc == nil {
		return
	}
	now := time.Now()
	if resetAt.IsZero() || !resetAt.After(now) {
		resetAt = now.Add(dispatchCountFallbackWindow)
	}
	s.markCooldownUntil(acc, resetAt, "rate_limited", updateScheduler)
}

func (s *Store) markCooldownUntil(acc *Account, until time.Time, reason string, updateScheduler bool) {
	if acc == nil {
		return
	}
	now := time.Now()
	if until.IsZero() || !until.After(now) {
		until = now.Add(dispatchCountFallbackWindow)
	}
	reason = normalizeCooldownReason(reason)

	acc.mu.Lock()
	acc.Status = StatusCooldown
	acc.CooldownUtil = until
	acc.CooldownReason = reason
	switch reason {
	case "unauthorized":
		acc.LastUnauthorizedAt = now
		acc.LastFailureAt = now
		acc.HealthTier = HealthTierBanned
	case "rate_limited", "usage_limited", "usage_limit":
		acc.LastRateLimitedAt = now
		if acc.healthTierLocked() == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else if acc.HealthTier != HealthTierBanned {
			acc.HealthTier = HealthTierRisky
		}
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()

	if updateScheduler {
		s.fastSchedulerUpdate(acc)
	}
	s.setCachedAccountCooldown(acc.DBID, reason, until)

	if s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.SetCooldown(ctx, acc.DBID, reason, until); err != nil {
		log.Printf("[账号 %d] 持久化冷却状态失败: %v", acc.DBID, err)
	}
}

func (s *Store) markCooldown(acc *Account, duration time.Duration, reason string, errorMsg string) {
	if acc == nil {
		return
	}

	errorMsg = normalizeAccountErrorMessage(errorMsg, "")
	now := time.Now()
	acc.mu.Lock()
	switch reason {
	case "unauthorized":
		if !acc.LastUnauthorizedAt.IsZero() && now.Sub(acc.LastUnauthorizedAt) < 24*time.Hour {
			duration = 24 * time.Hour
		} else {
			duration = 6 * time.Hour
		}
		acc.LastUnauthorizedAt = now
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		acc.HealthTier = HealthTierBanned
	case "rate_limited":
		acc.LastRateLimitedAt = now
		acc.LastFailureAt = now
		acc.FailureStreak++
		acc.SuccessStreak = 0
		if acc.healthTierLocked() == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	}
	if errorMsg != "" {
		acc.ErrorMsg = errorMsg
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()

	until := now.Add(duration)
	acc.SetCooldownUntil(until, reason)
	s.fastSchedulerUpdate(acc)
	s.setCachedAccountCooldown(acc.DBID, reason, until)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	if errorMsg != "" {
		err = s.db.SetCooldownWithError(ctx, acc.DBID, reason, until, errorMsg)
	} else {
		err = s.db.SetCooldown(ctx, acc.DBID, reason, until)
	}
	if err != nil {
		log.Printf("[账号 %d] 持久化冷却状态失败: %v", acc.DBID, err)
	}
}

func (s *Store) MarkModelCooldown(acc *Account, model string, duration time.Duration, reason string) ModelCooldown {
	if acc == nil {
		return ModelCooldown{}
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return ModelCooldown{}
	}
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	if duration > 30*time.Minute {
		duration = 30 * time.Minute
	}

	now := time.Now()
	acc.mu.Lock()
	if acc.ModelCooldowns == nil {
		acc.ModelCooldowns = make(map[string]ModelCooldown)
	}
	current := acc.ModelCooldowns[key]
	level := current.BackoffLevel
	if current.ResetAt.After(now) {
		level++
		duration *= 2
		for i := 0; i < level-1; i++ {
			duration *= 2
		}
		if duration > 30*time.Minute {
			duration = 30 * time.Minute
		}
	}
	resetAt := now.Add(duration)
	if reason == "" {
		reason = "rate_limited"
	}
	cooldown := ModelCooldown{
		Model:        key,
		Reason:       reason,
		ResetAt:      resetAt,
		UpdatedAt:    now,
		BackoffLevel: level,
	}
	acc.ModelCooldowns[key] = cooldown
	acc.LastRateLimitedAt = now
	acc.LastFailureAt = now
	acc.FailureStreak = clampInt(acc.FailureStreak+1, 0, 20)
	acc.SuccessStreak = 0
	if acc.healthTierLocked() == HealthTierHealthy {
		acc.HealthTier = HealthTierWarm
	} else if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierRisky
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.setCachedModelCooldown(acc.DBID, cooldown)

	if s.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.db.SetModelCooldown(ctx, acc.DBID, key, reason, resetAt); err != nil {
			log.Printf("[账号 %d] 持久化模型冷却失败 model=%s: %v", acc.DBID, key, err)
		}
	}
	return cooldown
}

func (s *Store) ClearModelCooldown(acc *Account, model string) {
	if acc == nil {
		return
	}
	key := normalizeModelCooldownKey(model)
	if key == "" {
		return
	}
	if !acc.ClearModelCooldown(key) {
		return
	}
	s.deleteCachedModelCooldown(acc.DBID, key)
	s.fastSchedulerUpdate(acc)
	if s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.ClearModelCooldown(ctx, acc.DBID, key); err != nil {
		log.Printf("[账号 %d] 清理模型冷却失败 model=%s: %v", acc.DBID, key, err)
	}
}

// MarkError 标记账号为错误状态，并持久化到数据库。
func (s *Store) MarkError(acc *Account, errorMsg string) {
	if acc == nil {
		return
	}

	errorMsg = normalizeAccountErrorMessage(errorMsg, "账号测试失败")

	now := time.Now()
	acc.mu.Lock()
	acc.Status = StatusError
	acc.ErrorMsg = errorMsg
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.LastFailureAt = now
	acc.FailureStreak++
	acc.SuccessStreak = 0
	if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierRisky
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.deleteCachedAccountCooldown(acc.DBID)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.SetError(ctx, acc.DBID, errorMsg); err != nil {
		log.Printf("[账号 %d] 持久化错误状态失败: %v", acc.DBID, err)
	}
}

// ClearCooldown 清除账号冷却状态，并同步清理数据库
func (s *Store) ClearCooldown(acc *Account) {
	if acc == nil {
		return
	}

	atomic.StoreInt32(&acc.Disabled, 0) // 清除原子禁用标志
	acc.mu.Lock()
	wasCooling := acc.Status == StatusCooldown
	wasError := acc.Status == StatusError
	premium5hLimited := acc.premium5hRateLimitedLocked(time.Now())
	if acc.Status == StatusCooldown || acc.Status == StatusError {
		acc.Status = StatusReady
	}
	acc.ErrorMsg = ""
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	if wasCooling && !premium5hLimited {
		acc.HealthTier = HealthTierWarm
	} else if wasError && acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierWarm
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.deleteCachedAccountCooldown(acc.DBID)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.ClearError(ctx, acc.DBID); err != nil {
		log.Printf("[账号 %d] 清理账号状态失败: %v", acc.DBID, err)
	}
}

func isUsageLimitCooldownReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "rate_limited", "rate_limited_5h", "rate_limited_7d", "usage_limit":
		return true
	default:
		return false
	}
}

// ConfirmResponsesAvailable clears only a usage/rate-limit cooldown after a
// completed Responses request succeeds. Authentication and unrelated error
// states are intentionally untouched.
func (s *Store) ConfirmResponsesAvailable(acc *Account) bool {
	if s == nil || acc == nil {
		return false
	}

	acc.mu.Lock()
	if !acc.ignoreUsageLimitStatus || acc.Status != StatusCooldown || !isUsageLimitCooldownReason(acc.CooldownReason) {
		acc.mu.Unlock()
		return false
	}
	acc.Status = StatusReady
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.ErrorMsg = ""
	acc.LastRateLimitedAt = time.Time{}
	if acc.HealthTier != HealthTierBanned {
		acc.HealthTier = HealthTierWarm
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)
	s.deleteCachedAccountCooldown(acc.DBID)
	if s.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.db.ClearCooldown(ctx, acc.DBID); err != nil {
			log.Printf("[账号 %d] Responses 成功后清理用量限流冷却失败: %v", acc.DBID, err)
		}
	}
	return true
}

// RecordManualTestSuccess clears failure/cooldown state after an explicit admin
// connection test succeeds.
func (s *Store) RecordManualTestSuccess(acc *Account, latency time.Duration) {
	if acc == nil {
		return
	}

	now := time.Now()
	atomic.StoreInt32(&acc.Disabled, 0)
	acc.mu.Lock()
	wasCooling := acc.Status == StatusCooldown
	wasError := acc.Status == StatusError
	wasBanned := acc.HealthTier == HealthTierBanned
	wasUsageLimitCooldown := acc.ignoreUsageLimitStatus && wasCooling && isUsageLimitCooldownReason(acc.CooldownReason)
	premium5hLimited := acc.premium5hRateLimitedLocked(now)
	acc.recordLatencyLocked(latency)
	acc.recordResultLocked(true)
	if wasCooling || wasError {
		acc.Status = StatusReady
	}
	acc.ErrorMsg = ""
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.LastSuccessAt = now
	acc.SuccessStreak = clampInt(acc.SuccessStreak+1, 0, 20)
	acc.FailureStreak = 0
	if wasUsageLimitCooldown {
		acc.LastRateLimitedAt = time.Time{}
	}
	if premium5hLimited {
		acc.HealthTier = HealthTierRisky
	} else if wasUsageLimitCooldown {
		acc.HealthTier = HealthTierHealthy
	} else if wasBanned || wasCooling || wasError {
		acc.HealthTier = HealthTierWarm
	} else if acc.HealthTier == "" {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	s.deleteCachedAccountCooldown(acc.DBID)

	if s.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.ClearError(ctx, acc.DBID); err != nil {
		log.Printf("[账号 %d] 清理账号测试成功状态失败: %v", acc.DBID, err)
	}
}

// ReportRequestSuccess 记录一次成功请求，用于动态调度评分
func (s *Store) ReportRequestSuccess(acc *Account, latency time.Duration) {
	if acc == nil {
		return
	}

	acc.mu.Lock()
	acc.recordLatencyLocked(latency)
	acc.recordResultLocked(true)
	acc.LastSuccessAt = time.Now()
	acc.SuccessStreak = clampInt(acc.SuccessStreak+1, 0, 20)
	acc.FailureStreak = 0
	if acc.HealthTier == "" {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
}

// ReportRequestFailure 记录一次失败请求，用于动态调度评分
func (s *Store) ReportRequestFailure(acc *Account, kind string, latency time.Duration) {
	if acc == nil {
		return
	}

	now := time.Now()
	acc.mu.Lock()
	acc.recordLatencyLocked(latency)
	acc.recordResultLocked(false)
	acc.LastFailureAt = now
	acc.FailureStreak = clampInt(acc.FailureStreak+1, 0, 20)
	acc.SuccessStreak = 0

	switch kind {
	case "unauthorized":
		acc.LastUnauthorizedAt = now
		acc.HealthTier = HealthTierBanned
	case "timeout":
		acc.LastTimeoutAt = now
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	case "server":
		acc.LastServerErrorAt = now
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	case "transport":
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		} else {
			acc.HealthTier = HealthTierRisky
		}
	case "client":
		if acc.HealthTier == HealthTierHealthy {
			acc.HealthTier = HealthTierWarm
		}
	}

	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
}

// PersistUsageSnapshot 持久化账号用量快照（7d + 5h）
func (s *Store) PersistUsageSnapshot(acc *Account, pct7d float64) {
	if acc == nil {
		return
	}

	now := time.Now()
	acc.SetUsageSnapshot(pct7d, now)
	s.fastSchedulerUpdate(acc)

	if s.db == nil {
		return
	}

	// 如果有 5h 数据，使用完整存储
	if pct5h, ok := acc.GetUsagePercent5h(); ok {
		reset5hAt := acc.GetReset5hAt()
		reset7dAt := acc.GetReset7dAt()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.db.UpdateUsageSnapshotFull(ctx, acc.DBID, pct7d, reset7dAt, pct5h, reset5hAt, now, acc.GetUsageUpdatedAt5h()); err != nil {
			log.Printf("[账号 %d] 持久化用量快照失败: %v", acc.DBID, err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateUsageSnapshot(ctx, acc.DBID, pct7d, now); err != nil {
		log.Printf("[账号 %d] 持久化用量快照失败: %v", acc.DBID, err)
	}
}

// UpdateAccountSubscriptionExpiresAt persists the latest subscription expiration observed from upstream.
func (s *Store) UpdateAccountSubscriptionExpiresAt(acc *Account, expiresAt time.Time) bool {
	if s == nil || acc == nil || expiresAt.IsZero() {
		return false
	}

	acc.mu.Lock()
	changed := acc.SubscriptionExpiresAt.IsZero() || !acc.SubscriptionExpiresAt.Equal(expiresAt)
	if changed {
		acc.SubscriptionExpiresAt = expiresAt
		acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	}
	acc.mu.Unlock()
	if changed {
		s.fastSchedulerUpdate(acc)
	}

	if s.db == nil {
		return changed
	}

	formatted := expiresAt.Format(time.RFC3339)
	if !changed {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		row, err := s.db.GetAccountByID(ctx, acc.DBID)
		if err != nil {
			log.Printf("[账号 %d] 读取 subscription_expires_at 失败: %v", acc.DBID, err)
			return changed
		}
		if row.GetCredential("subscription_expires_at") == formatted {
			return changed
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, map[string]interface{}{"subscription_expires_at": formatted}); err != nil {
		log.Printf("[账号 %d] 持久化 subscription_expires_at 失败: %v", acc.DBID, err)
	}
	return changed
}

// UpdateAccountPlanType persists the latest Codex plan type observed from upstream headers.
func (s *Store) UpdateAccountPlanType(acc *Account, planType string) bool {
	if s == nil || acc == nil {
		return false
	}
	plan := strings.ToLower(strings.TrimSpace(planType))
	if plan == "" {
		return false
	}

	acc.mu.Lock()
	changed := acc.PlanType != plan
	if changed {
		acc.PlanType = plan
		acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	}
	acc.mu.Unlock()
	if changed {
		s.fastSchedulerUpdate(acc)
	}

	if s.db == nil || !changed {
		return changed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, map[string]interface{}{"plan_type": plan}); err != nil {
		log.Printf("[账号 %d] 持久化 plan_type 失败: %v", acc.DBID, err)
	}
	return changed
}

// UpdateAccountIdentity persists account identity observed from upstream usage APIs.
func (s *Store) UpdateAccountIdentity(acc *Account, email, accountID string) bool {
	if s == nil || acc == nil {
		return false
	}
	email = strings.TrimSpace(email)
	accountID = strings.TrimSpace(accountID)
	if email == "" && accountID == "" {
		return false
	}

	fields := make(map[string]interface{}, 2)
	acc.mu.Lock()
	changed := false
	if email != "" && acc.Email != email {
		acc.Email = email
		fields["email"] = email
		changed = true
	}
	if accountID != "" && acc.AccountID != accountID {
		acc.AccountID = accountID
		fields["account_id"] = accountID
		changed = true
	}
	acc.mu.Unlock()

	if s.db == nil || len(fields) == 0 {
		return changed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, fields); err != nil {
		log.Printf("[账号 %d] 持久化账号身份失败: %v", acc.DBID, err)
	}
	return changed
}

// ApplyUsageLimitMetadata applies metadata returned by Codex usage_limit_reached errors.
func (s *Store) ApplyUsageLimitMetadata(acc *Account, planType string, resetAt time.Time) {
	if acc == nil {
		return
	}

	plan := strings.ToLower(strings.TrimSpace(planType))
	now := time.Now()
	fields := make(map[string]interface{})

	acc.mu.Lock()
	if plan != "" {
		acc.PlanType = plan
		fields["plan_type"] = plan
	}
	if plan == "free" && !resetAt.IsZero() && resetAt.After(now) {
		acc.UsagePercent7d = 100
		acc.UsagePercent7dValid = true
		acc.Reset7dAt = resetAt
		acc.UsageUpdatedAt = now
		fields["codex_7d_used_percent"] = float64(100)
		fields["codex_7d_reset_at"] = resetAt.Format(time.RFC3339)
		fields["codex_usage_updated_at"] = now.Format(time.RFC3339)
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)

	// free plan 的 7d 窗口重置时刻武装「到点即探」，重置一到即刷新进度条。
	if plan == "free" {
		s.WakeBoundaryProbe(resetAt)
	}

	if s.db == nil || len(fields) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, fields); err != nil {
		log.Printf("[账号 %d] 持久化 usage_limit 元数据失败: %v", acc.DBID, err)
	}
}

// SetUsageProbeFunc 注册主动探针回调
func (s *Store) SetUsageProbeFunc(fn func(context.Context, *Account) error) {
	s.usageProbeMu.Lock()
	defer s.usageProbeMu.Unlock()
	s.usageProbe = fn
}

// wsAuthVerifyMinInterval 限制同一账号 WS 鉴权验证探针的最小触发间隔，
// 避免高频 WS 上游异常关闭下反复探针。
const wsAuthVerifyMinInterval = 30 * time.Second

// VerifyAccountAuthAsync 在 WS 上游异常关闭（如 close 1008 policy violation）后，
// 异步对单个账号跑一次用量探针（wham 优先、零额度成本）。
//
// 背景：token 失效在 HTTP 通道会返回 401 → 走 applyCooldown 标记 unauthorized 冷却；
// 但在 WS 通道上游是用 close 1008 踢连接，被归类为普通 transport 失败，账号不会被封、
// 仍留在号池反复失败。这里用一次探针把"看不见的 401"补成与 HTTP 一致的处理：
// wham 探针 401 时由 /responses 回退探针裁决，回退命中 401 才 MarkCooldownWithError
// （wham 单方面 401 不定罪，避免误封 wham 恒 401 但流量可用的 codex_at 账号，issue #328）；
// 若只是内容策略/网络抖动触发的 1008，探针返回正常，不会误封。带最小间隔节流。
func (s *Store) VerifyAccountAuthAsync(account *Account) {
	if s == nil || account == nil {
		return
	}
	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil {
		return
	}

	now := time.Now()
	account.mu.Lock()
	if !account.lastAuthVerifyAt.IsZero() && now.Sub(account.lastAuthVerifyAt) < wsAuthVerifyMinInterval {
		account.mu.Unlock()
		return
	}
	account.lastAuthVerifyAt = now
	account.mu.Unlock()

	if !account.TryBeginUsageProbe() {
		return
	}
	go func() {
		defer account.FinishUsageProbe()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := probeFn(ctx, account); err != nil {
			log.Printf("[账号 %d] WS 上游异常关闭后鉴权验证探针失败: %v", account.DBID, err)
		}
	}()
}

// TriggerUsageProbeAsync 异步触发一次批量用量探针
func (s *Store) TriggerUsageProbeAsync() {
	if !s.usageProbeBatch.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer s.usageProbeBatch.Store(false)
		s.parallelProbeUsage(context.Background())
	}()
}

// WakeBoundaryProbe 提示「到点即探」调度器：某账号的限流冷却 / 窗口重置边界发生了变化，
// 可能出现了比当前武装更早的边界，需要重排定时器。at 为该边界时刻（IsZero 表示未知，
// 强制重排）。仅当 at 严格早于当前武装边界（或未武装）时才打扰后台 goroutine，避免
// 高频 429/流量刷新导致的无谓重排。本方法只做一次非阻塞 channel 写入，任何锁下调用都安全。
func (s *Store) WakeBoundaryProbe(at time.Time) {
	if s == nil || s.boundaryProbeWakeCh == nil {
		return
	}
	if !at.IsZero() {
		if !at.After(time.Now()) {
			return // 边界已过，交给常规巡检/探针即可
		}
		armed := atomic.LoadInt64(&s.armedBoundaryAt)
		if armed != 0 && at.UnixNano() >= armed {
			return // 已有更早或同刻的唤醒计划，定时器到点后会重新扫描接管更晚的边界
		}
	}
	select {
	case s.boundaryProbeWakeCh <- struct{}{}:
	default:
	}
}

// armNextBoundaryProbe 扫描所有账号，找出最近的「到点即探」边界并把 timer 重排到该时刻
// （加 probeBoundaryLag 滞后）。无待处理边界时停表。只在后台刷新 goroutine 内调用
// （该 goroutine 不持有任何账号锁，故此处逐账号取 RLock 不会死锁）。
func (s *Store) armNextBoundaryProbe(timer *time.Timer) {
	now := time.Now()
	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	var next time.Time
	for _, acc := range accounts {
		if t, ok := acc.nextProbeBoundary(now); ok {
			if next.IsZero() || t.Before(next) {
				next = t
			}
		}
	}

	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	if next.IsZero() {
		atomic.StoreInt64(&s.armedBoundaryAt, 0)
		return
	}
	atomic.StoreInt64(&s.armedBoundaryAt, next.UnixNano())
	d := time.Until(next) + probeBoundaryLag
	if d < 0 {
		d = probeBoundaryLag
	}
	timer.Reset(d)
}

// TriggerRecoveryProbeAsync 异步触发一次封禁账号恢复探测
func (s *Store) TriggerRecoveryProbeAsync() {
	if s.GetLazyMode() {
		return
	}
	if !s.recoveryProbeBatch.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer s.recoveryProbeBatch.Store(false)
		s.parallelRecoveryProbe(context.Background())
	}()
}

// TriggerAutoCleanupAsync 异步触发一次自动清理巡检
func (s *Store) TriggerAutoCleanupAsync() {
	if !s.autoCleanupBatch.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer s.autoCleanupBatch.Store(false)
		s.runAutoCleanupSweep(context.Background())
	}()
}

func (s *Store) runAutoCleanupSweep(ctx context.Context) {
	if !s.GetAutoCleanUnauthorized() && !s.GetAutoCleanRateLimited() && !s.GetAutoCleanError() {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cleanedUnauthorized := 0
	cleanedRateLimited := 0
	cleanedError := 0

	if s.GetAutoCleanUnauthorized() {
		cleanedUnauthorized = s.CleanByRuntimeStatus(cleanupCtx, "unauthorized")
	}
	if s.GetAutoCleanRateLimited() {
		cleanedRateLimited = s.CleanByRuntimeStatus(cleanupCtx, "rate_limited")
	}
	if s.GetAutoCleanError() {
		cleanedError = s.CleanByRuntimeStatus(cleanupCtx, "error")
	}

	if cleanedUnauthorized > 0 || cleanedRateLimited > 0 || cleanedError > 0 {
		log.Printf("自动清理完成: unauthorized=%d, rate_limited=%d, error=%d", cleanedUnauthorized, cleanedRateLimited, cleanedError)
	}
}

// CleanFullUsageAccounts 清理用量达到 100% 的账号（跳过正在处理请求的账号）
func (s *Store) CleanFullUsageAccounts(ctx context.Context) int {
	accounts := s.Accounts()
	cleaned := 0

	for _, acc := range accounts {
		if acc == nil {
			continue
		}

		// 锁定账号跳过自动清理
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}

		// 跳过正在处理请求的账号
		if atomic.LoadInt64(&acc.ActiveRequests) > 0 {
			continue
		}

		// 用量窗口对该账号仅作展示参考时（忽略用量限制/重置券跳过窗口），
		// 快照不构成"账号已耗尽"的依据，不做自动清理。
		if acc.SkipsUsageWindowLimits() {
			continue
		}

		// 检查用量是否 >= 100%
		pct, valid := acc.GetUsagePercent7d()
		if !valid || pct < 100.0 {
			continue
		}

		if s.db != nil {
			if err := s.db.SoftDeleteAccount(ctx, acc.DBID); err != nil {
				log.Printf("[账号 %d] 清理用量满账号失败: %v", acc.DBID, err)
				continue
			}
		}

		s.RemoveAccount(acc.DBID)
		log.Printf("[账号 %d] 用量 %.1f%% 已满，已自动清理 (email=%s)", acc.DBID, pct, acc.Email)
		if s.db != nil {
			s.db.InsertAccountEventAsync(acc.DBID, "deleted", "clean_full_usage")
		}
		cleaned++
	}

	if cleaned > 0 {
		log.Printf("用量清理完成: 共清理 %d 个满用量账号", cleaned)
	}
	return cleaned
}

// CleanExpiredAccounts 清理加入号池超过指定时长的账号（不管是否被调用过）
// 批量操作优化：先收集所有过期 ID，再一次性完成数据库更新和内存移除
func (s *Store) CleanExpiredAccounts(ctx context.Context, maxAge time.Duration) int {
	accounts := s.Accounts()
	now := time.Now()
	cutoff := now.Add(-maxAge).UnixNano()

	// 1. 收集所有需要清理的账号 ID
	var expiredIDs []int64
	var skipNoAddedAt, skipNotExpired, skipActive, skipProven int
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		// 锁定账号跳过自动清理
		if atomic.LoadInt32(&acc.Locked) == 1 {
			continue
		}
		addedAt := atomic.LoadInt64(&acc.AddedAt)
		if addedAt == 0 {
			skipNoAddedAt++
			continue
		}
		if addedAt > cutoff {
			skipNotExpired++
			continue
		}
		if atomic.LoadInt64(&acc.ActiveRequests) > 0 {
			skipActive++
			continue
		}
		// 成功请求超过 10 次的账号保留，不做过期清理
		if atomic.LoadInt64(&acc.TotalRequests) > 10 {
			skipProven++
			continue
		}
		expiredIDs = append(expiredIDs, acc.DBID)
	}

	log.Printf("过期清理扫描: 总数=%d, 待清理=%d, 跳过(无时间=%d, 未过期=%d, 处理中=%d, 已验证=%d)",
		len(accounts), len(expiredIDs), skipNoAddedAt, skipNotExpired, skipActive, skipProven)

	if len(expiredIDs) == 0 {
		return 0
	}

	log.Printf("过期清理: 发现 %d 个超时账号，开始批量处理", len(expiredIDs))

	// 2. 批量更新数据库状态
	if s.db != nil {
		if err := s.db.BatchSoftDeleteAccounts(ctx, expiredIDs); err != nil {
			log.Printf("过期清理: 批量更新数据库失败: %v，回退逐条处理", err)
			return s.cleanExpiredFallback(ctx, expiredIDs)
		}
	}

	// 3. 批量从内存池移除
	s.RemoveAccounts(expiredIDs)

	// 4. 批量写入事件日志（异步）
	if s.db != nil {
		s.db.BatchInsertAccountEventsAsync(expiredIDs, "deleted", "clean_expired")
	}

	log.Printf("过期清理完成: 共清理 %d 个超时账号", len(expiredIDs))
	return len(expiredIDs)
}

// cleanExpiredFallback 批量操作失败时逐条回退处理
func (s *Store) cleanExpiredFallback(ctx context.Context, ids []int64) int {
	cleaned := 0
	for _, id := range ids {
		if err := s.db.SoftDeleteAccount(ctx, id); err != nil {
			log.Printf("[账号 %d] 过期清理失败: %v", id, err)
			continue
		}
		s.RemoveAccount(id)
		s.db.InsertAccountEventAsync(id, "deleted", "clean_expired")
		cleaned++
	}
	if cleaned > 0 {
		log.Printf("过期清理(回退): 共清理 %d 个超时账号", cleaned)
	}
	return cleaned
}

// RemoveAccounts 批量从内存池移除账号（一次加锁、一次遍历，避免 O(n²)）
func (s *Store) RemoveAccounts(dbIDs []int64) {
	if len(dbIDs) == 0 {
		return
	}

	removeSet := make(map[int64]struct{}, len(dbIDs))
	for _, id := range dbIDs {
		removeSet[id] = struct{}{}
	}

	s.mu.Lock()
	kept := s.accounts[:0]
	for _, acc := range s.accounts {
		if _, remove := removeSet[acc.DBID]; remove {
			s.fastSchedulerRemove(acc.DBID)
			if scheduler := s.GetRefreshScheduler(); scheduler != nil {
				scheduler.CancelTask(acc.DBID)
			}
		} else {
			kept = append(kept, acc)
		}
	}
	s.accounts = kept
	s.rebuildAccountIndex()
	s.mu.Unlock()
}

func (s *Store) parallelProbeUsage(ctx context.Context) {
	s.parallelProbeUsageWith(ctx, s.GetUsageProbeMaxAge())
}

// parallelProbeUsageWith 以指定 maxAge 阈值执行一次批量用量探针。
// maxAge<=0 时视为"立即探针"——只要账号能跑就刷一次。
func (s *Store) parallelProbeUsageWith(ctx context.Context, maxAge time.Duration) {
	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil {
		return
	}

	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	sem := make(chan struct{}, s.GetUsageProbeConcurrency())
	var wg sync.WaitGroup

	for _, acc := range accounts {
		if !acc.NeedsUsageProbe(maxAge) {
			continue
		}
		if !acc.TryBeginUsageProbe() {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(account *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer account.FinishUsageProbe()

			probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			if err := probeFn(probeCtx, account); err != nil {
				log.Printf("[账号 %d] 用量探针失败: %v", account.DBID, err)
			}
		}(acc)
	}

	wg.Wait()
}

// TriggerUsageProbeForceAsync 异步触发一次"无视缓存阈值"的批量用量探针。
// 用于管理端手动刷新场景。
func (s *Store) TriggerUsageProbeForceAsync() {
	if !s.usageProbeBatch.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer s.usageProbeBatch.Store(false)
		s.parallelProbeUsageWith(context.Background(), 0)
	}()
}

func (s *Store) parallelRecoveryProbe(ctx context.Context) {
	s.usageProbeMu.RLock()
	probeFn := s.usageProbe
	s.usageProbeMu.RUnlock()
	if probeFn == nil {
		return
	}

	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup

	for _, acc := range accounts {
		if !acc.NeedsRecoveryProbe(s.GetRecoveryProbeInterval()) {
			continue
		}
		if !acc.TryBeginRecoveryProbe() {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(account *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer account.FinishRecoveryProbe()

			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			if account.NeedsRefresh() {
				if err := s.refreshAccount(probeCtx, account); err != nil {
					log.Printf("[账号 %d] 恢复探测前刷新失败: %v", account.DBID, err)
				}
			}

			if err := probeFn(probeCtx, account); err != nil {
				log.Printf("[账号 %d] 恢复探测失败: %v", account.DBID, err)
			} else {
				// 用量已耗尽的账号不重置状态
				account.mu.RLock()
				exhausted := account.usageExhaustedLocked()
				account.mu.RUnlock()
				if exhausted {
					log.Printf("[账号 %d] 恢复探测成功但用量已耗尽，保持当前状态", account.DBID)
				} else {
					// 探测成功：将账号从 banned 升级到 warm，给予重新调度的机会
					atomic.StoreInt32(&account.Disabled, 0) // 清除原子禁用标志
					account.mu.Lock()
					if account.HealthTier == HealthTierBanned {
						account.HealthTier = HealthTierWarm
						account.SchedulerScore = 80
						account.FailureStreak = 0
						account.SuccessStreak = 1
						account.LastSuccessAt = time.Now()
						if account.Status == StatusCooldown {
							account.Status = StatusReady
							account.CooldownUtil = time.Time{}
							account.CooldownReason = ""
						}
						account.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
						log.Printf("[账号 %d] 恢复探测成功！已从 banned 升级到 warm", account.DBID)
					}
					account.mu.Unlock()
					// 清理数据库冷却状态
					s.deleteCachedAccountCooldown(account.DBID)
					if s.db != nil {
						_ = s.db.ClearCooldown(context.Background(), account.DBID)
					}
				}
			}
		}(acc)
	}

	wg.Wait()
}

// RefreshSingle 刷新单个账号（供 admin handler 调用）
func (s *Store) RefreshSingle(ctx context.Context, dbID int64) error {
	s.mu.RLock()
	var target *Account
	for _, acc := range s.accounts {
		if acc.DBID == dbID {
			target = acc
			break
		}
	}
	s.mu.RUnlock()

	if target == nil {
		return fmt.Errorf("账号 %d 不存在", dbID)
	}
	return s.refreshAccountForced(ctx, target)
}

// AccountCount 返回账号数量
func (s *Store) AccountCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.accounts)
}

// AvailableCount 返回可用账号数量
func (s *Store) AvailableCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	lazy := s.GetLazyMode()
	for _, acc := range s.accounts {
		if (lazy && s.accountLazySelectable(acc)) || (!lazy && acc.IsAvailable()) {
			count++
		}
	}
	return count
}

// Accounts 返回所有账号（用于统计）
func (s *Store) Accounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Account, len(s.accounts))
	copy(result, s.accounts)
	return result
}

// ==================== 并行刷新 ====================

// parallelRefreshAll 并行刷新所有需要刷新的账号（Worker Pool，并发度 10）
func (s *Store) parallelRefreshAll(ctx context.Context) {
	s.mu.RLock()
	accounts := make([]*Account, len(s.accounts))
	copy(accounts, s.accounts)
	s.mu.RUnlock()

	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup

	for i, acc := range accounts {
		if acc.Status == StatusError {
			continue
		}
		if acc.IsBanned() {
			continue
		}
		if acc.HasActiveCooldown() {
			continue
		}
		// AT-only 账号无 RT，无法刷新
		acc.mu.RLock()
		hasRT := acc.RefreshToken != ""
		acc.mu.RUnlock()
		if !hasRT {
			continue
		}
		if !acc.NeedsRefresh() {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, account *Account) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := s.refreshAccount(ctx, account); err != nil {
				log.Printf("[账号 %d] 刷新失败: %v", idx+1, err)
			} else {
				log.Printf("[账号 %d] 刷新成功: email=%s", idx+1, account.Email)
			}
		}(i, acc)
	}
	wg.Wait()
}

func (s *Store) refreshAccount(ctx context.Context, acc *Account) error {
	return s.refreshAccountWithOptions(ctx, acc, false)
}

func (s *Store) refreshAccountForced(ctx context.Context, acc *Account) error {
	return s.refreshAccountWithOptions(ctx, acc, true)
}

// refreshAccountWithOptions 刷新单个账号的 AT（带缓存锁与 token 缓存）
func (s *Store) refreshAccountWithOptions(ctx context.Context, acc *Account, forceRefresh bool) error {
	acc.mu.RLock()
	rt := acc.RefreshToken
	st := acc.SessionToken
	dbID := acc.DBID
	cooldownUntil := acc.CooldownUtil
	cooldownReason := acc.CooldownReason
	now := time.Now()
	activeCooldown := acc.Status == StatusCooldown && now.Before(acc.CooldownUtil)
	expiredCooldown := acc.Status == StatusCooldown && !now.Before(acc.CooldownUtil)
	acc.mu.RUnlock()

	// 1. 尝试从缓存读取 AT
	cachedToken := ""
	var err error
	if s.tokenCache != nil && !forceRefresh {
		cachedToken, err = s.tokenCache.GetAccessToken(ctx, dbID)
	}
	if cachedToken != "" {
		acc.mu.Lock()
		acc.AccessToken = cachedToken
		if acc.ExpiresAt.IsZero() || time.Until(acc.ExpiresAt) < 5*time.Minute {
			acc.ExpiresAt = time.Now().Add(30 * time.Minute)
		}
		if activeCooldown {
			acc.Status = StatusCooldown
			acc.CooldownUtil = cooldownUntil
			acc.CooldownReason = cooldownReason
		} else {
			acc.Status = StatusReady
			acc.CooldownUtil = time.Time{}
			acc.CooldownReason = ""
		}
		acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
		acc.mu.Unlock()
		s.fastSchedulerUpdate(acc)
		if expiredCooldown {
			s.deleteCachedAccountCooldown(dbID)
			_ = s.db.ClearCooldown(ctx, dbID)
		} else if !activeCooldown && s.db != nil {
			_ = s.db.ClearError(ctx, dbID)
		}
		return nil
	}

	// 2. 获取刷新锁
	if s.tokenCache != nil {
		acquired, lockErr := s.tokenCache.AcquireRefreshLock(ctx, dbID, 30*time.Second)
		if lockErr != nil {
			log.Printf("[账号 %d] 获取刷新锁失败: %v", dbID, lockErr)
		}
		if !acquired && lockErr == nil {
			// 另一个进程在刷新，等待它完成
			token, waitErr := s.tokenCache.WaitForRefreshComplete(ctx, dbID, 30*time.Second)
			if !forceRefresh && waitErr == nil && token != "" {
				acc.mu.Lock()
				acc.AccessToken = token
				acc.ExpiresAt = time.Now().Add(55 * time.Minute)
				if activeCooldown {
					acc.Status = StatusCooldown
					acc.CooldownUtil = cooldownUntil
					acc.CooldownReason = cooldownReason
				} else {
					acc.Status = StatusReady
					acc.CooldownUtil = time.Time{}
					acc.CooldownReason = ""
				}
				acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
				acc.mu.Unlock()
				s.fastSchedulerUpdate(acc)
				if expiredCooldown && s.db != nil {
					s.deleteCachedAccountCooldown(dbID)
					_ = s.db.ClearCooldown(ctx, dbID)
				} else if !activeCooldown && s.db != nil {
					_ = s.db.ClearError(ctx, dbID)
				}
				return nil
			}
			if forceRefresh {
				if waitErr != nil {
					log.Printf("[账号 %d] 等待已有刷新任务完成失败，继续尝试强制刷新: %v", dbID, waitErr)
				}
				acquired, lockErr = s.tokenCache.AcquireRefreshLock(ctx, dbID, 30*time.Second)
				if lockErr != nil {
					log.Printf("[账号 %d] 获取强制刷新锁失败: %v", dbID, lockErr)
				}
				if !acquired && lockErr == nil {
					return fmt.Errorf("账号 %d 正在刷新，请稍后重试", dbID)
				}
			}
		}
		if acquired {
			defer s.tokenCache.ReleaseRefreshLock(ctx, dbID)
		}
	}

	// 3. 执行 RT 刷新（Resin 启用时传入 DBID 用于粘性代理）
	resinID := fmt.Sprintf("%d", dbID)
	proxy := s.ResolveProxyForAccount(acc)
	var td *TokenData
	var info *AccountInfo
	if rt != "" {
		td, info, err = RefreshWithRetry(ctx, rt, proxy, resinID)
	} else {
		err = fmt.Errorf("refresh_token 为空")
	}
	if err != nil && st != "" {
		rtErr := err
		if stTD, stInfo, stErr := RefreshWithSessionTokenRetry(ctx, st, proxy, resinID); stErr == nil {
			td, info, err = stTD, stInfo, nil
			if td.RefreshToken == "" {
				td.RefreshToken = rt
			}
			log.Printf("[账号 %d] RT 刷新失败后已使用 session_token 回退刷新 AT", dbID)
		} else {
			err = fmt.Errorf("RT 刷新失败: %v；session_token 回退失败: %w", rtErr, stErr)
		}
	}
	if err != nil {
		if isNonRetryable(err) {
			acc.mu.Lock()
			acc.Status = StatusError
			acc.ErrorMsg = err.Error()
			acc.mu.Unlock()
			s.fastSchedulerUpdate(acc)

			_ = s.db.SetError(ctx, dbID, err.Error())
		}
		return err
	}

	// 4. 更新内存状态
	appliedPlanType := ""
	skippedPlanType := ""
	acc.mu.Lock()
	acc.AccessToken = td.AccessToken
	if td.RefreshToken != "" {
		acc.RefreshToken = td.RefreshToken
	}
	acc.SessionToken = st
	acc.ExpiresAt = td.ExpiresAt
	acc.ErrorMsg = ""
	if info != nil {
		if info.ChatGPTAccountID != "" {
			acc.AccountID = info.ChatGPTAccountID
		}
		if info.Email != "" {
			acc.Email = info.Email
		}
		// 不用空值覆盖已有的 PlanType，避免 plus 号被误标为 free
		if info.PlanType != "" {
			if plan, applied := acc.applyRefreshedPlanTypeLocked(info.PlanType, now); applied {
				appliedPlanType = plan
			} else {
				skippedPlanType = plan
			}
		} else if acc.PlanType == "" {
			log.Printf("[账号 %d] 刷新后 plan_type 为空，无法识别套餐类型", dbID)
		}
		if !info.SubscriptionExpiresAt.IsZero() {
			acc.SubscriptionExpiresAt = info.SubscriptionExpiresAt
		}
	}
	if activeCooldown {
		acc.Status = StatusCooldown
		acc.CooldownUtil = cooldownUntil
		acc.CooldownReason = cooldownReason
	} else {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	if skippedPlanType != "" {
		log.Printf("[账号 %d] 刷新返回 plan_type=%s，但 Codex free 7d 额度仍处于耗尽窗口，保留 plan_type=free", dbID, skippedPlanType)
	}

	// 5. 写入缓存
	ttl := time.Until(td.ExpiresAt) - 5*time.Minute
	if s.tokenCache != nil && ttl > 0 {
		_ = s.tokenCache.SetAccessToken(ctx, dbID, td.AccessToken, ttl)
	}

	// 6. 更新数据库 credentials
	credentials := map[string]interface{}{
		"access_token": td.AccessToken,
		"id_token":     td.IDToken,
		"expires_at":   td.ExpiresAt.Format(time.RFC3339),
	}
	if td.RefreshToken != "" {
		credentials["refresh_token"] = td.RefreshToken
	}
	if st != "" {
		credentials["session_token"] = st
	}
	if info != nil {
		if info.ChatGPTAccountID != "" {
			credentials["account_id"] = info.ChatGPTAccountID
		}
		if info.Email != "" {
			credentials["email"] = info.Email
		}
		if appliedPlanType != "" {
			credentials["plan_type"] = appliedPlanType
		}
		if !info.SubscriptionExpiresAt.IsZero() {
			credentials["subscription_expires_at"] = info.SubscriptionExpiresAt.Format(time.RFC3339)
		}
	}
	if err := s.db.UpdateCredentials(ctx, dbID, credentials); err != nil {
		log.Printf("[账号 %d] 更新数据库失败: %v", dbID, err)
	}
	if err := s.db.ClearError(ctx, dbID); err != nil {
		log.Printf("[账号 %d] 清理错误状态失败: %v", dbID, err)
	}

	// 自动锁定 free 以上的账号（pro/plus/team/teamplus 等）
	if appliedPlanType != "" && atomic.LoadInt32(&acc.Locked) == 0 {
		if appliedPlanType != "free" {
			atomic.StoreInt32(&acc.Locked, 1)
			_ = s.db.SetAccountLocked(ctx, dbID, true)
			log.Printf("[账号 %d] 检测到 %s 套餐，已自动锁定", dbID, appliedPlanType)
		}
	}

	if expiredCooldown {
		s.deleteCachedAccountCooldown(dbID)
		if err := s.db.ClearCooldown(ctx, dbID); err != nil {
			log.Printf("[账号 %d] 清理过期冷却状态失败: %v", dbID, err)
		}
	}

	return nil
}
