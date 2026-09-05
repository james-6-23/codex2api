package proxy

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
)

const (
	OfficialCodexModelsURL = "https://developers.openai.com/codex/models"

	ModelCategoryCodex = "codex"
	ModelCategoryImage = "image"

	ModelSourceBuiltin           = "builtin"
	ModelSourceOfficialCodexDocs = "official_codex_docs"
	ModelSourceReasoningEffort   = "reasoning_effort"
	ModelSourceUpstreamManifest  = "upstream_manifest"
)

// ModelInfo describes one model exposed by this proxy.
type ModelInfo struct {
	ID                  string     `json:"id"`
	Enabled             bool       `json:"enabled"`
	Category            string     `json:"category"`
	Source              string     `json:"source"`
	ProOnly             bool       `json:"pro_only"`
	APIKeyAuthAvailable bool       `json:"api_key_auth_available"`
	LastSeenAt          *time.Time `json:"last_seen_at,omitempty"`
	UpdatedAt           *time.Time `json:"updated_at,omitempty"`
}

// ModelCatalog is the admin-facing model list plus registry metadata.
type ModelCatalog struct {
	Models []string    `json:"models"`
	Items  []ModelInfo `json:"items"`
	// GrokModels 是全部 Grok 账号声明模型的并集（由 admin 层填充），
	// 供前端在渠道选 grok 时切换模型下拉选项；注册表本身仍只管 Codex 模型。
	GrokModels        []string   `json:"grok_models,omitempty"`
	AntigravityModels []string   `json:"antigravity_models,omitempty"`
	ClaudeModels      []string   `json:"claude_models,omitempty"`
	LastSyncedAt      *time.Time `json:"last_synced_at,omitempty"`
	SourceURL         string     `json:"source_url"`
	Warning           string     `json:"warning,omitempty"`
}

// ModelSyncResult is returned after a manual upstream sync.
type ModelSyncResult struct {
	Added     int      `json:"added"`
	Updated   int      `json:"updated"`
	Unchanged int      `json:"unchanged"`
	Skipped   []string `json:"skipped"`
	// Removed 是本次同步删掉的 official_codex_docs 来源行（官方页已不存在）。
	Removed      []string    `json:"removed,omitempty"`
	Models       []string    `json:"models"`
	Items        []ModelInfo `json:"items"`
	LastSyncedAt time.Time   `json:"last_synced_at"`
	SourceURL    string      `json:"source_url"`
}

var builtinModelInfos = []ModelInfo{
	// gpt-6-astra：官方模型页与定价页均已收录（$10/$50，长上下文 $20/$75）。
	// 官方文档同步与 manifest 学习都能发现它，但内置一行保证冷启动 / 未同步的
	// 部署也能直接调用，不必等一次同步或一次带清单的请求。
	modelInfoForID("gpt-6-astra", ModelSourceBuiltin),
	// gpt-5.6 系列（Sol/Terra/Luna）：官网已出现的新模型，先内置兜底，
	// 官方文档页同步（SyncOfficialCodexModels）上线后会以同步结果为准。
	modelInfoForID("gpt-5.6-sol", ModelSourceBuiltin),
	modelInfoForID("gpt-5.6-terra", ModelSourceBuiltin),
	modelInfoForID("gpt-5.6-luna", ModelSourceBuiltin),
	modelInfoForID("gpt-5.5", ModelSourceBuiltin),
	modelInfoForID("gpt-5.4", ModelSourceBuiltin),
	modelInfoForID("gpt-5.4-mini", ModelSourceBuiltin),
	// 5.3 只保留 spark 变体；gpt-5.3-codex 及 5.2/更低模型已下线（含上游同步过滤）。
	modelInfoForID("gpt-5.3-codex-spark", ModelSourceBuiltin),
	// codex-auto-review — Codex internal auto-review model.
	// Upstream confirms: returns effective model "gpt-5.4" (tested 2026-05-20).
	// Available on Plus/Pro/Team/Business per official catalog; excludes free.
	// Pricing: gpt-5.4 standard ($2.50/$15.00), priority ($5.00/$30.00).
	// Ref: codex_client_models.json via CLIProxyAPI model registry.
	modelInfoForID("codex-auto-review", ModelSourceBuiltin),
	// gpt-reserve — non-versioned model ID; keep as builtin fallback only.
	// Note: the official docs sync only extracts gpt-<major>[.<minor>] IDs, so it
	// would never be discovered there; manifest learning does admit non-versioned
	// gpt-* slugs (issue #624), but a builtin row is still needed for cold start.
	modelInfoForID("gpt-reserve", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2-2k", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2-4k", ModelSourceBuiltin),
}

