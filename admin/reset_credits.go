package admin

import (
	"context"
	"encoding/json"
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

const resetCreditLeaseTTL = 45 * time.Second

const resetCreditCooldownNamespace = "reset-credit-cooldown"

const autoResetCreditHandledIDRetention = 8 * 24 * time.Hour

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

	list, resp, err := h.queryResetCreditsUpstream(ctx, account, h.store.ResolveProxyForAccount(account))
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
			"expires_at": credit.EffectiveConsumableUntil(),
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

	// 工作区级互斥：同一上游工作区的重置串行执行，避免并发重复消耗与计数竞态。
	lock := h.resetCreditLock(account)
	lock.Lock()
	defer lock.Unlock()

	// 本地已知次数为 0 时直接拒绝（与前端按钮门槛一致），减少无效上游调用。
	if count, ok := account.GetRateLimitResetCredits(); ok && count <= 0 {
		writeError(c, http.StatusConflict, "该账号没有可用的主动重置次数")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	// 整个重置操作（含 401 刷新后的重试）复用同一个幂等键，
	// 让上游对重复请求去重，避免多扣一次重置次数。
	redeemRequestID := uuid.New().String()
	outcome, failure := h.consumeResetCreditLocked(ctx, account, redeemRequestID, "manual")
	if failure != nil {
		h.writeManualResetCreditFailure(c, account, failure)
		return
	}
	if outcome.InProgress {
		writeError(c, http.StatusConflict, "该工作区正在执行额度重置，请稍后重试")
		return
	}
	log.Printf("[账号 %d] 主动重置额度成功，windows_reset=%d，剩余次数=%d", account.DBID, outcome.WindowsReset, outcome.Remaining)

	payload := gin.H{"message": "已重置额度"}
	if outcome.Remaining >= 0 {
		payload["rate_limit_reset_credits"] = outcome.Remaining
	}
	if outcome.WindowsReset > 0 {
		payload["windows_reset"] = outcome.WindowsReset
	}
	c.JSON(http.StatusOK, payload)
}

type resetCreditConsumeOutcome struct {
	WindowsReset   int
	Remaining      int
	AlreadyHandled bool
	InProgress     bool
}

type resetCreditConsumeFailure struct {
	Status     int
	Body       []byte
	RequestErr error
	RefreshErr error
}

// consumeResetCreditLocked 执行一次共享的重置消费流程。调用方必须持有工作区级
// resetCreditLock，确保手动与自动路径不会在同一进程内并发消耗。
func (h *Handler) consumeResetCreditLocked(ctx context.Context, account *auth.Account, redeemRequestID, source string) (resetCreditConsumeOutcome, *resetCreditConsumeFailure) {
	if source == "auto" && strings.TrimSpace(redeemRequestID) != "" {
		if h.autoResetCreditRequestHandled(redeemRequestID, time.Now()) {
			remaining := -1
			if count, ok := account.GetRateLimitResetCredits(); ok {
				remaining = count
			}
			return resetCreditConsumeOutcome{Remaining: remaining, AlreadyHandled: true}, nil
		}
	}
	acquired, releaseLease, leaseErr := h.acquireResetCreditLease(ctx, account)
	if leaseErr != nil {
		return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{RequestErr: fmt.Errorf("acquire reset-credit lease: %w", leaseErr)}
	}
	if !acquired {
		return resetCreditConsumeOutcome{InProgress: true}, nil
	}
	defer releaseLease()
	if source == "auto" {
		coolingDown, cooldownErr := h.resetCreditCooldownActive(ctx, account)
		if cooldownErr != nil {
			return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{RequestErr: fmt.Errorf("recheck reset-credit cooldown: %w", cooldownErr)}
		}
		if coolingDown {
			return resetCreditConsumeOutcome{AlreadyHandled: true}, nil
		}
	}
	proxyURL := h.store.ResolveProxyForAccount(account)
	result, resp, err := h.consumeResetCreditUpstream(ctx, account, proxyURL, redeemRequestID)
	status := upstreamResetStatus(resp)

	// 上游鉴权失败：刷新一次 token 后，用同一幂等键重试。
	if status == http.StatusUnauthorized {
		drainResetResponse(resp)
		if refreshErr := h.refreshAccountForReset(ctx, account.DBID); refreshErr != nil {
			return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{Status: status, RefreshErr: refreshErr}
		}
		proxyURL = h.store.ResolveProxyForAccount(account)
		result, resp, err = h.consumeResetCreditUpstream(ctx, account, proxyURL, redeemRequestID)
		status = upstreamResetStatus(resp)
	}

	if resp != nil && (status < 200 || status >= 300) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{Status: status, Body: body, RequestErr: err}
	}
	if err != nil {
		return resetCreditConsumeOutcome{}, &resetCreditConsumeFailure{Status: status, RequestErr: err}
	}

	outcome := resetCreditConsumeOutcome{Remaining: h.applyOptimisticResetDecrement(account)}
	if result != nil {
		outcome.WindowsReset = result.WindowsReset
	}
	if source == "auto" && strings.TrimSpace(redeemRequestID) != "" {
		h.rememberAutoResetCreditRequest(redeemRequestID, time.Now())
	}
	h.markResetCreditSuccess(account)
	h.recordResetCreditEvent(account.DBID, source)
	h.refreshUsageAfterReset(account)
	return outcome, nil
}

