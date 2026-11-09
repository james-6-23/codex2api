package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// antigravityQuotaSummaryBody 生成一份最小可用的 groups summary 响应：
// weekly 已知零 + 未来 resetTime（V2 presence 齐备，可参与硬门禁）。
func antigravityQuotaSummaryBody(resetAt time.Time) string {
	return fmt.Sprintf(`{"groups":[{"displayName":"Weekly","buckets":[{"window":"weekly","remainingFraction":0,"resetTime":%q}]}]}`,
		resetAt.UTC().Format(time.RFC3339))
}

// antigravityQuotaFactsForTest 把落库的 antigravity_quota 文本解析成可比较的文档，
// 并剔除预约/提交协议自行维护的时间与序号簿记（current_attempt_seq / last_attempt_at
// / last_success_at）：预约事务本身必然推进它们，「不被清空」断言只针对配额事实本体。
func antigravityQuotaFactsForTest(t *testing.T, raw string) map[string]any {
	t.Helper()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode persisted antigravity_quota %q: %v", raw, err)
	}
	if meta, ok := doc["source_metadata"].(map[string]any); ok {
		if groups, ok := meta["groups"].(map[string]any); ok {
			delete(groups, "current_attempt_seq")
			delete(groups, "last_attempt_at")
			delete(groups, "last_success_at")
		}
	}
	return doc
}

// TestRefreshAntigravityQuotaSummaryPersistsAndPublishes 验证单账号配额刷新成功完成预约、
// 远程获取、合并与提交，并正确推进运行时投影而不触碰 OAuth 令牌。
func TestRefreshAntigravityQuotaSummaryPersistsAndPublishes(t *testing.T) {
	var summaryCalls atomic.Int32
	var tokenCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/summary":
			summaryCalls.Add(1)
			_, _ = io.WriteString(w, antigravityQuotaSummaryBody(time.Now().UTC().Add(2*time.Hour)))
		case "/token":
			// 只读 summary 绝不允许触发换票（轮换 RT 属全量同步职责）。
			tokenCalls.Add(1)
			http.Error(w, `{"error":"token endpoint must not be reached"}`, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	store, db, account, accountID := newAntigravityRefreshTestAccount(t, nil)
	ctx := context.Background()
	epochBefore, err := db.GetAntigravityQuotaEpoch(ctx, accountID)
	if err != nil {
		t.Fatalf("read quota epoch: %v", err)
	}

	if err := store.RefreshAntigravityQuotaSummary(ctx, account); err != nil {
		t.Fatalf("RefreshAntigravityQuotaSummary: %v", err)
	}
	if calls := summaryCalls.Load(); calls != 1 {
		t.Fatalf("expected exactly one summary request, got %d", calls)
	}
	if calls := tokenCalls.Load(); calls != 0 {
		t.Fatalf("summary refresh must never rotate tokens, token endpoint calls=%d", calls)
	}

	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row: %v", err)
	}
	raw := row.GetCredential("antigravity_quota")
	if !strings.Contains(raw, `"schema_version":2`) {
		t.Fatalf("persisted snapshot must be V2, got %s", raw)
	}
	if !strings.Contains(raw, `"source_metadata"`) || !strings.Contains(raw, `"groups"`) {
		t.Fatalf("persisted snapshot must carry groups source metadata, got %s", raw)
	}
	if got := row.GetCredential("access_token"); got != "access-old" {
		t.Fatalf("summary refresh must not touch the access token, got %q", got)
	}

	epochAfter, err := db.GetAntigravityQuotaEpoch(ctx, accountID)
	if err != nil {
		t.Fatalf("read quota epoch after refresh: %v", err)
	}
	if epochAfter != epochBefore {
		t.Fatalf("summary refresh must not advance the quota epoch, before=%d after=%d", epochBefore, epochAfter)
	}

	account.mu.RLock()
	groups := len(account.AntigravityQuota.Groups)
	account.mu.RUnlock()
	if groups == 0 {
		t.Fatalf("runtime projection must publish quota groups, snapshot=%+v", account.AntigravityQuota)
	}
}