// SupportedModels is the static built-in fallback list. Runtime handlers use
// SupportedModelIDs so synced registry entries can take effect immediately.
var SupportedModels = BuiltinModelIDs()

func BuiltinModelIDs() []string {
	ids := make([]string, 0, len(builtinModelInfos))
	for _, model := range builtinModelInfos {
		ids = append(ids, model.ID)
	}
	return ids
}

func modelInfoForID(id string, source string) ModelInfo {
	id = strings.TrimSpace(id)
	if source == "" {
		source = ModelSourceBuiltin
	}
	info := ModelInfo{
		ID:                  id,
		Enabled:             true,
		Category:            ModelCategoryCodex,
		Source:              source,
		APIKeyAuthAvailable: true,
	}
	switch strings.ToLower(id) {
	case "gpt-5.3-codex-spark":
		info.ProOnly = true
	case "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra":
		info.APIKeyAuthAvailable = false
	case "gpt-image-2":
		info.Category = ModelCategoryImage
	}
	if strings.Contains(strings.ToLower(id), "image") {
		info.Category = ModelCategoryImage
	}
	return info
}

func modelInfoFromRow(row database.ModelRegistryRow) ModelInfo {
	var lastSeenAt *time.Time
	if row.LastSeenAt.Valid {
		t := row.LastSeenAt.Time.UTC()
		lastSeenAt = &t
	}
	var updatedAt *time.Time
	if !row.UpdatedAt.IsZero() {
		t := row.UpdatedAt.UTC()
		updatedAt = &t
	}
	return ModelInfo{
		ID:                  row.ID,
		Enabled:             row.Enabled,
		Category:            valueOrDefault(row.Category, ModelCategoryCodex),
		Source:              valueOrDefault(row.Source, "manual"),
		ProOnly:             row.ProOnly,
		APIKeyAuthAvailable: row.APIKeyAuthAvailable,
		LastSeenAt:          lastSeenAt,
		UpdatedAt:           updatedAt,
	}
}

func modelInfoToRow(info ModelInfo, lastSeenAt time.Time) database.ModelRegistryRow {
	return database.ModelRegistryRow{
		ID:                  info.ID,
		Enabled:             info.Enabled,
		Category:            valueOrDefault(info.Category, ModelCategoryCodex),
		Source:              valueOrDefault(info.Source, "manual"),
		ProOnly:             info.ProOnly,
		APIKeyAuthAvailable: info.APIKeyAuthAvailable,
		LastSeenAt:          sql.NullTime{Time: lastSeenAt.UTC(), Valid: !lastSeenAt.IsZero()},
	}
}

