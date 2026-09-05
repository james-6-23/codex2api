package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		<div data-model-slug="gpt-5.4"></div>
		<astro-island props="{&quot;slug&quot;:[0,&quot;gpt-5.3-codex-spark&quot;]}"></astro-island>
		<div data-model-slug="gpt-5.2"></div>
		<div data-model-slug="gpt-5.2-codex"></div>
		<div data-model-slug="gpt-4.1"></div>
		<div data-model-slug="gpt-6-astra"></div>
	`
	models, skipped := ParseOfficialCodexModelIDs(html)
	for _, model := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.3-codex-spark", "gpt-6-astra"} {
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
	html := ``
	for _, id := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.2", "gpt-5.2-codex", "gpt-4.1"} {
		html += `<div data-model-slug="` + id + `"></div>`
	}

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
		"gpt-6-astra":         true,
		"gpt-5.6-sol":         true,
		"gpt-5.5":             true,
		"gpt-5.4":             true,
		"gpt-5.4-mini":        true,
		"gpt-6.0":             true,
		"gpt-6-astra-v2":      false,
		"gpt-6":               false,
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
		// Trusted Access for Cyber（issue #624）：稳定别名没有数字版本号，
		// 但清单里出现即代表账号真实权益，必须放行；带版本号的 cyber 变体走常规规则。
		"gpt-daybreak-blue-latest": true,
		"gpt-daybreak-red-latest":  true,
		"gpt-5.6-cyber":            true,
		"gpt-5.5-cyber-preview":    true,
		"gpt-":                     false,
		"gpt-4o-mini":              false,
		"gpt-5o":                   false,
		"gpt-.foo":                 false,
		"gpt-_foo":                 false,
		"gpt-+foo":                 false,
		"gpt-daybreak-image":       false,
		"daybreak-blue":            false,
		// 内部变体后缀（个别账号清单里的 gpt-5.6-sol-wm）：不学进注册表。
		"gpt-5.6-sol-wm": false,
		"gpt-6-astra-wm": false,
	}
	for id, want := range cases {
		if got := isAllowedUpstreamCodexModel(id); got != want {
			t.Errorf("isAllowedUpstreamCodexModel(%q) = %v, want %v", id, got, want)
		}
	}
}

// /models/sync 的出站请求必须遵循全局 ProxyURL（issue #371）：官方模型页
// （https）经 HTTP 代理访问时表现为一次 CONNECT。这里用假代理记录 CONNECT
// 目标来证明请求真的走了代理，而不是直连。
func TestSyncOfficialCodexModelsUsesGlobalProxy(t *testing.T) {
	db := newTestModelRegistryDB(t)

	var mu sync.Mutex
	var connectHosts []string
	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodConnect {
			connectHosts = append(connectHosts, r.URL.Host)
		}
		mu.Unlock()
		// 拒绝隧道，令同步以网络错误结束——本测试只关心流量是否经过代理。
		http.Error(w, "tunnel rejected by test proxy", http.StatusBadGateway)
	}))
	defer fakeProxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := SyncOfficialCodexModels(ctx, db, fakeProxy.URL)
	if err == nil {
		t.Fatal("expected sync to fail through rejecting test proxy")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(connectHosts) == 0 {
		t.Fatal("sync request bypassed the configured global proxy (no CONNECT observed)")
	}
	wantHost := "developers.openai.com:443"
	if connectHosts[0] != wantHost {
		t.Fatalf("CONNECT host = %q, want %q", connectHosts[0], wantHost)
	}
}

// 空 proxyURL 保持直连语义：请求应命中目标站点本身（用可覆盖 URL 不可行，
// 官方 URL 是常量——退而验证空代理不会因代理配置失败而报错，走到网络阶段）。
func TestSyncOfficialCodexModelsEmptyProxyStillAttempts(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// 极短超时：无论网络环境如何都快速返回；断言错误是网络类而非参数类。
	_, err := SyncOfficialCodexModels(ctx, db, "")
	if err == nil {
		return // 网络环境好到 50ms 内完成同步也算通过
	}
	if !strings.Contains(err.Error(), "官方模型页面") {
		t.Fatalf("unexpected error kind: %v", err)
	}
}

func TestIsAllowedUpstreamCodexModelAcceptsOnlyKnownMajorOnlyVersion(t *testing.T) {
	// gpt-6-astra 这类没有小数点的新一代型号必须被允许进入注册表。
	for _, id := range []string{"gpt-6-astra"} {
		if !isAllowedUpstreamCodexModel(id) {
			t.Fatalf("%s should be allowed", id)
		}
	}
	for _, id := range []string{"gpt-4", "gpt-4-turbo", "gpt-5", "gpt-5-codex", "gpt-6", "gpt-6-astra-v2", "gpt-7-nova"} {
		if isAllowedUpstreamCodexModel(id) {
			t.Fatalf("%s should be rejected", id)
		}
	}
	models, _ := ParseOfficialCodexModelIDs(`<div data-model-slug="gpt-6-astra"></div> &quot;slug&quot;:[0,&quot;gpt-5.4&quot;]`)
	if !slices.Contains(models, "gpt-6-astra") || !slices.Contains(models, "gpt-5.4") {
		t.Fatalf("parsed models missing gpt-6-astra / gpt-5.4: %v", models)
	}
}

func TestParseOfficialCodexModelIDsIgnoresNonModelContexts(t *testing.T) {
	// 真实官方页里"长得像模型 ID"的噪声：导航文案、锚点、图片文件名，以及用法示例
	// 代码里的家族占位名（codex --model gpt-5.6 / model = "gpt-5.6"）——官方并没有
	// 叫 gpt-5.6 的模型，只有卡片的 data-model-slug / slug / name 属性才算目录条目。
	html := `
		<a href="#gpt-6-astra-in-enterprise">Using GPT-6 Astra</a>
		<img src="/images/api/models/gpt-6-astra-texture.webp" alt="Astra">
		<img src="/images/api/models/gpt-5.6-sol.webp">
		<astro-island props="{&quot;name&quot;:[0,&quot;gpt-6-astra&quot;],&quot;wallpaperUrl&quot;:[0,&quot;/images/api/models/gpt-6-astra-texture.webp&quot;]}"></astro-island>
		<div class="not-prose" data-model-slug="gpt-5.6-sol"><code>codex -m gpt-5.6-sol</code></div>
		<astro-island props="{&quot;slug&quot;:[0,&quot;gpt-5.4-mini&quot;]}"></astro-island>
		<astro-island props="{&quot;code&quot;:[0,&quot;codex --model gpt-5.6&quot;]}"></astro-island>
		<astro-island props="{&quot;code&quot;:[0,&quot;codex exec -m gpt-5.6 \&quot;review\&quot;&quot;]}"></astro-island>
		<span class="shiki-token">"gpt-5.6"</span>
		<code>codex -m gpt-5.5</code>
		<script>{"model":"gpt-5.4"}</script>
	`
	models, skipped := ParseOfficialCodexModelIDs(html)
	want := []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.4-mini"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want exactly %v (skipped=%v)", models, want, skipped)
	}
	for _, model := range want {
		if !slices.Contains(models, model) {
			t.Fatalf("parsed models missing %q in %v", model, models)
		}
	}
	for _, junk := range []string{"gpt-6", "gpt-6-astra-in-enterprise", "gpt-6-astra-texture", "gpt-5.6", "gpt-5.5", "gpt-5.4"} {
		if slices.Contains(models, junk) || slices.Contains(skipped, junk) {
			t.Fatalf("%q should not be extracted at all (models=%v skipped=%v)", junk, models, skipped)
		}
	}
}

// issue #624：Trusted Access for Cyber 账号的清单里带 gpt-daybreak-blue-latest，
// 学习后必须立刻进入请求侧支持列表，否则 /v1/models 不列、/responses 直接拒绝。
func TestLearnModelsFromManifest_AdmitsNonVersionedCyberAlias(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	manifest := []byte(`{"models":[
		{"slug":"gpt-5.5"},
		{"slug":"gpt-daybreak-blue-latest"},
		{"slug":"gpt-5.2-codex"}
	]}`)
	added, err := LearnModelsFromManifest(ctx, db, manifest, time.Now().UTC())
	if err != nil {
		t.Fatalf("LearnModelsFromManifest error: %v", err)
	}
	if !slices.Equal(added, []string{"gpt-daybreak-blue-latest"}) {
		t.Fatalf("added = %v, want [gpt-daybreak-blue-latest] (retired 5.2 must still be rejected)", added)
	}
	catalog, err := ListModelCatalog(ctx, db)
	if err != nil {
		t.Fatalf("ListModelCatalog: %v", err)
	}
	if !slices.Contains(catalog.Models, "gpt-daybreak-blue-latest") {
		t.Fatalf("learned alias missing from catalog: %v", catalog.Models)
	}
	for _, item := range catalog.Items {
		if item.ID == "gpt-daybreak-blue-latest" {
			if item.Source != ModelSourceUpstreamManifest || item.Category != ModelCategoryCodex || !item.Enabled {
				t.Fatalf("learned item = %+v", item)
			}
		}
	}
	if !slices.Contains(SupportedModelIDs(ctx, db), "gpt-daybreak-blue-latest") {
		t.Fatal("learned alias must be accepted by the request-side model gate immediately")
	}
	if isRetiredCodexModel("gpt-daybreak-blue-latest") {
		t.Fatal("non-versioned alias must never be treated as retired")
	}
}

// 官方页是 official_codex_docs 来源行的唯一真值：页面上已不存在的该来源行
// （早期解析噪声如裸 gpt-5.6）随同步删除；manifest 学习 / 手工来源与内置行不动。
func TestApplyOfficialCodexModelSyncPrunesStaleOfficialRows(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	seed := []database.ModelRegistryRow{
		{ID: "gpt-5.6", Enabled: true, Category: ModelCategoryCodex, Source: ModelSourceOfficialCodexDocs, APIKeyAuthAvailable: true},
		{ID: "gpt-daybreak-blue-latest", Enabled: true, Category: ModelCategoryCodex, Source: ModelSourceUpstreamManifest, APIKeyAuthAvailable: true},
		{ID: "gpt-7-manual", Enabled: true, Category: ModelCategoryCodex, Source: "manual", APIKeyAuthAvailable: true},
		// 内置模型即使带 official 来源、页面暂时没列出，也不能删。
		{ID: "gpt-5.4-mini", Enabled: false, Category: ModelCategoryCodex, Source: ModelSourceOfficialCodexDocs, APIKeyAuthAvailable: true},
	}
	if err := db.UpsertModelRegistryRows(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	html := `<div data-model-slug="gpt-6-astra"></div><div data-model-slug="gpt-5.6-sol"></div>`
	result, err := ApplyOfficialCodexModelSync(ctx, db, html, time.Now())
	if err != nil {
		t.Fatalf("ApplyOfficialCodexModelSync: %v", err)
	}
	if !slices.Equal(result.Removed, []string{"gpt-5.6"}) {
		t.Fatalf("Removed = %v, want [gpt-5.6]", result.Removed)
	}
	rows, err := db.ListModelRegistry(ctx)
	if err != nil {
		t.Fatalf("ListModelRegistry: %v", err)
	}
	got := map[string]database.ModelRegistryRow{}
	for _, row := range rows {
		got[row.ID] = row
	}
	if _, ok := got["gpt-5.6"]; ok {
		t.Fatal("stale official row gpt-5.6 should be deleted")
	}
	for _, keep := range []string{"gpt-daybreak-blue-latest", "gpt-7-manual", "gpt-5.4-mini", "gpt-6-astra", "gpt-5.6-sol"} {
		if _, ok := got[keep]; !ok {
			t.Fatalf("row %s should survive the sync", keep)
		}
	}
	if got["gpt-5.4-mini"].Enabled {
		t.Fatal("admin-disabled builtin row must keep enabled=false")
	}
	if slices.Contains(result.Models, "gpt-5.6") {
		t.Fatalf("catalog still exposes gpt-5.6: %v", result.Models)
	}
}

// 同步/学习进来的非内置模型排在列表最前，按版本新→旧；内置列表接在后面。
func TestMergeModelInfosPutsSyncedModelsFirstNewestFirst(t *testing.T) {
	rows := []database.ModelRegistryRow{
		// 早期学进来的内部变体残留行：不再暴露。
		{ID: "gpt-5.6-sol-wm", Enabled: true, Source: ModelSourceUpstreamManifest},
		{ID: "gpt-daybreak-blue-latest", Enabled: true, Source: ModelSourceUpstreamManifest},
		{ID: "gpt-6-nova", Enabled: true, Source: ModelSourceUpstreamManifest},
		{ID: "gpt-5.7-terra", Enabled: true, Source: ModelSourceOfficialCodexDocs},
		{ID: "gpt-5.7-sol", Enabled: true, Source: ModelSourceOfficialCodexDocs},
		{ID: "gpt-7", Enabled: true, Source: ModelSourceUpstreamManifest},
		// 内置行保持内置顺序，不参与前置。
		{ID: "gpt-5.5", Enabled: true, Source: ModelSourceOfficialCodexDocs},
	}
	merged := mergeModelInfos(rows)
	ids := make([]string, 0, len(merged))
	for _, info := range merged {
		ids = append(ids, info.ID)
	}
	wantHead := []string{"gpt-7", "gpt-6-nova", "gpt-5.7-sol", "gpt-5.7-terra", "gpt-daybreak-blue-latest"}
	if !slices.Equal(ids[:len(wantHead)], wantHead) {
		t.Fatalf("head = %v, want %v", ids[:len(wantHead)], wantHead)
	}
	if !slices.Equal(ids[len(wantHead):], BuiltinModelIDs()) {
		t.Fatalf("tail = %v, want builtin order %v", ids[len(wantHead):], BuiltinModelIDs())
	}
	if slices.Contains(ids, "gpt-5.6-sol-wm") {
		t.Fatalf("internal variant row must stay hidden: %v", ids)
	}
}
