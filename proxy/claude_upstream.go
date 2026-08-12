package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeCodeIdentityPrompt 是 Anthropic OAuth 凭据访问 /v1/messages 的准入口令。
// 上游按 system 首块是否等于该串判断请求来自 Claude Code；不一致直接 403，
// 与配额、套餐都无关。因此它必须在网关侧强制注入，不能指望下游客户端自带。
const ClaudeCodeIdentityPrompt = "You are Claude Code, Anthropic's official CLI for Claude."

// claudeMessagesEndpoint 拼出上游 /v1/messages 地址。
// base_url 允许自定义（自建反代），留空时回落到官方域。
func claudeMessagesEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = auth.ClaudeDefaultAPIBaseURL
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

// claudeBetaHeaderValue 把下游客户端声明的 anthropic-beta 与 OAuth 准入标记合并去重。
// 客户端可能自带 fine-grained-tool-streaming 等 beta：直接覆盖会丢功能，
// 直接透传又会丢 oauth-2025-04-20 导致 401。
func claudeBetaHeaderValue(downstream http.Header) string {
	seen := map[string]bool{auth.ClaudeOAuthBetaHeader: true}
	out := []string{auth.ClaudeOAuthBetaHeader}
	if downstream != nil {
		for _, raw := range downstream.Values("anthropic-beta") {
			for _, part := range strings.Split(raw, ",") {
				v := strings.TrimSpace(part)
				if v == "" || seen[v] {
					continue
				}
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return strings.Join(out, ",")
}

// applyClaudeRequestHeaders 按 Claude Code CLI 的契约装配上游请求头。
// 下游客户端的 x-api-key / Authorization 一律不透传：上游认的是账号池里的 OAuth AT。
func applyClaudeRequestHeaders(req *http.Request, account *auth.Account, bearer string, downstreamHeaders http.Header) {
	if req == nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("anthropic-version", auth.ClaudeAPIVersion)
	req.Header.Set("anthropic-beta", claudeBetaHeaderValue(downstreamHeaders))
	req.Header.Set("User-Agent", auth.ClaudeUserAgent())
	req.Header.Set("x-app", "cli")
	req.Header.Set("x-stainless-lang", "js")
	req.Header.Set("x-stainless-package-version", auth.EffectiveClaudeCLIVersion())
	applyAccountCustomHeaders(req, account)
	RecordUpstreamUserAgent(req.Context(), req.Header.Get("User-Agent"))
}

// claudeMetadataUserID 复刻官方 CLI 的 metadata.user_id 形态。
// cli_user_id 在建号时固化并跨刷新保持，上游据此认「同一台机器」；
// 每次请求换值会被当成异常客户端。
func claudeMetadataUserID(account *auth.Account, sessionID string) string {
	cliUserID := account.GetClaudeCLIUserID()
	if cliUserID == "" {
		return ""
	}
	accountUUID := account.GetClaudeAccountUUID()
	if accountUUID == "" {
		accountUUID = "unknown"
	}
	if strings.TrimSpace(sessionID) == "" {
		sessionID = uuid.New().String()
	}
	return fmt.Sprintf("user_%s_account_%s_session_%s", cliUserID, accountUUID, sessionID)
}

// ensureClaudeIdentitySystemPrompt 保证 system 首块是 Claude Code 身份串。
//
// 兼容三种入参形态：缺省、字符串、内容块数组。已经带身份串的请求（真正的 Claude Code
// 客户端）不重复注入。身份块标记 cache_control=ephemeral，与官方 CLI 一致，
// 让上游把这段固定前缀计入提示缓存而不是每轮重新计费。
func ensureClaudeIdentitySystemPrompt(body []byte) []byte {
	identityBlock := map[string]any{
		"type":          "text",
		"text":          ClaudeCodeIdentityPrompt,
		"cache_control": map[string]any{"type": "ephemeral"},
	}

	system := gjson.GetBytes(body, "system")
	switch {
	case !system.Exists():
		out, err := sjson.SetBytes(body, "system", []any{identityBlock})
		if err != nil {
			return body
		}
		return out

	case system.Type == gjson.String:
		text := strings.TrimSpace(system.String())
		blocks := []any{identityBlock}
		// 客户端已自带身份串时只保留其后的自定义部分，避免同一句话出现两遍。
		if rest := strings.TrimSpace(strings.TrimPrefix(text, ClaudeCodeIdentityPrompt)); rest != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": rest})
		}
		out, err := sjson.SetBytes(body, "system", blocks)
		if err != nil {
			return body
		}
		return out

	case system.IsArray():
		blocks := system.Array()
		if len(blocks) > 0 && strings.TrimSpace(blocks[0].Get("text").String()) == ClaudeCodeIdentityPrompt {
			return body
		}
		merged := make([]any, 0, len(blocks)+1)
		merged = append(merged, identityBlock)
		for _, blk := range blocks {
			var decoded any
			if err := json.Unmarshal([]byte(blk.Raw), &decoded); err != nil {
				continue
			}
			merged = append(merged, decoded)
		}
		out, err := sjson.SetBytes(body, "system", merged)
		if err != nil {
			return body
		}
		return out

	default:
		return body
	}
}

// prepareClaudeUpstreamBody 在投递前归一化请求体：
//   - 强制注入 Claude Code 身份 system 块（缺失即 403）；
//   - 补 metadata.user_id（缺失时上游按匿名客户端处理，风控更严）。
func prepareClaudeUpstreamBody(body []byte, account *auth.Account, sessionID string) []byte {
	out := ensureClaudeIdentitySystemPrompt(body)
	if !gjson.GetBytes(out, "metadata.user_id").Exists() {
		if userID := claudeMetadataUserID(account, sessionID); userID != "" {
			if updated, err := sjson.SetBytes(out, "metadata.user_id", userID); err == nil {
				out = updated
			}
		}
	}
	return out
}

// ExecuteClaudeRequest 向 Anthropic 官方 /v1/messages 投递原生 Messages 请求。
// 与 Grok/relay 不同，这里的 requestBody 不经任何协议翻译：下游 /v1/messages 的
// 请求体形态与上游本来就一致，多一层 Anthropic→Codex→Anthropic 往返只会丢字段。
func ExecuteClaudeRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)
	resetWsAcquireAudit(ctx)

	baseURL, bearer := account.ClaudeCredentials()
	if bearer == "" {
		return nil, ErrNoAvailableAccount()
	}
	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}

	sessionID := ""
	if headers != nil {
		sessionID = strings.TrimSpace(headers.Get("Session_id"))
	}
	body := prepareClaudeUpstreamBody(requestBody, account, sessionID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeMessagesEndpoint(baseURL), bytes.NewReader(body))
	if err != nil {
		return nil, ErrInternalError("创建请求失败", err)
	}
	applyClaudeRequestHeaders(req, account, bearer, headers)

	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 Claude 上游失败", err)
	}
	recordClaudeUpstreamObservations(account, resp.Header)
	return resp, nil
}