func valueOrDefault(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

// ListModelCatalog returns enabled model IDs plus metadata. It falls back to
// built-ins if the registry cannot be read.
func ListModelCatalog(ctx context.Context, db *database.DB) (ModelCatalog, error) {
	catalog := builtinCatalog()
	if db == nil {
		return catalog, nil
	}

	rows, err := db.ListModelRegistry(ctx)
	if err != nil {
		catalog.Warning = err.Error()
		return catalog, err
	}

	merged := mergeModelInfos(rows)
	if settings, settingsErr := db.GetSystemSettings(ctx); settingsErr == nil && settings != nil {
		merged = appendReasoningEffortModelInfos(merged, settings.ReasoningEffortModels)
	} else if settingsErr != nil && catalog.Warning == "" {
		catalog.Warning = settingsErr.Error()
	}
	catalog.Items = merged
	catalog.Models = enabledModelIDs(merged, false)
	if len(catalog.Models) == 0 {
		catalog.Models = BuiltinModelIDs()
	}

	state, err := db.GetModelRegistrySyncState(ctx)
	if err != nil {
		catalog.Warning = err.Error()
		return catalog, err
	}
	if state != nil {
		catalog.SourceURL = valueOrDefault(state.SourceURL, OfficialCodexModelsURL)
		if state.LastSyncedAt.Valid {
			t := state.LastSyncedAt.Time.UTC()
			catalog.LastSyncedAt = &t
		}
	}
	return catalog, nil
}

func builtinCatalog() ModelCatalog {
	items := append([]ModelInfo(nil), builtinModelInfos...)
	return ModelCatalog{
		Models:    enabledModelIDs(items, false),
		Items:     items,
		SourceURL: OfficialCodexModelsURL,
	}
}

func mergeModelInfos(rows []database.ModelRegistryRow) []ModelInfo {
	byID := make(map[string]ModelInfo, len(builtinModelInfos)+len(rows))
	for _, info := range builtinModelInfos {
		byID[info.ID] = info
	}
	for _, row := range rows {
		info := modelInfoFromRow(row)
		if info.ID == "" {
			continue
		}
		// 退役模型（5.3 非 spark、5.2 及以下、gpt-4*）与内部变体（-wm）即使注册表里
		// 有残留行也不再暴露，保证升级后 DB 旧行不会让它们复现。
		if isRetiredCodexModel(info.ID) || isInternalCodexModelVariant(info.ID) {
			continue
		}
		byID[info.ID] = info
	}

	builtins := make([]ModelInfo, 0, len(builtinModelInfos))
	for _, info := range builtinModelInfos {
		if merged, ok := byID[info.ID]; ok {
			builtins = append(builtins, merged)
			delete(byID, info.ID)
		}
	}
	// 同步/学习进来的非内置模型排在最前，按版本新→旧（gpt-6-x > gpt-5.6-x > …），
	// 同版本按 ID 字典序；没有数字版本的代号族（daybreak 等）排在数字版本之后。
	// 内置列表本身已是手工维护的新→旧顺序，接在后面；否则新发布的型号会被
	// 压到 gpt-image-2-4k 之后、按字母序埋在列表末尾。
	extras := make([]ModelInfo, 0, len(byID))
	for _, info := range byID {
		extras = append(extras, info)
	}
	sort.Slice(extras, func(i, j int) bool {
		return compareModelIDsNewestFirst(extras[i].ID, extras[j].ID) < 0
	})
	return append(extras, builtins...)
}

// compareModelIDsNewestFirst 按"版本新→旧"比较两个模型 ID：先比 ID 中的数字序列
// （逐段数值比较，长者视为更细的版本），有数字的排在无数字的前面，
// 完全相同时按 ID 字典序。返回 <0 表示 a 应排在 b 前面。
func compareModelIDsNewestFirst(a, b string) int {
	if a == b {
		return 0
	}
	pa, pb := modelIDVersionParts(a), modelIDVersionParts(b)
	switch {
	case len(pa) == 0 && len(pb) > 0:
		return 1
	case len(pa) > 0 && len(pb) == 0:
		return -1
	}
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return -1
			}
			return 1
		}
	}
	if len(pa) != len(pb) {
		if len(pa) > len(pb) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

var modelIDVersionNumberPattern = regexp.MustCompile(`\d+`)

func modelIDVersionParts(id string) []int {
	matches := modelIDVersionNumberPattern.FindAllString(strings.ToLower(id), -1)
	if len(matches) == 0 {
		return nil
	}
	parts := make([]int, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m)
		if err != nil {
			continue
		}
		parts = append(parts, n)
	}
	return parts
}

// internalCodexModelVariantSuffixes 是上游清单里会出现、但不该对外暴露的内部变体后缀。
// gpt-5.6-sol-wm 这类 slug 只在个别账号的清单里出现，官方模型页与定价页都没有，
// 学进注册表只会让 /v1/models 多出一个用户不认识、计费也只能按 sol 兜底的条目。
var internalCodexModelVariantSuffixes = []string{"-wm"}

// isInternalCodexModelVariant 判断是否为内部变体 slug：准入时拒绝，注册表里已有的
// 残留行也不再暴露（与退役模型同一处过滤）。
func isInternalCodexModelVariant(id string) bool {
	id = strings.TrimSpace(strings.ToLower(id))
	if !strings.HasPrefix(id, "gpt-") {
		return false
	}
	for _, suffix := range internalCodexModelVariantSuffixes {
		if strings.HasSuffix(id, suffix) {
			return true
		}
	}
	return false
}

