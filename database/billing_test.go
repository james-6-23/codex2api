package database

import (
	"math"
	"testing"
)

func TestGetModelPricingUsesMostSpecificOpenAIPrefix(t *testing.T) {
	tests := []struct {
		model      string
		wantInput  float64
		wantOutput float64
	}{
		{model: "gpt-4o-mini-2024-07-18", wantInput: 0.15, wantOutput: 0.6},
		{model: "gpt-4o-2024-08-06", wantInput: 2.5, wantOutput: 10.0},
		{model: "gpt-4-0613", wantInput: 30.0, wantOutput: 60.0},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetModelPricing(tt.model)
			assertPricing(t, got, tt.wantInput, tt.wantOutput)
		})
	}
}

func TestGetModelPricingUsesSub2APICodexFallbacks(t *testing.T) {
	tests := []struct {
		model      string
		wantInput  float64
		wantOutput float64
	}{
		{model: "gpt-5.4-mini-20260401", wantInput: 0.75, wantOutput: 4.5},
		{model: "gpt-5.3-codex-spark", wantInput: 1.25, wantOutput: 10.0},
		{model: "gpt-5.3-codex", wantInput: 1.75, wantOutput: 14.0},
		{model: "gpt-5.5", wantInput: 5.0, wantOutput: 30.0},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetModelPricing(tt.model)
			assertPricing(t, got, tt.wantInput, tt.wantOutput)
		})
	}
}

func TestGetModelPricingUsesSub2APIClaudeFamilies(t *testing.T) {
	tests := []struct {
		model      string
		wantInput  float64
		wantOutput float64
	}{
		{model: "claude-opus-4-7-20260401", wantInput: 5.0, wantOutput: 25.0},
		{model: "claude-opus-4-20250514", wantInput: 15.0, wantOutput: 75.0},
		{model: "claude-sonnet-4-5-20250929", wantInput: 3.0, wantOutput: 15.0},
		{model: "claude-3-5-haiku-20241022", wantInput: 0.8, wantOutput: 4.0},
		{model: "claude-unknown-model", wantInput: 3.0, wantOutput: 15.0},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetModelPricing(tt.model)
			assertPricing(t, got, tt.wantInput, tt.wantOutput)
		})
	}
}

func TestCalculateCostHandlesCachedTokensAndServiceTier(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		serviceTier  string
		inputTokens  int
		outputTokens int
		cachedTokens int
		want         float64
	}{
		{
			name:         "discounts cached tokens when cache pricing exists",
			model:        "gpt-5.4",
			inputTokens:  1000,
			outputTokens: 500,
			cachedTokens: 200,
			want:         0.00955,
		},
		{
			name:         "without cache pricing (gpt-4o)",
			model:        "gpt-4o",
			inputTokens:  1000,
			outputTokens: 500,
			cachedTokens: 200,
			want:         0.0075,
		},
		{
			name:         "uses priority prices when available",
			model:        "gpt-5.4",
			serviceTier:  "priority",
			inputTokens:  1000,
			outputTokens: 500,
			cachedTokens: 200,
			want:         0.0191,
		},
		{
			name:         "uses priority prices for fast tier",
			model:        "gpt-5.4",
			serviceTier:  "fast",
			inputTokens:  1000,
			outputTokens: 500,
			cachedTokens: 200,
			want:         0.0191,
		},
		{
			name:         "applies flex multiplier",
			model:        "gpt-5.4",
			serviceTier:  "flex",
			inputTokens:  1000,
			outputTokens: 500,
			cachedTokens: 200,
			want:         0.004775,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateCost(tt.inputTokens, tt.outputTokens, tt.cachedTokens, tt.model, tt.serviceTier)
			if math.Abs(got-tt.want) > 1e-12 {
				t.Fatalf("calculateCost() = %.12f, want %.12f", got, tt.want)
			}
		})
	}
}

