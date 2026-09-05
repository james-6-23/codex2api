package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Grok CLI 请求头契约的默认值（与 Grok CLI 0.2.106 实抓流量对齐），可用环境变量覆盖，
// 上游升级 CLI 版本导致指纹校验失败时无需改代码。
// 0.2.106 契约：UA 为 "grok-pager/<v> grok-shell/<v> (<os>; <arch>)"，
// identifier=grok-pager、mode=interactive，不再携带 client-surface / client-name 头。
var (
	grokClientVersion    = grokEnv("GROK_CLIENT_VERSION", "0.2.106")
	grokClientIdentifier = grokEnv("GROK_CLIENT_IDENTIFIER", "grok-pager")
	grokClientMode       = grokEnv("GROK_CLIENT_MODE", "interactive")
	grokTokenAuth        = grokEnv("GROK_TOKEN_AUTH", "xai-grok-cli")
	// x-compaction-at：CLI 声明的客户端侧压缩阈值。默认值对应实抓的
	// context window 500k × 80%；环境变量非空时作为逃生阀直接覆盖推导结果。
	grokCompactionAtOverride = strings.TrimSpace(os.Getenv("GROK_COMPACTION_AT"))
	grokCompactionAtDefault  = "400000"
	// 官方 Grok CLI 1.0.4 实抓：doom-loop 窗口 1024、会话内剩余压缩次数 1。
	grokDoomLoopCheck        = grokEnv("GROK_DOOM_LOOP_CHECK", "1024")
	grokCompactionsRemaining = grokEnv("GROK_COMPACTIONS_REMAINING", "1")
)

func grokEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// grokCompactionThresholdPercent 是客户端压缩阈值占上下文窗口的比例。
// 与上游 /v1/models 返回的 auto_compact_threshold_percent 一致：实抓的
// context_window=500000、阈值 80%，官方 CLI 正是据此发出 x-compaction-at: 400000。
const grokCompactionThresholdPercent = 80

// grokCompactionAtForAccount 决定发给上游的 x-compaction-at。
// 优先级：环境变量显式覆盖 > 按账号观测到的上下文窗口推导 > 默认值。
// 观测值来自上游响应头，因此首个请求必然走默认值；观测到的窗口与默认假设一致时
// 推导结果也与默认值相同，即行为不变、仅在上游改窗口后自动跟上。
func grokCompactionAtForAccount(account *auth.Account) string {
	if grokCompactionAtOverride != "" {
		return grokCompactionAtOverride
	}
	if window := account.GetGrokContextWindow(); window > 0 {
		return strconv.FormatInt(window*grokCompactionThresholdPercent/100, 10)
	}
	return grokCompactionAtDefault
}

// grokUserAgentOS 返回 UA 里的平台名。官方 CLI 用 "macos" 而非 Go 的 "darwin"。
func grokUserAgentOS() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func grokUserAgent() string {
	if grokClientIdentifier == "grok-shell" {
		return fmt.Sprintf("grok-shell/%s (%s; %s)", grokClientVersion, grokUserAgentOS(), runtime.GOARCH)
	}
	return fmt.Sprintf("%s/%s grok-shell/%s (%s; %s)", grokClientIdentifier, grokClientVersion, grokClientVersion, grokUserAgentOS(), runtime.GOARCH)
}

// grokAgentID 为每个账号生成稳定的 agent 标识（32 位 hex，与 Grok CLI 的
// global agent id 形态一致）。
func grokAgentID(account *auth.Account) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "codex2api:grok-agent:%d", account.ID()))
	return hex.EncodeToString(sum[:16])
}

func grokRandomHexID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")
}

// resolveGrokConversationID 给官方 Grok CLI 的 session/conv 头一个跨轮稳定值。
// 只认会话级标识，不用 Idempotency-Key / x-client-request-id（那些每轮都变）。
// Claude Code 通常不带 Session_id，因此再用 system + 首条 user 做内容种子。
func resolveGrokConversationID(headers http.Header, body []byte) string {
	if headers != nil {
		for _, key := range []string{"Session-Id", "Session_id", "Conversation-Id", "Conversation_id"} {
			if value := strings.TrimSpace(headers.Get(key)); value != "" {
				return value
			}
		}
	}
	if value := strings.TrimSpace(gjson.GetBytes(body, "metadata.session_id").String()); value != "" {
		return value
	}
	if value := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); value != "" {
		return value
	}
	if seed := deriveGrokConversationSeed(body); seed != "" {
		return seed
	}
	if key := grokDownstreamAPIKey(headers); key != "" {
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:grok-conv:"+key)).String()
	}
	return uuid.New().String()
}

