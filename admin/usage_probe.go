package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// errWhamUnauthorized 标记 wham 探针遭遇 401。
// wham（ChatGPT 后端额度端点）与 /responses 网关的鉴权口径不同：纯 AT 导入
// （codex_at）的账号可能因 token 缺工作区 claim 等原因在 wham 恒 401，
// 但真实流量完全可用（issue #328）。因此 wham 401 不能单方面定罪封号，
// 须由 ProbeUsageSnapshot 决定是否用 /responses 探针裁决。
var errWhamUnauthorized = errors.New("wham usage probe unauthorized")

type usageProbeRequestFunc func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, ...bool) (*http.Response, error)

func inspectResponsesProbeBody(body []byte) (responsesTerminalOutcome, []byte, error) {
	outcome := responsesTerminalUnknown
	var terminalPayload []byte
	err := proxy.ReadSSEStream(bytes.NewReader(body), func(data []byte) bool {
		classified := classifyResponsesTerminalEvent(data)
		if classified == responsesTerminalUnknown {
			return true
		}
		outcome = classified
		terminalPayload = append(terminalPayload[:0], data...)
		return false
	})
	if err != nil {
		return responsesTerminalUnknown, nil, err
	}
	// 测试替身或非流式兼容端点可能直接返回单个事件 JSON。
	if outcome == responsesTerminalUnknown {
		outcome = classifyResponsesTerminalEvent(body)
		if outcome != responsesTerminalUnknown {
			terminalPayload = append(terminalPayload[:0], body...)
		}
	}
	return outcome, terminalPayload, nil
}

