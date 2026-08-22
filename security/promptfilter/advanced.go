package promptfilter

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

type AdvancedConfig struct {
	Normalization   NormalizationConfig   `json:"normalization"`
	ContextDiscount ContextDiscountConfig `json:"context_discount"`
	Enforcement     EnforcementConfig     `json:"enforcement"`
	Risk            RiskConfig            `json:"risk"`
	Sidecar         SidecarConfig         `json:"sidecar"`
	Session         SessionConfig         `json:"session"`
	Attachment      AttachmentConfig      `json:"attachment"`
	Output          OutputConfig          `json:"output"`
	Intelligence    IntelligenceConfig    `json:"intelligence"`
	NewAPI          NewAPIConfig          `json:"newapi"`
	Guard           GuardConfig           `json:"guard"`
	ReviewAdapter   ReviewAdapterConfig   `json:"review_adapter"`
	AdaptiveReview  AdaptiveReviewConfig  `json:"adaptive_review"`
}

const (
	GuardModeInherit = "inherit"
	GuardModeOff     = "off"
	GuardModeShadow  = "shadow"
	GuardModeWarn    = "warn"
	GuardModeEnforce = "enforce"

	GuardProfileBalanced = "balanced"
	GuardProfileStrict   = "strict"
	GuardProfileResearch = "research"

	GuardShadowOverflowDrop = "drop"
	GuardShadowOverflowSync = "sync"
)

// GuardConfig controls the extensible request guard pipeline. The existing
// prompt-filter switch remains the master switch; Mode "inherit" maps the
// existing monitor/warn/block setting into shadow/warn/enforce.
// New source layers default to profile-controlled behavior; the balanced
// profile scans only the current user prompt, preserving legacy behavior.
type GuardConfig struct {
	Mode                  string                 `json:"mode"`
	DefaultProfile        string                 `json:"default_profile"`
	AllowTrustedOverrides bool                   `json:"allow_trusted_overrides"`
	ProviderProfiles      map[string]string      `json:"provider_profiles,omitempty"`
	Layers                GuardLayerConfig       `json:"layers"`
	Performance           GuardPerformanceConfig `json:"performance"`
}

// GuardPerformanceConfig controls transparent local scan acceleration and the
// optional removal of audit-only auxiliary shadow scans from the client-visible
// first-token path. Current-user/application input and every warn/enforce layer
// always remain synchronous regardless of these settings.
type GuardPerformanceConfig struct {
	AsyncShadowAuxiliaryEnabled bool   `json:"async_shadow_auxiliary_enabled"`
	ExactSegmentCacheEnabled    bool   `json:"exact_segment_cache_enabled"`
	ExactSegmentCacheEntries    int    `json:"exact_segment_cache_entries"`
	ExactSegmentCacheTTLSeconds int    `json:"exact_segment_cache_ttl_seconds"`
	MaxSegments                 int    `json:"max_segments"`
	MaxCurrentUserBytes         int    `json:"max_current_user_bytes"`
	MaxAuxiliaryBytes           int    `json:"max_auxiliary_bytes"`
	ScanChunkBytes              int    `json:"scan_chunk_bytes"`
	ScanOverlapBytes            int    `json:"scan_overlap_bytes"`
	ShadowWorkers               int    `json:"shadow_workers"`
	ShadowQueueSize             int    `json:"shadow_queue_size"`
	ShadowOverflowMode          string `json:"shadow_overflow_mode"`
}

const (
	MinGuardMaxSegments              = 1
	MaxGuardMaxSegments              = 256
	MinGuardCurrentUserBytes         = 8 * 1024
	MaxGuardCurrentUserBytes         = 1024 * 1024
	MinGuardAuxiliaryBytes           = 0
	MaxGuardAuxiliaryBytes           = 256 * 1024
	MinGuardScanChunkBytes           = 1024
	MaxGuardScanChunkBytes           = 64 * 1024
	MinGuardScanOverlapBytes         = 64
	MaxGuardScanOverlapBytes         = 8 * 1024
	DefaultGuardScanChunkBytes       = 16 * 1024
	DefaultGuardScanOverlapBytes     = 8 * 1024
	RecommendedGuardMaxSegments      = 64
	RecommendedGuardCurrentUserBytes = 128 * 1024
	RecommendedGuardAuxiliaryBytes   = 32 * 1024
	RecommendedGuardScanChunkBytes   = 8 * 1024
	RecommendedGuardScanOverlapBytes = 512
)

type GuardLayerConfig struct {
	CurrentUser       GuardLayerModeConfig `json:"current_user"`
	History           GuardLayerModeConfig `json:"history"`
	System            GuardLayerModeConfig `json:"system"`
	Developer         GuardLayerModeConfig `json:"developer"`
	Instructions      GuardLayerModeConfig `json:"instructions"`
	ToolOutput        GuardLayerModeConfig `json:"tool_output"`
	ToolArguments     GuardLayerModeConfig `json:"tool_arguments"`
	AttachmentRefs    GuardLayerModeConfig `json:"attachment_refs"`
	SessionContext    GuardLayerModeConfig `json:"session_context"`
	AttachmentContent GuardLayerModeConfig `json:"attachment_content"`
}

type GuardLayerModeConfig struct {
	Mode string `json:"mode"`
}

// NewAPIConfig controls signed identity verification for the request-local
// Codex2API Key binding. Enabled is runtime-only: persisted/global configuration
// must never enable NewAPI identity verification without a concrete binding.
type NewAPIConfig struct {
	Enabled             bool `json:"-"`
	MaxClockSkewSeconds int  `json:"max_clock_skew_seconds"`
}

type EnforcementConfig struct {
	TerminalCategories       []string `json:"terminal_categories"`
	TerminalBypassModels     []string `json:"terminal_bypass_models"`
	LocalBlockMessage        string   `json:"local_block_message,omitempty"`
	ConversationLockEnabled  bool     `json:"conversation_lock_enabled"`
	ConversationLockTTLHours int      `json:"conversation_lock_ttl_hours"`
	UserCyberCooldownMinutes int      `json:"user_cyber_cooldown_minutes"`
	CYBStrikeEnabled         bool     `json:"cyb_strike_enabled"`
	// LocalSevereStrikeEnabled 决定本地判定的**最高置信度**严重违规
	// (当前用户直接发出 + 敏感意图 + 终局 strict/category 命中)是否累计到
	// NewAPI 用户身上,从而触发 NewAPI 侧的 CYB 累计与自动封号。
	//
	// 打开(默认)时,严重违规无需先到达上游产生真实 cyber_policy,即可在本地
	// 扼杀的同时把一次 strike 记到人;会话锁保证同一会话不重复累计。误封面被
	// 收敛到最高置信度那一档:低置信度拦截、工具输出、历史上下文都不累计。
	//
	// 关闭时,本地严重违规仍被拦截,但不再记 strike——拦截与封号解耦。
	LocalSevereStrikeEnabled bool `json:"local_severe_strike_enabled"`
	// AuthorizedPentestAllowed 决定是否承认请求里的"声明式授权"
	// ("我有书面授权"、"我自己的服务器"、"with permission")。
	//
	// 授权声明是无法验证的自述:开启时,攻击者只要加一句就能让定向入侵终局规则
	// 失效(实测 score 100 -> 20 放行)。关闭(默认)时这类声明不再具有豁免效力,
	// 与 review.go 中 "Authorization is evidence, not an assumption" 的既定策略
	// 保持一致。
	//
	// 仅当部署确实承载已授权渗透测试业务时才开启。
	AuthorizedPentestAllowed bool `json:"authorized_pentest_allowed"`
}

const (
	DefaultUserCyberCooldownMinutes = 30
	MinUserCyberCooldownMinutes     = 1
	MaxUserCyberCooldownMinutes     = 24 * 60
	MaxLocalBlockMessageRunes       = 2000
)

type NormalizationConfig struct {
	Enabled           bool `json:"enabled"`
	DecodeURL         bool `json:"decode_url"`
	DecodeHTML        bool `json:"decode_html"`
	DecodeBase64      bool `json:"decode_base64"`
	DecodeHex         bool `json:"decode_hex"`
	DecodeROT13       bool `json:"decode_rot13"`
	DecodeEscapes     bool `json:"decode_escapes"`
	DecodeCompression bool `json:"decode_compression"`
	MaxDecodeRuns     int  `json:"max_decode_runs"`
	MaxDecodedBytes   int  `json:"max_decoded_bytes"`
	MaxEncodedBlocks  int  `json:"max_encoded_blocks"`
}

// ContextDiscountConfig keeps legitimate defensive analysis usable without
// allowing a stack of generic "research only" statements to erase an
// otherwise explicit operational request.
type ContextDiscountConfig struct {
	Enabled                bool `json:"enabled"`
	IntentAware            bool `json:"intent_aware"`
	MaxDiscount            int  `json:"max_discount"`
	OperationalMaxDiscount int  `json:"operational_max_discount"`
}

type RiskConfig struct {
	Enabled                           bool `json:"enabled"`
	WindowSeconds                     int  `json:"window_seconds"`
	BlockThreshold                    int  `json:"block_threshold"`
	ReviewThreshold                   int  `json:"review_threshold"`
	UserWeightPercent                 int  `json:"user_weight_percent"`
	IPWeightPercent                   int  `json:"ip_weight_percent"`
	SessionWeightPercent              int  `json:"session_weight_percent"`
	SessionCreationLimitEnabled       bool `json:"session_creation_limit_enabled"`
	SessionCreationLimit              int  `json:"session_creation_limit"`
	SessionCreationLimitWindowSeconds int  `json:"session_creation_limit_window_seconds"`
}

// AdaptiveReviewConfig reduces synchronous model-review latency only after a
// signed person identity has accumulated enough clean reviewed evidence. Local
// deterministic protection remains active on every request, and periodic
// sampling plus an upper review interval prevent trust from becoming permanent.
type AdaptiveReviewConfig struct {
	Enabled                    bool `json:"enabled"`
	MinCleanReviews            int  `json:"min_clean_reviews"`
	MinObservationHours        int  `json:"min_observation_hours"`
	SamplePercent              int  `json:"sample_percent"`
	ForceReviewIntervalMinutes int  `json:"force_review_interval_minutes"`
	TrustDurationHours         int  `json:"trust_duration_hours"`
	ReactivationCleanReviews   int  `json:"reactivation_clean_reviews"`
	ReactivationCooldownHours  int  `json:"reactivation_cooldown_hours"`
}

type SidecarConfig struct {
	Enabled                bool   `json:"enabled"`
	BaseURL                string `json:"base_url"`
	TimeoutSeconds         int    `json:"timeout_seconds"`
	FailClosed             bool   `json:"fail_closed"`
	MinScore               int    `json:"min_score"`
	ScanCleanEnabled       bool   `json:"scan_clean_enabled"`
	SamplePercent          int    `json:"sample_percent"`
	Mode                   string `json:"mode"`
	MaxTextLength          int    `json:"max_text_length"`
	CacheTTLSeconds        int    `json:"cache_ttl_seconds"`
	MaxConcurrent          int    `json:"max_concurrent"`
	CircuitBreakerFailures int    `json:"circuit_breaker_failures"`
	CircuitBreakerSeconds  int    `json:"circuit_breaker_seconds"`
}

type SessionConfig struct {
	Enabled               bool `json:"enabled"`
	WindowSeconds         int  `json:"window_seconds"`
	MaxFragments          int  `json:"max_fragments"`
	MaxTextLength         int  `json:"max_text_length"`
	CombineShortFragments bool `json:"combine_short_fragments"`
	ShortFragmentMaxChars int  `json:"short_fragment_max_chars"`
	RequireSignedIdentity bool `json:"require_signed_identity"`
}

type AttachmentConfig struct {
	Enabled                bool   `json:"enabled"`
	BaseURL                string `json:"base_url"`
	TimeoutSeconds         int    `json:"timeout_seconds"`
	MaxFiles               int    `json:"max_files"`
	MaxBytes               int    `json:"max_bytes"`
	MaxExtractedChars      int    `json:"max_extracted_chars"`
	CacheTTLSeconds        int    `json:"cache_ttl_seconds"`
	MaxConcurrent          int    `json:"max_concurrent"`
	CircuitBreakerFailures int    `json:"circuit_breaker_failures"`
	CircuitBreakerSeconds  int    `json:"circuit_breaker_seconds"`
	AllowRemoteURLs        bool   `json:"allow_remote_urls"`
}

type OutputConfig struct {
	Enabled      bool `json:"enabled"`
	BufferBytes  int  `json:"buffer_bytes"`
	OverlapBytes int  `json:"overlap_bytes"`
	StrictOnly   bool `json:"strict_only"`
}

// IntelligenceConfig controls the optional public-source rule intelligence job.
// Generated proposals are always staged for human review and never enter the
// runtime rule engine until an administrator explicitly publishes them.
type IntelligenceConfig struct {
	Enabled          bool     `json:"enabled"`
	IntervalHours    int      `json:"interval_hours"`
	Queries          []string `json:"queries"`
	MaxSearchResults int      `json:"max_search_results"`
	ModelEnabled     bool     `json:"model_enabled"`
	Model            string   `json:"model"`
	MaxModelCalls    int      `json:"max_model_calls"`
}

func DefaultIntelligenceQueries() []string {
	return []string{
		"LLM jailbreak prompt injection",
		"ChatGPT jailbreak prompt",
		"Codex prompt injection jailbreak",
		"大模型 破限 提示词",
		"GPT 破甲 提示词",
		"AI 越狱 提示词",
		"中文 prompt injection 绕过",
	}
}

func DefaultAdvancedConfig() AdvancedConfig {
	return AdvancedConfig{
		Normalization:   NormalizationConfig{MaxDecodeRuns: 1, MaxDecodedBytes: 32768, MaxEncodedBlocks: 16},
		ContextDiscount: ContextDiscountConfig{Enabled: true, IntentAware: true, MaxDiscount: 90, OperationalMaxDiscount: 0},
		Risk:            RiskConfig{WindowSeconds: 600, BlockThreshold: 100, ReviewThreshold: 60, UserWeightPercent: 50, IPWeightPercent: 30, SessionWeightPercent: 20, SessionCreationLimit: 5, SessionCreationLimitWindowSeconds: 3600},
		AdaptiveReview:  AdaptiveReviewConfig{MinCleanReviews: 10, MinObservationHours: 24, SamplePercent: 5, ForceReviewIntervalMinutes: 360, TrustDurationHours: 168, ReactivationCleanReviews: 5, ReactivationCooldownHours: 24},
		Sidecar:         SidecarConfig{TimeoutSeconds: 1, FailClosed: false, MinScore: 30, SamplePercent: 5, Mode: GuardModeShadow, MaxTextLength: 8192, CacheTTLSeconds: 60, MaxConcurrent: 16, CircuitBreakerFailures: 3, CircuitBreakerSeconds: 30},
		Session:         SessionConfig{WindowSeconds: 300, MaxFragments: 3, MaxTextLength: 4096, ShortFragmentMaxChars: 24, RequireSignedIdentity: true},
		Attachment:      AttachmentConfig{TimeoutSeconds: 2, MaxFiles: 4, MaxBytes: 65536, MaxExtractedChars: 8192, CacheTTLSeconds: 300, MaxConcurrent: 8, CircuitBreakerFailures: 3, CircuitBreakerSeconds: 30},
		Output:          OutputConfig{BufferBytes: 4096, OverlapBytes: 512, StrictOnly: true},
		Intelligence:    IntelligenceConfig{IntervalHours: 24, Queries: DefaultIntelligenceQueries(), MaxSearchResults: 20, Model: "gpt-5.4", MaxModelCalls: 1},
		NewAPI:          NewAPIConfig{MaxClockSkewSeconds: 120},
		Enforcement:     EnforcementConfig{TerminalBypassModels: []string{"codex-auto-review"}, ConversationLockEnabled: true, ConversationLockTTLHours: 168, UserCyberCooldownMinutes: DefaultUserCyberCooldownMinutes, CYBStrikeEnabled: false, LocalSevereStrikeEnabled: true},
		Guard:           DefaultGuardConfig(),
	}
}

