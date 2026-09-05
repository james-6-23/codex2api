package proxy

import (
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const (
	ClientCompatModePreserve = "preserve"
	ClientCompatModeAuto     = "auto"
	ClientCompatModeForce    = "force"

	StreamFlushPolicyImmediate = "immediate"
	StreamFlushPolicyCoalesce  = "coalesce"

	FirstTokenModeStrict = "strict"
	FirstTokenModeLoose  = "loose"

	BillingTierPolicyActual    = "actual"
	BillingTierPolicyRequested = "requested"

	// RequestIsolationMode 取值：
	//   isolated   —— 无显式会话的请求默认按"每请求"隔离上游身份（默认）；
	//   per-api-key —— 无显式会话的请求按下游 API Key 共享上游身份（恢复 v2 旧行为，
	//                  保留隐式 prompt cache 命中）。
	// 用环境变量 CODEX_REQUEST_ISOLATION_MODE 覆盖默认值。
	RequestIsolationModeIsolated  = "isolated"
	RequestIsolationModePerAPIKey = "per-api-key"

	defaultClientCompatMode      = ClientCompatModePreserve
	defaultCodexMinCLIVersion    = "0.153.3"
	defaultStreamFlushPolicy     = StreamFlushPolicyImmediate
	defaultStreamFlushIntervalMS = 20
	minStreamFlushIntervalMS     = 1
	maxStreamFlushIntervalMS     = 1000
	defaultFirstTokenMode        = FirstTokenModeStrict
	defaultFirstTokenTimeoutSec  = 0
	maxFirstTokenTimeoutSec      = 600
	defaultBillingTierPolicy     = BillingTierPolicyActual
	// defaultCodexRequestCompression 对齐真实 Codex CLI：它对 ChatGPT 后端的
	// HTTP /responses 请求体默认 zstd 压缩，所以网关默认也压。
	defaultCodexRequestCompression = true
	defaultCodexWSHideErrors       = true
	defaultCodexWSSilentRetry      = true
	defaultCodexWSSilentRetries    = 2
	defaultCodexWSSizeRouter       = true
	maxCodexWSSilentRetries        = 10
	defaultCodexWSBusyMaxWaitSec   = 30
	defaultCodexWSBusyPatienceSec  = 2
	maxCodexWSBusyWaitSec          = 300
	defaultCodexWSStatelessSlots   = 8
	maxCodexWSStatelessSlots       = 32

	defaultCodexContinueMaxRounds = 8
	minCodexContinueMaxRounds     = 1
	maxCodexContinueMaxRounds     = 32
)

type RuntimeSettings struct {
	ClientCompatMode      string
	CodexMinCLIVersion    string
	CodexUserAgentConfig  string
	StreamFlushPolicy     string
	StreamFlushIntervalMS int
	FirstTokenMode        string
	FirstTokenTimeoutSec  int
	BillingTierPolicy     string
	// ModelsListReadMaxBytes 是上游 /v1/models 与 Codex 模型清单成功响应的读取上限。
	ModelsListReadMaxBytes int64
	CodexForceWebsocket    bool // 强制 Codex 上游走 WebSocket（默认 false）
	// CodexRequestCompression 对 HTTP /responses 请求体做 zstd 压缩（默认 true，
	// 与真实 Codex CLI 一致）。与 CodexForceWebsocket 正交：WS 路径走
	// permessage-deflate（拨号器已开启），本项只作用于 HTTP 路径，两者可同时生效。
	CodexRequestCompression bool
	// CodexWSWeakNetworkMode 对 VPN/住宅代理等不稳定链路采用保守复用：
	// 缩短空闲/最大寿命、每次复用都做真实 Ping/Pong，并暂停空闲保活（默认 false）。
	CodexWSWeakNetworkMode bool
	CodexWSHideErrors      bool                           // 隐藏 Codex WS 上游原始错误（默认 true）
	CodexWSSilentRetry     bool                           // 首包前 Codex WS 上游错误静默换号重试（默认 true）
	CodexWSSilentRetries   int                            // Codex WS 静默换号最大重试次数（默认 2）
	ContinuousRetryPolicy  database.ContinuousRetryPolicy // 上游错误持续重试选择器（默认关闭）
	CodexWSSizeRouter      bool                           // 1009 自学习体积路由：超大请求直接首发 HTTP（默认 true）
	CodexWSBusyMaxWaitSec  int                            // busy session/容量等待的累计上限秒数（默认 30，issue #413）
	CodexWSBusyOverflow    bool                           // busy session 溢出到同账号兄弟连接（默认 false）
	CodexWSBusyPatienceSec int                            // 触发溢出前的短等待秒数（默认 2）
	// CodexWSStatelessSlots 无状态请求每 (账号, cacheKey) 维度的持久连接槽位数
	// （默认 8，范围 1-32，issue #522）。调大→单账号挂更多空闲连接；调小→握手更频繁，
	// 高 RPM 下可能触发上游握手限流。实际生效值仍受账号动态并发上限钳制。
	CodexWSStatelessSlots int
	// GithubToken 用于 api.github.com 请求的 Personal Access Token（提升 API 限流配额；
	// 只发给 api.github.com，绝不发给镜像或其他主机；空表示未配置，issue #522）。
	GithubToken string
	// GithubProxyURL GitHub 域名专用出站代理；空表示回落调用方的默认代理（issue #522）。
	GithubProxyURL string
	// Codex 过载熔断：单账号滑动窗口内 server_is_overloaded 错误占比达到阈值且样本数
	// 足够时，自动暂停该账号调度一段时间（默认关闭）。
	CodexOverloadPauseEnabled     bool
	CodexOverloadThresholdPercent int // 触发比例（%），默认 20
	CodexOverloadPauseMinutes     int // 暂停时长（分钟），默认 30
	CodexOverloadWindowMinutes    int // 统计窗口（分钟），默认 5
	// OverflowAutoCompact 上下文超窗时自动摘要旧轮次并重试一次（实验性，默认 false，issue #415）。
	// 全局开关与 per-key limits.auto_compact_overflow 为「或」关系。
	OverflowAutoCompact bool
	// CompactViaResponses 把 /v1/responses/compact 对官方 Codex OAuth 账号的透传
	// 改写为 /responses + compaction_trigger 的 body-signal 形态并聚合为一次性 JSON
	// （上游已下线专用 compact 端点，默认 false）。中转账号不受影响。
	CompactViaResponses bool
	// CodexPreflightSSEPassthrough 将前置元数据事件（codex.rate_limits /
	// codex.response.metadata / response.metadata）立即透传下游而不缓冲到首个内容
	// 事件（旧版兼容，默认 false，issue #425）。开启后首内容前会提前提交 200，
	// 该窗口内的 response.failed 只能以 SSE failed 帧下发，真实错误码/静默换号/
	// 超窗压缩重试均不可用。
	CodexPreflightSSEPassthrough bool
	// FirstTokenExcludesWsAcquire 落库的 first_token_ms 是否扣除本次 attempt 的
	// WS 取连耗时（默认 false，保持含取连的原口径；原始值 = first_token_ms + ws_acquire_ms）。
	FirstTokenExcludesWsAcquire bool
	// CodexContinueThinking 检测到上游按 518n-2 指纹截断思考时自动续想并折叠成单响应（默认 false）。
	CodexContinueThinking  bool
	CodexContinueMaxRounds int // 单次请求最大续想轮数，含首轮（默认 8，范围 1-32）
	// RequestIsolationMode 控制无显式会话请求的上游身份隔离粒度（isolated|per-api-key，默认 isolated）。
	RequestIsolationMode string
	// CodexSyncedCLIVersion 是从 openai/codex releases 同步到的最新 Codex CLI 版本；
	// 用于抬升出站 UA / manifest 的模拟版本，绝不低于内置常量，空表示未同步。
	CodexSyncedCLIVersion string
	// CodexCLIVersionSyncEnabled 控制后台定时同步 Codex CLI 版本（默认 true）。
	CodexCLIVersionSyncEnabled bool
	// CodexCLIVersionSyncIntervalHours 定时同步间隔（小时，默认 12，范围 1-720）。
	CodexCLIVersionSyncIntervalHours int
	// AutoResetCreditsEnabled 控制 Plus/Pro 主动重置次数的临期自动消费（默认 false）。
	AutoResetCreditsEnabled bool
	// AutoResetCreditsBeforeExpiryMin 是进入自动消费窗口的提前分钟数（默认 60）。
	AutoResetCreditsBeforeExpiryMin int
	// AutoActivate5hWindowEnabled 控制 5h 窗口重置后是否发送一次最小真实 /responses 启动下一轮窗口（默认 false，issue #581）。
	AutoActivate5hWindowEnabled bool
	// UTLSShutdownTimeoutMin 是 uTLS（CODEX_TRANSPORT_MODE=utls_chrome）连接被摘出
	// 连接池后，等待其上在途 stream 收尾的上限（分钟，默认 30，范围 1-240）。
	// 超时则强制关闭，保证异常挂死的 stream 不会把连接永久留住（issue #446）。
	UTLSShutdownTimeoutMin int
}

// UTLSShutdownTimeout 返回 uTLS 连接优雅关闭的等待上限。
func (s RuntimeSettings) UTLSShutdownTimeout() time.Duration {
	return time.Duration(database.NormalizeUTLSShutdownTimeoutMinutes(s.UTLSShutdownTimeoutMin)) * time.Minute
}

// IsolateRequestsByDefault 返回是否对无显式会话的请求默认按每请求隔离上游身份。
// 仅 per-api-key 模式返回 false（恢复按 API Key 共享缓存的旧行为）。
func (s RuntimeSettings) IsolateRequestsByDefault() bool {
	return NormalizeRequestIsolationMode(s.RequestIsolationMode) != RequestIsolationModePerAPIKey
}

var (
	runtimeSettings         atomic.Value // stores RuntimeSettings
	runtimeSettingsUpdateMu sync.Mutex
)

func init() {
	runtimeSettings.Store(DefaultRuntimeSettings())
}

func DefaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{
		ClientCompatMode:                 defaultClientCompatMode,
		CodexMinCLIVersion:               defaultCodexMinCLIVersion,
		CodexUserAgentConfig:             DefaultCodexUserAgentConfigJSON(),
		StreamFlushPolicy:                defaultStreamFlushPolicy,
		StreamFlushIntervalMS:            defaultStreamFlushIntervalMS,
		FirstTokenMode:                   defaultFirstTokenMode,
		FirstTokenTimeoutSec:             defaultFirstTokenTimeoutSec,
		BillingTierPolicy:                defaultBillingTierPolicy,
		ModelsListReadMaxBytes:           database.DefaultModelsListReadMaxBytes,
		CodexRequestCompression:          defaultCodexRequestCompression,
		CodexWSHideErrors:                defaultCodexWSHideErrors,
		CodexWSSilentRetry:               defaultCodexWSSilentRetry,
		CodexWSSilentRetries:             defaultCodexWSSilentRetries,
		ContinuousRetryPolicy:            database.DefaultContinuousRetryPolicy(),
		CodexWSSizeRouter:                defaultCodexWSSizeRouter,
		CodexWSBusyMaxWaitSec:            defaultCodexWSBusyMaxWaitSec,
		CodexWSBusyPatienceSec:           defaultCodexWSBusyPatienceSec,
		CodexWSStatelessSlots:            defaultCodexWSStatelessSlots,
		CodexOverloadThresholdPercent:    database.NormalizeCodexOverloadThresholdPercent(0),
		CodexOverloadPauseMinutes:        database.NormalizeCodexOverloadPauseMinutes(0),
		CodexOverloadWindowMinutes:       database.NormalizeCodexOverloadWindowMinutes(0),
		CodexContinueMaxRounds:           defaultCodexContinueMaxRounds,
		RequestIsolationMode:             defaultRequestIsolationMode(),
		CodexCLIVersionSyncEnabled:       true,
		CodexCLIVersionSyncIntervalHours: 12,
		AutoResetCreditsBeforeExpiryMin:  60,
		UTLSShutdownTimeoutMin:           database.NormalizeUTLSShutdownTimeoutMinutes(0),
	}
}

