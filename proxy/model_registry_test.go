package proxy

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
)

func newTestModelRegistryDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestParseOfficialCodexModelIDs(t *testing.T) {
	html := `
		<astro-island props="{&quot;name&quot;:[0,&quot;gpt-5.5&quot;]}"></astro-island>
		<code>codex -m gpt-5.4</code>
		<code>codex -m gpt-5.3-codex-spark</code>
		<code>codex -m gpt-5.2</code>
		<code>codex -m gpt-5.2-codex</code>
		<code>codex -m gpt-4.1</code>
	`
	models, skipped := ParseOfficialCodexModelIDs(html)
	for _, model := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.3-codex-spark"} {
		if !slices.Contains(models, model) {
			t.Fatalf("parsed models missing %q in %v", model, models)
		}
	}
	// 5.3 只保留 spark；gpt-5.2 及以下、gpt-5.2-codex、gpt-4.1 均被过滤。
	for _, model := range []string{"gpt-5.2", "gpt-5.2-codex", "gpt-4.1"} {
		if !slices.Contains(skipped, model) {
			t.Fatalf("skipped models missing %q in %v", model, skipped)
		}
	}
	if slices.Contains(models, "gpt-5.2") {
		t.Fatalf("gpt-5.2 should be filtered out, got %v", models)
	}
}

func TestApplyOfficialCodexModelSyncMergesWithBuiltinImageModel(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	html := `gpt-5.5 gpt-5.4 gpt-5.4-mini gpt-5.3-codex gpt-5.3-codex-spark gpt-5.2 gpt-5.2-codex gpt-4.1`

	result, err := ApplyOfficialCodexModelSync(ctx, db, html, time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ApplyOfficialCodexModelSync error: %v", err)
	}
	for _, model := range []string{"gpt-image-2", "gpt-image-2-2k", "gpt-image-2-4k"} {
		if !slices.Contains(result.Models, model) {
			t.Fatalf("sync should keep builtin image model %q, got %v", model, result.Models)
		}
	}
	if !slices.Contains(result.Skipped, "gpt-5.2-codex") {
		t.Fatalf("sync should skip gpt-5.2-codex, got %v", result.Skipped)
	}

	var spark *ModelInfo
	for i := range result.Items {
		if result.Items[i].ID == "gpt-5.3-codex-spark" {
			spark = &result.Items[i]
			break
		}
	}
	if spark == nil || !spark.ProOnly {
		t.Fatalf("spark model should be marked pro_only, got %#v", spark)
	}
}

func TestDynamicModelRegistryAffectsValidationImmediately(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	err := db.UpsertModelRegistryRows(ctx, []database.ModelRegistryRow{
		{
			ID:                  "gpt-6.0",
			Enabled:             true,
			Category:            ModelCategoryCodex,
			Source:              ModelSourceOfficialCodexDocs,
			APIKeyAuthAvailable: true,
		},
	})
	if err != nil {
		t.Fatalf("UpsertModelRegistryRows error: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	models := handler.supportedModelIDs(ctx)
	if !slices.Contains(models, "gpt-6.0") {
		t.Fatalf("runtime supported models missing synced model: %v", models)
	}

	result := api.ValidateResponsesAPIRequest([]byte(`{"model":"gpt-6.0","input":"hello"}`), models)
	if !result.Valid {
		t.Fatalf("synced model should pass validation: %#v", result.Errors)
	}
}

func TestReasoningEffortModelsAreIncludedInCatalog(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings error: %v", err)
	}
	if settings == nil {
		settings = &database.SystemSettings{
			SiteName:                         "CodexProxy",
			MaxConcurrency:                   2,
			TestModel:                        "gpt-5.4",
			TestConcurrency:                  50,
			BackgroundRefreshIntervalMinutes: 2,
			UsageProbeMaxAgeMinutes:          10,
			UsageProbeConcurrency:            16,
			RecoveryProbeIntervalMinutes:     30,
			PgMaxConns:                       50,
			RedisPoolSize:                    30,
			MaxRetries:                       2,
			MaxRateLimitRetries:              1,
			ModelMapping:                     "{}",
			CodexModelMapping:                "{}",
			PromptFilterMode:                 "monitor",
			PromptFilterThreshold:            50,
			PromptFilterStrictThreshold:      90,
			PromptFilterLogMatches:           true,
			PromptFilterMaxTextLength:        81920,
			PromptFilterCustomPatterns:       "[]",
			PromptFilterDisabledPatterns:     "[]",
			ClientCompatMode:                 "preserve",
			CodexMinCLIVersion:               "0.118.0",
			UsageLogMode:                     "full",
			UsageLogBatchSize:                200,
			UsageLogFlushIntervalSeconds:     5,
			StreamFlushPolicy:                "immediate",
			StreamFlushIntervalMS:            20,
			BillingTierPolicy:                "actual",
			ImageStorageConfig:               "{}",
			SchedulerMode:                    "round_robin",
			AffinityMode:                     "bounded",
			BackgroundConfig:                 "{}",
		}
	}
	settings.ReasoningEffortModels = `[{"model":"gpt-5.5","effort":"xhigh"}]`
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings error: %v", err)
	}

	catalog, err := ListModelCatalog(ctx, db)
	if err != nil {
		t.Fatalf("ListModelCatalog error: %v", err)
	}
	if !slices.Contains(catalog.Models, "gpt-5.5(xhigh)") {
		t.Fatalf("catalog models missing reasoning alias: %v", catalog.Models)
	}

	var aliasInfo *ModelInfo
	for i := range catalog.Items {
		if catalog.Items[i].ID == "gpt-5.5(xhigh)" {
			aliasInfo = &catalog.Items[i]
			break
		}
	}
	if aliasInfo == nil {
		t.Fatalf("catalog items missing reasoning alias: %#v", catalog.Items)
	}
	if aliasInfo.Source != ModelSourceReasoningEffort {
		t.Fatalf("alias source = %q, want %q", aliasInfo.Source, ModelSourceReasoningEffort)
	}
	if aliasInfo.Category != ModelCategoryCodex {
		t.Fatalf("alias category = %q, want %q", aliasInfo.Category, ModelCategoryCodex)
	}
	if slices.Contains(TextTestModelIDs(ctx, db), "gpt-5.5(xhigh)") {
		t.Fatalf("reasoning alias should not be used for direct connection tests")
	}
}

