package admin

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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

// modelPricingRow 是定价管理页每个规范模型的一行：当前生效价 + 来源。
type modelPricingRow struct {
	Model   string                        `json:"model"`
	Source  string                        `json:"source"` // custom / synced / default
	Pricing database.ModelPricingOverride `json:"pricing"`
}

// ListModelPricing 返回各规范模型的当前生效定价与来源，供设置页定价表展示。
func (h *Handler) ListModelPricing(c *gin.Context) {
	ctx := c.Request.Context()

	// 取当前对外暴露的模型，映射到规范定价键去重（退役模型自然被排除）。
	seen := map[string]struct{}{}
	keys := make([]string, 0, 16)
	for _, id := range proxy.SupportedModelIDs(ctx, h.db) {
		key := database.CanonicalBillingModelKey(id)
		if key == "" || strings.Contains(key, "(") { // 跳过思考强度别名
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	// 新版本在前（gpt-5.6 > gpt-5.5 > gpt-5.4 …），避免字典序把旧模型顶到列表顶部。
	sortModelKeysNewestFirst(keys)

	rows := make([]modelPricingRow, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, modelPricingRow{
			Model:   key,
			Source:  database.ModelPricingSourceFor(key),
			Pricing: database.ModelPricingOverrideFromPricing(database.GetModelPricing(key), database.ModelPricingSourceFor(key)),
		})
	}

	syncURL := ""
	if s, err := h.db.GetSystemSettings(ctx); err == nil && s != nil {
		syncURL = strings.TrimSpace(s.ModelPricingSyncURL)
	}
	c.JSON(http.StatusOK, gin.H{
		"models":           rows,
		"sync_url":         syncURL,
		"default_sync_url": proxy.DefaultModelPricingSyncURL,
		"models_dev_url":   proxy.ModelsDevPricingSyncURL,
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
	key := database.CanonicalBillingModelKey(strings.TrimSpace(req.Model))
	if key == "" {
		writeError(c, http.StatusBadRequest, "model 不能为空")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if settings == nil {
		settings = &database.SystemSettings{}
	}
	overrides, err := database.ParseModelPricingOverridesJSON(settings.ModelPricingOverrides)
	if err != nil {
		overrides = map[string]database.ModelPricingOverride{}
	}

	if req.Reset {
		delete(overrides, key)
	} else {
		if req.Pricing == nil || req.Pricing.IsEmpty() {
			writeError(c, http.StatusBadRequest, "pricing 不能为空（或用 reset 清除）")
			return
		}
		ov := *req.Pricing
		ov.Source = database.ModelPricingSourceCustom
		overrides[key] = ov
	}

	blob, err := database.MarshalModelPricingOverridesJSON(overrides)
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.db.UpdateModelPricingSettings(ctx, blob, settings.ModelPricingSyncURL); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	database.SetModelPricingOverrides(overrides)
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
