package admin

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"

	"github.com/gin-gonic/gin"
)

// Antigravity 渠道设置：
//   - 模型重定向：下游请求不带思考强度后缀的逻辑模型（gemini-3.8-flash）时，自动按
//     配置落到某个固定档位（gemini-3.8-flash-high）。默认只补位缺失的
//     reasoning.effort，开启覆盖后连显式 effort 也按重定向走。
//   - 思考内容下发：开启后 OAuth 账号的 Gemini thought 片段作为 reasoning 输出下发。
//   - 配额自动刷新与硬门禁：见 Spec §8，保存走整 JSON CAS（不读本机内存缓存做基准）。

// antigravityConfigSaveMaxRetries 是 CAS 冲突后的有界重试次数，超过返回 409（Spec §8.2 #2）。
const antigravityConfigSaveMaxRetries = 3

type antigravitySettingsResponse struct {
	ModelRedirects              map[string]string                 `json:"model_redirects"`
	RedirectOverridesEffort     bool                              `json:"redirect_overrides_effort"`
	ExposeThoughts              bool                              `json:"expose_thoughts"`
	QuotaRefreshEnabled         bool                              `json:"quota_refresh_enabled"`
	QuotaRefreshIntervalMinutes int                               `json:"quota_refresh_interval_minutes"`
	QuotaGateEnabled            bool                              `json:"quota_gate_enabled"`
	ExpiryPriorityEnabled       bool                              `json:"expiry_priority_enabled"`
	ExpiryPriorityHorizonHours  int                               `json:"expiry_priority_horizon_hours"`
	ConfigRevision              int64                             `json:"config_revision"`
	Choices                     []proxy.AntigravityRedirectChoice `json:"choices"`
}

func buildAntigravitySettingsResponse(settings auth.AntigravitySettings) antigravitySettingsResponse {
	redirects := settings.ModelRedirects
	if redirects == nil {
		redirects = map[string]string{}
	}
	return antigravitySettingsResponse{
		ModelRedirects:          redirects,
		RedirectOverridesEffort: settings.RedirectOverridesEffort,
		ExposeThoughts:          settings.ExposeThoughts,
		// 未配置的 quota 字段按默认值回退（Spec §8.2 GET）。
		QuotaRefreshEnabled:         settings.GetQuotaRefreshEnabled(),
		QuotaRefreshIntervalMinutes: settings.GetQuotaRefreshIntervalMinutes(),
		QuotaGateEnabled:            settings.GetQuotaGateEnabled(),
		ExpiryPriorityEnabled:       settings.GetExpiryPriorityEnabled(),
		ExpiryPriorityHorizonHours:  settings.GetExpiryPriorityHorizonHours(),
		ConfigRevision:              settings.ConfigRevision,
		Choices:                     proxy.AntigravityRedirectChoices(),
	}
}

// GetAntigravitySettings 返回当前 Antigravity 渠道设置与可选的重定向档位。
// GET /api/admin/settings/antigravity
func (h *Handler) GetAntigravitySettings(c *gin.Context) {
	c.JSON(http.StatusOK, buildAntigravitySettingsResponse(auth.ConfiguredAntigravitySettings()))
}

// antigravitySettingsPatch 保存 PUT 请求体的 presence 语义：字段缺失=保留现值，
// 显式 null=重置默认，出现即赋值（Spec §8.2 #1）。
type antigravitySettingsPatch struct {
	modelRedirects          *map[string]string
	redirectOverridesEffort *bool
	exposeThoughts          *bool

	quotaRefreshEnabled         *bool
	quotaRefreshEnabledPresent  bool
	quotaRefreshIntervalMinutes *int
	quotaRefreshIntervalPresent bool
	quotaGateEnabled            *bool
	quotaGateEnabledPresent     bool

	expiryPriorityEnabled        *bool
	expiryPriorityEnabledPresent bool
	expiryPriorityHorizonHours   *int
	expiryPriorityHorizonPresent bool
}