func TestCalculateCostBreakdownExposesDisplayFields(t *testing.T) {
	got := calculateCostBreakdown(1000, 500, 200, "gpt-5.4", "flex")

	assertFloatEqual(t, got.InputCost, 0.001)
	assertFloatEqual(t, got.CacheReadCost, 0.000025)
	assertFloatEqual(t, got.OutputCost, 0.00375)
	assertFloatEqual(t, got.TotalCost, 0.004775)
	assertFloatEqual(t, got.InputPricePerMToken, 1.25)
	assertFloatEqual(t, got.CacheReadPricePerMToken, 0.125)
	assertFloatEqual(t, got.OutputPricePerMToken, 7.5)
	assertFloatEqual(t, got.ServiceTierCostMultiplier, 0.5)
	if got.LongContext {
		t.Fatal("LongContext = true, want false")
	}
	if got.LongContextThreshold != longContextThreshold {
		t.Fatalf("LongContextThreshold = %d, want %d", got.LongContextThreshold, longContextThreshold)
	}
}

func TestFastTierFallbackDoublesCostForModelsWithoutPriorityPricing(t *testing.T) {
	for _, model := range []string{"gpt-5.4-mini", "gpt-5.4-nano"} {
		base := calculateCostBreakdown(1000, 500, 200, model, "")
		fast := calculateCostBreakdown(1000, 500, 200, model, "fast")
		priority := calculateCostBreakdown(1000, 500, 200, model, "priority")

		assertFloatEqual(t, fast.TotalCost, base.TotalCost*2)
		assertFloatEqual(t, priority.TotalCost, base.TotalCost*2)
		assertFloatEqual(t, fast.ServiceTierCostMultiplier, 2.0)
		assertFloatEqual(t, priority.ServiceTierCostMultiplier, 2.0)
	}
}

func TestGPT55PricingDoesNotMatchGPT54(t *testing.T) {
	gpt54 := GetModelPricing("gpt-5.4")
	gpt55 := GetModelPricing("gpt-5.5")

	// gpt-5.5 is 2x gpt-5.4: $5/$30 vs $2.5/$15
	assertFloatEqual(t, gpt55.InputPricePerMToken, 5.0)
	assertFloatEqual(t, gpt55.OutputPricePerMToken, 30.0)
	assertFloatEqual(t, gpt55.CacheReadPricePerMToken, 0.5)
	assertFloatEqual(t, gpt55.InputPricePerMTokenPriority, 12.5)
	assertFloatEqual(t, gpt55.OutputPricePerMTokenPriority, 75.0)
	assertFloatEqual(t, gpt55.CacheReadPricePerMTokenPriority, 1.25)
	_ = gpt54
}

// gpt-5.6 三个变体官方定价各不相同（developers.openai.com/api/docs/pricing）：
// sol $5/$30、terra $2/$12、luna $0.20/$1.20（standard）；priority 均为 2× standard。
func TestGPT56VariantPricing(t *testing.T) {
	cases := []struct {
		model                   string
		input, output, cache    float64
		priorityIn, priorityOut float64
	}{
		{"gpt-5.6-sol", 5.0, 30.0, 0.5, 10.0, 60.0},
		{"gpt-5.6-terra", 2.0, 12.0, 0.2, 4.0, 24.0},
		{"gpt-5.6-luna", 0.2, 1.2, 0.02, 0.4, 2.4},
		{"gpt-5.6-sol-high", 5.0, 30.0, 0.5, 10.0, 60.0},
	}
	for _, c := range cases {
		p := GetModelPricing(c.model)
		assertFloatEqual(t, p.InputPricePerMToken, c.input)
		assertFloatEqual(t, p.OutputPricePerMToken, c.output)
		assertFloatEqual(t, p.CacheReadPricePerMToken, c.cache)

		// priority(fast)档短上下文：用 100K tokens(低于长上下文阈值 272K)。
		const n = 100_000
		got := CalculateCost(n, n, 0, c.model, "fast")
		want := (c.priorityIn + c.priorityOut) * float64(n) / 1_000_000.0
		assertFloatEqual(t, got, want)
	}
}

func TestSparkPricingUsesGpt51CodexFallback(t *testing.T) {
	spark := GetModelPricing("gpt-5.3-codex-spark-high")

	assertFloatEqual(t, spark.InputPricePerMToken, 1.25)
	assertFloatEqual(t, spark.OutputPricePerMToken, 10.0)
	assertFloatEqual(t, spark.CacheReadPricePerMToken, 0.125)
	assertFloatEqual(t, spark.InputPricePerMTokenPriority, 2.5)
	assertFloatEqual(t, spark.OutputPricePerMTokenPriority, 20.0)
	assertFloatEqual(t, spark.CacheReadPricePerMTokenPriority, 0.25)
}