func grokDownstreamAPIKey(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if value := strings.TrimSpace(strings.TrimPrefix(headers.Get("Authorization"), "Bearer ")); value != "" {
		return value
	}
	for _, key := range []string{"X-Api-Key", "Anthropic-Auth-Token"} {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func deriveGrokConversationSeed(body []byte) string {
	seed := deriveContentSessionSeed(body)
	system := gjson.GetBytes(body, "system")
	if !system.Exists() || system.Raw == "" || system.Raw == "null" {
		return seed
	}
	stable := stableAnthropicSystemSeed([]byte(system.Raw))
	if stable == "" {
		return seed
	}
	sum := sha256.Sum256([]byte("codex2api:grok-conv:" + seed + "\x00" + stable))
	return "grokconv-" + hex.EncodeToString(sum[:16])
}

// applyGrokRequestHeaders 按 Grok CLI 的推理头契约装配上游请求头。
// API Key 凭据不发 x-xai-token-auth / x-authenticateresponse（与 Grok CLI 一致）。
// inboundBody 用于在下游没带会话头时派生稳定的 x-grok-session-id / x-grok-conv-id，
// 避免每轮随机 UUID 把 Grok prompt cache 打穿。探针/billing/媒体传 nil 即可。
func applyGrokRequestHeaders(req *http.Request, account *auth.Account, bearer string, downstreamHeaders http.Header, inboundBody []byte) {
	if req == nil {
		return
	}
	isAPIKey := account.GrokAuthKind() == auth.GrokAuthKindAPIKey

	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", grokUserAgent())
	req.Header.Set("x-grok-client-version", grokClientVersion)
	req.Header.Set("x-grok-client-identifier", grokClientIdentifier)
	req.Header.Set("x-grok-client-mode", grokClientMode)
	req.Header.Set("x-grok-doom-loop-check", grokDoomLoopCheck)
	req.Header.Set("x-compactions-remaining", grokCompactionsRemaining)
	if compactionAt := grokCompactionAtForAccount(account); compactionAt != "" {
		req.Header.Set("x-compaction-at", compactionAt)
	}
	if !isAPIKey {
		req.Header.Set("x-xai-token-auth", grokTokenAuth)
		req.Header.Set("x-authenticateresponse", "authenticate-response")
	}

	req.Header.Set("x-grok-agent-id", grokAgentID(account))
	sessionID := resolveGrokConversationID(downstreamHeaders, inboundBody)
	req.Header.Set("x-grok-session-id", sessionID)
	req.Header.Set("x-grok-conv-id", sessionID)
	req.Header.Set("x-grok-req-id", grokRandomHexID())

	if userID := account.GrokUserID(); userID != "" && !isAPIKey {
		req.Header.Set("x-userid", userID)
		req.Header.Set("x-grok-user-id", userID)
	}
	applyAccountCustomHeaders(req, account)
	RecordUpstreamUserAgent(req.Context(), req.Header.Get("User-Agent"))
}

// grokEndpointForBody 根据请求体推断上游 path（目前统一走 /responses，
// 下游三协议在进入执行器前都已被翻译成 Responses 体）。
func grokResponsesEndpoint(baseURL string) string {
	return auth.OpenAIResponsesEndpoint(baseURL, "/v1/responses")
}

// ExecuteGrokRequest 向 Grok 上游发送 Responses 请求。
// 复用 relay（openai_responses）的整条下游管道：进入这里的 requestBody 已是
// Responses 协议体，直接投递到 Grok chat-proxy / xAI API 的 /responses 端点。
func ExecuteGrokRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)
	resetWsAcquireAudit(ctx)

	baseURL, bearer := account.GrokCredentials()
	if baseURL == "" || bearer == "" {
		return nil, ErrNoAvailableAccount()
	}
	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}

	// 投递前一次性归一化：namespace 分组工具展平成子 function 并记录别名（响应流里再
	// 反解回 {name, namespace}）、web_search 降级为最小形态、历史项按 Grok 原生契约重建、
	// Codex 专属字段剥离、思考强度钳制，顺带算出轮次序号与模型名。
	conversationBody := requestBody
	preflight := prepareGrokUpstreamBody(requestBody)
	requestBody = preflight.Body
	nsAliases := preflight.Aliases
	logGrokPrefixFingerprint(requestBody, preflight.TurnIndex, preflight.Model)

	endpoint := grokResponsesEndpoint(baseURL)
	turnIdx := preflight.TurnIndex
	model := preflight.Model

	send := func(body []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, ErrInternalError("创建请求失败", err)
		}
		applyGrokRequestHeaders(req, account, bearer, headers, conversationBody)
		// 与官方 CLI 对齐的指纹头：会话内轮次序号 + 完整 Accept-Encoding。
		req.Header.Set("x-grok-turn-idx", strconv.Itoa(turnIdx))
		req.Header.Set("Accept-Encoding", "gzip, br, deflate")
		if model != "" {
			req.Header.Set("x-grok-model-override", model)
		}
		if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(body, "model").String()); err != nil {
			return nil, err
		}
		resp, err := getPooledClient(account, proxyURL).Do(req)
		if err != nil {
			if shouldRecyclePooledClient(err) {
				recyclePooledClient(account, proxyURL)
			}
			return nil, ErrUpstream(0, "请求 Grok 上游失败", err)
		}
		// 手动声明了 Accept-Encoding，需自行解压非流式的压缩响应。
		decodeGrokResponseEncoding(resp)
		return resp, nil
	}

	// 先带密文投递：同账号/同会话里 Grok 能解自己的 reasoning encrypted_content，保留完整推理上下文
	// （对齐官方 CLI include reasoning.encrypted_content 的往返）。仅当上游 400 且确为密文解码失败时，
	// 剥离外来密文重试一次（跨账号/外来 provider 的兜底降级）。
	resp, err := send(requestBody)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusBadRequest && grokBodyHasBlobs(requestBody) {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if grokIsBlobDecodeFailure(errBody) {
			resp, err = send(stripGrokUndecodableBlobs(requestBody))
			if err != nil {
				return nil, err
			}
		} else {
			// 不是密文问题：把读出的错误体放回去，交由上层按原状处理。
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
		}
	}
	recordGrokUpstreamObservations(account, resp.Header)
	// 请求侧展平过 namespace 工具时，把上游响应里的扁平函数名反解回 {name, namespace}。
	if len(nsAliases) > 0 && resp.Body != nil {
		streaming := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream")
		resp.Body = newGrokNamespaceReverser(resp.Body, streaming, nsAliases)
		if !streaming {
			resp.Header.Del("Content-Length")
			resp.ContentLength = -1
		}
	}
	return resp, nil
}

