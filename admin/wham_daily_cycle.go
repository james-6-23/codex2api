package admin

import (
	"context"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// whamDailyCycleReliablePercent 是额度估算被视为可靠的最低已用百分比。
// 实时百分比是整数，±0.5% 的舍入在 10% 时已经是约 ±5% 的区间，再低就没有参考价值。
const whamDailyCycleReliablePercent = 10

// whamDailyCycleMinPercent 是能反推的最低百分比：低于 0.5% 时舍入下界为 0，除不出来。
const whamDailyCycleMinPercent = 0.5

// whamDailyCycleInput 是估算所需的账号窗口快照与本周期官方成本。
//
// 两个上游数据都给不出周额度的绝对值（拆分端点的 percent 是区间峰值归一化，见
// usage_wham_breakdown.go），唯一诚实的推法是：本周期已用官方 credits 折美元 ÷
// wham/usage 的实时已用百分比。它的误差全部来自百分比只有整数精度。
type whamDailyCycleInput struct {
	ResetAt        time.Time
	WindowSeconds  int64
	WindowKind     string
	UsedPercent    float64
	UsedPercentOK  bool
	PercentUpdated time.Time
	// UsedCredits / Days 是周期起始日（UTC 整天）起所有带 counts 数据的快照合计。
	// 起始日按整天计入：周期通常在一天中间开始，那一天里周期开始前的消耗也会被算进来，
	// 这部分偏差远小于百分比舍入带来的区间。
	UsedCredits float64
	Days        int
}

// buildWhamDailyCycle 组装账号当前重置周期的成本与额度估算。窗口信息来自账号在
// 内存里的用量快照（响应头与 wham/usage 探针都会刷新），不额外打上游。
func (h *Handler) buildWhamDailyCycle(ctx context.Context, account *auth.Account, now time.Time) gin.H {
	if h == nil || h.db == nil || account == nil {
		return nil
	}
	snap := account.GetAccountListRuntimeSnapshot()
	windowSeconds := snap.Window7dSeconds
	if windowSeconds <= 0 {
		// 长窗口长度未知时按 7 天：plus/pro 默认周窗；free/team 月窗在探针刷新后会带上真实长度。
		windowSeconds = int64((7 * 24 * time.Hour) / time.Second)
	}
	in := whamDailyCycleInput{
		ResetAt:        snap.Reset7dAt,
		WindowSeconds:  windowSeconds,
		WindowKind:     account.Window7dKind(),
		UsedPercent:    snap.UsagePercent7d,
		UsedPercentOK:  snap.UsagePercent7dValid,
		PercentUpdated: account.GetUsageUpdatedAt(),
	}
	if !snap.Reset7dAt.IsZero() && snap.Reset7dAt.After(now) {
		startDay := snap.Reset7dAt.Add(-time.Duration(windowSeconds) * time.Second).UTC().Format("2006-01-02")
		if credits, days, err := h.db.SumAccountDailyUsageSince(ctx, account.DBID, startDay); err == nil {
			in.UsedCredits, in.Days = credits, days
		}
	}
	return projectWhamDailyCycle(in, now)
}

// projectWhamDailyCycle 把窗口快照与本周期成本投影成响应。available 表示窗口与实时
// 百分比都拿到了；estimate 只在能除得出来时给出，否则 reason 说明原因。
func projectWhamDailyCycle(in whamDailyCycleInput, now time.Time) gin.H {
	out := gin.H{
		"available":      false,
		"used_credits":   in.UsedCredits,
		"used_usd":       in.UsedCredits / proxy.WhamCreditsPerUSD,
		"days":           in.Days,
		"window_seconds": in.WindowSeconds,
		"window_kind":    in.WindowKind,
	}
	switch {
	case in.ResetAt.IsZero():
		out["reason"] = "no_window"
		return out
	case !in.ResetAt.After(now):
		// 重置时间已过而快照没刷新：周期起点算不准，等下一次用量刷新。
		out["reason"] = "window_stale"
		return out
	}
	window := time.Duration(in.WindowSeconds) * time.Second
	out["start_at"] = in.ResetAt.Add(-window).UTC()
	out["reset_at"] = in.ResetAt.UTC()
	if !in.UsedPercentOK {
		out["reason"] = "no_percent"
		return out
	}
	out["available"] = true
	out["used_percent"] = in.UsedPercent
	if !in.PercentUpdated.IsZero() {
		out["used_percent_updated_at"] = in.PercentUpdated.UTC()
	}
	switch {
	case in.UsedCredits <= 0:
		// free 号 credits 恒 0；付费号周期刚开始时官方统计也可能还没出数。
		out["reason"] = "no_credits"
	case in.UsedPercent < whamDailyCycleMinPercent:
		out["reason"] = "percent_too_low"
	default:
		usedUSD := in.UsedCredits / proxy.WhamCreditsPerUSD
		percent := in.UsedPercent
		out["estimate"] = gin.H{
			"usd":      usedUSD / (percent / 100),
			"usd_low":  usedUSD / ((percent + 0.5) / 100),
			"usd_high": usedUSD / ((percent - 0.5) / 100),
			"reliable": percent >= whamDailyCycleReliablePercent,
		}
	}
	return out
}
