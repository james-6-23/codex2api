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
		{model: "claude-3-5-haiku-20241022", wantInput: 1.0, wantOutput: 5.0},
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
			name:         "does not invent priority multiplier when priority price is unknown",
			model:        "gpt-4o",
			serviceTier:  "priority",
			inputTokens:  1000,
			outputTokens: 500,
			cachedTokens: 200,
			want:         0.0075,
		},
		{
			name:         "fast tier falls back to standard pricing when priority price is unknown",
			model:        "gpt-4o",
			serviceTier:  "fast",
			inputTokens:  1000,
			outputTokens: 500,
			cachedTokens: 200,
			want:         0.0075,
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

func TestLongContextPricingTriggersAbove272KTokens(t *testing.T) {
	// Below threshold: standard pricing.
	std := CalculateCostBreakdown(272000, 1000, 0, "gpt-5.4", "")
	assertFloatEqual(t, std.InputPricePerMToken, 2.5)
	assertFloatEqual(t, std.OutputPricePerMToken, 15.0)

	// Above threshold: long context premium pricing.
	long := CalculateCostBreakdown(272001, 1000, 0, "gpt-5.4", "")
	assertFloatEqual(t, long.InputPricePerMToken, 5.0)
	assertFloatEqual(t, long.OutputPricePerMToken, 22.5)

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
