package database

import "strings"

const longContextThreshold = 272000

type ModelPricing struct {
	UserBillingMode string
	ImageUnitPrice  float64
	// MediaUnitCost 是媒体模型的上游单位成本(USD/张 或 USD/秒,见 MediaBillingUnit)。
	MediaUnitCost                   float64
	ImageInputPricePerMToken        float64
	CacheReadImagePricePerMToken    float64
	InputPricePerMToken             float64
	InputPricePerMTokenPriority     float64
	OutputPricePerMToken            float64
	OutputPricePerMTokenPriority    float64
	CacheReadPricePerMToken         float64
	CacheReadPricePerMTokenPriority float64
	// CacheWrite* are Anthropic prompt-cache creation prices (USD / 1M tokens).
	// They are surfaced for transparent pricing, while cost calculation continues
	// to use CacheReadPricePerMToken for cached input tokens.
	CacheWrite5mPricePerMToken float64
	CacheWrite1hPricePerMToken float64

	LongInputPricePerMToken             float64
	LongInputPricePerMTokenPriority     float64
	LongOutputPricePerMToken            float64
	LongOutputPricePerMTokenPriority    float64
	LongCacheReadPricePerMToken         float64
	LongCacheReadPricePerMTokenPriority float64

	// LongContextThresholdTokens 是该模型进入长上下文档位的输入 token 阈值。
	// 留空用全局 longContextThreshold（OpenAI 的 272K）；xAI Grok 的分档线是 200K，
	// 需在规则里单独声明，否则 200K~272K 区间会按短档少算。
	LongContextThresholdTokens int

	// UltrafastMultiplier 是模型自有的 Ultrafast 档倍率(相对 standard 价)。
	// 大于 0 时 ultrafast 请求按 standard 价乘此倍率计费,不走 Fast 的 2× 与
	// priority 价;为 0 的模型 ultrafast 继续跟随 Fast 策略。不参与价格覆盖。
	UltrafastMultiplier float64
}

type modelPricingRule struct {
	model   string
	pricing ModelPricing
}

type CostBreakdown struct {
	ImageInputCost               float64 `json:"image_input_cost"`
	ImageCacheReadCost           float64 `json:"image_cache_read_cost"`
	ImageInputPricePerMToken     float64 `json:"image_input_price_per_mtoken"`
	CacheReadImagePricePerMToken float64 `json:"cached_image_input_price_per_mtoken"`
	InputCost                    float64 `json:"input_cost"`
	OutputCost                   float64 `json:"output_cost"`
	CacheReadCost                float64 `json:"cache_read_cost"`
	TotalCost                    float64 `json:"total_cost"`
	InputPricePerMToken          float64 `json:"input_price_per_mtoken"`
	OutputPricePerMToken         float64 `json:"output_price_per_mtoken"`
	CacheReadPricePerMToken      float64 `json:"cache_read_price_per_mtoken"`
	CacheWrite5mCost             float64 `json:"cache_write_5m_cost"`
	CacheWrite1hCost             float64 `json:"cache_write_1h_cost"`
	CacheWrite5mPricePerMToken   float64 `json:"cache_write_5m_price_per_mtoken"`
	CacheWrite1hPricePerMToken   float64 `json:"cache_write_1h_price_per_mtoken"`
	ServiceTierCostMultiplier    float64 `json:"service_tier_cost_multiplier"`
	LongContext                  bool    `json:"long_context"`
	LongContextThreshold         int     `json:"long_context_threshold"`
}

