package proxy

import (
	"os"
	"strings"
	"sync/atomic"
	"time"

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

	// CodexMimicMode 控制对上游伪装真实 Codex CLI 的严格程度。取值：
	//   legacy —— 保持本项目历史行为（Originator=codex-tui、握手头用 Session_id/
	//             Conversation_id、UA 带尾部后缀）。默认值，零 regression。
	//   strict —— 逐字节对齐官方 codex-rs 客户端（originator 默认 codex_cli_rs 且等于
	//             默认值时不发 Originator 头、会话头改用 session-id/thread-id、HTTP 路径
	//             补发 OpenAI-Beta 与 x-codex-* 客户端指纹头）。
	// 用环境变量 CODEX_MIMIC_MODE 覆盖默认值；灰度验证上游不拒绝后再切 strict。
	CodexMimicModeLegacy = "legacy"
	CodexMimicModeStrict = "strict"

	defaultClientCompatMode      = ClientCompatModePreserve
	defaultCodexMinCLIVersion    = "0.118.0"
	defaultStreamFlushPolicy     = StreamFlushPolicyImmediate
	defaultStreamFlushIntervalMS = 20
	minStreamFlushIntervalMS     = 1
	maxStreamFlushIntervalMS     = 1000
	defaultFirstTokenMode        = FirstTokenModeStrict
	defaultFirstTokenTimeoutSec  = 0
	maxFirstTokenTimeoutSec      = 600
	defaultBillingTierPolicy     = BillingTierPolicyActual
	defaultCodexWSHideErrors     = true
	defaultCodexWSSilentRetry    = true
	defaultCodexWSSilentRetries  = 2
	maxCodexWSSilentRetries      = 10
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
	CodexForceWebsocket   bool // 强制 Codex 上游走 WebSocket（默认 false）
	CodexWSHideErrors     bool // 隐藏 Codex WS 上游原始错误（默认 true）
	CodexWSSilentRetry    bool // 首包前 Codex WS 上游错误静默换号重试（默认 true）
	CodexWSSilentRetries  int  // Codex WS 静默换号最大重试次数（默认 2）
	// RequestIsolationMode 控制无显式会话请求的上游身份隔离粒度（isolated|per-api-key，默认 isolated）。
	RequestIsolationMode string
	// CodexMimicMode 控制对上游伪装真实 Codex CLI 的严格程度（legacy|strict，默认 legacy）。
	CodexMimicMode string
}

// MimicStrictHeaders 返回是否启用严格伪装（与官方 codex-rs 客户端逐字节对齐）。
func (s RuntimeSettings) MimicStrictHeaders() bool {
	return NormalizeCodexMimicMode(s.CodexMimicMode) == CodexMimicModeStrict
}

// IsolateRequestsByDefault 返回是否对无显式会话的请求默认按每请求隔离上游身份。
// 仅 per-api-key 模式返回 false（恢复按 API Key 共享缓存的旧行为）。
func (s RuntimeSettings) IsolateRequestsByDefault() bool {
	return NormalizeRequestIsolationMode(s.RequestIsolationMode) != RequestIsolationModePerAPIKey
}

var runtimeSettings atomic.Value // stores RuntimeSettings

func init() {
	runtimeSettings.Store(DefaultRuntimeSettings())
}

func DefaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{
		ClientCompatMode:      defaultClientCompatMode,
		CodexMinCLIVersion:    defaultCodexMinCLIVersion,
		CodexUserAgentConfig:  DefaultCodexUserAgentConfigJSON(),
		StreamFlushPolicy:     defaultStreamFlushPolicy,
		StreamFlushIntervalMS: defaultStreamFlushIntervalMS,
		FirstTokenMode:        defaultFirstTokenMode,
		FirstTokenTimeoutSec:  defaultFirstTokenTimeoutSec,
		BillingTierPolicy:     defaultBillingTierPolicy,
		CodexWSHideErrors:     defaultCodexWSHideErrors,
		CodexWSSilentRetry:    defaultCodexWSSilentRetry,
		CodexWSSilentRetries:  defaultCodexWSSilentRetries,
		RequestIsolationMode:  defaultRequestIsolationMode(),
		CodexMimicMode:        defaultCodexMimicMode(),
	}
}

// defaultCodexMimicMode 从环境变量解析默认伪装模式；缺省为 legacy（保持历史行为）。
// CODEX_MIMIC_MODE=strict 切换到与官方 codex-rs 客户端逐字节对齐的严格模式。
func defaultCodexMimicMode() string {
	return NormalizeCodexMimicMode(os.Getenv("CODEX_MIMIC_MODE"))
}

// NormalizeCodexMimicMode 归一化伪装模式，空/未知值回落到 legacy。
func NormalizeCodexMimicMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case CodexMimicModeStrict, "codex_cli_rs", "cli_rs", "real":
		return CodexMimicModeStrict
	default:
		return CodexMimicModeLegacy
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
	settings.RequestIsolationMode = NormalizeRequestIsolationMode(settings.RequestIsolationMode)
	settings.CodexMimicMode = NormalizeCodexMimicMode(settings.CodexMimicMode)
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
	return settings
}

func ApplyRuntimeSettingsFromSystem(settings *database.SystemSettings) RuntimeSettings {
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
		next.CodexForceWebsocket = settings.CodexForceWebsocket
		next.CodexWSHideErrors = settings.CodexWSHideUpstreamErrors
		next.CodexWSSilentRetry = settings.CodexWSSilentRetryEnabled
		next.CodexWSSilentRetries = settings.CodexWSSilentMaxRetries
	}
	next = NormalizeRuntimeSettings(next)
	runtimeSettings.Store(next)
	return next
}

func ApplyRuntimeSettings(settings RuntimeSettings) RuntimeSettings {
	settings = NormalizeRuntimeSettings(settings)
	runtimeSettings.Store(settings)
	return settings
}

func CurrentRuntimeSettings() RuntimeSettings {
	if v, ok := runtimeSettings.Load().(RuntimeSettings); ok {
		return NormalizeRuntimeSettings(v)
	}
	return DefaultRuntimeSettings()
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