func TestGPT53CodexPricingUsesGPT52CodexFallback(t *testing.T) {
	codex := GetModelPricing("gpt-5.3-codex-xhigh")
	gpt52 := GetModelPricing("gpt-5.2")

	assertFloatEqual(t, codex.InputPricePerMToken, gpt52.InputPricePerMToken)
	assertFloatEqual(t, codex.OutputPricePerMToken, gpt52.OutputPricePerMToken)
	assertFloatEqual(t, codex.CacheReadPricePerMToken, gpt52.CacheReadPricePerMToken)
	assertFloatEqual(t, codex.InputPricePerMTokenPriority, gpt52.InputPricePerMTokenPriority)
	assertFloatEqual(t, codex.OutputPricePerMTokenPriority, gpt52.OutputPricePerMTokenPriority)
	assertFloatEqual(t, codex.CacheReadPricePerMTokenPriority, gpt52.CacheReadPricePerMTokenPriority)
}

func TestUsageLogBreakdownScalesToStoredBilledTotal(t *testing.T) {
	log := &UsageLog{
		Model:         "gpt-5.4",
		InputTokens:   1000,
		StatusCode:    200,
		AccountBilled: 0.0025,
		UserBilled:    0.0025,
	}

	log.populateBillingBreakdown()

	assertFloatEqual(t, log.TotalCost, 0.0025)
	assertFloatEqual(t, log.InputCost, 0.0025)
	assertFloatEqual(t, log.InputPrice, 2.5)
}

func assertPricing(t *testing.T, got *ModelPricing, wantInput, wantOutput float64) {
	t.Helper()
	if got == nil {
		t.Fatal("GetModelPricing returned nil")
	}
	if math.Abs(got.InputPricePerMToken-wantInput) > 1e-12 || math.Abs(got.OutputPricePerMToken-wantOutput) > 1e-12 {
		t.Fatalf("pricing = input %.12f output %.12f, want input %.12f output %.12f",
			got.InputPricePerMToken, got.OutputPricePerMToken, wantInput, wantOutput)
	}
}

func assertFloatEqual(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("got %.12f, want %.12f", got, want)
	}
}

func TestProModelsHaveCorrectPricing(t *testing.T) {
	p55 := GetModelPricing("gpt-5.5-pro")
	if p55.InputPricePerMToken != 30.0 || p55.OutputPricePerMToken != 180.0 {
		t.Fatalf("gpt-5.5-pro pricing = input %.1f output %.1f, want input 30.0 output 180.0",
			p55.InputPricePerMToken, p55.OutputPricePerMToken)
	}

	p54 := GetModelPricing("gpt-5.4-pro")
	if p54.InputPricePerMToken != 30.0 || p54.OutputPricePerMToken != 180.0 {
		t.Fatalf("gpt-5.4-pro pricing = input %.1f output %.1f, want input 30.0 output 180.0",
			p54.InputPricePerMToken, p54.OutputPricePerMToken)
	}

	// Verify pro is NOT same as standard
	std55 := GetModelPricing("gpt-5.5")
	if std55.InputPricePerMToken == p55.InputPricePerMToken {
		t.Fatal("gpt-5.5-pro should have different pricing from gpt-5.5")
	}
}

func TestLongContextPricingTriggersAt272KTokens(t *testing.T) {
	// Below threshold: standard pricing.
	std := CalculateCostBreakdown(271999, 1000, 0, "gpt-5.4", "")
	assertFloatEqual(t, std.InputPricePerMToken, 2.5)
	assertFloatEqual(t, std.OutputPricePerMToken, 15.0)

	// Official table says <272K is short, so the 272K boundary is long pricing.
	long := CalculateCostBreakdown(272000, 1000, 0, "gpt-5.4", "")
	assertFloatEqual(t, long.InputPricePerMToken, 5.0)
	assertFloatEqual(t, long.OutputPricePerMToken, 22.5)
	if !long.LongContext {
		t.Fatal("LongContext = false, want true")
	}
	if long.LongContextThreshold != longContextThreshold {
		t.Fatalf("LongContextThreshold = %d, want %d", long.LongContextThreshold, longContextThreshold)
	}

	// Verify total cost is higher for long context.
	if long.TotalCost <= std.TotalCost {
		t.Fatalf("long context cost %.12f should be > standard cost %.12f", long.TotalCost, std.TotalCost)
	}
}

