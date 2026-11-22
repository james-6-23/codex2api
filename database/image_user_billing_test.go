package database

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestImageUserBillingPolicyAndSnapshot(t *testing.T) {
	previous := currentModelPricingOverrides()
	t.Cleanup(func() { SetModelPricingOverrides(previous) })
	SetModelPricingOverrides(map[string]ModelPricingOverride{"gpt-image-2.5-flare": {UserBillingMode: UserBillingModePerImage, ImageUnitPrice: .05}})
	for _, tc := range []struct {
		name          string
		status, count int
		retry         bool
		message       string
		want          float64
	}{
		{"success", 200, 2, false, "", .1}, {"empty", 200, 0, false, "", 0}, {"failed", 502, 2, false, "failure", 0},
		{"canceled", 499, 2, false, "", 0}, {"internal retry", 200, 2, true, "", 0}, {"stream failure", 200, 2, false, "incomplete", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &UsageLogInput{Model: "gpt-image-2.5-flare-2026-09-08-4k", StatusCode: tc.status, ImageCount: tc.count, IsRetryAttempt: tc.retry, ErrorMessage: tc.message, InputTokens: 1000, OutputTokens: 100}
			if got := UsageLogUserBilledCost(input); !approxEqual(got, tc.want) {
				t.Fatalf("fee %v want %v", got, tc.want)
			}
			if got := UsageLogBilledCost(input); !approxEqual(got, .008) {
				t.Fatalf("upstream cost changed: %v", got)
			}
		})
	}
	input := &UsageLogInput{Model: "gpt-image-2.5-flare", StatusCode: 200, ImageCount: 2, InputTokens: 1000, OutputTokens: 100}
	frozen := SnapshotUsageLogBilling(input)
	SetModelPricingOverrides(map[string]ModelPricingOverride{"gpt-image-2.5-flare": {UserBillingMode: UserBillingModePerImage, ImageUnitPrice: .25, Input: 50}})
	if UsageLogUserBilledCost(frozen) != .1 || !approxEqual(UsageLogBilledCost(frozen), .008) {
		t.Fatal("snapshot changed after repricing")
	}
	if got := UsageLogUserBilledCost(WithDeliveredImageCount(frozen, 1)); got != .05 {
		t.Fatalf("partial delivery = %v", got)
	}
	if got := UsageLogUserBilledCost(WithDeliveredImageCount(frozen, 0)); got != 0 {
		t.Fatalf("failed save = %v", got)
	}
	if input.billingSnapshot != nil || UsageLogUserBilledCost(input) != .5 {
		t.Fatal("snapshot mutated caller")
	}
	SetModelPricingOverrides(nil)
	if UsageLogUserBilledCost(input) != UsageLogBilledCost(input) {
		t.Fatal("token default changed")
	}
	text := &UsageLogInput{Model: "gpt-5.6-sol", StatusCode: 200, ImageCount: 2, InputTokens: 1000}
	if SnapshotUsageLogBilling(text) != text || UsageLogUserBilledCost(text) != UsageLogBilledCost(text) {
		t.Fatal("inline text/image tool billing changed")
	}
}

