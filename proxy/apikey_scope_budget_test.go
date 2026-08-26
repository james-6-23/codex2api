package proxy

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newScopeBudgetTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

func TestScopeBudgetGateFilterSkipsBlockedGroupAndKeepsOthers(t *testing.T) {
	premium := &auth.Account{DBID: 1, GroupIDs: []int64{10}}
	cheap := &auth.Account{DBID: 2, GroupIDs: []int64{20}}
	blockedAccount := &auth.Account{DBID: 3, GroupIDs: []int64{20}}

	gate := &scopeBudgetGate{
		blockedGroups:   map[int64]struct{}{10: {}},
		blockedAccounts: map[int64]struct{}{3: {}},
		message:         "exhausted",
	}
	filter := gate.filter(nil)

	if filter(premium) {
		t.Fatal("account in an exhausted group must be filtered out")
	}
	if filter(blockedAccount) {
		t.Fatal("account with an exhausted account-scope budget must be filtered out")
	}
	if !filter(cheap) {
		t.Fatal("account outside every exhausted scope must stay schedulable")
	}
	if gate.exhaustedMessage() != "exhausted" {
		t.Fatalf("exhaustedMessage = %q, want the scope description", gate.exhaustedMessage())
	}
}

func TestScopeBudgetGateFilterPreservesInnerFilter(t *testing.T) {
	gate := &scopeBudgetGate{blockedGroups: map[int64]struct{}{10: {}}, message: "exhausted"}
	filter := gate.filter(func(account *auth.Account) bool { return account.DBID == 2 })

	if filter(&auth.Account{DBID: 1, GroupIDs: []int64{20}}) {
		t.Fatal("inner filter rejection must be honored")
	}
	if !filter(&auth.Account{DBID: 2, GroupIDs: []int64{20}}) {
		t.Fatal("inner filter acceptance must pass through")
	}
	// 只有内层过滤器拒绝时，不应把失败归因到 scope 预算。
	if gate.exhaustedMessage() != "" {
		t.Fatalf("exhaustedMessage = %q, want empty when nothing was blocked by budget", gate.exhaustedMessage())
	}
}

func TestNilScopeBudgetGateIsTransparent(t *testing.T) {
	var gate *scopeBudgetGate
	if gate.filter(nil) != nil {
		t.Fatal("nil gate must not wrap a nil filter")
	}
	if gate.exhaustedMessage() != "" {
		t.Fatal("nil gate must report no exhaustion")
	}
	if scopeBudgetExhaustedMessage(newScopeBudgetTestContext()) != "" {
		t.Fatal("context without a gate must report no exhaustion")
	}
	handler := &Handler{}
	if handler.applyScopeBudgetFilter(newScopeBudgetTestContext(), nil) != nil {
		t.Fatal("applyScopeBudgetFilter must be a no-op without a gate")
	}
}

func TestScopeUsageTrackerOnlyCountsTrackedKeysAfterSnapshot(t *testing.T) {
	tracker := newAPIKeyScopeUsageTracker()

	// 未标记跟踪的 Key 不记账，避免为所有 Key 保留事件。
	tracker.record(7, 1, 100, 0.5)
	if delta := tracker.deltaSince(7, time.Now().Add(-time.Minute)); len(delta) != 0 {
		t.Fatalf("untracked key delta = %+v, want empty", delta)
	}

	tracker.markTracked(7)
	before := time.Now()
	time.Sleep(2 * time.Millisecond)
	tracker.record(7, 1, 100, 0.5)
	tracker.record(7, 1, 50, 0.25)
	tracker.record(7, 2, 10, 0.1)

	delta := tracker.deltaSince(7, before)
	if len(delta) != 2 {
		t.Fatalf("delta covered %d accounts, want 2", len(delta))
	}
	if got := delta[1]; got.Requests != 2 || got.Tokens != 150 || got.UserBilled != 0.75 {
		t.Fatalf("account 1 delta = %+v, want 2 requests / 150 tokens / 0.75 cost", got)
	}

	// 快照时间之后没有新事件时不叠加，避免与 DB 聚合重复计数。
	if delta := tracker.deltaSince(7, time.Now()); len(delta) != 0 {
		t.Fatalf("delta after the latest event = %+v, want empty", delta)
	}
}

