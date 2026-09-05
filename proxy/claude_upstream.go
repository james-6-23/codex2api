package proxy

// Claude Code(Anthropic)OAuth 账号的上游透传。
//
// 与其它 relay 账号不同:Grok / OpenAI-Responses 中转都会把请求翻译成 Codex
// "Responses" 协议再出站,而 Claude 账号本身就说 Anthropic Messages API,因此这里
// 采用近乎透传——把入站的原始 Anthropic body 直接发往 api.anthropic.com/v1/messages?beta=true,
// 并按真实 Claude Code CLI 2.1.259 抓包补齐 OAuth 凭据必需项与 CLI 特征:
//   - Authorization: Bearer <access_token>
//   - anthropic-beta: claude-code-20250219(非 haiku) + oauth-2025-04-20 核心标记,
//     再按 body 实际字段补齐成对 beta(thinking/context_management/tools/effort/
//     1h 缓存等),最后按白名单过滤透传入站 beta(见 buildClaudeBetaHeader)
//   - system 数组前两块整理为计费块(x-anthropic-billing-header: cc_version=…;
//     cc_entrypoint=cli;)与 "You are Claude Code, Anthropic's official CLI for Claude."
//     声明块，保留已有块的扩展属性。
//   - metadata.user_id = JSON 字符串 {device_id, account_uuid, session_id},
//     device_id 按账号稳定派生(可用 custom_headers.claude_device_id 覆盖)。
//   - X-Claude-Code-Session-Id:使用统一解析、隔离后的请求级会话身份，
//     与 Claude 结构化 metadata.user_id.session_id 同值，跨换号重试稳定。
//   - SDK 固定行为头:X-Stainless-Retry-Count / X-Stainless-Timeout /
//     anthropic-dangerous-direct-browser-access。
//
// 返回原始 *http.Response 交由调用方按 SSE 流式回传,响应本身已是 Anthropic 格式,
// 无需再做协议翻译。

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// claudeMessagesEndpoint 是 Anthropic 官方 Messages API 端点。真实 Claude Code
	// CLI 的 SDK 恒带 ?beta=true 查询参数(逆向 chunk-0017 抓包确认),保持逐字节一致。
	claudeMessagesEndpoint = "https://api.anthropic.com/v1/messages?beta=true"
	// claudeAnthropicVersion 是 Messages API 版本头。
	claudeAnthropicVersion = "2023-06-01"
	// claudeCodeSystemPreamble 是 OAuth 凭据要求的首个 system 块文本。
	claudeCodeSystemPreamble = "You are Claude Code, Anthropic's official CLI for Claude."
	// Sub2API protects integer conversion with math.MaxInt32/2. Keep the same
	// protocol-level guard while leaving normal model-specific limits to the
	// upstream provider (and the optional operator cap below).
	claudeMaxTokensProtocolLimit int64 = math.MaxInt32 / 2
)

// claudeCodeSystemBlockJSON 是注入到 system 数组首位的块(带 ephemeral 缓存标记,
// 与官方客户端一致)。
const claudeCodeSystemBlockJSON = `{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}}`

// claudeCodeSystemBlockNoCacheJSON 是不带 cache_control 的同一声明块。Anthropic 最多
// 接受 4 个 cache_control 块；客户端已用满时再注入带缓存标记的前言会被整体拒绝。
const claudeCodeSystemBlockNoCacheJSON = `{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}`

// claudeCodeSystemBlock1hJSON 是带 1 小时缓存标记的声明块。Anthropic 不允许 1h 块排在 5m
// 块之后，客户端请求 1h 缓存时前言必须同样使用 1h。
const claudeCodeSystemBlock1hJSON = `{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral","ttl":"1h"}}`

// claudeMaxCacheControlBlocks 是 Anthropic Messages API 允许的 cache_control 块上限。
const claudeMaxCacheControlBlocks = 4

// claudeCodeSystemBlockFor 在客户端未用满 cache_control 配额时返回带缓存标记的
// 声明块，否则返回无标记版本。
func claudeCodeSystemBlockFor(body []byte) string {
	if claudeCacheControlBlockCount(body) >= claudeMaxCacheControlBlocks {
		return claudeCodeSystemBlockNoCacheJSON
	}
	if claudeFirstCacheControlTTL(body) == "1h" {
		return claudeCodeSystemBlock1hJSON
	}
	return claudeCodeSystemBlockJSON
}

// defaultClaudeModelIDs 是未设白名单时对外暴露的当前 Claude 模型集(别名形式,
// Anthropic 侧会解析到带日期的具体版本)。模型演进时可在此维护,或用账号 Models
// 白名单 / 定价页覆盖。
var defaultClaudeModelIDs = []string{
	"claude-opus-4-5",
	"claude-sonnet-4-5",
	"claude-haiku-4-5",
}

// DefaultClaudeModelIDsForAccount 返回该 Claude 账号对外可见的原生模型:优先
// 账号 Models 白名单,否则用当前默认集。历史/误配的非 claude-* 条目必须在
// 目录源头过滤,避免 /v1/models 发布一个调度器随后必然拒绝的模型。
func DefaultClaudeModelIDsForAccount(account *auth.Account) []string {
	if account == nil {
		return nil
	}
	account.Mu().RLock()
	whitelist := append([]string(nil), account.Models...)
	account.Mu().RUnlock()
	if len(whitelist) > 0 {
		visible := make([]string, 0, len(whitelist))
		seen := make(map[string]struct{}, len(whitelist))
		for _, model := range whitelist {
			model = strings.TrimSpace(model)
			key := strings.ToLower(model)
			if !strings.HasPrefix(key, "claude-") {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			visible = append(visible, model)
		}
		return visible
	}
	return append([]string(nil), defaultClaudeModelIDs...)
}

// claudeAccountSupportsModel 判断 Claude Code OAuth 账号能否服务指定模型。
// 若账号设置了显式 Models 白名单,以白名单为准;否则默认放行 claude-* 模型。
func claudeAccountSupportsModel(account *auth.Account, model string) bool {
	if account == nil {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(model), "claude-") {
		return false
	}
	account.Mu().RLock()
	whitelist := append([]string(nil), account.Models...)
	account.Mu().RUnlock()
	if len(whitelist) > 0 {
		for _, m := range whitelist {
			if strings.EqualFold(strings.TrimSpace(m), model) {
				return true
			}
		}
		return false
	}
	return strings.HasPrefix(strings.ToLower(model), "claude")
}

// markClaudeNativeRoute 给 Claude 上游响应打上原生路由标记,复用 handler 里既有的
// 原生 Anthropic Messages SSE 透传路径(forwardGrokNativeResponseTo),无需新写流式
// 处理。标记头名沿用现有常量,语义为"上游已是原生目标协议,直接转发不再翻译"。
func markClaudeNativeRoute(resp *http.Response) {
	if resp != nil && resp.Header != nil {
		resp.Header.Set(grokNativeRouteHeader, "1")
	}
}

// claudeClientPolicyErrorCode marks a request rejected by the gateway-side
// Claude Code client policy (platform/version), never by the upstream.
const claudeClientPolicyErrorCode = "claude_client_policy"

// ExecuteClaudeMessagesRequest 把入站 Anthropic Messages 请求透传给 Claude Code
// OAuth 账号对应的上游,返回原始上游响应。它不做客户端平台/版本门禁:管理端的
// 探测/测连/用量采样都走这里,因为它们不是下游客户端流量(见 issue: 开启
// claude_code_cli_only 后 nil header 探针会被自己的策略拒掉并把整池标错)。
func ExecuteClaudeMessagesRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header, fingerprintMode string, securityConfigs ...auth.ClaudeSecurityConfig) (*http.Response, error) {
	return ExecuteClaudeMessagesRequestWithPolicy(ctx, account, requestBody, proxyOverride, headers, fingerprintMode, auth.ClaudeClientPolicy{}, securityConfigs...)
}