// TestRefreshAntigravityQuotaSummaryEmptyResponseKeepsPrevious 验证空配额响应返回 Incomplete 错误，
// 且原有持久化配额事实保持不变（不推进成功，不洗新时标）。
func TestRefreshAntigravityQuotaSummaryEmptyResponseKeepsPrevious(t *testing.T) {
	var body atomic.Value
	body.Store(antigravityQuotaSummaryBody(time.Now().UTC().Add(2 * time.Hour)))
	var summaryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/summary" {
			http.NotFound(w, r)
			return
		}
		summaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	store, db, account, accountID := newAntigravityRefreshTestAccount(t, nil)
	ctx := context.Background()
	if err := store.RefreshAntigravityQuotaSummary(ctx, account); err != nil {
		t.Fatalf("first RefreshAntigravityQuotaSummary: %v", err)
	}

	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row: %v", err)
	}
	before := antigravityQuotaFactsForTest(t, row.GetCredential("antigravity_quota"))
	if len(before) == 0 {
		t.Fatal("expected persisted quota facts after the first successful refresh")
	}

	body.Store(`{"groups":[]}`)
	err = store.RefreshAntigravityQuotaSummary(ctx, account)
	if !errors.Is(err, ErrAntigravityQuotaSummaryIncomplete) {
		t.Fatalf("empty summary must return ErrAntigravityQuotaSummaryIncomplete, got %v", err)
	}
	if calls := summaryCalls.Load(); calls != 2 {
		t.Fatalf("expected two summary requests, got %d", calls)
	}

	row, err = db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row after empty summary: %v", err)
	}
	after := antigravityQuotaFactsForTest(t, row.GetCredential("antigravity_quota"))
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("empty summary must not rewrite persisted quota facts:\nbefore=%v\nafter =%v", before, after)
	}
	if raw := row.GetCredential("antigravity_quota"); raw == "" {
		t.Fatal("empty summary must not clear the persisted snapshot")
	}
}

// TestRefreshAntigravityQuotaSummarySkipsDisabledAccount 验证禁用账号在入口处直接被拦截，不发生任何远程网络请求。
func TestRefreshAntigravityQuotaSummarySkipsDisabledAccount(t *testing.T) {
	var summaryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		summaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, antigravityQuotaSummaryBody(time.Now().UTC().Add(2*time.Hour)))
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	store, _, account, _ := newAntigravityRefreshTestAccount(t, nil)
	atomic.StoreInt32(&account.Disabled, 1)
	defer atomic.StoreInt32(&account.Disabled, 0)

	err := store.RefreshAntigravityQuotaSummary(context.Background(), account)
	if err == nil {
		t.Fatal("disabled account must not be refreshed")
	}
	if calls := summaryCalls.Load(); calls != 0 {
		t.Fatalf("disabled account must not reach the network, summary calls=%d", calls)
	}
}