func TestScopeUsageTrackerDropsExpiredEvents(t *testing.T) {
	tracker := newAPIKeyScopeUsageTracker()
	tracker.markTracked(7)
	tracker.events[7] = []apiKeyScopeUsageEvent{
		{at: time.Now().Add(-2 * apiKeyScopeDeltaRetention), accountID: 1, tokens: 10, cost: 1},
	}
	if delta := tracker.deltaSince(7, time.Now().Add(-3*apiKeyScopeDeltaRetention)); len(delta) != 0 {
		t.Fatalf("expired events leaked into delta: %+v", delta)
	}
	if len(tracker.events[7]) != 0 {
		t.Fatalf("expired events were not pruned: %+v", tracker.events[7])
	}
}

func TestRecordAPIKeyScopeUsageSkipsClientCancels(t *testing.T) {
	handler := &Handler{}
	handler.apiKeyScopeUsageTracker().markTracked(7)

	handler.recordAPIKeyScopeUsage(&database.UsageLogInput{
		APIKeyID:     7,
		AccountID:    1,
		TotalTokens:  100,
		InputTokens:  50,
		OutputTokens: 50,
		Model:        "gpt-5.4",
		StatusCode:   499,
	})
	if delta := handler.scopeUsage.deltaSince(7, time.Now().Add(-time.Minute)); len(delta) != 0 {
		t.Fatalf("499 client cancel was counted: %+v", delta)
	}

	handler.recordAPIKeyScopeUsage(&database.UsageLogInput{
		APIKeyID:     7,
		AccountID:    1,
		TotalTokens:  100,
		InputTokens:  50,
		OutputTokens: 50,
		Model:        "gpt-5.4",
		StatusCode:   200,
	})
	delta := handler.scopeUsage.deltaSince(7, time.Now().Add(-time.Minute))
	if len(delta) != 1 || delta[1].Tokens != 100 {
		t.Fatalf("successful request delta = %+v, want 100 tokens on account 1", delta)
	}
	if delta[1].UserBilled <= 0 {
		t.Fatalf("billed cost = %v, want the same figure InsertUsageLog would persist", delta[1].UserBilled)
	}
}

func TestEvaluateAPIKeyScopeBudgetsNoScopesIsNoOp(t *testing.T) {
	handler := &Handler{}
	gate, rejectMsg := handler.evaluateAPIKeyScopeBudgets(context.Background(), &database.APIKeyRow{ID: 1})
	if gate != nil || rejectMsg != "" {
		t.Fatalf("gate = %v rejectMsg = %q, want no-op for a key without scope limits", gate, rejectMsg)
	}
}