// defaultRequestIsolationMode 从环境变量解析默认隔离模式；缺省为按每请求隔离。
// CODEX_REQUEST_ISOLATION_MODE=per-api-key（或 per_api_key / shared / cache）可切回旧的
// 按 API Key 共享缓存行为，作为依赖隐式缓存命中的部署的逃生阀。
func defaultRequestIsolationMode() string {
	return NormalizeRequestIsolationMode(os.Getenv("CODEX_REQUEST_ISOLATION_MODE"))
}

// NormalizeRequestIsolationMode 归一化隔离模式，空/未知值回落到 isolated。
func NormalizeRequestIsolationMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case RequestIsolationModePerAPIKey, "per_api_key", "per-apikey", "shared", "cache":
		return RequestIsolationModePerAPIKey
	default:
		return RequestIsolationModeIsolated
	}
}

func NormalizeClientCompatMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ClientCompatModePreserve:
		return ClientCompatModePreserve
	case ClientCompatModeAuto:
		return ClientCompatModeAuto
	case ClientCompatModeForce:
		return ClientCompatModeForce
	default:
		return ClientCompatModePreserve
	}
}

func NormalizeStreamFlushPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", StreamFlushPolicyImmediate:
		return StreamFlushPolicyImmediate
	case StreamFlushPolicyCoalesce:
		return StreamFlushPolicyCoalesce
	default:
		return StreamFlushPolicyImmediate
	}
}

func NormalizeFirstTokenMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", FirstTokenModeStrict:
		return FirstTokenModeStrict
	case FirstTokenModeLoose:
		return FirstTokenModeLoose
	default:
		return FirstTokenModeStrict
	}
}

func NormalizeBillingTierPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", BillingTierPolicyActual:
		return BillingTierPolicyActual
	case BillingTierPolicyRequested:
		return BillingTierPolicyRequested
	default:
		return BillingTierPolicyActual
	}
}

func NormalizeRuntimeSettings(settings RuntimeSettings) RuntimeSettings {
	defaults := DefaultRuntimeSettings()
	settings.ClientCompatMode = NormalizeClientCompatMode(settings.ClientCompatMode)
	settings.StreamFlushPolicy = NormalizeStreamFlushPolicy(settings.StreamFlushPolicy)
	settings.FirstTokenMode = NormalizeFirstTokenMode(settings.FirstTokenMode)
	settings.BillingTierPolicy = NormalizeBillingTierPolicy(settings.BillingTierPolicy)
	settings.ModelsListReadMaxBytes = database.NormalizeModelsListReadMaxBytes(settings.ModelsListReadMaxBytes)
	settings.RequestIsolationMode = NormalizeRequestIsolationMode(settings.RequestIsolationMode)
	if strings.TrimSpace(settings.CodexMinCLIVersion) == "" {
		settings.CodexMinCLIVersion = defaults.CodexMinCLIVersion
	} else {
		settings.CodexMinCLIVersion = strings.TrimSpace(settings.CodexMinCLIVersion)
	}
	if normalized, err := NormalizeCodexUserAgentConfigJSON(settings.CodexUserAgentConfig); err == nil {
		settings.CodexUserAgentConfig = normalized
	} else {
		settings.CodexUserAgentConfig = defaults.CodexUserAgentConfig
	}
	if settings.StreamFlushIntervalMS < minStreamFlushIntervalMS {
		settings.StreamFlushIntervalMS = defaults.StreamFlushIntervalMS
	}
	if settings.StreamFlushIntervalMS > maxStreamFlushIntervalMS {
		settings.StreamFlushIntervalMS = maxStreamFlushIntervalMS
	}
	if settings.FirstTokenTimeoutSec < 0 {
		settings.FirstTokenTimeoutSec = defaultFirstTokenTimeoutSec
	}
	if settings.FirstTokenTimeoutSec > maxFirstTokenTimeoutSec {
		settings.FirstTokenTimeoutSec = maxFirstTokenTimeoutSec
	}
	if settings.CodexWSSilentRetries < 0 {
		settings.CodexWSSilentRetries = 0
	}
	if settings.CodexWSSilentRetries > maxCodexWSSilentRetries {
		settings.CodexWSSilentRetries = maxCodexWSSilentRetries
	}
	if settings.CodexWSBusyMaxWaitSec <= 0 {
		settings.CodexWSBusyMaxWaitSec = defaultCodexWSBusyMaxWaitSec
	}
	if settings.CodexWSBusyMaxWaitSec > maxCodexWSBusyWaitSec {
		settings.CodexWSBusyMaxWaitSec = maxCodexWSBusyWaitSec
	}
	if settings.CodexWSBusyPatienceSec < 0 {
		settings.CodexWSBusyPatienceSec = defaultCodexWSBusyPatienceSec
	}
	if settings.CodexWSBusyPatienceSec > maxCodexWSBusyWaitSec {
		settings.CodexWSBusyPatienceSec = maxCodexWSBusyWaitSec
	}
	if settings.CodexWSStatelessSlots <= 0 {
		settings.CodexWSStatelessSlots = defaultCodexWSStatelessSlots
	}
	if settings.CodexWSStatelessSlots > maxCodexWSStatelessSlots {
		settings.CodexWSStatelessSlots = maxCodexWSStatelessSlots
	}
	settings.CodexOverloadThresholdPercent = database.NormalizeCodexOverloadThresholdPercent(settings.CodexOverloadThresholdPercent)
	settings.CodexOverloadPauseMinutes = database.NormalizeCodexOverloadPauseMinutes(settings.CodexOverloadPauseMinutes)
	settings.CodexOverloadWindowMinutes = database.NormalizeCodexOverloadWindowMinutes(settings.CodexOverloadWindowMinutes)
	if settings.CodexContinueMaxRounds < minCodexContinueMaxRounds {
		settings.CodexContinueMaxRounds = defaults.CodexContinueMaxRounds
	}
	if settings.CodexContinueMaxRounds > maxCodexContinueMaxRounds {
		settings.CodexContinueMaxRounds = maxCodexContinueMaxRounds
	}
	settings.AutoResetCreditsBeforeExpiryMin = database.NormalizeAutoResetCreditsBeforeExpiryMinutes(settings.AutoResetCreditsBeforeExpiryMin)
	settings.UTLSShutdownTimeoutMin = database.NormalizeUTLSShutdownTimeoutMinutes(settings.UTLSShutdownTimeoutMin)
	settings.ContinuousRetryPolicy = database.NormalizeContinuousRetryPolicy(settings.ContinuousRetryPolicy)
	return settings
}