// RecommendedAdvancedConfig is intentionally separate from
// DefaultAdvancedConfig. The latter remains the compatibility fallback for
// older databases whose JSON is empty or missing fields; this preset is used
// only for fresh installs and explicit "recommended defaults" in the UI.
func RecommendedAdvancedConfig() AdvancedConfig {
	cfg := DefaultAdvancedConfig()
	cfg.Normalization = NormalizationConfig{
		Enabled:           true,
		DecodeURL:         true,
		DecodeHTML:        true,
		DecodeBase64:      true,
		DecodeHex:         true,
		DecodeROT13:       true,
		DecodeEscapes:     true,
		DecodeCompression: true,
		MaxDecodeRuns:     2,
		MaxDecodedBytes:   32768,
		MaxEncodedBlocks:  16,
	}
	cfg.Risk.UserWeightPercent = 60
	cfg.Risk.IPWeightPercent = 20
	cfg.Risk.SessionWeightPercent = 20
	cfg.AdaptiveReview.Enabled = true
	cfg.Sidecar.FailClosed = false
	// Signed session correlation is enabled for the recommended preset. Only an
	// explicit continuation inherits the preceding accepted user intent for
	// enforcement; heuristic short-fragment correlation remains opt-in.
	cfg.Session.Enabled = true
	cfg.Guard.DefaultProfile = GuardProfileBalanced
	cfg.Guard.Performance.AsyncShadowAuxiliaryEnabled = true
	cfg.Guard.Performance.MaxSegments = RecommendedGuardMaxSegments
	cfg.Guard.Performance.MaxCurrentUserBytes = RecommendedGuardCurrentUserBytes
	cfg.Guard.Performance.MaxAuxiliaryBytes = RecommendedGuardAuxiliaryBytes
	cfg.Guard.Performance.ScanChunkBytes = RecommendedGuardScanChunkBytes
	cfg.Guard.Performance.ScanOverlapBytes = RecommendedGuardScanOverlapBytes
	cfg.Guard.Layers = GuardLayerConfig{
		CurrentUser:       GuardLayerModeConfig{Mode: GuardModeEnforce},
		History:           GuardLayerModeConfig{Mode: GuardModeOff},
		System:            GuardLayerModeConfig{Mode: GuardModeOff},
		Developer:         GuardLayerModeConfig{Mode: GuardModeOff},
		Instructions:      GuardLayerModeConfig{Mode: GuardModeOff},
		ToolOutput:        GuardLayerModeConfig{Mode: GuardModeShadow},
		ToolArguments:     GuardLayerModeConfig{Mode: GuardModeOff},
		AttachmentRefs:    GuardLayerModeConfig{Mode: GuardModeShadow},
		SessionContext:    GuardLayerModeConfig{Mode: GuardModeShadow},
		AttachmentContent: GuardLayerModeConfig{Mode: GuardModeShadow},
	}
	return NormalizeAdvancedConfig(cfg)
}

func DefaultGuardConfig() GuardConfig {
	inherit := GuardLayerModeConfig{Mode: GuardModeInherit}
	return GuardConfig{
		Mode:             GuardModeInherit,
		DefaultProfile:   GuardProfileBalanced,
		ProviderProfiles: map[string]string{},
		Performance: GuardPerformanceConfig{
			ExactSegmentCacheEnabled:    true,
			ExactSegmentCacheEntries:    4096,
			ExactSegmentCacheTTLSeconds: 600,
			// Compatibility defaults intentionally mirror the pre-budget
			// MaxTextLength behavior. Fresh installs and the explicit recommended
			// preset use the tighter, source-specific values above.
			MaxSegments:         MaxGuardMaxSegments,
			MaxCurrentUserBytes: DefaultMaxTextLength,
			MaxAuxiliaryBytes:   DefaultMaxTextLength,
			ScanChunkBytes:      DefaultGuardScanChunkBytes,
			ScanOverlapBytes:    DefaultGuardScanOverlapBytes,
			ShadowWorkers:       2,
			ShadowQueueSize:     256,
			ShadowOverflowMode:  GuardShadowOverflowDrop,
		},
		Layers: GuardLayerConfig{
			CurrentUser:       inherit,
			History:           inherit,
			System:            inherit,
			Developer:         inherit,
			Instructions:      inherit,
			ToolOutput:        inherit,
			ToolArguments:     inherit,
			AttachmentRefs:    inherit,
			SessionContext:    inherit,
			AttachmentContent: inherit,
		},
	}
}

func ParseAdvancedConfig(raw string) (AdvancedConfig, error) {
	cfg := DefaultAdvancedConfig()
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return AdvancedConfig{}, err
	}
	return NormalizeAdvancedConfig(cfg), nil
}

// AdvancedConfigDocument keeps the persisted JSON separate from the
// normalized configuration used at runtime. Raw contains all known fields in
// their effective form while retaining fields introduced by newer versions.
type AdvancedConfigDocument struct {
	Raw       string
	Effective AdvancedConfig
}

// ParseAdvancedConfigDocument parses a persisted advanced configuration and
// returns both its normalized runtime form and a forward-compatible JSON
// document. Legacy partial documents are expanded with current defaults;
// unknown fields are retained at every object level.
func ParseAdvancedConfigDocument(raw string) (AdvancedConfigDocument, error) {
	root, err := parseAdvancedConfigObject(raw)
	if err != nil {
		return AdvancedConfigDocument{}, err
	}
	// Stable-cohort rollout used to be downgrade-only. Removing the field
	// without migrating an enabled legacy document would silently turn a
	// partial enforce deployment into full enforce. Collapse that retired
	// state to its configured safe fallback before deriving runtime config.
	migrateAdvancedConfigRetiredFields(root)
	migrated, err := json.Marshal(root)
	if err != nil {
		return AdvancedConfigDocument{}, err
	}
	effective, err := ParseAdvancedConfig(string(migrated))
	if err != nil {
		return AdvancedConfigDocument{}, err
	}
	formatted, err := marshalAdvancedConfigDocument(root, effective)
	if err != nil {
		return AdvancedConfigDocument{}, err
	}
	return AdvancedConfigDocument{Raw: formatted, Effective: effective}, nil
}

// MergeAdvancedConfigDocument applies an object-level JSON merge patch to an
// existing document. Objects are merged recursively, arrays and scalar values
// replace the previous value, and null removes a field. This lets an older UI
// update only fields it understands without deleting future-version fields.
func MergeAdvancedConfigDocument(baseRaw, patchRaw string) (AdvancedConfigDocument, error) {
	base, err := parseAdvancedConfigObject(baseRaw)
	if err != nil {
		return AdvancedConfigDocument{}, fmt.Errorf("parse existing advanced config: %w", err)
	}
	patch, err := parseAdvancedConfigObject(patchRaw)
	if err != nil {
		return AdvancedConfigDocument{}, fmt.Errorf("parse advanced config update: %w", err)
	}
	if err := mergeAdvancedConfigObjects(base, patch, ""); err != nil {
		return AdvancedConfigDocument{}, err
	}
	merged, err := json.Marshal(base)
	if err != nil {
		return AdvancedConfigDocument{}, err
	}
	return ParseAdvancedConfigDocument(string(merged))
}

// MarshalAdvancedConfigDocument overlays the effective known configuration
// onto a previously persisted document while retaining unknown fields. It is
// used when backend code changes the runtime configuration without receiving a
// new raw JSON document from the settings API.
func MarshalAdvancedConfigDocument(raw string, cfg AdvancedConfig) (string, error) {
	root, err := parseAdvancedConfigObject(raw)
	if err != nil {
		return "", err
	}
	return marshalAdvancedConfigDocument(root, cfg)
}

func parseAdvancedConfigObject(raw string) (map[string]json.RawMessage, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return make(map[string]json.RawMessage), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &object); err != nil {
		return nil, err
	}
	if object == nil {
		return make(map[string]json.RawMessage), nil
	}
	return object, nil
}

func marshalAdvancedConfigDocument(root map[string]json.RawMessage, cfg AdvancedConfig) (string, error) {
	removeAdvancedConfigSensitiveFields(root)
	removeAdvancedConfigRetiredFields(root)
	knownRaw, err := json.Marshal(NormalizeAdvancedConfig(cfg))
	if err != nil {
		return "", err
	}
	known, err := parseAdvancedConfigObject(string(knownRaw))
	if err != nil {
		return "", err
	}
	if err := mergeAdvancedConfigObjects(root, known, ""); err != nil {
		return "", err
	}
	formatted, err := json.Marshal(root)
	if err != nil {
		return "", err
	}
	return string(formatted), nil
}

// migrateAdvancedConfigRetiredFields converts an enabled legacy rollout into
// its safe global mode before removing the retired object. Explicit off,
// shadow and warn modes are already safe and are left untouched. Explicit
// enforce can preserve the old warn/shadow fallback. Missing, inherit and
// invalid modes depend on an outer legacy setting that is unavailable in this
// document; they therefore migrate to shadow so parsing can never promote a
// previously monitoring deployment to warn or enforce.
func migrateAdvancedConfigRetiredFields(root map[string]json.RawMessage) {
	for rootKey, raw := range root {
		if !strings.EqualFold(strings.TrimSpace(rootKey), "guard") {
			continue
		}
		guard, ok := decodeAdvancedConfigObject(raw)
		if !ok {
			continue
		}
		for key, rolloutRaw := range guard {
			if !strings.EqualFold(strings.TrimSpace(key), "rollout") {
				continue
			}
			rollout, rolloutOK := decodeAdvancedConfigObject(rolloutRaw)
			if legacyRolloutCouldBeEnabled(rolloutRaw, rollout, rolloutOK) {
				mode := strings.ToLower(strings.TrimSpace(advancedConfigObjectString(guard, "mode")))
				switch mode {
				case GuardModeOff, GuardModeShadow, GuardModeWarn:
					// The explicit mode was never promoted by rollout.
				case GuardModeEnforce:
					fallback := strings.ToLower(strings.TrimSpace(advancedConfigObjectString(rollout, "fallback_mode")))
					if fallback != GuardModeShadow && fallback != GuardModeWarn {
						fallback = GuardModeWarn
					}
					setAdvancedConfigObjectString(guard, "mode", fallback)
				default:
					setAdvancedConfigObjectString(guard, "mode", GuardModeShadow)
				}
			}
			delete(guard, key)
		}
		encoded, err := json.Marshal(guard)
		if err == nil {
			root[rootKey] = encoded
		}
	}
}

func removeAdvancedConfigRetiredFields(root map[string]json.RawMessage) {
	for rootKey, raw := range root {
		if !strings.EqualFold(strings.TrimSpace(rootKey), "guard") {
			continue
		}
		guard, ok := decodeAdvancedConfigObject(raw)
		if !ok {
			continue
		}
		// Stable cohort rollout has been retired. Unlike unknown future fields,
		// an old rollout object must not survive document round trips because
		// older values could otherwise appear active even though runtime
		// enforcement no longer consults them.
		for key := range guard {
			if strings.EqualFold(strings.TrimSpace(key), "rollout") {
				delete(guard, key)
			}
		}
		encoded, err := json.Marshal(guard)
		if err == nil {
			root[rootKey] = encoded
		}
	}
}

func legacyRolloutCouldBeEnabled(raw json.RawMessage, object map[string]json.RawMessage, decoded bool) bool {
	if isJSONNull(raw) {
		return false
	}
	// A malformed retired object could not have been interpreted reliably by
	// the old runtime. Treat it as potentially active so an upgrade cannot turn
	// ambiguous persisted state into full enforcement.
	if !decoded {
		return true
	}
	for key, raw := range object {
		if !strings.EqualFold(strings.TrimSpace(key), "enabled") {
			continue
		}
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			return true
		}
		return value
	}
	return false
}

func advancedConfigObjectString(object map[string]json.RawMessage, wanted string) string {
	for key, raw := range object {
		if !strings.EqualFold(strings.TrimSpace(key), wanted) {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
		return ""
	}
	return ""
}

func setAdvancedConfigObjectString(object map[string]json.RawMessage, wanted string, value string) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	for key := range object {
		if strings.EqualFold(strings.TrimSpace(key), wanted) {
			object[key] = encoded
			return
		}
	}
	object[wanted] = encoded
}

func removeAdvancedConfigSensitiveFields(root map[string]json.RawMessage) {
	keys := make([]string, 0, 1)
	for key := range root {
		if strings.EqualFold(strings.TrimSpace(key), "newapi") {
			keys = append(keys, key)
		}
	}
	// Merge case variants into the canonical lower-case object, with an exact
	// lower-case key taking precedence when malformed historical documents
	// contain both forms.
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i] == "newapi" {
			return false
		}
		if keys[j] == "newapi" {
			return true
		}
		return keys[i] < keys[j]
	})
	newAPI := map[string]json.RawMessage{}
	foundObject := false
	for _, rootKey := range keys {
		raw := root[rootKey]
		delete(root, rootKey)
		object, ok := decodeAdvancedConfigObject(raw)
		if !ok {
			continue
		}
		foundObject = true
		for key := range object {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "secret" || normalized == "enabled" || normalized == "offense_window_seconds" || normalized == "ban_after" {
				delete(object, key)
			}
		}
		for key, value := range object {
			newAPI[key] = value
		}
	}
	if !foundObject {
		return
	}
	// Global enablement, secrets, and penalty counters were retired in favor of
	// mandatory one-to-one identity bindings plus NewAPI-owned enforcement.
	// Strip the legacy fields so they cannot reactivate or misrepresent runtime
	// behavior after an upgrade.
	encoded, err := json.Marshal(newAPI)
	if err == nil {
		root["newapi"] = encoded
	}
}

func mergeAdvancedConfigObjects(base, patch map[string]json.RawMessage, path string) error {
	for key, patchValue := range patch {
		if isJSONNull(patchValue) {
			delete(base, key)
			continue
		}
		currentPath := key
		if path != "" {
			currentPath = path + "." + key
		}
		// provider_profiles is a user-managed map, not a configuration struct.
		// Replacing it as a unit preserves the existing add/remove semantics.
		if currentPath == "guard.provider_profiles" {
			base[key] = cloneRawMessage(patchValue)
			continue
		}
		patchObject, patchIsObject := decodeAdvancedConfigObject(patchValue)
		baseObject, baseIsObject := decodeAdvancedConfigObject(base[key])
		if patchIsObject && baseIsObject {
			if err := mergeAdvancedConfigObjects(baseObject, patchObject, currentPath); err != nil {
				return err
			}
			merged, err := json.Marshal(baseObject)
			if err != nil {
				return err
			}
			base[key] = merged
			continue
		}
		base[key] = cloneRawMessage(patchValue)
	}
	return nil
}

func decodeAdvancedConfigObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func MarshalAdvancedConfig(cfg AdvancedConfig) string {
	b, err := json.Marshal(NormalizeAdvancedConfig(cfg))
	if err != nil {
		return "{}"
	}
	return string(b)
}

