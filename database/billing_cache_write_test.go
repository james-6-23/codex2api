package database

import (
	"math"
	"testing"
)

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCalculateCostBreakdownWithCacheWrites_AnthropicPricing(t *testing.T) {
	// claude-opus-5: input 5, cache read 0.5, write 5m 6.25, write 1h 10, output 25 (USD / 1M).
	// InputTokens carries Anthropic's total input (uncached + cache read + cache writes).
	bd := CalculateCostBreakdownWithCacheWrites(6000, 100, 4000, 1000, 500, "claude-opus-5", "")
	wantInput := 500.0 / 1e6 * 5
	wantRead := 4000.0 / 1e6 * 0.5
	wantW5m := 1000.0 / 1e6 * 6.25
	wantW1h := 500.0 / 1e6 * 10
	wantOut := 100.0 / 1e6 * 25
	if !approxEqual(bd.InputCost, wantInput) {
		t.Fatalf("InputCost = %v, want %v (only the 500 uncached tokens)", bd.InputCost, wantInput)
	}
	if !approxEqual(bd.CacheReadCost, wantRead) || !approxEqual(bd.CacheWrite5mCost, wantW5m) || !approxEqual(bd.CacheWrite1hCost, wantW1h) {
		t.Fatalf("cache costs = read %v / 5m %v / 1h %v, want %v / %v / %v", bd.CacheReadCost, bd.CacheWrite5mCost, bd.CacheWrite1hCost, wantRead, wantW5m, wantW1h)
	}
	if !approxEqual(bd.TotalCost, wantInput+wantRead+wantW5m+wantW1h+wantOut) {
		t.Fatalf("TotalCost = %v, want %v", bd.TotalCost, wantInput+wantRead+wantW5m+wantW1h+wantOut)
	}
	if bd.CacheWrite5mPricePerMToken != 6.25 || bd.CacheWrite1hPricePerMToken != 10 {
		t.Fatalf("write prices = %v / %v, want 6.25 / 10", bd.CacheWrite5mPricePerMToken, bd.CacheWrite1hPricePerMToken)
	}
}

func TestCalculateCostBreakdownWithCacheWrites_CacheReadLargerThanUncachedInput(t *testing.T) {
	// The production shape: input_tokens=2, cache_read=235996 → total input 235998.
	bd := CalculateCostBreakdownWithCacheWrites(235998, 274, 235996, 0, 0, "claude-opus-5", "")
	wantRead := 235996.0 / 1e6 * 0.5
	if !approxEqual(bd.CacheReadCost, wantRead) {
		t.Fatalf("CacheReadCost = %v, want %v (must not be clamped away)", bd.CacheReadCost, wantRead)
	}
	if !approxEqual(bd.InputCost, 2.0/1e6*5) {
		t.Fatalf("InputCost = %v, want %v", bd.InputCost, 2.0/1e6*5)
	}
}

func TestCalculateCostBreakdownWithCacheWrites_DefaultsToInputMultiples(t *testing.T) {
	// gpt-5.4 has no explicit cache-write prices: fall back to 1.25x / 2x of the input price.
	bd := CalculateCostBreakdownWithCacheWrites(3000, 0, 0, 1000, 1000, "gpt-5.4", "")
	if !approxEqual(bd.CacheWrite5mPricePerMToken, bd.InputPricePerMToken*1.25) || !approxEqual(bd.CacheWrite1hPricePerMToken, bd.InputPricePerMToken*2) {
		t.Fatalf("default write prices = %v / %v for input %v", bd.CacheWrite5mPricePerMToken, bd.CacheWrite1hPricePerMToken, bd.InputPricePerMToken)
	}
	if !approxEqual(bd.InputCost, 1000.0/1e6*bd.InputPricePerMToken) {
		t.Fatalf("InputCost must exclude cache-write tokens: %v", bd.InputCost)
	}
}

func TestCalculateCostBreakdown_UnchangedWithoutCacheWrites(t *testing.T) {
	legacy := CalculateCostBreakdown(1000, 500, 200, "gpt-5.4", "")
	next := CalculateCostBreakdownWithCacheWrites(1000, 500, 200, 0, 0, "gpt-5.4", "")
	if !approxEqual(legacy.TotalCost, next.TotalCost) || legacy.TotalCost != 0.00955 {
		t.Fatalf("legacy path changed: %v vs %v", legacy.TotalCost, next.TotalCost)
	}
}

func TestUsageLogBilledCostIncludesCacheWrites(t *testing.T) {
	log := &UsageLogInput{Model: "claude-opus-5", InputTokens: 6000, OutputTokens: 100, CachedTokens: 4000, CacheWrite5mTokens: 1000, CacheWrite1hTokens: 500}
	got := UsageLogBilledCost(log)
	want := CalculateCostBreakdownWithCacheWrites(6000, 100, 4000, 1000, 500, "claude-opus-5", "").TotalCost
	if !approxEqual(got, want) || got < 0.018 {
		t.Fatalf("UsageLogBilledCost = %v, want %v", got, want)
	}
}
