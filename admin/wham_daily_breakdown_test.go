package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func newWhamDailyTestHandler(t *testing.T) (*Handler, *auth.Account) {
	t.Helper()
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "plus-oauth", map[string]interface{}{
		"access_token":  "oauth-access-token",
		"refresh_token": "oauth-refresh-token",
		"account_id":    "ws-1",
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	store := auth.NewStore(db, nil, nil)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	handler := &Handler{store: store, db: db}
	account := store.FindByID(id)
	if account == nil {
		t.Fatal("account not loaded")
	}
	return handler, account
}

func upstreamFailure(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func whamInt64Ptr(v int64) *int64 { return &v }

func utcDay(offset int) string {
	return time.Now().UTC().AddDate(0, 0, offset).Format("2006-01-02")
}

// counts 失败不能拖累拆分落库：两个端点并行、独立写自己的列。
// 拆分端点是稠密的，全 0 行与未来日期必须跳过，否则会造出一堆空行。
func TestSyncWhamDailyUsageBreakdownSurvivesCountsFailure(t *testing.T) {
	handler, account := newWhamDailyTestHandler(t)
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		return nil, upstreamFailure(http.StatusTooManyRequests, "busy"), errors.New("daily usage returned status 429")
	}
	yesterday, today, tomorrow := utcDay(-1), utcDay(0), utcDay(1)
	handler.queryWhamDailyTokenBreakdown = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyTokenBreakdownResponse, *http.Response, error) {
		return &proxy.WhamDailyTokenBreakdownResponse{Units: "percent", GroupBy: "day", Data: []proxy.WhamDailyTokenBreakdownDay{
			{Date: yesterday, Models: []proxy.WhamDailyTokenBreakdownModel{
				{Model: "gpt-5.6-sol", Speed: "fast", Percent: 3},
				{Model: "gpt-5.6-luna", Speed: "standard", Percent: 1},
				{Model: "gpt-6-astra", Speed: "standard", Percent: 0},
			}, Surfaces: map[string]float64{"cli": 4, "vscode": 0}},
			{Date: today, Models: []proxy.WhamDailyTokenBreakdownModel{{Model: "gpt-5.6-sol", Speed: "standard", Percent: 0}}, Surfaces: map[string]float64{"cli": 0}},
			{Date: tomorrow, Models: []proxy.WhamDailyTokenBreakdownModel{{Model: "gpt-5.6-sol", Speed: "standard", Percent: 5}}, Surfaces: map[string]float64{"cli": 5}},
		}}, &http.Response{StatusCode: http.StatusOK}, nil
	}

	outcome, err := handler.syncWhamDailyUsage(context.Background(), account)
	if !errors.Is(err, errWhamDailyUsageRateLimited) {
		t.Fatalf("err = %v, want rate limited", err)
	}
	if outcome.BreakdownErr != nil || outcome.BreakdownDays != 1 || outcome.CountsDays != 0 {
		t.Fatalf("outcome = %#v, want exactly one breakdown day written", outcome)
	}
	if handler.whamDailySyncedOnceFor(account.DBID) {
		t.Fatal("counts failure must not mark the account as synced")
	}
	rows, err := handler.db.ListAccountDailyUsage(context.Background(), account.DBID, 7)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v, want the single active day (zero and future rows skipped)", len(rows), err)
	}
	got := rows[0]
	if got.Day != yesterday || got.Credits != 0 || got.BreakdownPercent != 4 || got.HasCounts() {
		t.Fatalf("row = %#v", got)
	}
	models := database.ParseAccountDailyBreakdownModels(got.BreakdownJSON)
	if len(models) != 2 || models[0].Model != "gpt-5.6-sol" || models[0].Speed != "fast" {
		t.Fatalf("models = %#v (zero catalog rows must be dropped)", models)
	}
}

