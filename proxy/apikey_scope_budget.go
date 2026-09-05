// Package proxy: API Key × 账号分组/账号 维度的用量预算（issue #439）。
//
// 与 apikey_limits.go 里的 Key 级限额不同,这里的限额只统计「该 Key 打到该 scope 的用量」,
// 因此判定结果不是简单的放行/拒绝,而是**调度候选过滤**:
//
//   - on_exhausted=skip(默认): 耗尽的 scope 从本次请求的候选池剔除,请求自动落到该 Key
//     允许的其它分组/账号;候选被剔空时返回 429 并说明是 scope 预算耗尽(而不是 503)。
//   - on_exhausted=reject:     只要该 scope 耗尽,这个 Key 的请求一律 429,不换号。
//
// 数据来源:usage_logs 按账号拆分的窗口聚合(每窗口每 60s 一次查询,与配了多少个 scope 无关)
// + 本进程内的增量事件修正。后者用于压掉 60s 缓存滞后带来的超支——高并发下没有它,
// 预算耗尽后仍会继续打满一整个缓存周期。
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	apiKeyScopeUsageCacheNamespace = "api-key-scope-usage"
	apiKeyScopeUsageCacheTTL       = 60 * time.Second
	// 本地增量只需覆盖缓存滞后窗口,留 5 分钟余量即可,不需要按限额窗口(最长 30d)保留。
	apiKeyScopeDeltaRetention = 5 * time.Minute
	// 单个 Key 保留的增量事件上限。超过后丢弃最旧的一半:极端速率下增量修正会偏低,
	// 但兜底的 DB 聚合仍然准确,不会漏判。
	apiKeyScopeDeltaMaxEvents = 4096
)

// apiKeyScopeUsageSnapshot 是缓存里的按账号窗口用量快照。SnapshotAt 用于和本地增量
// 拼接:只把快照时间之后发生的事件叠加上去,避免重复计数。
type apiKeyScopeUsageSnapshot struct {
	SnapshotAt time.Time                            `json:"snapshot_at"`
	Accounts   map[int64]database.APIKeyWindowUsage `json:"accounts"`
}

// ==================== 本地增量事件 ====================

type apiKeyScopeUsageEvent struct {
	at        time.Time
	accountID int64
	tokens    int64
	cost      float64
}

// apiKeyScopeUsageTracker 记录最近若干分钟内、按 (API Key, 账号) 拆分的用量事件。
// 只跟踪确实配了 scope 限额的 Key(由 buildScopeBudgetGate 标记),其余 Key 零开销。
type apiKeyScopeUsageTracker struct {
	mu       sync.Mutex
	events   map[int64][]apiKeyScopeUsageEvent
	tracked  map[int64]time.Time
	shared   map[int64]*sharedScopeBucketCache
	counters map[int64]*scopeCounterCache
}

// scopeCounterCache 缓存一次累计计数器读取的结果(见 apiKeyScopeCounterCacheTTL)。
type scopeCounterCache struct {
	counters  map[database.APIKeyScopeCounterKey]database.APIKeyScopeCounter
	expiresAt time.Time
}

// sharedScopeBucketCache 缓存一次跨实例分钟桶读取的结果(见 apiKeyScopeSharedReadTTL)。
type sharedScopeBucketCache struct {
	buckets   map[int64]map[int64]database.APIKeyWindowUsage
	readAt    time.Time
	expiresAt time.Time
}

func newAPIKeyScopeUsageTracker() *apiKeyScopeUsageTracker {
	return &apiKeyScopeUsageTracker{
		events:   make(map[int64][]apiKeyScopeUsageEvent),
		tracked:  make(map[int64]time.Time),
		shared:   make(map[int64]*sharedScopeBucketCache),
		counters: make(map[int64]*scopeCounterCache),
	}
}

// isTracked 判断某 Key 是否已被登记为「配了 scope 限额」。未登记的 Key 不记增量,
// 也不写跨实例桶。
func (t *apiKeyScopeUsageTracker) isTracked(apiKeyID int64) bool {
	if t == nil || apiKeyID <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.tracked[apiKeyID]
	return ok
}

