package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// Grok billing 端点（cli-chat-proxy，与官方 Grok CLI 对齐）。
const (
	grokBillingWeeklyPath  = "/billing?format=credits"
	grokBillingMonthlyPath = "/billing"
	grokSuperGrokCents     = 15_000   // $150
	grokSuperGrokHeavyCents = 150_000 // $1,500
)

// GrokProductUsage 是周额度内单个产品（如 GrokBuild / Api）的用量。
type GrokProductUsage struct {
	Product      string   `json:"product"`
	UsagePercent *float64 `json:"usage_percent,omitempty"`
}

// GrokBillingSummary 是 Grok 周/月额度的合并视图，供列表展示。
type GrokBillingSummary struct {
	Plan              string
	WeeklyPercent     *float64
	WeeklyPeriodStart string
	WeeklyPeriodEnd   string
	ProductUsage      []GrokProductUsage
	OnDemandCapCents  *float64
	OnDemandUsedCents *float64
	MonthlyLimitCents *float64
	MonthlyUsedCents  *float64
	MonthlyPercent    *float64
	MonthlyPeriodStart string
	MonthlyPeriodEnd   string
}

// GrokBillingDetail 是落库/透出给前端的完整额度视图（grok_billing_detail 凭据）。
type GrokBillingDetail struct {
	Plan               string             `json:"plan,omitempty"`
	WeeklyPercent      *float64           `json:"weekly_percent,omitempty"`
	WeeklyPeriodStart  string             `json:"weekly_period_start,omitempty"`
	WeeklyPeriodEnd    string             `json:"weekly_period_end,omitempty"`
	ProductUsage       []GrokProductUsage `json:"product_usage,omitempty"`
	OnDemandCapCents   *float64           `json:"on_demand_cap_cents,omitempty"`
	OnDemandUsedCents  *float64           `json:"on_demand_used_cents,omitempty"`
	MonthlyLimitCents  *float64           `json:"monthly_limit_cents,omitempty"`
	MonthlyUsedCents   *float64           `json:"monthly_used_cents,omitempty"`
	MonthlyPercent     *float64           `json:"monthly_percent,omitempty"`
	MonthlyPeriodStart string             `json:"monthly_period_start,omitempty"`
	MonthlyPeriodEnd   string             `json:"monthly_period_end,omitempty"`
	UpdatedAt          string             `json:"updated_at,omitempty"`
}

type grokBillingPayload struct {
	Config *grokBillingConfig `json:"config"`
}