func ApplyRuntimeSettingsFromSystem(settings *database.SystemSettings) RuntimeSettings {
	runtimeSettingsUpdateMu.Lock()
	defer runtimeSettingsUpdateMu.Unlock()

	next := DefaultRuntimeSettings()
	if settings != nil {
		next.ClientCompatMode = settings.ClientCompatMode
		next.CodexMinCLIVersion = settings.CodexMinCLIVersion
		next.CodexUserAgentConfig = settings.CodexUserAgentConfig
		next.StreamFlushPolicy = settings.StreamFlushPolicy
		next.StreamFlushIntervalMS = settings.StreamFlushIntervalMS
		next.FirstTokenMode = settings.FirstTokenMode
		next.FirstTokenTimeoutSec = settings.FirstTokenTimeoutSeconds
		next.BillingTierPolicy = settings.BillingTierPolicy
		next.ModelsListReadMaxBytes = settings.ModelsListReadMaxBytes
		next.CodexForceWebsocket = settings.CodexForceWebsocket
		next.CodexRequestCompression = settings.CodexRequestCompression
		next.CodexWSWeakNetworkMode = settings.CodexWSWeakNetworkMode
		next.CodexWSHideErrors = settings.CodexWSHideUpstreamErrors
		next.CodexWSSilentRetry = settings.CodexWSSilentRetryEnabled
		next.CodexWSSilentRetries = settings.CodexWSSilentMaxRetries
		next.ContinuousRetryPolicy = database.ParseContinuousRetryPolicy(settings.ContinuousRetryPolicy)
		if strings.TrimSpace(settings.ContinuousRetryPolicy) == "" {
			next.ContinuousRetryPolicy = database.DefaultContinuousRetryPolicy()
		}
		next.CodexWSSizeRouter = settings.CodexWSSizeRouterEnabled
		next.CodexWSBusyMaxWaitSec = settings.CodexWSBusyAcquireMaxWaitSec
		next.CodexWSBusyOverflow = settings.CodexWSBusyOverflowEnabled
		next.CodexWSBusyPatienceSec = settings.CodexWSBusyPatienceSec
		next.CodexWSStatelessSlots = settings.CodexWSStatelessSlots
		next.GithubToken = strings.TrimSpace(settings.GithubToken)
		next.GithubProxyURL = strings.TrimSpace(settings.GithubProxyURL)
		next.CodexOverloadPauseEnabled = settings.CodexOverloadPauseEnabled
		next.CodexOverloadThresholdPercent = settings.CodexOverloadThresholdPercent
		next.CodexOverloadPauseMinutes = settings.CodexOverloadPauseMinutes
		next.CodexOverloadWindowMinutes = settings.CodexOverloadWindowMinutes
		next.OverflowAutoCompact = settings.OverflowAutoCompactEnabled
		next.CompactViaResponses = settings.CompactViaResponsesEnabled
		next.CodexPreflightSSEPassthrough = settings.CodexPreflightSSEPassthroughEnabled
		next.FirstTokenExcludesWsAcquire = settings.FirstTokenExcludesWsAcquire
		next.CodexContinueThinking = settings.CodexContinueThinkingEnabled
		next.CodexContinueMaxRounds = settings.CodexContinueMaxRounds
		next.CodexSyncedCLIVersion = settings.CodexSyncedCLIVersion
		next.CodexCLIVersionSyncEnabled = settings.CodexCLIVersionSyncEnabled
		next.CodexCLIVersionSyncIntervalHours = settings.CodexCLIVersionSyncIntervalHours
		next.AutoResetCreditsEnabled = settings.AutoResetCreditsEnabled
		next.AutoResetCreditsBeforeExpiryMin = settings.AutoResetCreditsBeforeExpiryMin
		next.AutoActivate5hWindowEnabled = settings.AutoActivate5hWindowEnabled
		next.UTLSShutdownTimeoutMin = settings.UTLSShutdownTimeoutMinutes
		// Payload 重写规则不进 RuntimeSettings（编译后独立存放），此处顺带完成启动种子。
		if err := SetPayloadRulesJSON(settings.PayloadRules); err != nil {
			log.Printf("payload_rules 配置解析失败，已忽略: %v", err)
		}
		SetGrokFollowUpEffortConfig(auth.GrokFollowUpEffortConfigFromJSON(settings.GrokConfig))
	}
	return storeRuntimeSettings(next)
}

