package admin

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func approx(got, want float64) bool {
	return math.Abs(got-want) < 1e-6
}

// 估算 = 已用成本 ÷ 已用百分比，区间按整数百分比 ±0.5 推算；不足 10% 标不可靠。
func TestProjectWhamDailyCycleEstimate(t *testing.T) {
	now := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	reset := now.Add(36 * time.Hour)
	base := whamDailyCycleInput{
		ResetAt: reset, WindowSeconds: 604800, WindowKind: "weekly",
		UsedPercent: 20, UsedPercentOK: true, PercentUpdated: now.Add(-time.Minute),
		UsedCredits: 100, Days: 5,
	}

	out := projectWhamDailyCycle(base, now)
	if out["available"] != true || out["reason"] != nil {
		t.Fatalf("expected available cycle without reason: %#v", out)
	}
	if !approx(out["used_usd"].(float64), 4) || out["days"] != 5 {
		t.Fatalf("used_usd/days = %v/%v", out["used_usd"], out["days"])
	}
	if start := out["start_at"].(time.Time); !start.Equal(reset.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("start_at = %v", start)
	}
	est, ok := out["estimate"].(gin.H)
	if !ok {
		t.Fatalf("estimate missing: %#v", out)
	}
	if !approx(est["usd"].(float64), 20) || !approx(est["usd_low"].(float64), 4/0.205) || !approx(est["usd_high"].(float64), 4/0.195) {
		t.Fatalf("estimate = %#v, want 20 in [19.51, 20.51]", est)
	}
	if est["reliable"] != true {
		t.Fatalf("20%% used must be reliable: %#v", est)
	}

	low := base
	low.UsedPercent = 2
	est = projectWhamDailyCycle(low, now)["estimate"].(gin.H)
	if !approx(est["usd"].(float64), 200) || est["reliable"] != false {
		t.Fatalf("2%% used must give $200 and be unreliable: %#v", est)
	}
	if !approx(est["usd_low"].(float64), 160) || !approx(est["usd_high"].(float64), 4/0.015) {
		t.Fatalf("2%% band = %#v, want [160, 266.67]", est)
	}
}

func TestProjectWhamDailyCycleReasons(t *testing.T) {
	now := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	ok := whamDailyCycleInput{ResetAt: now.Add(time.Hour), WindowSeconds: 604800, UsedPercent: 30, UsedPercentOK: true, UsedCredits: 10}

	cases := []struct {
		name   string
		mutate func(*whamDailyCycleInput)
		reason string
		avail  bool
	}{
		{"no window", func(in *whamDailyCycleInput) { in.ResetAt = time.Time{} }, "no_window", false},
		{"stale window", func(in *whamDailyCycleInput) { in.ResetAt = now.Add(-time.Second) }, "window_stale", false},
		{"no percent", func(in *whamDailyCycleInput) { in.UsedPercentOK = false }, "no_percent", false},
		{"no credits", func(in *whamDailyCycleInput) { in.UsedCredits = 0 }, "no_credits", true},
		{"percent too low", func(in *whamDailyCycleInput) { in.UsedPercent = 0 }, "percent_too_low", true},
	}
	for _, tc := range cases {
		in := ok
		tc.mutate(&in)
		out := projectWhamDailyCycle(in, now)
		if out["reason"] != tc.reason || out["available"] != tc.avail {
			t.Fatalf("%s: reason=%v available=%v, want %s/%v", tc.name, out["reason"], out["available"], tc.reason, tc.avail)
		}
		if _, has := out["estimate"]; has {
			t.Fatalf("%s: estimate must be absent: %#v", tc.name, out)
		}
	}
}

// 接口按账号内存里的窗口快照给出本周期成本：只累加周期起始日（UTC 整天）之后带 counts
// 数据的行，周期之前的天不算，拆分先建的零 counts 行也不算。
func TestGetAccountWhamDailyUsageIncludesCycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, account := newWhamDailyTestHandler(t)
	ctx := context.Background()

	now := time.Now()
	reset := now.Add(48 * time.Hour)
	account.Reset7dAt = reset
	account.Window7dSeconds = 604800
	account.UsagePercent7d = 25
	account.UsagePercent7dValid = true
	startDay := reset.Add(-7 * 24 * time.Hour).UTC().Format("2006-01-02")
	beforeCycle := reset.Add(-8 * 24 * time.Hour).UTC().Format("2006-01-02")
	inCycle := reset.Add(-3 * 24 * time.Hour).UTC().Format("2006-01-02")

	for _, row := range []database.AccountDailyUsageInput{
		{AccountID: account.DBID, Day: beforeCycle, Credits: 500, Turns: 9, Settled: true},
		{AccountID: account.DBID, Day: startDay, Credits: 40, Turns: 2, Settled: true},
		{AccountID: account.DBID, Day: inCycle, Credits: 60, Turns: 3, Settled: true},
	} {
		if err := handler.db.UpsertAccountDailyUsage(ctx, row); err != nil {
			t.Fatalf("upsert %s: %v", row.Day, err)
		}
	}
	// 周期内一条只有拆分、没有 counts 的行，不能进已用成本。
	if err := handler.db.UpsertAccountDailyBreakdown(ctx, database.AccountDailyBreakdownInput{
		AccountID: account.DBID, Day: reset.Add(-2 * 24 * time.Hour).UTC().Format("2006-01-02"), Percent: 1,
		Models: []database.AccountDailyBreakdownModel{{Model: "gpt-5.6-sol", Speed: "standard", Percent: 1}},
	}); err != nil {
		t.Fatalf("breakdown upsert: %v", err)
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(account.DBID, 10)}}
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/"+strconv.FormatInt(account.DBID, 10)+"/wham-daily-usage?days=30", nil)
	handler.GetAccountWhamDailyUsage(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Cycle struct {
			Available   bool    `json:"available"`
			Reason      string  `json:"reason"`
			UsedCredits float64 `json:"used_credits"`
			UsedUSD     float64 `json:"used_usd"`
			Days        int     `json:"days"`
			UsedPercent float64 `json:"used_percent"`
			WindowKind  string  `json:"window_kind"`
			Estimate    *struct {
				USD      float64 `json:"usd"`
				Reliable bool    `json:"reliable"`
			} `json:"estimate"`
		} `json:"cycle"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c := payload.Cycle
	if !c.Available || c.Reason != "" {
		t.Fatalf("cycle = %#v", c)
	}
	if c.UsedCredits != 100 || c.Days != 2 || !approx(c.UsedUSD, 4) || c.UsedPercent != 25 || c.WindowKind != "weekly" {
		t.Fatalf("cycle sums = %#v, want 100 credits over 2 days at 25%%", c)
	}
	if c.Estimate == nil || !approx(c.Estimate.USD, 16) || !c.Estimate.Reliable {
		t.Fatalf("estimate = %#v, want $16 reliable", c.Estimate)
	}
}