func (h *Handler) autoResetCreditRequestHandled(redeemRequestID string, now time.Time) bool {
	if h == nil {
		return false
	}
	value, ok := h.resetCreditSuccessfulIDs.Load(redeemRequestID)
	if !ok {
		return false
	}
	expiresAt, ok := value.(time.Time)
	if !ok || !expiresAt.After(now) {
		h.resetCreditSuccessfulIDs.Delete(redeemRequestID)
		return false
	}
	return true
}

func (h *Handler) rememberAutoResetCreditRequest(redeemRequestID string, now time.Time) {
	if h == nil {
		return
	}
	h.resetCreditSuccessfulIDs.Range(func(key, value any) bool {
		expiresAt, ok := value.(time.Time)
		if !ok || !expiresAt.After(now) {
			h.resetCreditSuccessfulIDs.Delete(key)
		}
		return true
	})
	h.resetCreditSuccessfulIDs.Store(redeemRequestID, now.Add(autoResetCreditHandledIDRetention))
}

func (h *Handler) acquireResetCreditLease(ctx context.Context, account *auth.Account) (bool, func(), error) {
	if h == nil || h.cache == nil {
		return true, func() {}, nil
	}
	owner := uuid.New().String()
	key := resetCreditLockKey(account)
	acquired, err := h.cache.AcquireLease(ctx, "reset-credit", key, owner, resetCreditLeaseTTL)
	if err != nil || !acquired {
		return acquired, func() {}, err
	}
	return true, func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if releaseErr := h.cache.ReleaseLease(releaseCtx, "reset-credit", key, owner); releaseErr != nil {
			log.Printf("[账号 %d] 释放主动重置分布式租约失败: %v", account.DBID, releaseErr)
		}
	}, nil
}

func (h *Handler) resetCreditConsumedRecently(account *auth.Account, now time.Time, window time.Duration) bool {
	if h == nil || account == nil || window <= 0 {
		return false
	}
	value, ok := h.resetCreditLastSuccess.Load(resetCreditLockKey(account))
	if !ok {
		return false
	}
	consumedAt, ok := value.(time.Time)
	if !ok {
		return false
	}
	return consumedAt.After(now) || now.Sub(consumedAt) < window
}

func (h *Handler) resetCreditCooldownActive(ctx context.Context, account *auth.Account) (bool, error) {
	if h.resetCreditConsumedRecently(account, time.Now(), autoResetCreditsScanInterval) {
		return true, nil
	}
	if h == nil || h.cache == nil {
		return false, nil
	}
	_, found, err := h.cache.GetRuntime(ctx, resetCreditCooldownNamespace, resetCreditLockKey(account))
	return found, err
}