func (t *apiKeyScopeUsageTracker) cachedSharedBuckets(apiKeyID int64) (map[int64]map[int64]database.APIKeyWindowUsage, time.Time, bool) {
	if t == nil {
		return nil, time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.shared[apiKeyID]
	if entry == nil || time.Now().After(entry.expiresAt) {
		return nil, time.Time{}, false
	}
	return entry.buckets, entry.readAt, true
}

func (t *apiKeyScopeUsageTracker) storeSharedBuckets(apiKeyID int64, buckets map[int64]map[int64]database.APIKeyWindowUsage, readAt time.Time) {
	if t == nil || apiKeyID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.shared[apiKeyID] = &sharedScopeBucketCache{
		buckets:   buckets,
		readAt:    readAt,
		expiresAt: readAt.Add(apiKeyScopeSharedReadTTL),
	}
}

func (t *apiKeyScopeUsageTracker) cachedCounters(apiKeyID int64) (map[database.APIKeyScopeCounterKey]database.APIKeyScopeCounter, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.counters[apiKeyID]
	if entry == nil || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.counters, true
}

func (t *apiKeyScopeUsageTracker) storeCounters(apiKeyID int64, counters map[database.APIKeyScopeCounterKey]database.APIKeyScopeCounter) {
	if t == nil || apiKeyID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counters[apiKeyID] = &scopeCounterCache{
		counters:  counters,
		expiresAt: time.Now().Add(apiKeyScopeCounterCacheTTL),
	}
}

// markTracked 声明某 Key 需要增量跟踪。每次请求构造闸门时调用,顺带清理长期不活跃的 Key。
func (t *apiKeyScopeUsageTracker) markTracked(apiKeyID int64) {
	if t == nil || apiKeyID <= 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tracked[apiKeyID] = now
	t.sweepLocked(now)
}

// record 追加一条用量事件。未被跟踪的 Key 直接忽略。
func (t *apiKeyScopeUsageTracker) record(apiKeyID, accountID int64, tokens int64, cost float64) {
	if t == nil || apiKeyID <= 0 || accountID <= 0 {
		return
	}
	if tokens <= 0 && cost <= 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.tracked[apiKeyID]; !ok {
		return
	}
	events := append(t.pruneLocked(t.events[apiKeyID], now), apiKeyScopeUsageEvent{
		at:        now,
		accountID: accountID,
		tokens:    tokens,
		cost:      cost,
	})
	if len(events) > apiKeyScopeDeltaMaxEvents {
		events = append([]apiKeyScopeUsageEvent(nil), events[len(events)/2:]...)
	}
	t.events[apiKeyID] = events
}

// deltaSince 返回某 Key 在 since 之后的增量用量(按账号)。since 为零值时返回 nil。
func (t *apiKeyScopeUsageTracker) deltaSince(apiKeyID int64, since time.Time) map[int64]database.APIKeyWindowUsage {
	if since.IsZero() {
		return nil
	}
	return t.deltaSelect(apiKeyID, func(at time.Time) bool { return at.After(since) })
}

// deltaWithSharedBuckets 在跨实例共享增量已启用时挑本地事件,避免与共享桶重复计数:
//   - 落在快照那一分钟、且晚于快照时刻的事件:共享桶跳过了这一分钟,必须由本地补;
//   - 晚于共享桶读取时刻的事件:还没进(或没被读到)共享桶,同样要补。
//
// 其余事件都已被共享桶覆盖。
func (t *apiKeyScopeUsageTracker) deltaWithSharedBuckets(apiKeyID int64, snapshotAt, sharedReadAt time.Time) map[int64]database.APIKeyWindowUsage {
	if snapshotAt.IsZero() {
		return nil
	}
	snapshotMinute := snapshotAt.Unix() / 60
	return t.deltaSelect(apiKeyID, func(at time.Time) bool {
		if at.After(snapshotAt) && at.Unix()/60 <= snapshotMinute {
			return true
		}
		return !sharedReadAt.IsZero() && at.After(sharedReadAt)
	})
}

func (t *apiKeyScopeUsageTracker) deltaSelect(apiKeyID int64, keep func(time.Time) bool) map[int64]database.APIKeyWindowUsage {
	if t == nil || apiKeyID <= 0 || keep == nil {
		return nil
	}
	now := time.Now()
	t.mu.Lock()
	events := t.pruneLocked(t.events[apiKeyID], now)
	t.events[apiKeyID] = events
	snapshot := make([]apiKeyScopeUsageEvent, 0, len(events))
	for _, event := range events {
		if keep(event.at) {
			snapshot = append(snapshot, event)
		}
	}
	t.mu.Unlock()

	if len(snapshot) == 0 {
		return nil
	}
	out := make(map[int64]database.APIKeyWindowUsage, len(snapshot))
	for _, event := range snapshot {
		usage := out[event.accountID]
		usage.Requests++
		usage.Tokens += event.tokens
		usage.UserBilled += event.cost
		out[event.accountID] = usage
	}
	return out
}

func (t *apiKeyScopeUsageTracker) pruneLocked(events []apiKeyScopeUsageEvent, now time.Time) []apiKeyScopeUsageEvent {
	cutoff := now.Add(-apiKeyScopeDeltaRetention)
	idx := 0
	for idx < len(events) && !events[idx].at.After(cutoff) {
		idx++
	}
	if idx == 0 {
		return events
	}
	if idx >= len(events) {
		return nil
	}
	return append([]apiKeyScopeUsageEvent(nil), events[idx:]...)
}

func (t *apiKeyScopeUsageTracker) sweepLocked(now time.Time) {
	cutoff := now.Add(-2 * apiKeyScopeDeltaRetention)
	for apiKeyID, seen := range t.tracked {
		if seen.After(cutoff) {
			continue
		}
		delete(t.tracked, apiKeyID)
		delete(t.events, apiKeyID)
		delete(t.shared, apiKeyID)
		delete(t.counters, apiKeyID)
	}
}

func (h *Handler) apiKeyScopeUsageTracker() *apiKeyScopeUsageTracker {
	if h == nil {
		return nil
	}
	h.scopeUsageMu.Lock()
	defer h.scopeUsageMu.Unlock()
	if h.scopeUsage == nil {
		h.scopeUsage = newAPIKeyScopeUsageTracker()
	}
	return h.scopeUsage
}

// recordAPIKeyScopeUsage 在用量落库前把这笔消耗登记进本地增量。计费口径与
// InsertUsageLog 完全一致(共用 database.UsageLogBilledCost)。
// 运行态缓存跨实例共享(Redis)时同时写一份分钟桶,让其它实例也看得到这笔消耗。
func (h *Handler) recordAPIKeyScopeUsage(input *database.UsageLogInput) {
	if h == nil || input == nil || input.APIKeyID <= 0 || input.AccountID <= 0 {
		return
	}
	if input.StatusCode == 499 {
		return
	}
	tokens := int64(input.TotalTokens)
	cost := database.UsageLogBilledCost(input)
	tracker := h.apiKeyScopeUsageTracker()
	if !tracker.isTracked(input.APIKeyID) {
		return
	}
	tracker.record(input.APIKeyID, input.AccountID, tokens, cost)
	h.publishSharedScopeDelta(input.APIKeyID, input.AccountID, tokens, cost)
}

// ==================== 跨实例共享增量 ====================

const (
	apiKeyScopeSharedDeltaNamespace = "api-key-scope-delta"
	// 分钟桶各自带 TTL,自然过期即清理,不需要额外的字段回收逻辑。
	apiKeyScopeSharedDeltaTTL = 5 * time.Minute
	// 读取当前分钟 + 前两个分钟桶:覆盖 60s 聚合缓存的滞后即够。
	apiKeyScopeSharedDeltaLookback = 3
	// 共享桶的本地读缓存:把每请求 3 次 Redis 往返压到每 5 秒 3 次。
	apiKeyScopeSharedReadTTL = 5 * time.Second
	// 累计额度计数器的本地读缓存。
	apiKeyScopeCounterCacheTTL = 5 * time.Second
)

// sharedScopeDeltaEnabled 判断是否值得走跨实例共享增量。单实例(内存缓存)时
// 本地增量已经完全准确,不必付 Redis 往返。
func (h *Handler) sharedScopeDeltaEnabled() bool {
	return h != nil && h.cache != nil && h.cache.SharedAcrossInstances()
}

func apiKeyScopeSharedDeltaKey(apiKeyID int64, minute int64) string {
	return fmt.Sprintf("%d:%d", apiKeyID, minute)
}

// publishSharedScopeDelta 把这笔消耗累加到当前分钟桶。异步写:记账不该让请求收尾
// 等一次 Redis 往返,丢一次也只是短暂少算,DB 聚合仍会兜住。
func (h *Handler) publishSharedScopeDelta(apiKeyID, accountID, tokens int64, cost float64) {
	if !h.sharedScopeDeltaEnabled() {
		return
	}
	if tokens <= 0 && cost <= 0 {
		return
	}
	deltas := map[string]float64{
		fmt.Sprintf("%d:r", accountID): 1,
		fmt.Sprintf("%d:t", accountID): float64(tokens),
		fmt.Sprintf("%d:c", accountID): cost,
	}
	key := apiKeyScopeSharedDeltaKey(apiKeyID, time.Now().Unix()/60)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.cache.IncrRuntimeCounters(ctx, apiKeyScopeSharedDeltaNamespace, key, deltas, apiKeyScopeSharedDeltaTTL)
	}()
}