var (
	defaultModelPricing = &ModelPricing{InputPricePerMToken: 1.0, OutputPricePerMToken: 2.0}

	modelPricingRules = []modelPricingRule{
		// GPT-6 Sol/Luna 使用官方标准价和超过 272K 的长上下文价，fast 档沿用 2×。
		{model: "gpt-6-sol", pricing: ModelPricing{
			InputPricePerMToken:         2.0,
			OutputPricePerMToken:        10.0,
			CacheReadPricePerMToken:     0.2,
			LongInputPricePerMToken:     4.0,
			LongOutputPricePerMToken:    15.0,
			LongCacheReadPricePerMToken: 0.4,
		}},
		// gpt-6.1-sol 是独立型号：缓存读取 $0.10、缓存写入 $2.50，
		// 超过 272K 按输入/缓存 2×、输出 1.5×。不能落入未知 gpt-6 变体的 Astra 兜底。
		{model: "gpt-6.1-sol", pricing: ModelPricing{
			InputPricePerMToken:         2.0,
			OutputPricePerMToken:        10.0,
			CacheReadPricePerMToken:     0.1,
			CacheWrite5mPricePerMToken:  2.5,
			CacheWrite1hPricePerMToken:  2.5,
			LongInputPricePerMToken:     4.0,
			LongOutputPricePerMToken:    15.0,
			LongCacheReadPricePerMToken: 0.2,
		}},
		// gpt-6.1-terra 官方标准价 $1/$5、缓存读取 $0.05、缓存写入 $1.25，
		// 超过 272K 为 $2/$7.50、缓存 $0.10；fast 档沿用 2×。
		{model: "gpt-6.1-terra", pricing: ModelPricing{
			InputPricePerMToken:         1.0,
			OutputPricePerMToken:        5.0,
			CacheReadPricePerMToken:     0.05,
			CacheWrite5mPricePerMToken:  1.25,
			CacheWrite1hPricePerMToken:  1.25,
			LongInputPricePerMToken:     2.0,
			LongOutputPricePerMToken:    7.5,
			LongCacheReadPricePerMToken: 0.1,
		}},
		{model: "gpt-6-luna", pricing: ModelPricing{
			InputPricePerMToken:         0.1,
			OutputPricePerMToken:        0.5,
			CacheReadPricePerMToken:     0.01,
			LongInputPricePerMToken:     0.2,
			LongOutputPricePerMToken:    0.75,
			LongCacheReadPricePerMToken: 0.02,
		}},
		// gpt-6-astra：超过 272K 按长上下文价计费（输入/缓存 2×、输出 1.5×），
		// 即 $20/$75、缓存 $2。fast（priority）2× 倍率由 serviceTierCostMultiplier 兜底；
		// Ultrafast 档按所处档位 standard 价的 6× 计费。
		{model: "gpt-6-astra", pricing: ModelPricing{
			InputPricePerMToken:         10.0,
			OutputPricePerMToken:        50.0,
			CacheReadPricePerMToken:     1.0,
			LongInputPricePerMToken:     20.0,
			LongOutputPricePerMToken:    75.0,
			LongCacheReadPricePerMToken: 2.0,
			UltrafastMultiplier:         6.0,
		}},
		{model: "gpt-5.5", pricing: ModelPricing{
			InputPricePerMToken:                 5.0,
			InputPricePerMTokenPriority:         12.5,
			OutputPricePerMToken:                30.0,
			OutputPricePerMTokenPriority:        75.0,
			CacheReadPricePerMToken:             0.5,
			CacheReadPricePerMTokenPriority:     1.25,
			LongInputPricePerMToken:             10.0,
			LongInputPricePerMTokenPriority:     25.0,
			LongOutputPricePerMToken:            45.0,
			LongOutputPricePerMTokenPriority:    112.5,
			LongCacheReadPricePerMToken:         1.0,
			LongCacheReadPricePerMTokenPriority: 2.5,
		}},
		{model: "gpt-5.5-pro", pricing: ModelPricing{
			InputPricePerMToken:              30.0,
			InputPricePerMTokenPriority:      75.0,
			OutputPricePerMToken:             180.0,
			OutputPricePerMTokenPriority:     450.0,
			LongInputPricePerMToken:          60.0,
			LongInputPricePerMTokenPriority:  150.0,
			LongOutputPricePerMToken:         270.0,
			LongOutputPricePerMTokenPriority: 675.0,
		}},
		// gpt-5.6-sol: standard 同 gpt-5.5，但 priority 为 2× standard（$10/$60），
		// 低于 gpt-5.5 的 2.5×，故不复用 gpt-5.5 条目。priority 留空由 fast 档兜底 2×。
		{model: "gpt-5.6-sol", pricing: ModelPricing{
			InputPricePerMToken:         5.0,
			OutputPricePerMToken:        30.0,
			CacheReadPricePerMToken:     0.5,
			LongInputPricePerMToken:     10.0,
			LongOutputPricePerMToken:    45.0,
			LongCacheReadPricePerMToken: 1.0,
		}},
		// gpt-5.6-terra: 2026-07-30 官方降价 20%（$2/$12，原 $2.5/$15），priority 2×；
		// 独立规范键，便于定价页单独覆盖，不与 gpt-5.4 互相污染。
		{model: "gpt-5.6-terra", pricing: ModelPricing{
			InputPricePerMToken:                 2.0,
			InputPricePerMTokenPriority:         4.0,
			OutputPricePerMToken:                12.0,
			OutputPricePerMTokenPriority:        24.0,
			CacheReadPricePerMToken:             0.2,
			CacheReadPricePerMTokenPriority:     0.4,
			LongInputPricePerMToken:             4.0,
			LongInputPricePerMTokenPriority:     8.0,
			LongOutputPricePerMToken:            18.0,
			LongOutputPricePerMTokenPriority:    36.0,
			LongCacheReadPricePerMToken:         0.4,
			LongCacheReadPricePerMTokenPriority: 0.8,
		}},
		// gpt-5.6-luna: 2026-07-30 官方降价 80%（$0.20/$1.20，原 $1/$6）。
		// priority 均为 2× standard，由 fast 档兜底自动得出。
		{model: "gpt-5.6-luna", pricing: ModelPricing{
			InputPricePerMToken:         0.2,
			OutputPricePerMToken:        1.2,
			CacheReadPricePerMToken:     0.02,
			LongInputPricePerMToken:     0.4,
			LongOutputPricePerMToken:    1.8,
			LongCacheReadPricePerMToken: 0.04,
		}},
		{model: "gpt-5.4-mini", pricing: ModelPricing{InputPricePerMToken: 0.75, OutputPricePerMToken: 4.5, CacheReadPricePerMToken: 0.075}},
		{model: "gpt-5.4-nano", pricing: ModelPricing{InputPricePerMToken: 0.2, OutputPricePerMToken: 1.25, CacheReadPricePerMToken: 0.02}},
		{model: "gpt-5.4", pricing: ModelPricing{
			InputPricePerMToken:                 2.5,
			InputPricePerMTokenPriority:         5.0,
			OutputPricePerMToken:                15.0,
			OutputPricePerMTokenPriority:        30.0,
			CacheReadPricePerMToken:             0.25,
			CacheReadPricePerMTokenPriority:     0.5,
			LongInputPricePerMToken:             5.0,
			LongInputPricePerMTokenPriority:     10.0,
			LongOutputPricePerMToken:            22.5,
			LongOutputPricePerMTokenPriority:    45.0,
			LongCacheReadPricePerMToken:         0.5,
			LongCacheReadPricePerMTokenPriority: 1.0,
		}},
		{model: "gpt-5.4-pro", pricing: ModelPricing{
			InputPricePerMToken:              30.0,
			InputPricePerMTokenPriority:      75.0,
			OutputPricePerMToken:             180.0,
			OutputPricePerMTokenPriority:     450.0,
			LongInputPricePerMToken:          60.0,
			LongInputPricePerMTokenPriority:  150.0,
			LongOutputPricePerMToken:         270.0,
			LongOutputPricePerMTokenPriority: 675.0,
		}},
		{model: "gpt-5.3-codex-spark", pricing: ModelPricing{
			InputPricePerMToken:             1.25,
			InputPricePerMTokenPriority:     2.5,
			OutputPricePerMToken:            10.0,
			OutputPricePerMTokenPriority:    20.0,
			CacheReadPricePerMToken:         0.125,
			CacheReadPricePerMTokenPriority: 0.25,
		}},
		{model: "gpt-5.3-codex", pricing: ModelPricing{
			InputPricePerMToken:             1.75,
			InputPricePerMTokenPriority:     3.5,
			OutputPricePerMToken:            14.0,
			OutputPricePerMTokenPriority:    28.0,
			CacheReadPricePerMToken:         0.175,
			CacheReadPricePerMTokenPriority: 0.35,
		}},
		{model: "gpt-5.2", pricing: ModelPricing{
			InputPricePerMToken:             1.75,
			InputPricePerMTokenPriority:     3.5,
			OutputPricePerMToken:            14.0,
			OutputPricePerMTokenPriority:    28.0,
			CacheReadPricePerMToken:         0.175,
			CacheReadPricePerMTokenPriority: 0.35,
		}},
		// ===== xAI Grok（USD / 1M token）=====
		// 新型号以内置的 xAI 官方价格快照为准；官方页面未收录的老型号
		// （grok-3-fast / grok-2）保留既有公开价，缓存价未公开的留空
		// （留空 = 缓存 token 按输入价计）。任何一条都可在定价页覆盖。
		// Grok 的长上下文分档线是 200K，与 OpenAI 的 272K 不同，逐条声明。
		// grok-4.7 / grok-4.6 必须独立成条：否则会命中 grok-4 前缀被当成 $3/$15。
		// 两代同价：短档 $2/$6、缓存 $0.50；≥200K 长档 $4/$12、缓存 $1.00（与 grok-4.5 同输入/输出，缓存更高）。
		// grok-4.7 价格来源 models.dev xai/grok-4.7（2026-09-21 发布，上下文 500K）。
		{model: "grok-4.7", pricing: ModelPricing{
			InputPricePerMToken:         2.0,
			OutputPricePerMToken:        6.0,
			CacheReadPricePerMToken:     0.5,
			LongInputPricePerMToken:     4.0,
			LongOutputPricePerMToken:    12.0,
			LongCacheReadPricePerMToken: 1.0,
			LongContextThresholdTokens:  200000,
		}},
		{model: "grok-4.6", pricing: ModelPricing{
			InputPricePerMToken:         2.0,
			OutputPricePerMToken:        6.0,
			CacheReadPricePerMToken:     0.5,
			LongInputPricePerMToken:     4.0,
			LongOutputPricePerMToken:    12.0,
			LongCacheReadPricePerMToken: 1.0,
			LongContextThresholdTokens:  200000,
		}},
		{model: "grok-4.5", pricing: ModelPricing{
			InputPricePerMToken:         2.0,
			OutputPricePerMToken:        6.0,
			CacheReadPricePerMToken:     0.3,
			LongInputPricePerMToken:     4.0,
			LongOutputPricePerMToken:    12.0,
			LongCacheReadPricePerMToken: 1.0,
			LongContextThresholdTokens:  200000,
		}},
		{model: "grok-4.3", pricing: ModelPricing{
			InputPricePerMToken:         1.25,
			OutputPricePerMToken:        2.5,
			CacheReadPricePerMToken:     0.2,
			LongInputPricePerMToken:     2.5,
			LongOutputPricePerMToken:    5.0,
			LongCacheReadPricePerMToken: 0.4,
			LongContextThresholdTokens:  200000,
		}},
		{model: "grok-4.20", pricing: ModelPricing{
			InputPricePerMToken:         1.25,
			OutputPricePerMToken:        2.5,
			CacheReadPricePerMToken:     0.2,
			LongInputPricePerMToken:     2.5,
			LongOutputPricePerMToken:    5.0,
			LongCacheReadPricePerMToken: 0.4,
			LongContextThresholdTokens:  200000,
		}},
		{model: "grok-4-fast", pricing: ModelPricing{InputPricePerMToken: 0.2, OutputPricePerMToken: 0.5, CacheReadPricePerMToken: 0.05}},
		{model: "grok-4", pricing: ModelPricing{InputPricePerMToken: 3.0, OutputPricePerMToken: 15.0, CacheReadPricePerMToken: 0.75}},
		{model: "grok-code-fast-1", pricing: ModelPricing{InputPricePerMToken: 0.2, OutputPricePerMToken: 1.5, CacheReadPricePerMToken: 0.02}},
		{model: "grok-3-mini", pricing: ModelPricing{InputPricePerMToken: 0.3, OutputPricePerMToken: 0.5, CacheReadPricePerMToken: 0.075}},
		{model: "grok-3-fast", pricing: ModelPricing{InputPricePerMToken: 5.0, OutputPricePerMToken: 25.0}},
		{model: "grok-3", pricing: ModelPricing{InputPricePerMToken: 3.0, OutputPricePerMToken: 15.0, CacheReadPricePerMToken: 0.75}},
		{model: "grok-2", pricing: ModelPricing{InputPricePerMToken: 2.0, OutputPricePerMToken: 10.0}},

		// ===== Google Gemini public API estimates (USD / 1M token) =====
		{model: "gemini-4-argon", pricing: ModelPricing{InputPricePerMToken: 1.25, OutputPricePerMToken: 10.0, CacheReadPricePerMToken: 0.125}},
		{model: "gemini-3.8-flash", pricing: ModelPricing{InputPricePerMToken: 0.75, OutputPricePerMToken: 3.75, CacheReadPricePerMToken: 0.075}},
		{model: "gemini-3.7-flash", pricing: ModelPricing{InputPricePerMToken: 0.75, OutputPricePerMToken: 3.75, CacheReadPricePerMToken: 0.075}},
		{model: "gemini-3.6-flash", pricing: ModelPricing{InputPricePerMToken: 0.75, OutputPricePerMToken: 3.75, CacheReadPricePerMToken: 0.075}},
		{model: "gemini-3.5-flash-lite", pricing: ModelPricing{InputPricePerMToken: 0.25, OutputPricePerMToken: 1.5, CacheReadPricePerMToken: 0.025}},
		{model: "gemini-3.1-flash-lite", pricing: ModelPricing{InputPricePerMToken: 0.25, OutputPricePerMToken: 1.5, CacheReadPricePerMToken: 0.025}},
		{model: "gemini-3-flash", pricing: ModelPricing{InputPricePerMToken: 0.5, OutputPricePerMToken: 3.0, CacheReadPricePerMToken: 0.05}},
		{model: "gemini-3-pro-preview", pricing: ModelPricing{InputPricePerMToken: 2.0, OutputPricePerMToken: 12.0, CacheReadPricePerMToken: 0.2}},
		{model: "gemini-2.5-pro", pricing: ModelPricing{InputPricePerMToken: 1.25, OutputPricePerMToken: 10.0, CacheReadPricePerMToken: 0.125}},
		{model: "gemini-2.5-flash", pricing: ModelPricing{InputPricePerMToken: 0.3, OutputPricePerMToken: 2.5, CacheReadPricePerMToken: 0.03}},

		{model: "gpt-4o-mini", pricing: ModelPricing{InputPricePerMToken: 0.15, OutputPricePerMToken: 0.6}},
		{model: "gpt-4o", pricing: ModelPricing{InputPricePerMToken: 2.5, OutputPricePerMToken: 10.0}},
		{model: "gpt-4-turbo", pricing: ModelPricing{InputPricePerMToken: 10.0, OutputPricePerMToken: 30.0}},
		{model: "gpt-4", pricing: ModelPricing{InputPricePerMToken: 30.0, OutputPricePerMToken: 60.0}},
		{model: "gpt-3.5-turbo", pricing: ModelPricing{InputPricePerMToken: 0.5, OutputPricePerMToken: 1.5}},
	}
)