func TestValidateImageUserBilling(t *testing.T) {
	for _, tc := range []struct {
		model, mode string
		price       float64
		valid       bool
	}{
		{"gpt-image-2", "per_image", .05, true}, {"gpt-image-2-4k", "per_image", .1, true}, {"gpt-image-2", "token", 0, true},
		{"gpt-image-2", "per_image", 0, false}, {"gpt-image-2", "per_image", -.1, false}, {"gpt-image-2", "unknown", 1, false},
		{"gpt-5.6-sol", "per_image", .05, false}, {"gpt-image-2", "per_image", math.Inf(1), false}, {"gpt-image-2", "per_image", math.NaN(), false},
	} {
		err := ValidateModelUserBilling(tc.model, ModelPricingOverride{UserBillingMode: tc.mode, ImageUnitPrice: tc.price})
		if (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	if _, err := ParseModelPricingOverridesJSON(`{"gpt-image-2":{"user_billing_mode":"per_image"}}`); err == nil {
		t.Fatal("invalid persisted price accepted")
	}
}

func TestImageUserBillingPersistence(t *testing.T) {
	testImageUserBillingPersistence(t, "sqlite", filepath.Join(t.TempDir(), "image-fees.db"))
}
func TestImageUserBillingPersistencePostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires an isolated CODEX2API_TEST_POSTGRES_DSN database")
	}
	testImageUserBillingPersistence(t, "postgres", dsn)
}
func testImageUserBillingPersistence(t *testing.T, driver, dsn string) {
	previous := currentModelPricingOverrides()
	t.Cleanup(func() { SetModelPricingOverrides(previous) })
	db, err := New(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	key, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{Name: "image-fees", Key: "sk-image-fee-test", QuotaLimit: 10})
	if err != nil {
		t.Fatal(err)
	}
	SetModelPricingOverrides(map[string]ModelPricingOverride{"gpt-image-2.5-flare": {UserBillingMode: UserBillingModePerImage, ImageUnitPrice: .05}})
	for _, status := range []int{200, 502} {
		if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: 1, APIKeyID: key, Endpoint: "/v1/images/generations", Model: "gpt-image-2.5-flare", StatusCode: status, ImageCount: 2, InputTokens: 1000, OutputTokens: 100}); err != nil {
			t.Fatal(err)
		}
	}
	// Neither buffered settlement nor historical display may use the new price.
	SetModelPricingOverrides(map[string]ModelPricingOverride{"gpt-image-2.5-flare": {Input: 50, Output: 300}})
	db.FlushUsageLogs()
	row, err := db.GetAPIKeyByID(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !approxEqual(row.QuotaUsed, .1) || !approxEqual(row.TotalUsed, .1) {
		t.Fatalf("quota=%v total=%v", row.QuotaUsed, row.TotalUsed)
	}
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	logs, err := db.ListUsageLogsByTimeRange(ctx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("logs=%d", len(logs))
	}
	for _, l := range logs {
		want, count := 0., 0
		if l.StatusCode == 200 {
			want, count = .1, 2
		}
		if l.UserBillingMode != UserBillingModePerImage || l.ImageUnitPrice != .05 || l.BilledImageCount != count || l.UserBilled != want || l.TotalCost != want || !approxEqual(l.AccountBilled, .008) {
			t.Fatalf("log=%+v", l)
		}
	}
	filter := UsageLogFilter{Start: start, End: end, APIKeyID: &key, Page: 1, PageSize: 10}
	if page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter); err != nil || len(page.Logs) != 2 {
		t.Fatalf("paged logs: %v %v", page, err)
	}
	if all, err := db.ListUsageLogsByFilter(ctx, filter); err != nil || len(all) != 2 {
		t.Fatalf("filtered logs: %v %v", all, err)
	}
	report, err := db.GetAPIKeySelfUsageReport(ctx, key, start, end, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RecentLogs) != 2 {
		t.Fatalf("self logs=%d", len(report.RecentLogs))
	}
	for _, l := range report.RecentLogs {
		if l.TotalCost != l.UserBilled || l.UserBillingMode != UserBillingModePerImage || l.InputCost != 0 {
			t.Fatalf("self billing=%+v", l)
		}
	}
	// Audit filtering must not suppress charging.
	db.SetUsageLogConfig(UsageLogModeOff, 100, 60)
	SetModelPricingOverrides(map[string]ModelPricingOverride{"gpt-image-2": {UserBillingMode: UserBillingModePerImage, ImageUnitPrice: .05}})
	if err := db.InsertUsageLog(ctx, &UsageLogInput{AccountID: 1, APIKeyID: key, Model: "gpt-image-2", StatusCode: 200, ImageCount: 1}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	row, err = db.GetAPIKeyByID(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !approxEqual(row.QuotaUsed, .15) {
		t.Fatalf("audit-off quota=%v", row.QuotaUsed)
	}
}