// scopeBudgetTestEnv 搭一个真实 sqlite + 账号池的最小环境，用来端到端验证
// 「按分组折算用量 → 剔除候选 / 直接拒绝」这条链路。
func scopeBudgetTestEnv(t *testing.T) (*Handler, *database.DB, int64) {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	keyID, err := db.InsertAPIKey(context.Background(), "scoped", "sk-scope-gate-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "premium", GroupIDs: []int64{10}})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "cheap", GroupIDs: []int64{20}})

	return &Handler{store: store, db: db}, db, keyID
}

func insertScopeUsageLog(t *testing.T, db *database.DB, keyID, accountID int64, tokens int) {
	t.Helper()
	if err := db.InsertUsageLog(context.Background(), &database.UsageLogInput{
		APIKeyID:     keyID,
		AccountID:    accountID,
		Endpoint:     "/v1/responses",
		Model:        "gpt-5.4",
		StatusCode:   200,
		TotalTokens:  tokens,
		InputTokens:  tokens / 2,
		OutputTokens: tokens - tokens/2,
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	db.FlushUsageLogs()
}

func TestEvaluateAPIKeyScopeBudgetsBlocksExhaustedGroupOnly(t *testing.T) {
	handler, db, keyID := scopeBudgetTestEnv(t)
	// 优质组账号 1 已经消耗了 4000 token，劣质组账号 2 没有用量。
	insertScopeUsageLog(t, db, keyID, 1, 4000)

	row := &database.APIKeyRow{ID: keyID, Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{
		{ScopeType: database.APIKeyScopeTypeGroup, ScopeID: 10, Token1d: 1000},
		{ScopeType: database.APIKeyScopeTypeGroup, ScopeID: 20, Token1d: 1000},
	}}}

	gate, rejectMsg := handler.evaluateAPIKeyScopeBudgets(context.Background(), row)
	if rejectMsg != "" {
		t.Fatalf("rejectMsg = %q, want empty for skip-mode scopes", rejectMsg)
	}
	if gate == nil {
		t.Fatal("gate = nil, want the exhausted group blocked")
	}
	if _, blocked := gate.blockedGroups[10]; !blocked {
		t.Fatalf("blockedGroups = %+v, want group 10", gate.blockedGroups)
	}
	if _, blocked := gate.blockedGroups[20]; blocked {
		t.Fatalf("blockedGroups = %+v, want group 20 untouched", gate.blockedGroups)
	}

	filter := gate.filter(nil)
	if filter(handler.store.FindByID(1)) {
		t.Fatal("premium account must be filtered out after its group budget is exhausted")
	}
	if !filter(handler.store.FindByID(2)) {
		t.Fatal("cheap account must remain schedulable")
	}
}

func TestEvaluateAPIKeyScopeBudgetsRejectModeShortCircuits(t *testing.T) {
	handler, db, keyID := scopeBudgetTestEnv(t)
	insertScopeUsageLog(t, db, keyID, 1, 4000)

	row := &database.APIKeyRow{ID: keyID, Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{
		{ScopeType: database.APIKeyScopeTypeGroup, ScopeID: 10, Token1d: 1000, OnExhausted: database.APIKeyScopeOnExhaustedReject},
	}}}

	gate, rejectMsg := handler.evaluateAPIKeyScopeBudgets(context.Background(), row)
	if gate != nil {
		t.Fatalf("gate = %+v, want nil for reject mode", gate)
	}
	if rejectMsg == "" || !strings.Contains(rejectMsg, "scope budget exhausted") {
		t.Fatalf("rejectMsg = %q, want a scope budget exhaustion message", rejectMsg)
	}
}

func TestEvaluateAPIKeyScopeBudgetsUnderLimitKeepsEveryone(t *testing.T) {
	handler, db, keyID := scopeBudgetTestEnv(t)
	insertScopeUsageLog(t, db, keyID, 1, 500)

	row := &database.APIKeyRow{ID: keyID, Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{
		{ScopeType: database.APIKeyScopeTypeGroup, ScopeID: 10, Token1d: 1000},
	}}}

	gate, rejectMsg := handler.evaluateAPIKeyScopeBudgets(context.Background(), row)
	if gate != nil || rejectMsg != "" {
		t.Fatalf("gate = %+v rejectMsg = %q, want no restriction under the limit", gate, rejectMsg)
	}
}

func TestEvaluateAPIKeyScopeBudgetsAccountScopeIgnoresSiblings(t *testing.T) {
	handler, db, keyID := scopeBudgetTestEnv(t)
	// 同组的另一个账号把量用掉，不应影响针对账号 1 的账号维度预算。
	insertScopeUsageLog(t, db, keyID, 2, 4000)

	row := &database.APIKeyRow{ID: keyID, Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{
		{ScopeType: database.APIKeyScopeTypeAccount, ScopeID: 1, Token1d: 1000},
	}}}

	gate, rejectMsg := handler.evaluateAPIKeyScopeBudgets(context.Background(), row)
	if gate != nil || rejectMsg != "" {
		t.Fatalf("gate = %+v rejectMsg = %q, want account 1 untouched", gate, rejectMsg)
	}
}

