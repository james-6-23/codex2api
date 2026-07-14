package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// errWhamUnauthorized 标记 wham 探针遭遇 401。
// wham（ChatGPT 后端额度端点）与 /responses 网关的鉴权口径不同：纯 AT 导入
// （codex_at）的账号可能因 token 缺工作区 claim 等原因在 wham 恒 401，
// 但真实流量完全可用（issue #328）。因此 wham 401 不能单方面定罪封号，
// 须由 ProbeUsageSnapshot 决定是否用 /responses 探针裁决。
var errWhamUnauthorized = errors.New("wham usage probe unauthorized")

// ProbeUsageSnapshot 主动刷新账号用量。
//
// 优先尝试 /backend-api/wham/usage（零额度成本的结构化端点）；
// 失败时（4xx/5xx/网络）回退到给 /backend-api/codex/responses 发一个最小请求
// （会真实计入用量但保证向下兼容）。
// 鉴权裁决：wham 401 不单方面封号，由 /responses 回退探针定夺（issue #328）。
func (h *Handler) ProbeUsageSnapshot(ctx context.Context, account *auth.Account) error {
	if account == nil {
		return nil
	}

	account.Mu().RLock()
	hasToken := account.AccessToken != ""
	account.Mu().RUnlock()
	if !hasToken {
		return nil
	}

	// 限流/冷却（429 或 premium 5h 限流）状态下只做 wham（零成本），
	// 失败也不回退 /responses，避免加重限流或额外消耗额度。
	limited := account.InLimitedState()
	whamOnly := limited || h.store.GetLazyMode() || !h.store.UsageProbeResponsesFallbackEnabled()

	// 1) 优先用 wham（零成本）
	if err := h.probeUsageViaWham(ctx, account, limited); err == nil {
		return nil
	} else if errors.Is(err, errWhamUnauthorized) {
		// wham 401 不直接封号（codex_at 账号可能 wham 恒 401 但流量可用，issue #328）：
		// 能回退时交给 /responses 探针做鉴权最终裁决（200 恢复 / 401 才封）；
		// 不能回退时仅记录不封——真正失效的 token 会在真实流量 401 时被网关冷却。
		if whamOnly {
			log.Printf("[账号 %d] wham 探针 401，缺少 /responses 佐证（限流/lazy/回退关闭），跳过封禁: %v", account.DBID, err)
			return err
		}
		log.Printf("[账号 %d] wham 探针 401，交由 /responses 探针裁决鉴权状态: %v", account.DBID, err)
	} else {
		if whamOnly {
			log.Printf("[账号 %d] wham 用量探测失败，已按配置/限流状态跳过 /responses 探针: %v", account.DBID, err)
			return err
		}
		log.Printf("[账号 %d] wham 用量探测失败，回退到 /responses 探针: %v", account.DBID, err)
	}

	// 2) Fallback: 原有的 /responses 最小探针
	return h.probeUsageViaResponses(ctx, account)
}

