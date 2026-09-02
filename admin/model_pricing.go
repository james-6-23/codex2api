package admin

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// modelVersionNumberRE 提取模型名中的数字段，用于版本排序（如 gpt-5.6-luna → 5,6）。
var modelVersionNumberRE = regexp.MustCompile(`\d+`)

// preferredBillingModelOrder 定价列表置顶顺序：gpt-5.6 sol → terra → luna。
var preferredBillingModelOrder = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
}

// modelPreferredRank 返回置顶优先级（越小越靠前）；非置顶模型返回 -1。
func modelPreferredRank(model string) int {
	lower := strings.ToLower(strings.TrimSpace(model))
	for i, preferred := range preferredBillingModelOrder {
		if lower == preferred {
			return i
		}
		// 变体后缀 / 思考强度别名：gpt-5.6-sol-high、gpt-5.6-sol(xhigh)
		if strings.HasPrefix(lower, preferred+"-") || strings.HasPrefix(lower, preferred+"(") {
			return i
		}
	}
	return -1
}

// modelVersionParts 从模型 ID 中取出数字序列，供新→旧排序使用。
func modelVersionParts(model string) []int {
	matches := modelVersionNumberRE.FindAllString(model, -1)
	if len(matches) == 0 {
		return nil
	}
	parts := make([]int, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m)
		if err != nil {
			continue
		}
		parts = append(parts, n)
	}
	return parts
}