func TestScopeBudgetGateDoesNotClaimBlocksForInnerRejections(t *testing.T) {
	gate := &scopeBudgetGate{blockedGroups: map[int64]struct{}{10: {}}, message: "exhausted"}
	// 内层过滤器（例如模型不匹配）先拒绝：这次失败不该归因到 scope 预算。
	filter := gate.filter(func(*auth.Account) bool { return false })
	if filter(&auth.Account{DBID: 1, GroupIDs: []int64{10}}) {
		t.Fatal("inner rejection must win")
	}
	if gate.exhaustedMessage() != "" {
		t.Fatalf("exhaustedMessage = %q, want empty when the account was unusable anyway", gate.exhaustedMessage())
	}

	// 本来选得上、只因预算被拒时才算 blocked。
	filter = gate.filter(func(*auth.Account) bool { return true })
	if filter(&auth.Account{DBID: 1, GroupIDs: []int64{10}}) {
		t.Fatal("exhausted group must be filtered out")
	}
	if gate.exhaustedMessage() != "exhausted" {
		t.Fatalf("exhaustedMessage = %q, want the scope description", gate.exhaustedMessage())
	}
}

func TestSharedScopeDeltaAfterSnapshotSkipsSnapshotMinute(t *testing.T) {
	base := time.Unix(1_800_000_000, 0) // 整分钟起点
	snapshotAt := base.Add(30 * time.Second)
	snapshotMinute := snapshotAt.Unix() / 60
	buckets := map[int64]map[int64]database.APIKeyWindowUsage{
		// 跨越快照时刻的桶：整桶相加会把快照已包含的用量重复计一次，必须跳过。
		snapshotMinute: {1: {Requests: 5, Tokens: 500, UserBilled: 5}},
		// 快照之后的桶：全部算增量。
		snapshotMinute + 1: {1: {Requests: 2, Tokens: 200, UserBilled: 2}, 2: {Requests: 1, Tokens: 10, UserBilled: 1}},
	}

	delta := sharedScopeDeltaAfterSnapshot(buckets, snapshotAt)
	if len(delta) != 2 {
		t.Fatalf("delta = %+v, want two accounts", delta)
	}
	if got := delta[1]; got.Requests != 2 || got.Tokens != 200 || got.UserBilled != 2 {
		t.Fatalf("account 1 delta = %+v, want only the post-snapshot bucket", got)
	}
	if delta := sharedScopeDeltaAfterSnapshot(nil, snapshotAt); delta != nil {
		t.Fatalf("empty buckets delta = %+v, want nil", delta)
	}
}

func TestDeltaWithSharedBucketsAvoidsDoubleCounting(t *testing.T) {
	tracker := newAPIKeyScopeUsageTracker()
	tracker.markTracked(7)

	// Anchor the fixture away from minute boundaries. Deriving both timestamps
	// directly from time.Now made this test flaky whenever snapshotAt landed at
	// xx:xx:59 and snapshotAt+1s crossed into the next minute.
	baseMinute := time.Now().Add(-3 * time.Minute).Truncate(time.Minute)
	snapshotAt := baseMinute.Add(30 * time.Second)
	sharedReadAt := baseMinute.Add(90 * time.Second)
	snapshotMinute := snapshotAt.Unix() / 60

	tracker.events[7] = []apiKeyScopeUsageEvent{
		// 快照之前：DB 快照已包含。
		{at: snapshotAt.Add(-time.Second), accountID: 1, tokens: 1000, cost: 10},
		// 快照那一分钟内、快照之后：共享桶跳过了这一分钟，只能靠本地补。
		{at: snapshotAt.Add(time.Second), accountID: 1, tokens: 7, cost: 0.07},
		// 快照之后的分钟、且早于共享桶读取时刻：已在共享桶里，不能再算。
		{at: sharedReadAt.Add(-20 * time.Second), accountID: 1, tokens: 500, cost: 5},
		// 共享桶读取之后：还没被读到，要补。
		{at: sharedReadAt.Add(time.Second), accountID: 1, tokens: 3, cost: 0.03},
	}
	// 构造校验前提：第三条事件确实落在快照分钟之后。
	if tracker.events[7][2].at.Unix()/60 <= snapshotMinute {
		t.Skip("测试时刻落在分钟边界，跳过")
	}

	delta := tracker.deltaWithSharedBuckets(7, snapshotAt, sharedReadAt)
	if got := delta[1]; got.Requests != 2 || got.Tokens != 10 {
		t.Fatalf("delta = %+v, want only the two events the shared buckets cannot cover", got)
	}
}