func TestExtractManifestModelSlugs(t *testing.T) {
	manifest := []byte(`{"models":[
		{"slug":"gpt-5.5","prefer_websockets":true},
		{"slug":"gpt-5.5"},
		{"slug":"  "},
		{"slug":"bad slug with spaces"},
		{"slug":"gpt-9-new"}
	]}`)
	got := ExtractManifestModelSlugs(manifest)
	want := []string{"gpt-5.5", "gpt-9-new"}
	if !slices.Equal(got, want) {
		t.Fatalf("ExtractManifestModelSlugs = %v, want %v", got, want)
	}

	if got := ExtractManifestModelSlugs([]byte(`{"data":[{"id":"x"}]}`)); got != nil {
		t.Fatalf("non-manifest shape should yield nil, got %v", got)
	}
	if got := ExtractManifestModelSlugs(nil); got != nil {
		t.Fatalf("empty body should yield nil, got %v", got)
	}
}

func TestLearnModelsFromManifest_AddsOnlyUnknownAndNeverTouchesExisting(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()

	// 预置一条管理员禁用的行:学习绝不能翻案。
	disabled := database.ModelRegistryRow{
		ID: "gpt-old", Enabled: false, Category: ModelCategoryCodex, Source: "manual",
		APIKeyAuthAvailable: true,
	}
	if err := db.UpsertModelRegistryRows(ctx, []database.ModelRegistryRow{disabled}); err != nil {
		t.Fatalf("seed disabled row: %v", err)
	}

	manifest := []byte(`{"models":[
		{"slug":"gpt-5.5"},
		{"slug":"gpt-old"},
		{"slug":"gpt-9.9-new"}
	]}`)
	added, err := LearnModelsFromManifest(ctx, db, manifest, time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("LearnModelsFromManifest error: %v", err)
	}
	if !slices.Equal(added, []string{"gpt-9.9-new"}) {
		t.Fatalf("added = %v, want [gpt-9.9-new] (builtin/existing must be skipped)", added)
	}

	rows, err := db.ListModelRegistry(ctx)
	if err != nil {
		t.Fatalf("ListModelRegistry: %v", err)
	}
	var sawNew, sawOld bool
	for _, row := range rows {
		switch row.ID {
		case "gpt-9.9-new":
			sawNew = true
			if !row.Enabled || row.Source != ModelSourceUpstreamManifest {
				t.Fatalf("learned row = %+v, want enabled with source %s", row, ModelSourceUpstreamManifest)
			}
		case "gpt-old":
			sawOld = true
			if row.Enabled {
				t.Fatal("disabled row must never be re-enabled by manifest learning")
			}
		}
	}
	if !sawNew || !sawOld {
		t.Fatalf("registry rows missing expected entries: new=%v old=%v", sawNew, sawOld)
	}

	// 学习后的模型立即进入请求侧支持列表。
	if !slices.Contains(SupportedModelIDs(ctx, db), "gpt-9.9-new") {
		t.Fatal("learned model should appear in SupportedModelIDs immediately")
	}
	if slices.Contains(SupportedModelIDs(ctx, db), "gpt-old") {
		t.Fatal("disabled model must stay out of SupportedModelIDs")
	}
}

func TestLearnModelsFromManifest_AllKnownIsNoOp(t *testing.T) {
	db := newTestModelRegistryDB(t)
	added, err := LearnModelsFromManifest(context.Background(), db,
		[]byte(`{"models":[{"slug":"gpt-5.5"},{"slug":"gpt-5.4"}]}`), time.Now().UTC())
	if err != nil {
		t.Fatalf("LearnModelsFromManifest error: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added = %v, want empty for builtin-only manifest", added)
	}
	rows, err := db.ListModelRegistry(context.Background())
	if err != nil {
		t.Fatalf("ListModelRegistry: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("no rows should be written for all-known manifest, got %d", len(rows))
	}
}

// 上游同步/学习的模型准入策略：5.4+ 放行，5.3 仅 spark，5.2 及以下下线。
func TestIsAllowedUpstreamCodexModel_Policy(t *testing.T) {
	cases := map[string]bool{
		"gpt-5.6-sol":         true,
		"gpt-5.5":             true,
		"gpt-5.4":             true,
		"gpt-5.4-mini":        true,
		"gpt-6.0":             true,
		"gpt-5.3-codex-spark": true,
		"gpt-5.3-codex":       false,
		"gpt-5.3":             false,
		"gpt-5.2":             false,
		"gpt-5.2-codex":       false,
		"gpt-5.1-codex":       false,
		"gpt-4.1":             false,
		"gpt-4o":              false,
		"gpt-image-2":         false,
		"":                    false,
	}
	for id, want := range cases {
		if got := isAllowedUpstreamCodexModel(id); got != want {
			t.Errorf("isAllowedUpstreamCodexModel(%q) = %v, want %v", id, got, want)
		}
	}
}
