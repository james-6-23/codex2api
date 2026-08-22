package admin

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type accountPageStatsItem struct {
	Usage5hDetail *accountUsageWindow `json:"usage_5h_detail,omitempty"`
	Usage7dDetail *accountUsageWindow `json:"usage_7d_detail,omitempty"`
	// UsageTodayDetail 是"今日"(服务器时区当天 0 点起)的网关侧聚合。
	// 与 5h/7d 不同,这个字段对每个请求到的账号都必下发:零值代表
	// "今天没跑过",缺字段才代表"还没加载",前端据此区分 0 和占位。
	UsageTodayDetail *accountUsageWindow `json:"usage_today_detail,omitempty"`
	Billed5h         *float64            `json:"billed_5h,omitempty"`
	Billed7d         *float64            `json:"billed_7d,omitempty"`
	// OfficialUSD7d 是官方结算口径的近 7 天成本，与上面按本地日志算的
	// Billed7d 是两套账：网关只看得到自己转发的请求，官方账单还含用户直接
	// 用官方客户端的消耗。读的是快照表，不打上游。
	OfficialUSD7d *float64 `json:"official_usd_7d,omitempty"`
	// OfficialUsageSynced 表示官方快照已成功同步过、但上游窗口内没有数据
	//（官方统计有滞后，或账号没有官方客户端消耗）。前端据此显示静态占位
	// 并停止重拉，而不是把「暂无数据」当成「还在加载」无限转圈。
	OfficialUsageSynced bool `json:"official_usage_synced,omitempty"`
}