// ExecuteClaudeMessagesRequestWithPolicy performs client platform/version
// preflight before touching the Anthropic transport. The legacy function above
// intentionally keeps the any/passthrough default for non-handler callers.
func ExecuteClaudeMessagesRequestWithPolicy(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header, fingerprintMode string, clientPolicy auth.ClaudeClientPolicy, securityConfigs ...auth.ClaudeSecurityConfig) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// A retry can reuse the request context. Clear any previous attempt's
	// observation before building this attempt so a transport failure cannot
	// make UsageLog attribute the old upstream User-Agent to the new request.
	resetUpstreamUserAgentAudit(ctx)
	if account == nil {
		return nil, ErrNoAvailableAccount()
	}

	account.Mu().RLock()
	accessToken := strings.TrimSpace(account.AccessToken)
	proxyURL := account.ProxyURL
	// 该账号绑定的稳定指纹(导入时生成,存于 credentials.custom_headers)。
	fingerprint := cloneStringMap(account.CustomHeaders)
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}
	model := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	decision, policyErr := auth.ValidateClaudeClientRequest(clientPolicy, headers.Get("User-Agent"), model)
	if policyErr != nil {
		return nil, ErrBadRequest("Claude 客户端策略无效: " + policyErr.Error())
	}
	if !decision.Allowed {
		return nil, &Error{Code: claudeClientPolicyErrorCode, Message: decision.DetailMessage(), Type: ErrorTypeInvalidRequest, Retryable: false, HTTPStatus: decision.HTTPStatus()}
	}

	securityConfig := auth.DefaultClaudeSecurityConfig()
	if len(securityConfigs) > 0 {
		securityConfig = auth.NormalizeClaudeSecurityConfig(securityConfigs[0])
	}
	// 会话身份:handler 已按请求会话派生稳定 UUIDv7 放入 ctx;管理端探针等
	// 无会话上下文的调用方这里兜底生成,保证 X-Claude-Code-Session-Id 恒存在。
	sessionID := claudeSessionIDFromContext(ctx)
	if sessionID == "" {
		// 管理端等直接调用方没有 handler 上下文，仍统一解析头/body 的来源。
		identity := resolveClaudeRequestSessionIdentity(headers, requestBody)
		sessionID = claudeUpstreamSessionID(identity.explicitUpstreamID)
		if sessionID == "" {
			sessionID = NewUpstreamSessionUUID()
		}
		ctx = WithClaudeSessionID(ctx, sessionID)
	}
	// Canonicalize before sending so the handler can run the exact same body
	// through Prompt Filter. The ingress body retained in gin remains untouched
	// for NewAPI signature verification and audit correlation.
	body, err := prepareClaudeRequestBody(requestBody, securityConfig, claudeBodyIdentity{
		deviceID:    account.ClaudeDeviceID(),
		accountUUID: account.ClaudeAccountUUID(),
		sessionID:   sessionID,
	})
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	stream := gjson.GetBytes(body, "stream").Bool()

	client := getPooledClient(account, proxyURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeMessagesEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInternalError("创建 Claude 请求失败", err)
	}
	applyClaudeMessagesHeadersWithVersion(req, accessToken, headers, stream, body, fingerprint, fingerprintMode, decision.RewriteVersion, securityConfig)

	if perr := applyClaudeOutboundVersionAlignment(req, claudeOutboundRequiredVersion(decision, model)); perr != nil {
		return nil, perr
	}

	if aligned := alignClaudeBillingBlock(body, req.Header.Get("User-Agent")); !bytes.Equal(aligned, body) {
		req.Body = io.NopCloser(bytes.NewReader(aligned))
		req.ContentLength = int64(len(aligned))
		req.Header.Set("Content-Length", strconv.Itoa(len(aligned)))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(aligned)), nil
		}
	}

	if err := ConsumeAPIKeyModelRequestQuota(ctx, model); err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 Anthropic Messages API 失败", err)
	}
	return resp, nil
}

type claudeSessionIDContextKey struct{}

// WithClaudeSessionID 把本次请求的稳定会话 UUID 放入 ctx,用于保证
// X-Claude-Code-Session-Id 头与 metadata.user_id.session_id 字段同值
// (真实 Claude Code 两者恒为同一会话 ID)。
func WithClaudeSessionID(ctx context.Context, sessionID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(sessionID) == "" {
		return ctx
	}
	return context.WithValue(ctx, claudeSessionIDContextKey{}, strings.TrimSpace(sessionID))
}

// claudeSessionIDFromContext 读取 ctx 中的稳定会话 UUID;不存在返回空串。
func claudeSessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(claudeSessionIDContextKey{}).(string); ok {
		return v
	}
	return ""
}

// claudeUpstreamSessionID 把 handler 解析出的上游会话键归一为 UUID 形态:
// 已是 UUID 则原样透传,否则确定性派生 UUIDv7(真实 CLI 的会话 ID 恒为 UUID)。
func claudeUpstreamSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if _, err := uuid.Parse(raw); err == nil {
		return raw
	}
	return DeriveStableSessionUUIDv7("codex2api:claude-session:" + raw)
}

// resolveClaudeRequestSessionIdentity 仅在 Claude 原生路径识别 CLI 身份：
// 专用会话头 > 结构化 user_id.session_id > 既有通用会话来源。
// 仍交给既有解析器处理本地 affinity，并由 handler 按 API Key 隔离上游身份。
func resolveClaudeRequestSessionIdentity(headers http.Header, body []byte) requestSessionIdentity {
	raw := strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id"))
	if raw == "" {
		if metadata := claudeMetadataIdentity(body); metadata != nil {
			_ = json.Unmarshal(metadata["session_id"], &raw)
			raw = strings.TrimSpace(raw)
		}
	}
	if raw == "" {
		return resolveRequestSessionIdentity(headers, body)
	}
	normalized := headers.Clone()
	if normalized == nil {
		normalized = make(http.Header)
	}
	normalized.Set("Session-Id", raw)
	return resolveRequestSessionIdentity(normalized, body)
}

// claudeMetadataIdentity 只识别 Claude 的结构化身份，业务字符串/其他 JSON 不改写。
func claudeMetadataIdentity(body []byte) map[string]json.RawMessage {
	userID := gjson.GetBytes(body, "metadata.user_id")
	if userID.Type != gjson.String {
		return nil
	}
	var identity map[string]json.RawMessage
	if json.Unmarshal([]byte(userID.String()), &identity) != nil || identity == nil {
		return nil
	}
	for _, key := range []string{"device_id", "account_uuid", "session_id"} {
		if _, ok := identity[key]; ok {
			return identity
		}
	}
	return nil
}