func NormalizeAdvancedConfig(cfg AdvancedConfig) AdvancedConfig {
	d := DefaultAdvancedConfig()
	cfg.Guard = NormalizeGuardConfig(cfg.Guard)
	seenCategories := map[string]bool{}
	categories := make([]string, 0, len(cfg.Enforcement.TerminalCategories))
	for _, category := range cfg.Enforcement.TerminalCategories {
		category = strings.ToLower(strings.TrimSpace(category))
		if category != "" && !seenCategories[category] {
			seenCategories[category] = true
			categories = append(categories, category)
		}
	}
	cfg.Enforcement.TerminalCategories = categories
	bypassModels := cfg.Enforcement.TerminalBypassModels
	if bypassModels == nil {
		bypassModels = d.Enforcement.TerminalBypassModels
	}
	seenBypassModels := map[string]bool{}
	normalizedBypassModels := make([]string, 0, len(bypassModels))
	for _, model := range bypassModels {
		model = strings.ToLower(strings.TrimSpace(model))
		if model != "" && !seenBypassModels[model] {
			seenBypassModels[model] = true
			normalizedBypassModels = append(normalizedBypassModels, model)
		}
	}
	cfg.Enforcement.TerminalBypassModels = normalizedBypassModels
	localBlockMessage := []rune(strings.TrimSpace(cfg.Enforcement.LocalBlockMessage))
	if len(localBlockMessage) > MaxLocalBlockMessageRunes {
		localBlockMessage = localBlockMessage[:MaxLocalBlockMessageRunes]
	}
	cfg.Enforcement.LocalBlockMessage = strings.TrimSpace(string(localBlockMessage))
	if cfg.Enforcement.ConversationLockTTLHours <= 0 {
		cfg.Enforcement.ConversationLockTTLHours = d.Enforcement.ConversationLockTTLHours
	}
	if cfg.Enforcement.ConversationLockTTLHours > 30*24 {
		cfg.Enforcement.ConversationLockTTLHours = 30 * 24
	}
	if cfg.Enforcement.UserCyberCooldownMinutes < MinUserCyberCooldownMinutes {
		cfg.Enforcement.UserCyberCooldownMinutes = d.Enforcement.UserCyberCooldownMinutes
	}
	if cfg.Enforcement.UserCyberCooldownMinutes > MaxUserCyberCooldownMinutes {
		cfg.Enforcement.UserCyberCooldownMinutes = MaxUserCyberCooldownMinutes
	}
	if cfg.Normalization.MaxDecodeRuns <= 0 {
		cfg.Normalization.MaxDecodeRuns = d.Normalization.MaxDecodeRuns
	}
	if cfg.Normalization.MaxDecodeRuns > 2 {
		cfg.Normalization.MaxDecodeRuns = 2
	}
	if cfg.Normalization.MaxDecodedBytes <= 0 {
		cfg.Normalization.MaxDecodedBytes = d.Normalization.MaxDecodedBytes
	}
	if cfg.Normalization.MaxDecodedBytes > 65536 {
		cfg.Normalization.MaxDecodedBytes = 65536
	}
	if cfg.Normalization.MaxEncodedBlocks <= 0 {
		cfg.Normalization.MaxEncodedBlocks = d.Normalization.MaxEncodedBlocks
	}
	if cfg.Normalization.MaxEncodedBlocks > 32 {
		cfg.Normalization.MaxEncodedBlocks = 32
	}
	if cfg.ContextDiscount.MaxDiscount < 0 {
		cfg.ContextDiscount.MaxDiscount = 0
	}
	if cfg.ContextDiscount.MaxDiscount > 90 {
		cfg.ContextDiscount.MaxDiscount = 90
	}
	if cfg.ContextDiscount.OperationalMaxDiscount < 0 {
		cfg.ContextDiscount.OperationalMaxDiscount = 0
	}
	if cfg.ContextDiscount.OperationalMaxDiscount > cfg.ContextDiscount.MaxDiscount {
		cfg.ContextDiscount.OperationalMaxDiscount = cfg.ContextDiscount.MaxDiscount
	}
	if cfg.Risk.WindowSeconds <= 0 {
		cfg.Risk.WindowSeconds = d.Risk.WindowSeconds
	}
	if cfg.Risk.WindowSeconds > 86400 {
		cfg.Risk.WindowSeconds = 86400
	}
	if cfg.Risk.BlockThreshold <= 0 {
		cfg.Risk.BlockThreshold = d.Risk.BlockThreshold
	}
	if cfg.Risk.ReviewThreshold <= 0 {
		cfg.Risk.ReviewThreshold = d.Risk.ReviewThreshold
	}
	if cfg.Risk.SessionCreationLimit <= 0 {
		cfg.Risk.SessionCreationLimit = d.Risk.SessionCreationLimit
	}
	if cfg.Risk.SessionCreationLimit > 100000 {
		cfg.Risk.SessionCreationLimit = 100000
	}
	if cfg.Risk.SessionCreationLimitWindowSeconds <= 0 {
		cfg.Risk.SessionCreationLimitWindowSeconds = d.Risk.SessionCreationLimitWindowSeconds
	}
	if cfg.Risk.SessionCreationLimitWindowSeconds < 60 {
		cfg.Risk.SessionCreationLimitWindowSeconds = 60
	}
	if cfg.Risk.SessionCreationLimitWindowSeconds > 30*24*60*60 {
		cfg.Risk.SessionCreationLimitWindowSeconds = 30 * 24 * 60 * 60
	}
	if cfg.AdaptiveReview.MinCleanReviews <= 0 {
		cfg.AdaptiveReview.MinCleanReviews = d.AdaptiveReview.MinCleanReviews
	}
	if cfg.AdaptiveReview.MinCleanReviews > 1000 {
		cfg.AdaptiveReview.MinCleanReviews = 1000
	}
	if cfg.AdaptiveReview.MinObservationHours <= 0 {
		cfg.AdaptiveReview.MinObservationHours = d.AdaptiveReview.MinObservationHours
	}
	if cfg.AdaptiveReview.MinObservationHours > 30*24 {
		cfg.AdaptiveReview.MinObservationHours = 30 * 24
	}
	if cfg.AdaptiveReview.SamplePercent < 0 {
		cfg.AdaptiveReview.SamplePercent = 0
	}
	if cfg.AdaptiveReview.SamplePercent > 100 {
		cfg.AdaptiveReview.SamplePercent = 100
	}
	if cfg.AdaptiveReview.ForceReviewIntervalMinutes <= 0 {
		cfg.AdaptiveReview.ForceReviewIntervalMinutes = d.AdaptiveReview.ForceReviewIntervalMinutes
	}
	if cfg.AdaptiveReview.ForceReviewIntervalMinutes > 7*24*60 {
		cfg.AdaptiveReview.ForceReviewIntervalMinutes = 7 * 24 * 60
	}
	if cfg.AdaptiveReview.TrustDurationHours <= 0 {
		cfg.AdaptiveReview.TrustDurationHours = d.AdaptiveReview.TrustDurationHours
	}
	if cfg.AdaptiveReview.TrustDurationHours > 30*24 {
		cfg.AdaptiveReview.TrustDurationHours = 30 * 24
	}
	if cfg.AdaptiveReview.ReactivationCleanReviews <= 0 {
		cfg.AdaptiveReview.ReactivationCleanReviews = d.AdaptiveReview.ReactivationCleanReviews
	}
	if cfg.AdaptiveReview.ReactivationCleanReviews > cfg.AdaptiveReview.MinCleanReviews {
		cfg.AdaptiveReview.ReactivationCleanReviews = cfg.AdaptiveReview.MinCleanReviews
	}
	if cfg.AdaptiveReview.ReactivationCooldownHours <= 0 {
		cfg.AdaptiveReview.ReactivationCooldownHours = d.AdaptiveReview.ReactivationCooldownHours
	}
	if cfg.AdaptiveReview.ReactivationCooldownHours > 30*24 {
		cfg.AdaptiveReview.ReactivationCooldownHours = 30 * 24
	}
	if cfg.Sidecar.TimeoutSeconds <= 0 {
		cfg.Sidecar.TimeoutSeconds = d.Sidecar.TimeoutSeconds
	}
	if cfg.Sidecar.TimeoutSeconds > 30 {
		cfg.Sidecar.TimeoutSeconds = 30
	}
	if cfg.Sidecar.MinScore < 0 {
		cfg.Sidecar.MinScore = 0
	}
	if cfg.Sidecar.MinScore > 100 {
		cfg.Sidecar.MinScore = 100
	}
	if cfg.Sidecar.SamplePercent < 0 {
		cfg.Sidecar.SamplePercent = 0
	}
	if cfg.Sidecar.SamplePercent > 100 {
		cfg.Sidecar.SamplePercent = 100
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Sidecar.Mode)) {
	case GuardModeShadow, GuardModeWarn, GuardModeEnforce:
		cfg.Sidecar.Mode = strings.ToLower(strings.TrimSpace(cfg.Sidecar.Mode))
	default:
		cfg.Sidecar.Mode = d.Sidecar.Mode
	}
	if cfg.Sidecar.MaxTextLength <= 0 {
		cfg.Sidecar.MaxTextLength = d.Sidecar.MaxTextLength
	}
	if cfg.Sidecar.MaxTextLength > 65536 {
		cfg.Sidecar.MaxTextLength = 65536
	}
	if cfg.Sidecar.CacheTTLSeconds < 0 {
		cfg.Sidecar.CacheTTLSeconds = 0
	}
	if cfg.Sidecar.CacheTTLSeconds > 86400 {
		cfg.Sidecar.CacheTTLSeconds = 86400
	}
	if cfg.Sidecar.MaxConcurrent <= 0 {
		cfg.Sidecar.MaxConcurrent = d.Sidecar.MaxConcurrent
	}
	if cfg.Sidecar.MaxConcurrent > 128 {
		cfg.Sidecar.MaxConcurrent = 128
	}
	if cfg.Sidecar.CircuitBreakerFailures <= 0 {
		cfg.Sidecar.CircuitBreakerFailures = d.Sidecar.CircuitBreakerFailures
	}
	if cfg.Sidecar.CircuitBreakerFailures > 20 {
		cfg.Sidecar.CircuitBreakerFailures = 20
	}
	if cfg.Sidecar.CircuitBreakerSeconds <= 0 {
		cfg.Sidecar.CircuitBreakerSeconds = d.Sidecar.CircuitBreakerSeconds
	}
	if cfg.Sidecar.CircuitBreakerSeconds > 3600 {
		cfg.Sidecar.CircuitBreakerSeconds = 3600
	}
	if cfg.Session.WindowSeconds <= 0 {
		cfg.Session.WindowSeconds = d.Session.WindowSeconds
	}
	if cfg.Session.WindowSeconds > 3600 {
		cfg.Session.WindowSeconds = 3600
	}
	if cfg.Session.MaxFragments <= 0 {
		cfg.Session.MaxFragments = d.Session.MaxFragments
	}
	if cfg.Session.MaxFragments > 10 {
		cfg.Session.MaxFragments = 10
	}
	if cfg.Session.MaxTextLength <= 0 {
		cfg.Session.MaxTextLength = d.Session.MaxTextLength
	}
	if cfg.Session.MaxTextLength > 16384 {
		cfg.Session.MaxTextLength = 16384
	}
	if cfg.Session.ShortFragmentMaxChars <= 0 {
		cfg.Session.ShortFragmentMaxChars = d.Session.ShortFragmentMaxChars
	}
	if cfg.Session.ShortFragmentMaxChars > 256 {
		cfg.Session.ShortFragmentMaxChars = 256
	}
	if cfg.Attachment.TimeoutSeconds <= 0 {
		cfg.Attachment.TimeoutSeconds = d.Attachment.TimeoutSeconds
	}
	if cfg.Attachment.TimeoutSeconds > 30 {
		cfg.Attachment.TimeoutSeconds = 30
	}
	if cfg.Attachment.MaxFiles <= 0 {
		cfg.Attachment.MaxFiles = d.Attachment.MaxFiles
	}
	if cfg.Attachment.MaxFiles > 16 {
		cfg.Attachment.MaxFiles = 16
	}
	if cfg.Attachment.MaxBytes < 1024 {
		cfg.Attachment.MaxBytes = d.Attachment.MaxBytes
	}
	if cfg.Attachment.MaxBytes > 1048576 {
		cfg.Attachment.MaxBytes = 1048576
	}
	if cfg.Attachment.MaxExtractedChars <= 0 {
		cfg.Attachment.MaxExtractedChars = d.Attachment.MaxExtractedChars
	}
	if cfg.Attachment.MaxExtractedChars > 65536 {
		cfg.Attachment.MaxExtractedChars = 65536
	}
	if cfg.Attachment.CacheTTLSeconds < 0 {
		cfg.Attachment.CacheTTLSeconds = 0
	}
	if cfg.Attachment.CacheTTLSeconds > 86400 {
		cfg.Attachment.CacheTTLSeconds = 86400
	}
	if cfg.Attachment.MaxConcurrent <= 0 {
		cfg.Attachment.MaxConcurrent = d.Attachment.MaxConcurrent
	}
	if cfg.Attachment.MaxConcurrent > 64 {
		cfg.Attachment.MaxConcurrent = 64
	}
	if cfg.Attachment.CircuitBreakerFailures <= 0 {
		cfg.Attachment.CircuitBreakerFailures = d.Attachment.CircuitBreakerFailures
	}
	if cfg.Attachment.CircuitBreakerFailures > 20 {
		cfg.Attachment.CircuitBreakerFailures = 20
	}
	if cfg.Attachment.CircuitBreakerSeconds <= 0 {
		cfg.Attachment.CircuitBreakerSeconds = d.Attachment.CircuitBreakerSeconds
	}
	if cfg.Attachment.CircuitBreakerSeconds > 3600 {
		cfg.Attachment.CircuitBreakerSeconds = 3600
	}
	if cfg.Output.BufferBytes < 512 {
		cfg.Output.BufferBytes = d.Output.BufferBytes
	}
	if cfg.Output.BufferBytes > 65536 {
		cfg.Output.BufferBytes = 65536
	}
	if cfg.Output.OverlapBytes < 64 {
		cfg.Output.OverlapBytes = d.Output.OverlapBytes
	}
	if cfg.Output.OverlapBytes >= cfg.Output.BufferBytes {
		cfg.Output.OverlapBytes = cfg.Output.BufferBytes / 4
	}
	if cfg.Intelligence.IntervalHours < 1 {
		cfg.Intelligence.IntervalHours = d.Intelligence.IntervalHours
	}
	if cfg.Intelligence.IntervalHours > 720 {
		cfg.Intelligence.IntervalHours = 720
	}
	if cfg.Intelligence.MaxSearchResults < 1 {
		cfg.Intelligence.MaxSearchResults = d.Intelligence.MaxSearchResults
	}
	if cfg.Intelligence.MaxSearchResults > 100 {
		cfg.Intelligence.MaxSearchResults = 100
	}
	if strings.TrimSpace(cfg.Intelligence.Model) == "" {
		cfg.Intelligence.Model = d.Intelligence.Model
	}
	if cfg.Intelligence.MaxModelCalls < 0 {
		cfg.Intelligence.MaxModelCalls = 0
	}
	if cfg.Intelligence.MaxModelCalls > 3 {
		cfg.Intelligence.MaxModelCalls = 3
	}
	if cfg.NewAPI.MaxClockSkewSeconds < 30 {
		cfg.NewAPI.MaxClockSkewSeconds = d.NewAPI.MaxClockSkewSeconds
	}
	if cfg.NewAPI.MaxClockSkewSeconds > 600 {
		cfg.NewAPI.MaxClockSkewSeconds = 600
	}
	queries := make([]string, 0, len(cfg.Intelligence.Queries))
	for _, query := range cfg.Intelligence.Queries {
		query = strings.TrimSpace(query)
		if query != "" && len(queries) < 10 {
			queries = append(queries, query)
		}
	}
	cfg.Intelligence.Queries = queries
	return cfg
}

func NormalizeGuardConfig(cfg GuardConfig) GuardConfig {
	d := DefaultGuardConfig()
	cfg.Mode = normalizeGuardMode(cfg.Mode, d.Mode)
	cfg.DefaultProfile = normalizeGuardProfileName(cfg.DefaultProfile, d.DefaultProfile)
	if cfg.Performance.MaxSegments < MinGuardMaxSegments {
		cfg.Performance.MaxSegments = d.Performance.MaxSegments
	} else if cfg.Performance.MaxSegments > MaxGuardMaxSegments {
		cfg.Performance.MaxSegments = MaxGuardMaxSegments
	}
	if cfg.Performance.MaxCurrentUserBytes <= 0 {
		cfg.Performance.MaxCurrentUserBytes = d.Performance.MaxCurrentUserBytes
	} else if cfg.Performance.MaxCurrentUserBytes < MinGuardCurrentUserBytes {
		cfg.Performance.MaxCurrentUserBytes = MinGuardCurrentUserBytes
	} else if cfg.Performance.MaxCurrentUserBytes > MaxGuardCurrentUserBytes {
		cfg.Performance.MaxCurrentUserBytes = MaxGuardCurrentUserBytes
	}
	if cfg.Performance.MaxAuxiliaryBytes < MinGuardAuxiliaryBytes {
		cfg.Performance.MaxAuxiliaryBytes = d.Performance.MaxAuxiliaryBytes
	} else if cfg.Performance.MaxAuxiliaryBytes > MaxGuardAuxiliaryBytes {
		cfg.Performance.MaxAuxiliaryBytes = MaxGuardAuxiliaryBytes
	}
	if cfg.Performance.ScanChunkBytes <= 0 {
		cfg.Performance.ScanChunkBytes = d.Performance.ScanChunkBytes
	} else if cfg.Performance.ScanChunkBytes < MinGuardScanChunkBytes {
		cfg.Performance.ScanChunkBytes = MinGuardScanChunkBytes
	} else if cfg.Performance.ScanChunkBytes > MaxGuardScanChunkBytes {
		cfg.Performance.ScanChunkBytes = MaxGuardScanChunkBytes
	}
	if cfg.Performance.ScanChunkBytes > cfg.Performance.MaxCurrentUserBytes {
		cfg.Performance.ScanChunkBytes = cfg.Performance.MaxCurrentUserBytes
	}
	if cfg.Performance.ScanOverlapBytes <= 0 {
		cfg.Performance.ScanOverlapBytes = d.Performance.ScanOverlapBytes
	} else if cfg.Performance.ScanOverlapBytes < MinGuardScanOverlapBytes {
		cfg.Performance.ScanOverlapBytes = MinGuardScanOverlapBytes
	} else if cfg.Performance.ScanOverlapBytes > MaxGuardScanOverlapBytes {
		cfg.Performance.ScanOverlapBytes = MaxGuardScanOverlapBytes
	}
	if cfg.Performance.ScanOverlapBytes >= cfg.Performance.ScanChunkBytes {
		cfg.Performance.ScanOverlapBytes = cfg.Performance.ScanChunkBytes / 16
		if cfg.Performance.ScanOverlapBytes < MinGuardScanOverlapBytes {
			cfg.Performance.ScanOverlapBytes = MinGuardScanOverlapBytes
		}
	}
	if cfg.Performance.ExactSegmentCacheEntries <= 0 {
		cfg.Performance.ExactSegmentCacheEntries = d.Performance.ExactSegmentCacheEntries
	} else if cfg.Performance.ExactSegmentCacheEntries > 32768 {
		cfg.Performance.ExactSegmentCacheEntries = 32768
	}
	if cfg.Performance.ExactSegmentCacheTTLSeconds <= 0 {
		cfg.Performance.ExactSegmentCacheTTLSeconds = d.Performance.ExactSegmentCacheTTLSeconds
	} else if cfg.Performance.ExactSegmentCacheTTLSeconds > 86400 {
		cfg.Performance.ExactSegmentCacheTTLSeconds = 86400
	}
	if cfg.Performance.ShadowWorkers <= 0 {
		cfg.Performance.ShadowWorkers = d.Performance.ShadowWorkers
	} else if cfg.Performance.ShadowWorkers > 16 {
		cfg.Performance.ShadowWorkers = 16
	}
	if cfg.Performance.ShadowQueueSize <= 0 {
		cfg.Performance.ShadowQueueSize = d.Performance.ShadowQueueSize
	} else if cfg.Performance.ShadowQueueSize > 4096 {
		cfg.Performance.ShadowQueueSize = 4096
	}
	// Synchronous overflow fallback is intentionally retired: auxiliary Shadow
	// work must never be pushed back onto the latency-sensitive request path.
	// Normalize legacy "sync" values (and any unknown values) to "drop" so old
	// persisted configurations migrate safely on their next read/write cycle.
	cfg.Performance.ShadowOverflowMode = GuardShadowOverflowDrop

	profiles := make(map[string]string, len(d.ProviderProfiles)+len(cfg.ProviderProfiles))
	for provider, profile := range d.ProviderProfiles {
		profiles[provider] = profile
	}
	for provider, profile := range cfg.ProviderProfiles {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			continue
		}
		profiles[provider] = normalizeGuardProfileName(profile, cfg.DefaultProfile)
	}
	cfg.ProviderProfiles = profiles

	cfg.Layers.CurrentUser.Mode = normalizeGuardMode(cfg.Layers.CurrentUser.Mode, d.Layers.CurrentUser.Mode)
	cfg.Layers.History.Mode = normalizeAuxiliaryGuardMode(cfg.Layers.History.Mode, d.Layers.History.Mode)
	cfg.Layers.System.Mode = normalizeAuxiliaryGuardMode(cfg.Layers.System.Mode, d.Layers.System.Mode)
	cfg.Layers.Developer.Mode = normalizeAuxiliaryGuardMode(cfg.Layers.Developer.Mode, d.Layers.Developer.Mode)
	cfg.Layers.Instructions.Mode = normalizeAuxiliaryGuardMode(cfg.Layers.Instructions.Mode, d.Layers.Instructions.Mode)
	cfg.Layers.ToolOutput.Mode = normalizeAuxiliaryGuardMode(cfg.Layers.ToolOutput.Mode, d.Layers.ToolOutput.Mode)
	cfg.Layers.ToolArguments.Mode = normalizeAuxiliaryGuardMode(cfg.Layers.ToolArguments.Mode, d.Layers.ToolArguments.Mode)
	cfg.Layers.AttachmentRefs.Mode = normalizeAuxiliaryGuardMode(cfg.Layers.AttachmentRefs.Mode, d.Layers.AttachmentRefs.Mode)
	cfg.Layers.SessionContext.Mode = normalizeAuxiliaryGuardMode(cfg.Layers.SessionContext.Mode, d.Layers.SessionContext.Mode)
	cfg.Layers.AttachmentContent.Mode = normalizeAuxiliaryGuardMode(cfg.Layers.AttachmentContent.Mode, d.Layers.AttachmentContent.Mode)
	return cfg
}

