package database

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGPT6SolAndLunaIndependentPricingKeys(t *testing.T) {
	previous := currentModelPricingOverrides()
	SetModelPricingOverrides(nil)
	t.Cleanup(func() { SetModelPricingOverrides(previous) })

	for _, tc := range []struct {
		model                 string
		input, cached, output float64
	}{
		{"gpt-6-sol", 2, 0.2, 10},
		{"gpt-6-luna", 0.1, 0.01, 0.5},
	} {
		for _, model := range []string{
			tc.model,
			strings.ToUpper(tc.model),
			"models/" + tc.model,
			tc.model + "-high",
			tc.model + "(xhigh)",
			tc.model + "-2026-09-22",
			tc.model + "-openai-compact",
		} {
			t.Run(model, func(t *testing.T) {
				if got := PricingManagementModelKey(model); got != tc.model {
					t.Fatalf("pricing key = %q, want %q", got, tc.model)
				}
				if got := PricingAliasTarget(model); got != "" {
					t.Fatalf("independent model incorrectly aliases %q", got)
				}
				p := GetModelPricing(model)
				assertFloatEqual(t, p.InputPricePerMToken, tc.input)
				assertFloatEqual(t, p.CacheReadPricePerMToken, tc.cached)
				assertFloatEqual(t, p.OutputPricePerMToken, tc.output)
				assertFloatEqual(t, p.LongInputPricePerMToken, tc.input*2)
				assertFloatEqual(t, p.LongCacheReadPricePerMToken, tc.cached*2)
				assertFloatEqual(t, p.LongOutputPricePerMToken, tc.output*1.5)
			})
		}
	}
}

func TestGPT61SolUsesIndependentSolPricing(t *testing.T) {
	previous := currentModelPricingOverrides()
	SetModelPricingOverrides(nil)
	t.Cleanup(func() { SetModelPricingOverrides(previous) })

	for _, model := range []string{
		"gpt-6.1-sol",
		"GPT-6.1-SOL",
		"models/gpt-6.1-sol",
		"gpt-6.1-sol-high",
		"gpt-6.1-sol(xhigh)",
		"gpt-6.1-sol-2026-09-22",
		"gpt-6.1-sol-openai-compact",
	} {
		if got := CanonicalBillingModelKey(model); got != "gpt-6.1-sol" {
			t.Fatalf("CanonicalBillingModelKey(%q) = %q, want gpt-6.1-sol", model, got)
		}
		if got := PricingManagementModelKey(model); got != "gpt-6.1-sol" {
			t.Fatalf("PricingManagementModelKey(%q) = %q, want gpt-6.1-sol", model, got)
		}
		if got := PricingAliasTarget(model); got != "" {
			t.Fatalf("PricingAliasTarget(%q) = %q, want canonical model", model, got)
		}
		p := GetModelPricing(model)
		assertFloatEqual(t, p.InputPricePerMToken, 2)
		assertFloatEqual(t, p.CacheReadPricePerMToken, 0.1)
		assertFloatEqual(t, p.CacheWrite5mPricePerMToken, 2.5)
		assertFloatEqual(t, p.CacheWrite1hPricePerMToken, 2.5)
		assertFloatEqual(t, p.OutputPricePerMToken, 10)
		assertFloatEqual(t, p.LongInputPricePerMToken, 4)
		assertFloatEqual(t, p.LongCacheReadPricePerMToken, 0.2)
		assertFloatEqual(t, p.LongOutputPricePerMToken, 15)
	}

	short := CalculateCostBreakdownWithCacheWrites(271999, 1000, 100000, 1000, 500, "gpt-6.1-sol", "")
	if short.LongContext {
		t.Fatalf("short-tier request unexpectedly used long pricing: %+v", short)
	}
	assertFloatEqual(t, short.InputPricePerMToken, 2)
	assertFloatEqual(t, short.CacheReadPricePerMToken, 0.1)
	assertFloatEqual(t, short.CacheWrite5mPricePerMToken, 2.5)
	assertFloatEqual(t, short.CacheWrite1hPricePerMToken, 2.5)
	assertFloatEqual(t, short.OutputPricePerMToken, 10)

	long := CalculateCostBreakdownWithCacheWrites(272001, 1000, 100000, 1000, 500, "gpt-6.1-sol", "")
	if !long.LongContext {
		t.Fatalf("long-tier request did not use long pricing: %+v", long)
	}
	assertFloatEqual(t, long.InputPricePerMToken, 4)
	assertFloatEqual(t, long.CacheReadPricePerMToken, 0.2)
	assertFloatEqual(t, long.OutputPricePerMToken, 15)
}