func TestLongContextPricingWithPriorityTier(t *testing.T) {
	long := CalculateCostBreakdown(300000, 1000, 200, "gpt-5.4", "priority")
	assertFloatEqual(t, long.InputPricePerMToken, 10.0)
	assertFloatEqual(t, long.OutputPricePerMToken, 45.0)
	assertFloatEqual(t, long.CacheReadPricePerMToken, 1.0)
	assertFloatEqual(t, long.ServiceTierCostMultiplier, 1.0)
}

func TestLongContextPricingDoesNotApplyWhenNoLongPricingDefined(t *testing.T) {
	// gpt-4o has no long context pricing fields defined.
	std := CalculateCostBreakdown(1000, 500, 0, "gpt-4o", "")
	long := CalculateCostBreakdown(300000, 500, 0, "gpt-4o", "")
	// Input price should be the same since no long variant exists.
	assertFloatEqual(t, std.InputPricePerMToken, long.InputPricePerMToken)
	assertFloatEqual(t, std.OutputPricePerMToken, long.OutputPricePerMToken)
	if long.LongContext {
		t.Fatal("LongContext = true for model without long pricing, want false")
	}
}

func TestCodexAutoReviewModelNormalizesToGPT54(t *testing.T) {
	pricing := GetModelPricing("codex-auto-review")
	if pricing == nil {
		t.Fatal("GetModelPricing(codex-auto-review) returned nil")
	}
	// Should normalize to gpt-5.4 pricing.
	gpt54 := GetModelPricing("gpt-5.4")
	assertFloatEqual(t, pricing.InputPricePerMToken, gpt54.InputPricePerMToken)
	assertFloatEqual(t, pricing.OutputPricePerMToken, gpt54.OutputPricePerMToken)
	assertFloatEqual(t, pricing.CacheReadPricePerMToken, gpt54.CacheReadPricePerMToken)
	assertFloatEqual(t, pricing.InputPricePerMTokenPriority, gpt54.InputPricePerMTokenPriority)
	assertFloatEqual(t, pricing.OutputPricePerMTokenPriority, gpt54.OutputPricePerMTokenPriority)
	assertFloatEqual(t, pricing.CacheReadPricePerMTokenPriority, gpt54.CacheReadPricePerMTokenPriority)
}

func TestCodexAutoReviewLongContextPricing(t *testing.T) {
	// codex-auto-review maps to gpt-5.4 which has long context pricing.
	long := CalculateCostBreakdown(300000, 500, 100, "codex-auto-review", "")
	assertFloatEqual(t, long.InputPricePerMToken, 5.0)     // long input price
	assertFloatEqual(t, long.OutputPricePerMToken, 22.5)   // long output price
	assertFloatEqual(t, long.CacheReadPricePerMToken, 0.5) // long cache read price
}

func TestCodexAutoReviewPriorityPricing(t *testing.T) {
	bd := CalculateCostBreakdown(1000, 500, 0, "codex-auto-review", "priority")
	assertFloatEqual(t, bd.InputPricePerMToken, 5.0)
	assertFloatEqual(t, bd.OutputPricePerMToken, 30.0)
	assertFloatEqual(t, bd.ServiceTierCostMultiplier, 1.0)
}