// quotaSourceMetaFromPersisted 解析落库 antigravity_quota 里 groups 来源的 applied_seq
// 与 observed_at，用于直接断言 durable 行的协议簿记。
func quotaSourceMetaFromPersisted(t *testing.T, raw string) (int64, time.Time) {
	t.Helper()
	var doc struct {
		SourceMetadata struct {
			Groups struct {
				AppliedSeq int64     `json:"applied_seq"`
				ObservedAt time.Time `json:"observed_at"`
			} `json:"groups"`
		} `json:"source_metadata"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &doc); err != nil {
		t.Fatalf("decode persisted antigravity_quota %q: %v", raw, err)
	}
	return doc.SourceMetadata.Groups.AppliedSeq, doc.SourceMetadata.Groups.ObservedAt
}

// TestRefreshAntigravityQuotaSummaryObservedAtIsReservationTime 断言观测时标取自预约
// 事务（Spec §3.1）：慢响应（这里 1.2s）绝不把它一起拖后，否则 TTL/reset 基线与覆盖窗
// 判定都会随网络延迟漂移。
func TestRefreshAntigravityQuotaSummaryObservedAtIsReservationTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/summary" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(1200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, antigravityQuotaSummaryBody(time.Now().UTC().Add(2*time.Hour)))
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	store, db, account, accountID := newAntigravityRefreshTestAccount(t, nil)
	ctx := context.Background()

	started := time.Now().UTC()
	if err := store.RefreshAntigravityQuotaSummary(ctx, account); err != nil {
		t.Fatalf("RefreshAntigravityQuotaSummary: %v", err)
	}
	completed := time.Now().UTC()
	if elapsed := completed.Sub(started); elapsed < time.Second {
		t.Fatalf("the slow-response fixture did not take effect (elapsed=%v); this guard would be vacuous", elapsed)
	}

	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row: %v", err)
	}
	_, observedAt := quotaSourceMetaFromPersisted(t, row.GetCredential("antigravity_quota"))
	if observedAt.IsZero() {
		t.Fatal("persisted groups metadata must carry observed_at")
	}
	if observedAt.After(completed) {
		t.Fatalf("observed_at %v is later than the response completion %v", observedAt, completed)
	}
	if lag := completed.Sub(observedAt); lag < time.Second {
		t.Fatalf("observed_at must be the reservation instant, not the response instant: observed_at=%v sits only %v before completion %v",
			observedAt, lag, completed)
	}
	if early := started.Sub(observedAt); early > time.Second {
		t.Fatalf("observed_at %v predates the refresh call %v by %v", observedAt, started, early)
	}
}

// TestRefreshAntigravityQuotaSummaryRejectedCommitReturnsNil 断言 Latest-Success-Wins
// 拒绝（applied=false）是正常结局而不是失败：本方观测被更晚预约的观测覆盖时返回 nil，
// 调用方据此绝不触发退避。
func TestRefreshAntigravityQuotaSummaryRejectedCommitReturnsNil(t *testing.T) {
	store, db, account, accountID := newAntigravityRefreshTestAccount(t, nil)
	ctx := context.Background()

	var hijacked atomic.Bool
	var hijackSeq atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/summary" {
			http.NotFound(w, r)
			return
		}
		// 在 refresh 仍停留于网络阶段时，抢先预约并提交一枚更晚序号的观测：refresh
		// 自己随后拿着更小的序号提交，必然被 applied_seq 栅栏拒绝。
		if !hijacked.Swap(true) {
			row, err := db.GetAccountByID(context.Background(), accountID)
			if err != nil {
				t.Errorf("reload account row inside the summary window: %v", err)
				http.Error(w, "fixture reload failed", http.StatusInternalServerError)
				return
			}
			family := row.GetCredential("credential_family_id")
			seq, epoch, err := db.ReserveAntigravityQuotaSource(context.Background(), accountID, "groups", row.CredentialGeneration, family)
			if err != nil {
				t.Errorf("fixture reservation: %v", err)
				http.Error(w, "fixture reservation failed", http.StatusInternalServerError)
				return
			}
			applied, err := db.CommitAntigravityQuotaSource(context.Background(), accountID, "groups", seq, epoch, row.CredentialGeneration, family, `{"schema_version":2}`)
			if err != nil || !applied {
				t.Errorf("fixture commit applied=%v err=%v", applied, err)
				http.Error(w, "fixture commit failed", http.StatusInternalServerError)
				return
			}
			hijackSeq.Store(seq)
		}
		// 让被抢提交真正先于本方提交落库。
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, antigravityQuotaSummaryBody(time.Now().UTC().Add(2*time.Hour)))
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	if err := store.RefreshAntigravityQuotaSummary(ctx, account); err != nil {
		t.Fatalf("a Latest-Success-Wins rejection must not surface as a failure, got %v", err)
	}
	if seq := hijackSeq.Load(); seq == 0 {
		t.Fatal("the fixture never hijacked the commit window")
	}

	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row: %v", err)
	}
	appliedSeq, _ := quotaSourceMetaFromPersisted(t, row.GetCredential("antigravity_quota"))
	if appliedSeq != hijackSeq.Load() {
		t.Fatalf("the rejected commit must not move applied_seq: durable=%d winner=%d", appliedSeq, hijackSeq.Load())
	}
}

// antigravityQuotaAuthFixture 是一个按 Authorization 判权的 Antigravity fixture：
// 只有 newToken（/token 现换出来的那枚）才能通过 /userinfo、/load 与 /quota，其余一律
// 401——与线上「DB 里存量 AT 已失效、summary 恒定 401」的真实形态一致。
//
// 计数口径：换票链路（refreshAntigravityAccountDurable → client.Sync）成功后自己也会
// 读一次 summary，那是另一条通道（全量同步）的观测，混进 summaryCalls 会让「quota
// summary 刷新到底观测了几次」失去意义。Sync 的观测必然发生在 /token 与 /credits 之间
// （quota→summary→credits 是 Sync 的固定顺序），因此 fixture 用 inCredentialExchange
// 标出这段换票窗口：窗口内的 /summary 照常服务（保证 Sync 成功），但不计入 summaryCalls。
type antigravityQuotaAuthFixture struct {
	server *httptest.Server
	// newToken 是 /token 换出来的那枚；handle 以它判定请求是否已带上新凭证。
	newToken string
	// summaryAlwaysUnauthorized 让 /summary 无视 token 永远 401，用来验证不做循环重试。
	summaryAlwaysUnauthorized bool
	// inCredentialExchange 标记换票窗口（/token 起、/credits 止）。
	inCredentialExchange atomic.Bool
	tokenCalls           atomic.Int32
	summaryCalls         atomic.Int32
	// staleSummaryCalls 统计窗口外拿存量（已失效）令牌访问 summary 的次数。
	staleSummaryCalls atomic.Int32
	lastSummaryAuth   atomic.Value
}

// newAntigravityQuotaAuthFixture 构造用于鉴权刷新与重试测试的仿真上游服务器。
func newAntigravityQuotaAuthFixture(t *testing.T, newToken string, summaryAlwaysUnauthorized bool) *antigravityQuotaAuthFixture {
	t.Helper()
	fixture := &antigravityQuotaAuthFixture{newToken: newToken, summaryAlwaysUnauthorized: summaryAlwaysUnauthorized}
	fixture.lastSummaryAuth.Store("")
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handle))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// lastSummaryAuthorization 获取上一次配额请求收到的 Authorization 头部信息。
func (f *antigravityQuotaAuthFixture) lastSummaryAuthorization() string {
	value, _ := f.lastSummaryAuth.Load().(string)
	return value
}

// handle 模拟上游端点鉴权分支逻辑。
func (f *antigravityQuotaAuthFixture) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	authorized := r.Header.Get("Authorization") == "Bearer "+f.newToken
	reject := func() { http.Error(w, `{"error":"invalid authentication credentials"}`, http.StatusUnauthorized) }
	switch r.URL.Path {
	case "/token":
		f.tokenCalls.Add(1)
		f.inCredentialExchange.Store(true)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"access_token":%q,"refresh_token":"refresh-new","expires_in":3600}`, f.newToken))
	case "/userinfo":
		if !authorized {
			reject()
			return
		}
		_, _ = io.WriteString(w, `{"id":"subject-1","email":"user@example.com","verified_email":true}`)
	case "/load":
		if !authorized {
			reject()
			return
		}
		_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-old","paidTier":{"name":"Pro"}}`)
	case "/quota":
		if !authorized {
			reject()
			return
		}
		_, _ = io.WriteString(w, `{"models":{"gemini-test":{"quotaInfo":{"remainingFraction":0.9}}}}`)
	case "/credits":
		// credits 是 Sync 的最后一步：换票窗口到此结束，之后的 summary 观测都属于
		// quota summary 刷新自己。
		f.inCredentialExchange.Store(false)
		_, _ = io.WriteString(w, `{"paidTier":{"availableCredits":[]}}`)
	case "/summary":
		inExchange := f.inCredentialExchange.Load()
		if !inExchange {
			f.summaryCalls.Add(1)
			f.lastSummaryAuth.Store(r.Header.Get("Authorization"))
			if !authorized {
				f.staleSummaryCalls.Add(1)
			}
		}
		if f.summaryAlwaysUnauthorized || !authorized {
			reject()
			return
		}
		_, _ = io.WriteString(w, antigravityQuotaSummaryBody(time.Now().UTC().Add(2*time.Hour)))
	default:
		http.NotFound(w, r)
	}
}