// empty 报告 patch 是否未包含任何更新字段。
func (p antigravitySettingsPatch) empty() bool {
	return p.modelRedirects == nil && p.redirectOverridesEffort == nil && p.exposeThoughts == nil &&
		!p.quotaRefreshEnabledPresent && !p.quotaRefreshIntervalPresent && !p.quotaGateEnabledPresent &&
		!p.expiryPriorityEnabledPresent && !p.expiryPriorityHorizonPresent
}

// isJSONNull 报告原始 JSON 字段是否显式为 null 字面量。
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// parseAntigravitySettingsPatch 解析请求体并做类型/范围校验，非法返回 error 由调用方转 400。
func parseAntigravitySettingsPatch(body []byte) (antigravitySettingsPatch, error) {
	var patch antigravitySettingsPatch
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return patch, fmt.Errorf("请求体解析失败: %w", err)
	}
	for key, raw := range fields {
		switch key {
		case "model_redirects":
			if isJSONNull(raw) {
				continue
			}
			var redirects map[string]string
			if err := json.Unmarshal(raw, &redirects); err != nil {
				return patch, fmt.Errorf("model_redirects 必须是对象: %v", err)
			}
			patch.modelRedirects = &redirects
		case "redirect_overrides_effort":
			if isJSONNull(raw) {
				continue
			}
			value, err := decodeAntigravityBool(raw, "redirect_overrides_effort")
			if err != nil {
				return patch, err
			}
			patch.redirectOverridesEffort = value
		case "expose_thoughts":
			if isJSONNull(raw) {
				continue
			}
			value, err := decodeAntigravityBool(raw, "expose_thoughts")
			if err != nil {
				return patch, err
			}
			patch.exposeThoughts = value
		case "quota_refresh_enabled":
			patch.quotaRefreshEnabledPresent = true
			if isJSONNull(raw) {
				patch.quotaRefreshEnabled = nil
				continue
			}
			value, err := decodeAntigravityBool(raw, "quota_refresh_enabled")
			if err != nil {
				return patch, err
			}
			patch.quotaRefreshEnabled = value
		case "quota_gate_enabled":
			patch.quotaGateEnabledPresent = true
			if isJSONNull(raw) {
				patch.quotaGateEnabled = nil
				continue
			}
			value, err := decodeAntigravityBool(raw, "quota_gate_enabled")
			if err != nil {
				return patch, err
			}
			patch.quotaGateEnabled = value
		case "expiry_priority_enabled":
			patch.expiryPriorityEnabledPresent = true
			if isJSONNull(raw) {
				patch.expiryPriorityEnabled = nil
				continue
			}
			value, err := decodeAntigravityBool(raw, "expiry_priority_enabled")
			if err != nil {
				return patch, err
			}
			patch.expiryPriorityEnabled = value
		case "expiry_priority_horizon_hours":
			patch.expiryPriorityHorizonPresent = true
			if isJSONNull(raw) {
				patch.expiryPriorityHorizonHours = nil
				continue
			}
			var hours int
			if err := json.Unmarshal(raw, &hours); err != nil {
				return patch, fmt.Errorf("expiry_priority_horizon_hours 必须是整数")
			}
			if hours < auth.AntigravityExpiryPriorityHorizonMinHours || hours > auth.AntigravityExpiryPriorityHorizonMaxHours {
				return patch, fmt.Errorf("expiry_priority_horizon_hours 必须在 [%d, %d] 之间",
					auth.AntigravityExpiryPriorityHorizonMinHours, auth.AntigravityExpiryPriorityHorizonMaxHours)
			}
			patch.expiryPriorityHorizonHours = &hours
		case "quota_refresh_interval_minutes":
			patch.quotaRefreshIntervalPresent = true
			if isJSONNull(raw) {
				patch.quotaRefreshIntervalMinutes = nil
				continue
			}
			var minutes int
			if err := json.Unmarshal(raw, &minutes); err != nil {
				return patch, fmt.Errorf("quota_refresh_interval_minutes 必须是整数")
			}
			if minutes < auth.AntigravityQuotaRefreshIntervalMinMinutes || minutes > auth.AntigravityQuotaRefreshIntervalMaxMinutes {
				return patch, fmt.Errorf("quota_refresh_interval_minutes 必须在 [%d, %d] 之间",
					auth.AntigravityQuotaRefreshIntervalMinMinutes, auth.AntigravityQuotaRefreshIntervalMaxMinutes)
			}
			patch.quotaRefreshIntervalMinutes = &minutes
		}
	}
	return patch, nil
}