// setIfAbsentFromIncoming 在入站未携带该头时给 req 设置默认值(真实客户端的值优先)。
func setIfAbsentFromIncoming(reqHeader http.Header, incoming http.Header, name, value string) {
	if incoming != nil {
		if v := strings.TrimSpace(incoming.Get(name)); v != "" {
			reqHeader.Set(name, v)
			return
		}
	}
	reqHeader.Set(name, value)
}

// applyClaudeMessagesHeaders 设置透传请求头。
//
// 指纹一致性策略(由 fingerprintMode 决定,来自账号级覆盖 > 全局默认):
//   - preserve(默认):入站真实 Claude Code 客户端的身份头优先保留,缺失才用账号
//     绑定指纹补齐——它本身就是一致的,伪造反而破坏一致性。
//   - force:无条件用账号绑定指纹覆盖入站身份头,保证该账号对 Anthropic 始终呈现
//     同一套 Claude Code 身份(强制替换,防跨客户端指纹漂移)。
//
// fingerprint 为账号绑定指纹头(规范化头名→值),来自 credentials.custom_headers。
func applyClaudeMessagesHeaders(req *http.Request, accessToken string, incoming http.Header, stream bool, body []byte, fingerprint map[string]string, fingerprintMode string, securityConfigs ...auth.ClaudeSecurityConfig) {
	securityConfig := auth.DefaultClaudeSecurityConfig()
	if len(securityConfigs) > 0 {
		securityConfig = auth.NormalizeClaudeSecurityConfig(securityConfigs[0])
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	// anthropic-version:优先保留入站真实客户端的值。
	if v := strings.TrimSpace(incoming.Get("anthropic-version")); v != "" {
		req.Header.Set("anthropic-version", v)
	} else {
		req.Header.Set("anthropic-version", claudeAnthropicVersion)
	}
	req.Header.Set("anthropic-beta", buildClaudeBetaHeader(incoming, securityConfig, body))
	// OAuth 凭据不带 x-api-key;若入站客户端塞了，务必剔除避免冲突。
	req.Header.Del("x-api-key")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	// SDK 固定行为头(真实 CLI 抓包恒定值):每次出站请求都带,重试计数按本次
	// HTTP 请求计为 0(换号/重试会重建请求)。入站已带时优先保留入站值。
	setIfAbsentFromIncoming(req.Header, incoming, "X-Stainless-Retry-Count", "0")
	setIfAbsentFromIncoming(req.Header, incoming, "X-Stainless-Timeout", "600")
	setIfAbsentFromIncoming(req.Header, incoming, "anthropic-dangerous-direct-browser-access", "true")
	// 使用 handler 已完成隔离的唯一会话身份，不能再被入站头覆盖。
	if sid := claudeSessionIDFromContext(req.Context()); sid != "" {
		req.Header.Set("X-Claude-Code-Session-Id", sid)
	} else if v := strings.TrimSpace(incoming.Get("X-Claude-Code-Session-Id")); v != "" {
		req.Header.Set("X-Claude-Code-Session-Id", claudeUpstreamSessionID(v))
	}

	// 指纹 map 键大小写不定(来自 custom_headers),统一小写后按小写头名查。
	fpLower := make(map[string]string, len(fingerprint))
	for k, v := range fingerprint {
		fpLower[strings.ToLower(strings.TrimSpace(k))] = v
	}
	// force 模式:账号指纹优先,无条件覆盖入站身份头(有指纹才覆盖,避免抹成空)。
	// preserve 模式:入站有则保留,无则用账号指纹补齐。
	force := auth.NormalizeClaudeFingerprintMode(fingerprintMode) == auth.ClaudeFingerprintModeForce
	// preserve 只放行真实 Claude Code CLI 的入站身份(UA 可解析为 CLI 版本)。
	// 第三方客户端(opencode、curl 等)自带 UA 若原样透传,会与 OAuth 凭据预期的
	// claude-cli 指纹不一致,因此视同 force,统一呈现账号绑定的 Claude Code 身份。
	if !force {
		if _, isCLI := auth.ParseClaudeClientVersion(strings.TrimSpace(incoming.Get("User-Agent"))); !isCLI {
			force = true
		}
	}
	for _, name := range auth.ClaudeIdentityHeaderNames {
		fpVal := strings.TrimSpace(fpLower[name])
		if force {
			// Legacy accounts may contain only a partial fingerprint. In force
			// mode every identity header must still be deterministic; otherwise
			// the missing field would inherit a different downstream client and
			// silently defeat the stable-account contract.
			if fpVal == "" {
				fpVal = defaultClaudeIdentityHeader(name)
			}
			if fpVal != "" {
				req.Header.Set(name, fpVal)
			}
			continue
		}
		if v := strings.TrimSpace(incoming.Get(name)); v != "" {
			req.Header.Set(name, v)
			continue
		}
		if fpVal != "" {
			req.Header.Set(name, fpVal)
		}
	}
	// 保底:连指纹都没有(老账号未生成指纹)时,给一个稳定的默认 UA,避免空 UA 破绽。
	if strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		req.Header.Set("User-Agent", "claude-cli/"+auth.EffectiveClaudeCLIVersion()+" (external, cli)")
	}
	// Keep Claude on the same request-scoped User-Agent audit path as Codex,
	// Grok, and WebSocket transports. Record only the final sanitized header
	// after preserve/force resolution so the Usage page can show whether the
	// upstream identity was actually rewritten.
	RecordUpstreamUserAgent(req.Context(), req.Header.Get("User-Agent"))
}

// applyClaudeMessagesHeadersWithVersion is the policy-aware variant used by
// ExecuteClaudeMessagesRequestWithPolicy. Keeping the legacy helper signature
// avoids changing existing callers/tests while still recording the final UA.
func applyClaudeMessagesHeadersWithVersion(req *http.Request, accessToken string, incoming http.Header, stream bool, body []byte, fingerprint map[string]string, fingerprintMode, rewriteVersion string, securityConfigs ...auth.ClaudeSecurityConfig) {
	applyClaudeMessagesHeaders(req, accessToken, incoming, stream, body, fingerprint, fingerprintMode, securityConfigs...)
	if strings.TrimSpace(rewriteVersion) != "" {
		rewritten := rewriteClaudeCLIUserAgentVersion(req.Header.Get("User-Agent"), rewriteVersion)
		if rewritten != "" {
			req.Header.Set("User-Agent", rewritten)
			RecordUpstreamUserAgent(req.Context(), rewritten)
		}
	}
}

func rewriteClaudeCLIUserAgentVersion(userAgent, version string) string {
	return auth.RewriteClaudeCLIUserAgentVersion(userAgent, version)
}

// claudeOutboundRequiredVersion 取入站门控得出的 required 与模型下限中的较大者。
// 入站非 CLI 时 decision.RequiredVersion 为空,但 force 指纹可能把出站改成 CLI UA,
// 此时仍必须遵守模型下限。
func claudeOutboundRequiredVersion(decision auth.ClaudeClientDecision, model string) string {
	required := strings.TrimSpace(decision.RequiredVersion)
	floor := auth.ClaudeModelMinimumVersion(model)
	if floor == "" {
		return required
	}
	if required == "" {
		return floor
	}
	if cmp, err := auth.CompareClaudeClientVersions(floor, required); err == nil && cmp > 0 {
		return floor
	}
	return required
}