// TestRefreshAntigravityQuotaSummaryRefreshesExpiredTokenBeforeFetch 断言存量令牌已
// 过期时，summary 刷新必须先走一次安全换票再观测（Spec §5.1「必要认证恢复例外」）：
// 线上 4 账号全部 401 invalid authentication credentials 正是因为只读 summary、
// 从不换票，自动刷新永远观测不到数据。
func TestRefreshAntigravityQuotaSummaryRefreshesExpiredTokenBeforeFetch(t *testing.T) {
	const newToken = "access-fresh-before-fetch"
	fixture := newAntigravityQuotaAuthFixture(t, newToken, false)
	useAntigravityTestEndpoints(t, fixture.server.URL)

	store, db, account, accountID := newAntigravityRefreshTestAccount(t, map[string]any{
		"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	ctx := context.Background()
	if !account.NeedsRefresh() {
		t.Fatal("fixture account must be past its access-token expiry")
	}
	rowBefore, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row before refresh: %v", err)
	}

	if err := store.RefreshAntigravityQuotaSummary(ctx, account); err != nil {
		t.Fatalf("RefreshAntigravityQuotaSummary: %v", err)
	}
	if calls := fixture.tokenCalls.Load(); calls != 1 {
		t.Fatalf("an expired credential must be recovered by exactly one token exchange, token calls=%d", calls)
	}
	if calls := fixture.staleSummaryCalls.Load(); calls != 0 {
		t.Fatalf("an expired credential must never be spent on a quota observation, stale summary calls=%d", calls)
	}
	if calls := fixture.summaryCalls.Load(); calls != 1 {
		t.Fatalf("expected exactly one summary observation, got %d", calls)
	}
	if got := fixture.lastSummaryAuthorization(); got != "Bearer "+newToken {
		t.Fatalf("the summary request must carry the freshly exchanged token, got %q", got)
	}

	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row: %v", err)
	}
	raw := row.GetCredential("antigravity_quota")
	if !strings.Contains(raw, `"schema_version":2`) {
		t.Fatalf("persisted snapshot must be V2, got %s", raw)
	}
	if !strings.Contains(raw, `"source_metadata"`) || !strings.Contains(raw, `"groups"`) {
		t.Fatalf("persisted snapshot must carry groups source metadata, got %s", raw)
	}
	if got := row.GetCredential("access_token"); got != newToken {
		t.Fatalf("the recovered credential must be durable, access_token=%q", got)
	}
	// 认证恢复必须留下一整代新凭据，且内存态与该代际同源：quota 观测的预约与提交都排在
	// 换票之后，因此 durable 里的 groups 必然属于这一代——既不会被换票通道清空，也不是
	// 旧代际的残留。
	if row.CredentialGeneration != rowBefore.CredentialGeneration+1 {
		t.Fatalf("the credential recovery must advance the generation exactly once: before=%d after=%d",
			rowBefore.CredentialGeneration, row.CredentialGeneration)
	}
	account.mu.RLock()
	runtimeGeneration := account.CredentialGeneration
	runtimeAccessToken := account.AccessToken
	runtimeGroups := len(account.AntigravityQuota.Groups)
	account.mu.RUnlock()
	if runtimeGeneration != row.CredentialGeneration || runtimeAccessToken != newToken {
		t.Fatalf("runtime must be published from the recovered generation: runtime generation=%d access_token=%q, durable generation=%d",
			runtimeGeneration, runtimeAccessToken, row.CredentialGeneration)
	}
	if runtimeGroups == 0 {
		t.Fatalf("runtime projection must publish the observed groups on the recovered generation, snapshot=%+v", account.AntigravityQuota)
	}
}