func GetModelPricing(model string) *ModelPricing {
	normalized := normalizeBillingModelName(model)
	if alias := daybreakPricingAliasKey(model); alias != "" && alias != normalized {
		return daybreakAliasPricing(normalized, alias)
	}
	canonical := CanonicalBillingModelKey(model)
	base := baseModelPricing(normalized, canonical)
	// 新型号沿用兜底基础价，但覆盖必须独立，不能继承另一个型号的手工价格。
	if key := discoveredGPTPricingKey(normalized); key != "" {
		canonical = key
		if override, ok := lookupModelPricingOverride(key); ok && override.Input > 0 && override.Output > 0 {
			base = &ModelPricing{}
		}
	}

	// custom / synced 覆盖：以代码默认为底，合并非 0 字段（部分覆盖）。
	// 覆盖表拷贝到本地副本再改，绝不改动共享的默认 pricing 指针。
	//
	// An explicit alias entry is more specific than the canonical model entry.
	// This matters for internal aliases such as codex-auto-review: it maps to
	// gpt-5.4 for fallback pricing, but its own price must not be shadowed by a
	// stale gpt-5.4 override. 两条护栏维持文档化的 custom > synced > 默认次序:
	//   - synced 别名条目不得压过 custom canonical 条目,否则同步价会在升级后
	//     静默替换管理员手填价;
	//   - 别名条目未填的字段回退 canonical 生效价而不是代码默认价,部分覆盖
	//     不会悄悄丢掉管理员在 canonical 上配好的其余字段。
	canonicalOv, hasCanonical := lookupModelPricingOverride(canonical)
	var aliasOv ModelPricingOverride
	hasAlias := false
	aliasKey := PricingManagementModelKey(model)
	if aliasKey != "" && aliasKey != canonical {
		if ov, ok := lookupModelPricingOverride(aliasKey); ok {
			if ov.Source == ModelPricingSourceCustom || !hasCanonical || canonicalOv.Source != ModelPricingSourceCustom {
				aliasOv, hasAlias = ov, true
			}
		}
	}
	if !hasCanonical && !hasAlias {
		return base
	}
	merged := *base
	if hasCanonical {
		canonicalOv.applyNonZero(&merged)
	}
	if hasAlias {
		aliasOv.applyNonZero(&merged)
	}
	return &merged
}

