package database

import (
	"testing"
)

func TestModelPricingOverride_MergeAndPrecedence(t *testing.T) {
	t.Cleanup(func() { SetModelPricingOverrides(nil) })

	// 基线：gpt-5.4 代码默认 $2.5/$15。
	base := GetModelPricing("gpt-5.4")
	if base.InputPricePerMToken != 2.5 || base.OutputPricePerMToken != 15.0 {
		t.Fatalf("baseline gpt-5.4 = %.2f/%.2f, want 2.5/15", base.InputPricePerMToken, base.OutputPricePerMToken)
	}

	// 部分覆盖：只改 output，input 应保持代码默认。
	SetModelPricingOverrides(map[string]ModelPricingOverride{
		"gpt-5.4": {Source: ModelPricingSourceCustom, Output: 99.0},
	})
	p := GetModelPricing("gpt-5.4")
	if p.OutputPricePerMToken != 99.0 {
		t.Fatalf("overridden output = %.2f, want 99", p.OutputPricePerMToken)
	}
	if p.InputPricePerMToken != 2.5 {
		t.Fatalf("input should stay code default 2.5, got %.2f", p.InputPricePerMToken)
	}
	// 覆盖不能污染共享默认：再查一次未覆盖模型仍为原值。
	if base2 := GetModelPricing("gpt-5.5"); base2.OutputPricePerMToken != 30.0 {
		t.Fatalf("gpt-5.5 leaked to %.2f, want 30", base2.OutputPricePerMToken)
	}

	// gpt-5.6-terra 是独立规范键：不跟随 gpt-5.4 的 custom 覆盖。
	if terra := GetModelPricing("gpt-5.6-terra"); terra.OutputPricePerMToken != 12.0 {
		t.Fatalf("terra should keep own default 12, got %.2f", terra.OutputPricePerMToken)
	}
	// terra 自身覆盖可独立生效。
	SetModelPricingOverrides(map[string]ModelPricingOverride{
		"gpt-5.4":       {Source: ModelPricingSourceCustom, Output: 99.0},
		"gpt-5.6-terra": {Source: ModelPricingSourceCustom, Output: 42.0},
	})
	if terra := GetModelPricing("gpt-5.6-terra"); terra.OutputPricePerMToken != 42.0 {
		t.Fatalf("terra override = %.2f, want 42", terra.OutputPricePerMToken)
	}
	if gpt54 := GetModelPricing("gpt-5.4"); gpt54.OutputPricePerMToken != 99.0 {
		t.Fatalf("gpt-5.4 override = %.2f, want 99", gpt54.OutputPricePerMToken)
	}

	// 清空覆盖 → 回退代码默认。
	SetModelPricingOverrides(nil)
	if p := GetModelPricing("gpt-5.4"); p.OutputPricePerMToken != 15.0 {
		t.Fatalf("after clear, output = %.2f, want 15", p.OutputPricePerMToken)
	}
}

func TestModelPricingOverride_ExactCodexAliasPrecedesCanonicalOverride(t *testing.T) {
	t.Cleanup(func() { SetModelPricingOverrides(nil) })

	// codex-auto-review is an internal alias whose intended billing is the
	// public gpt-5.4 standard price. Keep a deliberately bad canonical override
	// here to prove the alias-specific entry cannot be shadowed by it.
	SetModelPricingOverrides(map[string]ModelPricingOverride{
		"codex-auto-review": {Source: ModelPricingSourceSynced, Input: 2.5, CachedInput: 0.25, Output: 15},
		"gpt-5.4":           {Source: ModelPricingSourceSynced, Input: 15, Output: 120},
	})

	for _, model := range []string{"codex-auto-review", "CODEX_AUTO_REVIEW", "codex auto review"} {
		pricing := GetModelPricing(model)
		if pricing.InputPricePerMToken != 2.5 ||
			pricing.CacheReadPricePerMToken != 0.25 ||
			pricing.OutputPricePerMToken != 15 {
			t.Fatalf("%s alias pricing = %.2f/%.2f/%.2f, want 2.5/0.25/15", model,
				pricing.InputPricePerMToken,
				pricing.CacheReadPricePerMToken,
				pricing.OutputPricePerMToken)
		}
	}
}

