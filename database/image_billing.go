package database

import (
	"regexp"
	"strings"
)

var gptImage25ModelPattern = regexp.MustCompile(`^(gpt-image-2\.5-(?:flare|sunburst))(?:-\d{4}-\d{2}-\d{2})?(?:-(?:2k|4k))?$`)

// GPTImage25BillingModel 将日期快照和本地尺寸别名归到对应的图片模型费率。
func GPTImage25BillingModel(model string) string {
	match := gptImage25ModelPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(model)))
	if len(match) == 0 {
		return ""
	}
	return match[1]
}

// 2026-09-09 核对官方 Flare/Sunburst 模型页，单位 USD / 1M tokens。
// https://developers.openai.com/api/docs/models/gpt-image-2.5-sunburst
var gptImage25Pricing = ModelPricing{
	InputPricePerMToken: 5, CacheReadPricePerMToken: 1.25,
	ImageInputPricePerMToken: 8, CacheReadImagePricePerMToken: 2,
	OutputPricePerMToken: 30,
}

// UsageLogCostBreakdown 统一实时限额、日志落库和日志展示的计费口径。
// 图片输入是 InputTokens 的子集，不能在总输入费用上重复计费。
func UsageLogCostBreakdown(log *UsageLogInput) CostBreakdown {
	if log == nil {
		return CostBreakdown{}
	}
	model := log.EffectiveModel
	if model == "" {
		model = log.Model
	}
	model = daybreakBillingModel(model, log.DaybreakProgram)
	if unit := MediaBillingUnit(model); unit != "" {
		return CostBreakdown{TotalCost: mediaUsageCost(log, model, unit)}
	}
	if GPTImage25BillingModel(model) == "" {
		return CalculateCostBreakdownWithCacheWrites(log.InputTokens, log.OutputTokens, log.CachedTokens, log.CacheWrite5mTokens, log.CacheWrite1hTokens, model, usageLogBillingServiceTier(log))
	}
	return imageTokenCostBreakdown(log.InputTokens, log.OutputTokens, log.CachedTokens, log.ImageInputTokens, log.ImageOutputTokens, log.CachedImageInputTokens, GetModelPricing(model))
}

func imageTokenCostBreakdown(input, output, cached, imageInput, imageOutput, cachedImage int, pricing *ModelPricing) CostBreakdown {
	input, output = max(0, input), max(0, output)
	imageInput = min(max(0, imageInput), input)
	textInput := input - imageInput
	cached = min(max(0, cached), input)
	cachedImage = min(max(0, cachedImage), min(cached, imageInput))
	cachedText := min(cached-cachedImage, textInput)
	// 上游只有缓存总量时，超出文本输入总量的部分必然来自图片。
	cachedImage = min(imageInput, cached-cachedText)
	if imageOutput > 0 {
		output = min(imageOutput, output)
	}
	imageInputPrice := pricing.ImageInputPricePerMToken
	if imageInputPrice <= 0 {
		imageInputPrice = pricing.InputPricePerMToken
	}
	imageCachePrice := pricing.CacheReadImagePricePerMToken
	if imageCachePrice <= 0 {
		imageCachePrice = imageInputPrice
	}
	imageCost := float64(imageInput-cachedImage) * imageInputPrice / 1e6
	imageCacheCost := float64(cachedImage) * imageCachePrice / 1e6
	inputCost := float64(textInput-cachedText)*pricing.InputPricePerMToken/1e6 + imageCost
	cacheCost := float64(cachedText)*pricing.CacheReadPricePerMToken/1e6 + imageCacheCost
	outputCost := float64(output) * pricing.OutputPricePerMToken / 1e6
	return CostBreakdown{
		InputCost: inputCost, OutputCost: outputCost, CacheReadCost: cacheCost,
		ImageInputCost: imageCost, ImageCacheReadCost: imageCacheCost,
		TotalCost:           inputCost + outputCost + cacheCost,
		InputPricePerMToken: pricing.InputPricePerMToken, OutputPricePerMToken: pricing.OutputPricePerMToken,
		CacheReadPricePerMToken:  pricing.CacheReadPricePerMToken,
		ImageInputPricePerMToken: imageInputPrice, CacheReadImagePricePerMToken: imageCachePrice,
		ServiceTierCostMultiplier: 1,
	}
}