// baseModelPricing 返回代码内置的模型定价（不含覆盖）。normalized 为归一化后的模型名，
// canonical 为 codex 归一后的规范名（用于规则表查找）。
func baseModelPricing(normalized, canonical string) *ModelPricing {
	if pricing := cyberDefaultPricing(canonical); pricing != nil {
		return pricing
	}
	if key := GrokMediaPricingKey(normalized); key != "" {
		return grokMediaBasePricing(key)
	}
	if GPTImage25BillingModel(canonical) != "" {
		return &gptImage25Pricing
	}
	if pricing := claudeFamilyPricing(normalized); pricing != nil {
		return pricing
	}
	if pricing := geminiFamilyPricing(normalized); pricing != nil {
		return pricing
	}
	if pricing := modelRulePricing(canonical); pricing != nil {
		return pricing
	}
	return defaultModelPricing
}

func CalculateCost(inputTokens, outputTokens, cachedTokens int, model string, serviceTier string) float64 {
	return CalculateCostBreakdown(inputTokens, outputTokens, cachedTokens, model, serviceTier).TotalCost
}

// usageLogBillingServiceTier 解析一条待写入用量事件的计费 service tier:
// 显式 BillingServiceTier 优先,其次上游实际 tier,最后请求 tier。
func usageLogBillingServiceTier(log *UsageLogInput) string {
	if log == nil {
		return ""
	}
	if tier := log.BillingServiceTier; tier != "" {
		return tier
	}
	if tier := log.ActualServiceTier; tier != "" {
		return tier
	}
	return log.ServiceTier
}

