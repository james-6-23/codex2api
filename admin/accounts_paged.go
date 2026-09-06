package admin

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	accountListSnapshotTTL      = 5 * time.Second
	accountListSnapshotTTLLarge = 30 * time.Second
	accountListSnapshotLargeMin = 2000
	accountListPageMax          = 500
	accountListPageDefault      = 20
	// requestCountFullPoolScanMax 是「账号数量超载」阈值:超过就不再单条
	// ANY() 扫近 7 天全表,改走分批聚合。
	requestCountFullPoolScanMax = accountListPageMax
	// requestCountBatchSize 是全池刷新每一批的 ID 数。比单页上限更保守,
	// 让规划器稳定走 (account_id, created_at),避免 500 个 ID 仍被估成顺序扫。
	requestCountBatchSize = 100
	// requestCountBatchRefreshBaseTimeout / requestCountBatchBudget 决定分批
	// 全池刷新的后台时限:基础 + 每批预算,再按 Max 封顶。固定时限在 3 万+
	// 池上会周期性超时;配合断点续跑(requestCountStaging),超时也不再整轮作废。
	requestCountBatchRefreshBaseTimeout = time.Minute
	requestCountBatchBudget             = 3 * time.Second
	requestCountBatchRefreshMaxTimeout  = 20 * time.Minute
	// requestCountStagingMaxAge 是断点续跑半成品的最大搁置时长:换天或搁太久
	// 的部分聚合结果口径已陈,直接重来。
	requestCountStagingMaxAge = 30 * time.Minute
	// requestCountCacheTTL 是请求数统计缓存的保鲜期。它只喂列表排序与统计卡,
	// 不需要 10s 级新鲜度;前端静默刷新退避后轮询间隔最长 80s,TTL 太短会让
	// 几乎每次轮询都撞上过期缓存、stats_state 长期停在 stale(表现为
	// 「统计刷新中…」常驻)。
	// 另一侧约束:刷新是对全渠道账号扫 7 天 usage_logs 的三条 GROUP BY,
	// 万级号池 + 大日志量下单轮就要秒级;TTL 过短等于让这组重查询近乎
	// 永续循环,把数据库拖垮(表现为管理页整体变慢、接口超时)。统计本身
	// 是 7 天累计值,分钟级陈旧完全可接受。
	requestCountCacheTTL = 5 * time.Minute
)

type accountListSnapshot struct {
	Channel    string
	Items      []*accountListSnapshotItem
	Summary    accountListSummary
	Facets     accountListFacets
	BuiltAt    time.Time
	ExpiresAt  time.Time
	StatsState string
}

type accountListSnapshotItem struct {
	Row                 *database.AccountRow
	ID                  int64
	Status              string
	CooldownReason      string
	Enabled             bool
	Locked              bool
	UsingCredits        bool
	PlanType            string
	GrokAuthKind        string
	GrokPlanCategory    string
	Email               string
	EmailDomain         string
	Tags                []string
	GroupIDs            []int64
	GroupSortKey        string
	UsagePercent5h      float64
	UsagePercent5hOK    bool
	UsagePercent7d      float64
	UsagePercent7dOK    bool
	UsagePercentSpark   float64
	UsagePercentSparkOK bool
	ResetSparkAt        time.Time
	RequestCount        int64
	TodayRequests       int64
	TodayTokens         int64
	TodayAccountBilled  float64
	SchedulerPriority   int64
	HealthTier          string
	DispatchScore       float64
	LatencyPenalty      float64
	LastUnauthorizedAt  time.Time
	LastRateLimitedAt   time.Time
	LastTimeoutAt       time.Time
	Reset5hAt           time.Time
	Reset7dAt           time.Time
	CooldownUntil       time.Time
	Window7dSeconds     int64
	ActiveRequests      int64
	OccupiedRequests    int64
	DynamicConcurrency  int64
	OpenAIResponses     bool
	Antigravity         bool
	Claude              bool
	ClaudeAuthKind      string
	ClaudeUsageProbeAt  string
	ClaudeUsageProbeErr string
	SearchText          string
}

type accountListSummary struct {
	Total                int `json:"total"`
	Normal               int `json:"normal"`
	Active               int `json:"active"`
	OverloadPaused       int `json:"overload_paused"`
	RateLimited          int `json:"rate_limited"`
	RateLimited5h        int `json:"rate_limited_5h"`
	RateLimited7d        int `json:"rate_limited_7d"`
	Abnormal             int `json:"abnormal"`
	Banned               int `json:"banned"`
	Error                int `json:"error"`
	Unsampled            int `json:"unsampled"`
	Disabled             int `json:"disabled"`
	Locked               int `json:"locked"`
	Healthy              int `json:"healthy"`
	Warm                 int `json:"warm"`
	Risky                int `json:"risky"`
	OAuth                int `json:"oauth"`
	APIKey               int `json:"api_key"`
	SetupToken           int `json:"setup_token"`
	SubscriptionUnlocked int `json:"subscription_unlocked"`
	Unauthorized24h      int `json:"unauthorized_24h"`
	RateLimited1h        int `json:"rate_limited_1h"`
	Timeout15m           int `json:"timeout_15m"`
	SelfServicePending   int `json:"self_service_pending"`
}

type accountListDomainFacet struct {
	Domain string `json:"domain"`
	Total  int    `json:"total"`
	Banned int    `json:"banned"`
}

type accountListFacets struct {
	Tags         []string                 `json:"tags"`
	EmailDomains []accountListDomainFacet `json:"email_domains"`
}

type accountsPageResponse struct {
	Accounts      []accountResponse  `json:"accounts"`
	Page          int                `json:"page"`
	PageSize      int                `json:"page_size"`
	Total         int                `json:"total"`
	Summary       accountListSummary `json:"summary"`
	Facets        accountListFacets  `json:"facets"`
	SnapshotAt    string             `json:"snapshot_at"`
	StatsState    string             `json:"stats_state"`
	DisabledSorts []string           `json:"disabled_sorts,omitempty"`
}

type accountPageSelection struct {
	Rows          []*database.AccountRow
	Page          int
	PageSize      int
	Total         int
	Summary       accountListSummary
	Facets        accountListFacets
	SnapshotAt    time.Time
	StatsState    string
	DisabledSorts []string
}

type accountPageQuery struct {
	Page         int
	PageSize     int
	Search       string
	Status       string
	Plan         string
	AuthKind     string
	Tag          string
	EmailDomain  string
	GroupInclude []int64
	GroupExclude []int64
	Ungrouped    bool
	HealthTier   string
	ProxyURL     string
	ProxyFilter  string
	Sort         string
	Order        string
}

type accountPageQueryError struct {
	err error
}

func (e *accountPageQueryError) Error() string {
	return e.err.Error()
}