// 拆分失败是软失败：counts 照常落库并标记已同步，错误只随 outcome 带回。
// 同时验证结算判定按日期：今天的行即便带 token 也是未结算。
func TestSyncWhamDailyUsageBreakdownFailureIsSoft(t *testing.T) {
	handler, account := newWhamDailyTestHandler(t)
	yesterday, today := utcDay(-1), utcDay(0)
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		return &proxy.WhamDailyUsageResponse{GroupBy: "day", Data: []proxy.WhamDailyUsageDay{
			{Date: yesterday, Totals: proxy.WhamDailyUsageCounts{Turns: 14, Credits: 3.34, TextTotalTokens: whamInt64Ptr(1000)},
				Models: []proxy.WhamDailyUsageModel{{Model: "gpt-5.6-sol"}}},
			// 2026-09 形态的当天行：有 token 与 credits，turns=0，没有 models。
			{Date: today, Totals: proxy.WhamDailyUsageCounts{Turns: 0, Credits: 54.35, TextTotalTokens: whamInt64Ptr(18641788)}},
		}}, &http.Response{StatusCode: http.StatusOK}, nil
	}
	handler.queryWhamDailyTokenBreakdown = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyTokenBreakdownResponse, *http.Response, error) {
		return nil, upstreamFailure(http.StatusUnauthorized, "expired"), errors.New("token breakdown returned status 401")
	}

	outcome, err := handler.syncWhamDailyUsage(context.Background(), account)
	if err != nil {
		t.Fatalf("counts success must not surface breakdown failure as error: %v", err)
	}
	if outcome.CountsDays != 2 || !errors.Is(outcome.BreakdownErr, errWhamDailyUsageUnauthorized) {
		t.Fatalf("outcome = %#v", outcome)
	}
	if !handler.whamDailySyncedOnceFor(account.DBID) {
		t.Fatal("counts success must mark the account as synced")
	}
	rows, err := handler.db.ListAccountDailyUsage(context.Background(), account.DBID, 7)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if !rows[0].Settled || rows[0].Day != yesterday {
		t.Fatalf("yesterday must be settled: %#v", rows[0])
	}
	if rows[1].Settled || rows[1].TotalTokens != 18641788 {
		t.Fatalf("today carries tokens but must stay unsettled: %#v", rows[1])
	}
	if rows[0].BreakdownPercent != 0 {
		t.Fatalf("failed breakdown must leave the split empty: %#v", rows[0])
	}
}

// 读 body / 解析失败时查询函数会带回一个 200 的 resp（body 已读掉），分类器必须原样
// 返回真实错误，而不是报成「上游返回 200」。
func TestClassifyWhamDailyUpstreamErrorKeepsParseErrors(t *testing.T) {
	parseErr := errors.New("parse daily usage response: unexpected end of JSON input")
	got := classifyWhamDailyUpstreamError(&http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, parseErr)
	if !errors.Is(got, parseErr) {
		t.Fatalf("200 resp must keep the original error, got %v", got)
	}
	if got := classifyWhamDailyUpstreamError(nil, parseErr); !errors.Is(got, parseErr) {
		t.Fatalf("nil resp must keep the original error, got %v", got)
	}
	if got := classifyWhamDailyUpstreamError(upstreamFailure(http.StatusUnauthorized, "x"), errors.New("401")); !errors.Is(got, errWhamDailyUsageUnauthorized) {
		t.Fatalf("401 must map to unauthorized, got %v", got)
	}
}

