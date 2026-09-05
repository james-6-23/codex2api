package admin

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// 模型目录页「刷新账号模型」的统一入口：一次刷新所有渠道，并把各渠道刷出来的
// 模型写回各自的真源（Codex → model_registry，Claude/Grok/Antigravity → 账号凭据），
// 使目录页、/v1/models 与调度准入看到同一份清单。
//
// 探测按「套餐分组抽样」进行：同一渠道内先按订阅套餐（plan_type）把账号分组，
// 每个有账号的分组随机抽一个账号去上游拉清单——同套餐账号看到的清单相同，
// 一万个 Pro 号逐个拉只会放大上游请求量。抽样结果写回同组全部账号
// （Claude），或写入共享注册表（Codex）。

const (
	modelRefreshAllTimeout       = 120 * time.Second
	modelRefreshChannelWorkers   = 2
	modelRefreshUnknownPlanLabel = "unknown"
)

// modelRefreshChannelOrder 固定渠道展示顺序，与定价页分组顺序一致。
var modelRefreshChannelOrder = []string{
	database.UpstreamChannelCodex,
	database.UpstreamChannelClaude,
	database.UpstreamChannelGrok,
	database.UpstreamChannelAntigravity,
}

// channelModelRefreshResult 是单个渠道的刷新结果。
type channelModelRefreshResult struct {
	Channel   string   `json:"channel"`
	Groups    int      `json:"groups"`          // 参与探测的套餐分组数（每组抽一个账号）
	Refreshed int      `json:"refreshed"`       // 成功刷新（写回）的探测数；Codex 含官方页同步计 1
	Failed    int      `json:"failed"`          // 拉取或写回失败的探测数
	Added     []string `json:"added"`           // 本次新出现在目录里的模型
	Error     string   `json:"error,omitempty"` // 渠道级失败原因（探测级失败只计数）
}

type refreshAllModelsResponse struct {
	Type       string                      `json:"type"` // complete —— 与 SSE 事件共用一个结构
	Message    string                      `json:"message"`
	Channels   []channelModelRefreshResult `json:"channels"`
	Added      []string                    `json:"added"`
	ModelCount int                         `json:"model_count"`
	DurationMs int64                       `json:"duration_ms"`
}

// modelRefreshEvent 是 SSE 进度事件（type=start|progress）。
type modelRefreshEvent struct {
	Type         string   `json:"type"`
	Channel      string   `json:"channel"`
	Groups       int      `json:"groups,omitempty"`  // start：该渠道待探测分组数
	Current      int      `json:"current,omitempty"` // progress：该渠道已完成探测数（含本条）
	Total        int      `json:"total,omitempty"`   // progress：该渠道探测总数
	Plan         string   `json:"plan,omitempty"`
	Members      int      `json:"members,omitempty"` // 该套餐分组内账号数
	AccountID    int64    `json:"account_id,omitempty"`
	AccountEmail string   `json:"account_email,omitempty"`
	Status       string   `json:"status,omitempty"` // ok | failed
	Message      string   `json:"message,omitempty"`
	Error        string   `json:"error,omitempty"`
	ModelCount   int      `json:"model_count,omitempty"`
	Added        []string `json:"added,omitempty"`
}

// modelRefreshEmitter 接收进度事件；JSON 模式下为 no-op。
type modelRefreshEmitter func(event modelRefreshEvent)