func normalizeAuxiliaryGuardMode(mode string, fallback string) string {
	mode = normalizeGuardMode(mode, fallback)
	if mode == GuardModeEnforce {
		return GuardModeShadow
	}
	return mode
}

func normalizeGuardMode(mode string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case GuardModeInherit:
		return GuardModeInherit
	case GuardModeOff:
		return GuardModeOff
	case GuardModeShadow:
		return GuardModeShadow
	case GuardModeWarn:
		return GuardModeWarn
	case GuardModeEnforce:
		return GuardModeEnforce
	default:
		if fallback == "" {
			return GuardModeInherit
		}
		return fallback
	}
}

func normalizeGuardProfileName(name string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case GuardProfileBalanced:
		return GuardProfileBalanced
	case GuardProfileStrict:
		return GuardProfileStrict
	case GuardProfileResearch:
		return GuardProfileResearch
	default:
		if fallback == "" {
			return GuardProfileBalanced
		}
		return fallback
	}
}

type scanView struct {
	Text                    string
	ReviewOnly              bool
	Compacted               bool
	NormalizationIncomplete bool
}

func scanViews(text string, cfg NormalizationConfig, currentEngines ...*Engine) []scanView {
	return scanViewsWithBudget(text, cfg, 0, currentEngines...)
}

func scanViewsWithBudget(text string, cfg NormalizationConfig, maxNormalizedBytes int, currentEngines ...*Engine) []scanView {
	performance := GuardPerformanceConfig{
		ScanChunkBytes:   DefaultGuardScanChunkBytes,
		ScanOverlapBytes: DefaultGuardScanOverlapBytes,
	}
	if len(currentEngines) > 0 && currentEngines[0] != nil {
		performance = currentEngines[0].cfg.Advanced.Guard.Performance
	}
	return scanViewsWithRuntimeBudget(text, cfg, maxNormalizedBytes, performance, currentEngines...)
}

// scanViewsWithRuntimeBudget keeps operational scan-window tuning outside the
// compiled Engine identity. Callers on a hot configuration path must pass the
// current normalized performance config explicitly so an Engine cached under
// otherwise-identical detection rules cannot retain stale chunk/overlap values.
func scanViewsWithRuntimeBudget(text string, cfg NormalizationConfig, maxNormalizedBytes int, performance GuardPerformanceConfig, currentEngines ...*Engine) []scanView {
	if maxNormalizedBytes > 0 {
		text = limitScanTextExact(text, maxNormalizedBytes)
	}
	base := normalizeForScan(text)
	if !cfg.Enabled {
		return []scanView{{Text: base}}
	}

	const (
		maxNormalizationViews   = 64
		maxNormalizationSources = 32
		maxDerivedViewBytesCap  = 4 * 1024 * 1024
	)
	scanChunkBytes, scanOverlapBytes := guardScanWindow(performance)
	maxEncodedFragmentBytes := len(text)
	if maxEncodedFragmentBytes < 16*1024 {
		maxEncodedFragmentBytes = 16 * 1024
	}
	if maxEncodedFragmentBytes > 1024*1024 {
		maxEncodedFragmentBytes = 1024 * 1024
	}
	maxDerivedViewBytes := DefaultMaxTextLength * 4
	if sourceBound := len(text) * 4; sourceBound > maxDerivedViewBytes {
		maxDerivedViewBytes = sourceBound
	}
	if maxDerivedViewBytes > maxDerivedViewBytesCap {
		maxDerivedViewBytes = maxDerivedViewBytesCap
	}
	if maxNormalizedBytes > 0 && maxDerivedViewBytes > maxNormalizedBytes {
		maxDerivedViewBytes = maxNormalizedBytes
	}
	if cfg.DecodeBase64 || cfg.DecodeHex {
		base = normalizeForScan(collapseRecognizedEncodedPayloads(text, cfg, maxEncodedFragmentBytes, nil))
	}

	views := []scanView{{Text: base}}
	viewIndex := map[string]int{base: 0}
	derivedRemaining := maxDerivedViewBytes
	primaryAllowance := maxDerivedViewBytes / 2
	addOne := func(value string, reviewOnly, compacted bool, allowance *int) {
		value = normalizeForScan(value)
		if value == "" || len(views) >= maxNormalizationViews {
			return
		}
		if index, exists := viewIndex[value]; exists {
			// An executable occurrence must win when the same normalized text is
			// also seen inside a quoted, explicitly non-executing review sample.
			if views[index].ReviewOnly && !reviewOnly {
				views[index].ReviewOnly = false
			}
			views[index].Compacted = views[index].Compacted || compacted
			return
		}
		available := derivedRemaining
		if allowance != nil && *allowance < available {
			available = *allowance
		}
		if available <= 0 {
			return
		}
		value = safeUTF8Prefix(value, available)
		if value == "" {
			return
		}
		derivedRemaining -= len(value)
		if allowance != nil {
			*allowance -= len(value)
		}
		viewIndex[value] = len(views)
		views = append(views, scanView{Text: value, ReviewOnly: reviewOnly, Compacted: compacted})
	}
	add := func(value string, reviewOnly bool, primary bool) {
		sourceCanonical := boundedNFKC(value, maxDerivedViewBytes)
		canonical := stripInvisible(sourceCanonical)
		var allowance *int
		if primary {
			allowance = &primaryAllowance
		}
		addOne(canonical, reviewOnly, false, allowance)
		// Compact matching scans the cleaned value, while its enablement keeps
		// evidence from the pre-strip source (for example zero-width separators).
		if minorSafetyShouldInspectCompact(sourceCanonical) {
			addOne(compactForScan(canonical), reviewOnly, true, allowance)
		}
	}

	maxDecodedBytes := cfg.MaxDecodedBytes
	if maxDecodedBytes <= 0 || maxDecodedBytes > 65536 {
		maxDecodedBytes = 32768
	}
	if maxNormalizedBytes > 0 && maxDecodedBytes > maxNormalizedBytes {
		maxDecodedBytes = maxNormalizedBytes
	}
	maxEncodedBlocks := cfg.MaxEncodedBlocks
	if maxEncodedBlocks <= 0 || maxEncodedBlocks > 32 {
		maxEncodedBlocks = 16
	}
	budget := decodeBudget{remainingBytes: maxDecodedBytes, maxBlocks: maxEncodedBlocks}
	sourceNormalized := boundedNFKC(text, maxDerivedViewBytes)
	normalized := stripInvisible(sourceNormalized)
	baseSource := sourceNormalized
	if cfg.DecodeBase64 || cfg.DecodeHex {
		baseSource = collapseRecognizedEncodedPayloads(sourceNormalized, cfg, maxEncodedFragmentBytes, nil)
	}
	add(baseSource, false, true)
	sourceKey := func(value string, reviewOnly bool) string {
		if reviewOnly {
			return value + "\x00review"
		}
		return value + "\x00active"
	}
	sourceSet := map[string]struct{}{sourceKey(normalized, false): {}}
	frontier := []scanView{{Text: normalized}}
	activeNormalizationIncomplete := false

	for run := 0; run < cfg.MaxDecodeRuns && len(frontier) > 0 && budget.remainingBytes > 0; run++ {
		next := make([]scanView, 0, len(frontier))
		enqueue := func(value string, encodedBlock bool, reviewOnly bool) bool {
			if value == "" || derivedRemaining <= 0 || len(sourceSet) >= maxNormalizationSources {
				return false
			}
			value = safeUTF8Prefix(value, min(maxDerivedViewBytes, derivedRemaining))
			if value == "" {
				return false
			}
			key := sourceKey(value, reviewOnly)
			if _, exists := sourceSet[key]; exists {
				return false
			}
			if !budget.accept(len(value), encodedBlock) {
				return false
			}
			sourceSet[key] = struct{}{}
			add(value, reviewOnly, false)
			next = append(next, scanView{Text: value, ReviewOnly: reviewOnly})
			return true
		}
		enqueueAcceptedDecoded := func(values []string, reviewOnly bool) {
			if len(values) == 0 {
				return
			}
			value := values[0]
			if len(values) > 1 {
				value = strings.Join(values, " ")
			}
			if value == "" || len(value) > maxDerivedViewBytes {
				return
			}
			value = safeUTF8Prefix(value, min(maxDerivedViewBytes, derivedRemaining))
			if value == "" {
				return
			}
			key := sourceKey(value, reviewOnly)
			if _, exists := sourceSet[key]; !exists {
				if len(sourceSet) >= maxNormalizationSources {
					return
				}
				sourceSet[key] = struct{}{}
			}
			add(value, reviewOnly, false)
			next = append(next, scanView{Text: value, ReviewOnly: reviewOnly})
		}
		addScanOnly := func(value string, reviewOnly bool) {
			if value == "" || len(value) > maxDerivedViewBytes {
				return
			}
			key := sourceKey(value, reviewOnly)
			if _, exists := sourceSet[key]; !exists {
				if len(sourceSet) >= maxNormalizationSources {
					return
				}
				sourceSet[key] = struct{}{}
			}
			add(value, reviewOnly, false)
		}
		acceptDecodedBlock := func(value string, reviewOnly bool) bool {
			if value == "" || len(value) > maxDerivedViewBytes || len(sourceSet) >= maxNormalizationSources {
				return false
			}
			key := sourceKey(value, reviewOnly)
			if _, exists := sourceSet[key]; exists {
				return false
			}
			if !budget.accept(len(value), true) {
				return false
			}
			sourceSet[key] = struct{}{}
			return true
		}

		for _, source := range frontier {
			if cfg.DecodeURL {
				if decoded, err := url.QueryUnescape(source.Text); err == nil && decoded != source.Text {
					enqueue(decoded, false, source.ReviewOnly)
				}
			}
			if cfg.DecodeHTML {
				if decoded := html.UnescapeString(source.Text); decoded != source.Text {
					enqueue(decoded, false, source.ReviewOnly)
				}
			}
			if cfg.DecodeEscapes {
				if decoded, changed := decodeEscapedText(source.Text); changed {
					enqueue(decoded, false, source.ReviewOnly)
				}
			}
			if cfg.DecodeBase64 || cfg.DecodeHex {
				decodedBatch := decodeEmbeddedBlockBatchWithWindow(source.Text, cfg, budget.remainingBytes, budget.maxBlocks-budget.blocks, maxEncodedFragmentBytes, scanChunkBytes, scanOverlapBytes, currentEngines...)
				// A derived review-only source must never turn an opaque or
				// truncated fixture into an active warning. Its incomplete state
				// remains non-enforcing review provenance.
				if !source.ReviewOnly && decodedBatch.activeIncomplete {
					activeNormalizationIncomplete = true
				}
				decodedBlocks := decodedBatch.blocks
				reviewContext := newMinorSafetyScanContext(source.Text)
				activeJoined := make([]string, 0, len(decodedBlocks))
				reviewJoined := make([]string, 0, len(decodedBlocks))
				activeAccepted := make([]decodedBlock, 0, len(decodedBlocks))
				reviewAccepted := make([]decodedBlock, 0, len(decodedBlocks))
				for _, block := range decodedBlocks {
					reviewOnly := source.ReviewOnly || encodedBlockReviewOnlyWithContext(reviewContext, block.start, block.end)
					if acceptDecodedBlock(block.value, reviewOnly) {
						if reviewOnly {
							reviewAccepted = append(reviewAccepted, block)
							reviewJoined = append(reviewJoined, block.value)
						} else {
							activeAccepted = append(activeAccepted, block)
							activeJoined = append(activeJoined, block.value)
						}
					}
				}
				// Scan one aggregate per provenance instead of one Engine view per
				// ordinary block. The decoded byte/block budget was already charged
				// above; aggregation adds no new decoded material. This prevents a
				// large decoy list from multiplying full-source policy scans while
				// retaining every accepted fragment and multi-block matching.
				enqueueAcceptedDecoded(activeJoined, false)
				enqueueAcceptedDecoded(reviewJoined, true)
				// Oversized decoded candidates cannot enter the ordinary decoded-byte
				// budget. Preserve a small, rule-matching evidence window instead of
				// replacing a real match with a generic overflow label. Review-only
				// provenance remains non-enforcing exactly like ordinary decoded views.
				if source.ReviewOnly {
					addScanOnly(strings.TrimSpace(decodedBatch.activeEvidenceText+" "+decodedBatch.reviewEvidenceText), true)
				} else {
					addScanOnly(decodedBatch.activeEvidenceText, false)
					addScanOnly(decodedBatch.reviewEvidenceText, true)
				}
				if len(activeAccepted) > 0 {
					enqueue(collapseRecognizedEncodedPayloads(source.Text, cfg, maxEncodedFragmentBytes, activeAccepted), false, source.ReviewOnly)
				}
				if len(reviewAccepted) > 0 {
					enqueue(collapseRecognizedEncodedPayloads(source.Text, cfg, maxEncodedFragmentBytes, reviewAccepted), false, true)
				}
			}
			// ROT13 is intentionally last: unlike the structured decoders it can
			// transform any ordinary English text, so it must not consume the
			// shared byte budget before Base64/hex/compressed payloads are handled.
			if cfg.DecodeROT13 {
				if decoded, ok := decodeROT13Text(source.Text); ok {
					enqueue(decoded, false, source.ReviewOnly)
				}
			}
		}
		frontier = next
	}
	if activeNormalizationIncomplete && len(views) > 0 {
		// Carry analysis state separately from rule evidence. The Engine may emit
		// a non-terminal review warning, but must never invent a malicious match
		// merely because a bounded decoder did not reach EOF.
		views[0].NormalizationIncomplete = true
	}
	return views
}

func guardScanWindow(performance GuardPerformanceConfig) (chunkBytes, overlapBytes int) {
	chunkBytes = performance.ScanChunkBytes
	if chunkBytes <= 0 {
		chunkBytes = DefaultGuardScanChunkBytes
	}
	overlapBytes = performance.ScanOverlapBytes
	if overlapBytes <= 0 || overlapBytes >= chunkBytes {
		overlapBytes = DefaultGuardScanOverlapBytes
		if overlapBytes >= chunkBytes {
			overlapBytes = chunkBytes / 16
			if overlapBytes < 1 {
				overlapBytes = 1
			}
			if overlapBytes >= chunkBytes {
				overlapBytes = chunkBytes - 1
			}
		}
	}
	return chunkBytes, overlapBytes
}

