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
	// x-compaction-at：CLI 声明的客户端侧压缩阈值（上游 context window 500k，CLI 报 400k）。
	grokCompactionAt = grokEnv("GROK_COMPACTION_AT", "400000")
)

func grokEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
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

// applyGrokRequestHeaders 按 Grok CLI 的推理头契约装配上游请求头。
// API Key 凭据不发 x-xai-token-auth / x-authenticateresponse（与 Grok CLI 一致）。
func applyGrokRequestHeaders(req *http.Request, account *auth.Account, bearer string, downstreamHeaders http.Header) {
	if req == nil {
		return
	}
	isAPIKey := account.GrokAuthKind() == auth.GrokAuthKindAPIKey

	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", grokUserAgent())
	req.Header.Set("x-grok-client-version", grokClientVersion)
	req.Header.Set("x-grok-client-identifier", grokClientIdentifier)
	req.Header.Set("x-grok-client-mode", grokClientMode)
	if grokCompactionAt != "" {
		req.Header.Set("x-compaction-at", grokCompactionAt)
	}
	if !isAPIKey {
		req.Header.Set("x-xai-token-auth", grokTokenAuth)
		req.Header.Set("x-authenticateresponse", "authenticate-response")
	}

	req.Header.Set("x-grok-agent-id", grokAgentID(account))
	sessionID := ""
	if downstreamHeaders != nil {
		sessionID = strings.TrimSpace(downstreamHeaders.Get("Session_id"))
	}
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
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

	// Grok 上游不接受 Codex 的 namespace 分组工具与 web_search 控制字段，投递前归一化：
	// namespace 展平成子 function 并记录别名（响应流里再反解回 {name, namespace}），
	// web_search 降级为最小形态。
	requestBody, nsAliases := normalizeGrokUpstreamTools(requestBody)
	// Grok 上游不认识 Codex 专属字段，投递前剥离。
	requestBody = sanitizeGrokRequestBody(requestBody)

	endpoint := grokResponsesEndpoint(baseURL)
	turnIdx := grokTurnIndex(requestBody)
	model := gjson.GetBytes(requestBody, "model").String()

	send := func(body []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, ErrInternalError("创建请求失败", err)
		}
		applyGrokRequestHeaders(req, account, bearer, headers)
		// 与官方 CLI 对齐的指纹头：会话内轮次序号 + 完整 Accept-Encoding。
		req.Header.Set("x-grok-turn-idx", strconv.Itoa(turnIdx))
		req.Header.Set("Accept-Encoding", "gzip, br, deflate")
		if model != "" {
			req.Header.Set("x-grok-model-override", model)
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
	recordGrokRateLimitHeaders(account, resp.Header)
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
	if !ok1 && !ok2 && !ok3 && !ok4 {
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
	return body
}

// mapGrokReasoningEffort 把思考强度映射到 Grok build 支持的档位（只有 low/medium/high）：
// Codex 侧更高的 xhigh/max → high、更低的 minimal → low；low/medium/high 原样；其它未知不动。
func mapGrokReasoningEffort(effort string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "xhigh", "max":
		return "high", true
	case "minimal":
		return "low", true
	default:
		return effort, false
	}
}

// clampGrokReasoningEffort 规范化发给 Grok 上游的思考强度：同时覆盖 Responses
// （reasoning.effort）与 Chat（reasoning_effort）两种形态，避免 Grok 不认的档位报错。
func clampGrokReasoningEffort(body []byte) []byte {
	for _, path := range []string{"reasoning.effort", "reasoning_effort"} {
		v := gjson.GetBytes(body, path)
		if !v.Exists() {
			continue
		}
		mapped, changed := mapGrokReasoningEffort(v.String())
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
func FetchGrokModelIDs(ctx context.Context, account *auth.Account) ([]string, error) {
	baseURL, bearer := account.GrokCredentials()
	if baseURL == "" || bearer == "" {
		return nil, fmt.Errorf("grok 账号缺少可用凭据")
	}
	isAPIKey := account.GrokAuthKind() == auth.GrokAuthKindAPIKey

	endpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	applyGrokRequestHeaders(req, account, bearer, nil)
	req.Header.Set("Accept", "application/json")

	resp, err := getPooledClient(account, account.GetProxyURL()).Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Grok 模型列表失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
		if message == "" {
			message = truncateForLog(body, 200)
		}
		return nil, fmt.Errorf("Grok 模型列表返回 %d: %s", resp.StatusCode, message)
	}

	seen := make(map[string]struct{})
	var models []string
	collect := func(items gjson.Result) {
		if !items.IsArray() {
			return
		}
		items.ForEach(func(_, item gjson.Result) bool {
			id := ""
			if item.Type == gjson.String {
				id = strings.TrimSpace(item.String())
			} else if item.IsObject() {
				if item.Get("hidden").Bool() {
					return true
				}
				if isAPIKey {
					if supported := item.Get("supported_in_api"); supported.Exists() && !supported.Bool() {
						return true
					}
					if supported := item.Get("supportedInApi"); supported.Exists() && !supported.Bool() {
						return true
					}
				}
				id = strings.TrimSpace(item.Get("id").String())
				if id == "" {
					id = strings.TrimSpace(item.Get("model").String())
				}
			}
			if id != "" {
				if _, exists := seen[strings.ToLower(id)]; !exists {
					seen[strings.ToLower(id)] = struct{}{}
					models = append(models, id)
				}
			}
			return true
		})
	}
	parsed := gjson.ParseBytes(body)
	collect(parsed.Get("data"))
	collect(parsed.Get("models"))
	if len(models) == 0 {
		return nil, fmt.Errorf("Grok 上游返回了空模型目录")
	}
	return auth.NormalizeAccountModels(models), nil
}

// ==================== 错误分类与冷却映射 ====================

// DefaultGrokModelIDs 是 Grok 账号未声明 models 白名单时的默认可用文本模型集，
// 用于把 grok-4.5 等注册进 /v1/models（与 Grok CLI 常见目录对齐）。账号显式声明
// models 后以其白名单为准，不再补默认集。
func DefaultGrokModelIDs() []string {
	return []string{"grok-4.5", "grok-4", "grok-3-fast", "grok-3", "grok-2"}
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

// IsGrokSpendingLimitError 识别账号级超支限制。
func IsGrokSpendingLimitError(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("spending-limit")) || bytes.Contains(lower, []byte("spending limit"))
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
//   - 401 → 短冷却 + 异步强刷 AT（RT 失效时刷新路径会自行标 error）；
//   - 403 权限拒绝 → 账号标错误。
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
	if IsGrokSpendingLimitError(body) {
		store.MarkCooldown(account, 24*time.Hour, "usage_limited")
		log.Printf("Grok 账号 %d 触发超支限制，账号冷却 24h", account.ID())
		return codex429Decision{Reason: "usage_limited", Cooldown: 24 * time.Hour}
	}

	switch statusCode {
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
	case http.StatusUnauthorized, http.StatusForbidden:
		if IsGrokPermanentDenialError(body) {
			log.Printf("Grok 账号 %d 权限被永久拒绝，标记为错误", account.ID())
			store.MarkError(account, upstreamAccountErrorMessage(statusCode, body))
			return codex429Decision{}
		}
		// OAuth AT 可能过期/被吊销：短冷却挡住并发，异步强刷；RT 失效由刷新路径转 error。
		store.MarkCooldown(account, time.Minute, "unauthorized")
		if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
			go func(dbID int64) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := store.RefreshSingle(ctx, dbID); err != nil {
					log.Printf("Grok 账号 %d 401 后强刷失败: %v", dbID, err)
				}
			}(account.ID())
		}
		return codex429Decision{Reason: "unauthorized", Cooldown: time.Minute}
	}
	return codex429Decision{}
}

// parseRetryAfterHeader 解析 Retry-After 头（秒数或 HTTP 日期）。
func parseRetryAfterHeader(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds > 0 {
		return seconds
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}