// isRetiredCodexModel 判断模型是否已下线（不再对外暴露 / 不参与校验）：
// gpt-5.3 非 spark、gpt-5.2 及更低、gpt-4* 均退役；image、codex-auto-review、
// 非 gpt- 前缀及 5.4+ 保留。是 isAllowedUpstreamCodexModel 的"暴露侧"补集，
// 但对 image/非 gpt 模型返回 false（保留）。
func isRetiredCodexModel(id string) bool {
	id = strings.TrimSpace(strings.ToLower(id))
	if !strings.HasPrefix(id, "gpt-") || strings.Contains(id, "image") {
		return false
	}
	version := strings.TrimPrefix(id, "gpt-")
	if dash := strings.IndexByte(version, '-'); dash >= 0 {
		version = version[:dash]
	}
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	if major > 5 {
		return false
	}
	if major < 5 {
		return true
	}
	if minor >= 4 {
		return false
	}
	if minor == 3 {
		return !strings.Contains(id, "spark")
	}
	return true
}

func appendReasoningEffortModelInfos(items []ModelInfo, settingsJSON string) []ModelInfo {
	entries, _ := parseReasoningEffortModelEntries(settingsJSON, enabledModelIDs(items, false), false)
	if len(entries) == 0 {
		return items
	}

	result := append([]ModelInfo(nil), items...)
	byID := make(map[string]ModelInfo, len(result)+len(entries)*2)
	for _, item := range result {
		byID[strings.ToLower(strings.TrimSpace(item.ID))] = item
	}

	for _, entry := range entries {
		baseKey := strings.ToLower(entry.Model)
		baseInfo, baseExists := byID[baseKey]
		if !baseExists {
			baseInfo = modelInfoForID(entry.Model, ModelSourceReasoningEffort)
			result = append(result, baseInfo)
			byID[baseKey] = baseInfo
		}

		alias := ReasoningEffortModelAlias(entry.Model, entry.Effort)
		if alias == "" {
			continue
		}
		aliasKey := strings.ToLower(alias)
		if _, exists := byID[aliasKey]; exists {
			continue
		}
		aliasInfo := baseInfo
		aliasInfo.ID = alias
		aliasInfo.Source = ModelSourceReasoningEffort
		aliasInfo.Category = ModelCategoryCodex
		aliasInfo.LastSeenAt = nil
		aliasInfo.UpdatedAt = nil
		result = append(result, aliasInfo)
		byID[aliasKey] = aliasInfo
	}
	return result
}

// SupportedModelIDs returns enabled runtime model IDs.
func SupportedModelIDs(ctx context.Context, db *database.DB) []string {
	catalog, _ := ListModelCatalog(ctx, db)
	return catalog.Models
}

// TextTestModelIDs returns enabled non-image models for account connection tests.
func TextTestModelIDs(ctx context.Context, db *database.DB) []string {
	catalog, _ := ListModelCatalog(ctx, db)
	ids := enabledModelIDs(catalog.Items, true)
	filtered := ids[:0]
	for _, id := range ids {
		if strings.Contains(id, "(") || strings.Contains(id, ")") {
			continue
		}
		filtered = append(filtered, id)
	}
	return filtered
}

func IsTextTestModelID(ctx context.Context, db *database.DB, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, id := range TextTestModelIDs(ctx, db) {
		if model == id {
			return true
		}
	}
	return false
}

func enabledModelIDs(items []ModelInfo, textOnly bool) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if !item.Enabled {
			continue
		}
		if textOnly && isImageModelInfo(item) {
			continue
		}
		ids = append(ids, item.ID)
	}
	return ids
}

func isImageModelInfo(info ModelInfo) bool {
	return strings.EqualFold(info.Category, ModelCategoryImage) || strings.Contains(strings.ToLower(info.ID), "image")
}

var codexModelIDPattern = regexp.MustCompile(`\bgpt-[0-9]+(?:\.[0-9]+)*(?:-[a-z][a-z0-9]*(?:-[a-z0-9]+)*)?\b`)