// accountOperationSelector lets large-pool operations resolve their target set
// on the server instead of transferring tens of thousands of IDs.
type accountOperationSelector struct {
	Channel              string  `json:"channel"`
	Search               string  `json:"search,omitempty"`
	Status               string  `json:"status,omitempty"`
	Plan                 string  `json:"plan,omitempty"`
	AuthKind             string  `json:"auth_kind,omitempty"`
	Tag                  string  `json:"tag,omitempty"`
	EmailDomain          string  `json:"email_domain,omitempty"`
	GroupInclude         []int64 `json:"group_include,omitempty"`
	GroupExclude         []int64 `json:"group_exclude,omitempty"`
	Ungrouped            bool    `json:"ungrouped,omitempty"`
	RefreshableOnly      bool    `json:"refreshable_only,omitempty"`
	SubscriptionUnlocked bool    `json:"subscription_unlocked,omitempty"`
}

func (h *Handler) resolveAccountOperationSelector(ctx context.Context, selector *accountOperationSelector) ([]int64, error) {
	if selector == nil {
		return nil, fmt.Errorf("selector is required")
	}
	channel := strings.ToLower(strings.TrimSpace(selector.Channel))
	if channel != database.UpstreamChannelCodex && channel != database.UpstreamChannelGrok && channel != database.UpstreamChannelAntigravity && channel != database.UpstreamChannelClaude {
		return nil, fmt.Errorf("selector channel must be codex, grok, antigravity, or claude")
	}
	snapshot, err := h.getAccountListSnapshot(ctx, channel)
	if err != nil {
		return nil, err
	}
	query := accountPageQuery{
		Search:       strings.ToLower(strings.TrimSpace(selector.Search)),
		Status:       strings.ToLower(strings.TrimSpace(selector.Status)),
		Plan:         strings.ToLower(strings.TrimSpace(selector.Plan)),
		AuthKind:     strings.ToLower(strings.TrimSpace(selector.AuthKind)),
		Tag:          strings.TrimSpace(selector.Tag),
		EmailDomain:  strings.ToLower(strings.TrimSpace(selector.EmailDomain)),
		GroupInclude: positiveUniqueAdminIDs(selector.GroupInclude),
		GroupExclude: positiveUniqueAdminIDs(selector.GroupExclude),
		Ungrouped:    selector.Ungrouped,
	}
	if err := validateAccountPageFilters(query); err != nil {
		return nil, err
	}
	ids := make([]int64, 0)
	for _, item := range snapshot.Items {
		if !accountListItemMatches(item, query, channel) {
			continue
		}
		if selector.RefreshableOnly {
			if item.GrokAuthKind == auth.GrokAuthKindAPIKey || strings.TrimSpace(item.Row.GetCredential("refresh_token")) == "" {
				continue
			}
		}
		if selector.SubscriptionUnlocked && !accountListSubscriptionUnlocked(item, channel) {
			continue
		}
		ids = append(ids, item.ID)
	}
	return ids, nil
}

func positiveUniqueAdminIDs(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func parseAccountPageQuery(c *gin.Context) (accountPageQuery, error) {
	query := accountPageQuery{
		Page:        1,
		PageSize:    accountListPageDefault,
		Search:      strings.ToLower(strings.TrimSpace(c.Query("search"))),
		Status:      strings.ToLower(strings.TrimSpace(c.Query("status"))),
		Plan:        strings.ToLower(strings.TrimSpace(c.Query("plan"))),
		AuthKind:    strings.ToLower(strings.TrimSpace(c.Query("auth_kind"))),
		Tag:         strings.TrimSpace(c.Query("tag")),
		EmailDomain: strings.ToLower(strings.TrimSpace(c.Query("email_domain"))),
		HealthTier:  strings.ToLower(strings.TrimSpace(c.Query("health_tier"))),
		ProxyURL:    strings.TrimSpace(c.Query("proxy_url")),
		ProxyFilter: strings.ToLower(strings.TrimSpace(c.Query("proxy_filter"))),
		Sort:        strings.ToLower(strings.TrimSpace(c.Query("sort"))),
		Order:       strings.ToLower(strings.TrimSpace(c.Query("order"))),
	}
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return query, fmt.Errorf("page must be a positive integer")
		}
		query.Page = value
	}
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > accountListPageMax {
			return query, fmt.Errorf("page_size must be between 1 and %d", accountListPageMax)
		}
		query.PageSize = value
	}
	var err error
	if query.GroupInclude, err = parseAccountListIDs(c.Query("group_include")); err != nil {
		return query, fmt.Errorf("invalid group_include")
	}
	if query.GroupExclude, err = parseAccountListIDs(c.Query("group_exclude")); err != nil {
		return query, fmt.Errorf("invalid group_exclude")
	}
	if raw := strings.TrimSpace(c.Query("ungrouped")); raw != "" {
		value, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return query, fmt.Errorf("ungrouped must be true or false")
		}
		query.Ungrouped = value
	}
	if query.Order == "" {
		query.Order = "desc"
	}
	if query.Order != "asc" && query.Order != "desc" {
		return query, fmt.Errorf("order must be asc or desc")
	}
	validSorts := map[string]bool{
		"": true, "requests": true, "today": true, "usage": true, "created_at": true, "updated_at": true,
		"scheduler_priority": true, "group": true, "risk": true, "dispatch_score": true,
		"latency_penalty": true, "unauthorized": true,
	}
	if !validSorts[query.Sort] {
		return query, fmt.Errorf("unsupported sort")
	}
	if err := validateAccountPageFilters(query); err != nil {
		return query, err
	}
	return query, nil
}

func validateAccountPageFilters(query accountPageQuery) error {
	validStatuses := map[string]bool{
		"": true, "all": true, "normal": true, "active": true, "scheduling": true,
		"overload_paused": true, "rate_limited": true, "abnormal": true, "banned": true,
		"error": true, "unsampled": true, "disabled": true, "locked": true,
	}
	if !validStatuses[query.Status] {
		return fmt.Errorf("unsupported status")
	}
	validAuthKinds := map[string]bool{"": true, "all": true, auth.GrokAuthKindOAuth: true, auth.GrokAuthKindAPIKey: true, auth.ClaudeAuthKindSetupToken: true}
	if !validAuthKinds[query.AuthKind] {
		return fmt.Errorf("unsupported auth_kind")
	}
	validHealthTiers := map[string]bool{
		"": true, "all": true, "attention": true,
		"healthy": true, "warm": true, "risky": true, "banned": true,
	}
	if !validHealthTiers[query.HealthTier] {
		return fmt.Errorf("unsupported health_tier")
	}
	validProxyFilters := map[string]bool{"": true, "all": true, "unbound": true, "this": true, "other": true}
	if !validProxyFilters[query.ProxyFilter] {
		return fmt.Errorf("unsupported proxy_filter")
	}
	if query.ProxyFilter == "this" && query.ProxyURL == "" {
		return fmt.Errorf("proxy_url is required for proxy_filter=this")
	}
	return nil
}

func parseAccountListIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := make(map[int64]struct{})
	result := make([]int64, 0)
	for _, part := range strings.Split(raw, ",") {
		value, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("invalid id")
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func (h *Handler) getAccountPageSelection(ctx context.Context, c *gin.Context, channel string) (*accountPageSelection, error) {
	query, err := parseAccountPageQuery(c)
	if err != nil {
		return nil, &accountPageQueryError{err: err}
	}
	snapshot, err := h.getAccountListSnapshot(ctx, channel)
	if err != nil {
		return nil, err
	}
	filtered := make([]*accountListSnapshotItem, 0, len(snapshot.Items))
	for _, item := range snapshot.Items {
		if accountListItemMatches(item, query, channel) {
			filtered = append(filtered, item)
		}
	}
	disabledSorts := h.disabledUsageSorts(channel, len(snapshot.Items))
	sortAccountListItems(filtered, effectiveAccountListSort(query.Sort, disabledSorts), query.Order)
	total := len(filtered)
	totalPages := 1
	if total > 0 {
		totalPages = (total + query.PageSize - 1) / query.PageSize
	}
	page := query.Page
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * query.PageSize
	if start > total {
		start = total
	}
	end := start + query.PageSize
	if end > total {
		end = total
	}
	pageIDs := make([]int64, 0, end-start)
	for _, item := range filtered[start:end] {
		pageIDs = append(pageIDs, item.ID)
	}
	fullRows, err := h.db.ListActiveByIDs(ctx, pageIDs)
	if err != nil {
		return nil, err
	}
	rowsByID := make(map[int64]*database.AccountRow, len(fullRows))
	for _, row := range fullRows {
		rowsByID[row.ID] = row
	}
	rows := make([]*database.AccountRow, 0, len(pageIDs))
	for _, id := range pageIDs {
		if row := rowsByID[id]; row != nil {
			rows = append(rows, row)
		}
	}
	return &accountPageSelection{
		Rows: rows, Page: page, PageSize: query.PageSize, Total: total,
		Summary: snapshot.Summary, Facets: snapshot.Facets,
		SnapshotAt: snapshot.BuiltAt, StatsState: snapshot.StatsState,
		DisabledSorts: disabledSorts,
	}, nil
}

func usageLogSortKeys() []string {
	return []string{"requests", "today"}
}

func (h *Handler) disabledUsageSorts(channel string, poolSize int) []string {
	if poolSize <= requestCountFullPoolScanMax {
		return nil
	}
	if h.requestCountCacheComplete(channel) {
		return nil
	}
	return usageLogSortKeys()
}

// requestCountCacheComplete 报告该渠道的请求数统计是否聚合成功过。只看
// 「有没有」不看新鲜度:排序读的是列表快照里上一轮聚合的计数,条目过期后
// 旧值仍在、后台照常重聚合。若这里要求未过期,大池排序会在每个 TTL 周期
// 的重聚合窗口内被禁用一次,前端表现为排序反复变灰、用户选中的排序被打断。
func (h *Handler) requestCountCacheComplete(channel string) bool {
	if h == nil {
		return false
	}
	h.reqCountMu.RLock()
	entry := h.reqCountCache[channel]
	h.reqCountMu.RUnlock()
	return entry != nil
}

func effectiveAccountListSort(sort string, disabled []string) string {
	for _, key := range disabled {
		if sort == key {
			return ""
		}
	}
	return sort
}

func (h *Handler) getAccountListSnapshot(ctx context.Context, channel string) (*accountListSnapshot, error) {
	now := time.Now()
	h.accountListCacheMu.RLock()
	cached := h.accountListCache[channel]
	if cached != nil && now.Before(cached.ExpiresAt) {
		h.accountListCacheMu.RUnlock()
		return cached, nil
	}
	h.accountListCacheMu.RUnlock()

	if cached != nil {
		h.refreshAccountListSnapshotAsync(channel)
		return cached, nil
	}

	h.accountListBuildMu.Lock()
	defer h.accountListBuildMu.Unlock()
	h.accountListCacheMu.RLock()
	cached = h.accountListCache[channel]
	if cached != nil && time.Now().Before(cached.ExpiresAt) {
		h.accountListCacheMu.RUnlock()
		return cached, nil
	}
	h.accountListCacheMu.RUnlock()
	return h.rebuildAccountListSnapshot(ctx, channel)
}

func (h *Handler) refreshAccountListSnapshotAsync(channel string) {
	if !h.accountListBuildMu.TryLock() {
		return
	}
	go func() {
		defer h.accountListBuildMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := h.rebuildAccountListSnapshot(ctx, channel); err != nil {
			return
		}
	}()
}

// shouldInvalidateAccountSnapshotCaches 判定一次管理请求是否改动了账号数据:
// 非只读方法 + 账号/分组路由前缀 + 2xx/3xx。挂在路由组中间件上,覆盖全部
// 现有与未来的账号变更端点(含流式批量操作),避免逐 handler 手工失效。
// 单条/批量删除和按状态清理走增量剔除,不在这里整份作废,否则大批量清理会再投影全池。
func shouldInvalidateAccountSnapshotCaches(method, path string, status int) bool {
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return false
	}
	if status >= http.StatusBadRequest {
		return false
	}
	if isAccountListDeletePath(method, path) {
		return false
	}
	return strings.HasPrefix(path, "/api/admin/accounts") ||
		strings.HasPrefix(path, "/api/admin/account-groups")
}

// isAccountListDeletePath 只匹配会从活跃列表拿走账号的删除接口。
// 回收站彻底清除/清空仍走整份失效:那些账号本来就不在列表快照里。
func isAccountListDeletePath(method, path string) bool {
	switch method {
	case http.MethodDelete:
		if !strings.HasPrefix(path, "/api/admin/accounts/") {
			return false
		}
		rest := strings.TrimPrefix(path, "/api/admin/accounts/")
		if rest == "" || strings.Contains(rest, "/") {
			return false
		}
		_, err := strconv.ParseInt(rest, 10, 64)
		return err == nil
	case http.MethodPost:
		switch path {
		case "/api/admin/accounts/batch-delete",
			"/api/admin/accounts/clean-banned",
			"/api/admin/accounts/clean-rate-limited",
			"/api/admin/accounts/clean-error",
			"/api/admin/accounts/grok/clean-banned",
			"/api/admin/accounts/grok/clean-error":
			return true
		}
		return false
	default:
		return false
	}
}

// invalidateAccountSnapshotCaches 在账号发生变更(封禁/禁用/导入等)后
// 丢弃列表快照与分析缓存,让下一次读取同步重建。否则 stale-while-revalidate
// 的读路径会把变更前的统计卡/筛选计数原样返回给变更后的第一次刷新。
func (h *Handler) invalidateAccountSnapshotCaches() {
	h.accountCachesGen.Add(1)
	h.claudeAccountCachesGen.Add(1)
	h.accountListCacheMu.Lock()
	h.accountListCache = nil
	h.accountListCacheMu.Unlock()
	h.accountAnalysisCacheMu.Lock()
	h.accountAnalysisCache = nil
	h.accountAnalysisCacheMu.Unlock()
}