// sharedScopeBuckets 读最近几个分钟桶(全部实例的贡献),带 5s 本地缓存。
// 缓存的是**原始分钟桶**而不是过滤后的结果:同一个 Key 的不同窗口(5h/1d/...)快照时刻不同,
// 过滤条件也不同,缓存成品会串。
func (h *Handler) sharedScopeBuckets(ctx context.Context, apiKeyID int64) (map[int64]map[int64]database.APIKeyWindowUsage, time.Time) {
	if !h.sharedScopeDeltaEnabled() {
		return nil, time.Time{}
	}
	tracker := h.apiKeyScopeUsageTracker()
	if buckets, readAt, ok := tracker.cachedSharedBuckets(apiKeyID); ok {
		return buckets, readAt
	}

	nowMinute := time.Now().Unix() / 60
	buckets := make(map[int64]map[int64]database.APIKeyWindowUsage, apiKeyScopeSharedDeltaLookback)
	for i := 0; i < apiKeyScopeSharedDeltaLookback; i++ {
		minute := nowMinute - int64(i)
		counters, err := h.cache.GetRuntimeCounters(ctx, apiKeyScopeSharedDeltaNamespace, apiKeyScopeSharedDeltaKey(apiKeyID, minute))
		if err != nil || len(counters) == 0 {
			continue
		}
		perAccount := make(map[int64]database.APIKeyWindowUsage)
		for field, value := range counters {
			accountID, metric, ok := parseSharedScopeDeltaField(field)
			if !ok {
				continue
			}
			usage := perAccount[accountID]
			switch metric {
			case "r":
				usage.Requests += int64(value)
			case "t":
				usage.Tokens += int64(value)
			case "c":
				usage.UserBilled += value
			}
			perAccount[accountID] = usage
		}
		buckets[minute] = perAccount
	}
	readAt := time.Now()
	tracker.storeSharedBuckets(apiKeyID, buckets, readAt)
	return buckets, readAt
}