// compareModelKeysNewestFirst 比较两个模型键：
// 1) 置顶模型（sol/terra/luna）固定靠前；
// 2) 其余按版本号从新到旧；同版本再按名称字典序。
// 返回 -1 表示 a 应排在 b 前面，1 表示 a 在 b 后面，0 表示相等。
func compareModelKeysNewestFirst(a, b string) int {
	if a == b {
		return 0
	}
	ra, rb := modelPreferredRank(a), modelPreferredRank(b)
	if ra >= 0 || rb >= 0 {
		if ra < 0 {
			return 1
		}
		if rb < 0 {
			return -1
		}
		if ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		// 同置顶组内按名称稳定排序（sol-high 跟在 sol 后等）。
		return strings.Compare(a, b)
	}

	va, vb := modelVersionParts(a), modelVersionParts(b)
	// 无版本号的模型沉底。
	if len(va) == 0 && len(vb) == 0 {
		return strings.Compare(a, b)
	}
	if len(va) == 0 {
		return 1
	}
	if len(vb) == 0 {
		return -1
	}
	n := len(va)
	if len(vb) < n {
		n = len(vb)
	}
	for i := 0; i < n; i++ {
		if va[i] != vb[i] {
			if va[i] > vb[i] {
				return -1
			}
			return 1
		}
	}
	// 公共前缀相同：段更多视为更高版本（5.6 > 5）。
	if len(va) != len(vb) {
		if len(va) > len(vb) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// sortModelKeysNewestFirst 将模型键按版本号从新到旧排序。
func sortModelKeysNewestFirst(keys []string) {
	sort.SliceStable(keys, func(i, j int) bool {
		return compareModelKeysNewestFirst(keys[i], keys[j]) < 0
	})
}

// grokBillingModelIDs 返回定价页要展示的 Grok 模型：各 Grok 账号声明白名单的并集，
// 外加默认集（只要存在未声明白名单的 Grok 账号——这类账号按默认集对外放行）。
// 没有 Grok 账号时返回空，纯 Codex 部署的定价页不受影响。
func (h *Handler) grokBillingModelIDs() []string {
	if h == nil || h.store == nil {
		return nil
	}
	ids := h.grokChannelModels()
	// 默认集按凭据类型不同（OAuth 走 CLI 通道、API Key 走公开 API），
	// 两种通道各取一次，不能见到第一个未声明账号就收工。
	oauthCovered, apiKeyCovered := false, false
	for _, account := range h.store.Accounts() {
		if !account.IsGrokAPI() || len(account.GrokModels()) > 0 {
			continue
		}
		isAPIKey := account.GrokAuthKind() == auth.GrokAuthKindAPIKey
		if (isAPIKey && apiKeyCovered) || (!isAPIKey && oauthCovered) {
			continue
		}
		if isAPIKey {
			apiKeyCovered = true
		} else {
			oauthCovered = true
		}
		ids = append(ids, proxy.DefaultGrokModelIDsForAccount(account)...)
		if oauthCovered && apiKeyCovered {
			break
		}
	}
	return ids
}

// grokDefaultDisplayModelIDs 是定价页始终展示的 Grok 内置文本模型集(即使没有 Grok 账号),
// 与 Codex 内置模型的常显行为对齐。取 OAuth 与 API Key 两套默认集的并集(后者为超集)。
// 仅文本模型:定价页按 token 计费,媒体(生图/生视频)定价模型另计,不在此列。
func grokDefaultDisplayModelIDs() []string {
	ids := make([]string, 0, 8)
	ids = append(ids, auth.GrokOAuthDefaultModelIDs()...)
	ids = append(ids, auth.GrokAPIKeyDefaultModelIDs()...)
	return ids
}

// modelPricingRow 是定价管理页每个规范模型的一行：当前生效价 + 来源。
type modelPricingRow struct {
	Model          string                        `json:"model"`
	Channel        string                        `json:"channel"` // codex / grok / antigravity / claude —— 供前端按 provider 分组
	Source         string                        `json:"source"`  // custom / synced / default
	Pricing        database.ModelPricingOverride `json:"pricing"`
	CanonicalModel string                        `json:"canonical_model,omitempty"`
	IsAlias        bool                          `json:"is_alias,omitempty"`
}

// claudeChannelModels 返回定价页要展示的 Claude 模型:各 Claude 账号可见模型的并集。
// 没有 Claude 账号时返回空,纯 Codex/其它部署的定价页不受影响。
func (h *Handler) claudeChannelModels() []string {
	if h == nil || h.store == nil {
		return nil
	}
	seen := make(map[string]struct{})
	models := make([]string, 0)
	for _, account := range h.store.Accounts() {
		if account == nil || !account.IsClaudeOAuth() {
			continue
		}
		for _, model := range proxy.DefaultClaudeModelIDsForAccount(account) {
			key := strings.ToLower(strings.TrimSpace(model))
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models
}

// claudeAvailableChannelModels returns models from enabled, non-banned Claude
// accounts for request-facing catalogs. Pricing/history still use
// claudeChannelModels so a disabled account cannot make an unusable model
// selectable while its historical cost data remains visible to administrators.
func (h *Handler) claudeAvailableChannelModels() []string {
	if h == nil || h.store == nil {
		return nil
	}
	seen := make(map[string]struct{})
	models := make([]string, 0)
	for _, account := range h.store.Accounts() {
		if account == nil || !account.IsClaudeOAuth() ||
			atomic.LoadInt32(&account.Disabled) != 0 ||
			atomic.LoadInt32(&account.DispatchPaused) != 0 {
			continue
		}
		account.Mu().RLock()
		status := account.Status
		tier := account.HealthTier
		account.Mu().RUnlock()
		if status == auth.StatusError || tier == auth.HealthTierBanned {
			continue
		}
		for _, model := range proxy.DefaultClaudeModelIDsForAccount(account) {
			model = strings.TrimSpace(model)
			key := strings.ToLower(model)
			if key == "" || account.IsModelRateLimited(model) {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models
}

func modelPricingManagementKeys(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		key := database.PricingManagementModelKey(id)
		if key == "" || strings.Contains(key, "(") {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// ListModelPricing 返回各规范模型的当前生效定价与来源，供设置页定价表展示。
func (h *Handler) ListModelPricing(c *gin.Context) {
	ctx := c.Request.Context()

	// 取当前对外暴露的模型，映射到规范定价键去重（退役模型自然被排除）。
	collect := func(ids []string) []string {
		return modelPricingManagementKeys(ids)
	}

	keys := collect(proxy.SupportedModelIDs(ctx, h.db))
	// Grok 模型不在 Codex 注册表里，但同样对外暴露、同样按 token 计费，
	// 单独并进来，否则定价页看不到 grok-4.5 这类模型。
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		seen[key] = struct{}{}
	}
	dedup := func(ids []string) []string {
		out := make([]string, 0)
		for _, key := range collect(ids) {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, key)
		}
		return out
	}
	// Grok 内置默认模型始终并入,使定价页像 Codex 内置模型一样常显 grok 家族,
	// 即使当前没有任何 Grok 账号(官方同步的 grok 采集逻辑不受影响)。
	grokKeys := dedup(append(h.grokBillingModelIDs(), grokDefaultDisplayModelIDs()...))
	antigravityKeys := dedup(h.antigravityChannelModels())
	claudeKeys := dedup(h.claudeChannelModels())

	// 每个渠道内按新版本在前排序；渠道之间整体拼接,避免版本号交叉穿插。
	sortModelKeysNewestFirst(keys)
	sortModelKeysNewestFirst(grokKeys)
	sortModelKeysNewestFirst(antigravityKeys)
	sortModelKeysNewestFirst(claudeKeys)

	rows := make([]modelPricingRow, 0, len(keys)+len(grokKeys)+len(antigravityKeys)+len(claudeKeys))
	appendRows := func(modelKeys []string, channel string) {
		for _, key := range modelKeys {
			canonicalModel := database.PricingAliasTarget(key)
			rows = append(rows, modelPricingRow{
				Model:          key,
				Channel:        channel,
				Source:         database.ModelPricingSourceFor(key),
				Pricing:        database.ModelPricingOverrideFromPricing(database.GetModelPricing(key), database.ModelPricingSourceFor(key)),
				CanonicalModel: canonicalModel,
				IsAlias:        canonicalModel != "",
			})
		}
	}
	appendRows(keys, database.UpstreamChannelCodex)
	appendRows(grokKeys, database.UpstreamChannelGrok)
	appendRows(antigravityKeys, database.UpstreamChannelAntigravity)
	appendRows(claudeKeys, database.UpstreamChannelClaude)

	syncURL := ""
	if s, err := h.db.GetSystemSettings(ctx); err == nil && s != nil {
		syncURL = strings.TrimSpace(s.ModelPricingSyncURL)
	}
	officialCfg, _ := h.db.GetOfficialPricingSyncConfig(ctx)
	c.JSON(http.StatusOK, gin.H{
		"models":               rows,
		"sync_url":             syncURL,
		"default_sync_url":     proxy.DefaultModelPricingSyncURL,
		"models_dev_url":       proxy.ModelsDevPricingSyncURL,
		"official_openai_url":  proxy.OfficialOpenAIPricingURL,
		"official_xai_url":     strings.TrimSuffix(proxy.OfficialXAIPricingURL, ".md"),
		"official_claude_url":  proxy.OfficialAnthropicPricingURL,
		"official_sync_config": officialPricingConfigResponse(officialCfg),
	})
}

// UpdateModelPricingRequest 设置/清除某模型的 custom 定价覆盖。
type UpdateModelPricingRequest struct {
	Model   string                         `json:"model"`
	Reset   bool                           `json:"reset"`   // true 时清除该模型覆盖，回退代码默认
	Pricing *database.ModelPricingOverride `json:"pricing"` // reset=false 时必填
}

// UpdateModelPricing 写入/清除某模型的 custom 定价覆盖。
func (h *Handler) UpdateModelPricing(c *gin.Context) {
	var req UpdateModelPricingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	key := database.PricingManagementModelKey(strings.TrimSpace(req.Model))
	if key == "" {
		writeError(c, http.StatusBadRequest, "model 不能为空")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if !req.Reset && (req.Pricing == nil || req.Pricing.IsEmpty()) {
		writeError(c, http.StatusBadRequest, "pricing 不能为空（或用 reset 清除）")
		return
	}
	_, err := h.db.MutateModelPricingSettings(ctx, nil, func(overrides map[string]database.ModelPricingOverride) error {
		if req.Reset {
			delete(overrides, key)
			return nil
		}
		ov := *req.Pricing
		ov.Source = database.ModelPricingSourceCustom
		overrides[key] = ov
		return nil
	})
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"model": key, "reset": req.Reset})
}

// SyncModelPricingRequest 可选携带一次性同步 URL（同时保存为默认来源）。
type SyncModelPricingRequest struct {
	URL string `json:"url"`
}

// SyncModelPricing 从 JSON URL 同步定价（synced 覆盖，不动 custom）。
func (h *Handler) SyncModelPricing(c *gin.Context) {
	var req SyncModelPricingRequest
	_ = c.ShouldBindJSON(&req)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	// 字段即准：直接传设置页 URL 字段的当前值（空 → 用内置默认并清空存储来源）。
	result, err := proxy.SyncModelPricingFromURL(ctx, h.db, req.URL, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}