// pruneAccountsFromSnapshotCaches 从各渠道列表快照里拿掉已删除账号并重算
// 统计卡。比整份失效便宜:不必再 ListAccountListProjection 全池。
// 代数加一,避免在途全量重建把已删账号写回缓存。
func (h *Handler) pruneAccountsFromSnapshotCaches(ids []int64) {
	if len(ids) == 0 {
		return
	}
	drop := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		drop[id] = struct{}{}
	}
	h.accountCachesGen.Add(1)
	h.accountListCacheMu.Lock()
	for channel, cached := range h.accountListCache {
		if cached == nil {
			continue
		}
		kept := make([]*accountListSnapshotItem, 0, len(cached.Items))
		removed := 0
		for _, item := range cached.Items {
			if _, ok := drop[item.ID]; ok {
				removed++
				continue
			}
			kept = append(kept, item)
		}
		if removed == 0 {
			continue
		}
		next := *cached
		next.Items = kept
		next.BuiltAt = time.Now()
		ttl := accountListSnapshotTTL
		if len(kept) >= accountListSnapshotLargeMin {
			ttl = accountListSnapshotTTLLarge
		}
		next.ExpiresAt = next.BuiltAt.Add(ttl)
		next.Summary, next.Facets = summarizeAccountList(kept, channel)
		h.accountListCache[channel] = &next
	}
	h.accountListCacheMu.Unlock()
	h.accountAnalysisCacheMu.Lock()
	h.accountAnalysisCache = nil
	h.accountAnalysisCacheMu.Unlock()
}

func (h *Handler) rebuildAccountListSnapshot(ctx context.Context, channel string) (*accountListSnapshot, error) {
	gen := h.accountCachesGen.Load()
	claudeGen := h.claudeAccountCachesGen.Load()
	rows, err := h.db.ListAccountListProjection(ctx, channel)
	if err != nil {
		return nil, err
	}
	groups, _ := h.db.ListAccountGroups(ctx)
	groupNames := make(map[int64]string, len(groups))
	groupSort := make(map[int64]string, len(groups))
	for _, group := range groups {
		groupNames[group.ID] = group.Name
		groupSort[group.ID] = fmt.Sprintf("%020d\x00%s", group.SortOrder, strings.ToLower(group.Name))
	}
	channelIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		channelIDs = append(channelIDs, row.ID)
	}
	requestCounts, todayUsage, statsState := h.getCachedRequestCountsNonBlocking(channel, channelIDs)
	items := make([]*accountListSnapshotItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, h.buildAccountListSnapshotItem(row, requestCounts, todayUsage, groupNames, groupSort))
	}
	snapshot := &accountListSnapshot{
		Channel: channel, Items: items, BuiltAt: time.Now(), StatsState: statsState,
	}
	snapshotTTL := accountListSnapshotTTL
	if len(items) >= accountListSnapshotLargeMin {
		snapshotTTL = accountListSnapshotTTLLarge
	}
	snapshot.ExpiresAt = snapshot.BuiltAt.Add(snapshotTTL)
	snapshot.Summary, snapshot.Facets = summarizeAccountList(items, channel)
	h.installAccountListSnapshot(channel, snapshot, gen, claudeGen)
	return snapshot, nil
}

// installAccountListSnapshot 只在代数未漂移时入缓存:读库期间发生过账号
// 变更的快照可能早于变更,返回给当前调用方无妨,但不能留给后续请求。
func (h *Handler) installAccountListSnapshot(channel string, snapshot *accountListSnapshot, gen uint64, claudeGens ...uint64) {
	if h.accountCachesGen.Load() != gen {
		return
	}
	claudeGen := h.claudeAccountCachesGen.Load()
	if len(claudeGens) > 0 {
		claudeGen = claudeGens[0]
	}
	if channel == database.UpstreamChannelClaude && h.claudeAccountCachesGen.Load() != claudeGen {
		return
	}
	h.accountListCacheMu.Lock()
	if h.accountListCache == nil {
		h.accountListCache = make(map[string]*accountListSnapshot)
	}
	h.accountListCache[channel] = snapshot
	h.accountListCacheMu.Unlock()
}