// UsageLogBilledCost returns upstream token cost. User quotas and scope budgets
// must use UsageLogUserBilledCost, which also supports per-image fees.
func UsageLogBilledCost(log *UsageLogInput) float64 {
	if log == nil {
		return 0
	}
	if log.billingSnapshot != nil {
		return log.billingSnapshot.accountCost
	}
	return UsageLogCostBreakdown(log).TotalCost
}

func CalculateCostBreakdown(inputTokens, outputTokens, cachedTokens int, model string, serviceTier string) CostBreakdown {
	return CalculateCostBreakdownWithCacheWrites(inputTokens, outputTokens, cachedTokens, 0, 0, model, serviceTier)
}

// CalculateCostBreakdownWithCacheWrites 在 CalculateCostBreakdown 的基础上计入 Anthropic
// 提示缓存写入（5 分钟 / 1 小时）。inputTokens 是总输入（未缓存 + 缓存命中 + 缓存写入），
// 写入价缺省按输入价的 1.25 倍 / 2 倍。
func CalculateCostBreakdownWithCacheWrites(inputTokens, outputTokens, cachedTokens, cacheWrite5mTokens, cacheWrite1hTokens int, model string, serviceTier string) CostBreakdown {
	pricing := GetModelPricing(model)
	if GPTImage25BillingModel(model) != "" {
		return imageTokenCostBreakdown(inputTokens, outputTokens, cachedTokens, 0, 0, 0, pricing)
	}
	threshold := longContextThreshold
	if pricing.LongContextThresholdTokens > 0 {
		threshold = pricing.LongContextThresholdTokens
	}
	isLong := inputTokens >= threshold
	longContextApplied := false

	inputPrice := pricing.InputPricePerMToken
	outputPrice := pricing.OutputPricePerMToken
	cacheReadPrice := pricing.CacheReadPricePerMToken

	if isLong && pricing.LongInputPricePerMToken > 0 {
		longContextApplied = true
		inputPrice = pricing.LongInputPricePerMToken
		outputPrice = pricing.LongOutputPricePerMToken
		if pricing.LongCacheReadPricePerMToken > 0 {
			cacheReadPrice = pricing.LongCacheReadPricePerMToken
		}
	}

	tierMultiplier := serviceTierCostMultiplier(serviceTier)
	if normalizeServiceTier(serviceTier) == "ultrafast" && pricing.UltrafastMultiplier > 0 {
		tierMultiplier = pricing.UltrafastMultiplier
	} else if usePriorityPricing(serviceTier, pricing) {
		tierMultiplier = 1
		if isLong && pricing.LongInputPricePerMTokenPriority > 0 {
			inputPrice = pricing.LongInputPricePerMTokenPriority
		} else if pricing.InputPricePerMTokenPriority > 0 {
			inputPrice = pricing.InputPricePerMTokenPriority
		}
		if isLong && pricing.LongOutputPricePerMTokenPriority > 0 {
			outputPrice = pricing.LongOutputPricePerMTokenPriority
		} else if pricing.OutputPricePerMTokenPriority > 0 {
			outputPrice = pricing.OutputPricePerMTokenPriority
		}
		if isLong && pricing.LongCacheReadPricePerMTokenPriority > 0 {
			cacheReadPrice = pricing.LongCacheReadPricePerMTokenPriority
		} else if pricing.CacheReadPricePerMTokenPriority > 0 {
			cacheReadPrice = pricing.CacheReadPricePerMTokenPriority
		}
	}

	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > inputTokens {
		cachedTokens = inputTokens
	}
	if cacheWrite5mTokens < 0 {
		cacheWrite5mTokens = 0
	}
	if cacheWrite1hTokens < 0 {
		cacheWrite1hTokens = 0
	}
	cacheWrite5mPrice := pricing.CacheWrite5mPricePerMToken
	if cacheWrite5mPrice <= 0 {
		cacheWrite5mPrice = inputPrice * 1.25
	}
	cacheWrite1hPrice := pricing.CacheWrite1hPricePerMToken
	if cacheWrite1hPrice <= 0 {
		cacheWrite1hPrice = inputPrice * 2
	}

	uncachedInputTokens := inputTokens
	if cacheReadPrice > 0 {
		uncachedInputTokens = inputTokens - cachedTokens
	}
	uncachedInputTokens -= cacheWrite5mTokens + cacheWrite1hTokens
	if uncachedInputTokens < 0 {
		uncachedInputTokens = 0
	}

	inputCost := float64(uncachedInputTokens) / 1000000.0 * inputPrice
	cacheReadCost := float64(cachedTokens) / 1000000.0 * cacheReadPrice
	cacheWrite5mCost := float64(cacheWrite5mTokens) / 1000000.0 * cacheWrite5mPrice
	cacheWrite1hCost := float64(cacheWrite1hTokens) / 1000000.0 * cacheWrite1hPrice
	outputCost := float64(outputTokens) / 1000000.0 * outputPrice

	return CostBreakdown{
		InputCost:                  inputCost * tierMultiplier,
		OutputCost:                 outputCost * tierMultiplier,
		CacheReadCost:              cacheReadCost * tierMultiplier,
		CacheWrite5mCost:           cacheWrite5mCost * tierMultiplier,
		CacheWrite1hCost:           cacheWrite1hCost * tierMultiplier,
		TotalCost:                  (inputCost + cacheReadCost + cacheWrite5mCost + cacheWrite1hCost + outputCost) * tierMultiplier,
		InputPricePerMToken:        inputPrice * tierMultiplier,
		OutputPricePerMToken:       outputPrice * tierMultiplier,
		CacheReadPricePerMToken:    cacheReadPrice * tierMultiplier,
		CacheWrite5mPricePerMToken: cacheWrite5mPrice * tierMultiplier,
		CacheWrite1hPricePerMToken: cacheWrite1hPrice * tierMultiplier,
		ServiceTierCostMultiplier:  tierMultiplier,
		LongContext:                longContextApplied,
		LongContextThreshold:       threshold,
	}
}

