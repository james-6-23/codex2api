package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// modelCompatibilityCreatedUnix is used when an upstream has no persistent
// first-seen timestamp. It is deliberately constant: /v1/models must not
// change on every request just because a legacy source lacks that metadata.
const modelCompatibilityCreatedUnix int64 = 1700000000

type modelBacking uint8

const (
	modelBackingCodex modelBacking = 1 << iota
	modelBackingGrok
	modelBackingRelay
	modelBackingAntigravity
	modelBackingClaude
)

type scopedModelRecord struct {
	id      string
	created int64
	backing modelBacking
	alias   bool
}

func stableModelCreated(t time.Time) int64 {
	if t.IsZero() {
		return modelCompatibilityCreatedUnix
	}
	return t.UTC().Unix()
}

func addScopedModel(records map[string]*scopedModelRecord, id string, backing modelBacking, firstSeen time.Time, alias bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	key := strings.ToLower(id)
	created := stableModelCreated(firstSeen)
	if current := records[key]; current != nil {
		current.backing |= backing
		current.alias = current.alias || alias
		if created < current.created {
			current.created = created
		}
		// Make casing deterministic even if account insertion order changes.
		if id < current.id {
			current.id = id
		}
		return
	}
	records[key] = &scopedModelRecord{id: id, created: created, backing: backing, alias: alias}
}

func scopedModelOwner(record *scopedModelRecord) string {
	if record == nil || record.alias {
		return "codex2api"
	}
	switch record.backing {
	case modelBackingGrok:
		return "xai"
	case modelBackingCodex:
		return "openai"
	case modelBackingAntigravity:
		return "google"
	case modelBackingClaude:
		return "anthropic"
	default:
		return "codex2api"
	}
}

func (h *Handler) accountVisibleToAPIKey(account *auth.Account, apiKeyID int64, now time.Time) bool {
	if h == nil || h.store == nil || account == nil || (!account.ModelCatalogEligible() && !account.DaybreakCatalogEligible()) {
		return false
	}
	if !account.AllowsAPIKey(apiKeyID) || !h.store.APIKeyAllowsAccount(apiKeyID, account) {
		return false
	}
	if account.IsGrokAPI() && !account.GrokModelCatalogHardAllowed(now) {
		return false
	}
	return true
}

func intersectModelIDs(candidates, allow []string) []string {
	if len(allow) == 0 {
		return candidates
	}
	allowed := make(map[string]struct{}, len(allow))
	for _, value := range allow {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			allowed[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(candidates))
	for _, value := range candidates {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(value))]; ok {
			result = append(result, value)
		}
	}
	return result
}

func exactModelAliases(mappingJSON string, targetExists func(string) bool) []string {
	var aliases []string
	for _, rule := range parseModelMappingRules(mappingJSON) {
		if rule.Wildcard || !targetExists(rule.To) {
			continue
		}
		aliases = append(aliases, rule.From)
	}
	return aliases
}

