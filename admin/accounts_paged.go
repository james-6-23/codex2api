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
	accountListSnapshotTTL = 5 * time.Second
	accountListPageMax     = 500
	accountListPageDefault = 20
	requestCountCacheTTL   = 10 * time.Second
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
	Row                *database.AccountRow
	ID                 int64
	Status             string
	CooldownReason     string
	Enabled            bool
	Locked             bool
	UsingCredits       bool
	PlanType           string
	GrokAuthKind       string
	GrokPlanCategory   string
	Email              string
	EmailDomain        string
	Tags               []string
	GroupIDs           []int64
	GroupSortKey       string
	UsagePercent5h     float64
	UsagePercent5hOK   bool
	UsagePercent7d     float64
	UsagePercent7dOK   bool
	RequestCount       int64
	SchedulerPriority  int64
	HealthTier         string
	DispatchScore      float64
	LatencyPenalty     float64
	LastUnauthorizedAt time.Time
	LastRateLimitedAt  time.Time
	LastTimeoutAt      time.Time
	Reset5hAt          time.Time
	Reset7dAt          time.Time
	CooldownUntil      time.Time
	Window7dSeconds    int64
	ActiveRequests     int64
	DynamicConcurrency int64
	OpenAIResponses    bool
	SearchText         string
}

type accountListSummary struct {
	Total                int `json:"total"`
	Normal               int `json:"normal"`
	Active               int `json:"active"`
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
	SubscriptionUnlocked int `json:"subscription_unlocked"`
	Unauthorized24h      int `json:"unauthorized_24h"`
	RateLimited1h        int `json:"rate_limited_1h"`
	Timeout15m           int `json:"timeout_15m"`
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
	Accounts   []accountResponse  `json:"accounts"`
	Page       int                `json:"page"`
	PageSize   int                `json:"page_size"`
	Total      int                `json:"total"`
	Summary    accountListSummary `json:"summary"`
	Facets     accountListFacets  `json:"facets"`
	SnapshotAt string             `json:"snapshot_at"`
	StatsState string             `json:"stats_state"`
}

type accountPageSelection struct {
	Rows       []*database.AccountRow
	Page       int
	PageSize   int
	Total      int
	Summary    accountListSummary
	Facets     accountListFacets
	SnapshotAt time.Time
	StatsState string
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
	if channel != database.UpstreamChannelCodex && channel != database.UpstreamChannelGrok && channel != database.UpstreamChannelClaude {
		return nil, fmt.Errorf("selector channel must be codex, grok or claude")
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
		if selector.SubscriptionUnlocked && (!accountListSubscriptionPlan(item.PlanType) || item.Locked) {
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
		"": true, "requests": true, "usage": true, "created_at": true, "updated_at": true,
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
		"": true, "all": true, "normal": true, "active": true,
		"rate_limited": true, "abnormal": true, "banned": true,
		"error": true, "unsampled": true, "disabled": true, "locked": true,
	}
	if !validStatuses[query.Status] {
		return fmt.Errorf("unsupported status")
	}
	validAuthKinds := map[string]bool{"": true, "all": true, auth.GrokAuthKindOAuth: true, auth.GrokAuthKindAPIKey: true}
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
	sortAccountListItems(filtered, query.Sort, query.Order)
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
	}, nil
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
func shouldInvalidateAccountSnapshotCaches(method, path string, status int) bool {
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return false
	}
	if status >= http.StatusBadRequest {
		return false
	}
	return strings.HasPrefix(path, "/api/admin/accounts") ||
		strings.HasPrefix(path, "/api/admin/account-groups")
}

// invalidateAccountSnapshotCaches 在账号发生变更(删除/封禁/禁用/导入等)后
// 丢弃列表快照与分析缓存,让下一次读取同步重建。否则 stale-while-revalidate
// 的读路径会把变更前的统计卡/筛选计数原样返回给变更后的第一次刷新。
func (h *Handler) invalidateAccountSnapshotCaches() {
	h.accountCachesGen.Add(1)
	h.accountListCacheMu.Lock()
	h.accountListCache = nil
	h.accountListCacheMu.Unlock()
	h.accountAnalysisCacheMu.Lock()
	h.accountAnalysisCache = nil
	h.accountAnalysisCacheMu.Unlock()
}

func (h *Handler) rebuildAccountListSnapshot(ctx context.Context, channel string) (*accountListSnapshot, error) {
	gen := h.accountCachesGen.Load()
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
	requestCounts, statsState := h.getCachedRequestCountsNonBlocking()
	items := make([]*accountListSnapshotItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, h.buildAccountListSnapshotItem(row, requestCounts, groupNames, groupSort))
	}
	snapshot := &accountListSnapshot{
		Channel: channel, Items: items, BuiltAt: time.Now(), StatsState: statsState,
	}
	snapshot.ExpiresAt = snapshot.BuiltAt.Add(accountListSnapshotTTL)
	snapshot.Summary, snapshot.Facets = summarizeAccountList(items, channel)
	h.installAccountListSnapshot(channel, snapshot, gen)
	return snapshot, nil
}

// installAccountListSnapshot 只在代数未漂移时入缓存:读库期间发生过账号
// 变更的快照可能早于变更,返回给当前调用方无妨,但不能留给后续请求。
func (h *Handler) installAccountListSnapshot(channel string, snapshot *accountListSnapshot, gen uint64) {
	if h.accountCachesGen.Load() != gen {
		return
	}
	h.accountListCacheMu.Lock()
	if h.accountListCache == nil {
		h.accountListCache = make(map[string]*accountListSnapshot)
	}
	h.accountListCache[channel] = snapshot
	h.accountListCacheMu.Unlock()
}