// recordClaudeUpstreamObservations 采集上游配额头。
//
// Anthropic 对 OAuth 客户端返回统一限额头（anthropic-ratelimit-unified-*）：
// status 为 allowed / allowed_warning / rejected，reset 是窗口重置的 Unix 秒。
// 只有明确取到 reset 才写快照——猜出来的百分比会污染调度打分。
func recordClaudeUpstreamObservations(account *auth.Account, header http.Header) {
	if account == nil || header == nil {
		return
	}
	resetAt := claudeParseUnixHeader(header, "anthropic-ratelimit-unified-reset")
	if resetAt.IsZero() {
		return
	}
	switch strings.ToLower(strings.TrimSpace(header.Get("anthropic-ratelimit-unified-status"))) {
	case "rejected":
		account.SetUsageSnapshot(100, time.Now())
		account.SetReset7dAt(resetAt)
	case "allowed_warning":
		// 上游只说「接近上限」，没给具体余量。取一个保守的高位刻度让调度器降权，
		// 但不到 100——账号此刻仍可用，标满会被直接摘掉。
		account.SetUsageSnapshot(90, time.Now())
		account.SetReset7dAt(resetAt)
	}
}

func claudeParseUnixHeader(header http.Header, key string) time.Time {
	raw := strings.TrimSpace(header.Get(key))
	if raw == "" {
		return time.Time{}
	}
	if sec, err := strconv.ParseInt(raw, 10, 64); err == nil && sec > 0 {
		return time.Unix(sec, 0)
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

// IsClaudeOverloadedError 判断上游是否为 529 overloaded / overloaded_error。
// 这是 Anthropic 侧的瞬时容量问题，不该记到账号头上。
func IsClaudeOverloadedError(statusCode int, body []byte) bool {
	if statusCode == 529 {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.type").String()), "overloaded_error")
}

// applyClaudeCooldown 把 Claude 上游的错误语义映射为账号冷却。
//
// 与 Codex 的差别：
//   - 429 的重置时刻来自 retry-after / anthropic-ratelimit-unified-reset，不是错误体里的 plan 描述；
//   - 529 是上游容量问题，只做短冷却；
//   - 401/403 由上层统一按账号鉴权失效处理，不在这里降档。
func applyClaudeCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	now := time.Now()
	decision := codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited", Model: strings.TrimSpace(model)}

	if IsClaudeOverloadedError(statusCode, body) {
		decision.Reason = "upstream_overloaded"
		decision.Cooldown = 30 * time.Second
		decision.ResetAt = now.Add(decision.Cooldown)
		if store != nil && account != nil {
			store.MarkCooldown(account, decision.Cooldown, decision.Reason)
		}
		return decision
	}

	if statusCode != http.StatusTooManyRequests {
		return codex429Decision{}
	}

	resetAt := time.Time{}
	if resp != nil {
		if d := parseRetryAfterHeader(resp.Header.Get("Retry-After")); d > 0 {
			resetAt = now.Add(d)
		}
		if resetAt.IsZero() {
			resetAt = claudeParseUnixHeader(resp.Header, "anthropic-ratelimit-unified-reset")
		}
	}
	if resetAt.IsZero() || !resetAt.After(now) {
		// 上游没给重置时刻：Claude 订阅是 5 小时滚动窗口，按窗口长度兜底而不是按分钟
		// 重试——分钟级重试只会把同一个号反复撞在同一堵墙上。
		resetAt = now.Add(5 * time.Hour)
	}
	decision.ResetAt = resetAt
	decision.Cooldown = time.Until(resetAt)
	decision.Reason = "rate_limited_5h"
	if store != nil && account != nil {
		store.MarkCooldown(account, decision.Cooldown, decision.Reason)
	}
	return decision
}

// DefaultClaudeModelIDs 是账号未声明 models 白名单时的默认可服务模型集。
// 与 /v1/models 的注册保持一致，否则通用 Key 在模型列表里看得到却永远调度不到。
func DefaultClaudeModelIDs() []string {
	return []string{
		"claude-opus-4-5",
		"claude-sonnet-4-5",
		"claude-haiku-4-5",
	}
}