// ParseOfficialCodexModelIDs extracts allowed Codex model IDs from the official docs HTML.
func ParseOfficialCodexModelIDs(html string) (models []string, skipped []string) {
	seen := map[string]struct{}{}
	skippedSeen := map[string]struct{}{}
	lowered := strings.ToLower(html)
	for _, loc := range codexModelIDPattern.FindAllStringIndex(lowered, -1) {
		match := lowered[loc[0]:loc[1]]
		if !isOfficialCodexModelIDContext(lowered, loc[0], loc[1]) {
			continue
		}
		if isAllowedUpstreamCodexModel(match) {
			if _, ok := seen[match]; !ok {
				seen[match] = struct{}{}
				models = append(models, match)
			}
			continue
		}
		if _, ok := skippedSeen[match]; !ok {
			skippedSeen[match] = struct{}{}
			skipped = append(skipped, match)
		}
	}
	sort.SliceStable(models, func(i, j int) bool {
		return modelSortRank(models[i]) < modelSortRank(models[j])
	})
	sort.Strings(skipped)
	return models, skipped
}

// officialCodexModelCatalogContexts 是官方模型页里模型卡片承载 ID 的结构化属性
// （小写；astro-island props 里的引号是 HTML 实体 &quot;，服务端渲染的 DOM 属性是裸引号）：
//   - data-model-slug="<id>"          卡片容器属性
//   - "slug":[0,"<id>"] / "name":[0,"<id>"]   卡片组件 props
var officialCodexModelCatalogContexts = []string{
	`data-model-slug="`,
	`"slug":[0,"`,
	`&quot;slug&quot;:[0,&quot;`,
	`"name":[0,"`,
	`&quot;name&quot;:[0,&quot;`,
}

// isOfficialCodexModelIDContext 只接受官方模型页里**模型目录卡片**承载 ID 的位置
// （见 officialCodexModelCatalogContexts）。页面上其它"长得像模型 ID"的位置一律不算：
// 导航文案（"Using GPT-6 Astra"→gpt-6）、锚点（#gpt-6-astra-in-enterprise）、
// 图片文件名（gpt-6-astra-texture.webp），以及**用法示例代码里的占位模型名**——
// `codex --model gpt-5.6` / `model = "gpt-5.6"` 这类示例写的是家族名，不是可调用的
// 模型 ID，之前"引号包裹即算 ID"的宽松规则会把裸 gpt-5.6 学进注册表。
// 页面改版导致一个都解析不到时，同步会报错并保留本地列表，不会静默学到噪声。
func isOfficialCodexModelIDContext(lowered string, start, end int) bool {
	before := lowered[:start]
	after := lowered[end:]
	if after == "" || isModelIDByte(after[0]) {
		return false
	}
	for _, prefix := range officialCodexModelCatalogContexts {
		if strings.HasSuffix(before, prefix) {
			return true
		}
	}
	return false
}