// sharedScopeDeltaAfterSnapshot 汇总「严格晚于快照分钟」的共享桶。
// 刻意跳过跨越快照时刻的那个桶:桶里既有快照已包含的用量、也有之后的用量,整桶相加会
// 重复计数。代价是最多漏算其它实例在快照那一分钟内的消耗(本实例的那部分由本地事件补上)。
func sharedScopeDeltaAfterSnapshot(buckets map[int64]map[int64]database.APIKeyWindowUsage, snapshotAt time.Time) map[int64]database.APIKeyWindowUsage {
	if len(buckets) == 0 {
		return nil
	}
	snapshotMinute := snapshotAt.Unix() / 60
	out := make(map[int64]database.APIKeyWindowUsage)
	for minute, perAccount := range buckets {
		if minute <= snapshotMinute {
			continue
		}
		for accountID, usage := range perAccount {
			merged := out[accountID]
			merged.Requests += usage.Requests
			merged.Tokens += usage.Tokens
			merged.UserBilled += usage.UserBilled
			out[accountID] = merged
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseSharedScopeDeltaField(field string) (int64, string, bool) {
	idx := strings.LastIndex(field, ":")
	if idx <= 0 || idx == len(field)-1 {
		return 0, "", false
	}
	accountID, err := strconv.ParseInt(field[:idx], 10, 64)
	if err != nil || accountID <= 0 {
		return 0, "", false
	}
	return accountID, field[idx+1:], true
}

// ==================== 命中统计与日志 ====================

// scopeSkipKey 标识一条 scope 预算的命中统计。
type scopeSkipKey struct {
	apiKeyID  int64
	scopeType string
	scopeID   int64
}

// APIKeyScopeSkipStat 是某条 scope 预算被判定「已耗尽」的运行态统计。
// 只存在进程内(重启清零),用于回答「这条预算真的在生效吗、影响了多少请求」——
// 预算耗尽后请求会静默落到其它账号,没有这个统计只能靠 429 才看出来。
type APIKeyScopeSkipStat struct {
	Requests    int64     `json:"requests"`
	FirstAt     time.Time `json:"first_at"`
	LastAt      time.Time `json:"last_at"`
	LastMessage string    `json:"last_message"`
	Reject      bool      `json:"reject"`
}

type scopeSkipStats struct {
	mu      sync.Mutex
	entries map[scopeSkipKey]*APIKeyScopeSkipStat
	logged  map[scopeSkipKey]time.Time
}

// apiKeyScopeSkipStats 是进程级单例:/v1 流量与管理端读取的可能不是同一个 Handler 实例,
// 统计必须共享才能在管理界面看到。
var apiKeyScopeSkipStats = &scopeSkipStats{
	entries: make(map[scopeSkipKey]*APIKeyScopeSkipStat),
	logged:  make(map[scopeSkipKey]time.Time),
}

// scopeSkipLogInterval 是同一条 scope 预算打日志的最小间隔:预算耗尽期间每个请求都会命中,
// 不节流会直接刷屏。
const scopeSkipLogInterval = time.Minute

// record 登记一次命中(每个请求最多一次,由 evaluateAPIKeyScopeBudgets 调用),
// 并按间隔打一条日志。返回是否需要打日志。
func (s *scopeSkipStats) record(key scopeSkipKey, message string, reject bool, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	if entry == nil {
		entry = &APIKeyScopeSkipStat{FirstAt: now}
		s.entries[key] = entry
	}
	entry.Requests++
	entry.LastAt = now
	entry.LastMessage = message
	entry.Reject = reject

	last, ok := s.logged[key]
	if ok && now.Sub(last) < scopeSkipLogInterval {
		return false
	}
	s.logged[key] = now
	return true
}

// snapshot 返回某 Key 的全部命中统计副本。
func (s *scopeSkipStats) snapshot(apiKeyID int64) map[string]APIKeyScopeSkipStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]APIKeyScopeSkipStat)
	for key, entry := range s.entries {
		if key.apiKeyID != apiKeyID || entry == nil {
			continue
		}
		out[fmt.Sprintf("%s:%d", key.scopeType, key.scopeID)] = *entry
	}
	return out
}

