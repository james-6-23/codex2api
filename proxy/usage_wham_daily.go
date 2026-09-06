package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// WhamDailyUsageURL 是 ChatGPT 后端按天返回工作区用量统计的端点。
// 与 wham/usage 一样零额度成本，但给的是官方结算口径的绝对值（credits 与 token 数），
// 且能按客户端（CLI / Desktop / IDE / 网关流量）与模型拆分。
//
// 上游可回溯的深度实测至少 84 天（2026-09-06 用 plus 号查到 12 周前；2026-08 首次
// 接入时只回 7 天）。稳态同步仍只滚动回补最近 WhamDailyUsageRetentionDays 天，
// 首次同步（本地没有任何快照）用 WhamDailyUsageBackfillDays 深回补一次；
// 更久的历史仍然只存在于本地库。
const WhamDailyUsageURL = "https://chatgpt.com/backend-api/wham/analytics/daily-workspace-usage-counts"

// WhamDailyUsageRetentionDays 是稳态同步每轮回补的滚动窗口（含当天）。当天与前几天
// 的数值会随结算变化，每轮整窗覆盖顺带修复漏跑的空洞。名字沿用「保留期」是历史
// 原因：2026-08 接入时上游确实只回 7 天。
const WhamDailyUsageRetentionDays = 7

// WhamDailyUsageBackfillDays 是首次同步的深回补窗口。2026-09-06 实测 plus 号能查到
// 抓包前 84 天（恰为 12 周、且起点是周日，疑似「最近 12 周」截断）；更早是否可查
// 未验证，所以按已证实的深度取值。
const WhamDailyUsageBackfillDays = 84

// WhamCreditsPerUSD 是官方 credits 与美元的换算比例（1 美元 = 25 credits）。
// 2026-09-06 实测：单模型已结算日的 token × 官方目录价 恰等于 credits/25，
// 即 credits 就是按当时目录价计的 API 等价成本。
const WhamCreditsPerUSD = 25.0

// whamDailyUsageURLForTest 允许测试替换默认 URL。生产代码不要赋值。
var whamDailyUsageURLForTest = ""

// SetWhamDailyUsageURLForTest 供测试替换按天用量端点 URL，返回恢复函数。
// 生产代码不要调用。
func SetWhamDailyUsageURLForTest(u string) (restore func()) {
	old := whamDailyUsageURLForTest
	whamDailyUsageURLForTest = u
	return func() { whamDailyUsageURLForTest = old }
}

// WhamDailyUsageResponse 是 daily-workspace-usage-counts 的响应结构。
type WhamDailyUsageResponse struct {
	Data    []WhamDailyUsageDay `json:"data"`
	GroupBy string              `json:"group_by"`
}

// WhamDailyUsageDay 是单个统计周期（group_by=day 时为一天，week 时为周起始日）。
// 这个端点是稀疏的：没有消耗的日子不返回行。
type WhamDailyUsageDay struct {
	Date    string                 `json:"date"`
	Totals  WhamDailyUsageCounts   `json:"totals"`
	Clients []WhamDailyUsageClient `json:"clients"`
	Models  []WhamDailyUsageModel  `json:"models"`
}

// SettledOn 判断这一天的记录是否已结算：日期早于 today（UTC 日期，YYYY-MM-DD）且
// 带 token 明细。上游对「今天」这一行的形态改过（见 WhamDailyUsageCounts），按字段
// 有无判断已不可靠，按日期判断才稳：今天的数值一定还在变，昨天及更早的在下一轮
// 整窗回补时被覆盖。
func (d WhamDailyUsageDay) SettledOn(today string) bool {
	date := strings.TrimSpace(d.Date)
	return date != "" && date < strings.TrimSpace(today) && d.Totals.HasTokenDetail()
}

// WhamDailyUsageCounts 是一组用量计数。
//
// 当天（未结算）记录的形态上游改过：2026-08 时只有 users/threads/turns/credits、
// 四个 token 字段整个缺失；2026-09 实测反过来，token 与 credits 都在，缺的是 models
// 键且 users/threads/turns 全 0。token 字段保留指针区分「上游没给」与「给了 0」，
// 但「是否已结算」不能再靠字段有无判断，见 WhamDailyUsageDay.SettledOn。
type WhamDailyUsageCounts struct {
	Users   int     `json:"users"`
	Threads int     `json:"threads"`
	Turns   int     `json:"turns"`
	Credits float64 `json:"credits"`

	UncachedTextInputTokens *int64 `json:"uncached_text_input_tokens,omitempty"`
	CachedTextInputTokens   *int64 `json:"cached_text_input_tokens,omitempty"`
	TextOutputTokens        *int64 `json:"text_output_tokens,omitempty"`
	TextTotalTokens         *int64 `json:"text_total_tokens,omitempty"`
}