// alignClaudeOutboundUserAgent 保证最终出站 CLI UA 版本不低于 required。
// 低于时抬到生效版本;生效版本仍不够则返回拒绝消息(调用方本地 426,不发上游)。
func alignClaudeOutboundUserAgent(outbound, required string) (string, string) {
	if strings.TrimSpace(required) == "" {
		return outbound, ""
	}
	outVersion, isCLI := auth.ParseClaudeClientVersion(outbound)
	if !isCLI {
		return outbound, ""
	}
	// outVersion just came from ParseClaudeClientVersion (always a valid
	// SemVer when isCLI) and required is always either an already-validated
	// decision.RequiredVersion or a fixed auth.ClaudeModelMinimumVersion
	// constant, so a compare error here is unreachable in practice.
	if cmp, err := auth.CompareClaudeClientVersions(outVersion, required); err != nil || cmp >= 0 {
		return outbound, ""
	}
	effective := auth.EffectiveClaudeCLIVersion()
	// effective always comes from auth.EffectiveClaudeCLIVersion, which only
	// ever returns the built-in constant or a previously validated synced
	// version, so this compare error is likewise unreachable in practice.
	if cmp, err := auth.CompareClaudeClientVersions(effective, required); err != nil || cmp < 0 {
		return outbound, fmt.Sprintf("Claude Code CLI outbound version %s is below required %s (effective %s); update client_version or wait for CLI version sync", outVersion, required, effective)
	}
	rewritten := auth.RewriteClaudeCLIUserAgentVersion(outbound, effective)
	if rewritten == "" {
		// RewriteClaudeCLIUserAgentVersion failed even though outbound was
		// just confirmed to be a CLI UA and effective a valid version; fail
		// closed instead of silently keeping the stale, too-old outbound UA
		// this function exists to reject.
		return outbound, fmt.Sprintf("Claude Code CLI outbound version %s could not be rewritten to %s", outVersion, effective)
	}
	return rewritten, ""
}

// applyClaudeOutboundVersionAlignment aligns req's outbound User-Agent to the
// required Claude Code CLI version, recording the final UA on the request's
// upstream User-Agent audit when it changes. Returns a local 426 *Error
// (never sent upstream) when the effective CLI version still can't satisfy
// required.
func applyClaudeOutboundVersionAlignment(req *http.Request, required string) *Error {
	outbound := req.Header.Get("User-Agent")
	finalUA, deny := alignClaudeOutboundUserAgent(outbound, required)
	if deny != "" {
		return &Error{Code: "claude_client_policy", Message: deny, Type: ErrorTypeInvalidRequest, Retryable: false, HTTPStatus: http.StatusUpgradeRequired}
	}
	if finalUA != outbound {
		req.Header.Set("User-Agent", finalUA)
		RecordUpstreamUserAgent(req.Context(), finalUA)
	}
	return nil
}

// defaultClaudeIdentityHeader is a deterministic compatibility fallback for
// legacy accounts whose persisted fingerprint predates one of the current
// Claude Code identity headers. It is deliberately a fixed, provider-shaped
// value rather than a per-request random value, so force mode cannot drift.
func defaultClaudeIdentityHeader(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "user-agent":
		return "claude-cli/" + auth.EffectiveClaudeCLIVersion() + " (external, cli)"
	case "x-app":
		return "cli"
	case "x-stainless-lang":
		return "js"
	case "x-stainless-package-version":
		return "0.112.1"
	case "x-stainless-os":
		return "MacOS"
	case "x-stainless-arch":
		return "arm64"
	case "x-stainless-runtime":
		return "node"
	case "x-stainless-runtime-version":
		return "v26.3.0"
	default:
		return ""
	}
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// claudeInvisibleRunes 是应从请求文字中剔除的不可见/格式字符:零宽、词连接符、
// BOM、以及会误导审核/看起来像规避手段的双向控制符。剔除它们让请求更"正常"、
// 反而降低被标记概率,且不改变可见文字与语义。
// claudeInvisibleRune 只认双向文本控制符——它们在提示词里没有正当用途,却是
// "trojan source" 式视觉欺骗的经典载体,剔除后请求更"正常"、降低被标记概率。
//
// 刻意**不**剔除零宽字符(U+200B/200C/200D/2060/FEFF/180E):它们是合法正文而非
// 控制信号。U+200D 是 ZWJ,emoji 序列靠它成词(👩‍💻 = U+1F469 U+200D U+1F4BB);
// U+200C 是 ZWNJ,波斯语/印地语等靠它区分词形(می‌روم ≠ میروم)。一并剔除会静默
// 改写用户正文——把 emoji 拆成几个独立字符、把非英语文本改成错误词形。
func claudeInvisibleRune(r rune) bool {
	switch r {
	case 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // bidi embedding / override / pop
		0x2066, 0x2067, 0x2068, 0x2069: // bidi isolates
		return true
	}
	return false
}

// sanitizeClaudeRequestText 对请求体做安全净化:剔除双向文本控制符。JSON 的结构
// 字符与键均为 ASCII,不受影响;仅影响字符串值内的文字。净化后若不再是合法
// JSON(理论上不会),回退原始体。
//
// 这里刻意不做 Unicode NFC 归一。Claude Code 会把文件路径与文件内容原样送进
// 请求,而 macOS 文件系统用的是 NFD——归一会把 NFD 路径改写成预组合形态,模型
// 照抄回来的路径就找不到那个文件了。净化只做纯删除,不改写任何可见文字。
func sanitizeClaudeRequestText(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	text := string(body)
	var b strings.Builder
	b.Grow(len(text))
	changed := false
	for _, r := range text {
		if claudeInvisibleRune(r) {
			changed = true
			continue
		}
		b.WriteRune(r)
	}
	if !changed {
		return body
	}
	out := []byte(b.String())
	if !gjson.ValidBytes(out) {
		return body
	}
	return out
}