func (h *Handler) markResetCreditSuccess(account *auth.Account) {
	if h == nil || account == nil {
		return
	}
	key := resetCreditLockKey(account)
	h.resetCreditLastSuccess.Store(key, time.Now())
	if h.cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.cache.SetRuntime(ctx, resetCreditCooldownNamespace, key, json.RawMessage(`true`), autoResetCreditsScanInterval); err != nil {
		log.Printf("[账号 %d] 写入主动重置冷却标记失败: %v", account.DBID, err)
	}
}

func (h *Handler) writeManualResetCreditFailure(c *gin.Context, account *auth.Account, failure *resetCreditConsumeFailure) {
	if failure == nil {
		return
	}
	if failure.RefreshErr != nil {
		log.Printf("[账号 %d] 重置额度时刷新 token 失败: %v", account.DBID, failure.RefreshErr)
		writeError(c, http.StatusBadGateway, "上游鉴权失败（401），自动刷新账号失败，请手动刷新后重试")
		return
	}
	switch failure.Status {
	case http.StatusUnauthorized:
		writeError(c, http.StatusBadGateway, "上游鉴权失败（401），请先刷新账号后重试")
	case http.StatusTooManyRequests:
		writeError(c, http.StatusBadGateway, "上游限流（429），请稍后重试")
	case 0:
		if failure.RequestErr != nil {
			writeError(c, http.StatusBadGateway, "重置请求失败："+failure.RequestErr.Error())
			return
		}
		writeError(c, http.StatusBadGateway, "重置请求失败")
	default:
		writeError(c, http.StatusBadGateway, "重置失败："+upstreamResetErrorMessage(failure.Status, failure.Body))
	}
}

func (h *Handler) queryResetCreditsUpstream(ctx context.Context, account *auth.Account, proxyURL string) (*proxy.WhamResetCreditsList, *http.Response, error) {
	if h != nil && h.queryResetCredits != nil {
		return h.queryResetCredits(ctx, account, proxyURL)
	}
	return proxy.QueryWhamResetCredits(ctx, account, proxyURL)
}

func (h *Handler) consumeResetCreditUpstream(ctx context.Context, account *auth.Account, proxyURL, redeemRequestID string) (*proxy.WhamResetResult, *http.Response, error) {
	if h != nil && h.consumeResetCredit != nil {
		return h.consumeResetCredit(ctx, account, proxyURL, redeemRequestID)
	}
	return proxy.ConsumeResetCreditParsed(ctx, account, proxyURL, redeemRequestID)
}

func (h *Handler) recordResetCreditEvent(accountID int64, source string) {
	if h == nil {
		return
	}
	if h.recordAccountEvent != nil {
		h.recordAccountEvent(accountID, "reset_credit", source)
		return
	}
	if h.db != nil {
		h.db.InsertAccountEventAsync(accountID, "reset_credit", source)
	}
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
	probe := h.usageProbeFunc()
	if probe == nil {
		return
	}
	h.resetCreditPostMu.Lock()
	if h.resetCreditPostClosed {
		h.resetCreditPostMu.Unlock()
		return
	}
	parentCtx := h.resetCreditPostCtx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	h.resetCreditPostWG.Add(1)
	h.resetCreditPostMu.Unlock()
	go func() {
		defer h.resetCreditPostWG.Done()
		ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
		defer cancel()
		if err := probe(ctx, account); err != nil {
			log.Printf("[账号 %d] 重置后后台刷新用量失败: %v", account.DBID, err)
		}
	}()
}

// resetCreditLock 返回有效工作区级别的重置互斥锁（按需创建）。同一工作区
// 即使被重复导入为多条数据库记录，手动与自动消费也必须在本进程内串行。
func (h *Handler) resetCreditLock(account *auth.Account) *sync.Mutex {
	key := resetCreditLockKey(account)
	if v, ok := h.resetCreditLocks.Load(key); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	if actual, loaded := h.resetCreditLocks.LoadOrStore(key, mu); loaded {
		return actual.(*sync.Mutex)
	}
	return mu
}

func resetCreditLockKey(account *auth.Account) string {
	if account == nil {
		return "account:nil"
	}
	if identity := strings.TrimSpace(account.EffectiveAccountID()); identity != "" {
		return "workspace:" + identity
	}
	return "db:" + strconv.FormatInt(account.DBID, 10)
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