type grokBillingConfig struct {
	CurrentPeriod      *struct {
		Type  string `json:"type"`
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"currentPeriod"`
	CreditUsagePercent *float64 `json:"creditUsagePercent"`
	ProductUsage       []struct {
		Product      string          `json:"product"`
		UsagePercent json.RawMessage `json:"usagePercent"`
	} `json:"productUsage"`
	MonthlyLimit       json.RawMessage `json:"monthlyLimit"`
	Used               json.RawMessage `json:"used"`
	OnDemandCap        json.RawMessage `json:"onDemandCap"`
	OnDemandUsed       json.RawMessage `json:"onDemandUsed"`
	BillingPeriodStart string          `json:"billingPeriodStart"`
	BillingPeriodEnd   string          `json:"billingPeriodEnd"`
}

// FetchGrokBilling 拉取 Grok 账号的周额度 + 月额度（chat-proxy /v1/billing）。
func FetchGrokBilling(ctx context.Context, account *auth.Account, proxyURL string) (*GrokBillingSummary, error) {
	if account == nil || !account.IsGrokAPI() {
		return nil, fmt.Errorf("not a grok account")
	}
	baseURL, bearer := account.GrokCredentials()
	if bearer == "" {
		return nil, fmt.Errorf("grok 账号缺少 access token")
	}
	// billing 只在 chat-proxy 上有完整套餐视图；API Key 账号若走 api.x.ai 也尝试同一 path。
	if baseURL == "" || strings.Contains(baseURL, "api.x.ai") {
		// OAuth 默认 chat-proxy；纯 API Key 也尝试 chat-proxy 拿不到就只试 baseURL。
		if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
			baseURL = auth.GrokDefaultChatProxyBaseURL
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")

	client := getPooledClient(account, proxyURL)
	weekly, weeklyErr := fetchGrokBillingOnce(ctx, client, account, bearer, baseURL+grokBillingWeeklyPath)
	monthly, monthlyErr := fetchGrokBillingOnce(ctx, client, account, bearer, baseURL+grokBillingMonthlyPath)
	if weeklyErr != nil && monthlyErr != nil {
		return nil, fmt.Errorf("billing 探针失败: weekly=%v monthly=%v", weeklyErr, monthlyErr)
	}

	summary := &GrokBillingSummary{}
	if weekly != nil && weekly.Config != nil {
		cfg := weekly.Config
		if cfg.CreditUsagePercent != nil {
			v := *cfg.CreditUsagePercent
			summary.WeeklyPercent = &v
		}
		if cfg.CurrentPeriod != nil {
			summary.WeeklyPeriodStart = strings.TrimSpace(cfg.CurrentPeriod.Start)
			summary.WeeklyPeriodEnd = strings.TrimSpace(cfg.CurrentPeriod.End)
		}
		if summary.WeeklyPeriodStart == "" {
			summary.WeeklyPeriodStart = strings.TrimSpace(cfg.BillingPeriodStart)
		}
		if summary.WeeklyPeriodEnd == "" {
			summary.WeeklyPeriodEnd = strings.TrimSpace(cfg.BillingPeriodEnd)
		}
		summary.ProductUsage = parseGrokProductUsage(cfg)
	}
	if monthly != nil && monthly.Config != nil {
		cfg := monthly.Config
		summary.MonthlyLimitCents = parseGrokCentValue(cfg.MonthlyLimit)
		summary.MonthlyUsedCents = parseGrokCentValue(cfg.Used)
		summary.OnDemandCapCents = parseGrokCentValue(cfg.OnDemandCap)
		summary.OnDemandUsedCents = parseGrokCentValue(cfg.OnDemandUsed)
		summary.MonthlyPeriodStart = strings.TrimSpace(cfg.BillingPeriodStart)
		summary.MonthlyPeriodEnd = strings.TrimSpace(cfg.BillingPeriodEnd)
		if summary.MonthlyLimitCents != nil && *summary.MonthlyLimitCents > 0 && summary.MonthlyUsedCents != nil {
			used := math.Min(*summary.MonthlyUsedCents, *summary.MonthlyLimitCents)
			pct := (used / *summary.MonthlyLimitCents) * 100
			summary.MonthlyPercent = &pct
		}
		// 超出月度包含额度的部分记为按量付费用量（上游未显式给出时推导）。
		if summary.OnDemandUsedCents == nil &&
			summary.MonthlyUsedCents != nil && summary.MonthlyLimitCents != nil &&
			*summary.MonthlyUsedCents > *summary.MonthlyLimitCents {
			over := *summary.MonthlyUsedCents - *summary.MonthlyLimitCents
			summary.OnDemandUsedCents = &over
		}
		if len(summary.ProductUsage) == 0 {
			summary.ProductUsage = parseGrokProductUsage(cfg)
		}
		summary.Plan = resolveGrokPlan(summary.MonthlyLimitCents)
	}
	// weekly 载荷也可能带 onDemand 字段，作为兜底。
	if summary.OnDemandCapCents == nil && weekly != nil && weekly.Config != nil {
		summary.OnDemandCapCents = parseGrokCentValue(weekly.Config.OnDemandCap)
		if summary.OnDemandUsedCents == nil {
			summary.OnDemandUsedCents = parseGrokCentValue(weekly.Config.OnDemandUsed)
		}
	}
	return summary, nil
}

func parseGrokProductUsage(cfg *grokBillingConfig) []GrokProductUsage {
	if cfg == nil || len(cfg.ProductUsage) == 0 {
		return nil
	}
	out := make([]GrokProductUsage, 0, len(cfg.ProductUsage))
	for _, item := range cfg.ProductUsage {
		product := strings.TrimSpace(item.Product)
		if product == "" {
			continue
		}
		out = append(out, GrokProductUsage{
			Product:      product,
			UsagePercent: parseGrokCentValue(item.UsagePercent),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func fetchGrokBillingOnce(ctx context.Context, client *http.Client, account *auth.Account, bearer, url string) (*grokBillingPayload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 复用 Grok CLI 头契约；billing GET 也需要 x-xai-token-auth。
	applyGrokRequestHeaders(req, account, bearer, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Del("Accept") // re-set clean
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized: %s", truncateRunes(string(body), 200))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncateRunes(string(body), 200))
	}
	var payload grokBillingPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse billing: %w", err)
	}
	return &payload, nil
}

// ApplyGrokBilling 把 billing 摘要写入账号运行时字段（plan + 周/月用量），
// 并返回需落库的 credentials 增量。
func ApplyGrokBilling(store *auth.Store, account *auth.Account, summary *GrokBillingSummary) map[string]interface{} {
	if account == nil || summary == nil {
		return nil
	}
	now := time.Now()
	credentials := map[string]interface{}{}

	if plan := strings.TrimSpace(summary.Plan); plan != "" {
		account.Mu().Lock()
		account.PlanType = plan
		account.Mu().Unlock()
		credentials["plan_type"] = plan
	}

	// 周额度映射到 5h 字段位（前端进度条复用）；月额度映射到 7d。
	if summary.WeeklyPercent != nil {
		resetAt := parseGrokTime(summary.WeeklyPeriodEnd)
		account.SetUsageSnapshot5hAt(*summary.WeeklyPercent, resetAt, now)
	}
	if summary.MonthlyPercent != nil {
		account.SetUsageSnapshot(*summary.MonthlyPercent, now)
		if end := parseGrokTime(summary.MonthlyPeriodEnd); !end.IsZero() {
			account.SetReset7dAt(end)
		}
	}

	if store != nil {
		store.ReportRequestSuccess(account, 0)
		// 成功探针清除 unauthorized/error 冷却
		store.ClearCooldown(account)
	}

	if summary.WeeklyPercent != nil {
		credentials["grok_weekly_usage_percent"] = *summary.WeeklyPercent
	}
	if summary.WeeklyPeriodEnd != "" {
		credentials["grok_weekly_period_end"] = summary.WeeklyPeriodEnd
	}
	if summary.MonthlyPercent != nil {
		credentials["grok_monthly_usage_percent"] = *summary.MonthlyPercent
	}
	if summary.MonthlyLimitCents != nil {
		credentials["grok_monthly_limit_cents"] = *summary.MonthlyLimitCents
	}
	if summary.MonthlyUsedCents != nil {
		credentials["grok_monthly_used_cents"] = *summary.MonthlyUsedCents
	}
	if summary.MonthlyPeriodEnd != "" {
		credentials["grok_monthly_period_end"] = summary.MonthlyPeriodEnd
	}
	if summary.WeeklyPercent != nil || summary.MonthlyPercent != nil {
		credentials["grok_usage_updated_at"] = now.UTC().Format(time.RFC3339)
	}
	// 完整额度视图（产品用量、按量付费、月度金额）单独落一个 JSON 凭据，
	// 供账号列表透出给前端渲染。
	detail := &GrokBillingDetail{
		Plan:               strings.TrimSpace(summary.Plan),
		WeeklyPercent:      summary.WeeklyPercent,
		WeeklyPeriodStart:  summary.WeeklyPeriodStart,
		WeeklyPeriodEnd:    summary.WeeklyPeriodEnd,
		ProductUsage:       summary.ProductUsage,
		OnDemandCapCents:   summary.OnDemandCapCents,
		OnDemandUsedCents:  summary.OnDemandUsedCents,
		MonthlyLimitCents:  summary.MonthlyLimitCents,
		MonthlyUsedCents:   summary.MonthlyUsedCents,
		MonthlyPercent:     summary.MonthlyPercent,
		MonthlyPeriodStart: summary.MonthlyPeriodStart,
		MonthlyPeriodEnd:   summary.MonthlyPeriodEnd,
		UpdatedAt:          now.UTC().Format(time.RFC3339),
	}
	if detailJSON, err := json.Marshal(detail); err == nil {
		credentials["grok_billing_detail"] = string(detailJSON)
	}
	return credentials
}

// resolveGrokPlan 从月度包含额度推断套餐名。仅在 billing 月度配置成功返回时被调用
// （见 FetchGrokBilling 里的 monthly.Config != nil 分支），因此月度额度为 0 或字段缺失
// 即代表"没有付费月度额度" = 免费档；付费档按已知额度精确匹配，未知非零额度不臆断
// （返回空，交由上层保留占位，避免把未识别的付费档误标成 free）。
func resolveGrokPlan(monthlyLimitCents *float64) string {
	if monthlyLimitCents == nil {
		return "free"
	}
	switch math.Round(*monthlyLimitCents) {
	case grokSuperGrokCents:
		return "SuperGrok"
	case grokSuperGrokHeavyCents:
		return "SuperGrok Heavy"
	case 0:
		return "free"
	default:
		return ""
	}
}

func parseGrokCentValue(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var obj struct {
		Val any `json:"val"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Val != nil {
		return anyToFloat64(obj.Val)
	}
	var n any
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil
	}
	return anyToFloat64(n)
}

func anyToFloat64(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case float32:
		f := float64(n)
		return &f
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return nil
		}
		return &f
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		return &f
	default:
		return nil
	}
}

func parseGrokTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