func (s *scopeSkipStats) reset(apiKeyID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.entries {
		if key.apiKeyID == apiKeyID {
			delete(s.entries, key)
			delete(s.logged, key)
		}
	}
}

// APIKeyScopeSkipSnapshot 返回某 API Key 各条 scope 预算的命中统计(键为 "type:id")。
// 供管理端展示"这条预算已生效、影响了多少请求"。
func APIKeyScopeSkipSnapshot(apiKeyID int64) map[string]APIKeyScopeSkipStat {
	if apiKeyID <= 0 {
		return map[string]APIKeyScopeSkipStat{}
	}
	return apiKeyScopeSkipStats.snapshot(apiKeyID)
}

// ResetAPIKeyScopeSkipStats 清掉某 API Key 的命中统计(配置改动后重新观察时用)。
func ResetAPIKeyScopeSkipStats(apiKeyID int64) {
	if apiKeyID > 0 {
		apiKeyScopeSkipStats.reset(apiKeyID)
	}
}

// ==================== 请求级闸门 ====================

// scopeBudgetGate 是单次请求的 scope 预算闸门:记录哪些分组/账号因预算耗尽被剔除,
// 以及被剔除时给下游的说明。nil 闸门表示本次请求没有任何 scope 被剔除。
type scopeBudgetGate struct {
	apiKeyID        int64
	blockedGroups   map[int64]struct{}
	blockedAccounts map[int64]struct{}
	message         string
	blocked         atomic.Int64

	// concurrencyScopes 是配了并发上限的 scope。它与预算耗尽无关——即使全部预算都还有余额，
	// 只要配了并发上限就需要闸门存在（过滤时判位、选中后占位）。
	concurrencyScopes []database.APIKeyScopeLimit
	// concurrencyMessage 记录最近一次因并发位已满剔除候选的说明。
	concurrencyMu      sync.Mutex
	concurrencyMessage string
	leases             []scopeConcurrencyLease
}

func (g *scopeBudgetGate) setLeases(leases []scopeConcurrencyLease) {
	if g == nil {
		return
	}
	g.concurrencyMu.Lock()
	g.leases = leases
	g.concurrencyMu.Unlock()
}

func (g *scopeBudgetGate) releaseLeases() {
	if g == nil {
		return
	}
	g.concurrencyMu.Lock()
	leases := g.leases
	g.leases = nil
	g.concurrencyMu.Unlock()
	for _, lease := range leases {
		apiKeyScopeConcurrency.release(lease)
	}
}

func (g *scopeBudgetGate) noteConcurrencyBlock(message string) {
	if g == nil {
		return
	}
	g.concurrencyMu.Lock()
	g.concurrencyMessage = message
	g.concurrencyMu.Unlock()
}

// concurrencyFullFor 判断账号是否因某条 scope 的并发位已满而不可用。
func (g *scopeBudgetGate) concurrencyFullFor(account *auth.Account, allowance int) bool {
	if g == nil || account == nil || len(g.concurrencyScopes) == 0 {
		return false
	}
	for _, scope := range g.concurrencyScopes {
		if !scopeMatchesAccount(scope, account) {
			continue
		}
		if scopeConcurrencyFull(g.apiKeyID, scope, allowance) {
			g.noteConcurrencyBlock(scopeConcurrencyMessage(scopeLabelForMessage(scope), scope.MaxConcurrency))
			return true
		}
	}
	return false
}

func (g *scopeBudgetGate) blocks(account *auth.Account) bool {
	if g == nil || account == nil {
		return false
	}
	if len(g.blockedAccounts) > 0 {
		if _, ok := g.blockedAccounts[account.ID()]; ok {
			return true
		}
	}
	return account.InAnyGroup(g.blockedGroups)
}

// filter 把闸门包成账号过滤器,叠在既有过滤链最外层。
//
// 刻意先跑内层过滤器再判预算:只有「本来选得上、仅因预算被拒」的账号才计入 blocked,
// 这样「无可用账号」时才敢把 503 换成 scope 预算耗尽的 429——否则模型不匹配之类的
// 拒绝也会被归因到预算上。
func (g *scopeBudgetGate) filter(inner auth.AccountFilter) auth.AccountFilter {
	return g.filterWithConcurrencyAllowance(inner, 0)
}

func (g *scopeBudgetGate) filterWithConcurrencyAllowance(inner auth.AccountFilter, concurrencyAllowance int, traces ...*auth.SelectionTrace) auth.AccountFilter {
	if g == nil {
		return inner
	}
	return func(account *auth.Account) bool {
		if inner != nil && !inner(account) {
			return false
		}
		if g.blocks(account) {
			if len(traces) > 0 {
				traces[0].Reject("scope_budget_exhausted")
			}
			g.blocked.Add(1)
			return false
		}
		if g.concurrencyFullFor(account, concurrencyAllowance) {
			if len(traces) > 0 {
				traces[0].Reject("scope_concurrency_exhausted")
			}
			g.blocked.Add(1)
			return false
		}
		return true
	}
}