// recordGrokUpstreamObservations 采集上游逐请求返回的运行时观测：配额余量头、
// context window，以及 opaque 模型目录刷新提示。持久化 sink 是 generation
// fenced 且 DB-first；这里不等待后台刷新，也不会把 hint 当成 HTTP ETag。
func recordGrokUpstreamObservations(account *auth.Account, header http.Header) {
	recordGrokUpstreamObservationsAtOrigin(account, header, "")
}

func recordGrokUpstreamObservationsAtOrigin(account *auth.Account, header http.Header, origin string) {
	if account == nil || header == nil {
		return
	}
	recordGrokRateLimitHeaders(account, header)
	if window := parseGrokPositiveHeader(header, "x-grok-context-window"); window > 0 {
		account.SetGrokContextWindow(window)
	}
	if hint := strings.TrimSpace(header.Get("x-models-etag")); hint != "" {
		persistGrokRuntimeObservation(account, auth.GrokRuntimeFactObservation{
			ModelsETagHint: hint,
			Origin:         origin,
			ObservedAt:     time.Now(),
		})
	}
}

// parseGrokPositiveHeader 解析正整数响应头；缺失或非法返回 0。
func parseGrokPositiveHeader(header http.Header, key string) int64 {
	raw := strings.TrimSpace(header.Get(key))
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

// recordGrokRateLimitHeaders 采集上游逐请求返回的配额余量头（x-ratelimit-*），
// 写入账号运行时快照供账号列表展示。任一头缺失时不更新（避免半截观测覆盖完整值）。
func recordGrokRateLimitHeaders(account *auth.Account, header http.Header) {
	if account == nil || header == nil {
		return
	}
	parse := func(key string) (int64, bool) {
		raw := strings.TrimSpace(header.Get(key))
		if raw == "" {
			return 0, false
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			return 0, false
		}
		return v, true
	}
	limitTokens, ok1 := parse("x-ratelimit-limit-tokens")
	remainTokens, ok2 := parse("x-ratelimit-remaining-tokens")
	limitReqs, ok3 := parse("x-ratelimit-limit-requests")
	remainReqs, ok4 := parse("x-ratelimit-remaining-requests")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return
	}
	account.SetGrokRateLimitSnapshot(auth.GrokRateLimitSnapshot{
		LimitTokens:       limitTokens,
		RemainingTokens:   remainTokens,
		LimitRequests:     limitReqs,
		RemainingRequests: remainReqs,
		UpdatedAt:         time.Now(),
	})
}