// 深回补按端点各自决定：counts 首轮 429、拆分成功（拆分以零 counts 建了行）之后，
// 下一轮 counts 仍要走 84 天，否则历史永远拿不到；拆分已深拉过就回到 7 天。两者都深
// 拉过一次后全部回到滚动窗口。
func TestSyncWhamDailyUsageDeepBackfillIsPerEndpoint(t *testing.T) {
	handler, account := newWhamDailyTestHandler(t)
	var (
		mu     sync.Mutex
		starts = map[string][]string{}
	)
	record := func(kind, start string) {
		mu.Lock()
		starts[kind] = append(starts[kind], start)
		mu.Unlock()
	}
	var countsFail atomic.Bool
	countsFail.Store(true)
	yesterday := utcDay(-1)
	handler.queryWhamDailyUsage = func(_ context.Context, _ *auth.Account, _ string, start, _ string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		record("counts", start)
		if countsFail.Load() {
			return nil, upstreamFailure(http.StatusTooManyRequests, "busy"), errors.New("daily usage returned status 429")
		}
		return &proxy.WhamDailyUsageResponse{Data: []proxy.WhamDailyUsageDay{{Date: yesterday, Totals: proxy.WhamDailyUsageCounts{Credits: 1, Turns: 1, TextTotalTokens: whamInt64Ptr(1)}}}}, &http.Response{StatusCode: http.StatusOK}, nil
	}
	handler.queryWhamDailyTokenBreakdown = func(_ context.Context, _ *auth.Account, _ string, start, _ string) (*proxy.WhamDailyTokenBreakdownResponse, *http.Response, error) {
		record("breakdown", start)
		return &proxy.WhamDailyTokenBreakdownResponse{Units: "percent", Data: []proxy.WhamDailyTokenBreakdownDay{
			{Date: yesterday, Models: []proxy.WhamDailyTokenBreakdownModel{{Model: "gpt-5.6-sol", Speed: "standard", Percent: 5}}, Surfaces: map[string]float64{"cli": 5}},
		}}, &http.Response{StatusCode: http.StatusOK}, nil
	}
	deepStart, _ := proxy.WhamDailyUsageBackfillWindow(time.Now())
	rollingStart, _ := proxy.WhamDailyUsageWindow(time.Now())

	ctx := context.Background()
	if _, err := handler.syncWhamDailyUsage(ctx, account); !errors.Is(err, errWhamDailyUsageRateLimited) {
		t.Fatalf("first sync err = %v, want rate limited", err)
	}
	countsFail.Store(false)
	if _, err := handler.syncWhamDailyUsage(ctx, account); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, err := handler.syncWhamDailyUsage(ctx, account); err != nil {
		t.Fatalf("third sync: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := map[string][]string{
		"counts":    {deepStart, deepStart, rollingStart},
		"breakdown": {deepStart, rollingStart, rollingStart},
	}
	for kind, expected := range want {
		if got := starts[kind]; strings.Join(got, ",") != strings.Join(expected, ",") {
			t.Fatalf("%s windows = %v, want %v", kind, got, expected)
		}
	}
}

// 上游确实没有数据的账号（新号/闲置号）只深拉一次，之后按滚动窗口，不会每轮都拉
// 84 天的稠密拆分。
func TestSyncWhamDailyUsageEmptyAccountDeepBackfillsOnce(t *testing.T) {
	handler, account := newWhamDailyTestHandler(t)
	var (
		mu     sync.Mutex
		starts = map[string][]string{}
	)
	record := func(kind, start string) {
		mu.Lock()
		starts[kind] = append(starts[kind], start)
		mu.Unlock()
	}
	handler.queryWhamDailyUsage = func(_ context.Context, _ *auth.Account, _ string, start, _ string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		record("counts", start)
		return &proxy.WhamDailyUsageResponse{GroupBy: "day"}, &http.Response{StatusCode: http.StatusOK}, nil
	}
	handler.queryWhamDailyTokenBreakdown = func(_ context.Context, _ *auth.Account, _ string, start, _ string) (*proxy.WhamDailyTokenBreakdownResponse, *http.Response, error) {
		record("breakdown", start)
		return &proxy.WhamDailyTokenBreakdownResponse{Units: "percent"}, &http.Response{StatusCode: http.StatusOK}, nil
	}
	deepStart, _ := proxy.WhamDailyUsageBackfillWindow(time.Now())
	rollingStart, _ := proxy.WhamDailyUsageWindow(time.Now())

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := handler.syncWhamDailyUsage(ctx, account); err != nil {
			t.Fatalf("sync %d: %v", i+1, err)
		}
	}
	if !handler.whamDailySyncedOnceFor(account.DBID) {
		t.Fatal("empty upstream data still counts as a successful sync")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, kind := range []string{"counts", "breakdown"} {
		if got := starts[kind]; len(got) != 2 || got[0] != deepStart || got[1] != rollingStart {
			t.Fatalf("%s windows = %v, want [%s %s]", kind, got, deepStart, rollingStart)
		}
	}
}

// 接口把归一化占比换成当天份额，再按当天官方 credits 分摊成绝对成本；
// 拆分先到、counts 还没来的行不进 items 与 totals。
func TestGetAccountWhamDailyUsageProjectsBreakdownShares(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, account := newWhamDailyTestHandler(t)
	ctx := context.Background()
	breakdownOnly, withSplit, withoutSplit := utcDay(-3), utcDay(-2), utcDay(-1)
	if err := handler.db.UpsertAccountDailyBreakdown(ctx, database.AccountDailyBreakdownInput{
		AccountID: account.DBID, Day: breakdownOnly, Percent: 2,
		Models: []database.AccountDailyBreakdownModel{{Model: "gpt-5.6-sol", Speed: "standard", Percent: 2}},
	}); err != nil {
		t.Fatalf("breakdown-only upsert: %v", err)
	}
	if err := handler.db.UpsertAccountDailyUsage(ctx, database.AccountDailyUsageInput{
		AccountID: account.DBID, Day: withSplit, Credits: 100, Turns: 3, Settled: true, ClientsJSON: "[]", ModelsJSON: `[{"model":"gpt-5.6-sol","turns":3}]`,
	}); err != nil {
		t.Fatalf("counts upsert: %v", err)
	}
	if err := handler.db.UpsertAccountDailyBreakdown(ctx, database.AccountDailyBreakdownInput{
		AccountID: account.DBID, Day: withSplit, Percent: 4,
		Models:   []database.AccountDailyBreakdownModel{{Model: "gpt-5.6-sol", Speed: "fast", Percent: 3}, {Model: "gpt-5.6-luna", Speed: "standard", Percent: 1}},
		Surfaces: map[string]float64{"cli": 3, "desktop_app": 1},
	}); err != nil {
		t.Fatalf("breakdown upsert: %v", err)
	}
	if err := handler.db.UpsertAccountDailyUsage(ctx, database.AccountDailyUsageInput{
		AccountID: account.DBID, Day: withoutSplit, Credits: 50, Turns: 1, Settled: true, ClientsJSON: "[]", ModelsJSON: "[]",
	}); err != nil {
		t.Fatalf("counts upsert 2: %v", err)
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(account.DBID, 10)}}
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/"+strconv.FormatInt(account.DBID, 10)+"/wham-daily-usage?days=7", nil)
	handler.GetAccountWhamDailyUsage(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		RetentionDays int `json:"retention_days"`
		Totals        struct {
			Credits float64 `json:"credits"`
			Turns   int64   `json:"turns"`
		} `json:"totals"`
		Items []struct {
			Day                string `json:"day"`
			BreakdownAvailable bool   `json:"breakdown_available"`
			Breakdown          []struct {
				Model   string  `json:"model"`
				Speed   string  `json:"speed"`
				Share   float64 `json:"share"`
				Credits float64 `json:"credits"`
				USD     float64 `json:"usd"`
			} `json:"breakdown"`
			Surfaces []struct {
				Surface string  `json:"surface"`
				Share   float64 `json:"share"`
				Credits float64 `json:"credits"`
			} `json:"surfaces"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.RetentionDays != proxy.WhamDailyUsageBackfillDays {
		t.Fatalf("retention_days = %d, want backfill depth %d", payload.RetentionDays, proxy.WhamDailyUsageBackfillDays)
	}
	if len(payload.Items) != 2 || payload.Items[0].Day != withSplit {
		t.Fatalf("items must skip the breakdown-only day: %#v", payload.Items)
	}
	if payload.Totals.Credits != 150 || payload.Totals.Turns != 4 {
		t.Fatalf("totals = %#v", payload.Totals)
	}
	first := payload.Items[0]
	if !first.BreakdownAvailable || len(first.Breakdown) != 2 {
		t.Fatalf("first item = %#v", first)
	}
	sol := first.Breakdown[0]
	if sol.Model != "gpt-5.6-sol" || sol.Speed != "fast" || sol.Share != 0.75 || sol.Credits != 75 || sol.USD != 3 {
		t.Fatalf("sol fast = %#v, want share .75 / 75 credits / $3", sol)
	}
	if luna := first.Breakdown[1]; luna.Share != 0.25 || luna.Credits != 25 {
		t.Fatalf("luna = %#v", luna)
	}
	if len(first.Surfaces) != 2 || first.Surfaces[0].Surface != "cli" || first.Surfaces[0].Share != 0.75 || first.Surfaces[0].Credits != 75 {
		t.Fatalf("surfaces = %#v", first.Surfaces)
	}
	second := payload.Items[1]
	if second.Day != withoutSplit || second.BreakdownAvailable || len(second.Breakdown) != 0 || len(second.Surfaces) != 0 {
		t.Fatalf("day without split must expose empty arrays: %#v", second)
	}
}