func (h *Handler) buildAccountListSnapshotItem(row *database.AccountRow, requestCounts map[int64]*database.AccountRequestCount, todayUsage map[int64]*database.AccountTimeRangeUsage, groupNames, groupSort map[int64]string) *accountListSnapshotItem {
	upstreamType := strings.TrimSpace(row.GetCredential("upstream_type"))
	isGrok := strings.EqualFold(upstreamType, auth.UpstreamGrok)
	isAntigravity := strings.EqualFold(upstreamType, auth.UpstreamAntigravity)
	isOpenAIResponses := strings.EqualFold(upstreamType, auth.UpstreamOpenAIResponses)
	isClaude := strings.EqualFold(upstreamType, auth.UpstreamClaude)
	email := row.GetCredential("email")
	if isOpenAIResponses && email == "" {
		email = row.GetCredential("base_url")
	}
	planType := row.GetCredential("plan_type")
	if isOpenAIResponses && planType == "" {
		planType = "api"
	}
	grokAuthKind := ""
	if isGrok {
		if strings.TrimSpace(row.GetCredential("api_key")) != "" {
			grokAuthKind = auth.GrokAuthKindAPIKey
			planType = "api"
		} else {
			grokAuthKind = auth.GrokAuthKindOAuth
		}
	}
	status := row.Status
	if isAntigravity {
		status, _ = antigravityPersistedStatus(row)
	}
	item := &accountListSnapshotItem{
		Row: row, ID: row.ID, Status: status, CooldownReason: row.CooldownReason,
		Enabled: row.Enabled, Locked: row.Locked, PlanType: planType, GrokAuthKind: grokAuthKind,
		Email: email, EmailDomain: accountEmailDomain(email), Tags: append([]string(nil), row.Tags...),
		SchedulerPriority: valueOrZero(accountSchedulerPriority(row)), OpenAIResponses: isOpenAIResponses,
		Antigravity: isAntigravity, Claude: isClaude,
		ClaudeAuthKind:      claudeAuthKindForRow(row, isClaude),
		ClaudeUsageProbeAt:  row.GetCredential(auth.ClaudeUsageProbeAtCredentialKey),
		ClaudeUsageProbeErr: row.GetCredential(auth.ClaudeUsageProbeErrorCredentialKey),
	}
	if row.CooldownUntil.Valid {
		item.CooldownUntil = row.CooldownUntil.Time
	}
	if resolved, ok := auth.ResolveGrokPlan(planType); ok {
		item.GrokPlanCategory = resolved.Key
	} else {
		item.GrokPlanCategory = "other"
	}
	if h.store != nil {
		if runtimeAccount := h.store.FindByID(row.ID); runtimeAccount != nil {
			runtimeSnapshot := runtimeAccount.GetAccountListRuntimeSnapshot()
			item.GroupIDs = runtimeSnapshot.GroupIDs
			if !isAntigravity {
				item.Status = runtimeSnapshot.Status
				item.UsingCredits = runtimeSnapshot.UsingCredits
				if runtimePlan := runtimeSnapshot.PlanType; runtimePlan != "" {
					item.PlanType = runtimePlan
					if resolved, ok := auth.ResolveGrokPlan(runtimePlan); ok {
						item.GrokPlanCategory = resolved.Key
					}
				}
				if runtimeSnapshot.UsagePercent5hValid {
					item.UsagePercent5h, item.UsagePercent5hOK = runtimeSnapshot.UsagePercent5h, true
				}
				if runtimeSnapshot.UsagePercent7dValid {
					item.UsagePercent7d, item.UsagePercent7dOK = runtimeSnapshot.UsagePercent7d, true
				}
				if runtimeSnapshot.UsagePercentSparkValid {
					item.UsagePercentSpark, item.UsagePercentSparkOK = runtimeSnapshot.UsagePercentSpark, true
				}
				item.HealthTier = runtimeSnapshot.HealthTier
				item.DispatchScore = runtimeSnapshot.DispatchScore
				item.LatencyPenalty = runtimeSnapshot.LatencyPenalty
				item.LastUnauthorizedAt = runtimeSnapshot.LastUnauthorizedAt
				item.LastRateLimitedAt = runtimeSnapshot.LastRateLimitedAt
				item.LastTimeoutAt = runtimeSnapshot.LastTimeoutAt
				item.ActiveRequests = runtimeSnapshot.ActiveRequests
				item.OccupiedRequests = runtimeSnapshot.OccupiedRequests
				item.DynamicConcurrency = runtimeSnapshot.DynamicConcurrencyLimit
				item.Reset5hAt = runtimeSnapshot.Reset5hAt
				item.Reset7dAt = runtimeSnapshot.Reset7dAt
				item.ResetSparkAt = runtimeSnapshot.ResetSparkAt
				item.Window7dSeconds = runtimeSnapshot.Window7dSeconds
				if runtimeSnapshot.CooldownReason != "" {
					item.CooldownReason = runtimeSnapshot.CooldownReason
					item.CooldownUntil = runtimeSnapshot.CooldownUntil
				}
			}
		}
	}
	if counts := requestCounts[row.ID]; counts != nil {
		item.RequestCount = counts.SuccessCount + counts.ErrorCount
	}
	if today := todayUsage[row.ID]; today != nil {
		item.TodayRequests = today.Requests
		item.TodayTokens = today.Tokens
		item.TodayAccountBilled = today.AccountBilled
	}
	groupKeys := make([]string, 0, len(item.GroupIDs))
	groupLabels := make([]string, 0, len(item.GroupIDs))
	for _, id := range item.GroupIDs {
		if key := groupSort[id]; key != "" {
			groupKeys = append(groupKeys, key)
		}
		if name := groupNames[id]; name != "" {
			groupLabels = append(groupLabels, name)
		}
	}
	sort.Strings(groupKeys)
	item.GroupSortKey = strings.Join(groupKeys, "\x00")
	searchParts := []string{row.Name, email, strconv.FormatInt(row.ID, 10), item.EmailDomain}
	if isGrok {
		searchParts = append(searchParts,
			strings.Join(row.GetCredentialStringSlice("models"), " "), row.GetCredential("base_url"),
			item.PlanType, item.GrokPlanCategory, row.ErrorMessage, row.ProxyURL, strings.Join(groupLabels, " "))
	} else if isAntigravity {
		searchParts = append(searchParts, item.PlanType, row.GetCredential("project_id"), row.GetCredential("antigravity_sync_error"), strings.Join(groupLabels, " "))
	} else if isClaude {
		searchParts = append(searchParts,
			strings.Join(row.GetCredentialStringSlice("models"), " "), row.GetCredential("base_url"),
			item.PlanType, row.GetCredential(auth.ClaudeUsageProbeErrorCredentialKey), row.ErrorMessage,
			row.ProxyURL, strings.Join(groupLabels, " "))
	}
	item.SearchText = strings.ToLower(strings.Join(searchParts, " "))
	return item
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// requestCountCacheEntry 是单个渠道的请求数统计缓存。
type requestCountCacheEntry struct {
	counts     map[int64]*database.AccountRequestCount
	today      map[int64]*database.AccountTimeRangeUsage
	todayStart time.Time
	expiresAt  time.Time
}

func requestCountCacheReady(entry *requestCountCacheEntry, now time.Time) bool {
	if entry == nil || !now.Before(entry.expiresAt) {
		return false
	}
	if entry.todayStart.IsZero() {
		return true
	}
	return database.StartOfDay(now).Equal(entry.todayStart)
}

// getCachedRequestCountsNonBlocking 返回指定渠道的请求数统计。统计按渠道独立
// 缓存与刷新:在 codex 页刷新只扫 codex 账号的日志行,不为 grok 账号买单,
// 反之亦然。ids 是该渠道当前的账号列表,过期时交给后台刷新用。
func (h *Handler) getCachedRequestCountsNonBlocking(channel string, ids []int64) (map[int64]*database.AccountRequestCount, map[int64]*database.AccountTimeRangeUsage, string) {
	now := time.Now()
	h.reqCountMu.RLock()
	entry := h.reqCountCache[channel]
	h.reqCountMu.RUnlock()
	if requestCountCacheReady(entry, now) {
		return entry.counts, entry.today, "ready"
	}
	h.refreshRequestCountsAsync(channel, ids)
	if entry != nil {
		return entry.counts, entry.today, "stale"
	}
	if len(ids) > requestCountFullPoolScanMax {
		// 大池子首屏不走 warming:空计数即可先出列表,分批聚合在后台跑。
		// 返回 stale 让前端短轮询接到聚完后的快照,而不是整页转圈。
		return map[int64]*database.AccountRequestCount{}, map[int64]*database.AccountTimeRangeUsage{}, "stale"
	}
	return map[int64]*database.AccountRequestCount{}, map[int64]*database.AccountTimeRangeUsage{}, "warming"
}

func (h *Handler) refreshRequestCountsAsync(channel string, ids []int64) {
	h.reqCountRefreshMu.Lock()
	if h.reqCountRefreshing == nil {
		h.reqCountRefreshing = make(map[string]bool)
	}
	if h.reqCountRefreshing[channel] {
		h.reqCountRefreshMu.Unlock()
		return
	}
	h.reqCountRefreshing[channel] = true
	h.reqCountRefreshMu.Unlock()
	go func() {
		defer func() {
			h.reqCountRefreshMu.Lock()
			delete(h.reqCountRefreshing, channel)
			h.reqCountRefreshMu.Unlock()
		}()
		timeout := 15 * time.Second
		if len(ids) > requestCountFullPoolScanMax {
			timeout = requestCountBatchRefreshTimeout(accountRequestStatBatchCount(len(ids)))
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		started := time.Now()
		counts, today, todayStart, err := h.loadAccountRequestStats(ctx, channel, ids)
		if err != nil {
			// 静默失败会让请求数/用量排序与统计永久停在 warming 且无从排查
			// (issue #493),失败必须留痕。
			log.Printf("刷新账号请求统计失败 channel=%s(排序/统计将继续使用旧值): %v", channel, err)
			return
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			log.Printf("账号请求统计刷新耗时 %s channel=%s ids=%d batches=%d(分批 7 天聚合)", elapsed.Round(time.Millisecond), channel, len(ids), accountRequestStatBatchCount(len(ids)))
		}
		h.storeRequestCountCache(channel, counts, today, todayStart)
		// stats_state 是烙在列表快照里的:快照重建时统计缓存还没刷完,烙出来
		// 就是 stale,并一直随快照被返回。统计刷完后把本渠道快照标记为过期,
		// 下一次轮询即触发重建、烙上 ready——否则要等快照自然过期再叠一轮轮询,
		// 前端退避后「统计刷新中…」会挂几分钟。
		h.expireAccountListSnapshot(channel)
	}()
}

func (h *Handler) loadAccountRequestStats(ctx context.Context, channel string, ids []int64) (map[int64]*database.AccountRequestCount, map[int64]*database.AccountTimeRangeUsage, time.Time, error) {
	todayStart := database.StartOfDay(time.Now())
	ids = uniquePositiveAccountIDs(ids)
	if len(ids) == 0 {
		return map[int64]*database.AccountRequestCount{}, map[int64]*database.AccountTimeRangeUsage{}, todayStart, nil
	}
	if len(ids) <= requestCountBatchSize {
		counts, err := h.db.GetAccountRequestCountsByIDs(ctx, ids)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		today, err := h.db.GetAccountUsageSinceByIDs(ctx, ids, todayStart)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		return counts, today, todayStart, nil
	}
	// 断点续跑:上一轮超时/出错留下的半成品接着跑,只查还没完成的账号。
	staging := h.takeRequestCountStaging(channel, todayStart)
	remaining := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := staging.done[id]; !ok {
			remaining = append(remaining, id)
		}
	}
	for _, batch := range chunkInt64IDs(remaining, requestCountBatchSize) {
		if err := ctx.Err(); err != nil {
			h.saveRequestCountStaging(channel, staging)
			return nil, nil, time.Time{}, err
		}
		part, err := h.db.GetAccountRequestCountTotalsByIDs(ctx, batch)
		if err != nil {
			h.saveRequestCountStaging(channel, staging)
			return nil, nil, time.Time{}, err
		}
		todayPart, err := h.db.GetAccountUsageSinceByIDs(ctx, batch, todayStart)
		if err != nil {
			h.saveRequestCountStaging(channel, staging)
			return nil, nil, time.Time{}, err
		}
		for id, value := range part {
			staging.counts[id] = value
		}
		for id, value := range todayPart {
			staging.today[id] = value
		}
		for _, id := range batch {
			staging.done[id] = struct{}{}
		}
	}
	return staging.counts, staging.today, todayStart, nil
}

// requestCountStaging 是大池分批聚合的断点续跑状态。一轮超时/出错不再整轮
// 作废:进度存回 Handler,下一轮跳过已完成账号接着跑。刷新协程间由
// reqCountRefreshing 串行化,这里的锁只负责跨轮的存取与内存可见性。
type requestCountStaging struct {
	counts     map[int64]*database.AccountRequestCount
	today      map[int64]*database.AccountTimeRangeUsage
	done       map[int64]struct{}
	todayStart time.Time
	startedAt  time.Time
}

func (h *Handler) takeRequestCountStaging(channel string, todayStart time.Time) *requestCountStaging {
	h.reqCountStagingMu.Lock()
	defer h.reqCountStagingMu.Unlock()
	staging := h.reqCountStaging[channel]
	delete(h.reqCountStaging, channel)
	if staging != nil {
		// 换天(今日口径已变)或搁太久(数据太陈)的半成品不能续,重来。
		if !staging.todayStart.Equal(todayStart) || time.Since(staging.startedAt) > requestCountStagingMaxAge {
			staging = nil
		}
	}
	if staging == nil {
		staging = &requestCountStaging{
			counts:     make(map[int64]*database.AccountRequestCount),
			today:      make(map[int64]*database.AccountTimeRangeUsage),
			done:       make(map[int64]struct{}),
			todayStart: todayStart,
			startedAt:  time.Now(),
		}
	}
	return staging
}

func (h *Handler) saveRequestCountStaging(channel string, staging *requestCountStaging) {
	h.reqCountStagingMu.Lock()
	if h.reqCountStaging == nil {
		h.reqCountStaging = make(map[string]*requestCountStaging)
	}
	h.reqCountStaging[channel] = staging
	h.reqCountStagingMu.Unlock()
}

// requestCountBatchRefreshTimeout 按批数缩放全池刷新时限:固定时限在 3 万+
// 池上会周期性超时,叠加整轮作废就是永远白干的重扫循环。
func requestCountBatchRefreshTimeout(batches int) time.Duration {
	timeout := requestCountBatchRefreshBaseTimeout + time.Duration(batches)*requestCountBatchBudget
	if timeout > requestCountBatchRefreshMaxTimeout {
		return requestCountBatchRefreshMaxTimeout
	}
	return timeout
}

func uniquePositiveAccountIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func chunkInt64IDs(ids []int64, size int) [][]int64 {
	if size <= 0 {
		size = requestCountBatchSize
	}
	if len(ids) == 0 {
		return nil
	}
	batches := make([][]int64, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		batches = append(batches, ids[start:end])
	}
	return batches
}

func accountRequestStatBatchCount(idCount int) int {
	if idCount <= 0 {
		return 0
	}
	return (idCount + requestCountBatchSize - 1) / requestCountBatchSize
}

func (h *Handler) storeRequestCountCache(channel string, counts map[int64]*database.AccountRequestCount, today map[int64]*database.AccountTimeRangeUsage, todayStart time.Time) {
	h.reqCountMu.Lock()
	if h.reqCountCache == nil {
		h.reqCountCache = make(map[string]*requestCountCacheEntry)
	}
	h.reqCountCache[channel] = &requestCountCacheEntry{
		counts:     counts,
		today:      today,
		todayStart: todayStart,
		expiresAt:  time.Now().Add(requestCountCacheTTL),
	}
	h.reqCountMu.Unlock()
}

// expireAccountListSnapshot 把指定渠道的列表快照标记为过期,但保留内容:
// 读路径仍按 stale-while-revalidate 先返回旧值,只是下一次读取会立刻触发重建。
func (h *Handler) expireAccountListSnapshot(channel string) {
	// Invalidate in-flight rebuilds as well as the cached TTL. A probe may
	// finish while an older projection query is still running; without a new
	// generation that stale query could reinstall the pre-probe metadata.
	if channel == database.UpstreamChannelClaude {
		h.claudeAccountCachesGen.Add(1)
	} else {
		h.accountCachesGen.Add(1)
	}
	h.accountListCacheMu.Lock()
	if cached := h.accountListCache[channel]; cached != nil {
		cached.ExpiresAt = time.Time{}
	}
	h.accountListCacheMu.Unlock()
}

func accountListItemMatches(item *accountListSnapshotItem, query accountPageQuery, channel string) bool {
	if query.Search != "" && !strings.Contains(item.SearchText, query.Search) {
		return false
	}
	if query.Status != "" && query.Status != "all" && !accountListStatusMatches(item, query.Status, channel) {
		return false
	}
	if query.Plan != "" && query.Plan != "all" {
		if channel == database.UpstreamChannelGrok {
			if item.GrokPlanCategory != query.Plan {
				return false
			}
		} else if !strings.EqualFold(strings.TrimSpace(item.PlanType), query.Plan) {
			return false
		}
	}
	if query.AuthKind != "" && query.AuthKind != "all" {
		if channel == database.UpstreamChannelGrok {
			if item.GrokAuthKind != query.AuthKind {
				return false
			}
		} else if channel == database.UpstreamChannelClaude {
			// Claude 渠道:oauth=可刷新 OAuth 凭据,setup_token=长效 Setup Token。
			if item.ClaudeAuthKind != query.AuthKind {
				return false
			}
		} else if item.OpenAIResponses != (query.AuthKind == auth.GrokAuthKindAPIKey) {
			// Codex 渠道复用 auth_kind：api_key=Responses API 中转账号，oauth=官方账号（issue #522）
			return false
		}
	}
	if query.Tag != "" && !containsString(item.Tags, query.Tag) {
		return false
	}
	if query.EmailDomain != "" && item.EmailDomain != query.EmailDomain {
		return false
	}
	if query.HealthTier != "" && query.HealthTier != "all" {
		if query.HealthTier == "attention" {
			if item.HealthTier != "warm" && item.HealthTier != "risky" && item.Status != "unauthorized" {
				return false
			}
		} else if item.HealthTier != query.HealthTier {
			return false
		}
	}
	if query.ProxyFilter != "" && query.ProxyFilter != "all" {
		boundURL := strings.TrimSpace(item.Row.ProxyURL)
		switch query.ProxyFilter {
		case "unbound":
			if boundURL != "" {
				return false
			}
		case "this":
			if query.ProxyURL == "" || boundURL != query.ProxyURL {
				return false
			}
		case "other":
			if boundURL == "" || boundURL == query.ProxyURL {
				return false
			}
		}
	}
	if query.Ungrouped && len(item.GroupIDs) != 0 {
		return false
	}
	if len(query.GroupInclude) > 0 && !intersectsInt64(item.GroupIDs, query.GroupInclude) {
		return false
	}
	if len(query.GroupExclude) > 0 && intersectsInt64(item.GroupIDs, query.GroupExclude) {
		return false
	}
	return true
}

func accountListStatusMatches(item *accountListSnapshotItem, status, channel string) bool {
	banned := item.Status == "unauthorized"
	errorState := item.Status == "error"
	limited := accountListRateLimited(item)
	if channel == database.UpstreamChannelGrok {
		switch status {
		case "normal":
			return accountListNormal(item)
		case "active", "scheduling":
			return accountListSchedulable(item)
		case "overload_paused":
			return accountListOverloadPaused(item)
		case "rate_limited":
			return limited
		case "disabled":
			return !item.Enabled
		case "banned":
			return banned
		case "error":
			return errorState
		}
		return true
	}
	switch status {
	case "normal":
		// 过载暂停、禁用都归入「正常」；真正可调度的账号走 scheduling/active。
		return accountListNormal(item)
	case "active", "scheduling":
		return accountListSchedulable(item)
	case "overload_paused":
		return accountListOverloadPaused(item)
	case "rate_limited":
		return !banned && !errorState && limited
	case "abnormal":
		return banned || errorState
	case "banned":
		return banned
	case "error":
		return errorState
	case "unsampled":
		return accountListUnsampled(item)
	case "disabled":
		return !item.Enabled
	case "locked":
		return item.Locked
	}
	return true
}

func accountListOverloadPaused(item *accountListSnapshotItem) bool {
	return strings.EqualFold(item.Status, "overload_paused") ||
		strings.EqualFold(item.CooldownReason, "overload_paused")
}

func accountListUnsampled(item *accountListSnapshotItem) bool {
	if item == nil || item.OpenAIResponses || item.GrokAuthKind != "" || item.Antigravity {
		return false
	}
	if item.Status == "unauthorized" || item.Status == "error" {
		return false
	}
	// k12 等 team 型工作区可能只返回 5h 窗口：任一窗口有数据即算已采样。
	if item.UsagePercent5hOK || item.UsagePercent7dOK {
		return false
	}
	// Claude 的 native Messages 端点可能合法地省略统一配额头；一次成功
	// 的 provider-native probe 仍代表账号已采样，只是配额未知。
	return item.ClaudeUsageProbeAt == "" || item.ClaudeUsageProbeErr != ""
}

func accountListNormal(item *accountListSnapshotItem) bool {
	if item.Status == "unauthorized" || item.Status == "error" || accountListUnsampled(item) {
		return false
	}
	if !item.Enabled || accountListOverloadPaused(item) {
		return true
	}
	return !accountListRateLimited(item)
}

func accountListSchedulable(item *accountListSnapshotItem) bool {
	return item.Enabled &&
		item.Status != "unauthorized" &&
		item.Status != "error" &&
		!accountListUnsampled(item) &&
		!accountListRateLimited(item) &&
		!accountListOverloadPaused(item)
}

func accountListRateLimited(item *accountListSnapshotItem) bool {
	if item.UsingCredits || item.Status == "unauthorized" || item.Status == "error" {
		return false
	}
	if accountListOverloadPaused(item) {
		return false
	}
	limited := map[string]bool{
		"usage_limited": true, "usage_exhausted": true, "rate_limited": true,
		auth.ResponsesRateLimitedCooldownReason: true,
		"rate_limited_5h":                       true,
		"rate_limited_7d":                       true,
		"quota_paused":                          true,
	}
	return limited[strings.ToLower(item.Status)] || limited[strings.ToLower(item.CooldownReason)]
}

func sortAccountListItems(items []*accountListSnapshotItem, key, order string) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		cmp := 0
		switch key {
		case "requests":
			cmp = compareInt64(a.RequestCount, b.RequestCount)
		case "today":
			cmp = compareInt64(a.TodayRequests, b.TodayRequests)
			if cmp == 0 {
				cmp = compareInt64(a.TodayTokens, b.TodayTokens)
			}
			if cmp == 0 {
				cmp = compareFloat64(a.TodayAccountBilled, b.TodayAccountBilled)
			}
		case "usage":
			cmp = compareFloat64(accountListUsageValue(a), accountListUsageValue(b))
		case "created_at":
			cmp = compareTime(a.Row.CreatedAt, b.Row.CreatedAt)
		case "updated_at":
			cmp = compareTime(a.Row.UpdatedAt, b.Row.UpdatedAt)
		case "scheduler_priority":
			cmp = compareInt64(a.SchedulerPriority, b.SchedulerPriority)
		case "group":
			cmp = strings.Compare(a.GroupSortKey, b.GroupSortKey)
		case "risk":
			cmp = compareInt64(accountListRiskRank(a), accountListRiskRank(b))
			if cmp == 0 {
				cmp = compareFloat64(a.DispatchScore, b.DispatchScore)
			}
		case "dispatch_score":
			cmp = compareFloat64(a.DispatchScore, b.DispatchScore)
		case "latency_penalty":
			cmp = compareFloat64(a.LatencyPenalty, b.LatencyPenalty)
		case "unauthorized":
			cmp = compareTime(a.LastUnauthorizedAt, b.LastUnauthorizedAt)
		default:
			return a.ID < b.ID
		}
		if cmp == 0 {
			return a.ID < b.ID
		}
		if order == "asc" {
			return cmp < 0
		}
		return cmp > 0
	})
}