// normalizeClaudeRequestBody applies the same canonicalization and egress
// safety policy used for native Claude requests. It intentionally does not
// inject the trusted Claude Code system preamble; callers may do that after
// Prompt Filter has captured the user-visible request text.
func normalizeClaudeRequestBody(body []byte, cfg auth.ClaudeSecurityConfig) ([]byte, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, nil
	}
	cfg = auth.NormalizeClaudeSecurityConfig(cfg)
	out := sanitizeClaudeRequestText(body)
	root := gjson.ParseBytes(out)
	if !root.IsObject() {
		return nil, fmt.Errorf("Claude request body must be a JSON object")
	}
	for field, allowed := range map[string]bool{
		"service_tier":      cfg.AllowServiceTier,
		"inference_geo":     cfg.AllowInferenceGeo,
		"speed":             cfg.AllowSpeed,
		"safety_identifier": cfg.AllowSafetyIdentifier,
		"stream_options":    true,
	} {
		if allowed {
			continue
		}
		var err error
		out, err = sjson.DeleteBytes(out, field)
		if err != nil {
			return nil, fmt.Errorf("remove Claude field %s: %w", field, err)
		}
	}
	if includeObfuscation := gjson.GetBytes(out, "stream_options.include_obfuscation"); includeObfuscation.Exists() {
		var err error
		out, err = sjson.DeleteBytes(out, "stream_options.include_obfuscation")
		if err != nil {
			return nil, fmt.Errorf("remove Claude stream option: %w", err)
		}
		if streamOptions := gjson.GetBytes(out, "stream_options"); streamOptions.IsObject() && len(streamOptions.Map()) == 0 {
			out, err = sjson.DeleteBytes(out, "stream_options")
			if err != nil {
				return nil, fmt.Errorf("remove empty Claude stream_options: %w", err)
			}
		}
	}
	// Sub2API accepts the legacy max_tokens_to_sample alias. Anthropic's
	// Messages endpoint expects max_tokens, so normalize the alias before the
	// request reaches Prompt Filter or the upstream. A conflicting pair is
	// rejected instead of silently choosing one value.
	legacyMaxTokens := gjson.GetBytes(out, "max_tokens_to_sample")
	currentMaxTokens := gjson.GetBytes(out, "max_tokens")
	if legacyMaxTokens.Exists() {
		if legacyMaxTokens.Type == gjson.Null {
			var err error
			out, err = sjson.DeleteBytes(out, "max_tokens_to_sample")
			if err != nil {
				return nil, fmt.Errorf("remove null max_tokens_to_sample: %w", err)
			}
		} else if currentMaxTokens.Exists() {
			legacyValue, legacyErr := parseClaudeMaxTokens(legacyMaxTokens, cfg)
			currentValue, currentErr := parseClaudeMaxTokens(currentMaxTokens, cfg)
			if legacyErr != nil {
				return nil, legacyErr
			}
			if currentErr != nil {
				return nil, currentErr
			}
			if legacyValue != currentValue {
				return nil, fmt.Errorf("max_tokens and max_tokens_to_sample must match")
			}
			var err error
			out, err = sjson.DeleteBytes(out, "max_tokens_to_sample")
			if err != nil {
				return nil, fmt.Errorf("remove max_tokens_to_sample: %w", err)
			}
		} else {
			var err error
			out, err = sjson.SetRawBytes(out, "max_tokens", []byte(legacyMaxTokens.Raw))
			if err != nil {
				return nil, fmt.Errorf("normalize max_tokens_to_sample: %w", err)
			}
			out, err = sjson.DeleteBytes(out, "max_tokens_to_sample")
			if err != nil {
				return nil, fmt.Errorf("remove max_tokens_to_sample: %w", err)
			}
		}
	}
	if maxTokens := gjson.GetBytes(out, "max_tokens"); maxTokens.Exists() {
		if _, err := parseClaudeMaxTokens(maxTokens, cfg); err != nil {
			return nil, err
		}
	}
	// context_management 不再无条件剥离:真实 Claude Code CLI 每次请求都携带它,
	// 且与 context-management-2025-06-27 beta 成对出现(见 buildClaudeBetaHeader)。
	// 网关自身无状态,透传该字段不影响会话,只影响单次请求内的上下文编辑行为。
	tools := gjson.GetBytes(out, "tools")
	if tools.Exists() {
		if !tools.IsArray() {
			return nil, fmt.Errorf("tools must be an array")
		}
		items := tools.Array()
		if cfg.MaxToolCount > 0 && len(items) > cfg.MaxToolCount {
			return nil, fmt.Errorf("tools exceeds ClaudeCode safety limit (%d)", cfg.MaxToolCount)
		}
		var schemaBytes int64
		for _, item := range items {
			schemaBytes += int64(len(item.Raw))
			if cfg.MaxToolSchemaBytes > 0 && schemaBytes > cfg.MaxToolSchemaBytes {
				return nil, fmt.Errorf("tool schema exceeds ClaudeCode safety limit (%d bytes)", cfg.MaxToolSchemaBytes)
			}
		}
	}
	return out, nil
}

func parseClaudeMaxTokens(result gjson.Result, cfg auth.ClaudeSecurityConfig) (int64, error) {
	value, parseErr := strconv.ParseInt(strings.TrimSpace(result.Raw), 10, 64)
	if result.Type != gjson.Number || parseErr != nil || value < 0 {
		return 0, fmt.Errorf("max_tokens must be a non-negative integer")
	}
	if value > claudeMaxTokensProtocolLimit {
		return 0, fmt.Errorf("max_tokens exceeds Claude protocol limit (%d)", claudeMaxTokensProtocolLimit)
	}
	cfg = auth.NormalizeClaudeSecurityConfig(cfg)
	if cfg.MaxOutputTokens > 0 && value > cfg.MaxOutputTokens {
		return 0, fmt.Errorf("max_tokens exceeds ClaudeCode safety limit (%d)", cfg.MaxOutputTokens)
	}
	return value, nil
}

// claudeBodyIdentity 是出站 body 注入所需的账号/会话身份。deviceID/accountUUID
// 来自账号,sessionID 是本次请求级稳定会话 UUIDv7(与 X-Claude-Code-Session-Id 同值)。
type claudeBodyIdentity struct {
	deviceID    string
	accountUUID string
	sessionID   string
}

// prepareClaudeRequestBody is the canonical body used by both Prompt Filter
// and the native Claude transport. Trusted Claude Code system metadata is
// injected only after user-controlled fields have been normalized and bounded.
func prepareClaudeRequestBody(body []byte, cfg auth.ClaudeSecurityConfig, id claudeBodyIdentity) ([]byte, error) {
	normalized, err := normalizeClaudeRequestBody(body, cfg)
	if err != nil {
		return nil, err
	}
	// 客户端会话文件损坏时会回传空/截断签名的 thinking 块，上游必然 400；
	// 文档允许省略历史 thinking，发送前直接丢弃。
	if cleaned, dropped := dropUnsignedClaudeThinkingBlocks(normalized); dropped > 0 {
		log.Printf("[claude-thinking-signature] 丢弃 %d 个签名为空或截断的 thinking 块", dropped)
		normalized = cleaned
	}
	// 思考常开的模型（Fable / Mythos）拒绝 thinking.type=disabled，直接省略该参数。
	if cleaned, dropped := dropClaudeDisabledThinking(normalized); dropped {
		log.Printf("[claude-thinking-signature] 模型 %s 不接受 thinking.type=disabled，已移除该参数", gjson.GetBytes(normalized, "model").String())
		normalized = cleaned
	}
	normalized = injectClaudeCodeSystemPrompt(normalized)
	return injectClaudeMetadataUserID(normalized, id), nil
}

// injectClaudeMetadataUserID 按真实 Claude Code CLI 的形态注入
// metadata.user_id = JSON 字符串 {"device_id","account_uuid","session_id"}。
// 已有 Claude 身份对象按选中账号和唯一会话更新，保留其余字段；业务 user_id 保留。
func injectClaudeMetadataUserID(body []byte, id claudeBodyIdentity) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	identity := claudeMetadataIdentity(body)
	if existing := gjson.GetBytes(body, "metadata.user_id"); existing.Exists() && existing.Type != gjson.Null {
		if strings.TrimSpace(existing.String()) != "" && identity == nil {
			return body
		}
	}
	if id.deviceID == "" && id.accountUUID == "" && id.sessionID == "" {
		return body
	}
	if identity == nil {
		identity = make(map[string]json.RawMessage)
	}
	updates := map[string]string{"device_id": id.deviceID, "account_uuid": id.accountUUID, "session_id": id.sessionID}
	var previousSession, parentSession string
	_ = json.Unmarshal(identity["session_id"], &previousSession)
	_ = json.Unmarshal(identity["parent_session_id"], &parentSession)
	if previousSession != "" && parentSession == previousSession {
		// 子代理可能把同一会话同时写到 parent_session_id；关联值需一起更新。
		// 真正不同的父会话保持不变，不能擅自改成当前子会话。
		updates["parent_session_id"] = id.sessionID
	}
	for key, value := range updates {
		encoded, err := json.Marshal(value)
		if err != nil {
			return body
		}
		identity[key] = encoded
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return body
	}
	out, err := sjson.SetBytes(body, "metadata.user_id", string(payload))
	if err != nil {
		return body
	}
	return out
}

