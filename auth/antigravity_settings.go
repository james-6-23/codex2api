package auth

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Antigravity 渠道级设置（system_settings.antigravity_config 列）。包含模型重定向
// （下游请求不带思考强度后缀的逻辑模型如 gemini-3.8-flash 时，自动按配置落到某个
// 固定档位如 gemini-3.8-flash-high）和思考内容下发开关。

// AntigravityModelRedirectMaxEntries 限制重定向条目数，避免无界 JSON。
const AntigravityModelRedirectMaxEntries = 64

// 配额自动刷新间隔的取值范围与缺省值（Spec §8.1）。
const (
	AntigravityQuotaRefreshIntervalMinMinutes     = 3
	AntigravityQuotaRefreshIntervalMaxMinutes     = 60
	AntigravityQuotaRefreshIntervalDefaultMinutes = 60
)

// 临期优先调度的时间窗（小时）取值范围与缺省值：周配额离重置不超过该时长、且仍有
// 存量额度的账号在同优先级同健康档位内优先承接新会话。
const (
	AntigravityExpiryPriorityHorizonMinHours     = 1
	AntigravityExpiryPriorityHorizonMaxHours     = 168
	AntigravityExpiryPriorityHorizonDefaultHours = 24
)

// AntigravitySettings 是 antigravity_config 列的 JSON 形态。
type AntigravitySettings struct {
	// ModelRedirects 以逻辑模型（无后缀）为键，固定档位公开 ID 为值。
	ModelRedirects map[string]string `json:"model_redirects,omitempty"`
	// RedirectOverridesEffort 为 true 时，即使请求自带 reasoning.effort 也按重定向
	// 走；默认只在请求没有指定思考强度时生效。
	RedirectOverridesEffort bool `json:"redirect_overrides_effort,omitempty"`
	// ExposeThoughts 为 true 时，OAuth 账号的 Gemini 请求带 includeThoughts，并把
	// 上游 thought 片段作为 reasoning 输出下发；默认关闭，保持只下发正文。
	ExposeThoughts bool `json:"expose_thoughts,omitempty"`
	// QuotaRefreshEnabled 控制是否派发自动配额查询（缺省 true）。关闭只停止新查询，
	// 已生效的门禁与模型目录维护不受影响（Spec §8.3）。
	QuotaRefreshEnabled *bool `json:"quota_refresh_enabled,omitempty"`
	// QuotaRefreshIntervalMinutes 是单账号一轮配额查询的名义间隔，缺省 60，范围
	// [3, 60]。读取一律经 GetQuotaRefreshIntervalMinutes 钳位。
	QuotaRefreshIntervalMinutes *int `json:"quota_refresh_interval_minutes,omitempty"`
	// QuotaGateEnabled 是硬门禁止血开关（缺省 true）：只控制是否用配额事实阻断调度
	// 与软评分，不改写、不删除已落库快照（Spec §8.1）。
	QuotaGateEnabled *bool `json:"quota_gate_enabled,omitempty"`
	// ExpiryPriorityEnabled 开启临期优先调度（缺省 false）：新会话选号时，请求模型所在
	// 配额族的周配额临近重置且仍有存量的账号优先承接，避免到期清零浪费。只在
	// round_robin + 加权避险路径上生效。
	ExpiryPriorityEnabled *bool `json:"expiry_priority_enabled,omitempty"`
	// ExpiryPriorityHorizonHours 是「临期」的时间窗，缺省 24，范围 [1, 168]。读取一律
	// 经 GetExpiryPriorityHorizonHours 钳位。
	ExpiryPriorityHorizonHours *int `json:"expiry_priority_horizon_hours,omitempty"`
	// ConfigRevision 是跨实例拒旧的单调递增版本号，由 PUT 侧在 CAS 事务内生成。
	ConfigRevision int64 `json:"config_revision,omitempty"`
}

// GetQuotaRefreshEnabled 返回自动配额刷新的生效值，未配置视为开启。
func (s AntigravitySettings) GetQuotaRefreshEnabled() bool {
	if s.QuotaRefreshEnabled == nil {
		return true
	}
	return *s.QuotaRefreshEnabled
}

