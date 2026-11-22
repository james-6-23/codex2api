package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGPTImage25BillingSeparatesImageInputAndCache(t *testing.T) {
	for _, model := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst-4k", "gpt-image-2.5-flare-2026-09-08-2k"} {
		log := &UsageLogInput{Model: model, InputTokens: 1550, ImageInputTokens: 1521, OutputTokens: 515, ImageOutputTokens: 515}
		if got := UsageLogBilledCost(log); !approxEqual(got, 0.027763) {
			t.Fatalf("%s: cost = %v, want 0.027763", model, got)
		}
		log.InputTokens, log.ImageInputTokens, log.OutputTokens, log.ImageOutputTokens = 1000, 800, 100, 100
		log.CachedTokens, log.CachedImageInputTokens = 500, 400
		got := UsageLogCostBreakdown(log)
		if !approxEqual(got.TotalCost, 0.007625) || !approxEqual(got.ImageInputCost, 0.0032) || !approxEqual(got.ImageCacheReadCost, 0.0008) {
			t.Fatalf("%s: cached cost = %+v", model, got)
		}
	}
}

func TestGPTImage25PricingOverrideAndLegacyIsolation(t *testing.T) {
	previous := currentModelPricingOverrides()
	t.Cleanup(func() { SetModelPricingOverrides(previous) })
	SetModelPricingOverrides(map[string]ModelPricingOverride{"gpt-image-2.5-flare": {Source: ModelPricingSourceCustom, ImageInput: 11, CachedImageInput: 3}})
	p := GetModelPricing("gpt-image-2.5-flare-2026-09-08-4k")
	if p.ImageInputPricePerMToken != 11 || p.CacheReadImagePricePerMToken != 3 || p.InputPricePerMToken != 5 || p.OutputPricePerMToken != 30 {
		t.Fatalf("partial image override lost fields: %+v", p)
	}
	if GetModelPricing("gpt-image-2").ImageInputPricePerMToken != 0 {
		t.Fatal("old image pricing changed")
	}
	log := &UsageLogInput{Model: "gpt-5.6-sol", InputTokens: 1000, OutputTokens: 20, ImageInputTokens: 800}
	if UsageLogBilledCost(log) != CalculateCost(1000, 20, 0, log.Model, "") {
		t.Fatal("text model billing changed")
	}
}

func TestGPTImage25UsagePersistence(t *testing.T) {
	testGPTImage25UsagePersistence(t, "sqlite", filepath.Join(t.TempDir(), "image-usage.db"))
}

func TestGPTImage25UsagePersistencePostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires an isolated CODEX2API_TEST_POSTGRES_DSN database")
	}
	testGPTImage25UsagePersistence(t, "postgres", dsn)
}

func testGPTImage25UsagePersistence(t *testing.T, driver, dsn string) {
	t.Helper()
	db, err := New(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	log := &UsageLogInput{AccountID: 1, APIKeyID: 1, Endpoint: "/v1/images/edits", Model: "gpt-image-2.5-sunburst", StatusCode: 200, InputTokens: 1000, OutputTokens: 100, CachedTokens: 500, ImageInputTokens: 800, ImageOutputTokens: 100, CachedImageInputTokens: 400}
	if err := db.InsertUsageLog(ctx, log); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	logs, err := db.ListUsageLogsByTimeRange(ctx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d", len(logs))
	}
	got := logs[0]
	if got.ImageInputTokens != 800 || got.ImageOutputTokens != 100 || got.CachedImageInputTokens != 400 || !approxEqual(got.AccountBilled, 0.007625) || !approxEqual(got.TotalCost, 0.007625) {
		t.Fatalf("persisted image usage = %+v", got)
	}
	self, _, _, _, err := db.listAPIKeySelfRecentLogs(ctx, 1, start, end, 1, 10, APIKeySelfLogFilter{}, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(self) != 1 || self[0].ImageInputTokens != 800 || !approxEqual(self[0].TotalCost, 0.007625) {
		t.Fatalf("self usage = %+v", self)
	}
}
