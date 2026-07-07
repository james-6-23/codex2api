package admin

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// GetResetCredits 查询账号「主动重置次数」的明细（每张券的发放/过期时间）。
// GET /api/accounts/:id/reset-credits
//
// 调官方 wham/rate-limit-reset-credits 列表端点（零额度成本），按
// reset_type=codex_rate_limits + status=available 过滤，
// 返回可用张数与逐张有效期，供前端展示"哪张券什么时候过期"(issue #322)。
// 顺带用权威 available_count 校准本地缓存的剩余次数。
func (h *Handler) GetResetCredits(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	account := h.findAccountByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}
	if account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "账号没有可用的 access token，请先刷新账号")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	list, resp, err := proxy.QueryWhamResetCredits(ctx, account, h.store.ResolveProxyForAccount(account))
	if err != nil {
		status := upstreamResetStatus(resp)
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			_ = resp.Body.Close()
			switch status {
			case http.StatusUnauthorized:
				writeError(c, http.StatusBadGateway, "上游鉴权失败（401），请先刷新账号后重试")
			case http.StatusTooManyRequests:
				writeError(c, http.StatusBadGateway, "上游限流（429），请稍后重试")
			default:
				writeError(c, http.StatusBadGateway, "查询重置次数失败："+upstreamResetErrorMessage(status, body))
			}
			return
		}
		writeError(c, http.StatusBadGateway, "查询重置次数失败："+err.Error())
		return
	}

	credits := list.AvailableCodexCredits()
	// 权威可用张数：优先上游 available_count，缺失(0 且有券)时以过滤后的张数兜底。
	available := list.AvailableCount
	if available == 0 && len(credits) > 0 {
		available = len(credits)
	}
	// 上游明细是权威数据，顺带校准本地缓存，让列表/弹窗立即一致。
	account.SetRateLimitResetCredits(available)

	items := make([]gin.H, 0, len(credits))
	for _, credit := range credits {
		items = append(items, gin.H{
			"id":         credit.ID,
			"granted_at": credit.GrantedAt,
			"expires_at": credit.ExpiresAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"available_count": available,
		"credits":         items,
	})
}

// ResetCredits 消耗账号 1 次「主动重置次数」以立即重置 Codex 额度。
// POST /api/accounts/:id/reset-credits
//
// 流程：找到账号 → 调用官方 wham/rate-limit-reset-credits/consume → 解析响应即时反馈
// （windows_reset）+ 本地乐观递减剩余次数 → 后台异步刷新用量。次数为 0 时直接拒绝。
//
// 健壮性 / 性能处理：
//   - 账号级互斥锁串行化同一账号的并发重置，避免重复消耗。
//   - consume 复用同一个 redeem_request_id 作幂等键：上游凭它去重，
//     这样 401 刷新后重试不会重复扣减一次宝贵的重置次数。
//   - 上游 401 时先尝试刷新账号 token，再用同一幂等键重试一次。
//   - 用 consume 响应直接反馈、本地乐观递减次数，省掉重置后那次同步 wham 往返；
//     窗口快照 / 精确次数 / 冷却状态交给后台探针异步对齐。
//   - 成功后写入 account_events 审计记录（event_type=reset_credit）。
func (h *Handler) ResetCredits(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	account := h.findAccountByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}

	if account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "账号没有可用的 access token，请先刷新账号")
		return
	}

	// 账号级互斥：同一账号的重置串行执行，避免并发重复消耗与计数竞态。
	lock := h.resetCreditLock(id)
	lock.Lock()
	defer lock.Unlock()

	// 本地已知次数为 0 时直接拒绝（与前端按钮门槛一致），减少无效上游调用。
	if count, ok := account.GetRateLimitResetCredits(); ok && count <= 0 {
		writeError(c, http.StatusConflict, "该账号没有可用的主动重置次数")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	proxyURL := h.store.ResolveProxyForAccount(account)
	// 整个重置操作（含 401 刷新后的重试）复用同一个幂等键，
	// 让上游对重复请求去重，避免多扣一次重置次数。
	redeemRequestID := uuid.New().String()

	result, resp, err := proxy.ConsumeResetCreditParsed(ctx, account, proxyURL, redeemRequestID)
	status := upstreamResetStatus(resp)

	// 上游鉴权失败：刷新一次 token 后用同一幂等键重试。
	if status == http.StatusUnauthorized {
		drainResetResponse(resp)
		if refreshErr := h.refreshAccountForReset(ctx, id); refreshErr != nil {
			log.Printf("[账号 %d] 重置额度时刷新 token 失败: %v", account.DBID, refreshErr)
			writeError(c, http.StatusBadGateway, "上游鉴权失败（401），自动刷新账号失败，请手动刷新后重试")
			return
		}
		// 重新解析代理（刷新可能改变 Resin 粘性等），再用同一幂等键重试。
		proxyURL = h.store.ResolveProxyForAccount(account)
		result, resp, err = proxy.ConsumeResetCreditParsed(ctx, account, proxyURL, redeemRequestID)
		status = upstreamResetStatus(resp)
	}

	// 非 2xx：读取并关闭 body 用于错误详情（2xx 时 body 已由 ConsumeResetCreditParsed 关闭）。
	if resp != nil && (status < 200 || status >= 300) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		switch status {
		case http.StatusUnauthorized:
			writeError(c, http.StatusBadGateway, "上游鉴权失败（401），请先刷新账号后重试")
		case http.StatusTooManyRequests:
			writeError(c, http.StatusBadGateway, "上游限流（429），请稍后重试")
		default:
			writeError(c, http.StatusBadGateway, "重置失败："+upstreamResetErrorMessage(status, body))
		}
		return
	}
	if err != nil && resp == nil {
		writeError(c, http.StatusBadGateway, "重置请求失败："+err.Error())
		return
	}

	// 重置成功。#6：直接用 consume 响应（windows_reset）即时反馈，并本地乐观递减剩余次数，
	// 省掉重置后那次同步的 wham/usage 往返；窗口快照与精确次数交给后台探针异步对齐。
	windowsReset := 0
	if result != nil {
		windowsReset = result.WindowsReset
	}
	remaining := h.applyOptimisticResetDecrement(account)

	// #7：把"主动重置"作为账号事件落库，留下可追溯的审计记录。
	if h.db != nil {
		h.db.InsertAccountEventAsync(account.DBID, "reset_credit", "manual")
	}
	log.Printf("[账号 %d] 主动重置额度成功，windows_reset=%d，剩余次数=%d", account.DBID, windowsReset, remaining)

	// 后台异步刷新用量窗口/精确次数与冷却状态（不阻塞响应）。
	h.refreshUsageAfterReset(account)

	payload := gin.H{"message": "已重置额度"}
	if remaining >= 0 {
		payload["rate_limit_reset_credits"] = remaining
	}
	if windowsReset > 0 {
		payload["windows_reset"] = windowsReset
	}
	c.JSON(http.StatusOK, payload)
}

