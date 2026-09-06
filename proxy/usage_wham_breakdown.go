package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/codex2api/auth"
)

// WhamDailyTokenBreakdownURL 是 ChatGPT 后端按天、按模型×速度拆分用量的端点。
// 与 daily-workspace-usage-counts 互补：counts 在 models 维度不给成本（credits 恒 0），
// 这个端点给的是每个 (model, speed) 以及每个产品入口（cli / desktop_app / …）的
// 相对占比，两者按日期 join 后就能把当天的官方 credits 分摊到模型与速度档。
//
// 关键语义（2026-09-06 用 plus 与 free 两号实测）：响应 units 为 "percent"，但这个
// 百分比**不是**周额度的占比，而是按「本次请求区间 + 分组内的峰值桶」归一化：
// 区间内消耗最大的那一天恒等于 100.0，其余按比例缩放。同一账号查两段不同区间，
// 同一天的数值会不同。因此这里的数值只能在同一天内部当份额用（模型 A 占当天的
// 多少），绝不能跨天累加、也不能拿去反推额度大小。
const WhamDailyTokenBreakdownURL = "https://chatgpt.com/backend-api/wham/usage/daily-token-usage-breakdown"

// WhamSpeedStandard / WhamSpeedFast 是 speed 字段实测出现过的两个取值。
// free 号只有 standard 行；plus 号两档都有，fast 对应 Responses 的 priority 档。
const (
	WhamSpeedStandard = "standard"
	WhamSpeedFast     = "fast"
)

// whamDailyTokenBreakdownURLForTest 允许测试替换默认 URL。生产代码不要赋值。
var whamDailyTokenBreakdownURLForTest = ""

// SetWhamDailyTokenBreakdownURLForTest 供测试替换拆分端点 URL，返回恢复函数。
// 生产代码不要调用。
func SetWhamDailyTokenBreakdownURLForTest(u string) (restore func()) {
	old := whamDailyTokenBreakdownURLForTest
	whamDailyTokenBreakdownURLForTest = u
	return func() { whamDailyTokenBreakdownURLForTest = old }
}

// WhamDailyTokenBreakdownResponse 是 daily-token-usage-breakdown 的响应结构。
type WhamDailyTokenBreakdownResponse struct {
	Data []WhamDailyTokenBreakdownDay `json:"data"`
	// Units 实测恒为 "percent"。保留下来是为了在上游改口径时能被测试和日志发现。
	Units   string `json:"units"`
	GroupBy string `json:"group_by"`
}

// WhamDailyTokenBreakdownDay 是一天的拆分。
//
// 与 counts 不同，这个端点是**稠密**的：请求区间内每一天都有一行，没有消耗的日子
// 全部为 0，甚至 end_date 落在未来时未来日期也会以全 0 出现。models 列表是该区间
// 内出现过的 (model, speed) 目录并集，每天重复一遍，未使用的行 credits=0。
type WhamDailyTokenBreakdownDay struct {
	Date   string                         `json:"date"`
	Models []WhamDailyTokenBreakdownModel `json:"models"`
	// Surfaces 是按产品入口的占比，键实测固定 17 个（cli / vscode / web / work_web /
	// mobile / work_mobile / slack / linear / jetbrains / sdk / exec / github /
	// desktop_app / work_desktop / github_code_review / agent_identity / unknown），
	// 与 counts.clients 的 client_id 按名字一一对应（cli↔CODEX_CLI 等）。
	// 每天 sum(Surfaces) == sum(Models.Percent)，两者是同一总量的两种切分。
	Surfaces map[string]float64 `json:"product_surface_usage_values"`
}

// WhamDailyTokenBreakdownModel 是一个 (model, speed) 的占比。
//
// 上游字段名叫 credits，但配合 units=percent 它是归一化百分比而不是 credits 数，
// 这里改名为 Percent 避免与 counts 里真正的 credits 混淆。
type WhamDailyTokenBreakdownModel struct {
	Model   string  `json:"model"`
	Speed   string  `json:"speed"`
	Percent float64 `json:"credits"`
}

