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
	defaultDisableFastMode       = false
	defaultCodexWSHideErrors     = true
	defaultCodexWSSilentRetry    = true
	defaultCodexWSSilentRetries  = 2
	maxCodexWSSilentRetries      = 10
)

type RuntimeSettings struct {
	ClientCompatMode      string
	CodexMinCLIVersion    string
	StreamFlushPolicy     string
	StreamFlushIntervalMS int
	FirstTokenMode        string
	FirstTokenTimeoutSec  int
	BillingTierPolicy     string
	DisableFastMode       bool // Ignore client fast/priority service tier requests when true.
	CodexForceWebsocket   bool // 强制 Codex 上游走 WebSocket（默认 false）
	CodexWSHideErrors     bool // 隐藏 Codex WS 上游原始错误（默认 true）
	CodexWSSilentRetry    bool // 首包前 Codex WS 上游错误静默换号重试（默认 true）
	CodexWSSilentRetries  int  // Codex WS 静默换号最大重试次数（默认 2）
	// RequestIsolationMode 控制无显式会话请求的上游身份隔离粒度（isolated|per-api-key，默认 isolated）。
	RequestIsolationMode string
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
		StreamFlushPolicy:     defaultStreamFlushPolicy,
		StreamFlushIntervalMS: defaultStreamFlushIntervalMS,
		FirstTokenMode:        defaultFirstTokenMode,
		FirstTokenTimeoutSec:  defaultFirstTokenTimeoutSec,
		BillingTierPolicy:     defaultBillingTierPolicy,
		DisableFastMode:       defaultDisableFastModeFromEnv(),
		CodexWSHideErrors:     defaultCodexWSHideErrors,
		CodexWSSilentRetry:    defaultCodexWSSilentRetry,
		CodexWSSilentRetries:  defaultCodexWSSilentRetries,
		RequestIsolationMode:  defaultRequestIsolationMode(),
	}
}

// defaultRequestIsolationMode 从环境变量解析默认隔离模式；缺省为按每请求隔离。
// CODEX_REQUEST_ISOLATION_MODE=per-api-key（或 per_api_key / shared / cache）可切回旧的
// 按 API Key 共享缓存行为，作为依赖隐式缓存命中的部署的逃生阀。
func defaultRequestIsolationMode() string {
	return NormalizeRequestIsolationMode(os.Getenv("CODEX_REQUEST_ISOLATION_MODE"))
}

func defaultDisableFastModeFromEnv() bool {
	for _, key := range []string{"CODEX_DISABLE_FAST_MODE", "DISABLE_FAST_MODE"} {
		if truthyRuntimeEnv(os.Getenv(key)) {
			return true
		}
	}
	return defaultDisableFastMode
}

func truthyRuntimeEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on", "enable", "enabled":
		return true
	default:
		return false
	}
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
	if strings.TrimSpace(settings.CodexMinCLIVersion) == "" {
		settings.CodexMinCLIVersion = defaults.CodexMinCLIVersion
	} else {
		settings.CodexMinCLIVersion = strings.TrimSpace(settings.CodexMinCLIVersion)
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