// HasTokenDetail 表示这条记录带有 token 明细字段。只说明字段存在，不代表已结算：
// 当天的记录现在也会带 token 且全天持续变化，结算判定用 WhamDailyUsageDay.SettledOn。
func (c WhamDailyUsageCounts) HasTokenDetail() bool {
	return c.TextTotalTokens != nil
}

// USD 按官方 1 美元 = 25 credits 折算本条记录的成本。
func (c WhamDailyUsageCounts) USD() float64 {
	return c.Credits / WhamCreditsPerUSD
}

// WhamDailyUsageClient 是按客户端入口拆分的用量。client_id 取值实测包括
// CODEX_CLI / CODEX_DESKTOP_APP / CODEX_IDE_VSCODE / CODEX_WORK_DESKTOP /
// CODEX_SERVICE_EXEC / CODEX_SDK_TS / CODEX_WEB，以及没有官方客户端标识的
// CODEX_UNKNOWN_DEFAULT（走本网关的流量落在这一档）。
type WhamDailyUsageClient struct {
	ClientID string `json:"client_id"`
	WhamDailyUsageCounts
}

// WhamDailyUsageModel 是按模型拆分的用量。实测上游在这一层不返回 token 明细，
// credits 也恒为 0，只有 users/threads/turns 有值。模型维度的成本要用
// daily-token-usage-breakdown 的份额分摊（见 usage_wham_breakdown.go）。
type WhamDailyUsageModel struct {
	Model string `json:"model"`
	WhamDailyUsageCounts
}

// QueryWhamDailyUsage 拉取 [startDate, endDate] 区间的按天用量统计，日期格式
// YYYY-MM-DD。零额度成本，与 QueryWhamUsage 同款请求形态。
// 非 200 时返回 resp（body 未读）供调用方处理。
func QueryWhamDailyUsage(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (*WhamDailyUsageResponse, *http.Response, error) {
	base := WhamDailyUsageURL
	if whamDailyUsageURLForTest != "" {
		base = whamDailyUsageURLForTest
	}
	return queryWhamDailyUsageWithURL(ctx, account, proxyURL, base, startDate, endDate)
}

// WhamDailyUsageWindow 返回稳态同步的滚动区间（含今天，UTC 日期）。
func WhamDailyUsageWindow(now time.Time) (startDate, endDate string) {
	return whamDailyUsageWindowDays(now, WhamDailyUsageRetentionDays)
}

// WhamDailyUsageBackfillWindow 返回首次同步的深回补区间（含今天，UTC 日期）。
func WhamDailyUsageBackfillWindow(now time.Time) (startDate, endDate string) {
	return whamDailyUsageWindowDays(now, WhamDailyUsageBackfillDays)
}

func whamDailyUsageWindowDays(now time.Time, days int) (startDate, endDate string) {
	if days <= 0 {
		days = 1
	}
	end := now.UTC()
	start := end.AddDate(0, 0, -(days - 1))
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

func queryWhamDailyUsageWithURL(ctx context.Context, account *auth.Account, proxyURL, base, startDate, endDate string) (*WhamDailyUsageResponse, *http.Response, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, nil, fmt.Errorf("account has no access token")
	}
	if startDate == "" || endDate == "" {
		return nil, nil, fmt.Errorf("daily usage requires start_date and end_date")
	}

	query := url.Values{}
	query.Set("start_date", startDate)
	query.Set("end_date", endDate)
	query.Set("group_by", "day")
	query.Set("workspace_user", "true")
	requestURL := base + "?" + query.Encode()

	// resinMaintenanceTarget 经 RequestURI() 重建目标，query 会被保留。
	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, requestURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build daily usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", MinimalCodexCLIUserAgentForHeaders())
	req.Header.Set("Originator", Originator)
	// 与 wham 用量查询一致：自定义头覆盖工作区 ID 时，统计必须查覆盖后的空间，
	// 否则拿到的是与实际流量不同的空间。
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	client := whamHTTPClient(req, account, resinClient, viaResin, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("daily usage request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp, fmt.Errorf("daily usage returned status %d", resp.StatusCode)
	}

	// 稀疏行 × 客户端与模型拆分，实测 plus 号 84 天约 77KB；上限给到 1MB 足够冗余。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, resp, fmt.Errorf("read daily usage response: %w", err)
	}

	var out WhamDailyUsageResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, resp, fmt.Errorf("parse daily usage response: %w", err)
	}
	return &out, resp, nil
}
