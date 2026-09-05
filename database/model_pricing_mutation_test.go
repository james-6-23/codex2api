package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestAstraPricingStaysFlatAcrossSettingsLoadAndMutation(t *testing.T) {
	t.Cleanup(func() { SetModelPricingOverrides(nil) })
	db, err := New("sqlite", filepath.Join(t.TempDir(), "astra-pricing.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	const legacy = `{
		"gpt-6-astra": {"source":"custom","input":10,"cached_input":1,"output":50,
			"input_priority":20,"cached_input_priority":2,"output_priority":100,
			"input_long":20,"cached_input_long":2,"output_long":75,
			"input_long_priority":40,"cached_input_long_priority":4,"output_long_priority":150,
			"long_context_threshold_tokens":272000},
		"gpt-5.4": {"source":"custom","input_long":7,"output_long":30}
	}`
	if err := db.UpdateModelPricingSettings(ctx, legacy, ""); err != nil {
		t.Fatalf("seed legacy settings: %v", err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("read legacy settings: %v", err)
	}
	loaded, err := ParseModelPricingOverridesJSON(settings.ModelPricingOverrides)
	if err != nil {
		t.Fatalf("load pricing: %v", err)
	}
	want := ModelPricingOverride{
		Source: ModelPricingSourceCustom, Input: 10, CachedInput: 1, Output: 50,
		InputPriority: 20, CachedInputPriority: 2, OutputPriority: 100,
	}
	if loaded["gpt-6-astra"] != want {
		t.Fatalf("loaded Astra pricing retained legacy tiers: %+v", loaded["gpt-6-astra"])
	}
	SetModelPricingOverrides(loaded)
	assertFloatEqual(t, CalculateCost(300000, 1000, 100000, "gpt-6-astra", "fast"), 4.3)

	// 所有手工/官方/JSON 同步共用此写入路径；旧 API 长档不能再次落入生效配置。
	var incoming map[string]ModelPricingOverride
	if err := json.Unmarshal([]byte(legacy), &incoming); err != nil {
		t.Fatalf("decode incoming pricing: %v", err)
	}
	updated, err := db.MutateModelPricingSettings(ctx, nil, func(current map[string]ModelPricingOverride) error {
		current["gpt-6-astra"] = incoming["gpt-6-astra"]
		return nil
	})
	if err != nil {
		t.Fatalf("mutate pricing: %v", err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("read persisted pricing: %v", err)
	}
	// 直接解码存储值，避免读取时的归一化掩盖持久化遗漏。
	var stored map[string]ModelPricingOverride
	if err := json.Unmarshal([]byte(settings.ModelPricingOverrides), &stored); err != nil {
		t.Fatalf("decode stored pricing: %v", err)
	}
	if stored["gpt-6-astra"] != want || updated["gpt-6-astra"] != want {
		t.Fatalf("Astra tiers survived mutation: stored=%+v returned=%+v", stored["gpt-6-astra"], updated["gpt-6-astra"])
	}
	if stored["gpt-5.4"] != incoming["gpt-5.4"] {
		t.Fatalf("other model pricing changed: %+v", stored["gpt-5.4"])
	}
	SetModelPricingOverrides(nil)
	SetModelPricingOverrides(stored)
	assertFloatEqual(t, CalculateCost(300000, 1000, 100000, "gpt-6-astra", "fast"), 4.3)
}

func TestMutateModelPricingSettingsSerializesReadMergeWrite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "pricing.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := db.MutateModelPricingSettings(context.Background(), nil, func(current map[string]ModelPricingOverride) error {
			close(firstEntered)
			<-releaseFirst
			current["gpt-first"] = ModelPricingOverride{Source: ModelPricingSourceCustom, Input: 1}
			return nil
		})
		firstDone <- err
	}()
	<-firstEntered

	secondDone := make(chan error, 1)
	go func() {
		_, err := db.MutateModelPricingSettings(context.Background(), nil, func(current map[string]ModelPricingOverride) error {
			current["gpt-second"] = ModelPricingOverride{Source: ModelPricingSourceSynced, Input: 2}
			return nil
		})
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		t.Fatalf("second mutation bypassed coordinator: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first mutation: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second mutation: %v", err)
	}

	settings, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	overrides, err := ParseModelPricingOverridesJSON(settings.ModelPricingOverrides)
	if err != nil {
		t.Fatalf("ParseModelPricingOverridesJSON: %v", err)
	}
	if overrides["gpt-first"].Input != 1 || overrides["gpt-second"].Input != 2 {
		t.Fatalf("serialized mutations lost data: %+v", overrides)
	}
}

func TestMutateModelPricingSettingsFailsClosedOnCorruptJSON(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "pricing-corrupt.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := db.UpdateModelPricingSettings(ctx, `{"gpt-keep":{"source":"custom","input":9}}`, ""); err != nil {
		t.Fatalf("seed pricing JSON: %v", err)
	}
	const corrupt = `{"gpt-keep":{"source":"custom","input":9}`
	if _, err := db.conn.ExecContext(ctx, `UPDATE system_settings SET model_pricing_overrides = $1 WHERE id = 1`, corrupt); err != nil {
		t.Fatalf("inject corrupt JSON: %v", err)
	}
	_, err = db.MutateModelPricingSettings(ctx, nil, func(current map[string]ModelPricingOverride) error {
		current["gpt-new"] = ModelPricingOverride{Source: ModelPricingSourceSynced, Input: 1}
		return nil
	})
	if err == nil {
		t.Fatal("expected corrupt pricing JSON to fail closed")
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if settings == nil || settings.ModelPricingOverrides != corrupt {
		t.Fatalf("corrupt blob was rewritten: %q", settings.ModelPricingOverrides)
	}
}