// channelModelRefreshFunc 是单渠道刷新实现；测试可通过 Handler.modelRefreshFuncs 注入。
type channelModelRefreshFunc func(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult

// RefreshAllModels 并行刷新所有渠道的可用模型（POST /api/admin/models/refresh-all）。
// 带 ?stream=1 时以 SSE 推送逐探测进度，最后一条为 type=complete 的汇总；
// 否则直接返回汇总 JSON。任一渠道失败不影响其他渠道写入；
// 单渠道失败在对应 channel.error 中报告，整体仍 200。
func (h *Handler) RefreshAllModels(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), modelRefreshAllTimeout)
	defer cancel()
	if c.Query("stream") != "1" {
		c.JSON(http.StatusOK, h.runRefreshAllModels(ctx, nil))
		return
	}
	setupSSE(c)
	var writeMu sync.Mutex
	emit := func(event modelRefreshEvent) {
		writeMu.Lock()
		defer writeMu.Unlock()
		sendSSEJSON(c, event)
	}
	summary := h.runRefreshAllModels(ctx, emit)
	writeMu.Lock()
	sendSSEJSON(c, summary)
	writeMu.Unlock()
}

func (h *Handler) runRefreshAllModels(ctx context.Context, emit modelRefreshEmitter) refreshAllModelsResponse {
	started := time.Now()
	if emit == nil {
		emit = func(modelRefreshEvent) {}
	}
	funcs := h.modelRefreshFuncs
	if funcs == nil {
		funcs = h.defaultModelRefreshFuncs()
	}

	results := make(map[string]channelModelRefreshResult, len(funcs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for channel, fn := range funcs {
		wg.Add(1)
		go func(channel string, fn channelModelRefreshFunc) {
			defer wg.Done()
			result := runChannelModelRefresh(ctx, channel, fn, emit)
			mu.Lock()
			results[channel] = result
			mu.Unlock()
		}(channel, fn)
	}
	wg.Wait()

	resp := refreshAllModelsResponse{
		Type:     "complete",
		Message:  "已刷新各渠道可用模型",
		Channels: make([]channelModelRefreshResult, 0, len(results)),
		Added:    make([]string, 0),
	}
	for _, channel := range orderedModelRefreshChannels(results) {
		result := results[channel]
		if result.Added == nil {
			result.Added = []string{}
		}
		resp.Channels = append(resp.Channels, result)
		resp.Added = append(resp.Added, result.Added...)
	}
	sort.Strings(resp.Added)
	resp.ModelCount = len(h.modelPricingCatalogKeys(ctx))
	resp.DurationMs = time.Since(started).Milliseconds()
	return resp
}

// runChannelModelRefresh 保证单渠道 panic 或超时不拖垮整体响应。
func runChannelModelRefresh(ctx context.Context, channel string, fn channelModelRefreshFunc, emit modelRefreshEmitter) (result channelModelRefreshResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = channelModelRefreshResult{Channel: channel, Error: fmt.Sprintf("panic: %v", recovered)}
		}
		result.Channel = channel
		if result.Error == "" && ctx.Err() != nil && result.Refreshed == 0 {
			result.Error = ctx.Err().Error()
		}
	}()
	return fn(ctx, emit)
}