// GetQuotaRefreshIntervalMinutes 返回钳位到 [3, 60] 的生效间隔，未配置视为 60。
func (s AntigravitySettings) GetQuotaRefreshIntervalMinutes() int {
	if s.QuotaRefreshIntervalMinutes == nil {
		return AntigravityQuotaRefreshIntervalDefaultMinutes
	}
	value := *s.QuotaRefreshIntervalMinutes
	if value < AntigravityQuotaRefreshIntervalMinMinutes {
		return AntigravityQuotaRefreshIntervalMinMinutes
	}
	if value > AntigravityQuotaRefreshIntervalMaxMinutes {
		return AntigravityQuotaRefreshIntervalMaxMinutes
	}
	return value
}

// GetQuotaGateEnabled 返回硬门禁的生效值，未配置视为开启。
func (s AntigravitySettings) GetQuotaGateEnabled() bool {
	if s.QuotaGateEnabled == nil {
		return true
	}
	return *s.QuotaGateEnabled
}

// GetExpiryPriorityEnabled 返回临期优先调度的生效值，未配置视为关闭。
func (s AntigravitySettings) GetExpiryPriorityEnabled() bool {
	return s.ExpiryPriorityEnabled != nil && *s.ExpiryPriorityEnabled
}

// GetExpiryPriorityHorizonHours 返回钳位到 [1, 168] 的临期时间窗，未配置视为 24。
func (s AntigravitySettings) GetExpiryPriorityHorizonHours() int {
	if s.ExpiryPriorityHorizonHours == nil {
		return AntigravityExpiryPriorityHorizonDefaultHours
	}
	value := *s.ExpiryPriorityHorizonHours
	if value < AntigravityExpiryPriorityHorizonMinHours {
		return AntigravityExpiryPriorityHorizonMinHours
	}
	if value > AntigravityExpiryPriorityHorizonMaxHours {
		return AntigravityExpiryPriorityHorizonMaxHours
	}
	return value
}

// clone 深拷贝全部字段（含新增指针字段），避免调用方改写进程内已生效配置。
func (s AntigravitySettings) clone() AntigravitySettings {
	out := AntigravitySettings{
		RedirectOverridesEffort:     s.RedirectOverridesEffort,
		ExposeThoughts:              s.ExposeThoughts,
		QuotaRefreshEnabled:         cloneOptionalBool(s.QuotaRefreshEnabled),
		QuotaRefreshIntervalMinutes: cloneOptionalInt(s.QuotaRefreshIntervalMinutes),
		QuotaGateEnabled:            cloneOptionalBool(s.QuotaGateEnabled),
		ExpiryPriorityEnabled:       cloneOptionalBool(s.ExpiryPriorityEnabled),
		ExpiryPriorityHorizonHours:  cloneOptionalInt(s.ExpiryPriorityHorizonHours),
		ConfigRevision:              s.ConfigRevision,
	}
	if len(s.ModelRedirects) > 0 {
		out.ModelRedirects = make(map[string]string, len(s.ModelRedirects))
		for key, value := range s.ModelRedirects {
			out.ModelRedirects[key] = value
		}
	}
	return out
}

// cloneOptionalBool 深拷贝可选布尔指针，避免外部修改造成内存逃逸或别名污染。
func cloneOptionalBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	copied := *v
	return &copied
}

// cloneOptionalInt 深拷贝可选整型指针，隔离配置快照内存副本。
func cloneOptionalInt(v *int) *int {
	if v == nil {
		return nil
	}
	copied := *v
	return &copied
}

var (
	configuredAntigravitySettings atomic.Value // AntigravitySettings
	// appliedAntigravityConfigRevision 是本机已应用配置的 revision 基线，用于跨实例
	// 拒旧与回退检测（Spec §8.2 #5）。
	appliedAntigravityConfigRevision atomic.Int64
)

// SetConfiguredAntigravitySettings 热更新 Antigravity 渠道设置。调用方负责先归一化。
// 同时把 revision 基线抬到不低于新配置（只升不降）。
func SetConfiguredAntigravitySettings(settings AntigravitySettings) {
	configuredAntigravitySettings.Store(settings)
	raiseConfiguredAntigravityRevision(settings.ConfigRevision)
}