// TotalPercent 返回当天所有 (model, speed) 占比之和，即当天在本次响应尺度下的总量。
// 用它做分母就能把每个模型换成当天内部的份额（0~1）。
func (d WhamDailyTokenBreakdownDay) TotalPercent() float64 {
	total := 0.0
	for _, m := range d.Models {
		if m.Percent > 0 {
			total += m.Percent
		}
	}
	return total
}

// ActiveModels 返回当天占比大于 0 的 (model, speed) 行，speed 缺省补 standard，
// 按占比降序、同占比按名字排序，保证落库 JSON 稳定。
func (d WhamDailyTokenBreakdownDay) ActiveModels() []WhamDailyTokenBreakdownModel {
	out := make([]WhamDailyTokenBreakdownModel, 0, len(d.Models))
	for _, m := range d.Models {
		if m.Percent <= 0 {
			continue
		}
		model := strings.TrimSpace(m.Model)
		if model == "" {
			continue
		}
		speed := strings.ToLower(strings.TrimSpace(m.Speed))
		if speed == "" {
			speed = WhamSpeedStandard
		}
		out = append(out, WhamDailyTokenBreakdownModel{Model: model, Speed: speed, Percent: m.Percent})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Percent != out[j].Percent {
			return out[i].Percent > out[j].Percent
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Speed < out[j].Speed
	})
	return out
}

// ActiveSurfaces 返回当天占比大于 0 的产品入口。
func (d WhamDailyTokenBreakdownDay) ActiveSurfaces() map[string]float64 {
	out := make(map[string]float64, len(d.Surfaces))
	for key, value := range d.Surfaces {
		name := strings.TrimSpace(key)
		if name == "" || value <= 0 {
			continue
		}
		out[name] = value
	}
	return out
}

// QueryWhamDailyTokenBreakdown 拉取 [startDate, endDate] 区间按天的模型×速度拆分，
// 日期格式 YYYY-MM-DD。零额度成本，请求形态与 QueryWhamDailyUsage 一致。
// 非 200 时返回 resp（body 未读）供调用方处理。
func QueryWhamDailyTokenBreakdown(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (*WhamDailyTokenBreakdownResponse, *http.Response, error) {
	base := WhamDailyTokenBreakdownURL
	if whamDailyTokenBreakdownURLForTest != "" {
		base = whamDailyTokenBreakdownURLForTest
	}
	return queryWhamDailyTokenBreakdownWithURL(ctx, account, proxyURL, base, startDate, endDate)
}

func queryWhamDailyTokenBreakdownWithURL(ctx context.Context, account *auth.Account, proxyURL, base, startDate, endDate string) (*WhamDailyTokenBreakdownResponse, *http.Response, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, nil, fmt.Errorf("account has no access token")
	}
	if startDate == "" || endDate == "" {
		return nil, nil, fmt.Errorf("token breakdown requires start_date and end_date")
	}

	query := url.Values{}
	query.Set("start_date", startDate)
	query.Set("end_date", endDate)
	// 不传 group_by 时上游也按 day 返回；显式写上避免默认值变动。
	// 不要用 group_by=week：周桶按周日起算且另行归一化，与账号的重置周期无关。
	query.Set("group_by", "day")
	requestURL := base + "?" + query.Encode()

	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, requestURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build token breakdown request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", MinimalCodexCLIUserAgentForHeaders())
	req.Header.Set("Originator", Originator)
	// 与 counts 一致：自定义头覆盖工作区 ID 时查覆盖后的空间。个人号 account_id
	// 为空，不带这个头上游同样正常返回。
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	client := whamHTTPClient(req, account, resinClient, viaResin, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("token breakdown request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp, fmt.Errorf("token breakdown returned status %d", resp.StatusCode)
	}

	// 稠密行 × 14 个模型目录项 × 17 个入口键，实测 plus 号 47 天约 100KB（美化过的
	// JSON）；84 天深回补约 200KB。上限给 4MB 足够冗余。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, resp, fmt.Errorf("read token breakdown response: %w", err)
	}

	var out WhamDailyTokenBreakdownResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, resp, fmt.Errorf("parse token breakdown response: %w", err)
	}
	return &out, resp, nil
}