func orderedModelRefreshChannels(results map[string]channelModelRefreshResult) []string {
	ordered := make([]string, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, channel := range modelRefreshChannelOrder {
		if _, ok := results[channel]; ok {
			ordered = append(ordered, channel)
			seen[channel] = struct{}{}
		}
	}
	extras := make([]string, 0)
	for channel := range results {
		if _, ok := seen[channel]; !ok {
			extras = append(extras, channel)
		}
	}
	sort.Strings(extras)
	return append(ordered, extras...)
}

func (h *Handler) defaultModelRefreshFuncs() map[string]channelModelRefreshFunc {
	return map[string]channelModelRefreshFunc{
		database.UpstreamChannelCodex:       h.refreshCodexChannelModels,
		database.UpstreamChannelClaude:      h.refreshClaudeChannelModels,
		database.UpstreamChannelGrok:        h.refreshGrokChannelModels,
		database.UpstreamChannelAntigravity: h.refreshAntigravityChannelModels,
	}
}

// modelPricingCatalogKeys 返回定价页/模型目录当前展示的全部规范模型键（各渠道拼接），
// 与 ListModelPricing 的口径一致。
func (h *Handler) modelPricingCatalogKeys(ctx context.Context) []string {
	keys := modelPricingManagementKeys(proxy.SupportedModelIDs(ctx, h.db))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		seen[key] = struct{}{}
	}
	appendUnique := func(ids []string) {
		for _, key := range modelPricingManagementKeys(ids) {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	appendUnique(append(h.grokBillingModelIDs(), grokDefaultDisplayModelIDs()...))
	appendUnique(h.antigravityChannelModels())
	appendUnique(h.claudeChannelModels())
	return keys
}

// newlyAddedModels 返回 after 中有而 before 中没有的模型（忽略大小写），已排序。
func newlyAddedModels(before, after []string) []string {
	known := make(map[string]struct{}, len(before))
	for _, id := range before {
		known[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}
	added := make([]string, 0)
	seen := make(map[string]struct{})
	for _, id := range after {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" {
			continue
		}
		if _, ok := known[key]; ok {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		added = append(added, strings.TrimSpace(id))
	}
	sort.Strings(added)
	return added
}

// ==================== 套餐分组抽样 ====================

// modelRefreshPlanGroup 是一个套餐分组：Sample 是被抽中去探测的账号，Members 是同组全部账号。
type modelRefreshPlanGroup struct {
	Plan    string
	Sample  *auth.Account
	Members []*auth.Account
}

// modelRefreshPlanKey 归一化账号套餐名作为分组键：空套餐归入 unknown，
// 不做进一步合并（pro / pro-5x / pro-20x / api … 各成一组）。
func modelRefreshPlanKey(account *auth.Account) string {
	plan := auth.NormalizePlanType(account.GetPlanType())
	if plan == "" {
		return modelRefreshUnknownPlanLabel
	}
	return plan
}

// modelRefreshSchedulable 排除已禁用、错误态的账号，避免用坏号探测。
func modelRefreshSchedulable(account *auth.Account) bool {
	if account == nil || atomic.LoadInt32(&account.Disabled) != 0 {
		return false
	}
	account.Mu().RLock()
	status := account.Status
	account.Mu().RUnlock()
	return status != auth.StatusError
}

// groupAccountsByPlan 按套餐分组并在每组随机抽一个账号。只有存在账号的分组才会出现。
func groupAccountsByPlan(accounts []*auth.Account, include func(*auth.Account) bool, pick func(n int) int) []modelRefreshPlanGroup {
	byPlan := make(map[string][]*auth.Account)
	for _, account := range accounts {
		if !modelRefreshSchedulable(account) || (include != nil && !include(account)) {
			continue
		}
		plan := modelRefreshPlanKey(account)
		byPlan[plan] = append(byPlan[plan], account)
	}
	plans := make([]string, 0, len(byPlan))
	for plan := range byPlan {
		plans = append(plans, plan)
	}
	sort.Strings(plans)
	if pick == nil {
		pick = rand.Intn
	}
	groups := make([]modelRefreshPlanGroup, 0, len(plans))
	for _, plan := range plans {
		members := byPlan[plan]
		groups = append(groups, modelRefreshPlanGroup{Plan: plan, Sample: members[pick(len(members))], Members: members})
	}
	return groups
}

func (h *Handler) planGroupsFor(include func(*auth.Account) bool) []modelRefreshPlanGroup {
	if h == nil || h.store == nil {
		return nil
	}
	return groupAccountsByPlan(h.store.Accounts(), include, nil)
}

func accountEmailForEvent(account *auth.Account) string {
	if account == nil {
		return ""
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return strings.TrimSpace(account.Email)
}

// probePlanGroups 逐组探测：每组调用一次 probe（并发 workers），并把每组的结果作为 progress 事件推出。
func (h *Handler) probePlanGroups(ctx context.Context, channel string, groups []modelRefreshPlanGroup, result *channelModelRefreshResult, emit modelRefreshEmitter, probe func(ctx context.Context, group modelRefreshPlanGroup) (modelCount int, added []string, err error)) {
	result.Groups = len(groups)
	emit(modelRefreshEvent{Type: "start", Channel: channel, Groups: len(groups)})
	if len(groups) == 0 {
		return
	}
	jobs := make(chan modelRefreshPlanGroup)
	var mu sync.Mutex
	var wg sync.WaitGroup
	done := 0
	for i := 0; i < modelRefreshChannelWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for group := range jobs {
				var (
					modelCount int
					added      []string
					err        error
				)
				if ctx.Err() != nil {
					err = ctx.Err()
				} else {
					modelCount, added, err = probe(ctx, group)
				}
				mu.Lock()
				done++
				event := modelRefreshEvent{
					Type: "progress", Channel: channel, Current: done, Total: len(groups),
					Plan: group.Plan, Members: len(group.Members),
					AccountID: group.Sample.ID(), AccountEmail: accountEmailForEvent(group.Sample),
					ModelCount: modelCount, Added: added,
				}
				if err != nil {
					result.Failed++
					event.Status = "failed"
					event.Error = err.Error()
				} else {
					result.Refreshed++
					event.Status = "ok"
				}
				mu.Unlock()
				emit(event)
			}
		}()
	}
	for _, group := range groups {
		jobs <- group
	}
	close(jobs)
	wg.Wait()
}

// ==================== Codex ====================

// isCodexOAuthAccount 判断账号是否为 ChatGPT OAuth 的 Codex 官方账号
// （非 relay 中转、非 Grok/Claude/Antigravity）。
func isCodexOAuthAccount(account *auth.Account) bool {
	if account == nil {
		return false
	}
	return !account.IsRelayStyle() && !account.IsAntigravityAPI() && !account.IsGrokAPI() && !account.IsClaudeOAuth()
}

func (h *Handler) refreshCodexChannelModels(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelCodex, Added: []string{}}
	if h == nil || h.db == nil {
		result.Error = "数据库不可用"
		return result
	}
	before := proxy.SupportedModelIDs(ctx, h.db)

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	// 1. 官方模型页 → 注册表（与设置页「同步上游模型」同一实现）。
	if _, err := proxy.SyncOfficialCodexModels(ctx, h.db, proxyURL); err != nil {
		result.Error = fmt.Sprintf("官方模型页同步失败: %v", err)
		result.Failed++
		emit(modelRefreshEvent{Type: "progress", Channel: database.UpstreamChannelCodex, Plan: "official_docs", Status: "failed", Error: err.Error()})
	} else {
		result.Refreshed++
		emit(modelRefreshEvent{Type: "progress", Channel: database.UpstreamChannelCodex, Plan: "official_docs", Status: "ok",
			ModelCount: len(proxy.SupportedModelIDs(ctx, h.db)), Added: newlyAddedModels(before, proxy.SupportedModelIDs(ctx, h.db))})
	}

	// 2. 每种套餐抽一个账号拉上游清单 → 注册表（只增不改不删，沿用 LearnModelsFromManifest）。
	now := time.Now().UTC()
	h.probePlanGroups(ctx, database.UpstreamChannelCodex, h.planGroupsFor(isCodexOAuthAccount), &result, emit,
		func(ctx context.Context, group modelRefreshPlanGroup) (int, []string, error) {
			manifest, err := proxy.FetchCodexModelsManifest(ctx, group.Sample, h.store.ResolveProxyForAccount(group.Sample), "", "")
			if err != nil {
				return 0, nil, err
			}
			proxy.RecordResponsesLiteSupportFromManifest(manifest.Body)
			added, err := proxy.LearnModelsFromManifest(ctx, h.db, manifest.Body, now)
			if err != nil {
				return 0, nil, err
			}
			return len(proxy.ExtractManifestModelSlugs(manifest.Body)), added, nil
		})

	result.Added = newlyAddedModels(before, proxy.SupportedModelIDs(ctx, h.db))
	return result
}

// ==================== Claude ====================

func (h *Handler) refreshClaudeChannelModels(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelClaude, Added: []string{}}
	if h == nil || h.db == nil {
		result.Error = "数据库不可用"
		return result
	}
	before := h.claudeChannelModels()
	var wrote int32
	h.probePlanGroups(ctx, database.UpstreamChannelClaude, h.planGroupsFor(func(a *auth.Account) bool { return a.IsClaudeOAuth() }), &result, emit,
		func(ctx context.Context, group modelRefreshPlanGroup) (int, []string, error) {
			sample := group.Sample
			accessToken := strings.TrimSpace(sample.GetAccessToken())
			if accessToken == "" {
				return 0, nil, fmt.Errorf("账号缺少 access_token")
			}
			models, err := auth.NewClaudeAuth(h.store.ResolveProxyForAccount(sample)).FetchModels(ctx, accessToken)
			if err != nil {
				return 0, nil, err
			}
			models = auth.NormalizeAccountModels(models)
			if len(models) == 0 {
				return 0, nil, fmt.Errorf("上游未返回可用模型")
			}
			// 同套餐账号权限一致：抽样结果写回同组全部账号，目录与调度准入保持一致。
			added := newlyAddedModels(group.Sample.CodexModels(), models)
			var writeErr error
			for _, member := range group.Members {
				if err := h.db.UpdateCredentials(ctx, member.ID(), map[string]interface{}{"models": models}); err != nil {
					writeErr = err
					continue
				}
				member.Mu().Lock()
				member.Models = append([]string(nil), models...)
				member.Mu().Unlock()
				atomic.StoreInt32(&wrote, 1)
			}
			return len(models), added, writeErr
		})
	if atomic.LoadInt32(&wrote) == 1 {
		h.invalidateClaudeCatalogCaches()
	}
	result.Added = newlyAddedModels(before, h.claudeChannelModels())
	return result
}