func (h *Handler) scopedModelRecords(ctx context.Context, row *database.APIKeyRow) map[string]*scopedModelRecord {
	records := make(map[string]*scopedModelRecord)
	if h == nil || h.store == nil || row == nil {
		return records
	}
	h.syncAPIKeyAllowedGroups(row)
	now := time.Now()
	accounts := h.store.Accounts()
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i] == nil {
			return false
		}
		if accounts[j] == nil {
			return true
		}
		return accounts[i].DBID < accounts[j].DBID
	})

	catalog, _ := ListModelCatalog(ctx, h.db)
	routeableTargets := make(map[string]struct{})
	addTarget := func(id string) {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			routeableTargets[id] = struct{}{}
		}
	}
	targetExists := func(id string) bool {
		_, ok := routeableTargets[strings.ToLower(strings.TrimSpace(id))]
		return ok
	}

	filteredCatalog := 0
	filteredCatalogSample := make([]string, 0, 8)
	for _, account := range accounts {
		if !h.accountVisibleToAPIKey(account, row.ID, now) {
			filteredCatalog++
			if len(filteredCatalogSample) < 8 {
				filteredCatalogSample = append(filteredCatalogSample, catalogFilterSample(account))
			}
			continue
		}
		switch {
		case account.IsGrokAPI():
			visible := GrokVisibleModelIDsForAccount(account)
			if declared := account.GrokModels(); len(declared) > 0 {
				visible = intersectModelIDs(visible, declared)
			}
			firstSeen := make(map[string]time.Time)
			for _, model := range account.GrokCatalogModels() {
				firstSeen[strings.ToLower(strings.TrimSpace(model.ModelID))] = model.FirstSeenAt
			}
			accountTargets := make(map[string]struct{}, len(visible))
			for _, id := range visible {
				if !GrokModelRoutable(account, id, GrokProtocolResponses, now) {
					continue
				}
				addScopedModel(records, id, modelBackingGrok, firstSeen[strings.ToLower(strings.TrimSpace(id))], false)
				key := strings.ToLower(strings.TrimSpace(id))
				accountTargets[key] = struct{}{}
				addTarget(id)
			}
			for _, alias := range exactModelAliases(account.OpenAIResponsesModelMapping(), func(target string) bool {
				_, ok := accountTargets[strings.ToLower(strings.TrimSpace(target))]
				return ok
			}) {
				addScopedModel(records, alias, modelBackingGrok, time.Time{}, true)
				addTarget(alias)
			}
			// 媒体(生图/生视频)模型不在账号文本目录里,按独立的媒体模型集补充展示,
			// 与媒体端点的调度准入(grokMediaAccountSupportsModel)同源。
			for _, id := range grokMediaModelsForAccount(account) {
				addScopedModel(records, id, modelBackingGrok, time.Time{}, false)
				addTarget(id)
			}

		case account.IsOpenAIResponsesAPI():
			models := account.OpenAIResponsesModels()
			accountTargets := make(map[string]struct{}, len(models))
			for _, id := range models {
				addScopedModel(records, id, modelBackingRelay, time.Time{}, false)
				key := strings.ToLower(strings.TrimSpace(id))
				accountTargets[key] = struct{}{}
				addTarget(id)
			}
			for _, alias := range exactModelAliases(account.OpenAIResponsesModelMapping(), func(target string) bool {
				_, ok := accountTargets[strings.ToLower(strings.TrimSpace(target))]
				return ok
			}) {
				addScopedModel(records, alias, modelBackingRelay, time.Time{}, true)
				addTarget(alias)
			}

		case account.IsAntigravityAPI():
			if !account.AntigravityDispatchEnabled() {
				continue
			}
			// The synchronized account catalog contains Google wire IDs. Publish
			// only the stable native surface understood by the Responses adapter;
			// raw backing names and per-account aliases must not leak into Codex.
			models := antigravityPublicModelsForAccount(account)
			for _, id := range models {
				addScopedModel(records, id, modelBackingAntigravity, time.Time{}, false)
				addTarget(id)
			}

		case account.IsClaudeOAuth():
			// Claude Code OAuth 账号:账号维度暴露 claude 模型(owner=anthropic),
			// 供下游客户端发现;调度/透传由 claude 原生路径处理。
			for _, id := range DefaultClaudeModelIDsForAccount(account) {
				addScopedModel(records, id, modelBackingClaude, time.Time{}, false)
				addTarget(id)
			}

		default:
			for _, item := range catalog.Items {
				if !account.ModelCatalogEligible() || !item.Enabled {
					continue
				}
				// 图片模型是独立能力轴:Images 路径按 imageCapableAccountFilter 调度,
				// 不看账号文本白名单(issue #826)。发现端点与之同源,否则纯文本白名单
				// 会隐藏实际可生图的型号,下游插件无法自动发现。
				if !isImageModelInfo(item) && !account.SupportsCodexModel(item.ID) {
					continue
				}
				if item.ProOnly && !isSparkPlanCandidate(account.GetPlanType()) {
					continue
				}
				addScopedModel(records, item.ID, modelBackingCodex, time.Time{}, false)
				addTarget(item.ID)
			}
			addDaybreakScopedModels(records, account, catalog.Items)
			for _, alias := range account.DaybreakAliases() {
				if records[alias] != nil {
					addTarget(alias)
				}
			}
		}
	}

	// Antigravity-only keys intentionally expose exactly the native logical
	// surface. Global/OpenAI aliases and synthesized effort aliases belong to
	// other providers and would make Cockpit's catalog diverge again.
	channel := row.Limits.ResolveUpstreamChannel()
	if channel != database.UpstreamChannelAntigravity && channel != database.UpstreamChannelClaude {
		// Global exact aliases are visible only when their concrete target is
		// routeable in this key's account snapshot. Wildcards are patterns, not
		// model IDs, and therefore never appear in /v1/models.
		for _, mapping := range []string{h.store.GetCodexModelMapping(), h.store.GetModelMapping()} {
			for _, alias := range exactModelAliases(mapping, targetExists) {
				addScopedModel(records, alias, 0, time.Time{}, true)
			}
		}
		if entries, _ := parseReasoningEffortModelEntries(h.store.GetReasoningEffortModels(), nil, false); len(entries) > 0 {
			for _, entry := range entries {
				if !targetExists(entry.Model) {
					continue
				}
				if alias := ReasoningEffortModelAlias(entry.Model, entry.Effort); alias != "" {
					addScopedModel(records, alias, 0, time.Time{}, true)
				}
			}
		}
	}

	// Model allow/deny applies to the name the downstream client requests. An
	// alias may therefore remain visible while its hidden target is denied; the
	// request path applies the same source-name policy before mapping.
	// Daybreak 另外校验目标程序模型，避免映射绕过该 Key 的权限名单。
	for key, record := range records {
		if checkAPIKeyModel(record.id, row.Limits) != "" || h.daybreakMappedModelDenied(record.id, row.Limits) {
			delete(records, key)
		}
	}
	if row.Limits.AllowLive {
		for _, id := range liveModelAliases {
			if liveModelExplicitlyDenied(id, row.Limits.ModelDeny) {
				continue
			}
			addScopedModel(records, id, modelBackingCodex, time.Time{}, true)
		}
	}
	if len(records) == 0 && filteredCatalog > 0 {
		logEmptyModelCatalog(row.ID, row.Limits.ResolveUpstreamChannel(), filteredCatalog, strings.Join(filteredCatalogSample, "; "))
	}
	return records
}