// TestRefreshAntigravityQuotaSummaryRetriesOnceAfter401 断言名义未过期但已被上游拒绝
// 的令牌（401）能在同一次刷新里恢复：换票 → 按新代际重新预约 → 再观测一次即成功
// （Spec §4.2 的 401 认证恢复）。
func TestRefreshAntigravityQuotaSummaryRetriesOnceAfter401(t *testing.T) {
	const newToken = "access-fresh-after-401"
	fixture := newAntigravityQuotaAuthFixture(t, newToken, false)
	useAntigravityTestEndpoints(t, fixture.server.URL)

	store, db, account, accountID := newAntigravityRefreshTestAccount(t, nil)
	ctx := context.Background()
	if account.NeedsRefresh() {
		t.Fatal("fixture credential must not need a proactive refresh")
	}
	rowBefore, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row before refresh: %v", err)
	}

	if err := store.RefreshAntigravityQuotaSummary(ctx, account); err != nil {
		t.Fatalf("RefreshAntigravityQuotaSummary: %v", err)
	}
	if calls := fixture.summaryCalls.Load(); calls != 2 {
		t.Fatalf("a 401 must be retried exactly once after recovery, summary calls=%d", calls)
	}
	if calls := fixture.tokenCalls.Load(); calls != 1 {
		t.Fatalf("the 401 recovery must rotate the credential exactly once, token calls=%d", calls)
	}
	if got := fixture.lastSummaryAuthorization(); got != "Bearer "+newToken {
		t.Fatalf("the retried summary request must carry the recovered token, got %q", got)
	}
	if calls := fixture.staleSummaryCalls.Load(); calls != 1 {
		t.Fatalf("exactly one observation may be spent on the rejected credential before recovery, stale summary calls=%d", calls)
	}
	if calls := fixture.staleSummaryCalls.Load(); calls != 1 {
		t.Fatalf("exactly one observation may be spent on the rejected credential before recovery, stale summary calls=%d", calls)
	}

	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row: %v", err)
	}
	raw := row.GetCredential("antigravity_quota")
	if !strings.Contains(raw, `"schema_version":2`) || !strings.Contains(raw, `"groups"`) {
		t.Fatalf("the retried observation must persist V2 groups facts, got %s", raw)
	}
	if got := row.GetCredential("access_token"); got != newToken {
		t.Fatalf("the recovered credential must be durable, access_token=%q", got)
	}
	// 401 恢复同样必须留下整整一代新凭据，且 durable 的 groups 属于这一代（换票在预约与
	// 提交之前完成，提交栅栏按新代际取值）。
	if row.CredentialGeneration != rowBefore.CredentialGeneration+1 {
		t.Fatalf("the 401 recovery must advance the generation exactly once: before=%d after=%d",
			rowBefore.CredentialGeneration, row.CredentialGeneration)
	}
	account.mu.RLock()
	runtimeGeneration := account.CredentialGeneration
	runtimeAccessToken := account.AccessToken
	runtimeGroups := len(account.AntigravityQuota.Groups)
	account.mu.RUnlock()
	if runtimeGeneration != row.CredentialGeneration || runtimeAccessToken != newToken {
		t.Fatalf("runtime must be published from the recovered generation: runtime generation=%d access_token=%q, durable generation=%d",
			runtimeGeneration, runtimeAccessToken, row.CredentialGeneration)
	}
	if runtimeGroups == 0 {
		t.Fatalf("runtime projection must publish the retried groups on the recovered generation, snapshot=%+v", account.AntigravityQuota)
	}
}

