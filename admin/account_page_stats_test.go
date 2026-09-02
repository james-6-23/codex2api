package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func invokeAccountPageStats(t *testing.T, handler *Handler, ids []int64) map[string]accountPageStatsItem {
	t.Helper()
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/page-stats?ids="+strings.Join(parts, ","), nil)
	handler.GetAccountPageStats(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Stats map[string]accountPageStatsItem `json:"stats"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode page-stats: %v", err)
	}
	return payload.Stats
}

func ageAccountForOfficialUsage(t *testing.T, store *auth.Store, id int64) {
	t.Helper()
	account := store.FindByID(id)
	if account == nil {
		t.Fatalf("account %d not in store", id)
	}
	account.AddedAt = time.Now().Add(-25 * time.Hour).UnixNano()
}

func waitAccountDailyUsage(t *testing.T, db *database.DB, id int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		totals, err := db.SumAccountDailyUsage(context.Background(), []int64{id}, 7)
		if err != nil {
			t.Fatalf("SumAccountDailyUsage: %v", err)
		}
		if _, ok := totals[id]; ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("account %d snapshot did not appear", id)
}

func TestGetAccountPageStatsBackfillsMissingOfficialUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	codexID, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt-codex",
		"access_token":  "oauth-codex",
		"email":         "codex@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert codex: %v", err)
	}
	grokID, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "oauth", map[string]interface{}{
		"upstream_type": "grok",
		"refresh_token": "rt-grok",
		"access_token":  "at-grok",
		"email":         "grok@example.net",
	}, "")
	if err != nil {
		t.Fatalf("insert grok: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")
	ageAccountForOfficialUsage(t, store, codexID)

	var mu sync.Mutex
	called := make([]int64, 0, 2)
	queried := make(chan struct{}, 1)
	handler.queryWhamDailyUsage = func(_ context.Context, account *auth.Account, _, _, _ string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		mu.Lock()
		called = append(called, account.DBID)
		mu.Unlock()
		select {
		case queried <- struct{}{}:
		default:
		}
		return &proxy.WhamDailyUsageResponse{Data: []proxy.WhamDailyUsageDay{{
			Date:   time.Now().UTC().Format("2006-01-02"),
			Totals: proxy.WhamDailyUsageCounts{Credits: 50},
		}}}, nil, nil
	}

	first := invokeAccountPageStats(t, handler, []int64{codexID, grokID})
	codexKey := strconv.FormatInt(codexID, 10)
	if item := first[codexKey]; item.OfficialUSD7d != nil {
		t.Fatalf("first page-stats official = %v, want omitted until snapshot exists", *item.OfficialUSD7d)
	}

	select {
	case <-queried:
	case <-time.After(2 * time.Second):
		t.Fatal("missing official snapshot did not trigger upstream backfill")
	}
	waitAccountDailyUsage(t, db, codexID)

	second := invokeAccountPageStats(t, handler, []int64{codexID, grokID})
	got := second[codexKey].OfficialUSD
	if got == nil || *got != 2 {
		t.Fatalf("official_usd = %v, want 2 (50 credits / 25)", got)
	}
	if second[codexKey].OfficialUSD7d == nil || *second[codexKey].OfficialUSD7d != 2 {
		t.Fatalf("official_usd_7d alias = %v, want 2", second[codexKey].OfficialUSD7d)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(called) != 1 || called[0] != codexID {
		t.Fatalf("upstream accounts = %v, want only codex %d", called, codexID)
	}
}

// 上游同步成功但窗口内没有数据(官方统计滞后/账号没有官方消耗)时,
// page-stats 必须下发显式空态并停止回补,而不是让前端永远当"还在加载"。
func TestGetAccountPageStatsMarksSyncedWhenUpstreamHasNoData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt",
		"access_token":  "at",
		"email":         "codex@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")
	ageAccountForOfficialUsage(t, store, id)

	var mu sync.Mutex
	calls := 0
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &proxy.WhamDailyUsageResponse{}, nil, nil
	}

	key := strconv.FormatInt(id, 10)
	first := invokeAccountPageStats(t, handler, []int64{id})
	if first[key].OfficialUSD7d != nil || first[key].OfficialUsageSynced {
		t.Fatalf("first page-stats = %+v, want plain missing before sync", first[key])
	}

	deadline := time.Now().Add(2 * time.Second)
	for !handler.whamDailySyncedOnceFor(id) {
		if time.Now().After(deadline) {
			t.Fatal("empty upstream sync did not mark the account as synced")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := invokeAccountPageStats(t, handler, []int64{id})
	item := second[key]
	if item.OfficialUSD7d != nil {
		t.Fatalf("official_usd_7d = %v, want omitted for empty upstream data", *item.OfficialUSD7d)
	}
	if !item.OfficialUsageSynced {
		t.Fatal("official_usage_synced missing: frontend would keep spinning and re-polling")
	}

	// 再打一次不允许触发新的回补:已同步的账号不进 missing 列表。
	invokeAccountPageStats(t, handler, []int64{id})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", calls)
	}
}

// 回补失败后要按失败冷却退避,而不是每过 2 分钟的普通冷却就再打一次上游。
func TestWhamDailyBackfillFailureCooldownSkipsRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt",
		"access_token":  "at",
		"email":         "codex@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")
	ageAccountForOfficialUsage(t, store, id)

	var mu sync.Mutex
	calls := 0
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil, nil, errWhamDailyUsageUnavailable
	}

	invokeAccountPageStats(t, handler, []int64{id})
	deadline := time.Now().Add(2 * time.Second)
	for {
		handler.whamDailyBackfillMu.Lock()
		_, failed := handler.whamDailyBackfillFailedAt[id]
		handler.whamDailyBackfillMu.Unlock()
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backfill failure was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 越过普通 2 分钟冷却,只留失败冷却:不允许再打上游。
	handler.whamDailyBackfillMu.Lock()
	delete(handler.whamDailyBackfillLast, id)
	handler.whamDailyBackfillMu.Unlock()
	invokeAccountPageStats(t, handler, []int64{id})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if calls != 1 {
		mu.Unlock()
		t.Fatalf("upstream calls = %d, want 1 while failure cooldown active", calls)
	}
	mu.Unlock()

	// 失败冷却过期后允许重试。
	handler.whamDailyBackfillMu.Lock()
	handler.whamDailyBackfillFailedAt[id] = time.Now().Add(-whamDailyUsageBackfillFailureCooldown - time.Minute)
	delete(handler.whamDailyBackfillLast, id)
	handler.whamDailyBackfillMu.Unlock()
	invokeAccountPageStats(t, handler, []int64{id})
	deadline = time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		done := calls >= 2
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired failure cooldown did not allow a retry")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGetAccountPageStatsSkipsOfficialBackfillForNewAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt-new",
		"access_token":  "oauth-new",
		"email":         "new@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		t.Fatal("new account must not hit official usage upstream")
		return nil, nil, nil
	}

	invokeAccountPageStats(t, handler, []int64{id})
	time.Sleep(80 * time.Millisecond)
}

func TestGetAccountPageStatsSkipsOfficialBackfillForCodexAT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex-at", map[string]interface{}{
		"access_token":      "at-opaque",
		"access_token_type": accessTokenTypeCodexAT,
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")
	ageAccountForOfficialUsage(t, store, id)
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		t.Fatal("codex_at account must not hit official usage upstream")
		return nil, nil, nil
	}

	stats := invokeAccountPageStats(t, handler, []int64{id})
	item := stats[strconv.FormatInt(id, 10)]
	if item.OfficialUSD7d != nil || item.OfficialUsageSynced {
		t.Fatalf("codex_at page-stats = %+v, want no official usage fields", item)
	}
	handler.whamDailyBackfillMu.Lock()
	_, scheduled := handler.whamDailyBackfillLast[id]
	handler.whamDailyBackfillMu.Unlock()
	if scheduled {
		t.Fatal("codex_at account was scheduled for official usage backfill")
	}
}

func TestGetAccountPageStatsSkipsOfficialBackfillWhenSnapshotExists(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt",
		"access_token":  "at",
		"email":         "codex@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.UpsertAccountDailyUsage(ctx, database.AccountDailyUsageInput{
		AccountID: id,
		Day:       time.Now().UTC().Format("2006-01-02"),
		Credits:   25,
		Settled:   true,
	}); err != nil {
		t.Fatalf("upsert snapshot: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		t.Fatal("existing snapshot must not hit upstream")
		return nil, nil, nil
	}

	stats := invokeAccountPageStats(t, handler, []int64{id})
	got := stats[strconv.FormatInt(id, 10)].OfficialUSD
	if got == nil || *got != 1 {
		t.Fatalf("official_usd = %v, want 1", got)
	}
}

func TestGetAccountPageStatsOfficialUSDIncludesOlderSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt",
		"access_token":  "at",
		"email":         "codex-history@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	now := time.Now().UTC()
	for _, item := range []database.AccountDailyUsageInput{
		{AccountID: id, Day: now.Format("2006-01-02"), Credits: 25, Settled: true},
		{AccountID: id, Day: now.AddDate(0, 0, -20).Format("2006-01-02"), Credits: 50, Settled: true},
	} {
		if err := db.UpsertAccountDailyUsage(ctx, item); err != nil {
			t.Fatalf("upsert %s: %v", item.Day, err)
		}
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		t.Fatal("existing snapshot must not hit upstream")
		return nil, nil, nil
	}

	stats := invokeAccountPageStats(t, handler, []int64{id})
	got := stats[strconv.FormatInt(id, 10)].OfficialUSD
	if got == nil || *got != 3 {
		t.Fatalf("official_usd = %v, want 3 (75 credits / 25, including day -20)", got)
	}
}

func TestGetAccountPageStatsIncludesTodayModelCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt-today-models",
		"access_token":  "oauth-today-models",
		"email":         "today-models@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	for _, item := range []struct {
		model        string
		statusCode   int
		firstTokenMs int
	}{
		{model: "gpt-5.4", statusCode: http.StatusOK, firstTokenMs: 1200},
		{model: "gpt-5.4", statusCode: http.StatusTooManyRequests, firstTokenMs: 1800},
		{model: "gpt-5.2", statusCode: http.StatusOK, firstTokenMs: 500},
	} {
		if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
			AccountID:    id,
			Endpoint:     "/v1/responses",
			Model:        item.model,
			StatusCode:   item.statusCode,
			TotalTokens:  40,
			FirstTokenMs: item.firstTokenMs,
		}); err != nil {
			t.Fatalf("InsertUsageLog(%s): %v", item.model, err)
		}
	}
	db.FlushUsageLogs()

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")

	stats := invokeAccountPageStats(t, handler, []int64{id})
	today := stats[strconv.FormatInt(id, 10)].UsageTodayDetail
	if today == nil || today.Requests != 3 {
		t.Fatalf("today detail = %+v, want 3 requests", today)
	}
	if today.ModelCounts["gpt-5.4"] != 2 || today.ModelCounts["gpt-5.2"] != 1 {
		t.Fatalf("today model counts = %#v, want gpt-5.4=2 gpt-5.2=1", today.ModelCounts)
	}
	if today.ModelSuccessCounts["gpt-5.4"] != 1 || today.ModelSuccessCounts["gpt-5.2"] != 1 {
		t.Fatalf("today model success = %#v, want gpt-5.4=1 gpt-5.2=1", today.ModelSuccessCounts)
	}
	if today.ModelAvgFirstTokenMs["gpt-5.4"] != 1500 || today.ModelAvgFirstTokenMs["gpt-5.2"] != 500 {
		t.Fatalf("today model first-token averages = %#v, want gpt-5.4=1500 gpt-5.2=500", today.ModelAvgFirstTokenMs)
	}
}