func normalizeBillingModelName(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimLeft(model, "/")
	model = strings.TrimPrefix(model, "models/")
	model = strings.TrimPrefix(model, "publishers/google/models/")
	if idx := strings.LastIndex(model, "/publishers/google/models/"); idx != -1 {
		model = model[idx+len("/publishers/google/models/"):]
	}
	if idx := strings.LastIndex(model, "/models/"); idx != -1 {
		model = model[idx+len("/models/"):]
	} else if idx := strings.LastIndex(model, "/"); idx != -1 {
		model = model[idx+1:]
	}
	// Daybreak 的规范计费模型仍是基础模型，专属价格覆盖由别名键处理。
	model = strings.TrimSuffix(strings.TrimSuffix(model, "-daybreak-blue"), "-daybreak-red")
	return strings.TrimLeft(model, "/")
}

func normalizeCodexBillingModel(model string) (string, bool) {
	compact := strings.NewReplacer(" ", "-", "_", "-").Replace(strings.ToLower(model))
	if target := daybreakCanonicalModel(compact); target != "" {
		return target, true
	}
	switch {
	case strings.HasPrefix(compact, "gpt-6-sol") || strings.HasPrefix(compact, "gpt6-sol"):
		return "gpt-6-sol", true
	case strings.HasPrefix(compact, "gpt-6.1-sol") || strings.HasPrefix(compact, "gpt6.1-sol"):
		return "gpt-6.1-sol", true
	case strings.HasPrefix(compact, "gpt-6.1-terra") || strings.HasPrefix(compact, "gpt6.1-terra"):
		return "gpt-6.1-terra", true
	case strings.HasPrefix(compact, "gpt-6-luna") || strings.HasPrefix(compact, "gpt6-luna"):
		return "gpt-6-luna", true
	// 未知 gpt-6 变体按 astra 兜底，避免掉进 $1/$2 的默认价严重低估。
	// 只认 gpt-6- / gpt-6. / 裸 gpt-6 前缀，gpt-5.6 不含 "gpt-6" 不会误命中。
	case strings.HasPrefix(compact, "gpt-6-") || strings.HasPrefix(compact, "gpt-6.") || compact == "gpt-6" ||
		strings.HasPrefix(compact, "gpt6-") || strings.HasPrefix(compact, "gpt6.") || compact == "gpt6":
		return "gpt-6-astra", true
	case strings.Contains(compact, "gpt-5.5-pro") || strings.Contains(compact, "gpt5-5-pro") || strings.Contains(compact, "gpt5.5-pro"):
		return "gpt-5.5-pro", true
	case strings.Contains(compact, "gpt-5.5") || strings.Contains(compact, "gpt5-5") || strings.Contains(compact, "gpt5.5"):
		return "gpt-5.5", true
	// GPT-5.6 三个变体官方定价各不相同（developers.openai.com/api/docs/pricing）：
	//   sol   $5/$30（standard，priority 2× = $10/$60）——同 gpt-5.5 standard 但 priority 更低
	//   terra $2/$12（2026-07-30 降 20%，原 $2.5/$15）——独立规范键，定价页可单独配置
	//   luna  $0.20/$1.20（2026-07-30 降 80%，原 $1/$6）
	// priority 均为 standard 的 2×，由 serviceTierCostMultiplier 兜底自动得出，无需显式配置。
	case strings.Contains(compact, "gpt-5.6-sol") || strings.Contains(compact, "gpt5-6-sol") || strings.Contains(compact, "gpt5.6-sol"):
		return "gpt-5.6-sol", true
	case strings.Contains(compact, "gpt-5.6-terra") || strings.Contains(compact, "gpt5-6-terra") || strings.Contains(compact, "gpt5.6-terra"):
		return "gpt-5.6-terra", true
	case strings.Contains(compact, "gpt-5.6-luna") || strings.Contains(compact, "gpt5-6-luna") || strings.Contains(compact, "gpt5.6-luna"):
		return "gpt-5.6-luna", true
	case strings.Contains(compact, "gpt-5.6") || strings.Contains(compact, "gpt5-6") || strings.Contains(compact, "gpt5.6"):
		// 未知 gpt-5.6 变体按 sol 兜底；Cyber 已在前面按独立价目处理。
		return "gpt-5.6-sol", true
	case strings.HasPrefix(compact, "gpt-daybreak-"):
		// 未知代号别名只保留项目既有 Sol 兜底，不据此生成同步价。
		return "gpt-5.6-sol", true
	case strings.Contains(compact, "gpt-5.4-mini") || strings.Contains(compact, "gpt5-4-mini") || strings.Contains(compact, "gpt5.4-mini"):
		return "gpt-5.4-mini", true
	case strings.Contains(compact, "gpt-5.4-nano") || strings.Contains(compact, "gpt5-4-nano") || strings.Contains(compact, "gpt5.4-nano"):
		return "gpt-5.4-nano", true
	case strings.Contains(compact, "gpt-5.4-pro") || strings.Contains(compact, "gpt5-4-pro") || strings.Contains(compact, "gpt5.4-pro"):
		return "gpt-5.4-pro", true
	case strings.Contains(compact, "gpt-5.4") || strings.Contains(compact, "gpt5-4") || strings.Contains(compact, "gpt5.4"):
		return "gpt-5.4", true
	case strings.Contains(compact, "gpt-5.2") || strings.Contains(compact, "gpt5-2") || strings.Contains(compact, "gpt5.2"):
		return "gpt-5.2", true
	case strings.Contains(compact, "gpt-5.3-codex-spark") || strings.Contains(compact, "gpt5-3-codex-spark") || strings.Contains(compact, "gpt5.3-codex-spark"):
		return "gpt-5.3-codex-spark", true
	case strings.Contains(compact, "gpt-5.3-codex") || strings.Contains(compact, "gpt5-3-codex") || strings.Contains(compact, "gpt5.3-codex"):
		return "gpt-5.3-codex", true
	case strings.Contains(compact, "gpt-5.3") || strings.Contains(compact, "gpt5-3") || strings.Contains(compact, "gpt5.3"):
		return "gpt-5.3-codex", true
	case strings.Contains(compact, "codex-auto-review"):
		// Codex internal auto-review model. ChatGPT backend API only
		// (chatgpt.com/backend-api/codex). Not available via public API.
		// Official catalog: Plus/Pro/Team/Business only, excludes free.
		// Specs match gpt-5.4 (272K context, 4 thinking levels).
		return "gpt-5.4", true
	case strings.Contains(compact, "codex"):
		return "gpt-5.3-codex", true
	case strings.Contains(compact, "gpt-5") || strings.Contains(compact, "gpt5"):
		return "gpt-5.4", true
	default:
		return "", false
	}
}