// sanitizeGrokRequestBody 剥离 Codex 管道注入的、Grok 上游不接受的字段。
func sanitizeGrokRequestBody(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	for _, path := range []string{"client_metadata", "prompt_cache_key", "service_tier", "safety_identifier"} {
		if gjson.GetBytes(body, path).Exists() {
			if updated, err := sjson.DeleteBytes(body, path); err == nil {
				body = updated
			}
		}
	}
	body = clampGrokReasoningEffort(body)
	body = dropGrokToolChoiceWithoutTools(body)
	return body
}

// dropGrokToolChoiceWithoutTools 在没有工具声明时撤掉残留的 tool_choice。Grok 上游对
// "设了 tool_choice 但 tools 为空" 硬校验（400 invalid-argument "A tool_choice was set on
// the request but no tools were specified."），Codex/ChatGPT 上游则容忍，因此只在 Grok 侧归一。
// 两种来源都要兜住：客户端压缩轮自带 tools:[] + tool_choice:"auto"（issue #450），以及归一化
// 把工具全剥空后残留的选择（如 external_web_access:false 丢弃唯一的 web_search 工具）。
// 没有工具时 tool_choice 本就无意义，删除对语义无损。
func dropGrokToolChoiceWithoutTools(body []byte) []byte {
	if !gjson.GetBytes(body, "tool_choice").Exists() {
		return body
	}
	// 仅在 tools 缺失或为空数组时动手：非数组形态属于畸形请求，保持原样交给上游报错。
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && (!tools.IsArray() || len(tools.Array()) > 0) {
		return body
	}
	if updated, err := sjson.DeleteBytes(body, "tool_choice"); err == nil {
		return updated
	}
	return body
}

// mapGrokReasoningEffort 把思考强度映射到当前 Grok 模型支持的档位。
// grok-4.6 起（含 grok-4.20-multi-agent）支持 xhigh；更旧的 build 只有 low/medium/high。
// Codex 的 max 在支持 xhigh 的模型上落到 xhigh，否则落到 high；minimal 一律落到 low。
// 无模型上下文时按旧 build 处理，避免 grok-4.5 / grok-3 收到不认的档位。
func mapGrokReasoningEffort(effort, model string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "xhigh":
		if grokSupportsXHighReasoningEffort(model) {
			return effort, false
		}
		return "high", true
	case "max":
		if grokSupportsXHighReasoningEffort(model) {
			return "xhigh", true
		}
		return "high", true
	case "minimal":
		return "low", true
	default:
		return effort, false
	}
}