func (h *Handler) buildAccountListSnapshotItem(row *database.AccountRow, requestCounts map[int64]*database.AccountRequestCount, groupNames, groupSort map[int64]string) *accountListSnapshotItem {
	upstreamType := strings.TrimSpace(row.GetCredential("upstream_type"))
	isGrok := strings.EqualFold(upstreamType, auth.UpstreamGrok)
	isOpenAIResponses := strings.EqualFold(upstreamType, auth.UpstreamOpenAIResponses)
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
	item := &accountListSnapshotItem{
		Row: row, ID: row.ID, Status: row.Status, CooldownReason: row.CooldownReason,
		Enabled: row.Enabled, Locked: row.Locked, PlanType: planType, GrokAuthKind: grokAuthKind,
		Email: email, EmailDomain: accountEmailDomain(email), Tags: append([]string(nil), row.Tags...),
		SchedulerPriority: valueOrZero(accountSchedulerPriority(row)), OpenAIResponses: isOpenAIResponses,
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
			item.Status = runtimeSnapshot.Status
			item.UsingCredits = runtimeSnapshot.UsingCredits
			item.GroupIDs = runtimeSnapshot.GroupIDs
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
			item.HealthTier = runtimeSnapshot.HealthTier
			item.DispatchScore = runtimeSnapshot.DispatchScore
			item.LatencyPenalty = runtimeSnapshot.LatencyPenalty
			item.LastUnauthorizedAt = runtimeSnapshot.LastUnauthorizedAt
			item.LastRateLimitedAt = runtimeSnapshot.LastRateLimitedAt
			item.LastTimeoutAt = runtimeSnapshot.LastTimeoutAt
			item.ActiveRequests = runtimeSnapshot.ActiveRequests
			item.DynamicConcurrency = runtimeSnapshot.DynamicConcurrencyLimit
			item.Reset5hAt = runtimeSnapshot.Reset5hAt
			item.Reset7dAt = runtimeSnapshot.Reset7dAt
			item.Window7dSeconds = runtimeSnapshot.Window7dSeconds
			if runtimeSnapshot.CooldownReason != "" {
				item.CooldownReason = runtimeSnapshot.CooldownReason
				item.CooldownUntil = runtimeSnapshot.CooldownUntil
			}
		}
	}
	if counts := requestCounts[row.ID]; counts != nil {
		item.RequestCount = counts.SuccessCount + counts.ErrorCount
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

func (h *Handler) getCachedRequestCountsNonBlocking() (map[int64]*database.AccountRequestCount, string) {
	now := time.Now()
	h.reqCountMu.RLock()
	cached := h.reqCountCache
	fresh := cached != nil && now.Before(h.reqCountExpiresAt)
	h.reqCountMu.RUnlock()
	if fresh {
		return cached, "ready"
	}
	h.refreshRequestCountsAsync()
	if cached != nil {
		return cached, "stale"
	}
	return map[int64]*database.AccountRequestCount{}, "warming"
}

func (h *Handler) refreshRequestCountsAsync() {
	if !h.reqCountRefreshMu.TryLock() {
		return
	}
	go func() {
		defer h.reqCountRefreshMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		counts, err := h.db.GetAccountRequestCounts(ctx)
		if err != nil {
			// 静默失败会让请求数/用量排序与统计永久停在 warming 且无从排查
			// (issue #493),失败必须留痕。
			log.Printf("刷新账号请求统计失败(排序/统计将继续使用旧值): %v", err)
			return
		}
		h.reqCountMu.Lock()
		h.reqCountCache = counts
		h.reqCountExpiresAt = time.Now().Add(requestCountCacheTTL)
		h.reqCountMu.Unlock()
	}()
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
	if query.AuthKind != "" && query.AuthKind != "all" && item.GrokAuthKind != query.AuthKind {
		return false
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
		case "active", "normal":
			return item.Enabled && !banned && !errorState && !limited
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
	case "normal", "active":
		return !banned && !errorState && !limited
	case "rate_limited":
		return !banned && !errorState && limited
	case "abnormal":
		return banned || errorState
	case "banned":
		return banned
	case "error":
		return errorState
	case "unsampled":
		return !item.OpenAIResponses && !item.UsagePercent5hOK && !item.UsagePercent7dOK
	case "disabled":
		return !item.Enabled
	case "locked":
		return item.Locked
	}
	return true
}

func accountListRateLimited(item *accountListSnapshotItem) bool {
	if item.UsingCredits || item.Status == "unauthorized" || item.Status == "error" {
		return false
	}
	limited := map[string]bool{
		"usage_limited": true, "usage_exhausted": true, "rate_limited": true,
		"rate_limited_5h": true, "rate_limited_7d": true, "quota_paused": true,
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
		if banned {
			summary.Banned++
		}
		if errorState {
			summary.Error++
		}
		if banned || errorState {
			summary.Abnormal++
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
		if !banned && !errorState && !limited {
			summary.Normal++
		}
		if item.Enabled && !banned && !errorState && !limited {
			summary.Active++
		}
		if !item.Enabled {
			summary.Disabled++
		}
		if item.Locked {
			summary.Locked++
		}
		if !item.OpenAIResponses && !item.UsagePercent5hOK && !item.UsagePercent7dOK {
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
		if channel == database.UpstreamChannelCodex && accountListSubscriptionPlan(item.PlanType) && !item.Locked {
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