// mergeAnthropicBeta 把入站声明的 anthropic-beta 与 OAuth 必需的 oauth-2025-04-20
// 合并去重,保证 OAuth 头始终在列。
func mergeAnthropicBeta(incoming http.Header) string {
	return mergeAnthropicBetaWithConfig(incoming, auth.DefaultClaudeSecurityConfig())
}

func mergeAnthropicBetaWithConfig(incoming http.Header, cfg auth.ClaudeSecurityConfig) string {
	return buildClaudeBetaHeader(incoming, cfg, nil)
}

// buildClaudeBetaHeader 生成贴近真实 Claude Code CLI 抓包顺序的 anthropic-beta:
//
//  1. 核心标记(恒定,不受白名单过滤):非 haiku 模型发 claude-code-20250219,
//     所有 OAuth 请求发 oauth-2025-04-20;
//  2. body 驱动(按 body 实际携带的功能补声明,保证字段与 beta 成对,
//     缺失会被上游 400 "required beta"):thinking→interleaved-thinking /
//     thinking-token-count / redact-thinking;context_management→
//     context-management;tools→advanced-tool-use;effort→effort;
//     1h 缓存→extended-cache-ttl;cache scope→prompt-caching-scope;
//  3. 下游透传:入站 anthropic-beta 按白名单过滤(白名单缺省时用真实 CLI
//     注册表 DefaultClaudeAllowedBetaHeaders,避免任意第三方 beta 混入)。
//
// 顺序对齐真实抓包:核心 → 功能声明 → 其余透传,降低与真实 CLI 的头部指纹差异。
func buildClaudeBetaHeader(incoming http.Header, cfg auth.ClaudeSecurityConfig, body []byte) string {
	cfg = auth.NormalizeClaudeSecurityConfig(cfg)
	allowed := make(map[string]struct{}, len(cfg.AllowedBetaHeaders)+len(auth.DefaultClaudeAllowedBetaHeaders)+2)
	for _, token := range auth.DefaultClaudeAllowedBetaHeaders {
		allowed[strings.ToLower(token)] = struct{}{}
	}
	for _, token := range cfg.AllowedBetaHeaders {
		allowed[strings.ToLower(strings.TrimSpace(token))] = struct{}{}
	}
	allowed[strings.ToLower(auth.ClaudeCodeBeta)] = struct{}{}
	allowed[strings.ToLower(auth.ClaudeOAuthBeta)] = struct{}{}

	seen := map[string]struct{}{}
	ordered := make([]string, 0, 16)
	add := func(v string, filter bool) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		key := strings.ToLower(v)
		if filter {
			if _, ok := allowed[key]; !ok {
				return
			}
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		ordered = append(ordered, v)
	}

	if body != nil && gjson.ValidBytes(body) {
		// 核心标记(haiku 不发 claude-code beta,对齐 CLI _re() 行为)。
		if !strings.Contains(strings.ToLower(gjson.GetBytes(body, "model").String()), "haiku") {
			add(auth.ClaudeCodeBeta, false)
		}
		add(auth.ClaudeOAuthBeta, false)

		// body 驱动的功能声明(恒放行,与字段成对出现)。
		if thinking := gjson.GetBytes(body, "thinking"); thinking.Exists() && thinking.Type != gjson.Null &&
			!strings.EqualFold(strings.TrimSpace(thinking.Get("type").String()), "disabled") {
			add("interleaved-thinking-2025-05-14", false)
			add("redact-thinking-2026-02-12", false)
			add("thinking-token-count-2026-05-13", false)
		}
		if gjson.GetBytes(body, "context_management").Exists() {
			add("context-management-2025-06-27", false)
		}
		if gjson.GetBytes(body, "tools").Exists() {
			add("advanced-tool-use-2025-11-20", false)
		}
		if gjson.GetBytes(body, "output_config.effort").Exists() || gjson.GetBytes(body, "reasoning_effort").Exists() || gjson.GetBytes(body, "effort").Exists() {
			add("effort-2025-11-24", false)
		}
		if claudeCacheControlHasExtendedTTL(body) {
			add("extended-cache-ttl-2025-04-11", false)
		}
		if claudeCacheControlHasScope(body) {
			add("prompt-caching-scope-2026-01-05", false)
		}
	} else {
		// 无 body 的兼容路径(如 mergeAnthropicBeta(nil)):仍带核心标记。
		add(auth.ClaudeCodeBeta, false)
		add(auth.ClaudeOAuthBeta, false)
	}

	// 下游透传:按白名单过滤,保持入站原序。
	if incoming != nil {
		for _, raw := range incoming.Values("anthropic-beta") {
			for _, part := range strings.Split(raw, ",") {
				add(part, true)
			}
		}
	}
	return strings.Join(ordered, ",")
}

// claudeCacheControlHasExtendedTTL 报告请求中是否存在 ttl=1h 的 cache_control 块
// (按 Anthropic 处理顺序 tools → system → messages 扫描)。
func claudeCacheControlHasExtendedTTL(body []byte) bool {
	found := false
	visit := func(items gjson.Result) {
		if found || !items.IsArray() {
			return
		}
		for _, item := range items.Array() {
			if cc := item.Get("cache_control"); cc.Exists() && cc.Get("ttl").String() == "1h" {
				found = true
				return
			}
		}
	}
	visit(gjson.GetBytes(body, "tools"))
	visit(gjson.GetBytes(body, "system"))
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		for _, msg := range messages.Array() {
			visit(msg.Get("content"))
			if found {
				return true
			}
		}
	}
	return found
}

// claudeCacheControlHasScope 报告请求中是否存在带 scope 的 cache_control 块
// (prompt-caching-scope beta 的触发条件)。
func claudeCacheControlHasScope(body []byte) bool {
	found := false
	visit := func(items gjson.Result) {
		if found || !items.IsArray() {
			return
		}
		for _, item := range items.Array() {
			if cc := item.Get("cache_control"); cc.Exists() && cc.Get("scope").Exists() {
				found = true
				return
			}
		}
	}
	visit(gjson.GetBytes(body, "tools"))
	visit(gjson.GetBytes(body, "system"))
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		for _, msg := range messages.Array() {
			visit(msg.Get("content"))
			if found {
				return true
			}
		}
	}
	return found
}

// claudeBillingHeaderPrefix 是真实 Claude Code 计费块文本的前缀标记。
const claudeBillingHeaderPrefix = "x-anthropic-billing-header:"

// claudeBillingBlockJSON 为缺少计费块的请求构造兼容块，沿用现有的版本派生后缀。
// 该后缀不是已验证的官方构建号/校验值；已有客户端计费块应保留，不能据此重造。
// 生成块不带 cache_control，不占用 4 块缓存配额。
func claudeBillingBlockJSON(version string) string {
	text := fmt.Sprintf("%s cc_version=%s.%s; cc_entrypoint=cli;", claudeBillingHeaderPrefix, version, claudeBillingBuildNumber(version))
	b, err := sjson.SetBytes([]byte(`{"type":"text"}`), "text", text)
	if err != nil {
		return `{"type":"text","text":"` + claudeBillingHeaderPrefix + ` cc_version=` + version + `; cc_entrypoint=cli;"}`
	}
	return string(b)
}