func modelRulePricing(model string) *ModelPricing {
	bestIdx := -1
	bestLen := -1
	for i := range modelPricingRules {
		rule := modelPricingRules[i]
		if modelMatchesRule(model, rule.model) && len(rule.model) > bestLen {
			bestIdx = i
			bestLen = len(rule.model)
		}
	}
	if bestIdx == -1 {
		return nil
	}
	return &modelPricingRules[bestIdx].pricing
}

func modelMatchesRule(model string, rule string) bool {
	if model == rule {
		return true
	}
	if !strings.HasPrefix(model, rule) {
		return false
	}
	if len(model) == len(rule) {
		return true
	}
	switch model[len(rule)] {
	case '-', '.', ':':
		return true
	default:
		return false
	}
}

func claudeFamilyPricing(model string) *ModelPricing {
	switch {
	case (strings.Contains(model, "fable-5.1") || strings.Contains(model, "fable-5-1") || strings.Contains(model, "mythos-5.1") || strings.Contains(model, "mythos-5-1")):
		return &ModelPricing{InputPricePerMToken: 10.0, CacheReadPricePerMToken: 0.25, CacheWrite5mPricePerMToken: 12.5, CacheWrite1hPricePerMToken: 20.0, OutputPricePerMToken: 50.0}
	case strings.Contains(model, "fable-5") || strings.Contains(model, "mythos-5"):
		return &ModelPricing{InputPricePerMToken: 10.0, CacheReadPricePerMToken: 1.0, CacheWrite5mPricePerMToken: 12.5, CacheWrite1hPricePerMToken: 20.0, OutputPricePerMToken: 50.0}
	case strings.Contains(model, "opus"):
		// 传统 Opus(3 / 4 / 4.1)为 $15/$75;自 4.5 起 Opus 降至 $5/$25,更新的版本
		// (4.6/4.7/4.8/5…)默认沿用现代档,避免新模型误套旧高价。
		legacyOpus := strings.Contains(model, "opus-3") || strings.Contains(model, "3-opus") ||
			strings.Contains(model, "opus-4-1") || strings.Contains(model, "opus-4.1") ||
			strings.Contains(model, "opus-4-0") || strings.Contains(model, "opus-4-2025") ||
			strings.HasSuffix(model, "opus-4")
		if legacyOpus {
			return &ModelPricing{InputPricePerMToken: 15.0, CacheReadPricePerMToken: 1.5, CacheWrite5mPricePerMToken: 18.75, CacheWrite1hPricePerMToken: 30.0, OutputPricePerMToken: 75.0}
		}
		return &ModelPricing{InputPricePerMToken: 5.0, CacheReadPricePerMToken: 0.5, CacheWrite5mPricePerMToken: 6.25, CacheWrite1hPricePerMToken: 10.0, OutputPricePerMToken: 25.0}
	case strings.Contains(model, "sonnet"):
		if strings.Contains(model, "sonnet-5") {
			return &ModelPricing{InputPricePerMToken: 2.0, CacheReadPricePerMToken: 0.2, CacheWrite5mPricePerMToken: 2.5, CacheWrite1hPricePerMToken: 4.0, OutputPricePerMToken: 10.0}
		}
		return &ModelPricing{InputPricePerMToken: 3.0, CacheReadPricePerMToken: 0.3, CacheWrite5mPricePerMToken: 3.75, CacheWrite1hPricePerMToken: 6.0, OutputPricePerMToken: 15.0}
	case strings.Contains(model, "haiku"):
		if strings.Contains(model, "3-5") || strings.Contains(model, "3.5") {
			return &ModelPricing{InputPricePerMToken: 0.8, CacheReadPricePerMToken: 0.08, CacheWrite5mPricePerMToken: 1.0, CacheWrite1hPricePerMToken: 1.6, OutputPricePerMToken: 4.0}
		}
		if strings.Contains(model, "4-5") || strings.Contains(model, "4.5") ||
			strings.Contains(model, "4-6") || strings.Contains(model, "4.6") ||
			strings.Contains(model, "4-7") || strings.Contains(model, "4.7") {
			return &ModelPricing{InputPricePerMToken: 1.0, CacheReadPricePerMToken: 0.1, CacheWrite5mPricePerMToken: 1.25, CacheWrite1hPricePerMToken: 2.0, OutputPricePerMToken: 5.0}
		}
		return &ModelPricing{InputPricePerMToken: 0.25, OutputPricePerMToken: 1.25}
	case strings.Contains(model, "claude"):
		return &ModelPricing{InputPricePerMToken: 3.0, OutputPricePerMToken: 15.0}
	default:
		return nil
	}
}