// grokSupportsXHighReasoningEffort 判断模型是否接受 reasoning.effort=xhigh。
// xAI 文档：grok-4.6 支持；grok-4.5 等不支持的模型会把 xhigh 当成 high。
// 版本线按 grok-4.6 起放行（grok-4.6-beta / grok-4.6-build / grok-4.20-multi-agent 同样识别）。
func grokSupportsXHighReasoningEffort(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if !strings.HasPrefix(model, "grok-") {
		return false
	}
	version := strings.TrimPrefix(model, "grok-")
	if dash := strings.IndexByte(version, '-'); dash >= 0 {
		version = version[:dash]
	}
	parts := strings.Split(version, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	if major > 4 {
		return true
	}
	if major != 4 || len(parts) < 2 {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return minor >= 6
}

// clampGrokReasoningEffort 规范化发给 Grok 上游的思考强度：同时覆盖 Responses
// （reasoning.effort）与 Chat（reasoning_effort）两种形态，避免旧 Grok 不认的档位报错。
func clampGrokReasoningEffort(body []byte) []byte {
	model := gjson.GetBytes(body, "model").String()
	for _, path := range []string{"reasoning.effort", "reasoning_effort"} {
		v := gjson.GetBytes(body, path)
		if !v.Exists() {
			continue
		}
		mapped, changed := mapGrokReasoningEffort(v.String(), model)
		if !changed {
			continue
		}
		if updated, err := sjson.SetBytes(body, path, mapped); err == nil {
			body = updated
		}
	}
	return body
}

// ExecuteRelayStyleRequest 按账号类型分派 relay 风格执行器：grok 账号走 Grok
// 上游，其余走 OpenAI Responses 中转。两者共享同一条下游 Responses 管道。
func ExecuteRelayStyleRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if account.IsGrokAPI() {
		return ExecuteGrokRequest(ctx, account, requestBody, proxyOverride, headers)
	}
	return ExecuteOpenAIResponsesRequest(ctx, account, requestBody, proxyOverride, headers)
}

// relayUpstreamEndpointForAccount 返回 relay 风格账号的上游 /responses 端点（用于日志/记账）。
func relayUpstreamEndpointForAccount(account *auth.Account) string {
	if account.IsGrokAPI() {
		baseURL, _ := account.GrokCredentials()
		return grokResponsesEndpoint(baseURL)
	}
	baseURL, _ := account.OpenAIResponsesCredentials()
	return auth.OpenAIResponsesEndpoint(baseURL, "/v1/responses")
}

// ==================== 模型目录 ====================

// FetchGrokModelIDs 用凭据探测 Grok 上游模型目录（GET /models），返回可用模型 ID
// 列表（过滤 hidden；API Key 凭据只保留 supported_in_api 的模型）。
// 条目兼容字符串与对象两种形态、data/models 两种容器（与 Grok CLI 目录响应一致）。
// proxyURL 由调用方解析(建议 store.ResolveProxyForAccount,与主请求路径同一套
// 三级回退),避免未绑定代理的账号在探测时直连同一出口 IP。
func FetchGrokModelIDs(ctx context.Context, account *auth.Account, proxyURL string) ([]string, error) {
	catalog, err := FetchGrokModelCatalog(ctx, account, proxyURL, "")
	if err != nil {
		return nil, err
	}
	models := VisibleGrokModelIDs(catalog.Models, account.GrokAuthKind())
	// A successful empty catalog is authoritative and must close routing rather
	// than being mistaken for "not yet synced" and reopening built-in defaults.
	// Temporary account probes are safe; persisted accounts will subsequently
	// publish the same fenced snapshot from the database.
	account.SetGrokRoutingState(auth.GrokRoutingState{
		CatalogKnown: true,
		Models:       grokCatalogToRoutingModels(catalog.Models),
		ObservedAt:   catalog.ObservedAt,
		ExpiresAt:    catalog.ObservedAt.Add(5 * time.Minute),
	})
	return models, nil
}

// ==================== 错误分类与冷却映射 ====================

// 默认模型集的权威定义在 auth 包(auth.GrokOAuthDefaultModelIDs /
// auth.GrokAPIKeyDefaultModelIDs),授权门与注册/调度面共用同一来源,
// 这里只做包内转发。详见 auth/grok_default_models.go 的注释。
func grokOAuthDefaultModelIDs() []string {
	return auth.GrokOAuthDefaultModelIDs()
}

func grokAPIKeyDefaultModelIDs() []string {
	return auth.GrokAPIKeyDefaultModelIDs()
}

// DefaultGrokModelIDsForAccount 按账号的凭据类型返回默认可用文本模型集。
func DefaultGrokModelIDsForAccount(account *auth.Account) []string {
	if account != nil && account.GrokAuthKind() == auth.GrokAuthKindAPIKey {
		return grokAPIKeyDefaultModelIDs()
	}
	return grokOAuthDefaultModelIDs()
}

// IsGrokFreeQuotaExhaustedError 识别免费额度耗尽（模型级，Grok 上游滚动 24h 窗口）。
func IsGrokFreeQuotaExhaustedError(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("used all the included free usage")) ||
		bytes.Contains(lower, []byte("subscription:free-usage-exhausted"))
}

// grokFreeQuotaUsagePattern 匹配免费额度耗尽错误体里的权威用量：
// "tokens (actual/limit): 1003617/1000000"。
var grokFreeQuotaUsagePattern = regexp.MustCompile(`(?i)tokens\s*\(actual/limit\)\s*:\s*([0-9]+)\s*/\s*([0-9]+)`)

