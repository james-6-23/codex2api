package admin

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// autoBalanceProxiesReq 是代理均衡绑定的请求体。
//   - Channel: grok/codex/claude/空(全部 OAuth)。同一出口上的 OAuth 账号过多时,
//     均衡绑定把账号摊开,降低上游按出口聚合限流的概率。
//   - Mode: unbound(默认,只分配未绑定账号) / all(全量重排,但尽量保留现有绑定以减少换 IP)。
//   - MaxPerProxy: 每条代理的账号数上限,0 表示不限。
//   - ProxyIDs: 限定参与分配的代理,空表示所有启用且未测出错误的代理。
type autoBalanceProxiesReq struct {
	Channel     string  `json:"channel"`
	Mode        string  `json:"mode"`
	MaxPerProxy int     `json:"max_per_proxy"`
	ProxyIDs    []int64 `json:"proxy_ids"`
}

type autoBalanceDistribution struct {
	ProxyID    int64  `json:"proxy_id"`
	Label      string `json:"label,omitempty"`
	BoundCount int64  `json:"bound_count"`
}

// balanceAccount 是参与均衡分配的一个账号:ID + 当前绑定(trim 后,可为空)。
type balanceAccount struct {
	id      int64
	current string
}

// isOAuthProxyBalanceTarget 只允许真正可刷新的 OAuth 账号参与自动均衡。
// API Key、中转 API、AT-only、Session Token 与 Agent Identity 账号都不应被
// 自动改变出口；它们已有的手工绑定仍会作为 baseline 负载参与容量计算。
func isOAuthProxyBalanceTarget(row *database.AccountRow) bool {
	if row == nil || strings.TrimSpace(row.GetCredential("refresh_token")) == "" {
		return false
	}
	if strings.TrimSpace(row.GetCredential("api_key")) != "" {
		return false
	}

	upstreamType := strings.ToLower(strings.TrimSpace(row.GetCredential("upstream_type")))
	switch upstreamType {
	case "":
		return strings.EqualFold(strings.TrimSpace(row.Type), "oauth")
	case auth.UpstreamGrok, auth.UpstreamAntigravity, auth.UpstreamClaude:
		return true
	default:
		return false
	}
}

func selectOAuthProxyBalanceTargets(rows []*database.AccountRow, mode string) []balanceAccount {
	targets := make([]balanceAccount, 0, len(rows))
	for _, row := range rows {
		if !isOAuthProxyBalanceTarget(row) {
			continue
		}
		bound := strings.TrimSpace(row.ProxyURL)
		if mode == "unbound" && bound != "" {
			continue
		}
		targets = append(targets, balanceAccount{id: row.ID, current: bound})
	}
	return targets
}

// balanceResult 是纯分配算法的产出。
type balanceResult struct {
	// assignments 只包含绑定发生变化的账号(写库范围)。
	assignments map[int64]string
	// load 是分配完成后各候选代理的账号数(含基线负载)。
	load map[string]int64
	// kept 是保持原绑定不动的账号数;skipped 是全部代理到达上限后放不下的账号数。
	kept    int
	skipped int
}

// computeProxyAssignments 把 targets 按"最少绑定优先"分配到 candidates 上。
//
// 分配原则:
//  1. 稳定优先:账号当前绑定仍在候选池且未超上限的保持不动
//     (Grok 风控对换 IP 敏感,重排只动必须动的账号);
//  2. 其余账号按账号 ID 升序,逐个放进当前负载最低的代理;
//     并列取 URL 字典序,保证结果确定可复现;
//  3. baseline 是各代理上不参与本次分配的既有绑定数(含其它渠道账号),
//     避免把号堆到已被其它渠道占满的代理上。
func computeProxyAssignments(targets []balanceAccount, candidates []string, baseline map[string]int64, maxPerProxy int64) balanceResult {
	result := balanceResult{
		assignments: make(map[int64]string),
		load:        make(map[string]int64, len(candidates)),
	}
	candidateSet := make(map[string]struct{}, len(candidates))
	for _, url := range candidates {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		candidateSet[url] = struct{}{}
		result.load[url] = baseline[url]
	}
	if len(candidateSet) == 0 {
		result.skipped = len(targets)
		return result
	}

	sorted := append([]balanceAccount(nil), targets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].id < sorted[j].id })

	underCap := func(url string) bool {
		return maxPerProxy == 0 || result.load[url] < maxPerProxy
	}
	pickLeastLoaded := func() (string, bool) {
		best := ""
		var bestLoad int64
		for url := range candidateSet {
			if !underCap(url) {
				continue
			}
			if best == "" || result.load[url] < bestLoad || (result.load[url] == bestLoad && url < best) {
				best = url
				bestLoad = result.load[url]
			}
		}
		return best, best != ""
	}

	// 第一遍:保留仍然有效的现有绑定。
	pending := make([]balanceAccount, 0, len(sorted))
	for _, t := range sorted {
		if t.current != "" {
			if _, ok := candidateSet[t.current]; ok && underCap(t.current) {
				result.load[t.current]++
				result.kept++
				continue
			}
		}
		pending = append(pending, t)
	}
	// 第二遍:剩余账号放进负载最低的代理。
	for _, t := range pending {
		url, ok := pickLeastLoaded()
		if !ok {
			result.skipped++
			continue
		}
		result.assignments[t.id] = url
		result.load[url]++
	}
	return result
}