// ProbeUsageSnapshot 主动刷新账号用量。
//
// 优先尝试 /backend-api/wham/usage（零额度成本的结构化端点）；
// 失败时（4xx/5xx/网络）回退到给 /backend-api/codex/responses 发一个最小请求
// （会真实计入用量但保证向下兼容）。Responses 权威限流判定开启且账号已处于
// 用量冷却时，即使 wham 成功也会补一次 /responses 探针：wham 只更新展示元数据，
// 只有真实 Responses 结果能确认账号是否已经恢复。
// 鉴权裁决：wham 401 不单方面封号，由 /responses 回退探针定夺（issue #328）。
func (h *Handler) ProbeUsageSnapshot(ctx context.Context, account *auth.Account) error {
	if account == nil {
		return nil
	}
	// Claude Code OAuth credentials are Anthropic-only. Never send them to the
	// ChatGPT WHAM or Responses probe: those endpoints use a different token
	// issuer and a false 401 would incorrectly quarantine a valid account.
	if account.IsClaudeOAuth() {
		return h.probeUsageViaClaudeMessages(ctx, account)
	}
	if account.IsAntigravityAPI() {
		return errors.New("Antigravity 账号请使用专用配额刷新，不能执行 Codex wham 探针")
	}

	// Grok 账号绝不能走 ChatGPT wham / codex responses 探针——
	// 否则会用错误的上游把有效 token 判成 unauthorized 并封禁。
	if account.IsGrokAPI() {
		return h.probeUsageViaGrokBilling(ctx, account)
	}

	// Agent Identity 无 AccessToken，wham（Bearer）用不了；直接用 /responses 最小探针
	// （ExecuteRequest 会用 AgentAssertion 动态签名），从响应头同步用量快照。
	if account.IsCodexAgentIdentity() {
		return h.probeUsageViaResponses(ctx, account)
	}

	account.Mu().RLock()
	hasToken := account.AccessToken != ""
	account.Mu().RUnlock()
	if !hasToken {
		return nil
	}

	// 默认在限流/冷却（429 或 premium 5h 限流）状态下只做 wham（零成本）。
	// Responses 权威模式例外：若一直只做 wham，已进入冷却的账号永远没有机会用
	// Responses 200 自证恢复，整个池可能长期只剩本地 503。
	limited := account.InLimitedState()
	responsesFallback := h.store.UsageProbeResponsesFallbackEnabled()
	lazyMode := h.store.GetLazyMode()
	authoritativeRecovery := limited && account.IgnoresUsageLimitStatus() && responsesFallback
	whamOnly := (limited && !authoritativeRecovery) || (lazyMode && !authoritativeRecovery) || !responsesFallback

	// 1) 优先用 wham（零成本）
	if err := h.probeUsageViaWham(ctx, account, limited); err == nil {
		if !authoritativeRecovery {
			return nil
		}
		log.Printf("[账号 %d] wham 用量元数据刷新成功，继续用 /responses 裁决限流恢复", account.DBID)
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

// selectClaudeUsageProbeModel picks a low-cost, previously unblocked Claude
// model for the background usage probe. Model discovery is not entitlement
// discovery: Anthropic may advertise a model such as Fable 5 while requiring
// purchased usage credits for a particular plan. Keep such models as a last
// resort, and never retry one while its model-level cooldown is active.
func selectClaudeUsageProbeModel(account *auth.Account) (string, error) {
	if account == nil {
		return "", errors.New("Claude 用量探针缺少账号")
	}
	models := proxy.DefaultClaudeModelIDsForAccount(account)
	account.Mu().RLock()
	explicit := len(account.Models) > 0
	account.Mu().RUnlock()
	if len(models) == 0 {
		if explicit {
			return "", errors.New("Claude 账号模型白名单没有有效的 claude-* 模型")
		}
		models = []string{"claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5"}
	}

	// Prefer the cheapest stable family, then unknown future models, and only
	// probe Fable after every other candidate is unavailable. This prevents a
	// credits_required Fable entry sorted first from creating a probe storm.
	bestModel := ""
	bestRank := 99
	for _, candidate := range models {
		candidate = strings.TrimSpace(candidate)
		lower := strings.ToLower(candidate)
		if candidate == "" || !strings.HasPrefix(lower, "claude-") || account.IsModelRateLimited(candidate) {
			continue
		}
		rank := 3
		switch {
		case strings.Contains(lower, "haiku"):
			rank = 0
		case strings.Contains(lower, "sonnet"):
			rank = 1
		case strings.Contains(lower, "opus"):
			rank = 2
		case strings.Contains(lower, "fable"):
			rank = 4
		}
		if rank < bestRank {
			bestModel = candidate
			bestRank = rank
		}
	}
	if bestModel == "" {
		return "", errors.New("Claude 用量探针跳过：所有模型均处于模型级冷却")
	}
	return bestModel, nil
}

// probeUsageViaClaudeMessages sends a bounded, non-streaming Anthropic Messages
// request and records the unified 5h/7d rate-limit headers. A probe failure is
// returned to the import queue but does not itself ban the account; a
// credits_required response is recorded as a model-only cooldown.
func (h *Handler) probeUsageViaClaudeMessages(ctx context.Context, account *auth.Account) (probeErr error) {
	if account == nil {
		return nil
	}
	var oauthWindows []auth.ClaudeUsageWindow
	defer func() {
		// Count failed/metadata-free attempts for freshness as well. This is a
		// bounded backoff marker, not a quota observation; it prevents a failed
		// provider probe from being retried on every scheduler sweep.
		account.MarkClaudeUsageObservation(time.Now())
		h.recordClaudeUsageProbe(account, probeErr, oauthWindows)
	}()
	// Claude Code exposes a zero-spend OAuth usage endpoint with model-scoped
	// weekly limits. Prefer it so refreshing an account never consumes a message
	// and Fable 5/5.1's shared quota is visible. Keep the Messages probe as a
	// compatibility fallback for older tokens/proxies that do not expose it.
	// Tests inject the Messages executor to provide a fully isolated upstream;
	// skip the real OAuth request in that mode instead of reaching Anthropic.
	if h != nil && h.executeClaudeUsageProbe == nil {
		if windows, err := h.fetchClaudeOAuthUsage(ctx, account); err == nil && len(windows) > 0 {
			oauthWindows = windows
			h.applyClaudeOAuthUsage(account, windows)
			return nil
		} else if err != nil {
			log.Printf("[账号 %d] Claude OAuth usage 端点不可用，回退 Messages 探针: %v", account.DBID, err)
		}
	}
	model, modelErr := selectClaudeUsageProbeModel(account)
	if modelErr != nil {
		return modelErr
	}
	body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"ping"}],"stream":false}`, model))
	var (
		resp *http.Response
		err  error
	)
	if h != nil && h.executeClaudeUsageProbe != nil {
		resp, err = h.executeClaudeUsageProbe(ctx, account, body)
	} else {
		proxyURL := ""
		fingerprintMode := ""
		securityConfig := auth.DefaultClaudeSecurityConfig()
		if h != nil && h.store != nil {
			proxyURL = h.store.ResolveProxyForAccount(account)
			fingerprintMode = account.EffectiveClaudeFingerprintMode(h.store.ClaudeFingerprintModeDefault())
			securityConfig = h.store.ClaudeSecurityConfig()
		}
		// Operator-originated probe: never apply the downstream client policy.
		resp, err = proxy.ExecuteClaudeMessagesRequest(ctx, account, body, proxyURL, nil, fingerprintMode, securityConfig)
	}
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("Claude Messages probe returned nil response")
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return fmt.Errorf("读取 Claude Messages probe 响应失败: %w", readErr)
	}
	if h != nil && h.store != nil {
		if proxy.HandleClaudeModelBillingRejection(h.store, account, model, resp.StatusCode, body) {
			return fmt.Errorf("Claude 模型 %s 需要 usage credits", model)
		}
		// Some compatibility layers wrap a native error payload in HTTP 200.
		// Treat credits_required the same way as the normal 429 path without
		// feeding it into the account-level quota synchronizer.
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "type").String()), "error") {
			if proxy.HandleClaudeModelBillingRejection(h.store, account, model, http.StatusTooManyRequests, body) {
				return fmt.Errorf("Claude 模型 %s 需要 usage credits", model)
			}
		}
		proxy.SyncClaudeUsageState(h.store, account, resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do not mark unauthorized here: OAuth token failures need corroboration
		// from real Claude traffic, while rate-limit state was already synced.
		return fmt.Errorf("Claude Messages probe returned status %d", resp.StatusCode)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("Claude Messages probe returned an empty body")
	}
	// Anthropic normally uses a non-2xx status for errors, but a proxy or
	// compatibility layer may wrap a native error in HTTP 200. Do not mark
	// such a response as a successful sample.
	if !gjson.ValidBytes(body) {
		return fmt.Errorf("Claude Messages probe returned an invalid JSON payload")
	}
	typeName := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "type").String()))
	if typeName == "error" {
		return fmt.Errorf("Claude Messages probe returned an error payload")
	}
	if typeName != "message" {
		return fmt.Errorf("Claude Messages probe returned an invalid message payload")
	}
	if h != nil && h.store != nil {
		h.store.ReportRequestSuccess(account, 0)
	}
	return nil
}

// recordClaudeUsageProbe persists only the outcome metadata needed by the
// account-management UI. It never changes account health/cooldown state and a
// persistence failure is intentionally best-effort: sampling must not block
// request routing or turn a valid OAuth token into an error account.
func (h *Handler) fetchClaudeOAuthUsage(ctx context.Context, account *auth.Account) ([]auth.ClaudeUsageWindow, error) {
	if account == nil {
		return nil, errors.New("Claude usage 缺少账号")
	}
	proxyURL := ""
	if h != nil && h.store != nil {
		proxyURL = h.store.ResolveProxyForAccount(account)
	}
	return auth.NewClaudeAuth(proxyURL).FetchUsage(ctx, account.GetAccessToken())
}

func (h *Handler) applyClaudeOAuthUsage(account *auth.Account, windows []auth.ClaudeUsageWindow) {
	if account == nil || len(windows) == 0 {
		return
	}
	observedAt := time.Now()
	var has7d, has5h bool
	var pct7d float64
	account.ApplyUsageObservation(observedAt, func() {
		for _, window := range windows {
			switch window.Name {
			case "5h":
				account.SetUsageSnapshot5hAt(window.Utilization, window.ResetAt, observedAt)
				has5h = true
			case "7d":
				account.SetUsageSnapshot(window.Utilization, observedAt)
				pct7d = window.Utilization
				if !window.ResetAt.IsZero() {
					account.SetReset7dAt(window.ResetAt)
				}
				has7d = true
			}
		}
		if h != nil && h.store != nil {
			if has7d {
				h.store.PersistUsageSnapshot(account, pct7d)
			} else if has5h {
				h.store.PersistUsageSnapshot5hOnly(account)
			}
		}
	})
}

func (h *Handler) recordClaudeUsageProbe(account *auth.Account, probeErr error, windows []auth.ClaudeUsageWindow) {
	if h == nil || h.db == nil || account == nil || account.DBID <= 0 {
		return
	}
	fields := map[string]interface{}{
		auth.ClaudeUsageProbeAtCredentialKey:    time.Now().UTC().Format(time.RFC3339),
		auth.ClaudeUsageProbeErrorCredentialKey: "",
	}
	if probeErr != nil {
		fields[auth.ClaudeUsageProbeErrorCredentialKey] = security.SafeTruncate(security.SanitizeLog(strings.TrimSpace(probeErr.Error())), 300)
	}
	// Always rewrite the window snapshot together with the probe timestamp so a
	// probe that produced no OAuth windows (fallback Messages path, endpoint
	// unavailable) clears stale model-scoped percentages instead of showing them
	// under a fresh timestamp. The key's presence also marks the row as probed.
	fields[auth.ClaudeUsageWindowsCredentialKey] = "[]"
	if len(windows) > 0 {
		if raw, err := json.Marshal(windows); err == nil {
			fields[auth.ClaudeUsageWindowsCredentialKey] = string(raw)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.db.UpdateCredentials(ctx, account.DBID, fields); err != nil {
		log.Printf("[账号 %d] 持久化 Claude 用量采样状态失败: %v", account.DBID, err)
		return
	}
	// The paged account list is projection-backed and may be cached for up to
	// 30s on large pools. Expire only the Claude snapshot so the next silent
	// poll observes this attempt without disturbing Codex/Grok pages.
	h.invalidateClaudeCatalogCaches()
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
			if isDefinitiveRevokedTokenError(body) {
				// 与缺少 workspace claim 等普通 WHAM 401 不同，token_revoked /
				// token_invalidated 是上游对 OAuth 凭据失效的明确裁决，不需要
				// /responses 二次佐证。否则关闭 fallback 时账号会永久停在“未采样”。
				h.store.ReportRequestFailure(account, "client", 0)
				errorMsg := fmt.Sprintf("用量探针上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
				h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", errorMsg)
				return nil
			}
			// 不在此处上报失败/封号：wham 401 对 codex_at 账号可能是误报，
			// 反复计入失败样本还会污染健康统计。交由 ProbeUsageSnapshot 裁决。
			return fmt.Errorf("%w: 上游返回 %d: %s", errWhamUnauthorized, resp.StatusCode, truncate(string(body), 300))
		case http.StatusTooManyRequests:
			h.store.ReportRequestFailure(account, "client", 0)
		case http.StatusPaymentRequired, http.StatusForbidden:
			// 与 wham 401 不同，deactivated_workspace 是上游对工作区状态的明确裁决
			// （错误体带 detail.code），不存在鉴权口径差异导致的误报；wham-only 模式
			// 下若不在此标错，被封 team 空间的账号会以"可用"无限留在池里、采样
			// 永远失败。仅在错误体确认时动手，裸 402/403 保持通用失败路径。
			if shouldMarkUsageProbeAccountError(resp.StatusCode, body) {
				h.store.ReportRequestFailure(account, "client", 0)
				errorMsg := fmt.Sprintf("用量探针上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
				if resp.StatusCode == http.StatusForbidden && proxy.IsAgentRuntimeDeletedError(body) {
					h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", errorMsg)
				} else {
					h.store.MarkDeactivatedWorkspace(account, errorMsg)
				}
				return nil
			}
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

// probeUsageViaGrokBilling 通过 cli-chat-proxy /v1/billing 拉取套餐与周/月额度。
// 不走 wham，401 才视作凭据失效。
func (h *Handler) probeUsageViaGrokBilling(ctx context.Context, account *auth.Account) error {
	if account == nil {
		return nil
	}
	// API Key 账号可能没有 AT；用 bearer（api_key 或 AT）探测。
	baseURL, bearer := account.GrokCredentials()
	_ = baseURL
	if strings.TrimSpace(bearer) == "" {
		// OAuth 无 AT 时先刷一次
		if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
			if err := h.store.RefreshSingle(ctx, account.DBID); err != nil {
				log.Printf("[账号 %d] Grok billing 探针前刷新失败: %v", account.DBID, err)
			}
		}
	}

	summary, err := proxy.FetchGrokBilling(ctx, account, h.store.ResolveProxyForAccount(account))
	if err != nil {
		errText := err.Error()
		if strings.Contains(strings.ToLower(errText), "unauthorized") {
			h.store.ReportRequestFailure(account, "client", 0)
			h.store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized",
				fmt.Sprintf("Grok billing 探针 401: %s", truncate(errText, 300)))
			return nil
		}
		log.Printf("[账号 %d] Grok billing 探针失败: %v", account.DBID, err)
		return err
	}

	credentials := proxy.ApplyGrokBilling(h.store, account, summary)
	if h.db != nil && len(credentials) > 0 {
		if err := h.db.UpdateCredentials(ctx, account.DBID, credentials); err != nil {
			log.Printf("[账号 %d] Grok billing 写库失败: %v", account.DBID, err)
		}
	}
	return nil
}

// probeUsageViaResponses 原有探针：发送最小 /responses 请求，
// 通过响应头同步 Codex 用量状态。会真实消耗少量 token。
func (h *Handler) probeUsageViaResponses(ctx context.Context, account *auth.Account) error {
	probeStartedAt := time.Now()
	payload := buildConnectionTestPayload(h.store, h.store.GetTestModel())
	executeRequest := usageProbeRequestFunc(proxy.ExecuteRequest)
	if h.executeUsageProbe != nil {
		executeRequest = h.executeUsageProbe
	}
	resp, err := executeRequest(ctx, account, payload, "", h.store.ResolveProxyForAccount(account), "", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	usageState := proxy.SyncCodexUsageState(h.store, account, resp)

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch resp.StatusCode {
	case http.StatusOK:
		outcome, terminalPayload, inspectErr := inspectResponsesProbeBody(body)
		if inspectErr != nil {
			return fmt.Errorf("读取 Responses 恢复探针终止事件失败: %w", inspectErr)
		}
		switch outcome {
		case responsesTerminalUsageLimited:
			h.applyResponsesUsageLimitFailure(account, resp, h.store.GetTestModel(), terminalPayload)
			return nil
		case responsesTerminalFailed:
			return fmt.Errorf("Responses 恢复探针未成功: %s", formatUpstreamTestError(terminalPayload, "上游返回失败终止事件"))
		case responsesTerminalUnknown:
			return fmt.Errorf("Responses 恢复探针缺少明确终止事件")
		}
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
			errorMsg := fmt.Sprintf("用量探针上游返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
			if resp.StatusCode == http.StatusForbidden && proxy.IsAgentRuntimeDeletedError(body) {
				h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", errorMsg)
			} else {
				h.store.MarkDeactivatedWorkspace(account, errorMsg)
			}
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

func isDefinitiveRevokedTokenError(body []byte) bool {
	for _, path := range []string{"error.code", "detail.code", "code"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String())) {
		case "token_revoked", "token_invalidated":
			return true
		}
	}
	return false
}

func shouldMarkUsageProbeAccountError(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusPaymentRequired, http.StatusForbidden:
		return proxy.IsDeactivatedWorkspaceError(body) ||
			(statusCode == http.StatusForbidden && proxy.IsAgentRuntimeDeletedError(body))
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