func boundedNFKC(value string, maxBytes int) string {
	if value == "" || maxBytes <= 0 {
		return ""
	}
	reader := transform.NewReader(strings.NewReader(value), norm.NFKC)
	data, _ := io.ReadAll(io.LimitReader(reader, int64(maxBytes+utf8.UTFMax)))
	return safeUTF8Prefix(string(data), maxBytes)
}

type decodeBudget struct {
	remainingBytes int
	blocks         int
	maxBlocks      int
}

func (b *decodeBudget) accept(size int, encodedBlock bool) bool {
	if size <= 0 || size > b.remainingBytes {
		return false
	}
	if encodedBlock && b.blocks >= b.maxBlocks {
		return false
	}
	b.remainingBytes -= size
	if encodedBlock {
		b.blocks++
	}
	return true
}

type decodedBlock struct {
	start int
	end   int
	value string
}

type encodedCandidate struct {
	start int
	end   int
	value string
	kind  string
}

type decodedCandidate struct {
	decodedBlock
	priority int
}

type decodedCandidateKey struct {
	start int
	end   int
	value string
}

type encodedCandidateKey struct {
	start int
	end   int
	kind  string
}

type compressedSafetyCandidate struct {
	candidate  encodedCandidate
	raw        []byte
	decoded    string
	reviewOnly bool
}

type decodedBlockBatch struct {
	blocks             []decodedBlock
	activeEvidenceText string
	reviewEvidenceText string
	activeIncomplete   bool
	reviewIncomplete   bool
}

func decodeEmbeddedBlocks(text string, cfg NormalizationConfig, remainingBytes, remainingBlocks, maxFragmentBytes int) []decodedBlock {
	return decodeEmbeddedBlockBatch(text, cfg, remainingBytes, remainingBlocks, maxFragmentBytes).blocks
}

func decodeEmbeddedBlockBatch(text string, cfg NormalizationConfig, remainingBytes, remainingBlocks, maxFragmentBytes int, currentEngines ...*Engine) decodedBlockBatch {
	return decodeEmbeddedBlockBatchWithWindow(text, cfg, remainingBytes, remainingBlocks, maxFragmentBytes, DefaultGuardScanChunkBytes, DefaultGuardScanOverlapBytes, currentEngines...)
}

func decodeEmbeddedBlockBatchWithWindow(text string, cfg NormalizationConfig, remainingBytes, remainingBlocks, maxFragmentBytes, scanChunkBytes, scanOverlapBytes int, currentEngines ...*Engine) decodedBlockBatch {
	if remainingBytes <= 0 || remainingBlocks <= 0 {
		return decodedBlockBatch{}
	}
	// The acceptance budget remains authoritative. Ordinary decoding stays on a
	// bounded head/tail pool, while every direct-text candidate receives a cheap
	// safety pre-scan from the current Engine. Compressed candidates are discovered
	// across the complete input and receive fair, bounded streaming scan budgets.
	allCandidates := encodedCandidates(text, cfg, maxFragmentBytes)
	priorityScanner := builtinDecodedSafetyPriorityScanner()
	if len(currentEngines) > 0 && currentEngines[0] != nil {
		priorityScanner = decodedSafetyPriorityScannerForEngine(currentEngines[0])
	}
	// A deliberately tiny administrator budget is an explicit resource policy,
	// not permission for an out-of-budget safety probe. At normal production
	// budgets, direct Base64/hex text remains bounded by the request size and can
	// be inspected without decompression; compressed expansion has a separate cap.
	allowOutOfBudgetSafetyProbe := remainingBytes >= 1024
	var activeEvidence, reviewEvidence strings.Builder
	appendEvidence := func(value string, reviewOnly bool) bool {
		if value == "" {
			return false
		}
		builder := &activeEvidence
		if reviewOnly {
			builder = &reviewEvidence
		}
		if builder.Len()+len(value)+1 > 8*1024 {
			return false
		}
		if builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		builder.WriteString(value)
		return true
	}
	ordinaryCandidates := boundedEncodedCandidates(allCandidates, 64)
	inspected := make([]decodedCandidate, 0, len(ordinaryCandidates)+remainingBlocks*2)
	indexes := make(map[decodedCandidateKey]int, cap(inspected))
	appendCandidate := func(candidate encodedCandidate, value string, priority int) {
		if value == "" || len(value) > remainingBytes {
			return
		}
		key := decodedCandidateKey{start: candidate.start, end: candidate.end, value: value}
		if index, exists := indexes[key]; exists {
			if priority > inspected[index].priority {
				inspected[index].priority = priority
			}
			return
		}
		indexes[key] = len(inspected)
		inspected = append(inspected, decodedCandidate{
			decodedBlock: decodedBlock{start: candidate.start, end: candidate.end, value: value},
			priority:     priority,
		})
	}
	ordinary := make(map[encodedCandidateKey]struct{}, len(ordinaryCandidates))
	for _, candidate := range ordinaryCandidates {
		ordinary[encodedCandidateKey{start: candidate.start, end: candidate.end, kind: candidate.kind}] = struct{}{}
	}
	reviewContext := newMinorSafetyScanContext(text)
	riskPoolLimit := remainingBlocks * 8
	if riskPoolLimit < 128 {
		riskPoolLimit = 128
	}
	if riskPoolLimit > 512 {
		riskPoolLimit = 512
	}
	riskCandidates := make([]decodedCandidate, 0, min(riskPoolLimit, len(allCandidates)))
	riskIndexes := make(map[decodedCandidateKey]int, cap(riskCandidates))
	appendRiskCandidate := func(candidate encodedCandidate, value string, priority int, reviewOnly bool) {
		if priority <= 0 {
			return
		}
		if !reviewOnly {
			// Active high-risk material outranks quoted non-execution samples when
			// both compete for the same final decode budget.
			priority += 1000
		}
		key := decodedCandidateKey{start: candidate.start, end: candidate.end, value: value}
		if index, exists := riskIndexes[key]; exists {
			if priority > riskCandidates[index].priority {
				riskCandidates[index].priority = priority
			}
			return
		}
		entry := decodedCandidate{
			decodedBlock: decodedBlock{start: candidate.start, end: candidate.end, value: value},
			priority:     priority,
		}
		if len(riskCandidates) < riskPoolLimit {
			riskIndexes[key] = len(riskCandidates)
			riskCandidates = append(riskCandidates, entry)
			return
		}
		worst := 0
		for index := 1; index < len(riskCandidates); index++ {
			if riskCandidates[index].priority < riskCandidates[worst].priority ||
				(riskCandidates[index].priority == riskCandidates[worst].priority && riskCandidates[index].start > riskCandidates[worst].start) {
				worst = index
			}
		}
		if entry.priority < riskCandidates[worst].priority ||
			(entry.priority == riskCandidates[worst].priority && entry.start >= riskCandidates[worst].start) {
			return
		}
		delete(riskIndexes, decodedCandidateKey{
			start: riskCandidates[worst].start,
			end:   riskCandidates[worst].end,
			value: riskCandidates[worst].value,
		})
		riskCandidates[worst] = entry
		riskIndexes[key] = worst
	}
	activeIncomplete, reviewIncomplete := false, false
	const maxCompressedSafetyCandidates = 512
	compressedCandidates := make([]compressedSafetyCandidate, 0, min(len(allCandidates), maxCompressedSafetyCandidates))
	for _, candidate := range allCandidates {
		raw, ok := decodeEncodedCandidateRaw(candidate)
		if !ok {
			continue
		}
		reviewOnly := encodedBlockReviewOnlyWithContext(reviewContext, candidate.start, candidate.end)
		var ordinaryDecoded string
		if _, shouldDecode := ordinary[encodedCandidateKey{start: candidate.start, end: candidate.end, kind: candidate.kind}]; shouldDecode {
			if value, decoded := decodedPayloadText(raw, cfg.DecodeCompression, remainingBytes); decoded {
				ordinaryDecoded = value
				appendCandidate(candidate, value, 0)
			}
		}
		// Plain decoded text is cheap to inspect, so every candidate—not only the
		// bounded ordinary pool—gets ranked against the current Engine's built-in and
		// custom decision rules. Oversized matches retain a small real evidence view;
		// an unrepresentable match is marked incomplete instead of being invented.
		if allowOutOfBudgetSafetyProbe {
			if value, decoded := decodedPayloadText(raw, false, len(raw)); decoded {
				priority, evidence := decodedSafetyPriority(value, priorityScanner)
				if priority > 0 && len(value) > remainingBytes {
					if !appendEvidence(evidence, reviewOnly) {
						if reviewOnly {
							reviewIncomplete = true
						} else {
							activeIncomplete = true
						}
					}
				} else if priority > 0 {
					appendRiskCandidate(candidate, value, priority, reviewOnly)
				}
			}
		}
		if cfg.DecodeCompression && compressedPayload(raw) {
			compressed := compressedSafetyCandidate{
				candidate:  candidate,
				raw:        raw,
				decoded:    ordinaryDecoded,
				reviewOnly: reviewOnly,
			}
			if len(compressedCandidates) < maxCompressedSafetyCandidates {
				compressedCandidates = append(compressedCandidates, compressed)
			} else if reviewOnly {
				reviewIncomplete = true
			} else {
				activeIncomplete = true
			}
		}
	}
	// Compression is opaque until expanded. Give every candidate a fair share of
	// one global expansion budget, and scan each share as a bounded stream with
	// overlap. A bomb or a large legitimate archive therefore cannot consume the
	// whole budget before a later candidate is inspected. Exhaustion is recorded
	// as incomplete analysis; it is never converted into fabricated malicious
	// evidence or a terminal decision.
	compressionExpansionBudget := remainingBytes * 16
	if compressionExpansionBudget > 1024*1024 {
		compressionExpansionBudget = 1024 * 1024
	}
	for index, compressed := range compressedCandidates {
		if !allowOutOfBudgetSafetyProbe {
			continue
		}
		if compressionExpansionBudget <= 0 {
			if compressed.reviewOnly {
				reviewIncomplete = true
			} else {
				activeIncomplete = true
			}
			continue
		}
		remainingCandidates := len(compressedCandidates) - index
		candidateBudget := compressionExpansionBudget / remainingCandidates
		if candidateBudget < 1 {
			candidateBudget = 1
		}
		if candidateBudget > 384*1024 {
			candidateBudget = 384 * 1024
		}
		value := compressed.decoded
		if value != "" {
			accounted := min(len(value), candidateBudget)
			compressionExpansionBudget -= accounted
			priority, evidence := decodedSafetyPriority(value, priorityScanner)
			if priority > 0 && len(value) > remainingBytes {
				if !appendEvidence(evidence, compressed.reviewOnly) {
					if compressed.reviewOnly {
						reviewIncomplete = true
					} else {
						activeIncomplete = true
					}
				}
				continue
			}
			appendRiskCandidate(compressed.candidate, value, priority, compressed.reviewOnly)
			continue
		}
		value, expanded, overflow, ok := decompressSmallPayloadAccounted(compressed.raw, candidateBudget)
		compressionExpansionBudget -= min(expanded, candidateBudget)
		if ok && !overflow && len(value) <= remainingBytes {
			priority, _ := decodedSafetyPriority(value, priorityScanner)
			appendRiskCandidate(compressed.candidate, value, priority, compressed.reviewOnly)
			continue
		}
		// A window probe cannot preserve arbitrary ExcludePatterns, defensive
		// context, or cumulative rule semantics across windows. If this candidate
		// cannot be fully decoded inside its bounded share, never replay a
		// highest-scoring slice as complete rule evidence; mark it audit/warn-only.
		if compressed.reviewOnly {
			reviewIncomplete = true
		} else {
			activeIncomplete = true
		}
	}
	for _, candidate := range riskCandidates {
		appendCandidate(encodedCandidate{start: candidate.start, end: candidate.end}, candidate.value, candidate.priority)
	}
	sort.SliceStable(inspected, func(i, j int) bool {
		if inspected[i].priority == inspected[j].priority {
			return inspected[i].start < inspected[j].start
		}
		return inspected[i].priority > inspected[j].priority
	})
	decoded := make([]decodedBlock, 0, min(remainingBlocks, len(inspected)))
	for _, candidate := range inspected {
		if len(decoded) >= remainingBlocks {
			break
		}
		if len(candidate.value) > remainingBytes {
			continue
		}
		decoded = append(decoded, candidate.decodedBlock)
		remainingBytes -= len(candidate.value)
	}
	// Replacement and multi-block reassembly rely on source order, not risk
	// order. Ranking only chooses the bounded accepted set.
	sort.Slice(decoded, func(i, j int) bool { return decoded[i].start < decoded[j].start })
	return decodedBlockBatch{
		blocks:             decoded,
		activeEvidenceText: activeEvidence.String(),
		reviewEvidenceText: reviewEvidence.String(),
		activeIncomplete:   activeIncomplete,
		reviewIncomplete:   reviewIncomplete,
	}
}

func decodeEncodedCandidateRaw(candidate encodedCandidate) ([]byte, bool) {
	if candidate.kind == "hex" {
		raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(candidate.value, "0x"), "0X"))
		return raw, err == nil
	}
	if candidate.kind != "base64" {
		return nil, false
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		raw, err := encoding.DecodeString(candidate.value)
		if err == nil {
			return raw, true
		}
	}
	return nil, false
}

func boundedEncodedCandidates(candidates []encodedCandidate, limit int) []encodedCandidate {
	if limit <= 0 || len(candidates) <= limit {
		return candidates
	}
	head := limit / 2
	tail := limit - head
	bounded := make([]encodedCandidate, 0, limit)
	bounded = append(bounded, candidates[:head]...)
	bounded = append(bounded, candidates[len(candidates)-tail:]...)
	return bounded
}

type decodedSafetyPriorityPattern struct {
	pattern     compiledPattern
	hints       []string
	hintClauses [][]string
}

type decodedSafetyPriorityScanner struct {
	patterns                   []decodedSafetyPriorityPattern
	sensitiveWords             []string
	hintIndex                  *decodedSafetyHintIndex
	decisionThreshold          int
	retainAnyDecisionCandidate bool
}

type decodedSafetyHintIndex struct {
	byFirstByte [256][]string
	unhinted    bool
	automaton   *decodedSafetyHintAutomaton
}

type decodedSafetyHintAutomaton struct {
	nodes []decodedSafetyHintNode
	root  [256]int
}

type decodedSafetyHintNode struct {
	next    map[byte]int
	failure int
	outputs []string
}

var (
	decodedSafetyPriorityOnce    sync.Once
	decodedSafetyPriorityBuiltin decodedSafetyPriorityScanner
)

func builtinDecodedSafetyPriorityScanner() decodedSafetyPriorityScanner {
	decodedSafetyPriorityOnce.Do(func() {
		// Initialize lazily so defaultPatternConfigs has completed package
		// initialization before NewEngine reads it.
		cfg := DefaultConfig()
		cfg.Enabled = true
		cfg.Advanced.Normalization.Enabled = false
		engine, err := NewEngine(cfg)
		if err != nil {
			return
		}
		decodedSafetyPriorityBuiltin = decodedSafetyPriorityScannerForEngine(engine)
	})
	return decodedSafetyPriorityBuiltin
}

func decodedSafetyPriorityScannerForEngine(engine *Engine) decodedSafetyPriorityScanner {
	if engine == nil {
		return decodedSafetyPriorityScanner{}
	}
	return engine.decodedPriorityScanner
}

func buildDecodedSafetyPriorityScanner(compiled []compiledPattern) decodedSafetyPriorityScanner {
	return buildDecodedSafetyScanner(compiled, false, DefaultThreshold, false)
}

func buildDecodedSafetyExactPrecheckScanner(compiled []compiledPattern, threshold int) decodedSafetyPriorityScanner {
	return buildDecodedSafetyScanner(compiled, true, threshold, true)
}

func buildDecodedSafetyScanner(compiled []compiledPattern, includeAll bool, threshold int, retainAnyDecisionCandidate bool) decodedSafetyPriorityScanner {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	patterns := make([]decodedSafetyPriorityPattern, 0, len(compiled))
	for _, pattern := range compiled {
		// Signal-only vocabulary is useful audit evidence but must not crowd
		// enforceable candidates out of the decode budget. Strict rules and
		// standalone decision rules cover every built-in high-risk family.
		if !includeAll && (pattern.cfg.SignalOnly || (!pattern.cfg.Strict && pattern.cfg.Weight < DefaultThreshold)) {
			continue
		}
		clauses := decodedSafetyMandatoryHintClauses(pattern)
		patterns = append(patterns, decodedSafetyPriorityPattern{
			pattern:     pattern,
			hints:       flattenDecodedSafetyHintClauses(clauses),
			hintClauses: clauses,
		})
	}
	return decodedSafetyPriorityScanner{
		patterns:                   patterns,
		decisionThreshold:          threshold,
		retainAnyDecisionCandidate: retainAnyDecisionCandidate,
	}
}