// applyOptimisticResetDecrement 在重置成功后本地把剩余次数 -1（available_count 会随之减少），
// 让响应即时反映新次数而不必等待后台 wham 探针；返回新的剩余次数，未知时返回 -1。
func (h *Handler) applyOptimisticResetDecrement(account *auth.Account) int {
	count, ok := account.GetRateLimitResetCredits()
	if !ok {
		return -1
	}
	if count > 0 {
		count--
		account.SetRateLimitResetCredits(count)
	}
	return count
}

// refreshUsageAfterReset 在后台刷新账号用量（窗口快照、精确剩余次数、冷却状态）。
// 使用独立 context，避免随 HTTP 请求结束被取消；失败仅记录，不影响已成功的重置。
func (h *Handler) refreshUsageAfterReset(account *auth.Account) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.ProbeUsageSnapshot(ctx, account); err != nil {
			log.Printf("[账号 %d] 重置后后台刷新用量失败: %v", account.DBID, err)
		}
	}()
}

// resetCreditLock 返回指定账号的重置互斥锁（按需创建）。
func (h *Handler) resetCreditLock(id int64) *sync.Mutex {
	if v, ok := h.resetCreditLocks.Load(id); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	if actual, loaded := h.resetCreditLocks.LoadOrStore(id, mu); loaded {
		return actual.(*sync.Mutex)
	}
	return mu
}

// refreshAccountForReset 在重置遇到 401 时刷新账号 token，复用注入的刷新函数
// （测试可替换），否则回退到 store.RefreshSingle。
func (h *Handler) refreshAccountForReset(ctx context.Context, id int64) error {
	if h.refreshAccount != nil {
		return h.refreshAccount(ctx, id)
	}
	if h.store == nil {
		return fmt.Errorf("账号池未初始化")
	}
	return h.store.RefreshSingle(ctx, id)
}

// upstreamResetStatus 安全读取上游响应状态码；resp 为 nil 时返回 0。
func upstreamResetStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// drainResetResponse 读尽并关闭上游响应体，便于连接复用。
func drainResetResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
}

// findAccountByID 按数据库 ID 在运行时号池中查找账号；找不到返回 nil。
func (h *Handler) findAccountByID(id int64) *auth.Account {
	if h.store == nil {
		return nil
	}
	for _, acc := range h.store.Accounts() {
		if acc != nil && acc.DBID == id {
			return acc
		}
	}
	return nil
}

// upstreamResetErrorMessage 从上游响应里提取错误信息用于回传。
// 已知的上游错误 code 会被翻译成中文提示，并附带上游返回的原文，
// 形如「<中文说明>（上游：<原文>）」，既易读又保留可排查的原始信息。
func upstreamResetErrorMessage(statusCode int, body []byte) string {
	raw := truncate(strings.TrimSpace(string(body)), 300)

	if zh := resetErrorCodeToChinese(body); zh != "" {
		if raw != "" {
			return zh + "（上游：" + raw + "）"
		}
		return zh
	}

	if raw != "" {
		return raw
	}
	return "上游返回状态 " + strconv.Itoa(statusCode)
}

// resetErrorCodeToChinese 解析上游错误 body 里的 code/reason，映射为中文说明；
// 无法识别时返回空串（调用方回退到原文）。
// 典型响应：{"detail":{"code":"rate_limit_not_resettable","reason":"credits_only"}}
func resetErrorCodeToChinese(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// code 可能出现在 detail.code 或顶层 code / error.code。
	code := firstNonEmptyGJSONString(body, "detail.code", "code", "error.code")
	reason := firstNonEmptyGJSONString(body, "detail.reason", "reason", "error.reason")

	switch code {
	case "rate_limit_not_resettable":
		if reason == "credits_only" {
			return "该账号为额度（credits）计费计划，当前限流不支持主动重置"
		}
		return "当前限流状态不支持主动重置"
	case "no_reset_credits_available", "insufficient_reset_credits":
		return "该账号没有可用的主动重置次数"
	case "rate_limit_already_reset":
		return "该账号的限流已被重置，暂无需再次重置"
	}
	return ""
}