// exhaustedMessage 在「无可用账号」时返回 scope 预算耗尽的说明;本次请求没有任何候选
// 因预算被剔除时返回 ""(说明确实是账号池本身没货,应沿用原有 503)。
func (g *scopeBudgetGate) exhaustedMessage() string {
	if g == nil || g.blocked.Load() == 0 {
		return ""
	}
	if g.message != "" {
		return g.message
	}
	g.concurrencyMu.Lock()
	defer g.concurrencyMu.Unlock()
	return g.concurrencyMessage
}

// applyScopeBudgetFilter 把本次请求的 scope 预算闸门叠加到账号过滤链上。
// 未配 scope 限额或全部 scope 都有余额时原样返回。
func (h *Handler) applyScopeBudgetFilter(c *gin.Context, filter auth.AccountFilter) auth.AccountFilter {
	gate := scopeBudgetGateFromContext(c)
	if gate == nil {
		return filter
	}
	allowance := 0
	if passiveInternalRequestAuthorized(c) {
		allowance = 1
	}
	return gate.filterWithConcurrencyAllowance(filter, allowance, selectionTraceForRequest(c))
}

// scopeBudgetExhaustedMessage 返回本次请求因 scope 预算耗尽而剔除候选的说明,
// 供各 handler 在「无可用账号」分支把 503 换成语义准确的 429。
func scopeBudgetExhaustedMessage(c *gin.Context) string {
	return scopeBudgetGateFromContext(c).exhaustedMessage()
}

func scopeBudgetGateFromContext(c *gin.Context) *scopeBudgetGate {
	if c == nil {
		return nil
	}
	v, ok := c.Get(contextScopeBudgetGate)
	if !ok || v == nil {
		return nil
	}
	gate, _ := v.(*scopeBudgetGate)
	return gate
}

// evaluateAPIKeyScopeBudgets 计算该 Key 的 scope 预算状态。
// 返回 rejectMsg 非空表示存在 on_exhausted=reject 的 scope 已耗尽,调用方应直接 429;
// 返回的 gate 非空表示有 skip 类 scope 被剔除,需要挂到账号过滤链上。
func (h *Handler) evaluateAPIKeyScopeBudgets(ctx context.Context, row *database.APIKeyRow) (*scopeBudgetGate, string) {
	if h == nil || row == nil || row.ID <= 0 {
		return nil, ""
	}
	scopes := database.NormalizeAPIKeyScopeLimits(row.Limits.ScopeLimits)
	if len(scopes) == 0 {
		return nil, ""
	}
	h.apiKeyScopeUsageTracker().markTracked(row.ID)

	// 按需拉取窗口快照:只有真的被某条 scope 用到的窗口才查。
	usageByWindow := make(map[string]map[int64]database.APIKeyWindowUsage, len(database.APIKeyScopeWindows))
	for _, window := range database.APIKeyScopeWindows {
		needed := false
		for _, scope := range scopes {
			if scope.NeedsWindow(window.Label) {
				needed = true
				break
			}
		}
		if !needed {
			continue
		}
		usage, err := h.apiKeyScopeWindowUsage(ctx, row.ID, window)
		if err != nil {
			// 读不到用量时不阻断请求:与 Key 级限额一致,可用性优先。
			continue
		}
		usageByWindow[window.Label] = usage
	}
	// 只配了累计额度 / 并发上限（没有任何滑动窗口）的 scope 也要处理，不能在这里就退出。
	concurrencyScopes := scopesWithConcurrencyLimit(scopes)
	if len(usageByWindow) == 0 && !anyCumulativeQuota(scopes) && len(concurrencyScopes) == 0 {
		return nil, ""
	}

	groupsByAccount := h.accountGroupLookup(usageByWindow)
	counters := h.apiKeyScopeCounters(ctx, row.ID, scopes)

	var gate *scopeBudgetGate
	for _, scope := range scopes {
		hit, ok := h.checkScopeBudget(scope, usageByWindow, groupsByAccount)
		if !ok {
			// 累计额度（不随时间回落）单独判定：它读 api_key_scope_counters，
			// 与窗口聚合是两套数据源。
			hit, ok = scope.CheckCumulative(counters[database.APIKeyScopeCounterKey{
				ScopeType: scope.ResolveScopeType(),
				ScopeID:   scope.ScopeID,
			}])
		}
		if !ok {
			continue
		}
		message := hit.Describe(h.describeScope(scope))
		reject := scope.ResolveOnExhausted() == database.APIKeyScopeOnExhaustedReject
		// 每个请求对每条耗尽的 scope 只登记一次(这里是每请求执行一次的路径),
		// 因此统计就是"受影响请求数",而不是候选剔除次数。
		if apiKeyScopeSkipStats.record(scopeSkipKey{
			apiKeyID:  row.ID,
			scopeType: scope.ResolveScopeType(),
			scopeID:   scope.ScopeID,
		}, message, reject, time.Now()) {
			action := "skipping its accounts for this request"
			if reject {
				action = "rejecting the request"
			}
			log.Printf("[scope-budget] api_key=%d %s exhausted, %s (%s)", row.ID, h.describeScope(scope), action, message)
		}
		if reject {
			return nil, message
		}
		if gate == nil {
			gate = newScopeBudgetGate(row.ID, concurrencyScopes)
			gate.message = message
		}
		if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
			gate.blockedAccounts[scope.ScopeID] = struct{}{}
		} else {
			gate.blockedGroups[scope.ScopeID] = struct{}{}
		}
	}
	if gate == nil && len(concurrencyScopes) > 0 {
		gate = newScopeBudgetGate(row.ID, concurrencyScopes)
	}
	return gate, ""
}