func buildDecodedSafetyHintIndex(scanner decodedSafetyPriorityScanner) *decodedSafetyHintIndex {
	index := &decodedSafetyHintIndex{}
	seen := make(map[string]struct{})
	add := func(hint string) {
		hint = strings.TrimSpace(hint)
		if hint == "" {
			return
		}
		if _, exists := seen[hint]; exists {
			return
		}
		seen[hint] = struct{}{}
		first := hint[0]
		index.byFirstByte[first] = append(index.byFirstByte[first], hint)
	}
	for _, candidatePattern := range scanner.patterns {
		if len(candidatePattern.hints) == 0 {
			index.unhinted = true
			continue
		}
		for _, hint := range candidatePattern.hints {
			add(hint)
		}
	}
	for _, word := range scanner.sensitiveWords {
		add(word)
	}
	index.automaton = newDecodedSafetyHintAutomaton(seen)
	return index
}

func buildGuaranteedPatternHintAutomaton(patterns []compiledPattern) *decodedSafetyHintAutomaton {
	hints := make(map[string]struct{})
	for _, pattern := range patterns {
		for _, clause := range pattern.guaranteedHintClauses {
			for _, hint := range clause {
				if hint = strings.TrimSpace(hint); hint != "" {
					hints[hint] = struct{}{}
				}
			}
		}
	}
	return newDecodedSafetyHintAutomaton(hints)
}

func newDecodedSafetyHintAutomaton(hints map[string]struct{}) *decodedSafetyHintAutomaton {
	if len(hints) == 0 {
		return nil
	}
	automaton := &decodedSafetyHintAutomaton{nodes: []decodedSafetyHintNode{{next: make(map[byte]int)}}}
	for hint := range hints {
		state := 0
		for index := 0; index < len(hint); index++ {
			value := hint[index]
			nextState, exists := automaton.nodes[state].next[value]
			if !exists {
				nextState = len(automaton.nodes)
				automaton.nodes[state].next[value] = nextState
				automaton.nodes = append(automaton.nodes, decodedSafetyHintNode{next: make(map[byte]int)})
			}
			state = nextState
		}
		automaton.nodes[state].outputs = append(automaton.nodes[state].outputs, hint)
	}

	queue := make([]int, 0, len(automaton.nodes))
	for value, child := range automaton.nodes[0].next {
		automaton.root[value] = child
		queue = append(queue, child)
	}
	for head := 0; head < len(queue); head++ {
		state := queue[head]
		for value, child := range automaton.nodes[state].next {
			queue = append(queue, child)
			failure := automaton.nodes[state].failure
			for failure != 0 {
				if target, exists := automaton.nodes[failure].next[value]; exists {
					failure = target
					break
				}
				failure = automaton.nodes[failure].failure
			}
			if failure == 0 {
				if target, exists := automaton.nodes[0].next[value]; exists && target != child {
					failure = target
				}
			}
			automaton.nodes[child].failure = failure
			if inherited := automaton.nodes[failure].outputs; len(inherited) > 0 {
				automaton.nodes[child].outputs = append(automaton.nodes[child].outputs, inherited...)
			}
		}
	}
	return automaton
}

func (a *decodedSafetyHintAutomaton) match(text string) map[string]struct{} {
	if a == nil || len(a.nodes) == 0 || text == "" {
		return nil
	}
	state := 0
	var matched map[string]struct{}
	for index := 0; index < len(text); index++ {
		value := text[index]
		for state != 0 {
			if nextState, exists := a.nodes[state].next[value]; exists {
				state = nextState
				goto collectOutputs
			}
			state = a.nodes[state].failure
		}
		state = a.root[value]
	collectOutputs:
		for _, output := range a.nodes[state].outputs {
			if matched == nil {
				matched = make(map[string]struct{})
			}
			matched[output] = struct{}{}
		}
	}
	return matched
}

// matchNormalizedSource feeds the same lowercase, whitespace-collapsed byte
// stream produced by normalizeForScan directly into the automaton. Clean
// overflow windows therefore avoid materializing strings.Map/Fields/Join
// intermediates; the full normalized string is only built after a hint match
// (or when an unhinted custom rule requires regex evaluation).
func (a *decodedSafetyHintAutomaton) matchNormalizedSource(text string) map[string]struct{} {
	return a.matchNormalizedSourceWithTransform(text, nil)
}

func (a *decodedSafetyHintAutomaton) matchNormalizedROT13Source(text string) map[string]struct{} {
	return a.matchNormalizedSourceWithTransform(text, func(value rune) rune {
		switch {
		case value >= 'a' && value <= 'z':
			return 'a' + (value-'a'+13)%26
		case value >= 'A' && value <= 'Z':
			return 'A' + (value-'A'+13)%26
		default:
			return value
		}
	})
}