// TestRefreshAntigravityQuotaSummaryDoesNotLoopOnPersistent401 断言 401 恢复只有一次
// 机会：换票后仍被拒绝即返回错误，绝不循环换票或循环观测（否则一个被吊销的账号会把
// OAuth 端点与配额端点一起打死）。
func TestRefreshAntigravityQuotaSummaryDoesNotLoopOnPersistent401(t *testing.T) {
	const newToken = "access-still-rejected"
	fixture := newAntigravityQuotaAuthFixture(t, newToken, true)
	useAntigravityTestEndpoints(t, fixture.server.URL)

	store, db, account, accountID := newAntigravityRefreshTestAccount(t, nil)
	ctx := context.Background()

	err := store.RefreshAntigravityQuotaSummary(ctx, account)
	if err == nil {
		t.Fatal("a persistently rejected summary must surface as a failure")
	}
	if calls := fixture.summaryCalls.Load(); calls > 2 {
		t.Fatalf("a persistent 401 must never be retried in a loop, summary calls=%d", calls)
	}
	if calls := fixture.tokenCalls.Load(); calls > 1 {
		t.Fatalf("a persistent 401 must trigger at most one credential recovery, token calls=%d", calls)
	}

	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("reload account row: %v", err)
	}
	// 预约协议自身会推进 current_attempt_seq，因此这里只断言「没有任何观测被提交」：
	// applied_seq 仍为 0、observed_at 仍为空。
	appliedSeq, observedAt := quotaSourceMetaFromPersisted(t, row.GetCredential("antigravity_quota"))
	if appliedSeq != 0 || !observedAt.IsZero() {
		t.Fatalf("a rejected observation must never be committed: applied_seq=%d observed_at=%v", appliedSeq, observedAt)
	}
}