// AppliedAntigravityConfigRevision 返回本机已应用配置的 revision 基线。
func AppliedAntigravityConfigRevision() int64 {
	return appliedAntigravityConfigRevision.Load()
}

// AntigravityApplyOutcome 描述 ApplyConfiguredAntigravitySettings 的处理结果。
type AntigravityApplyOutcome int

const (
	// AntigravityApplyApplied 表示配置更新（revision 更新或相同）并已应用。
	AntigravityApplyApplied AntigravityApplyOutcome = iota
	// AntigravityApplyRevisionRollback 表示 revision 缺失或小于已应用基线，检测到
	// 回退后重建基线并强制应用。
	AntigravityApplyRevisionRollback
)

// String 返回 AntigravityApplyOutcome 的文本枚举描述。
func (o AntigravityApplyOutcome) String() string {
	switch o {
	case AntigravityApplyRevisionRollback:
		return "revision_rollback"
	default:
		return "applied"
	}
}

// ApplyConfiguredAntigravitySettings 按 revision 单调应用配置（本机写侧与 outbox
// consumer 共用）。revision 缺失或小于已应用基线时不得静默拒绝后续所有更新：记录
// antigravity_config_revision_rollback 告警，把基线抬到 max(本机值, 新值) + 1 并强制
// 应用（Spec §8.2 #5）。调用方负责先归一化。
func ApplyConfiguredAntigravitySettings(next AntigravitySettings) AntigravityApplyOutcome {
	baseline := appliedAntigravityConfigRevision.Load()
	if next.ConfigRevision >= baseline {
		SetConfiguredAntigravitySettings(next)
		return AntigravityApplyApplied
	}
	// 回退：把基线抬到 max(本机值, 新值) + 1，保证后续合法更新不会被永久拒绝。
	rebuilt := baseline + 1
	if next.ConfigRevision+1 > rebuilt {
		rebuilt = next.ConfigRevision + 1
	}
	log.Printf("antigravity_config_revision_rollback incoming=%d applied_baseline=%d rebuilt_baseline=%d: 配置版本回退（混布写侧覆盖？），强制应用并重建基线",
		next.ConfigRevision, baseline, rebuilt)
	SetConfiguredAntigravitySettings(next)
	raiseConfiguredAntigravityRevision(rebuilt)
	return AntigravityApplyRevisionRollback
}

// raiseConfiguredAntigravityRevision 原子递增本机配置 revision 基线，确保版本单调不减。
func raiseConfiguredAntigravityRevision(revision int64) {
	if revision <= 0 {
		return
	}
	for {
		current := appliedAntigravityConfigRevision.Load()
		if revision <= current {
			return
		}
		if appliedAntigravityConfigRevision.CompareAndSwap(current, revision) {
			return
		}
	}
}

// AntigravityQuotaRestore 是仍被本机持有、但 DB 快照已抹掉的 quota_* 字段（混布写
// 侧覆盖危害，Spec §8.2 #5）。缺失即默认 true 会无痕掩盖止血开关，因此必须回写。
type AntigravityQuotaRestore struct {
	QuotaRefreshEnabled         *bool
	QuotaRefreshIntervalMinutes *int
	QuotaGateEnabled            *bool
}

// Empty 表示没有需要回写的字段。
func (r AntigravityQuotaRestore) Empty() bool {
	return r.QuotaRefreshEnabled == nil && r.QuotaRefreshIntervalMinutes == nil && r.QuotaGateEnabled == nil
}

// AntigravityQuotaFieldsLost 比对 DB 快照与本机生效配置，返回本机仍显式持有但 DB 已
// 丢失的 quota_* 字段。
func AntigravityQuotaFieldsLost(incoming, held AntigravitySettings) AntigravityQuotaRestore {
	restore := AntigravityQuotaRestore{}
	if incoming.QuotaRefreshEnabled == nil && held.QuotaRefreshEnabled != nil {
		restore.QuotaRefreshEnabled = cloneOptionalBool(held.QuotaRefreshEnabled)
	}
	if incoming.QuotaRefreshIntervalMinutes == nil && held.QuotaRefreshIntervalMinutes != nil {
		restore.QuotaRefreshIntervalMinutes = cloneOptionalInt(held.QuotaRefreshIntervalMinutes)
	}
	if incoming.QuotaGateEnabled == nil && held.QuotaGateEnabled != nil {
		restore.QuotaGateEnabled = cloneOptionalBool(held.QuotaGateEnabled)
	}
	return restore
}