// probeUsageViaWham 通过 /backend-api/wham/usage 拉取用量，
// 不消耗任何 token 额度。
//
// limited=true 表示账号正处于 429 冷却 / premium 5h 限流状态：本次仅为零成本刷新
// 「主动重置次数」与用量快照，不上报成功、也不清除冷却（冷却解除交给恢复探针/到期判断），
// 避免把一次额度查询误判为账号已恢复。
func (h *Handler) probeUsageViaWham(ctx context.Context, account *auth.Account, limited bool) error {
	probeStartedAt := time.Now()
	usage, resp, err := proxy.QueryWhamUsage(ctx, account, h.store.ResolveProxyForAccount(account))
	if resp != nil {
		// QueryWhamUsage 在非 200 时不会读 body；这里读取一小段用于账号错误详情。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			// 不在此处上报失败/封号：wham 401 对 codex_at 账号可能是误报，
			// 反复计入失败样本还会污染健康统计。交由 ProbeUsageSnapshot 裁决。
			return fmt.Errorf("%w: 上游返回 %d: %s", errWhamUnauthorized, resp.StatusCode, truncate(string(body), 300))
		case http.StatusTooManyRequests:
			h.store.ReportRequestFailure(account, "client", 0)
		}
	}
	if err != nil {
		return err
	}
	if usage == nil {
		return fmt.Errorf("wham returned empty body")
	}

	state := proxy.ApplyWhamUsage(h.store, account, usage)
	// wham 不含订阅到期字段，按需从网页端 /subscriptions 补权威到期时间
	// （带节流，best-effort，失败不影响探针结果）。(issue #360)
	proxy.MaybeSyncSubscriptionExpiry(ctx, h.store, account, h.store.ResolveProxyForAccount(account))
	if limited {
		if state.UsageWindowLimitsIgnored {
			// WHAM remains metadata-only in Responses-authoritative mode. It must
			// not clear a cooldown established by a real Responses failure.
			return nil
		}
		if !state.HasUsage5h && !state.HasUsage7d && !state.Cleared5h {
			// An empty/malformed WHAM payload is not evidence that a cooldown
			// ended. Preserve the existing source state and let the next probe
			// retry with a complete response.
			return nil
		}
		// 限流/冷却态下，用 wham 返回的权威用量窗口重新判定：
		// 若上游已重置窗口、不再限流（例如官方提前重置了 5h/7d 用量），
		// 则主动解除限流冷却，无需等待冷却到期或用户手动测试连接。
		// 仍不调用 ReportRequestSuccess，避免把一次零成本额度查询计入健康成功样本。
		if !applyUsageLimitedAccountState(h.store, account, state) {
			h.store.ClearUsageLimitCooldownSince(account, probeStartedAt)
			log.Printf("[账号 %d] wham 显示限流窗口已重置，自动解除限流冷却", account.DBID)
		}
		return nil
	}
	h.store.ReportRequestSuccess(account, 0)
	// 用量未耗尽时重置冷却
	if !applyUsageLimitedAccountState(h.store, account, state) {
		if state.HasUsage5h || state.HasUsage7d || state.Cleared5h {
			h.store.ClearUsageLimitCooldownSince(account, probeStartedAt)
		}
	}
	return nil
}

// probeUsageViaResponses 原有探针：发送最小 /responses 请求，
// 通过响应头同步 Codex 用量状态。会真实消耗少量 token。
func (h *Handler) probeUsageViaResponses(ctx context.Context, account *auth.Account) error {
	probeStartedAt := time.Now()
	payload := buildConnectionTestPayload(h.store, h.store.GetTestModel())
	resp, err := proxy.ExecuteRequest(ctx, account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	usageState := proxy.SyncCodexUsageState(h.store, account, resp)

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch resp.StatusCode {
	case http.StatusOK:
		h.store.ReportRequestSuccess(account, 0)
		// 只有用量未耗尽时才重置状态
		if !applyUsageLimitedAccountState(h.store, account, usageState) {
			h.store.ClearUsageLimitCooldownSince(account, probeStartedAt)
		}
		return nil
	case http.StatusUnauthorized:
		h.store.ReportRequestFailure(account, "client", 0)
		h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", fmt.Sprintf("用量探针上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300)))
		return nil
	case http.StatusTooManyRequests:
		h.store.ReportRequestFailure(account, "client", 0)
		proxy.Apply429Cooldown(h.store, account, body, resp, h.store.GetTestModel())
		return nil
	default:
		if proxy.IsUsageLimitReachedError(body) {
			h.store.ReportRequestFailure(account, "client", 0)
			proxy.Apply429Cooldown(h.store, account, body, resp, h.store.GetTestModel())
			return nil
		}
		if shouldMarkUsageProbeAccountError(resp.StatusCode, body) {
			h.store.MarkError(account, fmt.Sprintf("用量探针上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300)))
			return nil
		}
		if resp.StatusCode >= 500 {
			h.store.ReportRequestFailure(account, "server", 0)
		} else if resp.StatusCode >= 400 {
			h.store.ReportRequestFailure(account, "client", 0)
		}
		return fmt.Errorf("探针返回状态 %d", resp.StatusCode)
	}
}

func shouldMarkUsageProbeAccountError(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusPaymentRequired, http.StatusForbidden:
		return proxy.IsDeactivatedWorkspaceError(body)
	default:
		return false
	}
}

// ForceUsageProbe 主动触发一次"忽略缓存阈值"的全量用量探针，并立即返回。
// 真正的探针在后台并发执行（受 usage_probe_concurrency 限制）。
func (h *Handler) ForceUsageProbe(c *gin.Context) {
	h.store.TriggerUsageProbeForceAsync()
	payload := gin.H{
		"triggered":   true,
		"concurrency": h.store.GetUsageProbeConcurrency(),
	}
	if h.store.GetLazyMode() || !h.store.UsageProbeResponsesFallbackEnabled() {
		payload["mode"] = "wham_only"
	}
	c.JSON(http.StatusOK, payload)
}
