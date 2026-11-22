package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// 模型定价覆盖来源。custom 由管理员手填(最高优先级)，synced 由「从 JSON URL 同步」写入。
const (
	ModelPricingSourceCustom = "custom"
	ModelPricingSourceSynced = "synced"
)

// ModelPricingOverride 是单个模型的定价覆盖（设置存储与同步 URL 载荷共用同一形态）。
// Token 价格单位：USD / 1M tokens。Token 字段为 0 时回退代码默认价。
// ImageUnitPrice 则为用户单位价（按张/按次/按秒模式下的 USD / 计费单位），这些模式要求显式正单价；
// MediaUnitCost 是 Grok Imagine 媒体模型的上游单位成本（USD / 张 或 USD / 秒）。
// 优先级：custom > synced > 代码默认。
type ModelPricingOverride struct {
	UserBillingMode string  `json:"user_billing_mode,omitempty"`
	ImageUnitPrice  float64 `json:"image_unit_price,omitempty"` // USD per billed unit (image / video / second) in unit billing modes; upstream cost stays independent.
	// MediaUnitCost 是 Grok Imagine 媒体模型的上游单位成本(USD/张 或 USD/秒)。
	MediaUnitCost    float64 `json:"media_unit_cost,omitempty"`
	ImageInput       float64 `json:"image_input,omitempty"`
	CachedImageInput float64 `json:"cached_image_input,omitempty"`
	Source           string  `json:"source,omitempty"`

	// 标准档（短上下文）
	Input       float64 `json:"input,omitempty"`
	CachedInput float64 `json:"cached_input,omitempty"`
	// Anthropic prompt-cache creation prices are informational today; actual
	// billing still uses CachedInput (cache read) because usage logs expose
	// cache reads separately from cache creation only in provider payloads.
	CacheWrite5m float64 `json:"cache_write_5m,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
	Output       float64 `json:"output,omitempty"`

	// priority(fast) 档
	InputPriority       float64 `json:"input_priority,omitempty"`
	CachedInputPriority float64 `json:"cached_input_priority,omitempty"`
	OutputPriority      float64 `json:"output_priority,omitempty"`

	// 长上下文档（input 超过该模型的分档线：OpenAI 272K、Grok 200K）
	InputLong       float64 `json:"input_long,omitempty"`
	CachedInputLong float64 `json:"cached_input_long,omitempty"`
	OutputLong      float64 `json:"output_long,omitempty"`

	// 长上下文 + priority/fast 档。OpenAI 官方价目表会独立发布这组价格，
	// 不能再用短上下文 priority 或长上下文 standard 的倍率推算。
	InputLongPriority       float64 `json:"input_long_priority,omitempty"`
	CachedInputLongPriority float64 `json:"cached_input_long_priority,omitempty"`
	OutputLongPriority      float64 `json:"output_long_priority,omitempty"`

	// 官方长上下文分档线。OpenAI 当前为 272K，xAI 当前为 200K；同步保存该值，
	// 避免新模型在只有价格覆盖时错误继承全局默认阈值。
	LongContextThresholdTokens int `json:"long_context_threshold_tokens,omitempty"`
}

// NormalizeModelPricingOverride 保留模型覆盖中的长上下文档位。
// 长上下文价格由每个模型自己的规则决定，不能对 Astra 做全局清零。
func NormalizeModelPricingOverride(model string, o ModelPricingOverride) ModelPricingOverride {
	return o
}

// IsEmpty 判断覆盖是否不含任何价格（全 0）。
func (o ModelPricingOverride) IsEmpty() bool {
	return o.UserBillingMode == "" && o.ImageUnitPrice == 0 && o.MediaUnitCost == 0 && o.ImageInput == 0 && o.CachedImageInput == 0 && o.Input == 0 && o.CachedInput == 0 && o.CacheWrite5m == 0 && o.CacheWrite1h == 0 && o.Output == 0 &&
		o.InputPriority == 0 && o.CachedInputPriority == 0 && o.OutputPriority == 0 &&
		o.InputLong == 0 && o.CachedInputLong == 0 && o.OutputLong == 0 &&
		o.InputLongPriority == 0 && o.CachedInputLongPriority == 0 && o.OutputLongPriority == 0 &&
		o.LongContextThresholdTokens == 0
}

// applyNonZero 把覆盖里非 0 的价格字段写入 p（就地）。0 字段保持 p 原值（代码默认）。
func (o ModelPricingOverride) applyNonZero(p *ModelPricing) {
	if o.UserBillingMode != "" {
		p.UserBillingMode = o.UserBillingMode
	}
	if o.ImageUnitPrice > 0 {
		p.ImageUnitPrice = o.ImageUnitPrice
	}
	if o.MediaUnitCost > 0 {
		p.MediaUnitCost = o.MediaUnitCost
	}
	if o.ImageInput > 0 {
		p.ImageInputPricePerMToken = o.ImageInput
	}
	if o.CachedImageInput > 0 {
		p.CacheReadImagePricePerMToken = o.CachedImageInput
	}
	if o.Input > 0 {
		p.InputPricePerMToken = o.Input
	}
	if o.CachedInput > 0 {
		p.CacheReadPricePerMToken = o.CachedInput
	}
	if o.CacheWrite5m > 0 {
		p.CacheWrite5mPricePerMToken = o.CacheWrite5m
	}
	if o.CacheWrite1h > 0 {
		p.CacheWrite1hPricePerMToken = o.CacheWrite1h
	}
	if o.Output > 0 {
		p.OutputPricePerMToken = o.Output
	}
	if o.InputPriority > 0 {
		p.InputPricePerMTokenPriority = o.InputPriority
	}
	if o.CachedInputPriority > 0 {
		p.CacheReadPricePerMTokenPriority = o.CachedInputPriority
	}
	if o.OutputPriority > 0 {
		p.OutputPricePerMTokenPriority = o.OutputPriority
	}
	if o.InputLong > 0 {
		p.LongInputPricePerMToken = o.InputLong
	}
	if o.CachedInputLong > 0 {
		p.LongCacheReadPricePerMToken = o.CachedInputLong
	}
	if o.OutputLong > 0 {
		p.LongOutputPricePerMToken = o.OutputLong
	}
	if o.InputLongPriority > 0 {
		p.LongInputPricePerMTokenPriority = o.InputLongPriority
	}
	if o.CachedInputLongPriority > 0 {
		p.LongCacheReadPricePerMTokenPriority = o.CachedInputLongPriority
	}
	if o.OutputLongPriority > 0 {
		p.LongOutputPricePerMTokenPriority = o.OutputLongPriority
	}
	if o.LongContextThresholdTokens > 0 {
		p.LongContextThresholdTokens = o.LongContextThresholdTokens
	}
}

// ModelPricingOverrideFromPricing 把一份完整 ModelPricing 投影为覆盖 JSON 形态，
// 供管理端展示"当前生效价"与编辑初值。
func ModelPricingOverrideFromPricing(p *ModelPricing, source string) ModelPricingOverride {
	if p == nil {
		return ModelPricingOverride{Source: source}
	}
	return ModelPricingOverride{
		UserBillingMode:            p.UserBillingMode,
		ImageUnitPrice:             p.ImageUnitPrice,
		MediaUnitCost:              p.MediaUnitCost,
		ImageInput:                 p.ImageInputPricePerMToken,
		CachedImageInput:           p.CacheReadImagePricePerMToken,
		Source:                     source,
		Input:                      p.InputPricePerMToken,
		CachedInput:                p.CacheReadPricePerMToken,
		CacheWrite5m:               p.CacheWrite5mPricePerMToken,
		CacheWrite1h:               p.CacheWrite1hPricePerMToken,
		Output:                     p.OutputPricePerMToken,
		InputPriority:              p.InputPricePerMTokenPriority,
		CachedInputPriority:        p.CacheReadPricePerMTokenPriority,
		OutputPriority:             p.OutputPricePerMTokenPriority,
		InputLong:                  p.LongInputPricePerMToken,
		CachedInputLong:            p.LongCacheReadPricePerMToken,
		OutputLong:                 p.LongOutputPricePerMToken,
		InputLongPriority:          p.LongInputPricePerMTokenPriority,
		CachedInputLongPriority:    p.LongCacheReadPricePerMTokenPriority,
		OutputLongPriority:         p.LongOutputPricePerMTokenPriority,
		LongContextThresholdTokens: p.LongContextThresholdTokens,
	}
}

// pricingOverrides 存 map[string]ModelPricingOverride（key 为规范化模型名，小写）。
var pricingOverrides atomic.Value

// modelPricingMutationSlot serializes the very short read/merge/write phase of
// manual, JSON-reference, and official pricing updates. Network fetching must
// always finish before entering this slot.
var modelPricingMutationSlot = func() chan struct{} {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	return ch
}()

// MutateModelPricingSettings applies one in-process atomic read/merge/write.
// syncURL=nil preserves the current manual JSON source; a non-nil value replaces
// it (including an empty string). The callback must not perform network I/O.
func (db *DB) MutateModelPricingSettings(ctx context.Context, syncURL *string, mutate func(map[string]ModelPricingOverride) error) (map[string]ModelPricingOverride, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-modelPricingMutationSlot:
	}
	defer func() { modelPricingMutationSlot <- struct{}{} }()

	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		settings = &SystemSettings{}
	}
	overrides, err := ParseModelPricingOverridesJSON(settings.ModelPricingOverrides)
	if err != nil {
		return nil, fmt.Errorf("解析模型定价覆盖失败，已中止写入以免清空现有价格: %w", err)
	}
	if mutate != nil {
		if err := mutate(overrides); err != nil {
			return nil, err
		}
	}
	for model, override := range overrides {
		normalized := NormalizeModelPricingOverride(model, override)
		if normalized.IsEmpty() {
			delete(overrides, model)
		} else {
			overrides[model] = normalized
		}
	}
	blob, err := MarshalModelPricingOverridesJSON(overrides)
	if err != nil {
		return nil, err
	}
	effectiveSyncURL := settings.ModelPricingSyncURL
	if syncURL != nil {
		effectiveSyncURL = *syncURL
	}
	if err := db.UpdateModelPricingSettings(ctx, blob, effectiveSyncURL); err != nil {
		return nil, err
	}
	SetModelPricingOverrides(overrides)
	return overrides, nil
}

// SetModelPricingOverrides 刷新运行时定价覆盖表（key 归一为小写去空白）。
func SetModelPricingOverrides(m map[string]ModelPricingOverride) {
	norm := make(map[string]ModelPricingOverride, len(m))
	for k, v := range m {
		key := strings.ToLower(strings.TrimSpace(k))
		if err := ValidateModelUserBilling(key, v); err != nil {
			continue
		}
		v = NormalizeModelPricingOverride(key, v)
		if key == "" || v.IsEmpty() {
			continue
		}
		norm[key] = v
	}
	pricingOverrides.Store(norm)
}

func currentModelPricingOverrides() map[string]ModelPricingOverride {
	if v, ok := pricingOverrides.Load().(map[string]ModelPricingOverride); ok {
		return v
	}
	return nil
}

// lookupModelPricingOverride 按规范化模型名查覆盖。
func lookupModelPricingOverride(canonical string) (ModelPricingOverride, bool) {
	m := currentModelPricingOverrides()
	if m == nil {
		return ModelPricingOverride{}, false
	}
	ov, ok := m[strings.ToLower(strings.TrimSpace(canonical))]
	return ov, ok
}

// CanonicalBillingModelKey 返回某模型用于定价查找/覆盖的规范键（小写）。
func CanonicalBillingModelKey(model string) string {
	normalized := normalizeBillingModelName(model)
	if imageModel := GPTImage25BillingModel(normalized); imageModel != "" {
		return imageModel
	}
	if codexModel, ok := normalizeCodexBillingModel(normalized); ok {
		return strings.ToLower(codexModel)
	}
	if key := GrokMediaPricingKey(normalized); key != "" {
		return key
	}
	if canonical := antigravityCanonicalPricingModel(normalized); canonical != "" {
		return canonical
	}
	return strings.ToLower(normalized)
}

// PricingManagementModelKey returns the key exposed by pricing management.
// Most model variants share their canonical model's price, but selected
// internal aliases have an independent override and therefore need their own
// editable row instead of being deduplicated into the canonical model.
func PricingManagementModelKey(model string) string {
	if key := daybreakPricingAliasKey(model); key != "" {
		return key
	}
	if key := discoveredGPTPricingKey(model); key != "" {
		return key
	}
	normalized := normalizeBillingModelName(model)
	compact := strings.NewReplacer(" ", "-", "_", "-").Replace(normalized)
	if compact == "codex-auto-review" {
		return compact
	}
	if isAntigravityPricingAlias(compact) {
		return compact
	}
	return CanonicalBillingModelKey(normalized)
}

func daybreakPricingAliasKey(model string) string {
	normalized := normalizeBillingModelName(model)
	if normalized == "" {
		return ""
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, suffix := range []string{"-daybreak-blue", "-daybreak-red"} {
		if strings.HasSuffix(lower, suffix) {
			return normalized + suffix
		}
	}
	if strings.HasPrefix(normalized, "gpt-daybreak-") {
		return normalized
	}
	return ""
}

func antigravityCanonicalPricingModel(model string) string {
	normalized := normalizeBillingModelName(model)
	if normalized == "" {
		return ""
	}
	lower := strings.ToLower(strings.TrimSpace(normalized))
	if strings.HasPrefix(lower, "gemini-") {
		for _, suffix := range []string{"-high", "-medium", "-low", "-tiered"} {
			if strings.HasSuffix(lower, suffix) {
				return strings.TrimSuffix(lower, suffix)
			}
		}
	}
	if strings.HasPrefix(lower, "claude-") {
		if strings.HasSuffix(lower, "-thinking") {
			return strings.TrimSuffix(lower, "-thinking")
		}
		for _, suffix := range []string{"-high", "-medium", "-low"} {
			if strings.HasSuffix(lower, suffix) {
				return strings.TrimSuffix(lower, suffix)
			}
		}
	}
	return ""
}

func isAntigravityPricingAlias(model string) bool {
	return antigravityCanonicalPricingModel(model) != ""
}

// PricingAliasTarget reports the canonical fallback for an independently
// managed alias. An empty string means the model is already canonical.
func PricingAliasTarget(model string) string {
	if discoveredGPTPricingKey(model) != "" && daybreakPricingAliasKey(model) == "" {
		return ""
	}
	managed := PricingManagementModelKey(model)
	canonical := CanonicalBillingModelKey(model)
	if managed != canonical {
		return canonical
	}
	return ""
}

// ModelPricingSourceFor 返回某规范键当前定价来源：custom / synced / default。
func ModelPricingSourceFor(canonical string) string {
	if ov, ok := lookupModelPricingOverride(canonical); ok {
		if ov.Source == ModelPricingSourceCustom {
			return ModelPricingSourceCustom
		}
		return ModelPricingSourceSynced
	}
	if target := PricingAliasTarget(canonical); target != "" {
		if ov, ok := lookupModelPricingOverride(target); ok {
			if ov.Source == ModelPricingSourceCustom {
				return ModelPricingSourceCustom
			}
			return ModelPricingSourceSynced
		}
	}
	return "default"
}

// ParseModelPricingOverridesJSON 解析存储的 JSON blob（model → override）。
// 空串返回空 map、nil error。
func ParseModelPricingOverridesJSON(s string) (map[string]ModelPricingOverride, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return map[string]ModelPricingOverride{}, nil
	}
	var raw map[string]ModelPricingOverride
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	out := make(map[string]ModelPricingOverride, len(raw))
	for k, v := range raw {
		key := strings.ToLower(strings.TrimSpace(k))
		if err := ValidateModelUserBilling(key, v); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		v = NormalizeModelPricingOverride(key, v)
		if key == "" || v.IsEmpty() {
			continue
		}
		out[key] = v
	}
	return out, nil
}

// MarshalModelPricingOverridesJSON 序列化覆盖表为存储用 JSON。
func MarshalModelPricingOverridesJSON(m map[string]ModelPricingOverride) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	buf, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}