func ApplyRuntimeSettings(settings RuntimeSettings) RuntimeSettings {
	runtimeSettingsUpdateMu.Lock()
	defer runtimeSettingsUpdateMu.Unlock()
	return storeRuntimeSettings(settings)
}

// UpdateRuntimeSettings 在与完整设置写入共享的临界区内更新运行时配置。
// 后台任务应使用这个函数只修改自己拥有的字段，避免旧快照覆盖管理员刚保存的设置。
func UpdateRuntimeSettings(update func(RuntimeSettings) RuntimeSettings) RuntimeSettings {
	runtimeSettingsUpdateMu.Lock()
	defer runtimeSettingsUpdateMu.Unlock()

	next := currentRuntimeSettings()
	if update != nil {
		next = update(next)
	}
	return storeRuntimeSettings(next)
}

func CurrentRuntimeSettings() RuntimeSettings {
	return currentRuntimeSettings()
}

func currentRuntimeSettings() RuntimeSettings {
	if v, ok := runtimeSettings.Load().(RuntimeSettings); ok {
		return NormalizeRuntimeSettings(v)
	}
	return DefaultRuntimeSettings()
}

func storeRuntimeSettings(settings RuntimeSettings) RuntimeSettings {
	settings = NormalizeRuntimeSettings(settings)
	runtimeSettings.Store(settings)
	return settings
}

func currentStreamFlushInterval() time.Duration {
	ms := CurrentRuntimeSettings().StreamFlushIntervalMS
	if ms < minStreamFlushIntervalMS {
		ms = defaultStreamFlushIntervalMS
	}
	return time.Duration(ms) * time.Millisecond
}

func currentFirstTokenTimeout() time.Duration {
	seconds := CurrentRuntimeSettings().FirstTokenTimeoutSec
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func currentFirstTokenMode() string {
	return CurrentRuntimeSettings().FirstTokenMode
}

// codexContinueThinkingSettings 返回续想折叠开关与最大轮数（一次快照读取）。
func codexContinueThinkingSettings() (bool, int) {
	s := CurrentRuntimeSettings()
	return s.CodexContinueThinking, s.CodexContinueMaxRounds
}