func accountListRiskRank(item *accountListSnapshotItem) int64 {
	if item.Status == "unauthorized" || item.HealthTier == "banned" {
		return 3
	}
	if item.HealthTier == "risky" {
		return 2
	}
	if item.HealthTier == "warm" {
		return 1
	}
	return 0
}

func accountListUsageValue(item *accountListSnapshotItem) float64 {
	if item.UsagePercent7dOK {
		return item.UsagePercent7d
	}
	if item.UsagePercent5hOK {
		return item.UsagePercent5h
	}
	return -1
}

func compareInt64(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareFloat64(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareTime(a, b time.Time) int {
	if a.Before(b) {
		return -1
	}
	if a.After(b) {
		return 1
	}
	return 0
}

func summarizeAccountList(items []*accountListSnapshotItem, channel string) (accountListSummary, accountListFacets) {
	var summary accountListSummary
	now := time.Now()
	tags := make(map[string]struct{})
	domains := make(map[string]*accountListDomainFacet)
	for _, item := range items {
		summary.Total++
		banned := item.Status == "unauthorized"
		errorState := item.Status == "error"
		limited := accountListRateLimited(item)
		overloadPaused := accountListOverloadPaused(item)
		if banned {
			summary.Banned++
		}
		if errorState {
			summary.Error++
		}
		if banned || errorState {
			summary.Abnormal++
		}
		if overloadPaused {
			summary.OverloadPaused++
		}
		if limited {
			summary.RateLimited++
			status := strings.ToLower(item.Status + " " + item.CooldownReason)
			if strings.Contains(status, "5h") {
				summary.RateLimited5h++
			} else if strings.Contains(status, "7d") || item.UsagePercent7dOK && item.UsagePercent7d >= 100 {
				summary.RateLimited7d++
			} else if item.UsagePercent5hOK && item.UsagePercent5h >= 100 {
				summary.RateLimited5h++
			}
		}
		if accountListNormal(item) {
			summary.Normal++
		}
		if accountListSchedulable(item) {
			summary.Active++
		}
		if !item.Enabled {
			summary.Disabled++
			if containsString(item.Tags, selfServiceTag) {
				summary.SelfServicePending++
			}
		}
		if item.Locked {
			summary.Locked++
		}
		if accountListUnsampled(item) {
			summary.Unsampled++
		}
		switch item.HealthTier {
		case "healthy":
			summary.Healthy++
		case "warm":
			summary.Warm++
		case "risky":
			summary.Risky++
		}
		if item.GrokAuthKind == auth.GrokAuthKindOAuth {
			summary.OAuth++
		}
		if item.GrokAuthKind == auth.GrokAuthKindAPIKey {
			summary.APIKey++
		}
		if item.Claude {
			if item.ClaudeAuthKind == auth.ClaudeAuthKindAPIKey {
				summary.APIKey++
			} else if item.ClaudeAuthKind == auth.ClaudeAuthKindSetupToken {
				summary.SetupToken++
			} else {
				summary.OAuth++
			}
		}
		if channel == database.UpstreamChannelCodex {
			if item.OpenAIResponses {
				summary.APIKey++
			} else {
				summary.OAuth++
			}
		}
		if accountListSubscriptionUnlocked(item, channel) {
			summary.SubscriptionUnlocked++
		}
		if !item.LastUnauthorizedAt.IsZero() && now.Sub(item.LastUnauthorizedAt) <= 24*time.Hour {
			summary.Unauthorized24h++
		}
		if !item.LastRateLimitedAt.IsZero() && now.Sub(item.LastRateLimitedAt) <= time.Hour {
			summary.RateLimited1h++
		}
		if !item.LastTimeoutAt.IsZero() && now.Sub(item.LastTimeoutAt) <= 15*time.Minute {
			summary.Timeout15m++
		}
		for _, tag := range item.Tags {
			if tag != "" {
				tags[tag] = struct{}{}
			}
		}
		if item.EmailDomain != "" {
			facet := domains[item.EmailDomain]
			if facet == nil {
				facet = &accountListDomainFacet{Domain: item.EmailDomain}
				domains[item.EmailDomain] = facet
			}
			facet.Total++
			if banned {
				facet.Banned++
			}
		}
	}
	facets := accountListFacets{Tags: make([]string, 0, len(tags)), EmailDomains: make([]accountListDomainFacet, 0, len(domains))}
	for tag := range tags {
		facets.Tags = append(facets.Tags, tag)
	}
	sort.Strings(facets.Tags)
	for _, facet := range domains {
		facets.EmailDomains = append(facets.EmailDomains, *facet)
	}
	sort.Slice(facets.EmailDomains, func(i, j int) bool {
		a, b := facets.EmailDomains[i], facets.EmailDomains[j]
		if a.Banned != b.Banned {
			return a.Banned > b.Banned
		}
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Domain < b.Domain
	})
	return summary, facets
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func intersectsInt64(a, b []int64) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[int64]struct{}, len(a))
	for _, value := range a {
		set[value] = struct{}{}
	}
	for _, value := range b {
		if _, ok := set[value]; ok {
			return true
		}
	}
	return false
}

func accountListSubscriptionPlan(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "pro", "prolite", "pro_lite", "pro-lite", "plus", "team", "teamplus", "k12", "edu", "education", "go":
		return true
	default:
		return false
	}
}

