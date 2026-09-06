package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

// 2026-09-06 真实抓包裁剪：稠密行、目录里未用的模型 credits=0、speed 可能为空、
// 未来日期全 0。units 恒为 percent（按请求区间峰值归一化，不是额度占比）。
const whamBreakdownFixture = `{"data":[
 {"date":"2026-08-14",
  "product_surface_usage_values":{"cli":2.9683,"vscode":0.0,"desktop_app":0.0,"unknown":0.0},
  "models":[
   {"model":"gpt-5.6-sol","speed":"standard","credits":2.9},
   {"model":"gpt-5.6-sol","speed":"fast","credits":0.0},
   {"model":"gpt-5.6-luna","speed":"standard","credits":0.0683},
   {"model":"gpt-image-2","speed":"","credits":0.01},
   {"model":"codex-auto-review","speed":"standard","credits":0.0}
  ]},
 {"date":"2026-09-07","product_surface_usage_values":{"cli":0.0},"models":[]}
],"group_by":"day","units":"percent"}`

func TestWhamDailyTokenBreakdownParsesCatalogAndShares(t *testing.T) {
	var parsed WhamDailyTokenBreakdownResponse
	if err := json.Unmarshal([]byte(whamBreakdownFixture), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Units != "percent" {
		t.Fatalf("units = %q, want percent", parsed.Units)
	}
	if len(parsed.Data) != 2 {
		t.Fatalf("expected 2 dense rows, got %d", len(parsed.Data))
	}

	day := parsed.Data[0]
	if total := day.TotalPercent(); total < 2.9782 || total > 2.9784 {
		t.Fatalf("total percent = %f, want 2.9783", total)
	}
	active := day.ActiveModels()
	if len(active) != 3 {
		t.Fatalf("active models = %#v, want 3 non-zero rows", active)
	}
	// 按占比降序；目录里 credits=0 的行被剔除；空 speed 补 standard。
	if active[0].Model != "gpt-5.6-sol" || active[0].Speed != WhamSpeedStandard || active[0].Percent != 2.9 {
		t.Fatalf("first active = %#v", active[0])
	}
	if active[1].Model != "gpt-5.6-luna" || active[2].Model != "gpt-image-2" {
		t.Fatalf("order = %#v", active)
	}
	if active[2].Speed != WhamSpeedStandard {
		t.Fatalf("empty speed must default to standard, got %q", active[2].Speed)
	}
	surfaces := day.ActiveSurfaces()
	if len(surfaces) != 1 || surfaces["cli"] != 2.9683 {
		t.Fatalf("active surfaces = %#v", surfaces)
	}

	future := parsed.Data[1]
	if future.TotalPercent() != 0 || len(future.ActiveModels()) != 0 || len(future.ActiveSurfaces()) != 0 {
		t.Fatalf("future zero row must be inert: %#v", future)
	}
}

func TestQueryWhamDailyTokenBreakdownRequestShape(t *testing.T) {
	var gotQuery map[string][]string
	var gotHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		gotHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(whamBreakdownFixture))
	}))
	defer server.Close()
	restore := SetWhamDailyTokenBreakdownURLForTest(server.URL)
	defer restore()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	resp, httpResp, err := QueryWhamDailyTokenBreakdown(context.Background(), account, "", "2026-08-10", "2026-08-16")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if httpResp == nil || httpResp.StatusCode != http.StatusOK {
		t.Fatalf("http response = %#v", httpResp)
	}
	if len(resp.Data) != 2 || resp.Units != "percent" {
		t.Fatalf("parsed = %#v", resp)
	}
	if gotQuery["start_date"][0] != "2026-08-10" || gotQuery["end_date"][0] != "2026-08-16" || gotQuery["group_by"][0] != "day" {
		t.Fatalf("query = %v", gotQuery)
	}
	// counts 端点带 workspace_user，拆分端点不带；混进去不会报错，但别把两者的形态搅在一起。
	if _, ok := gotQuery["workspace_user"]; ok {
		t.Fatalf("breakdown request must not carry workspace_user: %v", gotQuery)
	}
	if gotHeader.Get("Authorization") != "Bearer at-123" {
		t.Fatalf("authorization = %q", gotHeader.Get("Authorization"))
	}
	if gotHeader.Get("chatgpt-account-id") != "acc-1" {
		t.Fatalf("chatgpt-account-id = %q", gotHeader.Get("chatgpt-account-id"))
	}
	if gotHeader.Get("Originator") != Originator {
		t.Fatalf("originator = %q", gotHeader.Get("Originator"))
	}
}

func TestQueryWhamDailyTokenBreakdownReturnsUnreadResponseOnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer server.Close()
	restore := SetWhamDailyTokenBreakdownURLForTest(server.URL)
	defer restore()

	account := &auth.Account{DBID: 1, AccessToken: "at-123"}
	resp, httpResp, err := QueryWhamDailyTokenBreakdown(context.Background(), account, "", "2026-08-10", "2026-08-16")
	if err == nil || resp != nil {
		t.Fatalf("expected error on 429, got resp=%#v err=%v", resp, err)
	}
	if httpResp == nil || httpResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("http response = %#v", httpResp)
	}
	// body 留给调用方分类错误，不能在这里读掉。
	body, _ := io.ReadAll(httpResp.Body)
	_ = httpResp.Body.Close()
	if string(body) != "slow down" {
		t.Fatalf("body = %q, want unread upstream body", body)
	}
}

// 「今天」这一行的形态上游改过两次（先是没 token 字段，后来有 token 但没 models），
// 按字段有无判断已不可靠；结算与否只看日期是否早于今天。
func TestWhamDailyUsageDaySettledOnUsesDate(t *testing.T) {
	tokens := int64(18641788)
	today := WhamDailyUsageDay{Date: "2026-09-05", Totals: WhamDailyUsageCounts{Credits: 54.35, TextTotalTokens: &tokens}}
	if today.SettledOn("2026-09-05") {
		t.Fatal("today's row carries tokens but must stay unsettled")
	}
	if !today.SettledOn("2026-09-06") {
		t.Fatal("same row is settled once the date has passed")
	}
	noTokens := WhamDailyUsageDay{Date: "2026-09-04", Totals: WhamDailyUsageCounts{Credits: 1}}
	if noTokens.SettledOn("2026-09-06") {
		t.Fatal("a past row without token detail is not settled")
	}
	if (WhamDailyUsageDay{}).SettledOn("2026-09-06") {
		t.Fatal("empty date is never settled")
	}
}

// 深回补窗口按已证实的上游深度（84 天，含今天）取值。
func TestWhamDailyUsageBackfillWindow(t *testing.T) {
	now := time.Date(2026, 9, 6, 1, 0, 0, 0, time.UTC)
	start, end := WhamDailyUsageBackfillWindow(now)
	if end != "2026-09-06" {
		t.Fatalf("end = %s", end)
	}
	if start != "2026-06-15" {
		t.Fatalf("start = %s, want 2026-06-15 (84 days inclusive)", start)
	}
}