// parseGrokFreeQuotaUsage 从免费额度耗尽错误体解析 used/limit token 数；解析不出返回 ok=false。
func parseGrokFreeQuotaUsage(body []byte) (used, limit int64, ok bool) {
	matches := grokFreeQuotaUsagePattern.FindSubmatch(body)
	if len(matches) != 3 {
		return 0, 0, false
	}
	used, usedErr := strconv.ParseInt(string(matches[1]), 10, 64)
	limit, limitErr := strconv.ParseInt(string(matches[2]), 10, 64)
	if usedErr != nil || limitErr != nil || limit <= 0 {
		return 0, 0, false
	}
	return used, limit, true
}

// parseGrokFreeQuotaModel 从错误体提取耗尽的模型名（"for model grok-4.5-build-free" 或 model 字段）。
func parseGrokFreeQuotaModel(body []byte) string {
	if m := strings.TrimSpace(gjson.GetBytes(body, "model").String()); m != "" {
		return m
	}
	matches := grokFreeQuotaModelPattern.FindSubmatch(body)
	if len(matches) == 2 {
		return string(matches[1])
	}
	return ""
}

var grokFreeQuotaModelPattern = regexp.MustCompile(`(?i)for\s+model\s+([a-z0-9._-]+)`)

// IsGrokSpendingLimitError 识别明确的账号级超支/余额耗尽。百分比本身不在
// 这里出现，也绝不能把单独的 100% 用量百分比升级为硬禁用。
func IsGrokSpendingLimitError(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("spending-limit")) ||
		bytes.Contains(lower, []byte("spending limit")) ||
		bytes.Contains(lower, []byte("usage balance exhausted")) ||
		bytes.Contains(lower, []byte("balance_exhausted")) ||
		bytes.Contains(lower, []byte("balance-exhausted")) ||
		bytes.Contains(lower, []byte("credit balance is too low")) ||
		bytes.Contains(lower, []byte("ran out of credits"))
}

// IsGrokPermanentDenialError 识别权限性永久拒绝（凭据对该端点无访问权）。
func IsGrokPermanentDenialError(body []byte) bool {
	return bytes.Contains(bytes.ToLower(body), []byte("access to the chat endpoint is denied"))
}

// applyGrokCooldownForModel 是 Grok 账号的上游错误 → 调度状态映射
// （对应 applyCooldownForModel 的 Codex 语义）：
//   - 免费额度耗尽 → free 账号整号冷却 24h（滚动窗口），付费账号模型级冷却 24h；
//     错误体里的 tokens (actual/limit) 作为权威用量快照落库供前端展示；
//   - 超支限制 → 账号冷却 24h；
//   - 429 → Retry-After 或 1 分钟，上限 15 分钟；
//   - 401 → 短隔离 + 异步强刷 AT（RT 失效时刷新路径会自行标 error）；
//   - 402 → 仅明确余额耗尽时硬隔离，否则保持 unknown；
//   - 403 → 已鉴权的权限/套餐拒绝，绝不触发 token refresh；
//   - 426 → version_required 短隔离，由调用方刷新 settings 后换号/重试；
//   - 429 → 独立 rate_limited 冷却。
func (h *Handler) applyGrokCooldownForModel(account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	if h == nil {
		return codex429Decision{}
	}
	return applyGrokCooldown(h.store, account, statusCode, body, resp, model)
}