// accountListSubscriptionUnlocked applies the subscription filter using the
// provider's own plan vocabulary. Codex and Claude expose different plan
// names, while relay/auxiliary providers have no subscription semantics in
// this list. Keeping the channel check here prevents a generic selector from
// accidentally treating another provider's plan as a Codex entitlement.
func accountListSubscriptionUnlocked(item *accountListSnapshotItem, channel string) bool {
	if item == nil || item.Locked {
		return false
	}
	switch channel {
	case database.UpstreamChannelCodex:
		return accountListSubscriptionPlan(item.PlanType)
	case database.UpstreamChannelClaude:
		return accountList5hQuotaEligible(item)
	default:
		return false
	}
}

// accountList5hQuotaEligible keeps provider-specific subscription semantics in
// one place. Claude OAuth plans (pro/max-5x/max-20x/team) expose a rolling 5h
// window even though they are not Codex plan names.
func accountList5hQuotaEligible(item *accountListSnapshotItem) bool {
	if item == nil {
		return false
	}
	if item.Claude || (item.Row != nil && strings.EqualFold(strings.TrimSpace(item.Row.GetCredential("upstream_type")), auth.UpstreamClaude)) {
		plan := strings.ToLower(strings.TrimSpace(item.PlanType))
		switch plan {
		case "claude", "pro", "max", "max-5x", "max-20x", "team", "enterprise", "business",
			"claude-pro", "claude-max", "claude-max-5x", "claude-max-20x", "claude-team", "claude-enterprise", "claude-business":
			return true
		default:
			return false
		}
	}
	return accountListSubscriptionPlan(item.PlanType)
}