func TestGPT62SolAndAstraUseIndependentPricing(t *testing.T) {
	previous := currentModelPricingOverrides()
	SetModelPricingOverrides(nil)
	t.Cleanup(func() { SetModelPricingOverrides(previous) })

	for _, tc := range []struct {
		model                             string
		input, cached, output             float64
		longInput, longCached, longOutput float64
		ultrafast                         float64
		ultrafastAfter                    float64
	}{
		{"gpt-6.2-sol", 2, 0.1, 10, 4, 0.2, 15, 3, 6},
		{"gpt-6.2-astra", 8, 0.4, 40, 16, 0.8, 60, 3, 6},
	} {
		for _, model := range []string{
			tc.model,
			strings.ToUpper(tc.model),
			"models/" + tc.model,
			tc.model + "-high",
			tc.model + "(xhigh)",
		} {
			t.Run(model, func(t *testing.T) {
				if got := CanonicalBillingModelKey(model); got != tc.model {
					t.Fatalf("CanonicalBillingModelKey(%q) = %q, want %q", model, got, tc.model)
				}
				if got := PricingManagementModelKey(model); got != tc.model {
					t.Fatalf("PricingManagementModelKey(%q) = %q, want %q", model, got, tc.model)
				}
				if got := PricingAliasTarget(model); got != "" {
					t.Fatalf("PricingAliasTarget(%q) = %q, want canonical model", model, got)
				}
				p := GetModelPricing(model)
				assertFloatEqual(t, p.InputPricePerMToken, tc.input)
				assertFloatEqual(t, p.CacheReadPricePerMToken, tc.cached)
				assertFloatEqual(t, p.OutputPricePerMToken, tc.output)
				assertFloatEqual(t, p.LongInputPricePerMToken, tc.longInput)
				assertFloatEqual(t, p.LongCacheReadPricePerMToken, tc.longCached)
				assertFloatEqual(t, p.LongOutputPricePerMToken, tc.longOutput)
				assertFloatEqual(t, p.UltrafastMultiplier, tc.ultrafast)
				assertFloatEqual(t, p.UltrafastMultiplierAfter, tc.ultrafastAfter)
			})
		}
	}
}

func TestGPT62UltrafastMultiplierResolvesByTime(t *testing.T) {
	previous := currentModelPricingOverrides()
	SetModelPricingOverrides(nil)
	t.Cleanup(func() { SetModelPricingOverrides(previous) })

	prevNow := ultrafastNow
	t.Cleanup(func() { ultrafastNow = prevNow })

	for _, model := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		p := GetModelPricing(model)

		// 促销期内（2027-03-01 00:00 太平洋时间前）：3×
		ultrafastNow = func() time.Time { return time.Date(2027, 2, 28, 23, 59, 0, 0, time.UTC) }
		assertFloatEqual(t, resolveUltrafastMultiplier(p), 3)

		// 促销期后（2027-03-01 00:00 太平洋时间起）：6×
		ultrafastNow = func() time.Time { return time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC) }
		assertFloatEqual(t, resolveUltrafastMultiplier(p), 6)
	}
}

func TestUsageLogHistoricalUltrafastUsesRecordTime(t *testing.T) {
	previous := currentModelPricingOverrides()
	SetModelPricingOverrides(nil)
	t.Cleanup(func() { SetModelPricingOverrides(previous) })

	prevNow := ultrafastNow
	t.Cleanup(func() { ultrafastNow = prevNow })

	// 当前时钟已在促销期之后；历史日志仍应按记录的计费时间解析倍率。
	ultrafastNow = func() time.Time { return time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC) }

	// 促销期内（2027-02-28）计费的记录：3×
	early := &UsageLogInput{Model: "gpt-6.2-sol", BillingServiceTier: "ultrafast", InputTokens: 1000, CreatedAt: time.Date(2027, 2, 28, 0, 0, 0, 0, time.UTC)}
	gotEarly := UsageLogCostBreakdown(early)
	assertFloatEqual(t, gotEarly.ServiceTierCostMultiplier, 3)
	assertFloatEqual(t, gotEarly.TotalCost, 1000.0/1e6*2*3)

	// 促销期后（2027-06-01）计费的记录：6×
	late := &UsageLogInput{Model: "gpt-6.2-sol", BillingServiceTier: "ultrafast", InputTokens: 1000, CreatedAt: time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)}
	gotLate := UsageLogCostBreakdown(late)
	assertFloatEqual(t, gotLate.ServiceTierCostMultiplier, 6)
	assertFloatEqual(t, gotLate.TotalCost, 1000.0/1e6*2*6)

	// CreatedAt 为零值的实时请求：回退当前时钟（6×）
	live := &UsageLogInput{Model: "gpt-6.2-sol", BillingServiceTier: "ultrafast", InputTokens: 1000}
	assertFloatEqual(t, UsageLogCostBreakdown(live).ServiceTierCostMultiplier, 6)
}