// GetAccountPageStats loads log-derived usage and billing values after the
// visible account page has rendered. The request is strictly bounded to one
// page, avoiding any pool-wide aggregation on the first-screen path.
func (h *Handler) GetAccountPageStats(c *gin.Context) {
	ids, err := parseAccountListIDs(c.Query("ids"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "ids 参数无效")
		return
	}
	if len(ids) == 0 {
		c.JSON(http.StatusOK, gin.H{"stats": map[int64]accountPageStatsItem{}})
		return
	}
	if len(ids) > accountListPageMax {
		writeError(c, http.StatusBadRequest, "ids 最多允许 500 个")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	now := time.Now()
	usage5h, usage7d, err := h.db.GetAccountUsageWindowsByIDs(ctx, ids, now.Add(-5*time.Hour), now.AddDate(0, 0, -7))
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// 今日口径与全局统计一致:服务器时区(TZ 配置,默认宿主时区)当天 0 点起。
	// 失败时整列不下发,让前端保持占位,避免把"查询失败"显示成 0。
	todaySince := database.StartOfDay(now)
	usageToday, todayErr := h.db.GetAccountUsageSinceByIDs(ctx, ids, todaySince)
	if todayErr != nil {
		log.Printf("获取当前页账号今日用量失败: %v", todayErr)
	}
	todayModels, todayModelErr := h.db.GetAccountModelCountsSinceByIDs(ctx, ids, todaySince)
	if todayModelErr != nil {
		log.Printf("获取当前页账号今日模型分布失败: %v", todayModelErr)
		todayModels = map[int64]map[string]database.AccountModelCount{}
	}

	billing5hWindows, billing7dWindows := h.accountBillingWindows(ids)
	billed5h, err := h.db.GetAccountsBilledSince(ctx, billing5hWindows)
	if err != nil {
		log.Printf("获取当前页账号 5h 成本失败: %v", err)
		billed5h = map[int64]float64{}
	}
	billed7d, err := h.db.GetAccountsBilledSince(ctx, billing7dWindows)
	if err != nil {
		log.Printf("获取当前页账号长窗口成本失败: %v", err)
		billed7d = map[int64]float64{}
	}

	// 官方结算快照缺失只是少一行展示，不能拖垮整页统计。
	officialTotals, err := h.db.SumAccountDailyUsage(ctx, ids, proxy.WhamDailyUsageRetentionDays)
	if err != nil {
		log.Printf("获取当前页账号官方结算成本失败: %v", err)
		officialTotals = map[int64]database.AccountDailyUsageTotal{}
	}

	stats := make(map[int64]accountPageStatsItem, len(ids))
	missingOfficial := make([]int64, 0)
	for _, id := range ids {
		item := accountPageStatsItem{}
		if value := usage5h[id]; value != nil {
			item.Usage5hDetail = &accountUsageWindow{
				Requests: value.Requests, Tokens: value.Tokens,
				AccountBilled: value.AccountBilled, UserBilled: value.UserBilled,
			}
		}
		if value := usage7d[id]; value != nil {
			item.Usage7dDetail = &accountUsageWindow{
				Requests: value.Requests, Tokens: value.Tokens,
				AccountBilled: value.AccountBilled, UserBilled: value.UserBilled,
			}
		}
		if todayErr == nil {
			todayWindow := &accountUsageWindow{}
			if value := usageToday[id]; value != nil {
				todayWindow.Requests = value.Requests
				todayWindow.Tokens = value.Tokens
				todayWindow.AccountBilled = value.AccountBilled
				todayWindow.UserBilled = value.UserBilled
			}
			if models := todayModels[id]; len(models) > 0 {
				todayWindow.ModelCounts = make(map[string]int64, len(models))
				todayWindow.ModelSuccessCounts = make(map[string]int64, len(models))
				todayWindow.ModelAvgFirstTokenMs = make(map[string]float64, len(models))
				for model, count := range models {
					todayWindow.ModelCounts[model] = count.Requests
					todayWindow.ModelSuccessCounts[model] = count.Success
					if count.AvgFirstTokenMs > 0 {
						todayWindow.ModelAvgFirstTokenMs[model] = count.AvgFirstTokenMs
					}
				}
				if len(todayWindow.ModelAvgFirstTokenMs) == 0 {
					todayWindow.ModelAvgFirstTokenMs = nil
				}
			}
			item.UsageTodayDetail = todayWindow
		}
		if value, ok := billed5h[id]; ok {
			item.Billed5h = &value
		}
		if value, ok := billed7d[id]; ok {
			item.Billed7d = &value
		}
		// 没有快照的账号不下发这个字段，前端显示占位而不是 $0；
		// 当前页缺快照时异步回补，不挡本次响应。已经成功同步过但
		// 上游没有数据的账号下发显式空态，不再反复回补。
		if total, ok := officialTotals[id]; ok {
			usd := total.Credits / proxy.WhamCreditsPerUSD
			item.OfficialUSD7d = &usd
		} else if h.whamDailySyncedOnceFor(id) {
			item.OfficialUsageSynced = true
		} else {
			missingOfficial = append(missingOfficial, id)
		}
		stats[id] = item
	}
	h.enqueueWhamDailyUsageBackfill(missingOfficial)
	c.JSON(http.StatusOK, gin.H{"stats": stats})
}

func (h *Handler) accountBillingWindows(ids []int64) (map[int64]time.Time, map[int64]time.Time) {
	shortWindows := make(map[int64]time.Time, len(ids))
	longWindows := make(map[int64]time.Time, len(ids))
	if h.store == nil {
		return shortWindows, longWindows
	}
	for _, id := range ids {
		account := h.store.FindByID(id)
		if account == nil {
			continue
		}
		if resetAt := account.GetReset5hAt(); !resetAt.IsZero() {
			shortWindows[id] = resetAt.Add(-5 * time.Hour)
		}
		if resetAt := account.GetReset7dAt(); !resetAt.IsZero() {
			window := 7 * 24 * time.Hour
			if seconds := account.GetWindow7dSeconds(); seconds > 0 {
				window = time.Duration(seconds) * time.Second
			}
			longWindows[id] = resetAt.Add(-window)
		}
	}
	return shortWindows, longWindows
}