// decodeAntigravityBool 解析 JSON 原始布尔字段，非布尔类型返回格式化错误。
func decodeAntigravityBool(raw json.RawMessage, field string) (*bool, error) {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s 必须是布尔值", field)
	}
	return &value, nil
}

// mergeAntigravitySettingsPatch 在 DB 当前配置上合并请求出现的字段（不读本机内存缓存
// 做基准）。混布窗口内旧二进制整包覆盖会抹掉 quota_*（Spec §3.1、§8.2 #5）：本机仍
// 显式持有的字段必须合并回来并告警，否则止血开关会被"缺失即默认 true"无痕掩盖。
func mergeAntigravitySettingsPatch(base auth.AntigravitySettings, patch antigravitySettingsPatch, held auth.AntigravitySettings) (auth.AntigravitySettings, bool) {
	merged := base
	if patch.modelRedirects != nil {
		redirects := make(map[string]string, len(*patch.modelRedirects))
		for key, value := range *patch.modelRedirects {
			redirects[key] = value
		}
		merged.ModelRedirects = redirects
	}
	if patch.redirectOverridesEffort != nil {
		merged.RedirectOverridesEffort = *patch.redirectOverridesEffort
	}
	if patch.exposeThoughts != nil {
		merged.ExposeThoughts = *patch.exposeThoughts
	}

	restore := auth.AntigravityQuotaFieldsLost(base, held)
	restored := false
	if patch.quotaRefreshEnabledPresent {
		merged.QuotaRefreshEnabled = patch.quotaRefreshEnabled
	} else if merged.QuotaRefreshEnabled == nil && restore.QuotaRefreshEnabled != nil {
		merged.QuotaRefreshEnabled = restore.QuotaRefreshEnabled
		restored = true
	}
	if patch.quotaRefreshIntervalPresent {
		merged.QuotaRefreshIntervalMinutes = patch.quotaRefreshIntervalMinutes
	} else if merged.QuotaRefreshIntervalMinutes == nil && restore.QuotaRefreshIntervalMinutes != nil {
		merged.QuotaRefreshIntervalMinutes = restore.QuotaRefreshIntervalMinutes
		restored = true
	}
	if patch.quotaGateEnabledPresent {
		merged.QuotaGateEnabled = patch.quotaGateEnabled
	} else if merged.QuotaGateEnabled == nil && restore.QuotaGateEnabled != nil {
		merged.QuotaGateEnabled = restore.QuotaGateEnabled
		restored = true
	}
	// 临期优先缺省关闭：混布窗口里被旧版本整包覆盖丢失只会回到「关」，不像门禁止血
	// 开关那样会被默认值无痕反转，因此不参与回写恢复。
	if patch.expiryPriorityEnabledPresent {
		merged.ExpiryPriorityEnabled = patch.expiryPriorityEnabled
	}
	if patch.expiryPriorityHorizonPresent {
		merged.ExpiryPriorityHorizonHours = patch.expiryPriorityHorizonHours
	}
	return merged, restored
}