// TestRefreshAntigravityQuotaSummaryRejectsAccountWithoutFamilyScope 断言缺少凭据
// family 身份时 fail-closed：OAuth 账号的正常路径必有 family，身份不明的账号一律不
// 预约、不联网（否则配额观测会写进身份不明的 durable 行）。
func TestRefreshAntigravityQuotaSummaryRejectsAccountWithoutFamilyScope(t *testing.T) {
	var summaryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		summaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, antigravityQuotaSummaryBody(time.Now().UTC().Add(2*time.Hour)))
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	store, db, account, accountID := newAntigravityRefreshTestAccount(t, map[string]any{"credential_family_id": ""})
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("reload account row: %v", err)
	}
	if family := strings.TrimSpace(row.GetCredential("credential_family_id")); family != "" {
		t.Fatalf("fixture must carry no credential family, got %q", family)
	}

	err = store.RefreshAntigravityQuotaSummary(context.Background(), account)
	if err == nil {
		t.Fatal("an account without a credential family must be rejected fail-closed")
	}
	if calls := summaryCalls.Load(); calls != 0 {
		t.Fatalf("the fail-closed path must not reach the network, summary calls=%d", calls)
	}
}

func TestRefreshAntigravityQuotaSummaryRecoversIdentityRevalidationQuarantine(t *testing.T) {
	const rotatedToken = "access-rotated-quarantined"
	var failUserinfo atomic.Bool
	failUserinfo.Store(true)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"`+rotatedToken+`","refresh_token":"refresh-rotated","expires_in":3600}`)
		case "/userinfo":
			if failUserinfo.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"temporary userinfo error"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"subject-1","email":"user@example.com","verified_email":true}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-recovered","paidTier":{"name":"Pro"}}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"models":{"gemini-3.8-flash-high":{"quotaInfo":{"remainingFraction":1}}}}`)
		case "/summary":
			_, _ = io.WriteString(w, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.95}]}]}`)
		case "/credits":
			_, _ = io.WriteString(w, `{"paidTier":{"availableCredits":[]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	store, db, account, accountID := newAntigravityRefreshTestAccount(t, map[string]any{
		"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	ctx := context.Background()

	if err := store.RefreshAntigravityQuotaSummary(ctx, account); err == nil {
		t.Fatal("first quota summary refresh should fail when userinfo returns 500")
	}
	quarantinedRow, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if quarantinedRow.GetCredential("project_id") != "" || !strings.HasPrefix(quarantinedRow.GetCredential("antigravity_sync_error"), antigravityIdentityRevalidationErrorPrefix) || account.IsAvailable() {
		t.Fatalf("expected account to be in identity revalidation quarantine, row=%#v available=%t", quarantinedRow.Credentials, account.IsAvailable())
	}

	failUserinfo.Store(false)
	if err := store.RefreshAntigravityQuotaSummary(ctx, account); err != nil {
		t.Fatalf("second RefreshAntigravityQuotaSummary should auto-recover quarantined account: %v", err)
	}
	recoveredRow, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredRow.GetCredential("project_id") != "project-recovered" || recoveredRow.GetCredential("account_id") != "subject-1" || recoveredRow.GetCredential("antigravity_sync_error") != "" || !account.IsAvailable() {
		t.Fatalf("quarantined account was not fully recovered: row=%#v available=%t", recoveredRow.Credentials, account.IsAvailable())
	}
}