func TestGPT6SolAndLunaCostBreakdown(t *testing.T) {
	previous := currentModelPricingOverrides()
	SetModelPricingOverrides(nil)
	t.Cleanup(func() { SetModelPricingOverrides(previous) })

	for _, tc := range []struct {
		model                 string
		input, cached, output float64
	}{
		{"gpt-6-sol", 2, 0.2, 10},
		{"gpt-6-luna", 0.1, 0.01, 0.5},
	} {
		for _, inputTokens := range []int{271999, 272001} {
			for _, tier := range []struct {
				name       string
				multiplier float64
			}{{"", 1}, {"fast", 2}, {"priority", 2}} {
				t.Run(fmt.Sprintf("%s/%d/%s", tc.model, inputTokens, tier.name), func(t *testing.T) {
					const cachedTokens, outputTokens = 100000, 1000
					input, cached, output := tc.input, tc.cached, tc.output
					long := inputTokens > 272000
					if long {
						input *= 2
						cached *= 2
						output *= 1.5
					}
					got := CalculateCostBreakdown(inputTokens, outputTokens, cachedTokens, tc.model, tier.name)
					if got.LongContext != long {
						t.Fatalf("long context = %v, want %v", got.LongContext, long)
					}
					assertFloatEqual(t, got.InputPricePerMToken, input*tier.multiplier)
					assertFloatEqual(t, got.CacheReadPricePerMToken, cached*tier.multiplier)
					assertFloatEqual(t, got.OutputPricePerMToken, output*tier.multiplier)
					want := (float64(inputTokens-cachedTokens)*input + cachedTokens*cached + outputTokens*output) / 1e6 * tier.multiplier
					assertFloatEqual(t, got.TotalCost, want)
				})
			}
		}
	}
}

func TestGPT6SolAndLunaOverridesStayIndependentOfAstra(t *testing.T) {
	previous := currentModelPricingOverrides()
	t.Cleanup(func() { SetModelPricingOverrides(previous) })
	SetModelPricingOverrides(map[string]ModelPricingOverride{
		"gpt-6-astra": {Source: ModelPricingSourceCustom, Input: 90, Output: 190},
		"gpt-6-sol": {
			Source: ModelPricingSourceSynced, Input: 3, CachedInput: 0.3, Output: 11,
			InputLong: 6, CachedInputLong: 0.6, OutputLong: 16,
		},
		"gpt-6-luna": {
			Source: ModelPricingSourceCustom, Input: 0.15, CachedInput: 0.015, Output: 0.6,
			InputLong: 0.3, CachedInputLong: 0.03, OutputLong: 0.9,
		},
	})

	for _, tc := range []struct {
		model              string
		input, output      float64
		longInput, longOut float64
	}{
		{"gpt-6-sol", 3, 11, 6, 16},
		{"gpt-6-luna", 0.15, 0.6, 0.3, 0.9},
	} {
		t.Run(tc.model, func(t *testing.T) {
			p := GetModelPricing(tc.model + "(xhigh)")
			assertFloatEqual(t, p.InputPricePerMToken, tc.input)
			assertFloatEqual(t, p.OutputPricePerMToken, tc.output)
			assertFloatEqual(t, p.LongInputPricePerMToken, tc.longInput)
			assertFloatEqual(t, p.LongOutputPricePerMToken, tc.longOut)
		})
	}
	astra := GetModelPricing("gpt-6-astra")
	assertFloatEqual(t, astra.InputPricePerMToken, 90)
	assertFloatEqual(t, astra.OutputPricePerMToken, 190)
}