func isModelIDByte(b byte) bool {
	return b == '-' || b == '.' || b == '_' || b == '/' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func modelSortRank(id string) int {
	for index, info := range builtinModelInfos {
		if info.ID == id {
			return index
		}
	}
	return len(builtinModelInfos) + 1000
}

// isAllowedUpstreamCodexModel 判断上游发现的模型是否允许进入本地注册表
// （官方文档同步 + manifest 学习共用）。策略：
//   - gpt-5.4 及更高版本：允许
//   - gpt-5.3：只允许 spark 变体（gpt-5.3-codex-spark），其余 5.3 下线
//   - gpt-5.2 及更低、image、非 gpt- 前缀：拒绝
//   - gpt- 后不是数字版本号的代号族（gpt-daybreak-blue-latest 这类稳定别名，
//     issue #624）：允许——版本退役规则对它们无从判断，而清单里出现即代表
//     账号真实权益，拒掉只会让探测看得见、调用却 404 的模型永远进不了注册表
func isAllowedUpstreamCodexModel(id string) bool {
	id = strings.TrimSpace(strings.ToLower(id))
	if id == "" || strings.Contains(id, "image") {
		return false
	}
	if isInternalCodexModelVariant(id) {
		return false
	}
	if !strings.HasPrefix(id, "gpt-") {
		return false
	}
	version := strings.TrimPrefix(id, "gpt-")
	if dash := strings.IndexByte(version, '-'); dash >= 0 {
		version = version[:dash]
	}
	parts := strings.Split(version, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		// 以字母开头的首段是代号族别名（daybreak 等），无版本可比，按上游清单为准放行；
		// 与 isRetiredCodexModel 对非数字 ID 恒返回 false（保留）保持一致。
		// 以数字开头却解析不出的（gpt-4o 这类旧世代写法）仍按退役拒绝；
		// 标点开头（gpt-.foo / gpt-_foo）不是任何已知命名，同样拒绝。
		return version != "" && version[0] >= 'a' && version[0] <= 'z'
	}
	if len(parts) < 2 {
		return id == "gpt-6-astra"
	}
	minor := 0
	if len(parts) >= 2 {
		minor, err = strconv.Atoi(parts[1])
		if err != nil {
			return false
		}
	}
	if major > 5 {
		return true
	}
	if major < 5 {
		return false
	}
	// major == 5
	if minor >= 4 {
		return true
	}
	if minor == 3 {
		// 5.3 只保留 spark
		return strings.Contains(id, "spark")
	}
	return false

}

// SyncOfficialCodexModels fetches the fixed official docs page and merges discovered models.
// proxyURL 为全局出站代理（可为空串直连）：官方模型页与其他上游链路一样可能
// 被网络环境阻断，同步请求必须遵循全局 ProxyURL（issue #371）。
func SyncOfficialCodexModels(ctx context.Context, db *database.DB, proxyURL string) (*ModelSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库不可用，无法同步模型注册表")
	}
	client := &http.Client{Transport: newCodexStandardTransport(proxyURL), Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OfficialCodexModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("官方模型页面暂时不可访问，已保留本地模型列表: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("官方模型页面返回 %d，已保留本地模型列表", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	return ApplyOfficialCodexModelSync(ctx, db, string(body), time.Now().UTC())
}

// ApplyOfficialCodexModelSync merges a fetched official docs page into the registry.
func ApplyOfficialCodexModelSync(ctx context.Context, db *database.DB, html string, syncedAt time.Time) (*ModelSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库不可用，无法同步模型注册表")
	}
	ids, skipped := ParseOfficialCodexModelIDs(html)
	if len(ids) == 0 {
		return nil, fmt.Errorf("未从官方模型页面解析到可用模型，已保留本地模型列表")
	}

	existingRows, err := db.ListModelRegistry(ctx)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]database.ModelRegistryRow, len(existingRows))
	for _, row := range existingRows {
		existing[row.ID] = row
	}

	rows := make([]database.ModelRegistryRow, 0, len(ids))
	result := &ModelSyncResult{
		Skipped:   skipped,
		SourceURL: OfficialCodexModelsURL,
	}
	parsed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		parsed[id] = struct{}{}
		info := modelInfoForID(id, ModelSourceOfficialCodexDocs)
		row := modelInfoToRow(info, syncedAt)
		if previous, ok := existing[id]; ok {
			row.Enabled = previous.Enabled
			if modelRegistryMetadataEqual(previous, row) {
				result.Unchanged++
			} else {
				result.Updated++
			}
		} else {
			result.Added++
		}
		rows = append(rows, row)
	}

	// 官方页是 official_codex_docs 来源行的唯一真值：页面上已经不存在的行
	// （早期解析噪声如裸 gpt-5.6，或官方下线的型号）随本次同步一并删除，
	// 否则注册表没有删除入口，噪声会永久留在 /v1/models 里。只动本来源的
	// 非内置行；manifest 学习、手工来源与内置模型不受影响。
	builtin := make(map[string]struct{}, len(builtinModelInfos))
	for _, info := range builtinModelInfos {
		builtin[strings.ToLower(info.ID)] = struct{}{}
	}
	stale := make([]string, 0)
	for _, row := range existingRows {
		if row.Source != ModelSourceOfficialCodexDocs {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(row.ID))
		if _, ok := parsed[key]; ok {
			continue
		}
		if _, ok := builtin[key]; ok {
			continue
		}
		stale = append(stale, row.ID)
	}
	sort.Strings(stale)

	if err := db.UpsertModelRegistryRows(ctx, rows); err != nil {
		return nil, err
	}
	if len(stale) > 0 {
		if err := db.DeleteModelRegistryRows(ctx, stale); err != nil {
			return nil, err
		}
		result.Removed = stale
	}
	if err := db.UpdateModelRegistrySyncState(ctx, OfficialCodexModelsURL, syncedAt); err != nil {
		return nil, err
	}

	catalog, err := ListModelCatalog(ctx, db)
	if err != nil {
		return nil, err
	}
	result.Models = catalog.Models
	result.Items = catalog.Items
	result.LastSyncedAt = syncedAt.UTC()
	return result, nil
}