func TestModelPricingOverride_SyncedAliasNeverShadowsCustomCanonical(t *testing.T) {
	t.Cleanup(func() { SetModelPricingOverrides(nil) })

	// 管理员手填(custom)的 canonical 价必须压过同步(synced)进来的别名价,
	// 否则一次定价同步就能静默替换运营者的手工定价决策(custom > synced)。
	SetModelPricingOverrides(map[string]ModelPricingOverride{
		"codex-auto-review": {Source: ModelPricingSourceSynced, Input: 2.5, Output: 15},
		"gpt-5.4":           {Source: ModelPricingSourceCustom, Input: 9, Output: 90},
	})
	if pricing := GetModelPricing("codex-auto-review"); pricing.InputPricePerMToken != 9 || pricing.OutputPricePerMToken != 90 {
		t.Fatalf("synced alias shadowed custom canonical: %.2f/%.2f, want 9/90",
			pricing.InputPricePerMToken, pricing.OutputPricePerMToken)
	}

	// custom 别名仍然压过 custom canonical(同源之下更精确的键生效)。
	SetModelPricingOverrides(map[string]ModelPricingOverride{
		"codex-auto-review": {Source: ModelPricingSourceCustom, Input: 2.5, Output: 15},
		"gpt-5.4":           {Source: ModelPricingSourceCustom, Input: 9, Output: 90},
	})
	if pricing := GetModelPricing("codex-auto-review"); pricing.InputPricePerMToken != 2.5 || pricing.OutputPricePerMToken != 15 {
		t.Fatalf("custom alias lost to custom canonical: %.2f/%.2f, want 2.5/15",
			pricing.InputPricePerMToken, pricing.OutputPricePerMToken)
	}
}

func TestModelPricingOverride_PartialAliasFallsBackToCanonicalOverride(t *testing.T) {
	t.Cleanup(func() { SetModelPricingOverrides(nil) })

	// 别名条目只填了 input 时,其余字段应回退 canonical 生效价(90),
	// 而不是无视管理员的 canonical 配置直接掉回代码默认价。
	SetModelPricingOverrides(map[string]ModelPricingOverride{
		"codex-auto-review": {Source: ModelPricingSourceCustom, Input: 2.5},
		"gpt-5.4":           {Source: ModelPricingSourceCustom, Input: 9, Output: 90},
	})
	pricing := GetModelPricing("codex-auto-review")
	if pricing.InputPricePerMToken != 2.5 || pricing.OutputPricePerMToken != 90 {
		t.Fatalf("partial alias merge = %.2f/%.2f, want 2.5/90",
			pricing.InputPricePerMToken, pricing.OutputPricePerMToken)
	}
}

func TestPricingManagementModelKeyPreservesIndependentAlias(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{model: "codex-auto-review", want: "codex-auto-review"},
		{model: "CODEX_AUTO_REVIEW", want: "codex-auto-review"},
		{model: "codex auto review", want: "codex-auto-review"},
		{model: "gpt-5.4", want: "gpt-5.4"},
		{model: "gpt-5.4-openai-compact", want: "gpt-5.4"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := PricingManagementModelKey(tt.model); got != tt.want {
				t.Fatalf("PricingManagementModelKey(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestParseModelPricingOverridesJSON(t *testing.T) {
	m, err := ParseModelPricingOverridesJSON(`{"gpt-5.4":{"source":"custom","input":3,"output":20},"gpt-x":{}}`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("empty override should be dropped, got %d entries: %v", len(m), m)
	}
	if m["gpt-5.4"].Input != 3 || m["gpt-5.4"].Output != 20 {
		t.Fatalf("parsed = %+v", m["gpt-5.4"])
	}
	if empty, _ := ParseModelPricingOverridesJSON(""); len(empty) != 0 {
		t.Fatalf("empty string should parse to empty map")
	}
}

func TestModelPricingOverride_LongPriorityFieldsRoundTripAndApply(t *testing.T) {
	t.Cleanup(func() { SetModelPricingOverrides(nil) })

	override := ModelPricingOverride{
		Source:                     ModelPricingSourceSynced,
		InputLongPriority:          20,
		CachedInputLongPriority:    2,
		OutputLongPriority:         90,
		LongContextThresholdTokens: 200000,
	}
	if override.IsEmpty() {
		t.Fatal("long priority-only override must not be dropped as empty")
	}
	SetModelPricingOverrides(map[string]ModelPricingOverride{"gpt-5.6-sol": override})
	pricing := GetModelPricing("gpt-5.6-sol")
	if pricing.LongInputPricePerMTokenPriority != 20 ||
		pricing.LongCacheReadPricePerMTokenPriority != 2 ||
		pricing.LongOutputPricePerMTokenPriority != 90 ||
		pricing.LongContextThresholdTokens != 200000 {
		t.Fatalf("long priority override not applied: %+v", pricing)
	}

	projected := ModelPricingOverrideFromPricing(pricing, ModelPricingSourceSynced)
	if projected.InputLongPriority != 20 || projected.CachedInputLongPriority != 2 || projected.OutputLongPriority != 90 || projected.LongContextThresholdTokens != 200000 {
		t.Fatalf("long priority projection lost values: %+v", projected)
	}
}