// applyGrokCooldown 把 Grok 上游错误映射到调度状态，并在免费额度耗尽时落权威用量快照。
// 抽成包级函数供代理主路径与 Apply429Cooldown（批量测试/连通性测试）共用——两条路径都必须
// 走这里才能识别 free-usage-exhausted、保存 grok_free_quota 快照并标 usage_limited。
func applyGrokCooldown(store *auth.Store, account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	if store == nil || account == nil {
		return codex429Decision{}
	}
	if IsGrokFreeQuotaExhaustedError(body) {
		cooldownModel := model
		if cooldownModel == "" {
			cooldownModel = parseGrokFreeQuotaModel(body)
		}
		if used, limit, ok := parseGrokFreeQuotaUsage(body); ok {
			store.SaveGrokFreeQuotaSnapshot(account, auth.GrokFreeQuotaSnapshot{
				UsedTokens:  used,
				LimitTokens: limit,
				Model:       cooldownModel,
				ExhaustedAt: time.Now(),
			})
		}
		resetAt := time.Now().Add(24 * time.Hour)
		// free 账号只有免费额度这一种资源，模型级隔离没有意义且不影响账号状态展示，
		// 直接整号冷却让列表显示"限流"。付费账号保留模型级隔离（其它模型仍可用）。
		if strings.EqualFold(strings.TrimSpace(account.GetPlanType()), "free") || cooldownModel == "" {
			store.MarkCooldown(account, 24*time.Hour, "usage_limited")
			log.Printf("Grok 账号 %d 免费额度耗尽 (model=%s)，账号冷却 24h", account.ID(), cooldownModel)
			return codex429Decision{Reason: "usage_limited", ResetAt: resetAt, Cooldown: 24 * time.Hour}
		}
		cooldown := store.MarkModelCooldownUntil(account, cooldownModel, "usage_limited", resetAt)
		log.Printf("Grok 账号 %d 模型 %s 免费额度耗尽，冷却到 %s", account.ID(), cooldownModel, cooldown.ResetAt.Format(time.RFC3339))
		return codex429Decision{Scope: rateLimitScopeModel, Reason: "usage_limited", Model: cooldownModel, ResetAt: cooldown.ResetAt, Cooldown: time.Until(cooldown.ResetAt)}
	}
	if IsGrokSpendingLimitError(body) && (statusCode == http.StatusPaymentRequired || statusCode == http.StatusTooManyRequests) {
		observeGrokExplicitBillingExhaustion(account, statusCode, body)
		store.MarkCooldown(account, 24*time.Hour, "usage_limited")
		log.Printf("Grok 账号 %d 触发超支限制，账号冷却 24h", account.ID())
		return codex429Decision{Reason: "usage_limited", Cooldown: 24 * time.Hour}
	}

	switch statusCode {
	case http.StatusPaymentRequired:
		// A bare/unknown 402 is not sufficient evidence of an exhausted balance.
		// Keep it out of durable billing gates and let the request surface normally.
		return codex429Decision{Reason: "payment_required_unknown"}
	case http.StatusTooManyRequests:
		cooldown := time.Minute
		if resp != nil {
			if retryAfter := parseRetryAfterHeader(resp.Header.Get("Retry-After")); retryAfter > 0 {
				cooldown = retryAfter
			}
		}
		if cooldown > 15*time.Minute {
			cooldown = 15 * time.Minute
		}
		store.MarkCooldown(account, cooldown, "rate_limited")
		log.Printf("Grok 账号 %d 被限速，冷却 %s", account.ID(), cooldown)
		return codex429Decision{Reason: "rate_limited", Cooldown: cooldown}
	case http.StatusUnauthorized:
		// OAuth AT may be expired/revoked. A one-minute neutral cooldown prevents
		// a refresh stampede without marking the account's health tier banned.
		store.MarkCooldown(account, time.Minute, "credential_refresh")
		if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
			store.RefreshSingleAsync(account.ID())
		}
		return codex429Decision{Reason: "unauthorized", Cooldown: time.Minute}
	case http.StatusForbidden:
		if IsGrokPermanentDenialError(body) {
			log.Printf("Grok 账号 %d 权限被永久拒绝，标记为错误", account.ID())
			store.MarkError(account, upstreamAccountErrorMessage(statusCode, body))
			return codex429Decision{Reason: "forbidden"}
		}
		// 403 means the credential was accepted but lacks endpoint/model policy.
		// Isolate briefly so another account can be tried; never consume a RT.
		store.MarkCooldown(account, 5*time.Minute, "forbidden")
		return codex429Decision{Reason: "forbidden", Cooldown: 5 * time.Minute}
	case http.StatusUpgradeRequired:
		store.MarkCooldown(account, time.Minute, "version_required")
		return codex429Decision{Reason: "version_required", Cooldown: time.Minute}
	}
	return codex429Decision{}
}

// parseRetryAfterHeader 解析 Retry-After 头（秒数或 HTTP 日期）。
func parseRetryAfterHeader(value string) time.Duration {
	return parseRetryAfterHeaderAt(value, time.Now())
}

func parseRetryAfterHeaderAt(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 || seconds > int64((1<<63-1)/int64(time.Second)) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
