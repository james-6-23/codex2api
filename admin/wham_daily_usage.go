package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// whamDailyUsageDefaultDays 是弹窗默认展示的天数。上游只留 7 天，本地库累积后
// 可以看更长，所以默认给 30。
const whamDailyUsageDefaultDays = 30

// whamDailyUsageMaxDays 限制单次查询窗口。与快照保留期 whamDailyUsageKeepDays
// 对齐：留了一年就要查得到一年，否则超出上限的那部分数据永远看不到。
const whamDailyUsageMaxDays = whamDailyUsageKeepDays

func (h *Handler) queryWhamDailyUsageUpstream(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
	if h != nil && h.queryWhamDailyUsage != nil {
		return h.queryWhamDailyUsage(ctx, account, proxyURL, startDate, endDate)
	}
	return proxy.QueryWhamDailyUsage(ctx, account, proxyURL, startDate, endDate)
}

// GetAccountWhamDailyUsage 返回账号的官方结算用量历史。
// GET /api/admin/accounts/:id/wham-daily-usage?days=30[&refresh=1]
//
// 默认读本地快照（秒开）。refresh=1 时先打一次上游把保留窗口内的数据回补落库，
// 再返回合并后的结果，用于弹窗上的「刷新」按钮。
func (h *Handler) GetAccountWhamDailyUsage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}

	days := whamDailyUsageDefaultDays
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed <= 0 || parsed > whamDailyUsageMaxDays {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("days 必须是 1~%d 的整数", whamDailyUsageMaxDays))
			return
		}
		days = parsed
	}

	// 刷新失败不挡住历史展示：上游 401/429 时仍然返回已落库的数据，
	// 只把错误原因带回去，让弹窗提示而不是整块变空。
	refreshError := ""
	if c.Query("refresh") == "1" {
		account := h.findAccountByID(id)
		switch {
		case account == nil:
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		case isCodexATAccount(account):
			refreshError = errWhamDailyUsageUnsupported.Error()
		case account.GetAccessToken() == "":
			refreshError = "账号没有可用的 access token，请先刷新账号"
		default:
			ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
			if _, err := h.syncWhamDailyUsage(ctx, account); err != nil {
				refreshError = err.Error()
			}
			cancel()
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	rows, err := h.db.ListAccountDailyUsage(ctx, id, days)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取官方用量历史失败："+err.Error())
		return
	}

	items := make([]gin.H, 0, len(rows))
	var totalCredits float64
	var totalTokens, totalTurns int64
	var lastSynced time.Time
	for _, row := range rows {
		if row == nil {
			continue
		}
		totalCredits += row.Credits
		totalTokens += row.TotalTokens
		totalTurns += int64(row.Turns)
		if row.SyncedAt.After(lastSynced) {
			lastSynced = row.SyncedAt
		}
		items = append(items, gin.H{
			"day":                   row.Day,
			"credits":               row.Credits,
			"usd":                   row.Credits / proxy.WhamCreditsPerUSD,
			"users":                 row.Users,
			"threads":               row.Threads,
			"turns":                 row.Turns,
			"uncached_input_tokens": row.UncachedInputTokens,
			"cached_input_tokens":   row.CachedInputTokens,
			"output_tokens":         row.OutputTokens,
			"total_tokens":          row.TotalTokens,
			"settled":               row.Settled,
			"clients":               rawJSONArray(row.ClientsJSON),
			"models":                rawJSONArray(row.ModelsJSON),
		})
	}

	payload := gin.H{
		"days":  days,
		"items": items,
		"totals": gin.H{
			"credits":      totalCredits,
			"usd":          totalCredits / proxy.WhamCreditsPerUSD,
			"total_tokens": totalTokens,
			"turns":        totalTurns,
		},
		"credits_per_usd": proxy.WhamCreditsPerUSD,
		"retention_days":  proxy.WhamDailyUsageRetentionDays,
	}
	if !lastSynced.IsZero() {
		payload["last_synced_at"] = lastSynced
	}
	if refreshError != "" {
		payload["refresh_error"] = refreshError
	}
	c.JSON(http.StatusOK, payload)
}

// rawJSONArray 把落库时原样保存的拆分数组回填成 JSON 值。解析失败时退回空数组，
// 保证响应结构稳定，前端不用做兼容分支。
func rawJSONArray(raw string) []any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []any{}
	}
	var out []any
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return []any{}
	}
	return out
}

// syncWhamDailyUsage 拉取上游保留窗口内的全部天数并 upsert 落库，返回写入的天数。
//
// 每次都覆盖整个 7 天窗口而不是只取当天：当天的记录不含 token 明细，隔天再抓
// 同一天才补齐；整窗回补顺带修复漏跑那几轮的空洞。
func (h *Handler) syncWhamDailyUsage(ctx context.Context, account *auth.Account) (int, error) {
	if h == nil || h.db == nil || account == nil {
		return 0, errWhamDailyUsageUnavailable
	}
	if isCodexATAccount(account) {
		return 0, errWhamDailyUsageUnsupported
	}
	startDate, endDate := proxy.WhamDailyUsageWindow(time.Now())
	result, resp, err := h.queryWhamDailyUsageUpstream(ctx, account, h.store.ResolveProxyForAccount(account), startDate, endDate)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			_ = resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				return 0, errWhamDailyUsageUnauthorized
			case http.StatusTooManyRequests:
				return 0, errWhamDailyUsageRateLimited
			default:
				return 0, upstreamDailyUsageError(resp.StatusCode, body)
			}
		}
		return 0, err
	}

	written := 0
	for _, day := range result.Data {
		normalized := strings.TrimSpace(day.Date)
		if normalized == "" {
			continue
		}
		clients, _ := json.Marshal(day.Clients)
		models, _ := json.Marshal(day.Models)
		input := database.AccountDailyUsageInput{
			AccountID:   account.DBID,
			Day:         normalized,
			Credits:     day.Totals.Credits,
			Users:       day.Totals.Users,
			Threads:     day.Totals.Threads,
			Turns:       day.Totals.Turns,
			Settled:     day.Totals.Settled(),
			ClientsJSON: string(clients),
			ModelsJSON:  string(models),
		}
		if day.Totals.UncachedTextInputTokens != nil {
			input.UncachedInputTokens = *day.Totals.UncachedTextInputTokens
		}
		if day.Totals.CachedTextInputTokens != nil {
			input.CachedInputTokens = *day.Totals.CachedTextInputTokens
		}
		if day.Totals.TextOutputTokens != nil {
			input.OutputTokens = *day.Totals.TextOutputTokens
		}
		if day.Totals.TextTotalTokens != nil {
			input.TotalTokens = *day.Totals.TextTotalTokens
		}
		if err := h.db.UpsertAccountDailyUsage(ctx, input); err != nil {
			return written, err
		}
		written++
	}
	// 空数据也算同步成功：上游窗口内确实没有记录（官方统计滞后或无官方
	// 消耗），必须记住这次成功，否则该账号会被 page-stats 永远当作缺失。
	h.markWhamDailySynced(account.DBID)
	return written, nil
}