// AutoBalanceProxies 把指定渠道的 OAuth 账号按"最少绑定优先"均匀分配到候选代理上。
// POST /api/admin/proxies/auto-balance
func (h *Handler) AutoBalanceProxies(c *gin.Context) {
	var req autoBalanceProxiesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "unbound"
	}
	if mode != "unbound" && mode != "all" {
		writeError(c, http.StatusBadRequest, "mode 仅支持 unbound / all")
		return
	}
	channel := strings.ToLower(strings.TrimSpace(req.Channel))
	if channel != "" && channel != database.UpstreamChannelGrok && channel != database.UpstreamChannelCodex && channel != database.UpstreamChannelAntigravity && channel != database.UpstreamChannelClaude {
		writeError(c, http.StatusBadRequest, "channel 仅支持 grok / codex / antigravity / claude / 空")
		return
	}
	if req.MaxPerProxy < 0 {
		writeError(c, http.StatusBadRequest, "max_per_proxy 不能为负数")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	proxies, err := h.candidateProxiesForBalance(ctx, req.ProxyIDs)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if len(proxies) == 0 {
		writeError(c, http.StatusBadRequest, "没有可用代理(需启用且连通性测试未失败)")
		return
	}

	rows, err := h.db.ListActiveByChannel(ctx, channel)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	// 待分配账号集合只包含可刷新的 OAuth 账号。unbound 模式只挑未绑定的;
	// all 模式重排全部 OAuth，非 OAuth 账号不参与但其已有绑定仍计入 baseline。
	targets := selectOAuthProxyBalanceTargets(rows, mode)
	if len(targets) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "没有需要分配的 OAuth 账号", "assigned": 0, "kept": 0, "skipped": 0})
		return
	}

	// 基线负载 = 全量绑定数减去本次目标账号的贡献(目标账号的绑定会在算法里重新计入)。
	candidates := make([]string, 0, len(proxies))
	for _, p := range proxies {
		candidates = append(candidates, strings.TrimSpace(p.URL))
	}
	allCounts, err := h.db.CountAccountsByProxyURL(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	baseline := make(map[string]int64, len(candidates))
	for _, url := range candidates {
		baseline[url] = allCounts[url]
	}
	for _, t := range targets {
		if t.current == "" {
			continue
		}
		if _, ok := baseline[t.current]; ok {
			baseline[t.current]--
		}
	}

	result := computeProxyAssignments(targets, candidates, baseline, int64(req.MaxPerProxy))

	if len(result.assignments) > 0 {
		if err := h.db.SetAccountProxyURLs(ctx, result.assignments); err != nil {
			writeInternalError(c, err)
			return
		}
		for id, url := range result.assignments {
			h.store.ApplyAccountProxyURL(id, url)
		}
	}

	distribution := make([]autoBalanceDistribution, 0, len(proxies))
	for _, p := range proxies {
		distribution = append(distribution, autoBalanceDistribution{
			ProxyID:    p.ID,
			Label:      p.Label,
			BoundCount: result.load[strings.TrimSpace(p.URL)],
		})
	}
	sort.Slice(distribution, func(i, j int) bool { return distribution[i].ProxyID < distribution[j].ProxyID })

	security.SecurityAuditLog("PROXY_AUTO_BALANCE", fmt.Sprintf(
		"channel=%s auth=oauth mode=%s assigned=%d kept=%d skipped=%d proxies=%d cap=%d ip=%s",
		channel, mode, len(result.assignments), result.kept, result.skipped, len(proxies), req.MaxPerProxy, c.ClientIP()))

	c.JSON(http.StatusOK, gin.H{
		"message":      "OAuth 账号均衡绑定完成",
		"assigned":     len(result.assignments),
		"kept":         result.kept,
		"skipped":      result.skipped,
		"proxies_used": len(proxies),
		"distribution": distribution,
	})
}

// candidateProxiesForBalance 返回参与均衡分配的代理:启用且连通性测试未失败;
// ids 非空时限定范围(未启用/出错的仍会被过滤)。
func (h *Handler) candidateProxiesForBalance(ctx context.Context, ids []int64) ([]*database.ProxyRow, error) {
	var proxies []*database.ProxyRow
	var err error
	if len(ids) > 0 {
		proxies, err = h.db.ListProxiesByIDs(ctx, ids)
	} else {
		proxies, err = h.db.ListProxies(ctx)
	}
	if err != nil {
		return nil, err
	}
	out := make([]*database.ProxyRow, 0, len(proxies))
	for _, p := range proxies {
		if !p.Enabled || p.TestStatus == "error" || strings.TrimSpace(p.URL) == "" {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