// catalogEmptyLogAt throttles the empty-catalog diagnostic. Clients poll
// /v1/models, and one redacted line per key per minute is enough to explain
// a stuck empty list.
var (
	catalogEmptyLogMu sync.Mutex
	catalogEmptyLogAt = map[int64]time.Time{}
)

func logEmptyModelCatalog(keyID int64, channel string, filtered int, sample string) {
	now := time.Now()
	catalogEmptyLogMu.Lock()
	if last, ok := catalogEmptyLogAt[keyID]; ok && now.Sub(last) < time.Minute {
		catalogEmptyLogMu.Unlock()
		return
	}
	catalogEmptyLogAt[keyID] = now
	if len(catalogEmptyLogAt) > 1024 {
		for id, at := range catalogEmptyLogAt {
			if now.Sub(at) >= time.Minute {
				delete(catalogEmptyLogAt, id)
			}
		}
	}
	catalogEmptyLogMu.Unlock()
	log.Printf("model catalog empty for api_key_id=%d channel=%s filtered=%d sample=%s", keyID, channel, filtered, sample)
}

// catalogFilterSample is a redacted reason an account contributed no models.
// It never includes tokens, email, or proxy URLs.
func catalogFilterSample(account *auth.Account) string {
	if account == nil {
		return "id=0 reason=nil"
	}
	upstream := strings.TrimSpace(account.UpstreamType)
	if len(upstream) > 32 {
		upstream = upstream[:32]
	}
	return fmt.Sprintf("id=%d upstream=%s reason=%s", account.ID(), upstream, catalogFilterReason(account))
}

func catalogFilterReason(account *auth.Account) string {
	if account == nil {
		return "nil"
	}
	disabled := atomic.LoadInt32(&account.Disabled) != 0
	if disabled && account.IsBanned() {
		return "disabled_banned"
	}
	if disabled {
		return "disabled"
	}
	if atomic.LoadInt32(&account.DispatchPaused) != 0 {
		return "dispatch_paused"
	}
	if atomic.LoadInt32(&account.IntelligencePaused) != 0 {
		return "intelligence_paused"
	}
	if !account.ModelCatalogEligible() && !account.DaybreakCatalogEligible() {
		if account.IsBanned() {
			return "banned"
		}
		if status := account.RuntimeStatus(); status != "" {
			return "catalog_" + status
		}
		return "catalog_ineligible"
	}
	return "scope"
}