func TestScopeConcurrencyFilterBlocksWhenFullAndReleasesAfterwards(t *testing.T) {
	scope := database.APIKeyScopeLimit{ScopeType: database.APIKeyScopeTypeGroup, ScopeID: 10, MaxConcurrency: 1}
	gate := newScopeBudgetGate(4242, []database.APIKeyScopeLimit{scope})
	premium := &auth.Account{DBID: 1, GroupIDs: []int64{10}}
	cheap := &auth.Account{DBID: 2, GroupIDs: []int64{20}}
	filter := gate.filter(nil)

	// 没占位时放行。
	if !filter(premium) {
		t.Fatal("account must be schedulable while the scope has a free slot")
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(contextScopeBudgetGate, gate)
	handler := &Handler{}
	handler.AcquireAPIKeyScopeConcurrency(c, premium)
	if got := APIKeyScopeInflight(4242, database.APIKeyScopeTypeGroup, 10); got != 1 {
		t.Fatalf("inflight = %d, want 1", got)
	}

	// 位满 → 该 scope 的账号被剔除，其它分组不受影响。
	if filter(premium) {
		t.Fatal("account must be filtered out once the scope concurrency slot is taken")
	}
	if !filter(cheap) {
		t.Fatal("accounts outside the scope must stay schedulable")
	}
	if msg := gate.exhaustedMessage(); msg == "" || !strings.Contains(msg, "concurrency") {
		t.Fatalf("exhaustedMessage = %q, want a concurrency explanation", msg)
	}

	// 换号重试时上一轮的位要先释放，否则一个请求会把自己挤死。
	handler.AcquireAPIKeyScopeConcurrency(c, cheap)
	if got := APIKeyScopeInflight(4242, database.APIKeyScopeTypeGroup, 10); got != 0 {
		t.Fatalf("inflight after switching accounts = %d, want 0", got)
	}

	handler.ReleaseAPIKeyScopeConcurrency(c)
	if got := APIKeyScopeInflight(4242, database.APIKeyScopeTypeGroup, 10); got != 0 {
		t.Fatalf("inflight after release = %d, want 0", got)
	}
}

func TestVerifiedPassiveGuardianGetsExactlyOneProtectedScopeSlot(t *testing.T) {
	const apiKeyID = int64(4343)
	scope := database.APIKeyScopeLimit{ScopeType: database.APIKeyScopeTypeGroup, ScopeID: 10, MaxConcurrency: 1}
	account := &auth.Account{DBID: 1, GroupIDs: []int64{10}}
	handler := &Handler{}

	parentGate := newScopeBudgetGate(apiKeyID, []database.APIKeyScopeLimit{scope})
	parent, _ := gin.CreateTestContext(httptest.NewRecorder())
	parent.Set(contextScopeBudgetGate, parentGate)
	handler.AcquireAPIKeyScopeConcurrency(parent, account)
	defer handler.ReleaseAPIKeyScopeConcurrency(parent)
	if got := APIKeyScopeInflight(apiKeyID, database.APIKeyScopeTypeGroup, 10); got != 1 {
		t.Fatalf("parent inflight = %d, want 1", got)
	}

	guardianGate := newScopeBudgetGate(apiKeyID, []database.APIKeyScopeLimit{scope})
	guardian, _ := gin.CreateTestContext(httptest.NewRecorder())
	guardian.Set(contextScopeBudgetGate, guardianGate)
	setPassiveInternalAuthorization(guardian, newAPIPassiveFeatureGuardianApproval, true)
	filter := handler.applyScopeBudgetFilter(guardian, nil)
	if filter != nil && !filter(account) {
		t.Fatal("verified Guardian was blocked by its parent's occupied scope slot")
	}
	handler.AcquireAPIKeyScopeConcurrency(guardian, account)
	defer handler.ReleaseAPIKeyScopeConcurrency(guardian)
	if got := APIKeyScopeInflight(apiKeyID, database.APIKeyScopeTypeGroup, 10); got != 2 {
		t.Fatalf("Guardian protected scope lease inflight=%d, want 2", got)
	}

	secondGate := newScopeBudgetGate(apiKeyID, []database.APIKeyScopeLimit{scope})
	second, _ := gin.CreateTestContext(httptest.NewRecorder())
	second.Set(contextScopeBudgetGate, secondGate)
	setPassiveInternalAuthorization(second, newAPIPassiveFeatureGuardianApproval, true)
	secondFilter := handler.applyScopeBudgetFilter(second, nil)
	if secondFilter == nil || secondFilter(account) {
		t.Fatal("second Guardian exceeded the single protected scope slot")
	}
}

func TestScopeConcurrencyLeaseSelfHeals(t *testing.T) {
	key := scopeSkipKey{apiKeyID: 99, scopeType: database.APIKeyScopeTypeAccount, scopeID: 5}
	lease := apiKeyScopeConcurrency.acquire(key)
	if got := apiKeyScopeConcurrency.count(key); got != 1 {
		t.Fatalf("inflight = %d, want 1", got)
	}
	// 模拟某条路径漏了释放：位过期后必须自动回收，不能永久卡死这条 scope。
	apiKeyScopeConcurrency.mu.Lock()
	apiKeyScopeConcurrency.inflight[key][lease.id] = time.Now().Add(-time.Second)
	apiKeyScopeConcurrency.mu.Unlock()
	if got := apiKeyScopeConcurrency.count(key); got != 0 {
		t.Fatalf("inflight after expiry = %d, want 0", got)
	}
}

func TestScopeGateExistsForConcurrencyOnlyConfig(t *testing.T) {
	handler, _, keyID := scopeBudgetTestEnv(t)
	row := &database.APIKeyRow{ID: keyID, Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{
		{ScopeType: database.APIKeyScopeTypeGroup, ScopeID: 10, MaxConcurrency: 2},
	}}}
	gate, rejectMsg := handler.evaluateAPIKeyScopeBudgets(context.Background(), row)
	if rejectMsg != "" {
		t.Fatalf("rejectMsg = %q, want empty", rejectMsg)
	}
	if gate == nil {
		t.Fatal("gate = nil, want a gate so the filter can enforce the concurrency slot")
	}
	if len(gate.concurrencyScopes) != 1 {
		t.Fatalf("concurrencyScopes = %+v, want the configured scope", gate.concurrencyScopes)
	}
}