func TestNormalizeCodexBillingModelCodexAutoReview(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"codex-auto-review", "gpt-5.4"},
		{"codex-auto-review-v2", "gpt-5.4"}, // variant suffix should match
		{"CODEX-AUTO-REVIEW", "gpt-5.4"},    // case-insensitive
		{"codex_auto_review", "gpt-5.4"},    // underscores normalized
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got, ok := normalizeCodexBillingModel(tt.model)
			if !ok {
				t.Fatalf("normalizeCodexBillingModel(%q) ok=false, want true", tt.model)
			}
			if got != tt.want {
				t.Fatalf("normalizeCodexBillingModel(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestGPT55LongContextPricing(t *testing.T) {
	// gpt-5.5 has long pricing: $10/$45 standard, $25/$112.5 priority.
	long := CalculateCostBreakdown(300000, 500, 0, "gpt-5.5", "")
	assertFloatEqual(t, long.InputPricePerMToken, 10.0)
	assertFloatEqual(t, long.OutputPricePerMToken, 45.0)

	longPri := CalculateCostBreakdown(300000, 500, 0, "gpt-5.5", "priority")
	assertFloatEqual(t, longPri.InputPricePerMToken, 25.0)
	assertFloatEqual(t, longPri.OutputPricePerMToken, 112.5)
	assertFloatEqual(t, longPri.ServiceTierCostMultiplier, 1.0)
}

func TestGrokPricingUsesXAIRates(t *testing.T) {
	tests := []struct {
		model      string
		wantInput  float64
		wantOutput float64
		wantCache  float64
	}{
		{model: "grok-4.6", wantInput: 2.0, wantOutput: 6.0, wantCache: 0.5},
		{model: "grok-4.6-beta", wantInput: 2.0, wantOutput: 6.0, wantCache: 0.5},
		{model: "grok-4.5", wantInput: 2.0, wantOutput: 6.0, wantCache: 0.3},
		{model: "grok-4.3", wantInput: 1.25, wantOutput: 2.5, wantCache: 0.2},
		{model: "grok-4-fast", wantInput: 0.2, wantOutput: 0.5, wantCache: 0.05},
		{model: "grok-4", wantInput: 3.0, wantOutput: 15.0, wantCache: 0.75},
		{model: "grok-code-fast-1", wantInput: 0.2, wantOutput: 1.5, wantCache: 0.02},
		{model: "grok-3-mini", wantInput: 0.3, wantOutput: 0.5, wantCache: 0.075},
		{model: "grok-3-fast", wantInput: 5.0, wantOutput: 25.0},
		{model: "grok-3", wantInput: 3.0, wantOutput: 15.0, wantCache: 0.75},
		{model: "grok-2", wantInput: 2.0, wantOutput: 10.0},
		// 日期后缀变体走同一条规则。
		{model: "grok-4-0709", wantInput: 3.0, wantOutput: 15.0, wantCache: 0.75},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetModelPricing(tt.model)
			assertPricing(t, got, tt.wantInput, tt.wantOutput)
			assertFloatEqual(t, got.CacheReadPricePerMToken, tt.wantCache)
		})
	}
}

// grok-4.6 / grok-4.5 更专用的规则必须压过 grok-4，否则 $2/$6 会被当成 $3/$15。
func TestAntigravityGeminiEstimatedPricing(t *testing.T) {
	tests := []struct {
		model      string
		wantInput  float64
		wantOutput float64
		wantCache  float64
	}{
		{model: "gemini-3-pro-preview", wantInput: 2.0, wantOutput: 12.0, wantCache: 0.2},
		{model: "gemini-2.5-pro", wantInput: 1.25, wantOutput: 10.0, wantCache: 0.125},
		{model: "gemini-2.5-flash", wantInput: 0.3, wantOutput: 2.5, wantCache: 0.03},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetModelPricing(tt.model)
			assertPricing(t, got, tt.wantInput, tt.wantOutput)
			assertFloatEqual(t, got.CacheReadPricePerMToken, tt.wantCache)
		})
	}
}

func TestGrokMoreSpecificRuleWinsOverShorterPrefix(t *testing.T) {
	assertPricing(t, GetModelPricing("grok-4.6"), 2.0, 6.0)
	assertFloatEqual(t, GetModelPricing("grok-4.6").CacheReadPricePerMToken, 0.5)
	assertPricing(t, GetModelPricing("grok-4.5"), 2.0, 6.0)
	assertFloatEqual(t, GetModelPricing("grok-4.5").CacheReadPricePerMToken, 0.3)
	assertPricing(t, GetModelPricing("grok-4"), 3.0, 15.0)
	assertPricing(t, GetModelPricing("grok-3-fast"), 5.0, 25.0)
	assertPricing(t, GetModelPricing("grok-3"), 3.0, 15.0)
}