// claudeBillingBuildNumber 沿用旧函数名，为本地生成块派生稳定后缀(0-9999)。
// 它不实现 Claude Code 的请求内容校验算法。
func claudeBillingBuildNumber(version string) string {
	sum := sha1.Sum([]byte("claude-code-build:" + version))
	return strconv.FormatUint(uint64(binary.BigEndian.Uint32(sum[:4])%10000), 10)
}

var claudeBillingVersionPattern = regexp.MustCompile(`cc_version=([0-9]+\.[0-9]+\.[0-9]+)`)

// alignClaudeBillingBlock 把 system 计费块里的 cc_version 校正为最终出站
// User-Agent 中的 CLI 版本。计费块在 prepareClaudeRequestBody 阶段按
// EffectiveClaudeCLIVersion 构建,但 preserve 模式下最终 UA 可能是账号指纹
// 自带的版本(例如更新版本),两者不一致会暴露"计费块与 UA 不同源"的特征。
// 版本已一致或 UA 非 CLI 形态时原样返回,避免每次请求都做无谓改写。
func alignClaudeBillingBlock(body []byte, userAgent string) []byte {
	uaVersion, isCLI := auth.ParseClaudeClientVersion(userAgent)
	if !isCLI || uaVersion == "" {
		return body
	}
	text := gjson.GetBytes(body, "system.0.text").String()
	if !strings.HasPrefix(strings.TrimSpace(text), claudeBillingHeaderPrefix) {
		return body
	}
	match := claudeBillingVersionPattern.FindStringSubmatchIndex(text)
	if match == nil || text[match[2]:match[3]] == uaVersion {
		return body
	}
	// 只替换已识别的 CLI 版本字段，保留 entrypoint、后缀及原块的扩展/缓存属性。
	// 参考实现的计费后缀算法依赖原始请求，不能在这里假定它只是构建号并重造整个块。
	text = text[:match[2]] + uaVersion + text[match[3]:]
	aligned, err := sjson.SetBytes(body, "system.0.text", text)
	if err != nil {
		return body
	}
	return aligned
}

// injectClaudeCodeSystemPrompt 保证请求的 system 数组以 Claude Code 的
// [计费块, 声明块] 开头——与真实 CLI 抓包一致(计费块在前、CLI 声明块紧随其后)。
// 兼容三种入站形态:无 system / system 为字符串 / system 为块数组;已存在的块
// 不重复注入。
func injectClaudeCodeSystemPrompt(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	system := gjson.GetBytes(body, "system")
	var blocks []json.RawMessage
	switch {
	case !system.Exists() || system.Type == gjson.Null:
	case system.Type == gjson.String:
		block, err := sjson.SetBytes([]byte(`{"type":"text"}`), "text", system.String())
		if err != nil {
			return body
		}
		blocks = append(blocks, block)
	case system.IsArray():
		if json.Unmarshal([]byte(system.Raw), &blocks) != nil {
			return body
		}
	default:
		return body
	}
	var billing, preamble json.RawMessage
	others := make([]json.RawMessage, 0, len(blocks))
	for _, block := range blocks {
		text := strings.TrimSpace(gjson.GetBytes(block, "text").String())
		isText := gjson.GetBytes(block, "type").String() == "text"
		switch {
		case billing == nil && isText && strings.HasPrefix(text, claudeBillingHeaderPrefix) && claudeBillingVersionPattern.MatchString(text):
			billing = block
		case preamble == nil && isText && strings.HasPrefix(text, claudeCodeSystemPreamble):
			preamble = block
		default:
			// 仅提取已有前导块；不删除重复块或改写用户其余内容。
			others = append(others, block)
		}
	}
	if billing == nil {
		billing = json.RawMessage(claudeBillingBlockJSON(auth.EffectiveClaudeCLIVersion()))
	}
	if preamble == nil {
		preamble = json.RawMessage(claudeCodeSystemBlockFor(body))
	}
	ordered := append([]json.RawMessage{billing, preamble}, others...)
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return body
	}
	out, err := sjson.SetRawBytes(body, "system", encoded)
	if err != nil {
		return body
	}
	return out
}

// ── Claude 统一限流头 → 账号用量快照 ─────────────────────────────────────────
//
// Anthropic 对 Claude Code OAuth 账号的每个响应都带统一限流头(实测 2026-08):
//   anthropic-ratelimit-unified-5h-utilization: 0.01   ← 5h 滚动窗口利用率
//   anthropic-ratelimit-unified-5h-reset:       1787943000 (unix 秒)
//   anthropic-ratelimit-unified-7d-utilization: 0.0    ← 周窗口利用率
//   anthropic-ratelimit-unified-7d-reset:       1788253200
//   anthropic-ratelimit-unified-status:         allowed | rejected
// 该族头为 0-1 小数约定(同响应的 fallback-percentage: 0.5 即 50%)。

// claudeRatelimitHeaderPct 解析 utilization 头为百分数(0-100)。
// 保守起见 >1.5 的值视作上游已改用百分数,不再 ×100,避免进度条爆表。
func claudeRatelimitHeaderPct(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	if f <= 1.5 {
		f *= 100
	}
	if f > 100 {
		f = 100
	}
	return f, true
}

// claudeRatelimitHeaderTime 解析 unix 秒时间戳头(如 *-reset)。
func claudeRatelimitHeaderTime(v string) time.Time {
	v = strings.TrimSpace(v)
	sec, err := strconv.ParseInt(v, 10, 64)
	if err == nil && sec > 0 {
		// Some compatible gateways serialize epoch milliseconds instead of the
		// Anthropic epoch-seconds contract. Normalize that form and reject
		// implausible values so a malformed header cannot create a multi-century
		// account cooldown.
		if sec > 100_000_000_000 {
			sec /= 1000
		}
		if sec >= 946684800 && sec <= 4102444800 { // 2000-01-01 .. 2100-01-01
			return time.Unix(sec, 0)
		}
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		if parsed, parseErr := time.Parse(layout, v); parseErr == nil {
			return parsed
		}
	}
	return time.Time{}
}