func modelRegistryMetadataEqual(a database.ModelRegistryRow, b database.ModelRegistryRow) bool {
	return a.Enabled == b.Enabled &&
		valueOrDefault(a.Category, ModelCategoryCodex) == valueOrDefault(b.Category, ModelCategoryCodex) &&
		valueOrDefault(a.Source, "manual") == valueOrDefault(b.Source, "manual") &&
		a.ProOnly == b.ProOnly &&
		a.APIKeyAuthAvailable == b.APIKeyAuthAvailable
}

// ExtractManifestModelSlugs 从上游模型清单里提取 models[].slug。
// 只依赖 slug 这一个身份字段，清单 schema 的其余演进不影响提取；
// 非法/超长的名字直接丢弃。解析不出任何 slug 时返回空切片（调用方按 no-op 处理）。
func ExtractManifestModelSlugs(manifest []byte) []string {
	if len(manifest) == 0 {
		return nil
	}
	items := gjson.GetBytes(manifest, "models")
	if !items.IsArray() {
		return nil
	}
	seen := make(map[string]struct{})
	slugs := make([]string, 0, 8)
	items.ForEach(func(_, item gjson.Result) bool {
		slug := strings.TrimSpace(item.Get("slug").String())
		if slug == "" {
			return true
		}
		if err := security.ValidateModelName(slug); err != nil {
			return true
		}
		key := strings.ToLower(slug)
		if _, dup := seen[key]; dup {
			return true
		}
		seen[key] = struct{}{}
		slugs = append(slugs, slug)
		return true
	})
	return slugs
}

// LearnModelsFromManifest 把上游清单里注册表尚不认识的模型学习进注册表。
//
// 严格"只增不改不删"：
//   - 只插入内置列表和注册表全部行（含已禁用）都没有的新 slug——已存在的行
//     一个字段不碰，管理员禁用过的模型不会被翻案；
//   - 清单里缺席的模型不删除：清单反映的是本次所用账号的真实权限，不同套餐
//     账号看到的清单不同，缺席不代表全局下线，注册表收敛为账号池权限的并集。
//
// 返回本次新插入的模型 ID（无新增时为空）。
func LearnModelsFromManifest(ctx context.Context, db *database.DB, manifest []byte, seenAt time.Time) ([]string, error) {
	if db == nil {
		return nil, nil
	}
	slugs := ExtractManifestModelSlugs(manifest)
	if len(slugs) == 0 {
		return nil, nil
	}

	known := make(map[string]struct{}, len(builtinModelInfos))
	for _, info := range builtinModelInfos {
		known[strings.ToLower(info.ID)] = struct{}{}
	}
	rows, err := db.ListModelRegistry(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		known[strings.ToLower(strings.TrimSpace(row.ID))] = struct{}{}
	}

	newRows := make([]database.ModelRegistryRow, 0, len(slugs))
	added := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		if _, exists := known[strings.ToLower(slug)]; exists {
			continue
		}
		// 上游同步不引入 5.3 以下模型（5.3 仅 spark）；与官方文档同步同一策略。
		if !isAllowedUpstreamCodexModel(slug) {
			continue
		}
		info := modelInfoForID(slug, ModelSourceUpstreamManifest)
		newRows = append(newRows, modelInfoToRow(info, seenAt))
		added = append(added, slug)
	}
	if len(newRows) == 0 {
		return nil, nil
	}
	if err := db.UpsertModelRegistryRows(ctx, newRows); err != nil {
		return nil, err
	}
	return added, nil
}