// UpdateAntigravitySettings 保存 Antigravity 渠道设置。只更新请求里出现的字段；
// model_redirects 整体替换，值为空的条目表示取消该模型的重定向；quota 字段缺失保留、
// null 重置默认。保存采用"读 DB 原文 → Go 侧 merge → 整 JSON CAS"，冲突有界重试。
// PUT /api/admin/settings/antigravity
// {"model_redirects":{"gemini-3.8-flash":"gemini-3.8-flash-high"},"quota_gate_enabled":false,"quota_refresh_interval_minutes":15}
func (h *Handler) UpdateAntigravitySettings(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		writeError(c, http.StatusBadRequest, "请求体读取失败: "+err.Error())
		return
	}
	patch, err := parseAntigravitySettingsPatch(body)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if patch.empty() {
		writeError(c, http.StatusBadRequest, "缺少 model_redirects、redirect_overrides_effort、expose_thoughts、quota_refresh_enabled、quota_refresh_interval_minutes、quota_gate_enabled、expiry_priority_enabled 或 expiry_priority_horizon_hours 字段")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}

	ctx := c.Request.Context()
	held := auth.ConfiguredAntigravitySettings()
	var applied auth.AntigravitySettings
	for attempt := 0; ; attempt++ {
		oldRaw, err := h.db.GetAntigravityConfigRaw(ctx)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "读取配置失败: "+err.Error())
			return
		}
		base, err := auth.ParseAntigravitySettings(oldRaw)
		if err != nil {
			// 库里是不可解析的历史内容：以空配置为基准整包替换，CAS guard 仍然生效。
			log.Printf("antigravity_config 解析失败，按空配置合并并整包替换: %v", err)
			base = auth.AntigravitySettings{}
		}
		merged, restoredQuotaFields := mergeAntigravitySettingsPatch(base, patch, held)
		if restoredQuotaFields {
			log.Printf("antigravity_config_revision_rollback: DB 配置缺失本机仍生效的 quota_* 字段（疑似混布窗口旧版本整包覆盖），已以 CAS 基准合并回写")
		}
		// revision = max(DB 值, 本机已应用值) + 1：旧副本将 revision 归零后也不会生成被
		// consumer 拒旧的版本号（Spec §8.2 #5）。
		revision := merged.ConfigRevision
		if local := auth.AppliedAntigravityConfigRevision(); local > revision {
			revision = local
		}
		merged.ConfigRevision = revision + 1
		normalized, err := auth.NormalizeAntigravitySettings(merged)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		if err := proxy.ValidateAntigravityModelRedirects(normalized.ModelRedirects); err != nil {
			writeError(c, http.StatusBadRequest, "model_redirects 非法: "+err.Error())
			return
		}
		encoded, err := auth.EncodeAntigravitySettings(normalized)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		ok, err := h.db.SaveAntigravityConfigCAS(ctx, oldRaw, encoded)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		if ok {
			applied = normalized
			break
		}
		if attempt >= antigravityConfigSaveMaxRetries {
			writeError(c, http.StatusConflict, "配置正被其他副本并发修改，请重新读取后重试")
			return
		}
	}

	// 本机按 revision 单调应用（含 revision 回退检测与基线重建）。
	outcome := auth.ApplyConfiguredAntigravitySettings(applied)
	log.Printf("设置已更新: antigravity_config redirects=%d overrides_effort=%v expose_thoughts=%v quota_refresh_enabled=%v quota_refresh_interval_minutes=%d quota_gate_enabled=%v expiry_priority_enabled=%v expiry_priority_horizon_hours=%d config_revision=%d apply=%s",
		len(applied.ModelRedirects), applied.RedirectOverridesEffort, applied.ExposeThoughts,
		applied.GetQuotaRefreshEnabled(), applied.GetQuotaRefreshIntervalMinutes(), applied.GetQuotaGateEnabled(),
		applied.GetExpiryPriorityEnabled(), applied.GetExpiryPriorityHorizonHours(),
		applied.ConfigRevision, outcome)
	// quota_refresh_enabled / interval 可能已变化：立即唤醒配额巡检调度器按新配置重算槽位，
	// 不必等下一轮自然到点。
	if h.store != nil {
		h.store.WakeAntigravityQuotaScheduler()
	}
	c.JSON(http.StatusOK, buildAntigravitySettingsResponse(applied))
}