// Grok 的长上下文分档线是 200K，不是 OpenAI 的 272K。
func TestGrokLongContextThresholdIs200K(t *testing.T) {
	std := CalculateCostBreakdown(199999, 1000, 0, "grok-4.5", "")
	assertFloatEqual(t, std.InputPricePerMToken, 2.0)
	assertFloatEqual(t, std.OutputPricePerMToken, 6.0)
	if std.LongContext {
		t.Fatal("LongContext = true below 200K for grok-4.5, want false")
	}

	long := CalculateCostBreakdown(200000, 1000, 500, "grok-4.5", "")
	assertFloatEqual(t, long.InputPricePerMToken, 4.0)
	assertFloatEqual(t, long.OutputPricePerMToken, 12.0)
	assertFloatEqual(t, long.CacheReadPricePerMToken, 1.0)
	if !long.LongContext {
		t.Fatal("LongContext = false above 200K for grok-4.5, want true")
	}
	if long.LongContextThreshold != 200000 {
		t.Fatalf("LongContextThreshold = %d, want 200000", long.LongContextThreshold)
	}

	// grok-4.6 同分档线；缓存短档 $0.50、长档 $1.00（与 grok-4.5 的 $0.30 不同）。
	std46 := CalculateCostBreakdown(199999, 1000, 500, "grok-4.6", "")
	assertFloatEqual(t, std46.InputPricePerMToken, 2.0)
	assertFloatEqual(t, std46.CacheReadPricePerMToken, 0.5)
	if std46.LongContext {
		t.Fatal("LongContext = true below 200K for grok-4.6, want false")
	}
	long46 := CalculateCostBreakdown(200000, 1000, 500, "grok-4.6", "")
	assertFloatEqual(t, long46.InputPricePerMToken, 4.0)
	assertFloatEqual(t, long46.OutputPricePerMToken, 12.0)
	assertFloatEqual(t, long46.CacheReadPricePerMToken, 1.0)
	if !long46.LongContext {
		t.Fatal("LongContext = false at 200K for grok-4.6, want true")
	}

	// Codex 模型不受影响，仍是 272K（官方短档为 <272K）。
	codex := CalculateCostBreakdown(271999, 1000, 0, "gpt-5.4", "")
	if codex.LongContext {
		t.Fatal("gpt-5.4 LongContext = true below 272K, want false")
	}
	if codex.LongContextThreshold != longContextThreshold {
		t.Fatalf("gpt-5.4 LongContextThreshold = %d, want %d", codex.LongContextThreshold, longContextThreshold)
	}
}

func TestGrok46OfficialPricingAndLongContextThreshold(t *testing.T) {
	pricing := GetModelPricing("grok-4.6")
	if pricing.InputPricePerMToken != 2 || pricing.CacheReadPricePerMToken != 0.5 || pricing.OutputPricePerMToken != 6 {
		t.Fatalf("grok-4.6 short pricing = %+v", pricing)
	}
	if pricing.LongInputPricePerMToken != 4 || pricing.LongCacheReadPricePerMToken != 1 || pricing.LongOutputPricePerMToken != 12 {
		t.Fatalf("grok-4.6 long pricing = %+v", pricing)
	}
	if pricing.LongContextThresholdTokens != 200000 {
		t.Fatalf("grok-4.6 long threshold = %d, want 200000", pricing.LongContextThresholdTokens)
	}

	breakdown := CalculateCostBreakdown(200001, 1000000, 0, "grok-4.6", "")
	if !breakdown.LongContext || breakdown.LongContextThreshold != 200000 {
		t.Fatalf("grok-4.6 should enter long pricing above 200K: %+v", breakdown)
	}
}

func TestClaudeFablePricingUsesOfficialCacheReadRates(t *testing.T) {
	fable51 := GetModelPricing("claude-fable-5.1")
	assertPricing(t, fable51, 10, 50)
	assertFloatEqual(t, fable51.CacheReadPricePerMToken, 0.25)
	if fable51.CacheWrite5mPricePerMToken != 12.5 || fable51.CacheWrite1hPricePerMToken != 20 {
		t.Fatalf("Fable 5.1 cache-write pricing = %+v", fable51)
	}
	fable5 := GetModelPricing("claude-fable-5")
	assertPricing(t, fable5, 10, 50)
	assertFloatEqual(t, fable5.CacheReadPricePerMToken, 1)
	if fable5.CacheWrite5mPricePerMToken != 12.5 || fable5.CacheWrite1hPricePerMToken != 20 {
		t.Fatalf("Fable 5 cache-write pricing = %+v", fable5)
	}
}