func (h *Handler) scopedModels(ctx context.Context, row *database.APIKeyRow) []api.Model {
	records := h.scopedModelRecords(ctx, row)
	models := make([]api.Model, 0, len(records))
	for _, record := range records {
		models = append(models, api.Model{
			ID: record.id, Object: "model", Created: record.created, OwnedBy: scopedModelOwner(record),
		})
	}
	sort.Slice(models, func(i, j int) bool {
		left, right := strings.ToLower(models[i].ID), strings.ToLower(models[j].ID)
		if left == right {
			return models[i].ID < models[j].ID
		}
		return left < right
	})
	return models
}

func (h *Handler) scopedCodexManifestAccount(row *database.APIKeyRow) *auth.Account {
	if h == nil || h.store == nil {
		return nil
	}
	if row != nil {
		h.syncAPIKeyAllowedGroups(row)
	}
	apiKeyID := int64(0)
	if row != nil {
		apiKeyID = row.ID
	}
	accounts := h.store.Accounts()
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i] == nil {
			return false
		}
		if accounts[j] == nil {
			return true
		}
		return accounts[i].DBID < accounts[j].DBID
	})
	for _, account := range accounts {
		if account == nil || account.IsRelayStyle() || !h.accountVisibleToAPIKey(account, apiKeyID, time.Now()) {
			continue
		}
		return account
	}
	return nil
}

func codexManifestNeedsFiltering(row *database.APIKeyRow, account *auth.Account) bool {
	if row != nil && (len(row.Limits.ModelAllow) > 0 || len(row.Limits.ModelDeny) > 0) {
		return true
	}
	return account != nil && len(account.CodexModels()) > 0
}

func codexManifestModelAllowed(row *database.APIKeyRow, account *auth.Account, slug string) bool {
	if strings.TrimSpace(slug) == "" || account == nil || !account.SupportsCodexModel(slug) {
		return false
	}
	if isProOnlyModel(slug) && !isSparkPlanCandidate(account.GetPlanType()) {
		return false
	}
	return row == nil || checkAPIKeyModel(slug, row.Limits) == ""
}

// filterCodexManifest accepts only the known {"models":[{"slug":...}]} shape.
// Any ambiguous item fails closed rather than accidentally broadening a
// restricted key. Unknown fields are retained as raw JSON.
func filterCodexManifest(body []byte, upstreamETag string, allowed func(string) bool) ([]byte, string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, "", fmt.Errorf("invalid codex manifest JSON: %w", err)
	}
	rawModels, exists := root["models"]
	if !exists {
		return nil, "", fmt.Errorf("unsupported codex manifest schema: models is missing")
	}
	var models []json.RawMessage
	if err := json.Unmarshal(rawModels, &models); err != nil {
		return nil, "", fmt.Errorf("unsupported codex manifest schema: models is not an array")
	}
	filtered := make([]json.RawMessage, 0, len(models))
	for _, raw := range models {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil || item == nil {
			return nil, "", fmt.Errorf("unsupported codex manifest schema: invalid model item")
		}
		var slug string
		if value, ok := item["slug"]; !ok || json.Unmarshal(value, &slug) != nil || strings.TrimSpace(slug) == "" {
			return nil, "", fmt.Errorf("unsupported codex manifest schema: model slug is missing")
		}
		if allowed(slug) {
			filtered = append(filtered, raw)
		}
	}
	encodedModels, err := json.Marshal(filtered)
	if err != nil {
		return nil, "", err
	}
	root["models"] = encodedModels
	filteredBody, err := json.Marshal(root)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(append(append([]byte("codex2api-manifest-v1\x00"+upstreamETag+"\x00"), filteredBody...), '\n'))
	return filteredBody, `"codex2api-` + hex.EncodeToString(sum[:]) + `"`, nil
}

func etagHeaderMatches(header, current string) bool {
	current = strings.TrimSpace(current)
	if current == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == current || strings.TrimPrefix(candidate, "W/") == strings.TrimPrefix(current, "W/") {
			return true
		}
	}
	return false
}