// ==================== Grok ====================

func (h *Handler) refreshGrokChannelModels(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelGrok, Added: []string{}}
	if h == nil || h.store == nil {
		return result
	}
	grokModels := func() []string { return append(h.grokBillingModelIDs(), grokDefaultDisplayModelIDs()...) }
	before := grokModels()
	h.probePlanGroups(ctx, database.UpstreamChannelGrok, h.planGroupsFor(func(a *auth.Account) bool { return a.IsGrokAPI() }), &result, emit,
		func(ctx context.Context, group modelRefreshPlanGroup) (int, []string, error) {
			id := group.Sample.ID()
			syncResult, err := h.syncGrokAccountState(ctx, id)
			if err != nil {
				return 0, nil, err
			}
			if syncResult.capabilityGeneration > 0 {
				h.triggerGrokCapabilityProbeForGeneration(id, syncResult.capabilityGeneration)
			}
			return len(syncResult.Models), nil, nil
		})
	result.Added = newlyAddedModels(before, grokModels())
	return result
}

// ==================== Antigravity ====================

func (h *Handler) refreshAntigravityChannelModels(ctx context.Context, emit modelRefreshEmitter) channelModelRefreshResult {
	result := channelModelRefreshResult{Channel: database.UpstreamChannelAntigravity, Added: []string{}}
	if h == nil || h.store == nil {
		return result
	}
	before := h.antigravityChannelModels()
	h.probePlanGroups(ctx, database.UpstreamChannelAntigravity, h.planGroupsFor(func(a *auth.Account) bool { return a.IsAntigravityAPI() }), &result, emit,
		func(ctx context.Context, group modelRefreshPlanGroup) (int, []string, error) {
			item := h.runAntigravityRefresh(ctx, group.Sample.ID())
			if !item.OK {
				if item.Error != "" {
					return 0, nil, fmt.Errorf("%s", item.Error)
				}
				return 0, nil, fmt.Errorf("刷新失败")
			}
			return len(group.Sample.AntigravityModels()), nil, nil
		})
	result.Added = newlyAddedModels(before, h.antigravityChannelModels())
	return result
}