func (a *decodedSafetyHintAutomaton) matchNormalizedSourceWithTransform(text string, transformRune func(rune) rune) map[string]struct{} {
	if a == nil || len(a.nodes) == 0 || text == "" {
		return nil
	}
	state := 0
	var matched map[string]struct{}
	emittedField := false
	pendingSpace := false
	emit := func(value byte) {
		for state != 0 {
			if nextState, exists := a.nodes[state].next[value]; exists {
				state = nextState
				goto collectOutputs
			}
			state = a.nodes[state].failure
		}
		state = a.root[value]
	collectOutputs:
		for _, output := range a.nodes[state].outputs {
			if matched == nil {
				matched = make(map[string]struct{})
			}
			matched[output] = struct{}{}
		}
	}
	for offset := 0; offset < len(text); {
		if len(text)-offset >= 3 && text[offset:offset+3] == "```" {
			if emittedField {
				pendingSpace = true
			}
			offset += 3
			continue
		}
		value, size := utf8.DecodeRuneInString(text[offset:])
		if size <= 0 {
			break
		}
		offset += size
		if unicode.IsControl(value) && value != '\n' && value != '\r' && value != '\t' {
			value = ' '
		}
		if transformRune != nil {
			value = transformRune(value)
		}
		value = unicode.ToLower(value)
		if unicode.IsSpace(value) {
			if emittedField {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			emit(' ')
			pendingSpace = false
		}
		emittedField = true
		if value < utf8.RuneSelf {
			emit(byte(value))
			continue
		}
		var encoded [utf8.UTFMax]byte
		encodedBytes := utf8.EncodeRune(encoded[:], value)
		for index := 0; index < encodedBytes; index++ {
			emit(encoded[index])
		}
	}
	return matched
}

func decodedSafetyPriority(value string, scanner decodedSafetyPriorityScanner) (int, string) {
	normalized := normalizeForScan(value)
	return decodedSafetyPriorityNormalized(normalized, scanner)
}

func decodedSafetyPriorityNormalized(normalized string, scanner decodedSafetyPriorityScanner) (int, string) {
	matchedHints, _ := decodedSafetyPriorityMatchedHints(normalized, scanner)
	if matchedHints == nil {
		// A non-nil empty set means the immutable Aho index was evaluated and
		// found no literals. Passing nil would make every pattern fall back to
		// repeated strings.Contains scans over the full candidate.
		matchedHints = emptyDecodedSafetyHintMatches
	}
	return decodedSafetyPriorityNormalizedWithHints(normalized, scanner, matchedHints)
}

var emptyDecodedSafetyHintMatches = map[string]struct{}{}

func decodedSafetyPriorityNormalizedWithHints(normalized string, scanner decodedSafetyPriorityScanner, matchedHints map[string]struct{}) (int, string) {
	priority := 0
	evidence := ""
	decisionScore := 0
	decisionEvidence := make([]string, 0, 4)
	decisionEvidenceComplete := true
	for _, candidatePattern := range scanner.patterns {
		// Signal-only rules are valuable once a bounded view has already been
		// admitted for inspection, but they are not enforcement evidence and must
		// never consume the finite out-of-budget candidate pool ahead of a real
		// decision rule.
		if candidatePattern.pattern.cfg.SignalOnly && !candidatePattern.pattern.cfg.Strict {
			continue
		}
		if len(candidatePattern.hintClauses) > 0 && !containsDecodedSafetyHintClauseWithMatches(normalized, candidatePattern.hintClauses, matchedHints) {
			continue
		}
		pattern := candidatePattern.pattern
		if !patternShouldRun(normalized, pattern, nil) ||
			patternSuppressedForQuotedPolicyReview(normalized, pattern) ||
			patternSuppressedForDefensiveRuleArtifact(normalized, pattern) {
			continue
		}
		loc := compiledPatternMatchIndex(normalized, pattern)
		if loc == nil {
			continue
		}
		patternEvidence := decodedSafetyPatternEvidence(normalized, pattern, loc)
		if pattern.cfg.Strict {
			candidatePriority := 300 + min(pattern.cfg.Weight, 99)
			if candidatePriority > priority {
				priority = candidatePriority
				evidence = patternEvidence
			}
			continue
		}
		decisionScore += pattern.cfg.Weight
		if patternEvidence == "" {
			decisionEvidenceComplete = false
		} else {
			decisionEvidence = append(decisionEvidence, patternEvidence)
		}
	}
	decisionThreshold := scanner.decisionThreshold
	if decisionThreshold <= 0 {
		decisionThreshold = DefaultThreshold
	}
	if decisionScore >= decisionThreshold {
		candidatePriority := 200 + min(decisionScore, 99)
		if candidatePriority > priority {
			priority = candidatePriority
			if decisionEvidenceComplete {
				evidence = strings.Join(decisionEvidence, " ")
			} else {
				evidence = ""
			}
		}
	} else if scanner.retainAnyDecisionCandidate && decisionScore > 0 {
		// The exact CurrentUser precheck evaluates every retained derived view in
		// one shared scoring pass. Preserve individually sub-threshold candidates
		// so rules split across several Base64/hex blocks can still accumulate under
		// the active configuration threshold. This changes candidate admission only;
		// the original weights, strictness, signal-only flags, and final action are
		// applied later by inspectPreparedScanViews.
		candidatePriority := 150 + min(decisionScore, 49)
		if candidatePriority > priority {
			priority = candidatePriority
			if decisionEvidenceComplete {
				evidence = strings.Join(decisionEvidence, " ")
			} else {
				evidence = ""
			}
		}
	}
	// Configured sensitive words are also signal-only evidence. Ordinary bounded
	// decoded views still audit them in the main Engine, but they do not rank an
	// otherwise out-of-budget encoded candidate.
	if minorSafetyShouldInspectCompact(normalized) && minorSafetyCompactMaterialMatchIndex(compactForScan(normalized)) != nil {
		if priority < 399 {
			priority = 399
			// Compact offsets do not map safely back to the original fragmented
			// source. Let the caller mark an oversized candidate incomplete rather
			// than manufacture evidence that the main Engine cannot reproduce.
			evidence = ""
		}
	}
	return priority, evidence
}

func decodedSafetyPriorityMayMatch(normalized string, scanner decodedSafetyPriorityScanner) bool {
	matched, unhinted := decodedSafetyPriorityMatchedHints(normalized, scanner)
	return unhinted || len(matched) > 0
}

func decodedSafetyPriorityMatchedHints(normalized string, scanner decodedSafetyPriorityScanner) (map[string]struct{}, bool) {
	index := scanner.hintIndex
	if index == nil {
		index = buildDecodedSafetyHintIndex(scanner)
	}
	if index.automaton != nil {
		return index.automaton.match(normalized), index.unhinted
	}
	var matched map[string]struct{}
	for offset := 0; offset < len(normalized); offset++ {
		for _, hint := range index.byFirstByte[normalized[offset]] {
			if len(hint) <= len(normalized)-offset && strings.HasPrefix(normalized[offset:], hint) {
				if matched == nil {
					matched = make(map[string]struct{})
				}
				matched[hint] = struct{}{}
			}
		}
	}
	return matched, index.unhinted
}

func decodedSafetyPriorityMatchedHintsSource(source string, scanner decodedSafetyPriorityScanner) (map[string]struct{}, bool) {
	index := scanner.hintIndex
	if index == nil {
		index = buildDecodedSafetyHintIndex(scanner)
	}
	if index.automaton != nil {
		return index.automaton.matchNormalizedSource(source), index.unhinted
	}
	return decodedSafetyPriorityMatchedHints(normalizeForScan(source), scanner)
}

func decodedSafetyPriorityMatchedHintSetCanMatch(scanner decodedSafetyPriorityScanner, matchedHints map[string]struct{}) bool {
	if len(matchedHints) == 0 {
		return false
	}
	for _, candidate := range scanner.patterns {
		if len(candidate.hintClauses) == 0 || containsDecodedSafetyHintClauseWithMatches("", candidate.hintClauses, matchedHints) {
			return true
		}
	}
	for _, word := range scanner.sensitiveWords {
		if _, exists := matchedHints[word]; exists {
			return true
		}
	}
	return false
}

func decodedSafetyWordEvidence(text string, loc []int) string {
	if len(loc) != 2 || loc[0] < 0 || loc[1] <= loc[0] || loc[1] > len(text) {
		return ""
	}
	const padding = 192
	start := max(0, loc[0]-padding)
	end := min(len(text), loc[1]+padding)
	for start < loc[0] && !utf8.RuneStart(text[start]) {
		start++
	}
	for end > loc[1] && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return strings.TrimSpace(text[start:end])
}

func decodedSafetyPatternEvidence(text string, pattern compiledPattern, fallbackLoc []int) string {
	const (
		evidencePadding  = 192
		maxEvidenceBytes = 6 * 1024
	)
	locations := make([][]int, 0, 2+len(pattern.all)+len(pattern.any))
	appendLocation := func(loc []int) bool {
		if len(loc) != 2 || loc[0] < 0 || loc[1] <= loc[0] || loc[1] > len(text) {
			return false
		}
		locations = append(locations, []int{loc[0], loc[1]})
		return true
	}
	if isBuiltinMinorSafetyPattern(pattern) {
		appendLocation(fallbackLoc)
	} else {
		if pattern.re != nil && !appendLocation(pattern.re.FindStringIndex(text)) {
			return ""
		}
		for _, re := range pattern.all {
			if !appendLocation(re.FindStringIndex(text)) {
				return ""
			}
		}
		minimum := pattern.cfg.MinMatches
		if minimum <= 0 && len(pattern.any) > 0 {
			minimum = 1
		}
		matchedAny := 0
		for _, re := range pattern.any {
			if loc := re.FindStringIndex(text); loc != nil {
				appendLocation(loc)
				matchedAny++
				if matchedAny >= minimum {
					break
				}
			}
		}
		if matchedAny < minimum {
			return ""
		}
	}
	if len(locations) == 0 && !appendLocation(fallbackLoc) {
		return ""
	}
	sort.Slice(locations, func(i, j int) bool {
		if locations[i][0] == locations[j][0] {
			return locations[i][1] < locations[j][1]
		}
		return locations[i][0] < locations[j][0]
	})
	spans := make([][2]int, 0, len(locations))
	for _, loc := range locations {
		start := loc[0] - evidencePadding
		if start < 0 {
			start = 0
		}
		end := loc[1] + evidencePadding
		if end > len(text) {
			end = len(text)
		}
		if len(spans) > 0 && start <= spans[len(spans)-1][1] {
			if end > spans[len(spans)-1][1] {
				spans[len(spans)-1][1] = end
			}
			continue
		}
		spans = append(spans, [2]int{start, end})
	}
	var output strings.Builder
	for _, span := range spans {
		if output.Len() > 0 {
			output.WriteByte(' ')
		}
		if output.Len()+span[1]-span[0] > maxEvidenceBytes {
			return ""
		}
		output.WriteString(text[span[0]:span[1]])
	}
	evidence := strings.TrimSpace(output.String())
	if evidence == "" || !patternShouldRun(evidence, pattern, nil) ||
		patternSuppressedForQuotedPolicyReview(evidence, pattern) ||
		patternSuppressedForDefensiveRuleArtifact(evidence, pattern) ||
		compiledPatternMatchIndex(evidence, pattern) == nil {
		return ""
	}
	return evidence
}

const maxDecodedSafetyHintClauses = 512

// decodedSafetyGuaranteedHintClauses returns only clauses that are proven by
// the regexp syntax tree to occur in every successful match of the compiled
// PatternConfig. Unlike decodedSafetyMandatoryHintClauses, it never uses
// hand-maintained or heuristic fallback vocabulary, so it is safe to use as a
// hard eligibility gate before running the regexp engine.
func decodedSafetyGuaranteedHintClauses(pattern compiledPattern) [][]string {
	var clauses [][]string
	combineRequired := func(expression string) {
		if strings.TrimSpace(expression) == "" {
			return
		}
		expressionClauses, guaranteed := decodedSafetyRegexpGuaranteedHintClauses(expression)
		if !guaranteed || len(expressionClauses) == 0 {
			return
		}
		clauses = preferDecodedSafetyGuaranteedHintClauses(clauses, expressionClauses)
	}

	combineRequired(pattern.cfg.Pattern)
	for _, expression := range pattern.cfg.AllPatterns {
		combineRequired(expression)
	}

	// At least one AnyPatterns expression must match (or MinMatches of them).
	// Their union is a mandatory OR-clause only when every eligible expression
	// exposes a syntax-proven literal. A single unprovable branch disables this
	// part of the gate rather than risking a false negative.
	if len(pattern.cfg.AnyPatterns) > 0 {
		anyClauses := make([][]string, 0)
		allGuaranteed := true
		for _, expression := range pattern.cfg.AnyPatterns {
			expressionClauses, guaranteed := decodedSafetyRegexpGuaranteedHintClauses(expression)
			if !guaranteed || len(expressionClauses) == 0 || len(anyClauses)+len(expressionClauses) > maxDecodedSafetyHintClauses {
				allGuaranteed = false
				break
			}
			anyClauses = append(anyClauses, expressionClauses...)
		}
		if allGuaranteed && len(anyClauses) > 0 {
			anyClauses = normalizeDecodedSafetyHintClauses(anyClauses)
			clauses = preferDecodedSafetyGuaranteedHintClauses(clauses, anyClauses)
		}
	}
	return normalizeDecodedSafetyHintClauses(clauses)
}

func preferDecodedSafetyGuaranteedHintClauses(current, candidate [][]string) [][]string {
	if len(current) == 0 {
		return candidate
	}
	if len(candidate) == 0 {
		return current
	}
	quality := func(clauses [][]string) (minRunes int, alternatives int) {
		minRunes = int(^uint(0) >> 1)
		for _, clause := range clauses {
			clauseRunes := 0
			for _, hint := range clause {
				clauseRunes += utf8.RuneCountInString(hint)
			}
			if clauseRunes < minRunes {
				minRunes = clauseRunes
			}
		}
		return minRunes, len(clauses)
	}
	currentRunes, currentAlternatives := quality(current)
	candidateRunes, candidateAlternatives := quality(candidate)
	if candidateRunes > currentRunes || candidateRunes == currentRunes && candidateAlternatives < currentAlternatives {
		return candidate
	}
	return current
}

func decodedSafetyRegexpGuaranteedHintClauses(expression string) ([][]string, bool) {
	parsed, err := syntax.Parse(expression, syntax.Perl)
	if err != nil {
		return nil, false
	}
	return decodedSafetySyntaxGuaranteedHintClauses(parsed.Simplify())
}

func decodedSafetySyntaxGuaranteedHintClauses(expression *syntax.Regexp) ([][]string, bool) {
	if expression == nil {
		return nil, false
	}
	switch expression.Op {
	case syntax.OpLiteral:
		return decodedSafetyLiteralGuaranteedHintClauses(expression)
	case syntax.OpCapture, syntax.OpPlus:
		return decodedSafetySyntaxGuaranteedHintClauses(expression.Sub[0])
	case syntax.OpRepeat:
		if expression.Min < 1 {
			return nil, false
		}
		return decodedSafetySyntaxGuaranteedHintClauses(expression.Sub[0])
	case syntax.OpConcat:
		clauses := make([][]string, 0)
		for _, child := range expression.Sub {
			childClauses, childGuaranteed := decodedSafetySyntaxGuaranteedHintClauses(child)
			if !childGuaranteed || len(childClauses) == 0 {
				continue
			}
			clauses = preferDecodedSafetyGuaranteedHintClauses(clauses, childClauses)
		}
		return clauses, len(clauses) > 0
	case syntax.OpAlternate:
		clauses := make([][]string, 0)
		for _, child := range expression.Sub {
			childClauses, childGuaranteed := decodedSafetySyntaxGuaranteedHintClauses(child)
			if !childGuaranteed || len(childClauses) == 0 || len(clauses)+len(childClauses) > maxDecodedSafetyHintClauses {
				return nil, false
			}
			clauses = append(clauses, childClauses...)
		}
		return normalizeDecodedSafetyHintClauses(clauses), len(clauses) > 0
	default:
		return nil, false
	}
}

func decodedSafetyLiteralGuaranteedHintClauses(expression *syntax.Regexp) ([][]string, bool) {
	if expression == nil || expression.Op != syntax.OpLiteral || len(expression.Rune) == 0 {
		return nil, false
	}
	if expression.Flags&syntax.FoldCase == 0 {
		hint := normalizeForScan(string(expression.Rune))
		if utf8.RuneCountInString(hint) < 2 {
			return nil, false
		}
		return [][]string{{hint}}, true
	}

	// normalizeForScan lowercases each rune. Most Unicode simple-fold cycles
	// therefore collapse to one canonical rune, but a few (for example long-s)
	// do not. Keep the longest contiguous fold-stable substring. Every regexp
	// match must contain that exact normalized substring, without enumerating an
	// exponential Cartesian product of all case-fold variants.
	stable := func(value rune) (rune, bool) {
		canonical := unicode.ToLower(value)
		for candidate := unicode.SimpleFold(value); candidate != value; candidate = unicode.SimpleFold(candidate) {
			if unicode.ToLower(candidate) != canonical {
				return 0, false
			}
		}
		if unicode.IsSpace(canonical) || unicode.IsControl(canonical) || canonical == '`' {
			return 0, false
		}
		return canonical, true
	}
	best := make([]rune, 0, len(expression.Rune))
	current := make([]rune, 0, len(expression.Rune))
	for _, value := range expression.Rune {
		canonical, ok := stable(value)
		if !ok {
			if len(current) > len(best) {
				best = append(best[:0], current...)
			}
			current = current[:0]
			continue
		}
		current = append(current, canonical)
	}
	if len(current) > len(best) {
		best = append(best[:0], current...)
	}
	if len(best) < 2 {
		return nil, false
	}
	hint := normalizeForScan(string(best))
	if utf8.RuneCountInString(hint) < 2 {
		return nil, false
	}
	return [][]string{{hint}}, true
}

func decodedSafetyMandatoryHintClauses(pattern compiledPattern) [][]string {
	if pattern.cfg.Name == "self_harm_facilitation" {
		// Every branch of the dedicated grammar contains one of these target
		// terms. Keep the list explicit so future relationship refinements do not
		// make the syntax extractor drop this strict rule from large-request and
		// decoded-content fast paths.
		return singleDecodedSafetyHintClauses([]string{
			"suicide", "self-harm", "self harm", "cutting", "starve myself", "starving myself",
			"自杀", "自残", "割腕", "伤害自己", "绝食",
		})
	}
	if pattern.cfg.Name == "prompt_unrestricted_mode" {
		return singleDecodedSafetyHintClauses([]string{
			"unrestricted mode", "unrestricted model", "unrestricted assistant", "unrestricted access",
			"developer mode", "jailbreak", "no restriction", "content filters disabled", "content filters off",
			"无限制模式", "开发者模式", "关闭安全过滤", "关闭内容过滤", "关闭安全限制", "关闭内容限制", "破限", "破甲", "越狱模式",
		})
	}
	clauses := make([][]string, 0)
	combineMandatory := func(expression string) bool {
		if strings.TrimSpace(expression) == "" {
			return false
		}
		expressionClauses, guaranteed := decodedSafetyRegexpMandatoryHintClauses(expression)
		if !guaranteed || len(expressionClauses) == 0 {
			return false
		}
		if len(clauses) == 0 {
			clauses = expressionClauses
			return true
		}
		combined, ok := combineDecodedSafetyHintClauses(clauses, expressionClauses)
		if !ok {
			clauses = nil
			return false
		}
		clauses = combined
		return true
	}

	mandatoryFound := combineMandatory(pattern.cfg.Pattern)
	for _, expression := range pattern.cfg.AllPatterns {
		mandatoryFound = combineMandatory(expression) || mandatoryFound
	}
	if mandatoryFound && len(clauses) > 0 {
		return normalizeDecodedSafetyHintClauses(clauses)
	}

	if len(pattern.cfg.AnyPatterns) > 0 {
		anyClauses := make([][]string, 0)
		for _, expression := range pattern.cfg.AnyPatterns {
			expressionClauses, guaranteed := decodedSafetyRegexpMandatoryHintClauses(expression)
			if !guaranteed || len(expressionClauses) == 0 {
				anyClauses = nil
				break
			}
			anyClauses = append(anyClauses, expressionClauses...)
			if len(anyClauses) > maxDecodedSafetyHintClauses {
				anyClauses = nil
				break
			}
		}
		if len(anyClauses) > 0 {
			return normalizeDecodedSafetyHintClauses(anyClauses)
		}
	}

	switch pattern.cfg.Name {
	case "prompt_unrestricted_activation_request":
		return singleDecodedSafetyHintClauses([]string{"jailbreak", "unrestricted", "developer mode", "破限", "破甲", "越狱", "无限制", "开发者"})
	case "minor_exploitation":
		return singleDecodedSafetyHintClauses([]string{
			"minor", "child", "children", "schoolchildren", "kid", "preteen", "teen", "teenager", "adolescent", "youth", "toddler", "infant", "underage", "year old", "csam",
			"未成年", "儿童", "孩子", "小孩", "小学生", "幼童", "幼儿", "婴幼儿", "青少年", "少年", "岁", "儿童色情", "儿童性虐待材料",
		})
	default:
		return singleDecodedSafetyHintClauses(strongDecodedSafetyFallbackHints(decodedSafetyMandatoryHints(pattern)))
	}
}

func strongDecodedSafetyFallbackHints(hints []string) []string {
	weak := map[string]struct{}{
		"build": {}, "conduct": {}, "create": {}, "develop": {}, "execute": {}, "generate": {},
		"give": {}, "launch": {}, "make": {}, "perform": {}, "provide": {}, "run": {}, "use": {}, "write": {},
		"for": {}, "from": {}, "into": {}, "me": {}, "on": {}, "the": {}, "to": {}, "us": {}, "with": {},
	}
	out := make([]string, 0, len(hints))
	for _, hint := range hints {
		hint = strings.TrimSpace(hint)
		if hint == "" {
			continue
		}
		if _, exists := weak[hint]; exists {
			continue
		}
		if isASCIIText(hint) && utf8.RuneCountInString(hint) < 4 {
			continue
		}
		out = append(out, hint)
	}
	return out
}

func isASCIIText(text string) bool {
	for index := 0; index < len(text); index++ {
		if text[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func decodedSafetyRegexpMandatoryHintClauses(expression string) ([][]string, bool) {
	parsed, err := syntax.Parse(expression, syntax.Perl)
	if err != nil {
		return nil, false
	}
	return decodedSafetySyntaxMandatoryHintClauses(parsed.Simplify())
}

func decodedSafetySyntaxMandatoryHintClauses(expression *syntax.Regexp) ([][]string, bool) {
	if expression == nil {
		return nil, false
	}
	switch expression.Op {
	case syntax.OpLiteral:
		hint := normalizeForScan(string(expression.Rune))
		if utf8.RuneCountInString(hint) < 2 {
			return nil, false
		}
		return [][]string{{hint}}, true
	case syntax.OpCapture, syntax.OpPlus:
		return decodedSafetySyntaxMandatoryHintClauses(expression.Sub[0])
	case syntax.OpRepeat:
		if expression.Min < 1 {
			return nil, false
		}
		return decodedSafetySyntaxMandatoryHintClauses(expression.Sub[0])
	case syntax.OpConcat:
		clauses := make([][]string, 0)
		found := false
		for _, child := range expression.Sub {
			childClauses, childGuaranteed := decodedSafetySyntaxMandatoryHintClauses(child)
			if !childGuaranteed || len(childClauses) == 0 {
				continue
			}
			if !found {
				clauses = childClauses
				found = true
				continue
			}
			combined, ok := combineDecodedSafetyHintClauses(clauses, childClauses)
			if !ok {
				return nil, false
			}
			clauses = combined
		}
		return clauses, found
	case syntax.OpAlternate:
		clauses := make([][]string, 0)
		for _, child := range expression.Sub {
			childClauses, childGuaranteed := decodedSafetySyntaxMandatoryHintClauses(child)
			if !childGuaranteed || len(childClauses) == 0 {
				return nil, false
			}
			clauses = append(clauses, childClauses...)
			if len(clauses) > maxDecodedSafetyHintClauses {
				return nil, false
			}
		}
		return clauses, len(clauses) > 0
	default:
		return nil, false
	}
}

func combineDecodedSafetyHintClauses(left, right [][]string) ([][]string, bool) {
	if len(left) == 0 {
		return right, len(right) > 0
	}
	if len(right) == 0 || len(left) > maxDecodedSafetyHintClauses/len(right) {
		return nil, false
	}
	out := make([][]string, 0, len(left)*len(right))
	for _, leftClause := range left {
		for _, rightClause := range right {
			clause := make([]string, 0, len(leftClause)+len(rightClause))
			clause = append(clause, leftClause...)
			clause = append(clause, rightClause...)
			out = append(out, clause)
		}
	}
	return out, true
}

func normalizeDecodedSafetyHintClauses(clauses [][]string) [][]string {
	seenClauses := make(map[string]struct{})
	out := make([][]string, 0, len(clauses))
	for _, clause := range clauses {
		seenHints := make(map[string]struct{})
		normalized := make([]string, 0, len(clause))
		for _, hint := range clause {
			hint = strings.TrimSpace(hint)
			if hint == "" {
				continue
			}
			if _, exists := seenHints[hint]; exists {
				continue
			}
			seenHints[hint] = struct{}{}
			normalized = append(normalized, hint)
		}
		if len(normalized) == 0 {
			continue
		}
		sort.Slice(normalized, func(i, j int) bool {
			if len(normalized[i]) == len(normalized[j]) {
				return normalized[i] < normalized[j]
			}
			return len(normalized[i]) > len(normalized[j])
		})
		key := strings.Join(normalized, "\x00")
		if _, exists := seenClauses[key]; exists {
			continue
		}
		seenClauses[key] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func singleDecodedSafetyHintClauses(hints []string) [][]string {
	clauses := make([][]string, 0, len(hints))
	for _, hint := range hints {
		clauses = append(clauses, []string{hint})
	}
	return normalizeDecodedSafetyHintClauses(clauses)
}

func flattenDecodedSafetyHintClauses(clauses [][]string) []string {
	seen := make(map[string]struct{})
	hints := make([]string, 0)
	for _, clause := range clauses {
		for _, hint := range clause {
			if _, exists := seen[hint]; exists {
				continue
			}
			seen[hint] = struct{}{}
			hints = append(hints, hint)
		}
	}
	return hints
}

func containsDecodedSafetyHintClause(text string, clauses [][]string) bool {
	return containsDecodedSafetyHintClauseWithMatches(text, clauses, nil)
}

func containsDecodedSafetyHintClauseWithMatches(text string, clauses [][]string, matchedHints map[string]struct{}) bool {
	for _, clause := range clauses {
		matched := true
		for _, hint := range clause {
			if matchedHints != nil {
				if _, exists := matchedHints[hint]; exists {
					continue
				}
				matched = false
				break
			}
			if !strings.Contains(text, hint) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func decodedSafetyMandatoryHints(pattern compiledPattern) []string {
	sets := make([][]string, 0, 1+len(pattern.all))
	if pattern.cfg.Pattern != "" {
		if hints, guaranteed := decodedSafetyRegexpMandatoryHints(pattern.cfg.Pattern); guaranteed {
			sets = append(sets, hints)
		}
	}
	for _, expression := range pattern.cfg.AllPatterns {
		if hints, guaranteed := decodedSafetyRegexpMandatoryHints(expression); guaranteed {
			sets = append(sets, hints)
		}
	}
	// At least MinMatches AnyPatterns must match. Requiring one hint from their
	// union is safe only when every eligible branch has a mandatory literal.
	if len(pattern.cfg.AnyPatterns) > 0 {
		anyHints := make([]string, 0, len(pattern.cfg.AnyPatterns))
		allGuaranteed := true
		for _, expression := range pattern.cfg.AnyPatterns {
			hints, guaranteed := decodedSafetyRegexpMandatoryHints(expression)
			if !guaranteed {
				allGuaranteed = false
				break
			}
			anyHints = append(anyHints, hints...)
		}
		if allGuaranteed {
			sets = append(sets, anyHints)
		}
	}
	if len(sets) == 0 {
		// These broad multilingual alternations do not expose one literal that is
		// common to every branch, but their policy targets are still finite and
		// cheap to screen before running the full expression.
		switch pattern.cfg.Name {
		case "prompt_unrestricted_activation_request":
			return []string{"jailbreak", "unrestricted", "developer mode", "破限", "破甲", "越狱", "无限制", "开发者"}
		case "minor_exploitation":
			return []string{
				"minor", "child", "children", "schoolchildren", "kid", "preteen", "teen", "teenager", "adolescent", "youth", "toddler", "infant", "underage", "year old", "csam",
				"未成年", "儿童", "孩子", "小孩", "小学生", "幼童", "幼儿", "婴幼儿", "青少年", "少年", "岁", "儿童色情", "儿童性虐待材料",
			}
		}
		return nil
	}
	seen := make(map[string]struct{})
	hints := make([]string, 0, 8)
	for _, set := range sets {
		for _, hint := range set {
			if _, exists := seen[hint]; exists {
				continue
			}
			seen[hint] = struct{}{}
			hints = append(hints, hint)
		}
	}
	return hints
}

func decodedSafetyRegexpMandatoryHints(expression string) ([]string, bool) {
	parsed, err := syntax.Parse(expression, syntax.Perl)
	if err != nil {
		return nil, false
	}
	hints, guaranteed := decodedSafetySyntaxMandatoryHints(parsed.Simplify())
	if !guaranteed || len(hints) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(hints))
	for hint := range hints {
		out = append(out, hint)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) == len(out[j]) {
			return out[i] < out[j]
		}
		return len(out[i]) > len(out[j])
	})
	return out, true
}

func decodedSafetySyntaxMandatoryHints(expression *syntax.Regexp) (map[string]struct{}, bool) {
	if expression == nil {
		return nil, false
	}
	switch expression.Op {
	case syntax.OpLiteral:
		hint := normalizeForScan(string(expression.Rune))
		if utf8.RuneCountInString(hint) < 2 {
			return nil, false
		}
		return map[string]struct{}{hint: {}}, true
	case syntax.OpCapture, syntax.OpPlus:
		return decodedSafetySyntaxMandatoryHints(expression.Sub[0])
	case syntax.OpRepeat:
		if expression.Min < 1 {
			return nil, false
		}
		return decodedSafetySyntaxMandatoryHints(expression.Sub[0])
	case syntax.OpConcat:
		out := make(map[string]struct{})
		guaranteed := false
		for _, child := range expression.Sub {
			hints, childGuaranteed := decodedSafetySyntaxMandatoryHints(child)
			if !childGuaranteed {
				continue
			}
			guaranteed = true
			for hint := range hints {
				out[hint] = struct{}{}
			}
		}
		return out, guaranteed
	case syntax.OpAlternate:
		out := make(map[string]struct{})
		for _, child := range expression.Sub {
			hints, childGuaranteed := decodedSafetySyntaxMandatoryHints(child)
			if !childGuaranteed {
				return nil, false
			}
			for hint := range hints {
				out[hint] = struct{}{}
			}
		}
		return out, len(out) > 0
	default:
		return nil, false
	}
}

func containsDecodedSafetyHint(text string, hints []string) bool {
	for _, hint := range hints {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return false
}

func encodedCandidates(text string, cfg NormalizationConfig, maxFragmentBytes int) []encodedCandidate {
	var hexCandidates []encodedCandidate
	var base64Candidates []encodedCandidate
	if cfg.DecodeHex {
		hexCandidates = embeddedHexCandidates(text, maxFragmentBytes)
	}
	if cfg.DecodeBase64 {
		base64Candidates = embeddedBase64Candidates(text, maxFragmentBytes)
	}
	if len(hexCandidates) == 0 {
		return base64Candidates
	}
	if len(base64Candidates) == 0 {
		return hexCandidates
	}
	candidates := make([]encodedCandidate, 0, len(hexCandidates)+len(base64Candidates))
	hexIndex, base64Index := 0, 0
	less := func(left, right encodedCandidate) bool {
		if left.start == right.start {
			return left.end < right.end
		}
		return left.start < right.start
	}
	for hexIndex < len(hexCandidates) && base64Index < len(base64Candidates) {
		if less(hexCandidates[hexIndex], base64Candidates[base64Index]) {
			candidates = append(candidates, hexCandidates[hexIndex])
			hexIndex++
		} else {
			candidates = append(candidates, base64Candidates[base64Index])
			base64Index++
		}
	}
	candidates = append(candidates, hexCandidates[hexIndex:]...)
	candidates = append(candidates, base64Candidates[base64Index:]...)
	return candidates
}

func embeddedBase64Candidates(text string, maxFragmentBytes int) []encodedCandidate {
	candidates := make([]encodedCandidate, 0, 4)
	for i := 0; i < len(text); {
		if !isASCIIBase64Data(text[i]) {
			i++
			continue
		}
		start := i
		for i < len(text) && isASCIIBase64Data(text[i]) {
			i++
		}
		for padding := 0; i < len(text) && text[i] == '=' && padding < 2; padding++ {
			i++
		}
		value := text[start:i]
		if len(value) < 12 || len(value) > maxFragmentBytes || !isBase64Candidate(value) {
			continue
		}
		candidates = append(candidates, encodedCandidate{start: start, end: i, value: value, kind: "base64"})
	}
	return candidates
}

func isASCIIBase64Data(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '+' || value == '/' || value == '-' || value == '_'
}

func embeddedHexCandidates(text string, maxFragmentBytes int) []encodedCandidate {
	candidates := make([]encodedCandidate, 0, 4)
	for i := 0; i < len(text); {
		start := i
		digitStart := i
		if i+2 <= len(text) && text[i] == '0' && (text[i+1] == 'x' || text[i+1] == 'X') {
			digitStart = i + 2
			i += 2
		}
		if i >= len(text) || !isASCIIHex(text[i]) {
			i = start + 1
			continue
		}
		for i < len(text) && isASCIIHex(text[i]) {
			i++
		}
		digits := i - digitStart
		if digits < 16 || digits%2 != 0 || i-start > maxFragmentBytes {
			continue
		}
		if start > 0 && isEncodedIdentifierByte(text[start-1]) {
			continue
		}
		if i < len(text) && isEncodedIdentifierByte(text[i]) {
			continue
		}
		candidates = append(candidates, encodedCandidate{start: start, end: i, value: text[start:i], kind: "hex"})
	}
	return candidates
}

func isASCIIHex(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

func isEncodedIdentifierByte(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || value == '_'
}

func overlapsEncodedCandidate(candidates []encodedCandidate, start, end int) bool {
	for _, candidate := range candidates {
		if start < candidate.end && end > candidate.start {
			return true
		}
	}
	return false
}

type recognizedEncodedSpan struct {
	start       int
	end         int
	replacement string
}

// collapseRecognizedEncodedPayloads removes only tokens that successfully
// decode to printable text (or carry a supported compression header). This
// keeps surrounding request intent visible while preventing thousands of
// opaque encoded characters from being rescanned once per derived view.
// Accepted blocks are substituted with their decoded value; all other
// recognized blocks collapse to whitespace.
func collapseRecognizedEncodedPayloads(source string, cfg NormalizationConfig, maxFragmentBytes int, accepted []decodedBlock) string {
	if source == "" || (!cfg.DecodeBase64 && !cfg.DecodeHex) {
		return source
	}
	acceptedValues := make(map[[2]int]string, len(accepted))
	for _, block := range accepted {
		if block.start >= 0 && block.end > block.start && block.end <= len(source) {
			acceptedValues[[2]int{block.start, block.end}] = block.value
		}
	}
	candidates := encodedCandidates(source, cfg, maxFragmentBytes)
	spans := make([]recognizedEncodedSpan, 0, len(candidates))
	for _, candidate := range candidates {
		replacement, selected := acceptedValues[[2]int{candidate.start, candidate.end}]
		// Slash/underscore-separated plaintext can also satisfy the Base64
		// alphabet. Keep explicit fragmentation evidence in the source view so
		// compact minor-safety matching is not erased as an "encoded" token.
		if !selected && minorSafetyShouldInspectCompact(candidate.value) {
			continue
		}
		raw, ok := decodeEncodedCandidateRaw(candidate)
		if !ok {
			continue
		}
		if !selected {
			if _, printable := decodedPayloadText(raw, false, maxFragmentBytes); !printable && !(cfg.DecodeCompression && compressedPayload(raw)) {
				continue
			}
		}
		spans = append(spans, recognizedEncodedSpan{start: candidate.start, end: candidate.end, replacement: replacement})
	}
	if len(spans) == 0 {
		return source
	}
	var output strings.Builder
	output.Grow(len(source))
	cursor := 0
	for index := 0; index < len(spans); {
		span := spans[index]
		if span.start < cursor {
			index++
			continue
		}
		groupEnd := span.end
		replacement := span.replacement
		index++
		for index < len(spans) && spans[index].start < groupEnd {
			if spans[index].end > groupEnd {
				groupEnd = spans[index].end
			}
			if replacement == "" && spans[index].replacement != "" {
				replacement = spans[index].replacement
			}
			index++
		}
		output.WriteString(source[cursor:span.start])
		if replacement != "" {
			output.WriteString(replacement)
		}
		output.WriteByte(' ')
		cursor = groupEnd
	}
	output.WriteString(source[cursor:])
	return output.String()
}

func trimEncodedField(field string) string {
	field = strings.Trim(field, "\"'`()[]{}<>,.;")
	lower := strings.ToLower(field)
	for _, prefix := range []string{"base64:", "base64=", "b64:", "b64=", "hex:", "hex=", "gzip:", "gzip=", "zlib:", "zlib="} {
		if strings.HasPrefix(lower, prefix) {
			return strings.Trim(field[len(prefix):], "\"'`()[]{}<>,.;")
		}
	}
	return field
}

func isHexCandidate(value string) bool {
	value = strings.TrimPrefix(strings.TrimPrefix(value, "0x"), "0X")
	if len(value) < 16 || len(value)%2 != 0 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func isBase64Candidate(value string) bool {
	if len(value) < 12 {
		return false
	}
	padding := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '+', r == '/', r == '-', r == '_':
			if padding {
				return false
			}
		case r == '=':
			padding = true
		default:
			return false
		}
	}
	return true
}

func decodedPayloadText(raw []byte, allowCompression bool, limit int) (string, bool) {
	if len(raw) == 0 || limit <= 0 {
		return "", false
	}
	if allowCompression {
		if decompressed, ok := decompressSmallPayload(raw, limit); ok {
			return decompressed, true
		}
	}
	if len(raw) > limit || !utf8.Valid(raw) || !mostlyPrintable(string(raw)) {
		return "", false
	}
	return string(raw), true
}

func compressedPayload(raw []byte) bool {
	if len(raw) < 2 {
		return false
	}
	if raw[0] == 0x1f && raw[1] == 0x8b {
		return true
	}
	return raw[0]&0x0f == 8 && ((int(raw[0])<<8)|int(raw[1]))%31 == 0
}

func decompressSmallPayload(raw []byte, limit int) (string, bool) {
	value, _, overflow, ok := decompressSmallPayloadAccounted(raw, limit)
	return value, ok && !overflow
}

func decompressSmallPayloadAccounted(raw []byte, limit int) (value string, expanded int, overflow bool, ok bool) {
	if len(raw) < 2 || limit <= 0 {
		return "", 0, false, false
	}
	reader, opened := openCompressedPayload(raw)
	if !opened {
		return "", 0, false, false
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	expanded = len(decoded)
	if err != nil {
		return "", expanded, false, false
	}
	if len(decoded) > limit {
		return "", expanded, true, false
	}
	if !utf8.Valid(decoded) || !mostlyPrintable(string(decoded)) {
		return "", expanded, false, false
	}
	return string(decoded), expanded, false, true
}

func openCompressedPayload(raw []byte) (io.ReadCloser, bool) {
	if len(raw) < 2 {
		return nil, false
	}
	var (
		reader io.ReadCloser
		err    error
	)
	if raw[0] == 0x1f && raw[1] == 0x8b {
		reader, err = gzip.NewReader(bytes.NewReader(raw))
	} else if raw[0]&0x0f == 8 && ((int(raw[0])<<8)|int(raw[1]))%31 == 0 {
		reader, err = zlib.NewReader(bytes.NewReader(raw))
	} else {
		return nil, false
	}
	if err != nil {
		if reader != nil {
			_ = reader.Close()
		}
		return nil, false
	}
	return reader, true
}

// scanCompressedPayload scans a fair, bounded share of one compressed stream.
// Overlap preserves policy phrases split across read boundaries, while the
// caller's global/fair-share budgets keep aggregate expansion and CPU bounded.
// Incomplete means the cap or a stream error prevented reaching EOF; callers
// may audit that state but must not turn it into fabricated policy evidence.
func scanCompressedPayload(raw []byte, limit int, scanner decodedSafetyPriorityScanner) (priority int, evidence string, expanded int, complete bool, decoded bool) {
	return scanCompressedPayloadWithWindow(raw, limit, DefaultGuardScanChunkBytes, DefaultGuardScanOverlapBytes, scanner)
}

func scanCompressedPayloadWithWindow(raw []byte, limit, chunkBytes, overlapBytes int, scanner decodedSafetyPriorityScanner) (priority int, evidence string, expanded int, complete bool, decoded bool) {
	if limit <= 0 {
		return 0, "", 0, false, false
	}
	reader, opened := openCompressedPayload(raw)
	if !opened {
		return 0, "", 0, false, false
	}
	defer reader.Close()
	decoded = true
	if chunkBytes <= 0 {
		chunkBytes = DefaultGuardScanChunkBytes
	}
	if overlapBytes <= 0 || overlapBytes >= chunkBytes {
		overlapBytes = DefaultGuardScanOverlapBytes
		if overlapBytes >= chunkBytes {
			overlapBytes = chunkBytes / 16
			if overlapBytes < 1 {
				overlapBytes = 1
			}
			if overlapBytes >= chunkBytes {
				overlapBytes = chunkBytes - 1
			}
		}
	}
	bufferSize := min(chunkBytes, limit)
	if bufferSize <= 0 {
		return 0, "", 0, false, true
	}
	buffer := make([]byte, bufferSize)
	tail := make([]byte, 0, min(overlapBytes, limit))
	zeroReads := 0
	for expanded < limit {
		readSize := min(len(buffer), limit-expanded)
		n, err := reader.Read(buffer[:readSize])
		if n > 0 {
			zeroReads = 0
			expanded += n
			window := make([]byte, len(tail)+n)
			copy(window, tail)
			copy(window[len(tail):], buffer[:n])
			windowText := string(window)
			if !utf8.Valid(window) {
				windowText = string(bytes.ToValidUTF8(window, []byte(" ")))
			}
			if mostlyPrintable(windowText) {
				candidatePriority, candidateEvidence := decodedSafetyPriority(windowText, scanner)
				if candidatePriority > priority || candidatePriority == priority && evidence == "" && candidateEvidence != "" {
					priority = candidatePriority
					evidence = candidateEvidence
				}
			}
			tailSize := min(overlapBytes, len(window))
			tailStart := len(window) - tailSize
			for tailStart < len(window) && !utf8.RuneStart(window[tailStart]) {
				tailStart++
			}
			tail = append(tail[:0], window[tailStart:]...)
		} else {
			zeroReads++
		}
		if err == io.EOF {
			return priority, evidence, expanded, true, true
		}
		if err != nil || zeroReads >= 3 {
			return priority, evidence, expanded, false, true
		}
	}
	var extra [1]byte
	n, err := reader.Read(extra[:])
	return priority, evidence, expanded, n == 0 && err == io.EOF, true
}

func decodeROT13Text(text string) (string, bool) {
	letters := 0
	decoded := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			letters++
			return 'a' + (r-'a'+13)%26
		case r >= 'A' && r <= 'Z':
			letters++
			return 'A' + (r-'A'+13)%26
		default:
			return r
		}
	}, text)
	return decoded, letters >= 8 && decoded != text
}

func decodeEscapedText(text string) (string, bool) {
	if !strings.Contains(text, `\`) {
		return text, false
	}
	var out strings.Builder
	out.Grow(len(text))
	changed := false
	for i := 0; i < len(text); {
		if text[i] != '\\' || i+1 >= len(text) {
			out.WriteByte(text[i])
			i++
			continue
		}
		next := text[i+1]
		switch next {
		case 'u', 'U', 'x':
			digits := 4
			if next == 'U' {
				digits = 8
			} else if next == 'x' {
				digits = 2
			}
			end := i + 2 + digits
			if end <= len(text) {
				value, err := strconv.ParseUint(text[i+2:end], 16, 32)
				r := rune(value)
				if err == nil && utf8.ValidRune(r) && !(r >= 0xd800 && r <= 0xdfff) {
					out.WriteRune(r)
					i = end
					changed = true
					continue
				}
			}
		case 'n', 'r', 't', 'b', 'f', '\\', '/', '"':
			replacements := map[byte]byte{'n': '\n', 'r': '\r', 't': '\t', 'b': '\b', 'f': '\f', '\\': '\\', '/': '/', '"': '"'}
			out.WriteByte(replacements[next])
			i += 2
			changed = true
			continue
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String(), changed
}

func stripInvisible(text string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
			return -1
		}
		if unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		if mapped, ok := commonHomoglyphs[r]; ok {
			return mapped
		}
		return r
	}, text)
}

var commonHomoglyphs = map[rune]rune{
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'х': 'x', 'у': 'y', 'і': 'i', 'ј': 'j',
	'А': 'a', 'Е': 'e', 'О': 'o', 'Р': 'p', 'С': 'c', 'Х': 'x', 'У': 'y', 'І': 'i', 'Ј': 'j',
	'Α': 'a', 'Β': 'b', 'Ε': 'e', 'Ζ': 'z', 'Η': 'h', 'Ι': 'i', 'Κ': 'k', 'Μ': 'm', 'Ν': 'n', 'Ο': 'o', 'Ρ': 'p', 'Τ': 't', 'Χ': 'x',
}

func compactForScan(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, text)
}

func mostlyPrintable(text string) bool {
	if text == "" {
		return false
	}
	printable, total := 0, 0
	for _, r := range text {
		total++
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			printable++
		}
	}
	return total > 0 && printable*100/total >= 85
}
