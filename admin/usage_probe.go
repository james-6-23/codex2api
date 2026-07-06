package admin

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// ProbeUsageSnapshot 主动刷新账号用量。
//
// 优先尝试 /backend-api/wham/usage（零额度成本的结构化端点）；
// 失败时（4xx/5xx/网络）回退到给 /backend-api/codex/responses 发一个最小请求
// （会真实计入用量但保证向下兼容）。
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
	usage, resp, err := proxy.QueryWhamUsage(ctx, account, h.store.ResolveProxyForAccount(account))
	if resp != nil {
		// QueryWhamUsage 在非 200 时不会读 body；这里读取一小段用于账号错误详情。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			h.store.ReportRequestFailure(account, "client", 0)
			h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", fmt.Sprintf("用量探针 wham 上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300)))
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
	if limited {
		// 限流/冷却态下，用 wham 返回的权威用量窗口重新判定：
		// 若上游已重置窗口、不再限流（例如官方提前重置了 5h/7d 用量），
		// 则主动解除限流冷却，无需等待冷却到期或用户手动测试连接。
		// 仍不调用 ReportRequestSuccess，避免把一次零成本额度查询计入健康成功样本。
		if !applyUsageLimitedAccountState(h.store, account, state) {
			h.store.ClearCooldown(account)
			log.Printf("[账号 %d] wham 显示限流窗口已重置，自动解除限流冷却", account.DBID)
		}
		return nil
	}
	h.store.ReportRequestSuccess(account, 0)
	// 用量未耗尽时重置冷却
	if !applyUsageLimitedAccountState(h.store, account, state) {
		h.store.ClearCooldown(account)
	}
	return nil
}

// probeUsageViaResponses 原有探针：发送最小 /responses 请求，
// 通过响应头同步 Codex 用量状态。会真实消耗少量 token。
func (h *Handler) probeUsageViaResponses(ctx context.Context, account *auth.Account) error {
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
			h.store.ClearCooldown(account)
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