func geminiFamilyPricing(model string) *ModelPricing {
	if pricing := modelRulePricing(model); pricing != nil && strings.HasPrefix(model, "gemini-") {
		return pricing
	}
	if strings.Contains(model, "gemini-3.1-pro") || strings.Contains(model, "gemini-3-1-pro") {
		return &ModelPricing{InputPricePerMToken: 2.0, OutputPricePerMToken: 12.0}
	}
	return nil
}

func usePriorityPricing(serviceTier string, pricing *ModelPricing) bool {
	tier := normalizeServiceTier(serviceTier)
	if tier != "priority" && tier != "fast" && tier != "ultrafast" {
		return false
	}
	return pricing.InputPricePerMTokenPriority > 0 ||
		pricing.OutputPricePerMTokenPriority > 0 ||
		pricing.CacheReadPricePerMTokenPriority > 0
}

func serviceTierCostMultiplier(serviceTier string) float64 {
	switch normalizeServiceTier(serviceTier) {
	// Ultrafast follows this gateway's Fast pricing policy, including custom
	// priority prices, unless the model declares its own UltrafastMultiplier.
	// This is a billing default, not an official tariff claim.
	case "priority", "fast", "ultrafast":
		return 2.0
	case "flex":
		return 0.5
	default:
		return 1.0
	}
}

func normalizeServiceTier(serviceTier string) string {
	return strings.ToLower(strings.TrimSpace(serviceTier))
}

// lowercase aliases for internal callers
func calculateCost(inputTokens, outputTokens, cachedTokens int, model string, serviceTier string) float64 {
	return CalculateCost(inputTokens, outputTokens, cachedTokens, model, serviceTier)
}

func calculateCostBreakdown(inputTokens, outputTokens, cachedTokens int, model string, serviceTier string) CostBreakdown {
	return CalculateCostBreakdown(inputTokens, outputTokens, cachedTokens, model, serviceTier)
}