// SyncClaudeUsageState 解析 Claude 响应的统一限流头,把 5h/7d 窗口利用率与重置
// 时刻写入与 Codex 同源的账号快照字段并持久化——管理页用量进度条/重置倒计时
// 直接生效。429 或 unified-status=rejected 时按上游给的重置时刻精确冷却。
// 持久化调用与 SyncCodexUsageState 同构:persist 在 ApplyUsageObservation 闭包内,
// MarkResponsesPremium5hRateLimited 自带观察序,必须留在闭包外(usageSyncMu 不可重入)。
func SyncClaudeUsageState(store *auth.Store, account *auth.Account, resp *http.Response) {
	if account == nil || resp == nil {
		return
	}
	h := resp.Header
	if h == nil {
		h = make(http.Header)
	}
	pct5h, ok5h := claudeRatelimitHeaderPct(h.Get("anthropic-ratelimit-unified-5h-utilization"))
	reset5h := claudeRatelimitHeaderTime(h.Get("anthropic-ratelimit-unified-5h-reset"))
	pct7d, ok7d := claudeRatelimitHeaderPct(h.Get("anthropic-ratelimit-unified-7d-utilization"))
	reset7d := claudeRatelimitHeaderTime(h.Get("anthropic-ratelimit-unified-7d-reset"))
	observedAt := time.Now()
	if !ok5h && !ok7d {
		// A valid native response without quota metadata is still evidence that
		// the token was observed. Record freshness without inventing a quota
		// percentage, otherwise the scheduler would repeat a paid probe forever.
		account.MarkClaudeUsageObservation(observedAt)
	}

	if ok5h || ok7d {
		account.ApplyUsageObservation(observedAt, func() {
			if ok5h {
				account.SetUsageSnapshot5hAt(pct5h, reset5h, observedAt)
			}
			if ok7d && !reset7d.IsZero() {
				account.SetReset7dAt(reset7d)
			}
			if store == nil {
				return
			}
			if ok7d {
				store.PersistUsageSnapshot(account, pct7d)
			} else if ok5h {
				store.PersistUsageSnapshot5hOnly(account)
			}
		})
		// A 7d-only unified response is still authoritative for the long
		// window, and therefore also authoritative evidence that a previously
		// cached 5h window is absent. Use the same observation timestamp so a
		// newer concurrent response wins and cannot be erased by this cleanup.
		if ok7d && !ok5h && store != nil {
			if _, hasStale5h := account.GetUsagePercent5h(); hasStale5h {
				store.ClearAbsentUsageSnapshot5hAt(account, observedAt)
			}
		}
	}

	// 上游拒绝(429 / unified-status=rejected)时,必须**按真实耗尽的窗口精确归因**,
	// 否则会把通用/边缘/周窗口的限流一律误标成「5h 窗口 100% 耗尽」并长时间冷却。
	// 注意不匹配 overage-status(那是溢出计费开关,200 响应上也会是 rejected)。
	rejected := resp.StatusCode == http.StatusTooManyRequests ||
		strings.EqualFold(strings.TrimSpace(h.Get("anthropic-ratelimit-unified-status")), "rejected")
	if rejected && store != nil {
		claim := strings.ToLower(strings.TrimSpace(h.Get("anthropic-ratelimit-unified-representative-claim")))
		fiveHourExhausted := (ok5h && pct5h >= 100) || claim == "five_hour" || claim == "five-hour" || claim == "5h"
		sevenDayExhausted := (ok7d && pct7d >= 100) || claim == "seven_day" || claim == "seven-day" || claim == "7d"
		switch {
		case sevenDayExhausted:
			// 周窗口耗尽:记到 7d 窗口(冷却到 7d 重置),不动 5h。上面已按 7d-utilization
			// 持久化;若上游只给了 representative-claim 而无 utilization,则补写 7d=100。
			if !(ok7d && pct7d >= 100) {
				r7 := reset7d
				if r7.IsZero() {
					r7 = claudeRatelimitHeaderTime(h.Get("anthropic-ratelimit-unified-reset"))
				}
				account.ApplyUsageObservation(time.Now(), func() {
					account.SetUsageSnapshot(100, time.Now())
					if !r7.IsZero() {
						account.SetReset7dAt(r7)
					}
					store.PersistUsageSnapshot(account, 100)
				})
			}
			store.MarkUsage7dRateLimited(account)
		case fiveHourExhausted:
			// 5h 窗口确实耗尽:标 5h 限流,冷却到 5h 重置。
			resetAt := reset5h
			if resetAt.IsZero() {
				resetAt = claudeRatelimitHeaderTime(h.Get("anthropic-ratelimit-unified-reset"))
			}
			store.MarkResponsesPremium5hRateLimited(account, resetAt)
		default:
			// 无任何窗口耗尽信号(通用/边缘/IP 限流,如 rate_limit_error,常无 unified 头)→
			// 只做短退避,绝不标 5h=100%。优先用 Retry-After,否则给保守默认。
			store.MarkCooldown(account, claudeGenericRateLimitBackoff(h), "rate_limited")
		}
	}
}

// claudeCreditsRequiredCooldown 是「模型需购买 usage credits」被拒时对该**模型**的冷却时长。
// credits_required 是模型级、需人工买 credits 才解除的计费门槛,不是账号级限流:
// 若按账号冷却会连累该号其它可用模型;用模型级冷却既避免反复打上游又不误伤别的模型。
const claudeCreditsRequiredCooldown = 30 * time.Minute

// HandleClaudeModelBillingRejection 处理 Claude 的**模型级计费拒绝**(429 credits_required):
// 只冷却被拒的那个模型(不动账号),已处理返回 true,调用方据此**跳过账号级用量/限流同步**。
// 非该类错误返回 false,调用方继续走正常的 SyncClaudeUsageState。
func HandleClaudeModelBillingRejection(store *auth.Store, account *auth.Account, model string, statusCode int, errBody []byte) bool {
	if store == nil || account == nil || statusCode != http.StatusTooManyRequests || len(errBody) == 0 {
		return false
	}
	code := strings.TrimSpace(gjson.GetBytes(errBody, "error.details.error_code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(errBody, "error.code").String())
	}
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(errBody, "details.error_code").String())
	}
	message := strings.ToLower(strings.Join([]string{
		gjson.GetBytes(errBody, "error.message").String(),
		gjson.GetBytes(errBody, "message").String(),
		string(errBody),
	}, " "))
	if !strings.EqualFold(code, "credits_required") &&
		!(strings.Contains(message, "usage credits") && strings.Contains(message, "required")) {
		return false
	}
	m := strings.TrimSpace(gjson.GetBytes(errBody, "error.details.model").String())
	if m == "" {
		m = strings.TrimSpace(gjson.GetBytes(errBody, "error.model").String())
	}
	if m == "" {
		m = strings.TrimSpace(model)
	}
	if m == "" {
		return false
	}
	// 模型级冷却,原因 credits_required;不做退避升级(固定窗口周期性复探,买 credits 后自然恢复)。
	store.MarkModelCooldownWithBackoff(account, m, claudeCreditsRequiredCooldown, "credits_required", false)
	// 套餐不含该模型时把它从账号白名单里移除,调度器此后不再把该模型派给这个账号。
	// 白名单为空(放行全部)时无法表达排除,只能靠上面的冷却。
	dropCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if removed, err := store.DropAccountModel(dropCtx, account, m); err != nil {
		log.Printf("[账号 %d] 移除不支持的模型 %s 失败: %v", account.ID(), m, err)
	} else if removed {
		log.Printf("[账号 %d] 上游 credits_required,已把模型 %s 从账号模型白名单移除", account.ID(), m)
	}
	return true
}

// claudeGenericRateLimitBackoff 返回通用限流(非窗口耗尽)的短冷却时长:
// 优先取 Retry-After(秒或 HTTP-date),否则默认 1 分钟;上限 15 分钟避免误封过久。
func claudeGenericRateLimitBackoff(h http.Header) time.Duration {
	const def = time.Minute
	const max = 15 * time.Minute
	ra := strings.TrimSpace(h.Get("Retry-After"))
	if ra == "" {
		return def
	}
	if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d > max {
			return max
		}
		return d
	}
	if t, err := http.ParseTime(ra); err == nil {
		if d := time.Until(t); d > 0 {
			if d > max {
				return max
			}
			return d
		}
	}
	return def
}