func newScopeBudgetGate(apiKeyID int64, concurrencyScopes []database.APIKeyScopeLimit) *scopeBudgetGate {
	return &scopeBudgetGate{
		apiKeyID:          apiKeyID,
		blockedGroups:     make(map[int64]struct{}),
		blockedAccounts:   make(map[int64]struct{}),
		concurrencyScopes: concurrencyScopes,
	}
}

func scopesWithConcurrencyLimit(scopes []database.APIKeyScopeLimit) []database.APIKeyScopeLimit {
	out := make([]database.APIKeyScopeLimit, 0, len(scopes))
	for _, scope := range scopes {
		if scope.MaxConcurrency > 0 {
			out = append(out, scope)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// scopeLabelForMessage 是不依赖 store 的 scope 标签(并发提示在过滤热路径上生成,
// 不便再去解析分组名)。
func scopeLabelForMessage(scope database.APIKeyScopeLimit) string {
	if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
		return fmt.Sprintf("account #%d", scope.ScopeID)
	}
	return fmt.Sprintf("group #%d", scope.ScopeID)
}

func anyCumulativeQuota(scopes []database.APIKeyScopeLimit) bool {
	for _, scope := range scopes {
		if scope.HasCumulativeQuota() {
			return true
		}
	}
	return false
}

// checkScopeBudget 按窗口顺序判定一条 scope 是否耗尽。命中即返回,短窗口优先。
func (h *Handler) checkScopeBudget(
	scope database.APIKeyScopeLimit,
	usageByWindow map[string]map[int64]database.APIKeyWindowUsage,
	groupsByAccount map[int64][]int64,
) (database.APIKeyScopeExhaustion, bool) {
	isAccountScope := scope.ResolveScopeType() == database.APIKeyScopeTypeAccount
	for _, window := range database.APIKeyScopeWindows {
		if !scope.NeedsWindow(window.Label) {
			continue
		}
		perAccount, ok := usageByWindow[window.Label]
		if !ok {
			continue
		}
		var usage database.APIKeyWindowUsage
		if isAccountScope {
			usage = perAccount[scope.ScopeID]
		} else {
			for accountID, accountUsage := range perAccount {
				if !containsGroupID(groupsByAccount[accountID], scope.ScopeID) {
					continue
				}
				usage.Requests += accountUsage.Requests
				usage.Tokens += accountUsage.Tokens
				usage.UserBilled += accountUsage.UserBilled
			}
		}
		if hit, exhausted := scope.CheckWindow(window.Label, usage); exhausted {
			return hit, true
		}
	}
	return database.APIKeyScopeExhaustion{}, false
}

// accountGroupLookup 为快照里出现过的账号解析当前所属分组。已从账号池移除的账号
// 解析不到分组,其历史用量在分组维度上被忽略(账号维度仍然准确)。
func (h *Handler) accountGroupLookup(usageByWindow map[string]map[int64]database.APIKeyWindowUsage) map[int64][]int64 {
	if h == nil || h.store == nil {
		return nil
	}
	out := make(map[int64][]int64)
	for _, perAccount := range usageByWindow {
		for accountID := range perAccount {
			if _, ok := out[accountID]; ok {
				continue
			}
			account := h.store.FindByID(accountID)
			if account == nil {
				out[accountID] = nil
				continue
			}
			out[accountID] = account.GroupIDSnapshot()
		}
	}
	return out
}

// describeScope 生成 scope 的可读标签,用于错误文案。分组名解析失败时退回 ID。
func (h *Handler) describeScope(scope database.APIKeyScopeLimit) string {
	if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
		return fmt.Sprintf("account #%d", scope.ScopeID)
	}
	if h != nil && h.store != nil {
		if names := h.store.ResolveGroupNames([]int64{scope.ScopeID}); len(names) > 0 && names[0] != "" {
			return fmt.Sprintf("group %q", names[0])
		}
	}
	return fmt.Sprintf("group #%d", scope.ScopeID)
}

// apiKeyScopeCounters 读取累计额度计数器,带 5s 本地缓存。
//
// 刻意不叠加进程内增量:累计计数器和 api_keys.quota_used 一样在批量落库的同一事务里累加,
// 因此判定天然滞后一个 flush 周期(秒级),与既有 Key 级累计额度语义一致。
// 5s 缓存把「每请求一次查库」压下来,同时保证耗尽后很快生效。
func (h *Handler) apiKeyScopeCounters(ctx context.Context, apiKeyID int64, scopes []database.APIKeyScopeLimit) map[database.APIKeyScopeCounterKey]database.APIKeyScopeCounter {
	needed := false
	for _, scope := range scopes {
		if scope.HasCumulativeQuota() {
			needed = true
			break
		}
	}
	if !needed || h == nil || h.db == nil {
		return nil
	}
	tracker := h.apiKeyScopeUsageTracker()
	if counters, ok := tracker.cachedCounters(apiKeyID); ok {
		return counters
	}
	counters, err := h.db.ListAPIKeyScopeCounters(ctx, apiKeyID)
	if err != nil {
		// 读不到就当没用量:与窗口限额一致,可用性优先。
		return nil
	}
	tracker.storeCounters(apiKeyID, counters)
	return counters
}

// apiKeyScopeWindowUsage 返回某窗口内按账号拆分的用量:缓存快照(≤60s)+ 本地增量修正。
func (h *Handler) apiKeyScopeWindowUsage(ctx context.Context, apiKeyID int64, window database.APIKeyScopeWindow) (map[int64]database.APIKeyWindowUsage, error) {
	snapshot, ok := h.readAPIKeyScopeUsageCache(ctx, apiKeyID, window.Label)
	if !ok {
		accounts, err := h.db.GetAPIKeyAccountWindowUsage(ctx, apiKeyID, window.Window)
		if err != nil {
			return nil, err
		}
		snapshot = &apiKeyScopeUsageSnapshot{SnapshotAt: time.Now(), Accounts: accounts}
		h.writeAPIKeyScopeUsageCache(ctx, apiKeyID, window.Label, snapshot)
	}

	merged := make(map[int64]database.APIKeyWindowUsage, len(snapshot.Accounts)+4)
	for accountID, usage := range snapshot.Accounts {
		merged[accountID] = usage
	}
	addDelta := func(delta map[int64]database.APIKeyWindowUsage) {
		for accountID, usage := range delta {
			current := merged[accountID]
			current.Requests += usage.Requests
			current.Tokens += usage.Tokens
			current.UserBilled += usage.UserBilled
			merged[accountID] = current
		}
	}

	tracker := h.apiKeyScopeUsageTracker()
	if h.sharedScopeDeltaEnabled() {
		buckets, sharedReadAt := h.sharedScopeBuckets(ctx, apiKeyID)
		addDelta(sharedScopeDeltaAfterSnapshot(buckets, snapshot.SnapshotAt))
		addDelta(tracker.deltaWithSharedBuckets(apiKeyID, snapshot.SnapshotAt, sharedReadAt))
	} else {
		addDelta(tracker.deltaSince(apiKeyID, snapshot.SnapshotAt))
	}
	return merged, nil
}

func apiKeyScopeUsageCacheKey(apiKeyID int64, label string) string {
	return fmt.Sprintf("%d:%s", apiKeyID, label)
}

func (h *Handler) readAPIKeyScopeUsageCache(ctx context.Context, apiKeyID int64, label string) (*apiKeyScopeUsageSnapshot, bool) {
	if h == nil || h.cache == nil {
		return nil, false
	}
	raw, ok, err := h.cache.GetRuntime(ctx, apiKeyScopeUsageCacheNamespace, apiKeyScopeUsageCacheKey(apiKeyID, label))
	if err != nil || !ok || len(raw) == 0 {
		return nil, false
	}
	var snapshot apiKeyScopeUsageSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, false
	}
	if snapshot.SnapshotAt.IsZero() {
		return nil, false
	}
	if snapshot.Accounts == nil {
		snapshot.Accounts = map[int64]database.APIKeyWindowUsage{}
	}
	return &snapshot, true
}

func (h *Handler) writeAPIKeyScopeUsageCache(ctx context.Context, apiKeyID int64, label string, snapshot *apiKeyScopeUsageSnapshot) {
	if h == nil || h.cache == nil || snapshot == nil {
		return
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	_ = h.cache.SetRuntime(ctx, apiKeyScopeUsageCacheNamespace, apiKeyScopeUsageCacheKey(apiKeyID, label), raw, apiKeyScopeUsageCacheTTL)
}

func containsGroupID(groups []int64, target int64) bool {
	for _, id := range groups {
		if id == target {
			return true
		}
	}
	return false
}