// ConfiguredAntigravitySettings 返回当前生效设置的副本。
func ConfiguredAntigravitySettings() AntigravitySettings {
	v, _ := configuredAntigravitySettings.Load().(AntigravitySettings)
	return v.clone()
}

// AntigravityModelRedirect 返回逻辑模型配置的重定向目标；未配置返回空串。
func AntigravityModelRedirect(model string) string {
	v, _ := configuredAntigravitySettings.Load().(AntigravitySettings)
	if len(v.ModelRedirects) == 0 {
		return ""
	}
	return v.ModelRedirects[strings.ToLower(strings.TrimSpace(model))]
}

// AntigravityRedirectOverridesEffort 报告重定向是否压过请求自带的思考强度。
func AntigravityRedirectOverridesEffort() bool {
	v, _ := configuredAntigravitySettings.Load().(AntigravitySettings)
	return v.RedirectOverridesEffort
}

// AntigravityExpiryPriority 返回临期优先调度的开关与时间窗（热路径读，不深拷贝）。
func AntigravityExpiryPriority() (bool, time.Duration) {
	v, _ := configuredAntigravitySettings.Load().(AntigravitySettings)
	if !v.GetExpiryPriorityEnabled() {
		return false, 0
	}
	return true, time.Duration(v.GetExpiryPriorityHorizonHours()) * time.Hour
}

// AntigravityExposeThoughts 报告是否向下游下发 Gemini 思考内容。
func AntigravityExposeThoughts() bool {
	v, _ := configuredAntigravitySettings.Load().(AntigravitySettings)
	return v.ExposeThoughts
}

// ParseAntigravitySettings 解析 antigravity_config JSON。空串/`{}` 表示未配置；
// 解析失败返回错误而不是静默清空，由调用方决定回落。
func ParseAntigravitySettings(raw string) (AntigravitySettings, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return AntigravitySettings{}, nil
	}
	var settings AntigravitySettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return AntigravitySettings{}, fmt.Errorf("parse antigravity settings: %w", err)
	}
	return NormalizeAntigravitySettings(settings)
}

// EncodeAntigravitySettings 把设置编码成落库 JSON。空配置编码成 `{}`。
func EncodeAntigravitySettings(settings AntigravitySettings) (string, error) {
	normalized, err := NormalizeAntigravitySettings(settings)
	if err != nil {
		return "", err
	}
	if len(normalized.ModelRedirects) == 0 && !normalized.RedirectOverridesEffort && !normalized.ExposeThoughts &&
		normalized.QuotaRefreshEnabled == nil && normalized.QuotaRefreshIntervalMinutes == nil &&
		normalized.QuotaGateEnabled == nil && normalized.ExpiryPriorityEnabled == nil &&
		normalized.ExpiryPriorityHorizonHours == nil && normalized.ConfigRevision == 0 {
		return "{}", nil
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// NormalizeAntigravitySettings 去空白、统一小写、丢弃空值条目并限制条目数。
// 目标模型是否真是该逻辑模型的档位由 admin 层结合模型目录校验（auth 不依赖 proxy）。
// 新增 quota_* 字段原样携带：空重定向早退不得丢失显式 false 或非默认 interval。
func NormalizeAntigravitySettings(settings AntigravitySettings) (AntigravitySettings, error) {
	out := settings.clone()
	out.ModelRedirects = nil
	if len(settings.ModelRedirects) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(settings.ModelRedirects))
	for key := range settings.ModelRedirects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, rawKey := range keys {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		value := strings.ToLower(strings.TrimSpace(settings.ModelRedirects[rawKey]))
		if key == "" || value == "" || key == value {
			continue
		}
		if out.ModelRedirects == nil {
			out.ModelRedirects = make(map[string]string, len(keys))
		}
		out.ModelRedirects[key] = value
		if len(out.ModelRedirects) > AntigravityModelRedirectMaxEntries {
			return AntigravitySettings{}, fmt.Errorf("antigravity model redirects exceed the maximum of %d entries", AntigravityModelRedirectMaxEntries)
		}
	}
	return out, nil
}