func TestCumulativeQuotaExhaustionBlocksScope(t *testing.T) {
	handler, db, keyID := scopeBudgetTestEnv(t)
	limits := database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{
		{ScopeType: database.APIKeyScopeTypeAccount, ScopeID: 1, QuotaRequests: 2},
	}}
	// 累计计数器只为「持久化的 limits 里配了累计额度」的 Key 记账，所以配置必须落库。
	if err := db.UpdateAPIKey(context.Background(), keyID, database.APIKeyUpdate{Limits: limits, LimitsSet: true}); err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}
	db.InvalidateScopeQuotaKeyCache()
	row := &database.APIKeyRow{ID: keyID, Limits: limits}

	// 未达累计上限：不拦。
	if gate, _ := handler.evaluateAPIKeyScopeBudgets(context.Background(), row); gate != nil {
		t.Fatalf("gate = %+v, want nil below the cumulative quota", gate)
	}

	for i := 0; i < 2; i++ {
		insertScopeUsageLog(t, db, keyID, 1, 10)
	}
	// 计数器缓存 5s，测试里直接清掉以立刻观察新状态。
	handler.scopeUsage.counters = map[int64]*scopeCounterCache{}

	gate, _ := handler.evaluateAPIKeyScopeBudgets(context.Background(), row)
	if gate == nil {
		t.Fatal("gate = nil, want the account blocked once the cumulative quota is used up")
	}
	if _, blocked := gate.blockedAccounts[1]; !blocked {
		t.Fatalf("blockedAccounts = %+v, want account 1", gate.blockedAccounts)
	}
	if msg := gate.message; !strings.Contains(msg, "in total") {
		t.Fatalf("message = %q, want it to say the quota needs a manual reset", msg)
	}
}
