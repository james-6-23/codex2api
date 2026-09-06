package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// whamDailyUsageDefaultDays 是弹窗默认展示的天数。上游可回溯约 84 天，本地库累积后
// 可以看更长，所以默认给 30。
const whamDailyUsageDefaultDays = 30

// whamDailyUsageMaxDays 限制单次查询窗口。与快照保留期 whamDailyUsageKeepDays
// 对齐：留了一年就要查得到一年，否则超出上限的那部分数据永远看不到。
const whamDailyUsageMaxDays = whamDailyUsageKeepDays

// whamDailyBreakdownMinShare 是拆分条目下发的最小份额。上游目录里会出现 1e-7 级
// 的碎屑（如 gpt-6-astra 0.0004%），显示成 $0 的一行只添乱。
const whamDailyBreakdownMinShare = 1e-6

func (h *Handler) queryWhamDailyUsageUpstream(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
	if h != nil && h.queryWhamDailyUsage != nil {
		return h.queryWhamDailyUsage(ctx, account, proxyURL, startDate, endDate)
	}
	return proxy.QueryWhamDailyUsage(ctx, account, proxyURL, startDate, endDate)
}

func (h *Handler) queryWhamDailyTokenBreakdownUpstream(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (*proxy.WhamDailyTokenBreakdownResponse, *http.Response, error) {
	if h != nil && h.queryWhamDailyTokenBreakdown != nil {
		return h.queryWhamDailyTokenBreakdown(ctx, account, proxyURL, startDate, endDate)
	}
	return proxy.QueryWhamDailyTokenBreakdown(ctx, account, proxyURL, startDate, endDate)
}

// GetAccountWhamDailyUsage 返回账号的官方结算用量历史。
// GET /api/admin/accounts/:id/wham-daily-usage?days=30[&refresh=1]
//
// 默认读本地快照（秒开）。refresh=1 时先打一次上游把滚动窗口内的数据回补落库，
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
	// counts 与拆分是两个端点，分别报错：拆分失败时 counts 已经刷新成功了。
	refreshError := ""
	breakdownRefreshError := ""
	if c.Query("refresh") == "1" {
		account := h.findAccountByID(id)
		switch {
		case account == nil:
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		case isCodexATAccount(account) || !whamDailyUsageChannelSupported(account):
			refreshError = errWhamDailyUsageUnsupported.Error()
		case account.GetAccessToken() == "":
			refreshError = "账号没有可用的 access token，请先刷新账号"
		default:
			ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
			outcome, syncErr := h.syncWhamDailyUsage(ctx, account)
			if syncErr != nil {
				refreshError = syncErr.Error()
			}
			if outcome.BreakdownErr != nil {
				breakdownRefreshError = outcome.BreakdownErr.Error()
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
		// 拆分先到、counts 还没来的行不展示：没有官方 credits 的一天既没法解读份额，
		// 也不该把它的 synced_at 当成「刚同步过」。counts 到了自然会出现。
		if row == nil || !row.HasCounts() {
			continue
		}
		totalCredits += row.Credits
		totalTokens += row.TotalTokens
		totalTurns += int64(row.Turns)
		if row.SyncedAt.After(lastSynced) {
			lastSynced = row.SyncedAt
		}
		item := gin.H{
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
		}
		breakdown, surfaces := projectWhamDailyBreakdown(row)
		item["breakdown_available"] = row.BreakdownPercent > 0
		item["breakdown"] = breakdown
		item["surfaces"] = surfaces
		items = append(items, item)
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
		// 上游可回溯的深度（深回补窗口），前端用它解释「更早的来自本地快照」。
		"retention_days": proxy.WhamDailyUsageBackfillDays,
	}
	if !lastSynced.IsZero() {
		payload["last_synced_at"] = lastSynced
	}
	// 本周期成本与额度估算：只对有 wham 链路的账号给，其它渠道没有窗口信息。
	if account := h.findAccountByID(id); account != nil && whamDailyUsageChannelSupported(account) && !isCodexATAccount(account) {
		if cycle := h.buildWhamDailyCycle(ctx, account, time.Now()); cycle != nil {
			payload["cycle"] = cycle
		}
	}
	if refreshError != "" {
		payload["refresh_error"] = refreshError
	}
	if breakdownRefreshError != "" {
		payload["breakdown_refresh_error"] = breakdownRefreshError
	}
	c.JSON(http.StatusOK, payload)
}

// projectWhamDailyBreakdown 把落库的归一化占比换成当天内部的份额与分摊成本。
//
// 上游 percent 按请求区间峰值归一化，不同天、不同次同步的数值不可比；但同一天内
// 各条目除以当天总量就是份额（0~1），份额乘当天官方 credits 就是该模型/入口的绝对
// 成本。free 号 credits 恒 0，此时只有份额有意义，credits/usd 下发 0。
// 模型与入口各用自己的总量做分母：实测两者相等，但万一上游哪天不一致，各自仍能
// 归一到 1。
func projectWhamDailyBreakdown(row *database.AccountDailyUsage) ([]gin.H, []gin.H) {
	breakdown := []gin.H{}
	surfaces := []gin.H{}
	if row == nil || row.BreakdownPercent <= 0 {
		return breakdown, surfaces
	}
	modelTotal := row.BreakdownPercent
	for _, m := range database.ParseAccountDailyBreakdownModels(row.BreakdownJSON) {
		share := m.Percent / modelTotal
		if share < whamDailyBreakdownMinShare {
			continue
		}
		credits := share * row.Credits
		speed := strings.TrimSpace(m.Speed)
		if speed == "" {
			speed = proxy.WhamSpeedStandard
		}
		breakdown = append(breakdown, gin.H{
			"model":   m.Model,
			"speed":   speed,
			"share":   share,
			"credits": credits,
			"usd":     credits / proxy.WhamCreditsPerUSD,
		})
	}
	surfaceMap := database.ParseAccountDailySurfaces(row.SurfacesJSON)
	surfaceTotal := 0.0
	for _, value := range surfaceMap {
		if value > 0 {
			surfaceTotal += value
		}
	}
	if surfaceTotal > 0 {
		for _, s := range database.SortedAccountDailySurfaces(surfaceMap) {
			share := s.Percent / surfaceTotal
			if share < whamDailyBreakdownMinShare {
				continue
			}
			credits := share * row.Credits
			surfaces = append(surfaces, gin.H{
				"surface": s.Surface,
				"share":   share,
				"credits": credits,
				"usd":     credits / proxy.WhamCreditsPerUSD,
			})
		}
	}
	return breakdown, surfaces
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

// whamDailySyncOutcome 是一次同步的结果。counts 是主链路，失败直接作为 error 返回；
// 拆分是补充信息，失败只记在 BreakdownErr 里，不影响 counts 落库与「已同步」标记。
type whamDailySyncOutcome struct {
	CountsDays    int
	BreakdownDays int
	BreakdownErr  error
}

// whamDailyDeepState 记录本进程内某账号两个端点是否已各自完成过一次深回补请求。
// 进程内存即可：重启后最多再深拉一次。
type whamDailyDeepState struct {
	Counts    bool
	Breakdown bool
}

func (h *Handler) whamDailyDeepStateFor(id int64) whamDailyDeepState {
	if h == nil {
		return whamDailyDeepState{}
	}
	h.whamDailyBackfillMu.Lock()
	defer h.whamDailyBackfillMu.Unlock()
	return h.whamDailyDeepSynced[id]
}

func (h *Handler) markWhamDailyDeepSynced(id int64, counts, breakdown bool) {
	if h == nil || id <= 0 {
		return
	}
	h.whamDailyBackfillMu.Lock()
	defer h.whamDailyBackfillMu.Unlock()
	if h.whamDailyDeepSynced == nil {
		h.whamDailyDeepSynced = map[int64]whamDailyDeepState{}
	}
	state := h.whamDailyDeepSynced[id]
	state.Counts = state.Counts || counts
	state.Breakdown = state.Breakdown || breakdown
	h.whamDailyDeepSynced[id] = state
}

// whamDailyWindow 是一次上游请求的日期区间；Deep 表示这是深回补（84 天）而不是
// 7 天滚动窗口。
type whamDailyWindow struct {
	Start string
	End   string
	Deep  bool
}

// whamDailySyncWindows 按端点各自决定同步窗口。
//
// 深回补（84 天）的条件：本进程还没为该端点深拉过，且本地该端点的覆盖起点晚于
// 深回补起点（含完全没有覆盖）。两个端点必须分开判断：拆分先到会以零 counts 建行，
// 若只看「有没有行」，counts 首轮失败就会被永久锁在 7 天窗口，历史再也拿不到；
// 反过来拆分同理。老部署升级后本地只有 7 天滚动积累的历史，同样满足条件，会各深拉
// 一次把上游还留着的 84 天搬进来。账号历史本来就短于 84 天的，每个进程生命周期只
// 会多拉一次，靠进程内标记兜住。
func (h *Handler) whamDailySyncWindows(ctx context.Context, account *auth.Account, now time.Time) (counts, breakdown whamDailyWindow) {
	rollingStart, endDate := proxy.WhamDailyUsageWindow(now)
	deepStart, _ := proxy.WhamDailyUsageBackfillWindow(now)
	counts = whamDailyWindow{Start: rollingStart, End: endDate}
	breakdown = counts
	if h == nil || h.db == nil || account == nil {
		return counts, breakdown
	}
	coverage, err := h.db.GetAccountDailyUsageCoverage(ctx, account.DBID)
	if err != nil {
		// 查不到覆盖就按滚动窗口走，不因此打断同步。
		return counts, breakdown
	}
	state := h.whamDailyDeepStateFor(account.DBID)
	if !state.Counts && (coverage.CountsOldestDay == "" || coverage.CountsOldestDay > deepStart) {
		counts.Start, counts.Deep = deepStart, true
	}
	if !state.Breakdown && (coverage.BreakdownOldestDay == "" || coverage.BreakdownOldestDay > deepStart) {
		breakdown.Start, breakdown.Deep = deepStart, true
	}
	return counts, breakdown
}

// syncWhamDailyUsage 并行拉取 counts 与模型×速度拆分并 upsert 落库。
//
// 窗口：稳态滚动回补最近 7 天（当天的记录全天在变，隔天才稳定；整窗覆盖顺带修复
// 漏跑的空洞）；某端点本地没有足够深的覆盖时对该端点深回补 84 天，见
// whamDailySyncWindows。两个端点用同一个 ctx 并行跑：任何一个慢或失败都不拖累
// 另一个的落库。
func (h *Handler) syncWhamDailyUsage(ctx context.Context, account *auth.Account) (whamDailySyncOutcome, error) {
	var outcome whamDailySyncOutcome
	if h == nil || h.db == nil || account == nil {
		return outcome, errWhamDailyUsageUnavailable
	}
	// 手动刷新也会走到这里：非 ChatGPT 渠道的凭据在此兜底拒绝，不只靠探针候选过滤。
	if isCodexATAccount(account) || !whamDailyUsageChannelSupported(account) {
		return outcome, errWhamDailyUsageUnsupported
	}
	countsWindow, breakdownWindow := h.whamDailySyncWindows(ctx, account, time.Now())
	proxyURL := h.store.ResolveProxyForAccount(account)

	var (
		wg        sync.WaitGroup
		countsErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		outcome.CountsDays, countsErr = h.syncWhamDailyCounts(ctx, account, proxyURL, countsWindow.Start, countsWindow.End)
		if countsErr == nil && countsWindow.Deep {
			h.markWhamDailyDeepSynced(account.DBID, true, false)
		}
	}()
	go func() {
		defer wg.Done()
		outcome.BreakdownDays, outcome.BreakdownErr = h.syncWhamDailyBreakdown(ctx, account, proxyURL, breakdownWindow.Start, breakdownWindow.End)
		if outcome.BreakdownErr == nil && breakdownWindow.Deep {
			h.markWhamDailyDeepSynced(account.DBID, false, true)
		}
	}()
	wg.Wait()

	if countsErr != nil {
		return outcome, countsErr
	}
	// 空数据也算同步成功：上游窗口内确实没有记录（官方统计滞后或无官方
	// 消耗），必须记住这次成功，否则该账号会被 page-stats 永远当作缺失。
	h.markWhamDailySynced(account.DBID)
	return outcome, nil
}

// classifyWhamDailyUpstreamError 把上游非 200 统一映射成可展示的错误。
// resp 为 nil（请求层面的错误）或 200（读 body / 解析失败，body 已被查询函数读掉）
// 时原样返回 err，不能把解析错误报成「上游返回 200」。
func classifyWhamDailyUpstreamError(resp *http.Response, err error) error {
	if resp == nil || resp.StatusCode == http.StatusOK {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return errWhamDailyUsageUnauthorized
	case http.StatusTooManyRequests:
		return errWhamDailyUsageRateLimited
	default:
		return upstreamDailyUsageError(resp.StatusCode, body)
	}
}

// syncWhamDailyCounts 拉取 counts 端点并按天落库，返回写入的天数。
func (h *Handler) syncWhamDailyCounts(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (int, error) {
	result, resp, err := h.queryWhamDailyUsageUpstream(ctx, account, proxyURL, startDate, endDate)
	if err != nil {
		return 0, classifyWhamDailyUpstreamError(resp, err)
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
			AccountID: account.DBID,
			Day:       normalized,
			Credits:   day.Totals.Credits,
			Users:     day.Totals.Users,
			Threads:   day.Totals.Threads,
			Turns:     day.Totals.Turns,
			// endDate 就是今天（UTC）：今天的行全天在变，按日期判定而不是按字段有无。
			Settled:     day.SettledOn(endDate),
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
	return written, nil
}

// syncWhamDailyBreakdown 拉取模型×速度拆分并按天落库，返回写入的天数。
//
// 拆分端点是稠密的：区间内每天一行、含未来日期、无消耗的日子全 0。只写有消耗的
// 历史日与今天；未来行与全 0 行跳过，否则会造出一堆零 counts 的空行。
func (h *Handler) syncWhamDailyBreakdown(ctx context.Context, account *auth.Account, proxyURL, startDate, endDate string) (int, error) {
	result, resp, err := h.queryWhamDailyTokenBreakdownUpstream(ctx, account, proxyURL, startDate, endDate)
	if err != nil {
		return 0, classifyWhamDailyUpstreamError(resp, err)
	}
	written := 0
	for _, day := range result.Data {
		date := strings.TrimSpace(day.Date)
		if date == "" || date > endDate {
			continue
		}
		total := day.TotalPercent()
		if total <= 0 {
			continue
		}
		active := day.ActiveModels()
		models := make([]database.AccountDailyBreakdownModel, 0, len(active))
		for _, m := range active {
			models = append(models, database.AccountDailyBreakdownModel{Model: m.Model, Speed: m.Speed, Percent: m.Percent})
		}
		input := database.AccountDailyBreakdownInput{
			AccountID: account.DBID,
			Day:       date,
			Percent:   total,
			Models:    models,
			Surfaces:  day.ActiveSurfaces(),
		}
		if err := h.db.UpsertAccountDailyBreakdown(ctx, input); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}
