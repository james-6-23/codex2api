package proxy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const consoleUpstreamErrorLogMaxBytes = 4 * 1024

func upstreamErrorConsoleBody(body []byte) string {
	if len(bytes.TrimSpace(body)) > 0 && !gjson.ValidBytes(body) {
		return fmt.Sprintf("[non-JSON upstream body omitted; %d bytes]", len(body))
	}
	truncated := false
	if len(body) > consoleUpstreamErrorLogMaxBytes {
		body = body[:consoleUpstreamErrorLogMaxBytes]
		truncated = true
	}
	bodyStr := security.SafeTruncate(security.SanitizeLog(string(body)), consoleUpstreamErrorLogMaxBytes)
	if truncated {
		bodyStr += " ... [truncated]"
	}
	return bodyStr
}

// Handler API 路由处理器
type Handler struct {
	store        *auth.Store
	configKeys   map[string]bool // 配置文件中的静态 key
	db           *database.DB
	cfg          *config.Config       // 全局配置
	deviceCfg    *DeviceProfileConfig // 设备指纹配置
	cache        cache.TokenCache     // Redis/Memory 运行态缓存
	apiKeyGateMu sync.Mutex
	promptRiskMu sync.Mutex
	apiKeyGate   *apiKeyConcurrencyLimiter
	scopeUsageMu sync.Mutex
	scopeUsage   *apiKeyScopeUsageTracker
	liveStore    *liveCallStore
}

const (
	apiKeyCacheNamespace      = "api-key"
	apiKeyCountCacheNamespace = "api-key-count"
	apiKeyCacheTTL            = 5 * time.Minute
	apiKeyCountCacheTTL       = 30 * time.Second
)

type apiKeyRuntimeRecord struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type apiKeyCountRuntimeRecord struct {
	Count int `json:"count"`
}

func (h *Handler) nextAccountForSession(sessionID string, apiKeyID int64, exclude map[int64]bool) (*auth.Account, string) {
	return h.nextAccountForSessionWithFilter(sessionID, apiKeyID, exclude, nil)
}

func (h *Handler) nextAccountForSessionWithFilter(sessionID string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter) (*auth.Account, string) {
	return h.nextAccountForSessionWithDispatch(sessionID, apiKeyID, exclude, filter, auth.DispatchPolicyStandard)
}

func (h *Handler) nextAccountForSessionWithDispatch(sessionID string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, policy auth.DispatchPolicy) (*auth.Account, string) {
	if h == nil || h.store == nil {
		return nil, ""
	}
	return h.store.NextForSessionWithDispatch(sessionID, apiKeyID, exclude, filter, policy)
}

func dispatchPolicyForModel(model string) auth.DispatchPolicy {
	if isProOnlyModel(model) {
		return auth.DispatchPolicySpark
	}
	return auth.DispatchPolicyStandard
}

func (h *Handler) withModelCooldownFilter(model string, filter auth.AccountFilter) auth.AccountFilter {
	if h == nil || h.store == nil {
		return filter
	}
	return h.store.WithModelCooldownFilter(model, filter)
}

func (h *Handler) shouldUseWebsocketForHTTP() bool {
	if h == nil {
		return false
	}
	// 运行时 DB 级开关 codex_force_websocket 优先：开启则强制走 WS
	// （与 ExecuteRequest 的 wantWebsocket 判定保持一致，也用于 usage 日志的 WS 标记）。
	if CurrentRuntimeSettings().CodexForceWebsocket {
		return true
	}
	// 管理后台的热更新值同样作为强制开关来源，避免运行时配置尚未同步时
	// UI 显示已开启但请求热路径仍按静态 env=http 判定。
	if h.store != nil && h.store.CodexForceWebsocket() {
		return true
	}
	if h.cfg == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(h.cfg.CodexUpstreamTransport)) {
	case "ws":
		return true
	case "http", "auto":
		return false
	default:
		return h.cfg.UseWebsocket
	}
}

func (h *Handler) resolveProxyForAttempt(account *auth.Account, stickyProxyURL string) string {
	if h != nil && h.store != nil {
		if proxyURL := strings.TrimSpace(stickyProxyURL); proxyURL != "" && !h.store.ManagedProxyUnavailable(proxyURL) {
			return proxyURL
		}
		return h.store.ResolveProxyForAccount(account)
	}
	return strings.TrimSpace(stickyProxyURL)
}

type usageLimitDetails struct {
	message         string
	planType        string
	resetsAt        int64
	resetsInSeconds int64
}

type CodexUsageSyncResult struct {
	UsagePct7d               float64
	HasUsage7d               bool
	Usage7dRateLimited       bool
	UsagePct5h               float64
	Reset5hAt                time.Time
	HasUsage5h               bool
	Used5hHeaders            bool
	Persisted5hOnly          bool
	Premium5hRateLimited     bool
	UsageWindowLimitsIgnored bool
	// Cleared5h 表示本次同步因上游未返回 5h 窗口而清除了本地陈旧 5h 快照（issue #382）。
	Cleared5h bool
}

type codexRateLimitWindow string

const (
	codexRateLimitWindowUnknown codexRateLimitWindow = ""
	codexRateLimitWindowShort   codexRateLimitWindow = "short"
	codexRateLimitWindow5h      codexRateLimitWindow = "5h"
	codexRateLimitWindow7d      codexRateLimitWindow = "7d"
)

type codex429Decision struct {
	Scope    string
	Reason   string
	Model    string
	ResetAt  time.Time
	Cooldown time.Duration
}

const (
	rateLimitScopeAccount = "account"
	rateLimitScopeModel   = "model"
)

const (
	contextAPIKeyID     = "apiKeyID"
	contextAPIKeyName   = "apiKeyName"
	contextAPIKeyMasked = "apiKeyMasked"
	contextAPIKeyRow    = "apiKeyRow"
	// contextScopeBudgetGate 存放本次请求的 scope 预算闸门（issue #439），
	// 由 enforceAPIKeyLimits 计算一次，供账号过滤链与「无可用账号」分支复用。
	contextScopeBudgetGate = "apiKeyScopeBudgetGate"
)

func requestAPIKeyID(c *gin.Context) int64 {
	if c == nil {
		return 0
	}
	if value, exists := c.Get(contextAPIKeyID); exists && value != nil {
		switch typed := value.(type) {
		case int64:
			return typed
		case int:
			return int64(typed)
		}
	}
	return 0
}

func sessionAffinityKey(sessionID string, apiKeyID int64) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || apiKeyID <= 0 {
		return sessionID
	}
	return fmt.Sprintf("%s::api-key:%d", sessionID, apiKeyID)
}

const codexTurnStateHeader = "X-Codex-Turn-State"

// codexTurnContinuationToken follows the official Codex per-turn contract:
// HTTP sends the token as a header, while Responses WebSocket v2 sends it in
// response.create client_metadata after the first request in that turn.
func codexTurnContinuationToken(headers http.Header, body []byte) string {
	if headers != nil {
		if token := strings.TrimSpace(headers.Get(codexTurnStateHeader)); token != "" {
			return token
		}
	}
	return strings.TrimSpace(gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String())
}

// codexWSTurnContinuationToken reads per-frame metadata only. Upgrade headers
// are connection-scoped and cannot prove that every response.create belongs to
// the same active turn.
func codexWSTurnContinuationToken(body []byte) string {
	return codexTurnContinuationToken(nil, body)
}

// applyAffinityGroupRouting keeps fingerprinted requests on the API key's original groups
// and routes requests without either a Codex engine fingerprint or the dedicated local
// affinity header to the configured split groups.
//
// 当 Key 没配「允许账号分组」（= 不限分组）时，带指纹的请求改为「除分流组以外的全部账号」：
// 否则分流组既服务无指纹请求、又照常接真 Codex 流量，隔离等于没做——而不限分组恰恰是
// 绝大多数 Key 的默认配置。
func applyAffinityGroupRouting(c *gin.Context, identity requestSessionIdentity, filter auth.AccountFilter) auth.AccountFilter {
	row := apiKeyRowFromContext(c)
	if row == nil || len(row.Limits.NoAffinityGroupIDs) == 0 {
		return filter
	}

	splitGroups := int64GroupSet(row.Limits.NoAffinityGroupIDs)
	if len(splitGroups) == 0 {
		return filter
	}

	if !identity.hasRequestFingerprint {
		return groupMembershipFilter(splitGroups, true, filter)
	}

	allowedGroups := int64GroupSet(row.AllowedGroupIDs)
	if len(allowedGroups) == 0 {
		// 不限分组：把分流组排除掉，其余（含未分组账号）照常可用。
		return groupMembershipFilter(splitGroups, false, filter)
	}
	return groupMembershipFilter(allowedGroups, true, filter)
}

func int64GroupSet(ids []int64) map[int64]struct{} {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id > 0 {
			set[id] = struct{}{}
		}
	}
	return set
}

// groupMembershipFilter 在 filter 之上叠加分组门：want=true 要求账号命中 groups，
// want=false 要求账号不在 groups 里。
func groupMembershipFilter(groups map[int64]struct{}, want bool, filter auth.AccountFilter) auth.AccountFilter {
	return func(account *auth.Account) bool {
		if account == nil || account.InAnyGroup(groups) != want {
			return false
		}
		return filter == nil || filter(account)
	}
}

const proOnlySparkModel = "gpt-5.3-codex-spark"

func isProOnlyModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), proOnlySparkModel)
}

func isSparkPlanCandidate(planType string) bool {
	switch auth.NormalizePlanType(planType) {
	case "free", "api":
		return false
	default:
		return true
	}
}

func accountFilterForModel(model string) auth.AccountFilter {
	model = strings.TrimSpace(model)
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		if account.IsRelayStyle() {
			return false
		}
		if model != "" && account.IsModelRateLimited(model) {
			return false
		}
		if !account.SupportsCodexModel(model) {
			return false
		}
		if isProOnlyModel(model) {
			return isSparkPlanCandidate(account.GetPlanType())
		}
		return true
	}
}

// requestUpstreamChannel 返回当前请求下游 Key 的上游渠道限定（空=不限）。
func requestUpstreamChannel(c *gin.Context) string {
	row := apiKeyRowFromContext(c)
	if row == nil {
		return ""
	}
	return row.Limits.ResolveUpstreamChannel()
}

// applyUpstreamChannelFilter 按下游 Key 的上游渠道限定改写账号过滤器。
// grok 渠道换成 Grok 专属过滤（账号未声明模型时按可见目录或保守默认集准入）；
// codex 渠道在原过滤器上排除 Grok 账号；未限定则原样返回。
func (h *Handler) applyUpstreamChannelFilter(c *gin.Context, effectiveModel string, filter auth.AccountFilter) auth.AccountFilter {
	switch requestUpstreamChannel(c) {
	case database.UpstreamChannelGrok:
		return grokChannelAccountFilter(effectiveModel)
	case database.UpstreamChannelCodex:
		return func(account *auth.Account) bool {
			if account == nil || account.IsGrokAPI() {
				return false
			}
			return filter(account)
		}
	}
	return filter
}

// grokChannelAccountFilter 是 grok 渠道 Key 的账号过滤器：仅 Grok 账号；
// mapping 先行，再按账号可见目录准入；显式 Models 白名单只会进一步收窄。
func grokChannelAccountFilter(model string) auth.AccountFilter {
	model = strings.TrimSpace(model)
	return func(account *auth.Account) bool {
		if account == nil || !account.IsGrokAPI() {
			return false
		}
		routedModel := model
		if mappedModel, ok := resolveAccountModelMapping(account, model); ok && mappedModel != "" {
			routedModel = mappedModel
		}
		if routedModel != "" && account.IsModelRateLimited(routedModel) {
			return false
		}
		return grokAccountSupportsVisibleModel(account, routedModel)
	}
}

func accountFilterForResponsesModel(model string, allowCodexAccounts bool) auth.AccountFilter {
	return accountFilterForResponsesModelWithOriginal(model, model, allowCodexAccounts)
}

func accountFilterForResponsesModelWithOriginal(originalModel string, effectiveModel string, allowCodexAccounts bool) auth.AccountFilter {
	return accountFilterForResponsesModelCandidates([]string{originalModel, effectiveModel}, effectiveModel, allowCodexAccounts)
}

func accountFilterForCompactResponsesModelWithOriginal(originalModel string, effectiveModel string, allowCodexAccounts bool) auth.AccountFilter {
	candidates := compactMappingCandidates(originalModel, effectiveModel)
	inner := accountFilterForResponsesModelResolver(effectiveModel, allowCodexAccounts, func(account *auth.Account) (string, bool) {
		return resolveAccountCompactModelMappingForCandidates(account, candidates)
	})
	return func(account *auth.Account) bool {
		// Grok 上游没有 /responses/compact 端点，compact 请求不路由到 Grok 账号
		if account.IsGrokAPI() {
			return false
		}
		return inner(account)
	}
}

func accountFilterForResponsesModelCandidates(modelCandidates []string, effectiveModel string, allowCodexAccounts bool) auth.AccountFilter {
	return accountFilterForResponsesModelResolver(effectiveModel, allowCodexAccounts, func(account *auth.Account) (string, bool) {
		return resolveAccountModelMappingForCandidates(account, modelCandidates...)
	})
}

func accountFilterForResponsesModelResolver(effectiveModel string, allowCodexAccounts bool, resolveMapping func(*auth.Account) (string, bool)) auth.AccountFilter {
	effectiveModel = strings.TrimSpace(effectiveModel)
	codexFilter := accountFilterForModel(effectiveModel)
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		if account.IsRelayStyle() {
			routedModel := effectiveModel
			if mappedModel, ok := resolveMapping(account); ok && mappedModel != "" {
				routedModel = mappedModel
			}
			return relayAccountSupportsModel(account, routedModel) && (routedModel == "" || !account.IsModelRateLimited(routedModel))
		}
		if !allowCodexAccounts {
			return false
		}
		return codexFilter(account)
	}
}

// relayAccountSupportsModel 判断 relay 风格账号能否服务指定模型。
// 普通 relay 中转必须显式声明 models 白名单；Grok 账号未声明白名单时按默认
// Grok 模型集放行——与 /v1/models 的默认集注册（supportedModelIDs）保持一致，
// 否则通用 Key 在模型列表里看得到 grok-4.5 却永远调度不到（恒 503）。
// 声明了白名单的 Grok 账号仍以白名单为准。
func relayAccountSupportsModel(account *auth.Account, model string) bool {
	if account == nil {
		return false
	}
	if account.IsGrokAPI() {
		return grokAccountSupportsVisibleModel(account, model)
	}
	if account.SupportsOpenAIResponsesModel(model) {
		return true
	}
	return false
}

// grokAccountSupportsVisibleModel keeps request admission aligned with the
// account-scoped catalog exposed by /v1/models. An explicit Models setting is
// a narrowing whitelist, never authority to invent or unhide a model absent
// from the account catalog (or conservative no-catalog defaults).
func grokAccountSupportsVisibleModel(account *auth.Account, model string) bool {
	if account == nil || !account.IsGrokAPI() || !account.GrokChannelSupportsModel(model) {
		return false
	}
	if !account.GrokDispatchHardAllowed(time.Now()) {
		return false
	}
	if !modelIDInList(model, GrokVisibleModelIDsForAccount(account)) {
		return false
	}
	return GrokModelRoutable(account, model, GrokProtocolResponses, time.Now())
}

func modelIDInList(model string, models []string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, candidate := range models {
		if strings.EqualFold(strings.TrimSpace(candidate), model) {
			return true
		}
	}
	return false
}

func (h *Handler) modelSupportedByAccountMapping(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" || h == nil || h.store == nil {
		return false
	}
	for _, account := range h.store.Accounts() {
		if account == nil || !account.IsRelayStyle() {
			continue
		}
		mappedModel, ok := resolveAccountModelMapping(account, model)
		if ok && mappedModel != "" && relayAccountSupportsModel(account, mappedModel) {
			return true
		}
	}
	return false
}

func (h *Handler) modelValidator(supportedModels []string) api.ValidationRule {
	validModels := make(map[string]bool, len(supportedModels))
	for _, model := range supportedModels {
		validModels[model] = true
	}
	return func(value gjson.Result, path string) *api.ValidationError {
		if !value.Exists() || value.Type != gjson.String {
			return nil
		}
		model := value.String()
		if validModels[model] || h.modelSupportedByAccountMapping(model) {
			return nil
		}
		return &api.ValidationError{
			Field:   path,
			Message: fmt.Sprintf("Model '%s' is not supported", model),
			Code:    "unsupported_model",
		}
	}
}

func effectiveRequestModel(body []byte, fallback string) string {
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model != "" {
		return model
	}
	return strings.TrimSpace(fallback)
}

func noAvailableAccountMessage(model string) string {
	if isProOnlyModel(model) {
		return "无可用付费或未知套餐账号，gpt-5.3-codex-spark 已排除明确 free/api 账号"
	}
	return "无可用账号，请稍后重试"
}

func noAvailableAccountError(model string) gin.H {
	return gin.H{
		"error": gin.H{
			"message": noAvailableAccountMessage(model),
			"type":    ErrorTypeServerError,
			"code":    ErrorCodeNoAvailableAccount,
		},
	}
}

func usageLogErrorMessage(statusCode int, body []byte) string {
	return usageLogErrorMessageImpl(statusCode, body, false)
}

// usageLogFailureMessage 记录网关自产的失败诊断（传输错误、断流原因、重试上下文等）。
// 与上游任意响应体不同，这些文本由网关代码拼装，脱敏截断后保留原文；否则断流类
// 错误在用量页只剩裸状态码，根因全靠翻容器日志（issue #524）。仍先尝试 JSON 提取，
// 因为部分诊断原样包含上游错误帧。
func usageLogFailureMessage(statusCode int, message string) string {
	return usageLogErrorMessageImpl(statusCode, []byte(message), true)
}

func usageLogErrorMessageImpl(statusCode int, body []byte, trustedText bool) string {
	if statusCode < 400 {
		return ""
	}

	candidates := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "response.error.message").String(),
		gjson.GetBytes(body, "response.status_details.error.message").String(),
		gjson.GetBytes(body, "message").String(),
	}
	message := ""
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			message = candidate
			break
		}
	}
	if message == "" {
		// Grok/xAI 风格错误体把说明放在顶层字符串 error 字段:
		// {"code":"invalid-argument","error":"..."}。仅在 error 是字符串时采用,
		// 避免把对象形态的整段 JSON 打进 message。
		if errField := gjson.GetBytes(body, "error"); errField.Type == gjson.String {
			message = strings.TrimSpace(errField.String())
		}
	}

	codeCandidates := []string{
		gjson.GetBytes(body, "error.code").String(),
		gjson.GetBytes(body, "response.error.code").String(),
		gjson.GetBytes(body, "response.status_details.error.code").String(),
		gjson.GetBytes(body, "detail.code").String(),
		gjson.GetBytes(body, "code").String(),
	}
	code := ""
	for _, candidate := range codeCandidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			code = candidate
			break
		}
	}

	typeCandidates := []string{
		gjson.GetBytes(body, "error.type").String(),
		gjson.GetBytes(body, "response.error.type").String(),
		gjson.GetBytes(body, "response.status_details.error.type").String(),
		gjson.GetBytes(body, "type").String(),
	}
	errType := ""
	for _, candidate := range typeCandidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" && candidate != "error" {
			errType = candidate
			break
		}
	}

	if message == "" {
		// HTML and plain-text provider pages routinely contain request IDs,
		// internal routing details or echoed credentials. They are not an API
		// contract, so persist only the transport status instead of the body —
		// unless the caller marked the text as gateway-generated (trustedText).
		if !trustedText {
			return fmt.Sprintf("HTTP %d", statusCode)
		}
		raw := strings.TrimSpace(string(body))
		if raw == "" {
			return fmt.Sprintf("HTTP %d", statusCode)
		}
		message = raw
	}

	parts := make([]string, 0, 3)
	if code != "" {
		parts = append(parts, code)
	}
	if errType != "" && errType != code {
		parts = append(parts, errType)
	}
	parts = append(parts, message)
	return security.SafeTruncate(security.SanitizeLog(strings.Join(parts, " · ")), 600)
}

var grokDownstreamResponseHeaders = map[string]struct{}{
	"cache-control": {}, "content-language": {}, "content-type": {},
	"retry-after": {}, "x-ratelimit-limit-requests": {},
	"x-ratelimit-limit-tokens": {}, "x-ratelimit-remaining-requests": {},
	"x-ratelimit-remaining-tokens": {}, "x-ratelimit-reset-requests": {},
	"x-ratelimit-reset-tokens": {}, "x-request-id": {},
}

// copyGrokNativeResponseHeaders copies only end-to-end, non-sensitive provider
// metadata. Authorization, Set-Cookie, model extra headers and process-local
// route markers can therefore never leak to downstream clients.
func copyGrokNativeResponseHeaders(c *gin.Context, header http.Header) {
	if c == nil {
		return
	}
	for name, values := range header {
		if _, ok := grokDownstreamResponseHeaders[strings.ToLower(strings.TrimSpace(name))]; !ok {
			continue
		}
		for _, value := range values {
			if !strings.ContainsAny(value, "\r\n") {
				c.Writer.Header().Add(name, value)
			}
		}
	}
}

func grokNativeUsage(protocol GrokProtocol, payload []byte) *UsageInfo {
	root := gjson.ParseBytes(payload)
	if !root.Exists() {
		return nil
	}
	switch auth.NormalizeGrokProtocol(string(protocol)) {
	case GrokProtocolChatCompletions:
		input := int(root.Get("usage.prompt_tokens").Int())
		output := int(root.Get("usage.completion_tokens").Int())
		reasoning := int(root.Get("usage.completion_tokens_details.reasoning_tokens").Int())
		cached := int(root.Get("usage.prompt_tokens_details.cached_tokens").Int())
		if !root.Get("usage").Exists() {
			return nil
		}
		return newUsageInfo(input, output, reasoning, cached)
	case GrokProtocolMessages:
		// 非流式 body 与 message_delta 事件的 usage 在顶层；流式 message_start
		// 事件的 input_tokens 在 message.usage 下。两处都认，交给 max 合并。
		usage := root.Get("usage")
		if !usage.Exists() {
			usage = root.Get("message.usage")
		}
		if !usage.Exists() {
			return nil
		}
		input := int(usage.Get("input_tokens").Int())
		output := int(usage.Get("output_tokens").Int())
		cached := int(usage.Get("cache_read_input_tokens").Int())
		return newUsageInfo(input, output, 0, cached)
	default:
		// Responses 协议：非流式 body 的 usage 在顶层；流式 response.completed /
		// response.incomplete 事件的 usage 在 response.usage 下。
		usage := root.Get("usage")
		if !usage.Exists() {
			usage = root.Get("response.usage")
		}
		return extractUsageFromResult(usage)
	}
}

func grokNativeTerminalEvent(protocol GrokProtocol, payload []byte) (terminal bool, failed bool) {
	root := gjson.ParseBytes(payload)
	switch auth.NormalizeGrokProtocol(string(protocol)) {
	case GrokProtocolChatCompletions:
		if root.Get("error").Exists() || root.Get("type").String() == "error" {
			return true, true
		}
		// A finish_reason chunk is not the wire terminal: include_usage streams
		// commonly send a separate usage-only chunk before the [DONE] sentinel.
		// [DONE] is handled by the raw SSE frame reader.
		return false, false
	case GrokProtocolMessages:
		switch root.Get("type").String() {
		case "error":
			return true, true
		case "message_stop":
			return true, false
		}
		return false, false
	default:
		switch eventType := root.Get("type").String(); {
		case isResponsesSuccessTerminalEvent(eventType):
			return true, false
		case eventType == "response.failed", eventType == "error":
			return true, true
		}
		return false, false
	}
}

func grokNativeStreamFailure(protocol GrokProtocol, payload []byte) streamOutcome {
	outcome := classifyResponseFailedOutcome(payload)
	if outcome.failureMessage == "" || outcome.failureMessage == fmt.Sprintf("HTTP %d", outcome.logStatusCode) {
		outcome.failureMessage = "Grok upstream stream failed"
	}
	return outcome
}

func writeGrokNativeStreamBreak(c *gin.Context, protocol GrokProtocol) error {
	switch auth.NormalizeGrokProtocol(string(protocol)) {
	case GrokProtocolChatCompletions:
		payload := []byte(`{"error":{"message":"` + upstreamStreamBreakMessage +
			`","type":"` + ErrorTypeUpstreamError + `","code":"` + ErrorCodeUpstreamStreamBreak + `"}}`)
		_, err := fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
		return err
	case GrokProtocolMessages:
		payload := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"` +
			upstreamStreamBreakMessage + ` (upstream_stream_break)"}}`)
		_, err := fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
		return err
	default:
		payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"` +
			ErrorCodeUpstreamStreamBreak + `","message":"` + upstreamStreamBreakMessage + `"}}}`)
		_, err := fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
		return err
	}
}

func safeGrokNativeHTTPStatus(status int) int {
	if status < 400 || status > 599 || status == logStatusUpstreamStreamBreak || status == logStatusClientClosed {
		return http.StatusBadGateway
	}
	return status
}

func (h *Handler) sendGrokNativeHTTPError(c *gin.Context, protocol GrokProtocol, outcome streamOutcome) {
	if c == nil {
		return
	}
	status := safeGrokNativeHTTPStatus(outcome.logStatusCode)
	message := strings.TrimSpace(outcome.failureMessage)
	if message == "" {
		message = fmt.Sprintf("Upstream returned status %d", status)
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	if auth.NormalizeGrokProtocol(string(protocol)) == GrokProtocolMessages {
		sendAnthropicError(c, status, mapHTTPStatusToAnthropicError(status), message)
		return
	}
	code := fmt.Sprintf("upstream_%d", status)
	if outcome.logStatusCode == logStatusUpstreamStreamBreak {
		code = ErrorCodeUpstreamStreamBreak
	}
	c.JSON(status, gin.H{"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": code}})
}

func mergeGrokNativeUsage(current, next *UsageInfo) *UsageInfo {
	if next == nil {
		return current
	}
	if current == nil {
		copy := *next
		return &copy
	}
	current.PromptTokens = max(current.PromptTokens, next.PromptTokens)
	current.CompletionTokens = max(current.CompletionTokens, next.CompletionTokens)
	current.InputTokens = max(current.InputTokens, next.InputTokens)
	current.OutputTokens = max(current.OutputTokens, next.OutputTokens)
	current.ReasoningTokens = max(current.ReasoningTokens, next.ReasoningTokens)
	current.CachedTokens = max(current.CachedTokens, next.CachedTokens)
	current.TotalTokens = max(current.TotalTokens, next.TotalTokens)
	current.TotalTokens = max(current.TotalTokens, current.InputTokens+current.OutputTokens)
	if current.CachedTokens > 0 {
		details := &TokenDetails{CachedTokens: current.CachedTokens}
		current.PromptTokensDetails = details
		current.InputTokensDetails = details
	}
	return current
}

func grokNativeNonStreamFailure(protocol GrokProtocol, payload []byte) (streamOutcome, bool) {
	if len(bytes.TrimSpace(payload)) == 0 || !gjson.ValidBytes(payload) {
		return streamOutcome{
			logStatusCode: logStatusUpstreamStreamBreak, failureKind: "transport",
			failureMessage: "Grok upstream returned an invalid JSON response", penalize: true,
		}, true
	}
	root := gjson.ParseBytes(payload)
	failed := false
	switch auth.NormalizeGrokProtocol(string(protocol)) {
	case GrokProtocolChatCompletions, GrokProtocolMessages:
		failed = root.Get("type").String() == "error" || (root.Get("error").Exists() && root.Get("error").Raw != "null")
	default:
		failed = root.Get("type").String() == "response.failed" || root.Get("status").String() == "failed" ||
			(root.Get("error").Exists() && root.Get("error").Raw != "null")
	}
	if !failed {
		return streamOutcome{}, false
	}
	return grokNativeStreamFailure(protocol, payload), true
}

// forwardGrokNativeResponse preserves a proven same-protocol upstream wire
// response. Native SSE frames are forwarded byte-for-byte (including event,
// id, retry, comments, multiline data, line endings, and [DONE]); their data
// projection is inspected only for terminal state, usage, and the pre-output
// retry boundary. firstTokenMs is measured from startedAt.
func forwardGrokNativeResponse(c *gin.Context, resp *http.Response, protocol GrokProtocol, streaming bool, startedAt time.Time, firstVisible func()) (*UsageInfo, streamOutcome, bool, int) {
	copyGrokNativeResponseHeaders(c, resp.Header)
	resp.Header.Del(grokNativeRouteHeader)
	if !streaming {
		body, err := readAllLimited(resp.Body, grokMaxDecodedBody)
		if err != nil {
			return nil, classifyStreamOutcome(nil, err, nil, false), false, 0
		}
		if failure, failed := grokNativeNonStreamFailure(protocol, body); failed {
			return grokNativeUsage(protocol, body), failure, false, 0
		}
		usage := grokNativeUsage(protocol, body)
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/json"
		}
		c.Data(resp.StatusCode, contentType, body)
		return usage, streamOutcome{logStatusCode: http.StatusOK}, len(body) > 0, 0
	}

	if c.Writer.Header().Get("Content-Type") == "" {
		c.Header("Content-Type", "text/event-stream")
	}
	if c.Writer.Header().Get("Cache-Control") == "" {
		c.Header("Cache-Control", "no-cache")
	}
	c.Header("X-Accel-Buffering", "no")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, streamOutcome{logStatusCode: http.StatusInternalServerError, failureKind: "server", failureMessage: "streaming not supported"}, false, 0
	}
	var usage *UsageInfo
	var terminal, failed, wrote, visible bool
	firstTokenMs := 0
	var failure streamOutcome
	var pending bytes.Buffer
	writeErr := error(nil)
	frameErr := error(nil)
	readErr := readRawGrokSSEFrames(resp.Body, func(frame rawGrokSSEFrame) bool {
		if frame.HasData && !frame.Done {
			usage = mergeGrokNativeUsage(usage, grokNativeUsage(protocol, frame.Data))
		}
		isTerminal, isFailed := false, false
		if frame.Done && auth.NormalizeGrokProtocol(string(protocol)) == GrokProtocolChatCompletions {
			isTerminal = true
		} else if frame.HasData && !frame.Done {
			isTerminal, isFailed = grokNativeTerminalEvent(protocol, frame.Data)
		}
		if isTerminal {
			terminal, failed = true, isFailed
			if isFailed {
				failure = grokNativeStreamFailure(protocol, frame.Data)
			}
		}
		isVisible := frame.HasData && !frame.Done && grokNativeVisibleEvent(protocol, frame.Data)
		// Hold only the frames that must stay invisible for a silent retry.
		// Responses used to buffer every non-text frame until the first
		// output_text.delta (issue #207's anti-pattern); reasoning models then
		// looked synchronous because thinking/structure never reached the client
		// (issue #521). Chat/Messages still hold role-only / start frames.
		if holdGrokNativePreOutput(protocol, frame, visible, isTerminal, isVisible) {
			if pending.Len()+len(frame.Raw) > grokMaxNativeSSEPendingBytes {
				frameErr = fmt.Errorf("Grok pre-output SSE exceeds %d bytes", grokMaxNativeSSEPendingBytes)
				return false
			}
			pending.Write(frame.Raw)
			return true
		}
		if isVisible && !visible {
			visible = true
			if firstVisible != nil {
				firstVisible()
			}
			if !startedAt.IsZero() {
				firstTokenMs = max(int(time.Since(startedAt).Milliseconds()), 1)
			}
		}
		if isFailed && !visible && !wrote {
			pending.Reset()
			return false
		}
		if pending.Len() > 0 {
			if _, err := c.Writer.Write(pending.Bytes()); err != nil {
				writeErr = err
				return false
			}
			wrote = true
			pending.Reset()
		}
		if len(frame.Raw) > 0 {
			if _, err := c.Writer.Write(frame.Raw); err != nil {
				writeErr = err
				return false
			}
			wrote = true
		}
		flusher.Flush()
		return !isTerminal
	})
	if frameErr != nil {
		readErr = frameErr
	}
	if failed {
		return usage, failure, wrote, firstTokenMs
	}
	outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, terminal)
	if !terminal && wrote && c.Request.Context().Err() == nil && writeErr == nil {
		_ = writeGrokNativeStreamBreak(c, protocol)
		flusher.Flush()
	}
	return usage, outcome, wrote, firstTokenMs
}

func holdGrokNativePreOutput(protocol GrokProtocol, frame rawGrokSSEFrame, alreadyVisible, isTerminal, isVisible bool) bool {
	if alreadyVisible || isTerminal || isVisible {
		return false
	}
	if auth.NormalizeGrokProtocol(string(protocol)) == GrokProtocolResponses {
		if !frame.HasData || frame.Done {
			return false
		}
		return isPreContentLifecycleEvent(gjson.GetBytes(frame.Data, "type").String())
	}
	return true
}

// GrokStreamEventIsVisible reports whether a native Grok SSE payload carries
// model output the client can see. Capability probes reuse this so usage_logs
// first_token_ms is not left at 0 for otherwise successful streams.
func GrokStreamEventIsVisible(protocol GrokProtocol, payload []byte) bool {
	return grokNativeVisibleEvent(protocol, payload)
}

func grokNativeVisibleEvent(protocol GrokProtocol, payload []byte) bool {
	root := gjson.ParseBytes(payload)
	switch auth.NormalizeGrokProtocol(string(protocol)) {
	case GrokProtocolChatCompletions:
		return root.Get("choices.0.delta.content").String() != "" ||
			root.Get("choices.0.delta.reasoning_content").String() != "" ||
			root.Get("choices.0.delta.tool_calls").IsArray()
	case GrokProtocolMessages:
		switch root.Get("type").String() {
		case "content_block_delta":
			return root.Get("delta.text").String() != "" || root.Get("delta.thinking").String() != "" ||
				root.Get("delta.partial_json").String() != "" || root.Get("delta.signature").String() != ""
		case "content_block_start":
			return root.Get("content_block.type").String() == "tool_use"
		}
		return false
	default:
		return isFirstTokenResult(root)
	}
}

func noAvailableAnthropicAccountMessage(model string) string {
	if isProOnlyModel(model) {
		return "No available paid or unknown-plan account for gpt-5.3-codex-spark"
	}
	return "No available accounts, please retry later"
}

// NewHandler 创建处理器
func NewHandler(store *auth.Store, db *database.DB, cfg *config.Config, deviceCfg *DeviceProfileConfig) *Handler {
	handler := &Handler{
		store:      store,
		configKeys: make(map[string]bool), // 不再使用硬编码，但保留结构以向后兼容逻辑
		db:         db,
		cfg:        cfg,
		deviceCfg:  deviceCfg,
		apiKeyGate: newAPIKeyConcurrencyLimiter(),
	}
	handler.liveStore = newLiveCallStore(handler)
	return handler
}

// SetRuntimeCache wires Redis/Memory runtime cache for hot auth metadata.
func (h *Handler) SetRuntimeCache(tc cache.TokenCache) {
	if h == nil {
		return
	}
	h.cache = tc
}

// NewHandlerWithDeviceProfile 创建处理器（带设备指纹配置）
func NewHandlerWithDeviceProfile(store *auth.Store, db *database.DB, deviceCfg *DeviceProfileConfig) *Handler {
	return NewHandler(store, db, nil, deviceCfg)
}

// resolveAPIKey 解析下游 API Key。返回值区分三种结果：
//   - (row, true, nil)：命中有效 key
//   - (nil, false, nil)：确认查无此 key（应答 401）
//   - (nil, false, err)：DB/基础设施暂时性故障（应答 503，而非误报 key 无效）
//
// 关键：绝不能把"数据库连接耗尽/超时"这类暂时性故障当成"客户端 key 无效"
// 返回 401，否则压测或 DB 抖动时客户端会误以为自己的凭证失效（issue #323）。
func (h *Handler) resolveAPIKey(key string) (*database.APIKeyRow, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false, nil
	}
	if h.configKeys[key] {
		return &database.APIKeyRow{
			ID:   0,
			Name: "config",
			Key:  key,
		}, true, nil
	}
	if row, ok := h.resolveAPIKeyFromRuntimeCache(key); ok {
		h.syncAPIKeyAllowedGroups(row)
		return row, true, nil
	}
	if h.db == nil {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	row, err := h.db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		// DB 故障（连接耗尽/超时/网络）：上报错误让调用方回 503，不当成 key 无效。
		log.Printf("查询 API Key 失败: %v", err)
		return nil, false, err
	}
	h.setAPIKeyRuntimeCache(row)
	h.syncAPIKeyAllowedGroups(row)
	return row, true, nil
}

func (h *Handler) resolveAPIKeyFromRuntimeCache(key string) (*database.APIKeyRow, bool) {
	if h == nil || h.cache == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	raw, ok, err := h.cache.GetRuntime(ctx, apiKeyCacheNamespace, key)
	if err != nil {
		log.Printf("读取 API Key Redis 缓存失败: %v", err)
		return nil, false
	}
	if !ok || len(raw) == 0 {
		return nil, false
	}
	var record apiKeyRuntimeRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		log.Printf("解析 API Key Redis 缓存失败: %v", err)
		return nil, false
	}
	if record.ID <= 0 {
		return nil, false
	}
	return &database.APIKeyRow{
		ID:        record.ID,
		Name:      record.Name,
		Key:       key,
		CreatedAt: record.CreatedAt,
	}, true
}

func (h *Handler) setAPIKeyRuntimeCache(row *database.APIKeyRow) {
	if h == nil || h.cache == nil || row == nil || strings.TrimSpace(row.Key) == "" || row.ID <= 0 {
		return
	}
	if row.HasAccessConstraints() {
		return
	}
	record := apiKeyRuntimeRecord{
		ID:        row.ID,
		Name:      row.Name,
		CreatedAt: row.CreatedAt,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := h.cache.SetRuntime(ctx, apiKeyCacheNamespace, row.Key, payload, apiKeyCacheTTL); err != nil {
		log.Printf("写入 API Key Redis 缓存失败: id=%d err=%v", row.ID, err)
	}
}

func (h *Handler) syncAPIKeyAllowedGroups(row *database.APIKeyRow) {
	if h == nil || h.store == nil || row == nil || row.ID <= 0 {
		return
	}
	h.store.SetAPIKeyAllowedGroups(row.ID, row.AllowedGroupIDs)
	h.store.SetAPIKeyNoAffinityGroups(row.ID, row.Limits.NoAffinityGroupIDs)
	h.store.SetAPIKeyAllowedPlans(row.ID, row.Limits.PlanAllow)
	h.store.SetAPIKeyUpstreamChannel(row.ID, row.Limits.ResolveUpstreamChannel())
}

// isValidKey 检查 key 是否有效（配置文件 + DB）。DB 故障时保守返回 false。
func (h *Handler) isValidKey(key string) bool {
	_, ok, _ := h.resolveAPIKey(key)
	return ok
}

// hasAnyKeys 检查是否配置了任何密钥
func (h *Handler) hasAnyKeys() bool {
	if len(h.configKeys) > 0 {
		return true
	}
	if h.cache != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		raw, ok, err := h.cache.GetRuntime(ctx, apiKeyCountCacheNamespace, "all")
		cancel()
		if err != nil {
			log.Printf("读取 API Key 数量缓存失败: %v", err)
		} else if ok {
			var record apiKeyCountRuntimeRecord
			if err := json.Unmarshal(raw, &record); err == nil {
				return record.Count > 0
			}
		}
	}
	if h.db == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	count, err := h.db.CountAPIKeys(ctx)
	if err != nil {
		log.Printf("统计 API Key 数量失败: %v", err)
		return false
	}
	if h.cache != nil {
		payload, _ := json.Marshal(apiKeyCountRuntimeRecord{Count: count})
		cacheCtx, cacheCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		if err := h.cache.SetRuntime(cacheCtx, apiKeyCountCacheNamespace, "all", payload, apiKeyCountCacheTTL); err != nil {
			log.Printf("写入 API Key 数量缓存失败: %v", err)
		}
		cacheCancel()
	}
	return count > 0
}

// logUsage 记录请求日志（非阻塞，写入内存缓冲由后台批量 flush）
func (h *Handler) logUsage(input *database.UsageLogInput) {
	if h.db == nil || input == nil {
		return
	}
	// Grok usage belongs to the credential generation that actually dispatched
	// this attempt. Resolve it from AccountID at the common sink so success,
	// failure and transport-retry paths cannot accidentally omit it. A retry
	// that switches accounts naturally resolves the replacement account here.
	// Non-Grok and unresolved accounts deliberately remain legacy/unscoped (0).
	h.populateUsageCredentialGeneration(input)
	// scope 维度预算（issue #439）在日志落库前先吃到这笔消耗，抵掉窗口聚合缓存的滞后。
	h.recordAPIKeyScopeUsage(input)
	// 渠道在写入时按调度账号固化（内存索引查询），供仪表盘分渠道聚合；
	// 账号已不在池中（如刚被删除）时按 codex 兜底。
	if input.Channel == "" && h.store != nil {
		input.Channel = database.UpstreamChannelCodex
		if input.AccountID > 0 {
			if acc := h.store.FindByID(input.AccountID); acc != nil && acc.IsGrokAPI() {
				input.Channel = database.UpstreamChannelGrok
			}
		}
	}
	// 过载熔断统计（仅 Codex 渠道，需在渠道固化之后）。
	h.noteOverloadOutcome(input)
	_ = h.db.InsertUsageLog(context.Background(), input)
}

func (h *Handler) populateUsageCredentialGeneration(input *database.UsageLogInput) {
	if input == nil || input.CredentialGeneration != 0 {
		return
	}
	if h == nil || h.store == nil || input.AccountID <= 0 {
		return
	}
	account := h.store.FindByID(input.AccountID)
	if account == nil || !account.IsGrokAPI() {
		return
	}
	input.CredentialGeneration = account.GetCredentialGeneration()
}

func populateAPIKeyMetaFromContext(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil {
		return
	}
	if v, exists := c.Get(contextAPIKeyID); exists && v != nil {
		switch typed := v.(type) {
		case int64:
			input.APIKeyID = typed
		case int:
			input.APIKeyID = int64(typed)
		}
	}
	if v, exists := c.Get(contextAPIKeyName); exists && v != nil {
		if name, ok := v.(string); ok {
			input.APIKeyName = name
		}
	}
	if v, exists := c.Get(contextAPIKeyMasked); exists && v != nil {
		if masked, ok := v.(string); ok {
			input.APIKeyMasked = masked
		}
	}
}

func populateInternalUsageMetaFromContext(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil {
		return
	}
	if value, exists := c.Get(contextInternalReason); exists {
		input.InternalReason, _ = value.(string)
	}
	if value, exists := c.Get(contextParentRequestID); exists {
		input.ParentRequestID, _ = value.(string)
	}
}

func (h *Handler) logUsageForRequest(c *gin.Context, input *database.UsageLogInput) {
	populateAPIKeyMetaFromContext(c, input)
	populateInternalUsageMetaFromContext(c, input)
	populateClientIPFromRequest(c, input)
	populateUserAgentMetaFromRequest(c, input)
	populateWsAcquireFromRequest(c, input)
	populateCompactUsageMetaFromRequest(c, input)
	markCyberPolicyUsageKind(input)
	h.logUsage(input)
}

// logContinueThinkingRounds 为思考截断续想中「被折叠隐藏」的上游轮次补记真实用量。
// 每一轮续想都是一次独立的上游请求，各自产生真实 token 消耗；对客户端折叠成单响应
// 后，最终成功轮的用量由本 attempt 收尾统一记账，这里补记除最终成功轮外的其余各轮
// （res.Rounds 除最后一条）以及失败的续想开轮（res.FailedContinuation），
// 使账面消耗与实际上游请求数一致，且不与收尾记账重复计费。
func (h *Handler) logContinueThinkingRounds(c *gin.Context, res continueFoldResult, account *auth.Account, logModel, logEffectiveModel, reasoningEffort string, useWebsocket bool, requestedServiceTier string) {
	logRound := func(round continueRoundStat) {
		usageTiers := resolveUsageServiceTiers("", requestedServiceTier)
		statusCode := round.StatusCode
		if statusCode == 0 {
			statusCode = http.StatusOK
		}
		logInput := &database.UsageLogInput{
			AccountID:            account.ID(),
			Endpoint:             "/v1/responses",
			Model:                logModel,
			EffectiveModel:       logEffectiveModel,
			StatusCode:           statusCode,
			DurationMs:           round.DurationMs,
			ReasoningEffort:      reasoningEffort,
			InboundEndpoint:      "/v1/responses",
			UpstreamEndpoint:     "/v1/responses",
			Stream:               true,
			ViaWebsocket:         useWebsocket,
			ServiceTier:          usageTiers.ServiceTier,
			RequestedServiceTier: usageTiers.RequestedServiceTier,
			ActualServiceTier:    usageTiers.ActualServiceTier,
			BillingServiceTier:   usageTiers.BillingServiceTier,
			// 隐藏的续想轮不是「重试」：不置 IsRetryAttempt/AttemptIndex，
			// 否则会污染重试统计并与外层 attempt 编号混淆。
		}
		if round.ErrMessage != "" {
			logInput.ErrorMessage = usageLogFailureMessage(statusCode, round.ErrMessage)
			logInput.UpstreamErrorKind = "continue_thinking_error"
		}
		if round.Usage != nil {
			logInput.PromptTokens = round.Usage.PromptTokens
			logInput.CompletionTokens = round.Usage.CompletionTokens
			logInput.TotalTokens = round.Usage.TotalTokens
			logInput.InputTokens = round.Usage.InputTokens
			logInput.OutputTokens = round.Usage.OutputTokens
			logInput.ReasoningTokens = round.Usage.ReasoningTokens
			logInput.CachedTokens = round.Usage.CachedTokens
		}
		h.logUsageForRequest(c, logInput)
	}

	// res.Rounds 的最后一条是最终成功轮，其用量由本 attempt 收尾统一记账，此处排除。
	for i := 0; i+1 < len(res.Rounds); i++ {
		logRound(res.Rounds[i])
	}
	if res.FailedContinuation != nil {
		logRound(*res.FailedContinuation)
	}
}

// markCyberPolicyUsageKind 在使用日志里把 cyber_policy 报错单独标记成 cyber_policy
// 类型，便于「使用统计」页识别并点击查看触发详情。仅改写日志展示字段，不参与
// 账号调度 / 冷却评分（那条路径用的是另外的 failureKind）。
func markCyberPolicyUsageKind(input *database.UsageLogInput) {
	if input == nil || input.UpstreamErrorKind == "cyber_policy" {
		return
	}
	msg := strings.ToLower(input.ErrorMessage)
	if strings.Contains(msg, "cyber_policy") || strings.Contains(msg, "cyber security risk") {
		input.UpstreamErrorKind = "cyber_policy"
	}
}

func populateClientIPFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil || strings.TrimSpace(input.ClientIP) != "" {
		return
	}
	clientIP := strings.TrimSpace(c.ClientIP())
	if clientIP == "" && c.Request != nil {
		clientIP = strings.TrimSpace(c.Request.RemoteAddr)
		if host, _, err := net.SplitHostPort(clientIP); err == nil {
			clientIP = host
		}
	}
	if len(clientIP) > 64 {
		clientIP = clientIP[:64]
	}
	input.ClientIP = clientIP
}

func populateCompactUsageMetaFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if input == nil {
		return
	}

	meta, ok := cachedRequestCompactionMeta(c)
	if !ok {
		if body, bodyOK := rawRequestBodyFromContext(c); bodyOK {
			meta = requestBodyCompactionMeta(body)
		}
		// HTTP requests may carry the same per-turn metadata as a request header.
		// Client WebSocket turns always cache frame-local metadata before logging,
		// so the Upgrade request header cannot leak into individual frames here.
		if c != nil && c.Request != nil && turnMetadataIndicatesCompaction(c.GetHeader("X-Codex-Turn-Metadata")) {
			meta.UsageTriggered = true
		}
	}

	// Only the original inbound path can mark an explicit Compact request.
	// UpstreamEndpoint may be rewritten internally and must not affect this signal.
	if isExplicitCompactUsageRequest(c, input) {
		meta.UsageTriggered = true
	}

	if meta.UsageTriggered {
		input.Compact = true
	}
	if meta.HasHistory {
		input.HasCompactionHistory = true
	}
}

func isExplicitCompactUsageRequest(c *gin.Context, input *database.UsageLogInput) bool {
	if c != nil && c.Request != nil && c.Request.URL != nil {
		return isCompactUsageEndpoint(c.Request.URL.Path)
	}
	if input == nil {
		return false
	}
	return isCompactUsageEndpoint(input.InboundEndpoint) || isCompactUsageEndpoint(input.Endpoint)
}

func isCompactUsageEndpoint(endpoint string) bool {
	endpoint = strings.TrimSpace(endpoint)
	if cut := strings.IndexAny(endpoint, "?#"); cut >= 0 {
		endpoint = endpoint[:cut]
	}
	endpoint = strings.TrimRight(endpoint, "/")
	return endpoint == "/v1/responses/compact"
}

func rawRequestBodyFromContext(c *gin.Context) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	v, exists := c.Get("raw_body")
	if !exists || v == nil {
		return nil, false
	}
	switch body := v.(type) {
	case []byte:
		if len(body) == 0 {
			return nil, false
		}
		return body, true
	case string:
		if body == "" {
			return nil, false
		}
		return []byte(body), true
	default:
		return nil, false
	}
}

func readRawRequestBody(c *gin.Context) ([]byte, error) {
	if body, ok := rawRequestBodyFromContext(c); ok {
		return body, nil
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	setRawRequestBody(c, body)
	return body, nil
}

const ingressRequestBodyContextKey = "ingress_raw_body"

func setIngressRequestBodyIfAbsent(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	if _, exists := c.Get(ingressRequestBodyContextKey); exists {
		return
	}
	// The request-size middleware already owns this immutable buffer for the
	// request lifetime. Retain the same slice instead of copying the full body.
	c.Set(ingressRequestBodyContextKey, body)
}

func ingressRequestBody(c *gin.Context, fallback []byte) []byte {
	if c != nil {
		if value, exists := c.Get(ingressRequestBodyContextKey); exists {
			if body, ok := value.([]byte); ok {
				return body
			}
		}
	}
	return fallback
}

func setRawRequestBody(c *gin.Context, body []byte) {
	if c != nil {
		c.Set("raw_body", body)
	}
}

const requestCompactionMetaContextKey = "request_compaction_meta"

type requestCompactionMeta struct {
	// ProtocolTriggered is the wire-level compaction_trigger control. Only this
	// value may affect routing, account pinning, or protocol-specific timeouts.
	ProtocolTriggered bool
	// UsageTriggered is the observability signal persisted in usage_logs.compact.
	UsageTriggered bool
	// HasHistory marks direct durable compaction/context_compaction input items.
	HasHistory bool
}

func cacheRequestCompactionMeta(c *gin.Context, meta requestCompactionMeta) {
	if c != nil {
		c.Set(requestCompactionMetaContextKey, meta)
	}
}

func cachedRequestCompactionMeta(c *gin.Context) (requestCompactionMeta, bool) {
	if c == nil {
		return requestCompactionMeta{}, false
	}
	value, exists := c.Get(requestCompactionMetaContextKey)
	if !exists {
		return requestCompactionMeta{}, false
	}
	meta, ok := value.(requestCompactionMeta)
	return meta, ok
}

func requestCompactionMetaForHTTP(c *gin.Context, body []byte) requestCompactionMeta {
	meta := requestBodyCompactionMeta(body)
	if c != nil && c.Request != nil {
		if turnMetadataIndicatesCompaction(c.GetHeader("X-Codex-Turn-Metadata")) {
			meta.UsageTriggered = true
		}
		if c.Request.URL != nil && isCompactUsageEndpoint(c.Request.URL.Path) {
			meta.UsageTriggered = true
		}
	}
	return meta
}

// requestBodyCompactionMeta inspects only direct input items plus the canonical
// per-turn metadata field. It deliberately does not recurse into messages,
// content, tool output, or arbitrary stringified JSON.
func requestBodyCompactionMeta(body []byte) requestCompactionMeta {
	meta := requestCompactionMeta{}
	input := gjson.GetBytes(body, "input")
	inspect := func(item gjson.Result) {
		switch {
		case gjsonResultIsCompactionTrigger(item):
			meta.ProtocolTriggered = true
		case gjsonResultIsCompactionHistory(item):
			meta.HasHistory = true
		}
	}
	if input.Exists() {
		if input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				inspect(item)
				return true
			})
		} else {
			inspect(input)
		}
	}

	meta.UsageTriggered = meta.ProtocolTriggered
	clientMetadata := gjson.GetBytes(body, "client_metadata")
	if clientMetadata.IsObject() {
		turnMetadata := clientMetadata.Get("x-codex-turn-metadata")
		if turnMetadata.Type == gjson.String && turnMetadataIndicatesCompaction(turnMetadata.String()) {
			meta.UsageTriggered = true
		}
	}
	return meta
}

func turnMetadataIndicatesCompaction(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || !gjson.Valid(raw) {
		return false
	}
	root := gjson.Parse(raw)
	if !root.IsObject() {
		return false
	}
	requestKind := root.Get("request_kind")
	return requestKind.Type == gjson.String &&
		strings.EqualFold(strings.TrimSpace(requestKind.String()), "compaction")
}

// requestBodyHasCompactionTrigger reports only the protocol-level control used
// for routing. Metadata-only local compaction turns must remain on /responses.
func requestBodyHasCompactionTrigger(body []byte) bool {
	return requestBodyCompactionMeta(body).ProtocolTriggered
}

// storeHasAvailableCodexAccount 判断账号池中是否还有可调度的官方（非中转）账号。
// 这是池级判断，不含 API Key 级的账号分组/套餐约束。流式 Remote Compact 不再
// 用它来排除中转账号（issue #540）。
func (h *Handler) storeHasAvailableCodexAccount() bool {
	if h == nil || h.store == nil {
		return false
	}
	for _, account := range h.store.Accounts() {
		if account.IsRelayStyle() {
			continue
		}
		if account.IsAvailable() {
			return true
		}
	}
	return false
}

// excludeRelayAccountsFilter 在既有过滤器上追加"排除中转/Grok 账号"约束。
func excludeRelayAccountsFilter(inner auth.AccountFilter) auth.AccountFilter {
	return func(account *auth.Account) bool {
		if account == nil || account.IsRelayStyle() {
			return false
		}
		return inner == nil || inner(account)
	}
}

func relayOnlyAccountFilter(inner auth.AccountFilter) auth.AccountFilter {
	return func(account *auth.Account) bool {
		if account == nil || !account.IsRelayStyle() {
			return false
		}
		return inner == nil || inner(account)
	}
}

func responseCachePreparationFailure(prepared responsesBodyPreparation) (status int, reason string, unavailable bool) {
	if prepared.PreviousResponseID == "" || prepared.Bypassed || prepared.CacheLookup.Kind == responseCacheLookupHit {
		return 0, "", false
	}
	switch prepared.CacheLookup.Kind {
	case responseCacheLookupKnownEvicted:
		return http.StatusConflict, "local_context_evicted", true
	case responseCacheLookupKnownOversize:
		return http.StatusConflict, "local_context_oversize", true
	case responseCacheLookupReconstructionTooLarge:
		return http.StatusConflict, "reconstruction_too_large", true
	case responseCacheLookupBackendCorrupt:
		return http.StatusConflict, "backend_value_corrupt", true
	case responseCacheLookupBackendError:
		if prepared.RequiresLocalContext {
			return http.StatusServiceUnavailable, "backend_unavailable", true
		}
	case responseCacheLookupMiss, responseCacheLookupExpired:
		if prepared.RequiresLocalContext {
			return http.StatusConflict, "missing_required_call_context", true
		}
	}
	return 0, "", false
}

func sendResponseContextUnavailable(c *gin.Context, status int, reason string) {
	code := api.ErrCodeResponseContextUnavailable
	errType := api.ErrorTypeInvalidRequest
	message := "Previous response context is unavailable"
	if status == http.StatusServiceUnavailable {
		code = api.ErrCodeServiceUnavailable
		errType = api.ErrorTypeServer
		message = "Previous response context backend is temporarily unavailable"
	}
	if status == http.StatusConflict {
		recordResponseCacheKnownUnavailableError()
	}
	api.SendErrorWithStatus(c, api.NewAPIErrorWithDetails(
		code,
		message,
		errType,
		api.ErrorDetail{Field: "previous_response_id", Message: reason},
	), status)
}

func gjsonResultIsCompactionTrigger(result gjson.Result) bool {
	return result.IsObject() && strings.EqualFold(strings.TrimSpace(result.Get("type").String()), "compaction_trigger")
}

func gjsonResultIsCompactionHistory(result gjson.Result) bool {
	if !result.IsObject() {
		return false
	}
	itemType := strings.TrimSpace(result.Get("type").String())
	return strings.EqualFold(itemType, "compaction") || strings.EqualFold(itemType, "context_compaction")
}

// extractReasoningEffort 从请求体提取推理强度
// 支持 reasoning.effort（Responses API）和 reasoning_effort（Chat Completions API）
func extractReasoningEffort(body []byte) string {
	// Responses API: reasoning.effort
	if effort := gjson.GetBytes(body, "reasoning.effort").String(); effort != "" {
		return effort
	}
	// Chat Completions API: reasoning_effort
	if effort := gjson.GetBytes(body, "reasoning_effort").String(); effort != "" {
		return effort
	}
	return ""
}

// responsesPhaseTimingHeader /v1/responses 请求准备阶段分段耗时的响应头。
// 首个 attempt 开始前写入(SSE 首字节尚未发出),下游网关可据此把
// "网关侧首字慢 vs codex2api first_token_ms 快"的差值归因到具体阶段(issue #405)。
const responsesPhaseTimingHeader = "X-Codex2API-Phase-Timing"

// emitResponsesPhaseTimings 输出 /v1/responses 首个 attempt 之前的分段耗时。
// 各分段含义:mw=进入 handler 前的中间件链(含 body 缓存/解压/鉴权),
// read=handler 内读取请求体,validate=模型映射与请求校验,
// prepare=上游请求体重建(Unmarshal→map→Marshal),schedule=Key 限流检查与账号调度。
// attempt 开始后的耗时由既有 first_token_ms 覆盖,两者相加即 handler 全程。
func emitResponsesPhaseTimings(c *gin.Context, logModel string, bodySize int, handlerStart, bodyReadDone, validateDone, prepareDone time.Time) {
	now := time.Now()
	middlewareMs := int64(0)
	if reqCtx := api.GetRequestContext(c); reqCtx != nil && !reqCtx.StartTime.IsZero() {
		middlewareMs = handlerStart.Sub(reqCtx.StartTime).Milliseconds()
	}
	summary := fmt.Sprintf("mw=%d;read=%d;validate=%d;prepare=%d;schedule=%d;body_kb=%d",
		middlewareMs,
		bodyReadDone.Sub(handlerStart).Milliseconds(),
		validateDone.Sub(bodyReadDone).Milliseconds(),
		prepareDone.Sub(validateDone).Milliseconds(),
		now.Sub(prepareDone).Milliseconds(),
		bodySize/1024)
	c.Header(responsesPhaseTimingHeader, summary)
	log.Printf("[TIMING] /v1/responses model=%s %s", logModel, summary)
}

// extractServiceTier 从请求体提取服务等级
func extractServiceTier(body []byte) string {
	if tier := gjson.GetBytes(body, "service_tier").String(); tier != "" {
		return tier
	}
	return gjson.GetBytes(body, "serviceTier").String()
}

const upstreamErrorKindMessageTooBig = "message_too_big"

// upstreamErrorKindWsBusyAcquire 是 wsrelay busy session acquire 超时（issue #413）：
// 同会话的前一个请求长时间占用 WS 连接导致的排队超时，属会话占用而非账号故障。
const upstreamErrorKindWsBusyAcquire = "ws_busy_acquire"

// isWsBusyAcquireTimeoutError 按错误文案识别 busy acquire 超时。wsrelay 依赖 proxy，
// proxy 无法反向导入其哨兵类型，跨包只能靠稳定的错误消息片段匹配。
func isWsBusyAcquireTimeoutError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "waiting for busy session")
}

// shouldPenalizeTransportKind 返回该传输失败是否应计入账号健康惩罚。
// busy acquire 超时的账号本身没有故障，惩罚会让 fast scheduler 错误降权（issue #413）。
func shouldPenalizeTransportKind(kind string) bool {
	return kind != "" && kind != upstreamErrorKindWsBusyAcquire
}

func isWebsocketMessageTooBigError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "message too big") ||
		strings.Contains(msg, "close 1009") ||
		strings.Contains(msg, "read limit exceeded")
}

func websocketMessageTooBigSource(message string) string {
	if strings.Contains(strings.ToLower(message), "read limit exceeded") {
		return "local_read_limit"
	}
	return "peer_close"
}

func isWebsocketMessageTooBigOutcome(outcome streamOutcome) bool {
	return outcome.failureKind == upstreamErrorKindMessageTooBig
}

func shouldFallbackWebsocketMessageTooBigToHTTP(outcome streamOutcome, useWebsocket bool, wroteAnyBody bool, ctxErr, writeErr error) bool {
	if !useWebsocket || !isWebsocketMessageTooBigOutcome(outcome) {
		return false
	}
	if wroteAnyBody || ctxErr != nil || writeErr != nil {
		return false
	}
	return true
}

func classifyTransportFailure(err error) string {
	if err == nil {
		return ""
	}

	if isWebsocketMessageTooBigError(err) {
		return upstreamErrorKindMessageTooBig
	}
	if isWsBusyAcquireTimeoutError(err) {
		return upstreamErrorKindWsBusyAcquire
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out") || strings.Contains(msg, "deadline exceeded") {
		return "timeout"
	}
	return "transport"
}

func classifyHTTPFailure(statusCode int) string {
	switch {
	case statusCode == http.StatusUnauthorized:
		return "unauthorized"
	case statusCode == http.StatusTooManyRequests:
		return "" // 429 由 applyCooldown 单独处理
	case statusCode >= 500:
		return "server"
	case statusCode >= 400:
		return "client"
	default:
		return ""
	}
}

type streamOutcome struct {
	logStatusCode  int
	failureKind    string
	failureMessage string
	penalize       bool
	// capacityShed 标记这是一次上游容量降载（server_is_overloaded / slow_down）。
	// penalize 仍为 true 以复用首包前帧缓冲/透明重试机制，但账号连击/健康度不因此
	// 受罚，且透明重试优先在同账号退避几次而非立即换号（见 reportStreamOutcomeFailure）。
	capacityShed bool
	// verifyAccountAuth 标记这是一次 WS 上游读流失败（如 close 1008 policy violation）。
	// WS 通道下 token 失效表现为上游主动关闭而非 401，需异步跑一次探针确认账号鉴权状态，
	// 命中 401 才按 unauthorized 冷却，避免失效账号不被封、反复被调度。
	verifyAccountAuth bool
}

// isWebsocketUpstreamClose 判断读流错误是否来自 WS 上游异常关闭/读失败。
// wsrelay 的读错误统一以 "websocket read error:" 前缀包裹（见 wsrelay/executor.go）。
func isWebsocketUpstreamClose(err error) bool {
	return err != nil && strings.Contains(err.Error(), "websocket read error")
}

func classifyStreamOutcome(ctxErr, readErr, writeErr error, gotTerminal bool) streamOutcome {
	if gotTerminal {
		return streamOutcome{logStatusCode: http.StatusOK}
	}

	if ctxErr != nil || writeErr != nil {
		msg := "下游客户端提前断开"
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			msg = "下游请求上下文超时"
		case writeErr != nil:
			msg = fmt.Sprintf("写回下游失败: %v", writeErr)
		case ctxErr != nil:
			msg = fmt.Sprintf("下游请求提前取消: %v", ctxErr)
		}
		return streamOutcome{
			logStatusCode:  logStatusClientClosed,
			failureMessage: msg,
		}
	}

	if readErr != nil {
		kind := classifyTransportFailure(readErr)
		if kind == "" {
			kind = "transport"
		}
		messageTooBig := kind == upstreamErrorKindMessageTooBig
		return streamOutcome{
			logStatusCode:     logStatusUpstreamStreamBreak,
			failureKind:       kind,
			failureMessage:    fmt.Sprintf("上游流读取失败: %v", readErr),
			penalize:          !messageTooBig,
			verifyAccountAuth: !messageTooBig && isWebsocketUpstreamClose(readErr),
		}
	}

	return streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "上游流提前结束，未收到终止事件",
		penalize:       true,
	}
}

func classifyResponseFailedOutcome(payload []byte) streamOutcome {
	statusCode := responseFailedStatusCode(payload)
	message := usageLogErrorMessage(statusCode, payload)
	cyberPolicy := isExplicitUpstreamCyberPolicy(payload)
	if cyberPolicy {
		// Upstream response.failed events often omit status_code, whose generic
		// fallback is 500. CYB is a deterministic request-policy rejection: expose
		// it as 400 and never rotate accounts/retry the same user request.
		statusCode = http.StatusBadRequest
		message = upstreamCyberPolicyUserMessage
	}
	if strings.TrimSpace(message) == "" || message == fmt.Sprintf("HTTP %d", statusCode) {
		message = "上游返回 response.failed"
	}
	kind := upstreamErrorKind(statusCode, payload, codex429Decision{})
	if cyberPolicy {
		kind = "cyber_policy"
	}
	if kind == "" {
		if statusCode >= 500 {
			kind = "server"
		} else {
			kind = "client"
		}
	}
	// 400 中"账号不支持该模型"属账号权益问题，冷却后换号重试有意义，视同可重试故障。
	modelUnsupported := statusCode == http.StatusBadRequest && isCodexModelUnsupportedError(responseFailedErrorBody(payload))
	return streamOutcome{
		logStatusCode:  statusCode,
		failureKind:    kind,
		failureMessage: message,
		penalize:       !cyberPolicy && (statusCode == http.StatusUnauthorized || statusCode == http.StatusTooManyRequests || statusCode >= 500 || modelUnsupported),
		capacityShed:   !capacityShedHandlingDisabled() && isCapacityShedPayload(payload),
	}
}

// reportStreamOutcomeFailure 上报流内 response.failed 类故障到账号健康度，但容量降载
// 例外：它是按模型/身份容量分桶的请求级瞬时信号，换号不改变被降载因素，计入连击只会
// 在高峰期把整池账号逐个调度降权。跳过上报即"软信号保留、硬惩罚移除"。
func (h *Handler) reportStreamOutcomeFailure(account *auth.Account, outcome streamOutcome, d time.Duration) {
	if outcome.capacityShed {
		return
	}
	h.store.ReportRequestFailure(account, outcome.failureKind, d)
}

// unbindOrRetainAffinityForCapacityShed 在首包前透明重试时决定是否保留会话亲和。
// 容量降载先在同账号退避重试 maxCapacityShedSameAccountRetries 次（保留亲和 → 下一轮
// 仍优先选回同账号），预算耗尽后解绑并软排除该账号强制换号；其余故障一律立即解绑换号。
// retries 是请求作用域的按账号计数器，每个降载账号各有一份退避预算。
//
// 必须软排除而非仅解绑：降载不惩罚账号健康度，若只解绑亲和，调度会立刻把这个仍是
// 满血的账号重新选回并重绑，"耗尽后换号"就名存实亡。软排除在账号池试完后由 ResetSoft
// 清空，不会永久搁置请求。
func (h *Handler) unbindOrRetainAffinityForCapacityShed(exclusions *retryAccountExclusions, affinityKey string, account *auth.Account, outcome streamOutcome, retries map[int64]int) {
	id := account.ID()
	if capacityShedRetainsAffinity(outcome, retries[id]) {
		retries[id]++
		return
	}
	h.store.UnbindSessionAffinity(affinityKey, id)
	if outcome.capacityShed {
		exclusions.MarkSoft(id)
	}
}

// capacityShedRetainsAffinity 判断本次容量降载是否仍在该账号的同账号退避重试预算内。
func capacityShedRetainsAffinity(outcome streamOutcome, retriesSoFar int) bool {
	return outcome.capacityShed && retriesSoFar < maxCapacityShedSameAccountRetries
}

func responseFailedErrorBody(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	if gjson.GetBytes(payload, "error").Exists() {
		return payload
	}
	for _, path := range []string{
		"response.error",
		"response.status_details.error",
	} {
		result := gjson.GetBytes(payload, path)
		raw := strings.TrimSpace(result.Raw)
		if raw == "" || raw == "null" {
			continue
		}
		return []byte(`{"error":` + raw + `}`)
	}
	return payload
}

// responseFailedRetryable 判断一个 response.failed 终止事件是否属于"换号重试有意义"的上游故障
// （额度耗尽/限流/5xx/401）。用于在首包前透明换号，避免把可恢复的失败帧直接下发给
// WebSocket 客户端而触发反复 Reconnecting。非可重试故障（如 invalid_request）仍照常透传。
func responseFailedRetryable(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	return classifyResponseFailedOutcome(payload).penalize
}

// isRetryableUpstreamErrorFrame 判断流内 {"type":"error"} 帧是否为可重试的上游故障。
// 上游容量降载的真实序列是「event: error → event: response.failed」：error 帧若被
// 当作首个客户端输出立即写出，wroteAnyBody 一旦置位，随后的 response.failed 就永远
// 进不了首包前静默换号分支，降载错误只能原样透传——Codex CLI 对
// server_is_overloaded/slow_down 按闭集判致命并直接终止会话。可重试类 error 帧
// 必须与生命周期帧一样缓冲；不可重试类（content_policy/invalid_request 等）
// 维持原样立即转发，保留上游错误细节。
func isRetryableUpstreamErrorFrame(eventType string, payload []byte) bool {
	if strings.TrimSpace(eventType) != "error" {
		return false
	}
	return responseFailedRetryable(payload)
}

// capacityShedRetryableClientCode 是把上游容量降载错误透传给下游时改写使用的
// 错误码。Codex CLI 按闭集分类：server_is_overloaded / slow_down 被判致命
// （提示 "Selected model is at capacity. Please try a different model." 并终止
// 会话），而 server_error 等闭集之外的错误码会进入客户端内置的退避重试。
const capacityShedRetryableClientCode = "server_error"

// maxCapacityShedSameAccountRetries 是容量降载时在同一账号上退避重试的次数上限。
// 降载是按客户端身份/模型容量分桶的请求级信号，换号并不改变被降载的因素，先在
// 同账号退避重试若干次，耗尽后再换号（且全程不计入账号连击/健康度）。
const maxCapacityShedSameAccountRetries = 2

// capacityShedHandlingDisabled 报告是否通过环境变量退回旧行为（把降载当普通 500
// 惩罚账号并立即换号）。CODEX_DISABLE_CAPACITY_SHED_HANDLING=1（或 true）时生效。
func capacityShedHandlingDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_DISABLE_CAPACITY_SHED_HANDLING"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// isCapacityShedPayload 判断 response.failed / error 帧是否为上游容量降载
// （server_is_overloaded / slow_down）。复用 sanitizeCapacityShedEventForClient
// 的三条 code path，供分类与失败上报两侧共用。
func isCapacityShedPayload(payload []byte) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	for _, path := range []string{"error.code", "response.error.code", "response.status_details.error.code"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String())) {
		case "server_is_overloaded", "slow_down":
			return true
		}
	}
	return false
}

// sanitizeCapacityShedEventForClient 把即将写给下游的 error / response.failed
// 事件中的容量降载错误码改写为客户端可重试的错误码。走到写出这一步说明网关侧
// 换号重试已不可用（流中途已有输出）或已用尽；保留原始降载码只会让 Codex CLI
// 就地终止会话。错误消息原样保留；监控、计费与账号冷却均基于改写前的原始
// payload（terminalFailurePayload 在写出前捕获），不受影响。rate_limit_exceeded
// 等其他错误码一律不动（客户端依赖原码解析重试延时）。
func sanitizeCapacityShedEventForClient(eventType string, payload []byte) []byte {
	switch strings.TrimSpace(eventType) {
	case "error", "response.failed":
	default:
		return payload
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	updated := payload
	for _, path := range []string{"error.code", "response.error.code", "response.status_details.error.code"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(updated, path).String())) {
		case "server_is_overloaded", "slow_down":
		default:
			continue
		}
		next, err := sjson.SetBytes(updated, path, capacityShedRetryableClientCode)
		if err != nil {
			return payload
		}
		updated = next
	}
	return updated
}

func (h *Handler) applyResponseFailedCooldown(account *auth.Account, payload []byte, resp *http.Response, model string) codex429Decision {
	if h == nil || account == nil || len(payload) == 0 {
		return codex429Decision{}
	}
	body := responseFailedErrorBody(payload)
	statusCode := responseFailedStatusCode(payload)
	return h.applyCooldownForModel(account, statusCode, body, resp, model)
}

func responseFailedStatusCode(payload []byte) int {
	for _, path := range []string{
		"response.status_code",
		"response.error.status_code",
		"response.status_details.error.status_code",
		"status_code",
		"error.status_code",
	} {
		code := int(gjson.GetBytes(payload, path).Int())
		if code >= 400 && code <= 599 {
			return code
		}
	}

	codeOrType := strings.ToLower(strings.Join([]string{
		gjson.GetBytes(payload, "response.error.code").String(),
		gjson.GetBytes(payload, "response.error.type").String(),
		gjson.GetBytes(payload, "response.status_details.error.code").String(),
		gjson.GetBytes(payload, "response.status_details.error.type").String(),
		gjson.GetBytes(payload, "error.code").String(),
		gjson.GetBytes(payload, "error.type").String(),
	}, " "))
	switch {
	case strings.Contains(codeOrType, "usage_limit"):
		return http.StatusTooManyRequests
	case strings.Contains(codeOrType, "rate_limit"):
		return http.StatusTooManyRequests
	case strings.Contains(codeOrType, "unauthorized") || strings.Contains(codeOrType, "invalid_api_key"):
		return http.StatusUnauthorized
	case strings.Contains(codeOrType, "payment"):
		return http.StatusPaymentRequired
	case strings.Contains(codeOrType, "forbidden"):
		return http.StatusForbidden
	case strings.Contains(codeOrType, "previous_response_not_found"):
		return http.StatusBadRequest
	// 确定性客户端错误：输入超上下文窗口/字段超长/模型不存在等，换号重试
	// 也必然失败。归为 400，避免落入 default 500 触发透明重试并惩罚账号
	// 健康度 (issue #310)。
	case strings.Contains(codeOrType, "context_length") ||
		strings.Contains(codeOrType, "context_window") ||
		strings.Contains(codeOrType, "above_max_length") ||
		strings.Contains(codeOrType, "model_not_found") ||
		strings.Contains(codeOrType, "unsupported"):
		return http.StatusBadRequest
	case strings.Contains(codeOrType, "invalid") || strings.Contains(codeOrType, "bad_request"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func shouldTransparentRetryStream(outcome streamOutcome, attempt int, maxRetries int, wroteAnyBody bool, ctxErr, writeErr error) bool {
	if attempt >= maxRetries {
		return false
	}
	if !outcome.penalize {
		return false
	}
	if wroteAnyBody || ctxErr != nil || writeErr != nil {
		return false
	}
	return true
}

func shouldSuppressRetryableResponseFailedBeforeFirstToken(eventType string, terminalFailurePayload []byte, ttftRecorded bool, wroteAnyBody bool, attempt int, maxRetries int, ctxErr, writeErr error) bool {
	if eventType != "response.failed" {
		return false
	}
	if ttftRecorded || wroteAnyBody || ctxErr != nil || writeErr != nil {
		return false
	}
	if attempt >= maxRetries {
		return false
	}
	return responseFailedRetryable(terminalFailurePayload)
}

// shouldReturnHTTPErrorForResponseFailed 判断:流式请求在首 token 之前收到
// response.failed(且尚未向下游写任何内容、客户端也未断开)时,应当中止 SSE 转发,
// 交由循环外按真实 HTTP 错误码返回,而不是把失败包装成 200 + [DONE]。
//
// 背景:pending 尚未 flush 时下游 HTTP 200 header 还没发出(见 stream_flush_writer.go),
// 此时若把 response.failed 写进流并补 [DONE],把本服务当上游的计费型中转层
// 会把它当成一次正常完成、按其本地预估的 input token 计费,
// 造成"上游拒绝(0 输出)却按 input 收费"。#310 已让 context_length_exceeded 等确定性
// 客户端错误不再换号重试,但流式下游返回仍是 200 + [DONE],本函数补上这一半。
//
// 注意:命中后除了中止转发,循环后的收尾 flush 也必须跳过(见 wroteAnyBody 守卫),
// 否则空 buffer 的 flusher.Flush 仍会提前提交 200 header,让循环外的 c.JSON(4xx) 失效。
func shouldReturnHTTPErrorForResponseFailed(eventType string, ttftRecorded, wroteAnyBody, clientGone bool) bool {
	return eventType == "response.failed" && !ttftRecorded && !wroteAnyBody && !clientGone
}

func imageGenerationOutputKey(item gjson.Result) string {
	if key := strings.TrimSpace(item.Get("id").String()); key != "" {
		return key
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return ""
	}
	return strings.TrimSpace(item.Get("output_format").String()) + "|" + result
}

func extractResponseImageGenerationOutput(data []byte, seen map[string]struct{}) (json.RawMessage, bool) {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil, false
	}
	if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
		return nil, false
	}
	item := gjson.GetBytes(data, "item")
	if !item.Exists() || !item.IsObject() || item.Get("type").String() != "image_generation_call" {
		return nil, false
	}
	if strings.TrimSpace(item.Get("result").String()) == "" {
		return nil, false
	}
	key := imageGenerationOutputKey(item)
	if key != "" && seen != nil {
		if _, ok := seen[key]; ok {
			return nil, false
		}
		seen[key] = struct{}{}
	}
	raw := []byte(item.Raw)
	var output map[string]any
	if err := json.Unmarshal(raw, &output); err == nil && addImageStatsToMap(output) {
		if annotated, err := json.Marshal(output); err == nil {
			raw = annotated
		}
	}
	return json.RawMessage(raw), true
}

func responseOutputItemDoneKey(item gjson.Result) string {
	if key := strings.TrimSpace(item.Get("id").String()); key != "" {
		return key
	}
	return strings.TrimSpace(item.Get("type").String()) + "|" + strings.TrimSpace(item.Raw)
}

func extractResponseOutputItemDone(data []byte, seen map[string]struct{}) (json.RawMessage, bool) {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil, false
	}
	if gjson.GetBytes(data, "type").String() != "response.output_item.done" {
		return nil, false
	}
	item := gjson.GetBytes(data, "item")
	if !item.Exists() || !item.IsObject() {
		return nil, false
	}
	key := responseOutputItemDoneKey(item)
	if key != "" && seen != nil {
		if _, ok := seen[key]; ok {
			return nil, false
		}
		seen[key] = struct{}{}
	}
	raw := []byte(item.Raw)
	if item.Get("type").String() == "image_generation_call" {
		var output map[string]any
		if err := json.Unmarshal(raw, &output); err == nil && addImageStatsToMap(output) {
			if annotated, err := json.Marshal(output); err == nil {
				raw = annotated
			}
		}
	}
	return json.RawMessage(raw), true
}

func restoreMissingResponseOutputs(responseJSON []byte, outputItems []json.RawMessage) []byte {
	if len(responseJSON) == 0 || len(outputItems) == 0 {
		return responseJSON
	}
	var response map[string]any
	if err := json.Unmarshal(responseJSON, &response); err != nil {
		return responseJSON
	}
	if outputs, ok := response["output"].([]any); ok && len(outputs) > 0 {
		return responseJSON
	}
	outputs := make([]any, 0, len(outputItems))
	for _, rawItem := range outputItems {
		if len(rawItem) == 0 || !gjson.ValidBytes(rawItem) {
			continue
		}
		var decoded any
		if err := json.Unmarshal(rawItem, &decoded); err != nil {
			continue
		}
		outputs = append(outputs, decoded)
	}
	if len(outputs) == 0 {
		return responseJSON
	}
	response["output"] = outputs
	restored, err := json.Marshal(response)
	if err != nil {
		return responseJSON
	}
	return restored
}

func appendMissingResponseImageOutputs(responseJSON []byte, imageOutputs []json.RawMessage) []byte {
	if len(responseJSON) == 0 {
		return responseJSON
	}
	var response map[string]any
	if err := json.Unmarshal(responseJSON, &response); err != nil {
		return responseJSON
	}

	seen := make(map[string]struct{})
	changed := false
	outputs, _ := response["output"].([]any)
	for _, rawOutput := range outputs {
		outputMap, ok := rawOutput.(map[string]any)
		if !ok {
			continue
		}
		if firstNonEmptyAnyString(outputMap["type"]) != "image_generation_call" {
			continue
		}
		outputBytes, err := json.Marshal(outputMap)
		if err != nil {
			continue
		}
		item := gjson.ParseBytes(outputBytes)
		if key := imageGenerationOutputKey(item); key != "" {
			seen[key] = struct{}{}
		}
		if addImageStatsToMap(outputMap) {
			changed = true
		}
	}

	for _, rawImage := range imageOutputs {
		if len(rawImage) == 0 || !gjson.ValidBytes(rawImage) {
			continue
		}
		item := gjson.ParseBytes(rawImage)
		key := imageGenerationOutputKey(item)
		if key != "" {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		var decoded any
		if err := json.Unmarshal(rawImage, &decoded); err != nil {
			continue
		}
		if outputMap, ok := decoded.(map[string]any); ok {
			addImageStatsToMap(outputMap)
		}
		outputs = append(outputs, decoded)
		changed = true
	}
	if !changed {
		return responseJSON
	}
	response["output"] = outputs
	merged, err := json.Marshal(response)
	if err != nil {
		return responseJSON
	}
	return merged
}

// RegisterRoutes 注册路由
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	auth := h.authMiddleware()

	// /v1 前缀路由（标准路径）
	v1 := r.Group("/v1")
	v1.Use(auth)
	v1.POST("/prompt-filter/newapi/verify", h.VerifyNewAPIPolicyHandshake)
	v1.POST("/chat/completions", h.ChatCompletions)
	v1.POST("/responses", h.Responses)
	v1.GET("/responses", h.ResponsesWebSocket)
	v1.GET("/realtime", h.RealtimeWebSocket)
	v1.POST("/responses/compact", h.ResponsesCompact)
	v1.POST("/images/generations", h.ImagesGenerations)
	v1.POST("/images/edits", h.ImagesEdits)
	// Grok 生视频:异步任务创建 + 客户端轮询 + 产物代理下载
	v1.POST("/videos/generations", h.VideosGenerations)
	v1.POST("/videos/edits", h.VideosEdits)
	v1.POST("/videos/extensions", h.VideosExtensions)
	v1.GET("/videos/:request_id", h.VideosStatus)
	v1.GET("/videos/:request_id/content", h.VideosContent)
	v1.POST("/messages", h.Messages)
	v1.POST("/messages/count_tokens", h.CountTokens)
	v1.POST("/responses/input_tokens", h.ResponsesInputTokens)
	// Codex CLI / Codex App 从 /models?client_version=... 刷新模型选单，期望
	// manifest 格式；client_version 是 Codex 客户端的天然指纹，普通 OpenAI
	// 客户端不携带，其余请求保持 OpenAI 格式列表不变。
	v1.GET("/models", h.listModelsOrManifest)
	// Codex CLI web_search = "live" 的 standalone 联网搜索端点 (issue #359)
	v1.POST("/alpha/search", h.CodexAlphaSearchHandler)
	v1.POST("/live", h.LiveCreate)
	v1.GET("/live/:call_id", h.LiveSideband)

	// 无前缀路由（兼容 base_url 已包含 /v1 的客户端）
	r.POST("/chat/completions", auth, h.ChatCompletions)
	r.POST("/responses", auth, h.Responses)
	r.GET("/responses", auth, h.ResponsesWebSocket)
	r.GET("/realtime", auth, h.RealtimeWebSocket)
	r.POST("/responses/compact", auth, h.ResponsesCompact)
	r.POST("/images/generations", auth, h.ImagesGenerations)
	r.POST("/images/edits", auth, h.ImagesEdits)
	r.POST("/videos/generations", auth, h.VideosGenerations)
	r.POST("/videos/edits", auth, h.VideosEdits)
	r.POST("/videos/extensions", auth, h.VideosExtensions)
	r.GET("/videos/:request_id", auth, h.VideosStatus)
	r.GET("/videos/:request_id/content", auth, h.VideosContent)
	r.POST("/messages", auth, h.Messages)
	r.POST("/messages/count_tokens", auth, h.CountTokens)
	r.POST("/responses/input_tokens", auth, h.ResponsesInputTokens)
	r.GET("/models", auth, h.listModelsOrManifest)
	r.POST("/alpha/search", auth, h.CodexAlphaSearchHandler)
	r.POST("/live", auth, h.LiveCreate)
	r.GET("/live/:call_id", auth, h.LiveSideband)

	codexDirect := r.Group("/backend-api/codex")
	codexDirect.Use(auth)
	codexDirect.POST("/responses", h.Responses)
	codexDirect.GET("/responses", h.ResponsesWebSocket)
	codexDirect.GET("/models", h.CodexModelsManifestHandler)
	codexDirect.POST("/alpha/search", h.CodexAlphaSearchHandler)
	codexDirect.POST("/realtime/calls", h.LiveCreate)
	codexDirect.POST("/responses/*subpath", func(c *gin.Context) {
		subpath := strings.TrimSpace(c.Param("subpath"))
		if subpath == "/compact" || strings.HasPrefix(subpath, "/compact/") {
			h.ResponsesCompact(c)
			return
		}
		h.Responses(c)
	})
	codexDirect.GET("/:call_id", h.LiveSideband)
}

// APIKeyAuthMiddleware exposes the standard /v1 API key authentication middleware
// for companion routes that live outside proxy.RegisterRoutes.
func (h *Handler) APIKeyAuthMiddleware() gin.HandlerFunc {
	return h.authMiddleware()
}

// authMiddleware API Key 鉴权中间件（增强版，带安全日志）
//
// 安全策略（fail-closed）：
//   - 默认情况下，未配置任何 API Key 时直接拒绝请求（503），避免裸奔账号池。
//   - 仅当显式设置 CODEX_ALLOW_ANONYMOUS=true 时才在无密钥情况下放行（兼容内网/测试）。
func (h *Handler) authMiddleware() gin.HandlerFunc {
	allowAnonymous := h.cfg != nil && h.cfg.AllowAnonymousV1
	return func(c *gin.Context) {
		attachUserAgentAudit(c)
		attachWsAcquireAudit(c)
		// 如果没有配置任何密钥
		if !h.hasAnyKeys() {
			if allowAnonymous {
				// 显式允许匿名访问（旧行为，仅在 CODEX_ALLOW_ANONYMOUS=true 时启用）
				c.Next()
				return
			}
			// fail-closed：未配置 API Key 即拒绝，避免账号池被未授权调用
			security.SecurityAuditLog("V1_BLOCKED_NO_KEYS", fmt.Sprintf("path=%s ip=%s", c.Request.URL.Path, c.ClientIP()))
			api.SendError(c, api.NewAPIError(
				api.ErrCodeServiceUnavailable,
				"Service is not configured: no API key has been created yet. Please add at least one API key in the admin dashboard, or set CODEX_ALLOW_ANONYMOUS=true to disable this check.",
				api.ErrorTypeServer,
			))
			c.Abort()
			return
		}

		authHeader := c.GetHeader("Authorization")
		// OpenAI-compatible WebSocket clients may carry the API key in the
		// standard subprotocol list instead of an Authorization header:
		//   Sec-WebSocket-Protocol: realtime, openai-insecure-api-key.<key>
		// Only honor it on an actual WebSocket upgrade so an ordinary HTTP
		// request cannot smuggle authentication through an unrelated header.
		if authHeader == "" && isResponsesWebSocketUpgradeRequest(c.Request) {
			if key := apiKeyFromWebSocketSubprotocol(c.GetHeader("Sec-WebSocket-Protocol")); key != "" {
				authHeader = "Bearer " + key
			}
		}
		// 兼容 Anthropic 客户端的多种认证方式:
		// - x-api-key: Anthropic SDK 默认方式
		// - ANTHROPIC_AUTH_TOKEN: Claude Code 通过此环境变量设置，
		//   实际发送为 Authorization: Bearer <token>（已被上面覆盖）
		//   或 anthropic-auth-token 自定义 header
		if authHeader == "" {
			for _, h := range []string{"x-api-key", "anthropic-auth-token"} {
				if v := strings.TrimSpace(c.GetHeader(h)); v != "" {
					authHeader = "Bearer " + v
					break
				}
			}
		}
		if authHeader == "" {
			// Use standardized error format from api package
			api.SendError(c, api.ErrMissingAPIKey)
			c.Abort()
			return
		}

		// 清理输入
		authHeader = security.SanitizeInput(authHeader)

		key := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		apiKeyRow, ok, resolveErr := h.resolveAPIKey(key)
		if resolveErr != nil {
			// DB/基础设施暂时性故障：返回 503，不当成客户端 key 无效（issue #323）。
			// 不记 AUTH_FAILED 审计日志，避免污染凭证攻击告警。
			api.SendError(c, api.ErrServiceUnavailable)
			c.Abort()
			return
		}
		if !ok {
			// 记录安全审计日志（脱敏）
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			// Use standardized error format from api package
			api.SendError(c, api.ErrInvalidAPIKey)
			c.Abort()
			return
		}
		if apiKeyRow.IsExpired(time.Now()) {
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED_EXPIRED_KEY", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			api.SendError(c, api.NewAPIError(api.ErrCodeInvalidAuth, "API key has expired", api.ErrorTypeAuthentication))
			c.Abort()
			return
		}
		if apiKeyRow.IsQuotaExhausted() {
			maskedKey := security.MaskAPIKey(key)
			security.SecurityAuditLog("AUTH_FAILED_QUOTA_EXHAUSTED", fmt.Sprintf("path=%s ip=%s key=%s", c.Request.URL.Path, c.ClientIP(), maskedKey))
			api.SendError(c, api.NewAPIError(api.ErrCodeRateLimitReached, "API key quota exhausted", api.ErrorTypeRateLimit))
			c.Abort()
			return
		}
		c.Set(contextAPIKeyID, apiKeyRow.ID)
		c.Set(contextAPIKeyName, strings.TrimSpace(apiKeyRow.Name))
		c.Set(contextAPIKeyMasked, security.MaskAPIKey(apiKeyRow.Key))
		c.Set(contextAPIKeyRow, apiKeyRow)
		c.Set("apiKey", key)
		if h.enforceRequiredNewAPIIdentityAtIngress(c) {
			c.Abort()
			return
		}
		c.Next()
	}
}

func apiKeyFromWebSocketSubprotocol(header string) string {
	for _, item := range strings.Split(header, ",") {
		item = strings.TrimSpace(item)
		const prefix = "openai-insecure-api-key."
		if strings.HasPrefix(item, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(item, prefix))
		}
	}
	return ""
}

// ==================== /v1/responses ====================

// getMaxRetries 从 store 读取可配置的最大重试次数
func (h *Handler) getMaxRetries() int {
	return h.store.GetMaxRetries()
}

func (h *Handler) getMaxRateLimitRetries() int {
	if h == nil || h.store == nil {
		return 1
	}
	return h.store.GetMaxRateLimitRetries()
}

// effectiveMaxRateLimitRetries 返回当前账号适用的限流(429)换号重试上限：Grok 账号可在
// grok 系统设置里配置专属次数(free 号限流频繁，换号重试更易成功)；未配置(0)或非 Grok
// 账号回落到全局 max_rate_limit_retries。按当前 attempt 的账号动态取值。
func (h *Handler) effectiveMaxRateLimitRetries(account *auth.Account, fallback int) int {
	if account != nil && account.IsGrokAPI() && h.store != nil {
		if n := h.store.GrokMaxRateLimitRetries(); n > 0 {
			return n
		}
	}
	return fallback
}

const (
	logStatusClientClosed        = 499
	logStatusUpstreamStreamBreak = 598
	// AccessLogStatusContextKey 允许流处理器在 HTTP 200 header 已提交后，
	// 把最终的内部结果（如客户端断开的 499）提供给访问日志中间件。
	AccessLogStatusContextKey = "x-access-log-status"
)

// upstreamStreamBreakMessage 是断流反馈给下游的稳定可读消息；机器识别用
// 稳定错误码 ErrorCodeUpstreamStreamBreak，下游网关/客户端可据此自动重试。
const upstreamStreamBreakMessage = "Upstream stream ended prematurely; safe to retry"

// writeResponsesStreamBreakEvent 在已写出正文的 Responses SSE 流上合成
// response.failed 终止事件。首包后断流无法整段静默重试，又不能让 SSE 静默
// EOF——下游会把截断响应当正常 200 收尾，既无从感知失败也无从重试
// (issue #473)。不发 response.completed，避免截断响应被当成功计费。
func writeResponsesStreamBreakEvent(w *streamFlushWriter) error {
	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"` +
		ErrorCodeUpstreamStreamBreak + `","message":"` + upstreamStreamBreakMessage + `"}}}`)
	if err := w.WriteSSEData(payload); err != nil {
		return err
	}
	return w.Flush()
}

// writeChatCompletionsStreamBreakEvent 同上，OpenAI Chat 协议形态：流内 error
// 对象，且不补 [DONE]——缺失 [DONE] 本身也是下游可识别的失败信号。
func writeChatCompletionsStreamBreakEvent(w *streamFlushWriter) error {
	payload := []byte(`{"error":{"message":"` + upstreamStreamBreakMessage +
		`","type":"` + ErrorTypeUpstreamError + `","code":"` + ErrorCodeUpstreamStreamBreak + `"}}`)
	if err := w.WriteSSEData(payload); err != nil {
		return err
	}
	return w.Flush()
}

// shouldWriteStreamBreakEvent 判断是否需要向下游写合成断流终止事件：
// 已写过正文、上游未给终态、客户端还在线。写过正文意味着透明重试窗口
// 已关闭（shouldTransparentRetryStream 要求零写入），这是最后的失败信号出口。
func shouldWriteStreamBreakEvent(gotTerminal, wroteAnyBody bool, ctxErr, writeErr error) bool {
	return !gotTerminal && wroteAnyBody && ctxErr == nil && writeErr == nil
}

// isResponsesSuccessTerminalEvent 判断事件是否为 Responses 的正常终态。
// response.incomplete 与 response.completed 同为正常终态：上游按
// max_output_tokens 截断时只发前者，且照样带完整 output 与 usage。漏认它会
// 让收尾逻辑把正常截断当断流——合成假的 response.failed / overloaded_error、
// 丢弃真实 usage 改用估算、并按断流惩罚账号。
func isResponsesSuccessTerminalEvent(eventType string) bool {
	return eventType == "response.completed" || eventType == "response.incomplete"
}

// isResponsesTerminalEvent 覆盖 Responses 的全部终态（正常/截断/失败），
// 供 SSE 读取循环判定"读到这里就可以收工"。
func isResponsesTerminalEvent(eventType string) bool {
	return isResponsesSuccessTerminalEvent(eventType) || eventType == "response.failed"
}

// responsesIncompleteFinishReason 把 Responses 的截断原因映射成 Chat 的
// finish_reason；非截断终态返回空串表示"沿用推导值"。
func responsesIncompleteFinishReason(eventType, reason string) string {
	if eventType != "response.incomplete" {
		return ""
	}
	if reason == "content_filter" {
		return "content_filter"
	}
	return "length"
}

// isRetryableStatus 检查是否可重试的上游状态码。
// 403 也视为可重试：Codex 上游 403 全是账号侧问题（payment_required /
// deactivated_workspace / codex_access_restricted 等 OAuth/套餐/工作区维度），
// 非请求内容问题，换到号池里其他健康账号即可继续（issue #396）。
// 402 同理：deactivated_workspace（team 空间被封）等计费维度拒绝是纯账号侧
// 问题，applyCooldownForModel 已把该账号标错隔离，换号重试即可成功。
func isRetryableStatus(code int) bool {
	return code == http.StatusServiceUnavailable ||
		code == http.StatusUnauthorized ||
		code == http.StatusInternalServerError ||
		code == http.StatusPaymentRequired ||
		code == http.StatusForbidden ||
		code == http.StatusUpgradeRequired
}

func shouldRetryHTTPStatus(statusCode int, body []byte, generalRetries *int, rateLimitRetries *int, maxGeneralRetries, maxRateLimitRetries int) bool {
	if statusCode == http.StatusTooManyRequests {
		if rateLimitRetries == nil || *rateLimitRetries >= maxRateLimitRetries {
			return false
		}
		*rateLimitRetries++
		return true
	}
	// 400 一般是请求内容问题不重试；唯独"账号不支持该模型"是账号权益问题，
	// 该账号已被模型冷却排除，换号重试可成功（issue #408）。
	modelUnsupported := statusCode == http.StatusBadRequest && isCodexModelUnsupportedError(body)
	if !isRetryableStatus(statusCode) && !modelUnsupported {
		return false
	}
	if generalRetries == nil || *generalRetries >= maxGeneralRetries {
		return false
	}
	*generalRetries++
	return true
}

func shouldRetryRequestError(err error, generalRetries *int, maxGeneralRetries int) bool {
	if err == nil || generalRetries == nil || *generalRetries >= maxGeneralRetries {
		return false
	}
	if IsRetryableError(err) || classifyTransportFailure(err) != "" {
		*generalRetries++
		return true
	}
	return false
}

const transportRetryPolicySticky = "sticky"

// waitBeforeRetry 在两次重试之间等待管理端配置的重试间隔(retry_interval_ms,0 = 立即重试)。
// 等待期间客户端断开返回 false,调用方应放弃本次重试(issue #331)。
func (h *Handler) waitBeforeRetry(ctx context.Context) bool {
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if h == nil || h.store == nil {
		return true
	}
	interval := time.Duration(h.store.GetRetryIntervalMS()) * time.Millisecond
	if interval <= 0 {
		return true
	}
	if ctx == nil {
		time.Sleep(interval)
		return true
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// stickyTransportRetryEnabled 返回是否对传输类失败粘滞同号重试(issue #331)。
// 网络波动/代理换节点等连接级故障的根源不在账号:粘滞模式下不换号、不记账号失败、
// 不解绑会话亲和,等重试间隔后同号重试;换号(rotate,默认)保持旧行为。
func (h *Handler) stickyTransportRetryEnabled() bool {
	return h != nil && h.store != nil && h.store.GetTransportRetryPolicy() == transportRetryPolicySticky
}

func IsDeactivatedWorkspaceError(body []byte) bool {
	for _, path := range []string{"detail.code", "error.code", "code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if code == "deactivated_workspace" {
			return true
		}
	}
	return strings.Contains(strings.ToLower(string(body)), "deactivated_workspace")
}

// IsAgentRuntimeDeletedError 判断响应体是否表示 Agent runtime 已删除。
func IsAgentRuntimeDeletedError(body []byte) bool {
	code := strings.ToLower(firstGJSONString(body, "error.code", "detail.code", "code"))
	if code != "biscuit_baker_service_agent_error_status" {
		return false
	}
	message := strings.ToLower(firstGJSONString(body, "error.message", "detail.message", "message"))
	return strings.Contains(message, "agent runtime has been deleted")
}

func upstreamAccountErrorMessage(statusCode int, body []byte) string {
	if IsDeactivatedWorkspaceError(body) {
		return fmt.Sprintf("上游返回 %d: deactivated_workspace", statusCode)
	}
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(body, "detail.message").String())
	}
	// Do not persist arbitrary HTML/plain-text bodies. Structured JSON fields
	// above are bounded and sanitised; an unstructured body becomes status text.
	if len(message) > 300 {
		message = message[:300]
	}
	if message == "" {
		message = http.StatusText(statusCode)
	}
	return fmt.Sprintf("上游返回 %d: %s", statusCode, message)
}

func upstreamErrorKind(statusCode int, body []byte, decision codex429Decision) string {
	if IsUsageLimitReachedError(body) {
		if decision.Reason != "" {
			return decision.Reason
		}
		return "usage_limit"
	}
	switch statusCode {
	case http.StatusTooManyRequests:
		if decision.Reason != "" {
			return decision.Reason
		}
		return "rate_limited"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusPaymentRequired:
		if IsDeactivatedWorkspaceError(body) {
			return "deactivated_workspace"
		}
		if decision.Reason != "" {
			return decision.Reason
		}
		return "payment_required_unknown"
	case http.StatusForbidden:
		if statusCode == http.StatusForbidden && IsAgentRuntimeDeletedError(body) {
			return "agent_runtime_deleted"
		}
		if IsDeactivatedWorkspaceError(body) {
			return "deactivated_workspace"
		}
		return "forbidden"
	case http.StatusUpgradeRequired:
		return "version_required"
	case http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
		return "server"
	default:
		if statusCode >= 400 {
			return "client"
		}
		return ""
	}
}

func parseUsageLimitDetails(body []byte) (usageLimitDetails, bool) {
	if len(body) == 0 {
		return usageLimitDetails{}, false
	}
	if !IsUsageLimitReachedError(body) {
		return usageLimitDetails{}, false
	}
	return usageLimitDetails{
		message:         firstGJSONString(body, "error.message", "response.error.message", "response.status_details.error.message"),
		planType:        firstGJSONString(body, "error.plan_type", "response.error.plan_type", "response.status_details.error.plan_type"),
		resetsAt:        firstGJSONInt(body, "error.resets_at", "response.error.resets_at", "response.status_details.error.resets_at"),
		resetsInSeconds: firstGJSONInt(body, "error.resets_in_seconds", "response.error.resets_in_seconds", "response.status_details.error.resets_in_seconds"),
	}, true
}

// IsUsageLimitReachedError reports whether an upstream error body represents
// account quota exhaustion, even when the transport status is incorrectly 5xx.
func IsUsageLimitReachedError(body []byte) bool {
	return strings.EqualFold(firstGJSONString(body, "error.type", "response.error.type", "response.status_details.error.type"), "usage_limit_reached")
}

func firstGJSONString(body []byte, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func firstGJSONInt(body []byte, paths ...string) int64 {
	for _, path := range paths {
		result := gjson.GetBytes(body, path)
		if result.Exists() {
			return result.Int()
		}
	}
	return 0
}

// Responses 处理 /v1/responses 请求（原生透传，增强输入验证）
func (h *Handler) Responses(c *gin.Context) {
	// 1. 读取请求体
	handlerStart := time.Now()
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	h.capturePromptRequestIngress(c, rawBody)
	bodyReadDone := time.Now()
	compactionMeta := requestCompactionMetaForHTTP(c, rawBody)
	cacheRequestCompactionMeta(c, compactionMeta)

	// Native remote compaction v2：较新的 Codex 客户端把会话压缩触发器作为
	// input item（type=compaction_trigger）嵌进普通 /responses，并带 stream=true。
	// 这条线就是原生 /responses，不要求 /responses/compact 能力，也不把请求
	// 钉死在官方 OAuth 账号上——能打普通 /responses 的中转同样可以接
	// compaction_trigger。旧逻辑在池里还有官方号时 exclude 中转，官方号限流
	// 或模型白名单对不上就会立刻 503（issue #540）。
	// 非流式 body-signal 仍提升到 compact 专用链路，兼容只实现
	// /responses/compact 的中转（issue #361：流式不能走一次性 JSON）。
	bodySignalCompact := compactionMeta.ProtocolTriggered
	nativeRemoteCompactionV2 := bodySignalCompact && gjson.GetBytes(rawBody, "stream").Bool()
	if bodySignalCompact && !nativeRemoteCompactionV2 {
		h.ResponsesCompact(c)
		return
	}

	supportedModels := h.supportedModelIDs(c.Request.Context())
	var requestModel, mappedModel string
	var mappingApplied bool
	if nativeRemoteCompactionV2 {
		rawBody, requestModel, mappedModel, mappingApplied = h.applyConfiguredCompactModelMappingToBody(rawBody, supportedModels)
	} else {
		rawBody, requestModel, mappedModel, mappingApplied = h.applyConfiguredModelMappingToBody(rawBody, supportedModels)
	}
	rawBody, _ = normalizePortableResponsesCompactionHistory(rawBody)
	setRawRequestBody(c, rawBody)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRulesForModel(mappedModel)
	if requestUpstreamChannel(c) != database.UpstreamChannelGrok {
		// grok 渠道 Key 的模型由 Grok 上游校验，跳过网关侧模型白名单
		rules["model"] = append(rules["model"], h.modelValidator(supportedModels))
	}
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	logModel := requestModel
	if logModel == "" {
		logModel = model
	}

	// 验证 model 参数
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}

	if model == "" {
		api.SendMissingFieldError(c, "model")
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/responses", model) {
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	if err := ValidateResponsesFunctionNames(rawBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	turnContinuation := codexTurnContinuationToken(c.Request.Header, rawBody) != ""
	_, turnHasBinding := h.store.SessionAffinityAccountID(affinityKey)
	turnContinuationPinned := turnContinuation && turnHasBinding
	ruleIdentity := h.payloadRuleIdentity(c)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	validateDone := time.Now()

	// 2. 准备 Codex 上游请求体（Unmarshal→map→Marshal，一次序列化）。
	// OpenAI Responses relay body 仅在实际命中 relay 账号时惰性生成，避免 Codex 路径重复转换。
	// previous_response_id 缓存按下游 API Key 隔离，防止跨用户注入他人对话历史。
	respCacheOwner := responseCacheOwner(apiKeyID)
	bodyPreparation := prepareResponsesBodyForOwnerDetailed(rawBody, respCacheOwner)
	codexBody, expandedInputRaw := bodyPreparation.Body, bodyPreparation.ExpandedInputRaw
	continuationStatus, continuationReason, continuationUnavailable := responseCachePreparationFailure(bodyPreparation)
	// strip 策略：剥离网关注入及客户端携带的图片工具能力声明，作为普通文本请求继续（issue #411）。
	codexBody = applyImageGenerationStripPolicy(c, codexBody)
	var openAIResponsesBody []byte
	resetOpenAIResponsesBody := func() {
		openAIResponsesBody = nil
	}
	getOpenAIResponsesBody := func() []byte {
		if openAIResponsesBody == nil {
			openAIResponsesBody = applyImageGenerationStripPolicy(c, PrepareOpenAIResponsesBody(rawBody))
		}
		return openAIResponsesBody
	}
	if err := validateResponsesImageGenerationSizes(codexBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	prepareDone := time.Now()
	effectiveModel := effectiveRequestModel(codexBody, model)
	logEffectiveModel := usageEffectiveModelForMapping(logModel, effectiveModel, mappingApplied)
	if h.enforceAPIKeyLimitsAndReply(c, effectiveModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	allowCodexAccounts := modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db))
	var accountFilter auth.AccountFilter
	if nativeRemoteCompactionV2 {
		accountFilter = accountFilterForCompactResponsesModelWithOriginal(logModel, effectiveModel, allowCodexAccounts)
	} else {
		accountFilter = accountFilterForResponsesModelWithOriginal(logModel, effectiveModel, allowCodexAccounts)
	}
	accountFilter = h.withModelCooldownFilter(effectiveModel, accountFilter)
	if continuationUnavailable {
		accountFilter = relayOnlyAccountFilter(accountFilter)
	}
	accountFilter = h.applyUpstreamChannelFilter(c, effectiveModel, accountFilter)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	// resolveCompactionAffinity 只在已知来源相互冲突时报错；缓存故障按未知
	// 来源处理，保持正常调度。
	compactionAffinity, compactionAffinityErr := h.resolveCompactionAffinity(c.Request.Context(), rawBody)
	if compactionAffinityErr != nil {
		sendCompactionProvenanceConflict(c)
		return
	}
	if compactionAffinity.Known {
		accountFilter = compactionDomainFilter(compactionAffinity.CompatibilityDomain, accountFilter)
	}
	// scope 并发位在选中账号后才能占，请求退出时统一释放（issue #439 v2）。
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	var wsHTTPFallback websocketHTTPFallbackState
	invalidEncryptedContentRetried := false
	relayContinuationAttempted := false
	overflowCompactRetried := false
	overflowCompactEnabled := autoCompactOverflowEnabled(c)

	// 上游 ctx 生命周期：每次 attempt 开始前用新的 drainable ctx 替换，
	// defer 兜底确保函数退出时上游被释放。
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	capacityShedRetries := map[int64]int{}
	dispatchPolicy := dispatchPolicyForModel(effectiveModel)
	for attempt := 0; ; attempt++ {
		account, stickyProxyURL, retainedHTTPFallback := wsHTTPFallback.Take()
		if !retainedHTTPFallback {
			if attempt == 0 && compactionAffinity.Known && !turnContinuationPinned {
				account = h.store.TakePreferredAccountWithDispatch(compactionAffinity.PreferredAccountID, apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy)
			}
			if account != nil {
				stickyProxyURL = account.GetProxyURL()
			} else if continuationUnavailable && !relayContinuationAttempted {
				account, stickyProxyURL = h.nextAccountForSessionWithDispatch(affinityKey, apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy)
			} else if turnContinuationPinned {
				account, stickyProxyURL = h.nextRetryAccountForContinuationWithDispatch(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, dispatchPolicy)
			} else {
				account, stickyProxyURL = h.nextRetryAccountForSessionWithDispatch(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, dispatchPolicy)
			}
		}
		if account == nil {
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			if compactionAffinity.Known {
				sendCompactionUpstreamUnavailable(c)
				return
			}
			// 候选被 scope 预算剔空时给出真实原因，而不是含糊的「无可用账号」。
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			if h.store.HasUsageLimitedCandidateWithDispatch(apiKeyID, retryExclusions.ForSelection(), accountFilter, dispatchPolicy) {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, "Codex 账号用量窗口已达上限")
				return
			}
			if continuationUnavailable && !relayContinuationAttempted {
				sendResponseContextUnavailable(c, continuationStatus, continuationReason)
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(effectiveModel))
			return
		}

		if attempt == 0 {
			emitResponsesPhaseTimings(c, logModel, len(rawBody), handlerStart, bodyReadDone, validateDone, prepareDone)
		}
		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !retainedHTTPFallback {
			h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		}
		if wsHTTPFallback.ForceHTTP() {
			log.Printf("上游 WebSocket 1009 后启动 HTTP 降级尝试 (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/responses, ws_elapsed_ms=%d)", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsHTTPFallback.WSElapsed().Milliseconds())
		}
		attemptEffectiveModel := effectiveModel
		attemptLogEffectiveModel := logEffectiveModel
		// relay/Grok 账号走 HTTP 执行器（下方 IsRelayStyle 分支优先于 WS），这里同步排除，
		// 避免日志把 relay 请求错标成 via_websocket。
		useWebsocket := h.shouldUseWebsocketForHTTP() && !wsHTTPFallback.ForceHTTP() && !account.IsRelayStyle()
		// 生图请求强制走 HTTP：WebSocket 传输大体积图片数据会卡死（issue #220）；
		// 自然语言生图意图也需保留 image_generation 工具（issue #288）。
		if useWebsocket && rawResponsesBodyShouldForceHTTPForImageGeneration(rawBody) {
			useWebsocket = false
		}
		// 体积达到已学习的 1009 阈值时直接首发 HTTP,跳过 WS 必败等待(issue #404)。
		if useWebsocket && globalWSSizeRouter.PreferHTTP(len(codexBody)) {
			useWebsocket = false
			if attempt == 0 {
				log.Printf("[WS] 请求体 %dKB 达到已学习的 1009 体积阈值，直接走 HTTP 上游 (endpoint=/v1/responses)", len(codexBody)/1024)
			}
		}

		// 提取 API Key 用于设备指纹稳定化
		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)

		// 使用注入的设备指纹配置
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{
				StabilizeDeviceProfile: false, // 默认关闭
			}
		}

		// 透传下游请求头用于指纹学习
		downstreamHeaders := c.Request.Header.Clone()

		if account.IsRelayStyle() {
			relayContinuationAttempted = true
			if lastUpstreamCancel != nil {
				lastUpstreamCancel()
			}
			upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
			lastUpstreamCancel = upstreamCancel
			ttftGuard := (*firstTokenTimeoutGuard)(nil)
			if isStream {
				ttftGuard = newFirstTokenTimeoutGuard(firstTokenTimeoutForRequest(currentFirstTokenTimeout(), bodySignalCompact), upstreamCancel)
			}
			stopTTFTGuard := func() {
				if ttftGuard != nil {
					ttftGuard.Stop()
				}
			}
			ttftTimedOut := func() bool {
				return ttftGuard != nil && ttftGuard.TimedOut()
			}
			upstreamEndpoint := relayUpstreamEndpointForProtocol(account, GrokProtocolResponses, attemptEffectiveModel)
			upstreamBody := getOpenAIResponsesBody()
			var mappedBody []byte
			var mappedModel string
			var accountMappingApplied bool
			if nativeRemoteCompactionV2 {
				mappedBody, mappedModel, accountMappingApplied = h.applyAccountCompactModelMappingToBody(upstreamBody, account, logModel, effectiveModel)
			} else {
				mappedBody, mappedModel, accountMappingApplied = h.applyAccountModelMappingToBodyForModels(upstreamBody, account, logModel, effectiveModel)
			}
			if accountMappingApplied {
				upstreamBody = mappedBody
				attemptEffectiveModel = mappedModel
				attemptLogEffectiveModel = usageEffectiveModelForMapping(logModel, attemptEffectiveModel, true)
			}
			resp, reqErr := ExecuteRelayStyleProtocolRequest(upstreamCtx, account, GrokProtocolResponses, rawBody, upstreamBody, proxyURL, downstreamHeaders)
			durationMs := int(time.Since(start).Milliseconds())

			if reqErr != nil {
				timedOut := ttftTimedOut()
				stopTTFTGuard()
				if timedOut {
					reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
				}
				kind := classifyTransportFailure(reqErr)
				if wsHTTPFallback.ForceHTTP() {
					wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, logStatusUpstreamStreamBreak)
				}
				retryable := IsRetryableError(reqErr) || kind != ""
				shouldRetry := false
				if retryable {
					shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
				}
				// 传输类失败粘滞同号重试:不记账号失败、不解绑亲和、不硬排除(issue #331)
				// busy acquire 超时不粘滞同号：同 key 再等只会重复排队，直接换号（issue #413）
				stickyRetry := shouldRetry && !timedOut && kind != "" && kind != upstreamErrorKindWsBusyAcquire && h.stickyTransportRetryEnabled()
				if shouldPenalizeTransportKind(kind) && !(timedOut && shouldRetry) && !stickyRetry {
					h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
				}
				h.store.Release(account)
				if !stickyRetry {
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
				}
				if timedOut && shouldRetry {
					retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
					log.Printf("OpenAI Responses 上游首字超时，断开并重试 (attempt %d/%d, account %d): %v", attempt+1, maxRetries+1, account.ID(), reqErr)
					continue
				}
				if !timedOut && !stickyRetry {
					retryExclusions.MarkHard(account.ID())
				}

				if !retryable {
					ErrorToGinResponse(c, reqErr)
					return
				}

				log.Printf("OpenAI Responses 上游请求失败 (attempt %d): %v", attempt+1, reqErr)
				if shouldRetry {
					if stickyRetry {
						log.Printf("传输错误粘滞重试：保留账号 %d 与会话亲和 (attempt %d/%d)", account.ID(), attempt+1, maxRetries+1)
					}
					if !h.waitBeforeRetry(c.Request.Context()) {
						return
					}
					continue
				}
				ErrorToGinResponse(c, reqErr)
				return
			}
			if !isStream {
				stopTTFTGuard()
			}

			if resp.StatusCode != http.StatusOK {
				stopTTFTGuard()
				if wsHTTPFallback.ForceHTTP() {
					wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, resp.StatusCode)
				}
				errBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				if !invalidEncryptedContentRetried && isInvalidEncryptedContentError(resp.StatusCode, errBody) {
					strippedRawBody, rawChanged := stripInvalidEncryptedContentFromResponsesBody(rawBody)
					strippedCodexBody, codexChanged := stripInvalidEncryptedContentFromResponsesBody(codexBody)
					if rawChanged || codexChanged {
						invalidEncryptedContentRetried = true
						if rawChanged {
							rawBody = strippedRawBody
							resetOpenAIResponsesBody()
						}
						if codexChanged {
							codexBody = strippedCodexBody
							expandedInputRaw = responsesInputRaw(codexBody)
						}
						log.Printf("OpenAI Responses 上游拒绝 encrypted_content，已移除加密 reasoning 上下文并重试一次 (attempt %d)", attempt+1)
						h.store.Release(account)
						h.store.UnbindSessionAffinity(affinityKey, account.ID())
						continue
					}
				}

				if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
					h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
				}
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				retryExclusions.MarkHard(account.ID())

				log.Printf("OpenAI Responses 上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
				logUpstreamError("/v1/responses", resp.StatusCode, logModel, account.ID(), errBody)
				promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, errBody, upstreamCyberPolicyAttempt{
					Transport: upstreamPromptPolicyTransport(isStream, useWebsocket), StatusCode: resp.StatusCode,
					AccountID: account.ID(), AttemptIndex: attempt + 1,
				}))
				decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, attemptEffectiveModel)
				shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries))
				usageTiers := resolveUsageServiceTiers("", serviceTier)
				h.logUsageForRequest(c, &database.UsageLogInput{
					AccountID:              account.ID(),
					Endpoint:               "/v1/responses",
					Model:                  logModel,
					EffectiveModel:         attemptLogEffectiveModel,
					StatusCode:             resp.StatusCode,
					DurationMs:             durationMs,
					ReasoningEffort:        reasoningEffort,
					InboundEndpoint:        "/v1/responses",
					UpstreamEndpoint:       upstreamEndpoint,
					Stream:                 isStream,
					ViaWebsocket:           useWebsocket,
					ServiceTier:            usageTiers.ServiceTier,
					RequestedServiceTier:   usageTiers.RequestedServiceTier,
					ActualServiceTier:      usageTiers.ActualServiceTier,
					BillingServiceTier:     usageTiers.BillingServiceTier,
					IsRetryAttempt:         shouldRetry,
					AttemptIndex:           attempt + 1,
					UpstreamErrorKind:      upstreamErrorKind(resp.StatusCode, errBody, decision),
					ErrorMessage:           usageLogErrorMessage(resp.StatusCode, errBody),
					PromptPolicyIncidentID: promptPolicyIncidentID,
				})

				if shouldRetry {
					lastStatusCode = resp.StatusCode
					lastBody = errBody
					if !h.waitBeforeRetry(c.Request.Context()) {
						return
					}
					continue
				}

				h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
				return
			}
			relayCodexTurnStateResponseHeader(c, affinityKey, account, resp.Header)
			if isGrokNativeRouteResponse(resp) {
				usage, outcome, wroteAnyBody, firstTokenMs := forwardGrokNativeResponse(c, resp, GrokProtocolResponses, isStream, start, stopTTFTGuard)
				totalDuration := int(time.Since(start).Milliseconds())
				stopTTFTGuard()
				resp.Body.Close()
				if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), nil) {
					h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					retryExclusions.MarkHard(account.ID())
					continue
				}
				if !wroteAnyBody && outcome.logStatusCode != http.StatusOK && c.Request.Context().Err() == nil {
					h.sendGrokNativeHTTPError(c, GrokProtocolResponses, outcome)
				}
				logInput := &database.UsageLogInput{
					AccountID: account.ID(), Endpoint: "/v1/responses", Model: logModel,
					EffectiveModel: attemptLogEffectiveModel, StatusCode: outcome.logStatusCode,
					DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
					InboundEndpoint: "/v1/responses", UpstreamEndpoint: upstreamEndpoint,
					Stream: isStream, ViaWebsocket: false, AttemptIndex: attempt + 1,
				}
				if usage != nil {
					logInput.PromptTokens, logInput.CompletionTokens, logInput.TotalTokens = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
					logInput.InputTokens, logInput.OutputTokens = usage.InputTokens, usage.OutputTokens
					logInput.ReasoningTokens, logInput.CachedTokens = usage.ReasoningTokens, usage.CachedTokens
				}
				if outcome.logStatusCode != http.StatusOK {
					logInput.UpstreamErrorKind = outcome.failureKind
					logInput.ErrorMessage = usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage)
				}
				h.logUsageForRequest(c, logInput)
				if outcome.penalize {
					h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
				} else if outcome.logStatusCode == http.StatusOK {
					h.store.ClearModelCooldown(account, attemptEffectiveModel)
					h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
				}
				if outcome.logStatusCode == http.StatusOK {
					h.store.ReleaseForSession(account, affinityKey)
				} else {
					h.store.Release(account)
				}
				return
			}

			account.Mu().RLock()
			relayAccountEmail := account.Email
			account.Mu().RUnlock()
			c.Set("x-account-email", relayAccountEmail)
			c.Set("x-account-proxy", proxyURL)
			c.Set("x-model", logModel)
			c.Set("x-reasoning-effort", reasoningEffort)

			var firstTokenMs int
			var usage *UsageInfo
			var actualServiceTier string
			ttftRecorded := false
			gotTerminal := false
			deltaCharCount := 0
			var readErr error
			var writeErr error
			wroteAnyBody := false
			// 断流现场判据(issue #491):区分下游背压与上游重置。
			streamDiag := newStreamPhaseDiagnostics()
			// 首 token 前收到不可重试的 response.failed 时置位:中止 SSE 转发、
			// 不做 transport flush(避免提前提交 200 header),循环外按真实错误码返回 JSON。
			abortedForHTTPError := false
			var imageLogInfo imageUsageLogInfo
			var terminalFailurePayload []byte
			promptPolicyIncidentID := ""
			upstreamCyberPolicyLogged := false

			if isStream {
				c.Header("Content-Type", "text/event-stream")
				c.Header("Cache-Control", "no-cache")
				c.Header("Connection", "keep-alive")
				c.Header("X-Accel-Buffering", "no")

				flusher, ok := c.Writer.(http.Flusher)
				if !ok {
					ttftGuard.Stop()
					c.JSON(http.StatusInternalServerError, gin.H{
						"error": gin.H{"message": "streaming not supported", "type": "server_error"},
					})
					resp.Body.Close()
					h.store.Release(account)
					return
				}
				streamWriter := h.newStreamFlushWriter(c, c.Writer, flusher)
				streamWriter.diag = streamDiag
				clientGone := false
				var pendingFirstTokenEvents bytes.Buffer
				readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
					streamDiag.markUpstreamFrame()
					h.recordCompactionProvenanceFromPayload(context.Background(), account, data)
					parsed := gjson.ParseBytes(data)
					eventType := parsed.Get("type").String()
					ttftGuard.MarkProgress(eventType)
					isFirstToken := isFirstTokenResultForMode(parsed, currentFirstTokenMode())
					if !ttftRecorded && isFirstToken {
						firstTokenMs = int(time.Since(start).Milliseconds())
						ttftRecorded = true
					}
					if eventType == "response.output_text.delta" {
						deltaCharCount += len(parsed.Get("delta").String())
					}
					if isResponsesSuccessTerminalEvent(eventType) {
						usage = extractUsageFromResult(parsed.Get("response.usage"))
						if tier := parsed.Get("response.service_tier").String(); tier != "" {
							actualServiceTier = tier
						}
						gotTerminal = true
					}
					if eventType == "response.failed" {
						var incidentID string
						var logged bool
						data, incidentID, logged = h.attachUpstreamCyberPolicyStreamDecision(c, "/v1/responses", logModel, data, upstreamCyberPolicyAttempt{
							Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: classifyResponseFailedOutcome(data).logStatusCode,
							AccountID: account.ID(), AttemptIndex: attempt + 1,
						})
						if logged {
							upstreamCyberPolicyLogged = true
							promptPolicyIncidentID = incidentID
						}
						terminalFailurePayload = append([]byte(nil), data...)
						gotTerminal = true
					}
					if !clientGone && shouldSuppressRetryableResponseFailedBeforeFirstToken(eventType, terminalFailurePayload, ttftRecorded, wroteAnyBody, attempt, maxRetries, c.Request.Context().Err(), writeErr) {
						pendingFirstTokenEvents.Reset()
						return false
					}
					// 首 token 前的 response.failed 不写进下游流:不可重试(如 context_length_exceeded)
					// 或已达重试上限时,交由循环外按真实错误码返回,而不是 200 流让中转层误计费。
					if shouldReturnHTTPErrorForResponseFailed(eventType, ttftRecorded, wroteAnyBody, clientGone) {
						pendingFirstTokenEvents.Reset()
						abortedForHTTPError = true
						return false
					}
					if image, ok := extractImageFromOutputItemDone(data, logModel); ok {
						imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
					}
					if !clientGone {
						// 可重试的 error 帧（上游降载先导帧）与生命周期帧一样缓冲：
						// 立即写出会置位 wroteAnyBody，随后的 response.failed 就进不了
						// 首包前静默换号分支。必须写出时改写降载码为客户端可重试码。
						shouldDefer := !ttftRecorded && !gotTerminal &&
							(isPreContentLifecycleEvent(eventType) || isRetryableUpstreamErrorFrame(eventType, data))
						wrote, err := writeDeferredSSEData(streamWriter, &pendingFirstTokenEvents, sanitizeCapacityShedEventForClient(eventType, data), shouldDefer)
						if err != nil {
							writeErr = err
							clientGone = true
						} else if wrote {
							wroteAnyBody = true
						}
					}
					return !isResponsesTerminalEvent(eventType)
				})
				// 仅在真的写过 body 时才做收尾 flush:flusher.Flush 会先提交 HTTP 200 header,
				// 零写入时提前 flush 会让循环外的 c.JSON(4xx) 失效(status 已定型为 200)。
				if writeErr == nil && wroteAnyBody {
					writeErr = streamWriter.Flush()
				}
				// 已写正文后的上游断流：合成 response.failed 终态（code=
				// upstream_stream_break），避免下游收到静默 EOF 的"假 200"(issue #473)。
				if shouldWriteStreamBreakEvent(gotTerminal, wroteAnyBody, c.Request.Context().Err(), writeErr) {
					if err := writeResponsesStreamBreakEvent(streamWriter); err != nil {
						log.Printf("写入合成 response.failed 断流事件失败 (OpenAI Responses relay): %v", err)
					}
				}
			} else {
				var respBody []byte
				respBody, readErr = io.ReadAll(resp.Body)
				if readErr == nil {
					h.recordCompactionProvenanceFromPayload(context.Background(), account, respBody)
					usage = extractUsageFromResult(gjson.GetBytes(respBody, "usage"))
					actualServiceTier = gjson.GetBytes(respBody, "service_tier").String()
					imageLogInfo = imageUsageLogInfoFromResponseJSON(respBody)
					gotTerminal = true
					contentType := resp.Header.Get("Content-Type")
					if contentType == "" {
						contentType = "application/json"
					}
					c.Data(http.StatusOK, contentType, respBody)
				}
			}

			totalDuration := int(time.Since(start).Milliseconds())
			outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
			if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
				outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
			}
			outcome = annotateStreamBreakDiagnostics(outcome, streamDiag)
			ttftGuard.Stop()
			if outcome.verifyAccountAuth {
				h.store.VerifyAccountAuthAsync(account)
			}
			var responseFailedDecision codex429Decision
			if len(terminalFailurePayload) > 0 {
				outcome = classifyResponseFailedOutcome(terminalFailurePayload)
				responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, attemptEffectiveModel)
				if responseFailedDecision.Reason != "" {
					outcome.failureKind = upstreamErrorKind(outcome.logStatusCode, responseFailedErrorBody(terminalFailurePayload), responseFailedDecision)
				}
				// 流式 response.failed（HTTP 200）里的 cyber_policy 处罚也要记录，
				// 否则只有非 2xx 错误体才会被记入提示词过滤日志。
				if !upstreamCyberPolicyLogged {
					promptPolicyIncidentID = acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, responseFailedErrorBody(terminalFailurePayload), upstreamCyberPolicyAttempt{
						Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: outcome.logStatusCode,
						AccountID: account.ID(), AttemptIndex: attempt + 1,
					}))
				}
				if isExplicitUpstreamCyberPolicy(terminalFailurePayload) {
					outcome.failureMessage = upstreamCyberPolicyResponseMessage(c)
				}
			}
			if wsHTTPFallback.ForceHTTP() {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, totalDuration, firstTokenMs, outcome.logStatusCode)
			}
			if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
				h.logPromptPolicyRetryUsage(c, database.UsageLogInput{
					AccountID: account.ID(), Endpoint: "/v1/responses", Model: logModel, EffectiveModel: attemptLogEffectiveModel,
					StatusCode: outcome.logStatusCode, DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
					InboundEndpoint: "/v1/responses", UpstreamEndpoint: upstreamEndpoint, Stream: isStream, ViaWebsocket: useWebsocket,
					AttemptIndex: attempt + 1, UpstreamErrorKind: outcome.failureKind,
					ErrorMessage: usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
				}, promptPolicyIncidentID)
				log.Printf("OpenAI Responses 上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
				recyclePooledClient(account, proxyURL)
				if isFirstTokenTimeoutOutcome(outcome) {
					retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				} else {
					h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
				}
				resp.Body.Close()
				h.store.Release(account)
				h.unbindOrRetainAffinityForCapacityShed(retryExclusions, affinityKey, account, outcome, capacityShedRetries)
				// 首字超时已白等一轮,不再叠加重试间隔;其余首包前断流按配置间隔等待
				if !isFirstTokenTimeoutOutcome(outcome) && !h.waitBeforeRetry(c.Request.Context()) {
					return
				}
				continue
			}
			if isStream && abortedForHTTPError && !wroteAnyBody {
				// 流式:首 token 前上游失败、未向下游写过任何内容,HTTP 200 header 尚未提交,
				// 覆盖预设的 SSE Content-Type 后按真实错误码返回 JSON,
				// 避免下游中转/计费方把它当成功并按预估 input token 计费(与回调内 reset 呼应)。
				c.Header("Content-Type", "application/json; charset=utf-8")
				c.JSON(outcome.logStatusCode, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
				})
			} else if isStream && !wroteAnyBody && outcome.logStatusCode == logStatusUpstreamStreamBreak &&
				c.Request.Context().Err() == nil && writeErr == nil {
				// 首包前断流/首字超时且重试耗尽：原先没有任何写出分支命中，下游
				// 收到空 body 的"假 200"，失败完全不可感知 (issue #473)。598 是内部
				// 日志状态，对外按真实 502 + 稳定错误码返回，下游可编程识别并重试。
				c.Header("Content-Type", "application/json; charset=utf-8")
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": ErrorTypeUpstreamError, "code": ErrorCodeUpstreamStreamBreak},
				})
			}
			if !isStream && readErr != nil {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "读取 OpenAI Responses 响应失败", "type": "upstream_error"},
				})
			}
			if outcome.logStatusCode != http.StatusOK {
				log.Printf("OpenAI Responses 流异常结束 (account %d, status %d): %s，已转发约 %d 字符", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
				if deltaCharCount > 0 {
					estOutputTokens := deltaCharCount / 3
					if estOutputTokens < 1 {
						estOutputTokens = 1
					}
					usage = &UsageInfo{
						OutputTokens:     estOutputTokens,
						CompletionTokens: estOutputTokens,
						TotalTokens:      estOutputTokens,
					}
				}
			}

			usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)
			c.Set("x-service-tier", usageTiers.ServiceTier)
			logInput := &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/responses",
				Model:                  logModel,
				EffectiveModel:         attemptLogEffectiveModel,
				StatusCode:             outcome.logStatusCode,
				DurationMs:             totalDuration,
				FirstTokenMs:           firstTokenMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/responses",
				UpstreamEndpoint:       upstreamEndpoint,
				Stream:                 isStream,
				ViaWebsocket:           useWebsocket,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				PromptPolicyIncidentID: promptPolicyIncidentID,
				AttemptIndex:           attempt + 1,
			}
			if outcome.logStatusCode != http.StatusOK {
				logInput.ErrorMessage = usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage)
				logInput.UpstreamErrorKind = outcome.failureKind
			}
			if usage != nil {
				logInput.PromptTokens = usage.PromptTokens
				logInput.CompletionTokens = usage.CompletionTokens
				logInput.TotalTokens = usage.TotalTokens
				logInput.InputTokens = usage.InputTokens
				logInput.OutputTokens = usage.OutputTokens
				logInput.ReasoningTokens = usage.ReasoningTokens
				logInput.CachedTokens = usage.CachedTokens
			}
			applyImageUsageLogInfo(logInput, imageLogInfo)
			h.logUsageForRequest(c, logInput)

			resp.Body.Close()
			if outcome.penalize {
				recyclePooledClient(account, proxyURL)
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			} else if outcome.logStatusCode == http.StatusOK {
				h.store.ClearModelCooldown(account, attemptEffectiveModel)
				h.store.ConfirmResponsesAvailableSince(account, start)
				h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
			}
			if outcome.logStatusCode == http.StatusOK {
				h.store.ReleaseForSession(account, affinityKey)
			} else {
				h.store.Release(account)
			}
			return
		}

		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, useWebsocket)
		// 上游使用与客户端解耦的 context：客户端中途断开时仍能继续读完
		// response.completed 拿到 usage（流式计费的关键）。
		// lastUpstreamCancel 在 attempt loop 顶部声明 + defer 兜底，
		// 这里覆盖前先 cancel 上一轮（重试时）。
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		// 身份按 attempt 附加实际选中账号维度：account_* 门随重试换号重新匹配（issue #410）。
		attemptIdentity := ruleIdentity.WithSelectedAccount(account, h.store)
		upstreamCtx = WithPayloadRuleIdentity(upstreamCtx, attemptIdentity)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(firstTokenTimeoutForRequest(currentFirstTokenTimeout(), bodySignalCompact), upstreamCancel)
		// WebSocket 上游下剥离自动注入的图片工具，防止模型自主生图产生大体积
		// 数据卡死 WS 流（issue #220）。显式生图请求已在上面强制走 HTTP。
		upstreamBody := codexBody
		if useWebsocket {
			upstreamBody = stripResponsesImageGenerationTool(codexBody)
		}
		// service_tier 记账按 payload 规则改写后的值归因（覆写 service_tier 的规则才生效）。
		// 按尝试重算：不同尝试的生效模型/账号可能不同，规则按模型或账号门匹配则结果随之变化。
		serviceTier = EffectiveRequestedServiceTier(upstreamBody, attemptEffectiveModel, downstreamHeaders, attemptIdentity)
		// 换号后剥离旧账号铸造的 turn-state 回带,防止跨账号矛盾信号打到上游。
		guardCodexTurnStateEcho(affinityKey, account, downstreamHeaders)
		resp, reqErr := ExecuteRequest(upstreamCtx, account, upstreamBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			timedOut := ttftGuard.TimedOut()
			ttftGuard.Stop()
			if timedOut {
				reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
			}
			kind := classifyTransportFailure(reqErr)
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, logStatusUpstreamStreamBreak)
			}
			if useWebsocket && kind == upstreamErrorKindMessageTooBig {
				wsElapsed := time.Since(start)
				globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
				wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(reqErr.Error()))
				log.Printf("上游 WebSocket 1009，保留账号租约并降级 HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/responses, ws_elapsed_ms=%d): %v", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), reqErr)
				continue
			}
			retryable := IsRetryableError(reqErr) || kind != ""
			shouldRetry := false
			if retryable {
				shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
			}
			// 传输类失败粘滞同号重试:不记账号失败、不解绑亲和、不硬排除(issue #331)
			// busy acquire 超时不粘滞同号：同 key 再等只会重复排队，直接换号（issue #413）
			stickyRetry := shouldRetry && !timedOut && kind != "" && kind != upstreamErrorKindWsBusyAcquire && h.stickyTransportRetryEnabled()
			if shouldPenalizeTransportKind(kind) && !(timedOut && shouldRetry) && !stickyRetry {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if !stickyRetry {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if timedOut && shouldRetry {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("上游首字超时，断开并重试 (attempt %d/%d, account %d, /v1/responses): %v", attempt+1, maxRetries+1, account.ID(), reqErr)
				continue
			}
			if !timedOut && !stickyRetry {
				retryExclusions.MarkHard(account.ID())
			}

			// 不可重试的结构化错误直接返回
			if !retryable {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			if shouldRetry {
				if stickyRetry {
					log.Printf("传输错误粘滞重试：保留账号 %d 与会话亲和 (attempt %d/%d, /v1/responses)", account.ID(), attempt+1, maxRetries+1)
				}
				if !h.waitBeforeRetry(c.Request.Context()) {
					return
				}
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, durationMs, 0, resp.StatusCode)
			}
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			accountReleasedForOverflow := false

			if !invalidEncryptedContentRetried && isInvalidEncryptedContentError(resp.StatusCode, errBody) {
				strippedRawBody, rawChanged := stripInvalidEncryptedContentFromResponsesBody(rawBody)
				strippedCodexBody, codexChanged := stripInvalidEncryptedContentFromResponsesBody(codexBody)
				if rawChanged || codexChanged {
					invalidEncryptedContentRetried = true
					if rawChanged {
						rawBody = strippedRawBody
						resetOpenAIResponsesBody()
					}
					if codexChanged {
						codexBody = strippedCodexBody
						expandedInputRaw = responsesInputRaw(codexBody)
					}
					log.Printf("上游拒绝 encrypted_content，已移除加密 reasoning 上下文并重试一次 (attempt %d)", attempt+1)
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			// 上下文超窗 + Key 开启自动压缩：摘要旧轮次后同参重试一次 (issue #415)
			if overflowCompactEnabled && !overflowCompactRetried &&
				resp.StatusCode == http.StatusBadRequest && isContextLengthExceededBody(errBody) {
				// 摘要请求需要沿用同一 Key 的路由/预算，但不能与父请求同时占住
				// 当前账号或 scope 并发位，否则单账号池会发生自锁。
				h.ReleaseAPIKeyScopeConcurrency(c)
				h.store.Release(account)
				accountReleasedForOverflow = true
				if compacted, ok := h.compactOverflowResponsesBodyForRequest(c, codexBody); ok {
					overflowCompactRetried = true
					codexBody = compacted
					expandedInputRaw = responsesInputRaw(codexBody)
					log.Printf("上游报上下文超窗，已压缩旧轮次并重试一次 (attempt %d)", attempt+1)
					continue
				}
			}

			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			if !accountReleasedForOverflow {
				h.store.Release(account)
			}
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHard(account.ID())

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: upstreamPromptPolicyTransport(isStream, useWebsocket), StatusCode: resp.StatusCode,
				AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries))
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/responses",
				Model:                  logModel,
				EffectiveModel:         logEffectiveModel,
				StatusCode:             resp.StatusCode,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/responses",
				UpstreamEndpoint:       "/v1/responses",
				Stream:                 isStream,
				ViaWebsocket:           useWebsocket,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				IsRetryAttempt:         shouldRetry,
				AttemptIndex:           attempt + 1,
				UpstreamErrorKind:      upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:           usageLogErrorMessage(resp.StatusCode, errBody),
				PromptPolicyIncidentID: promptPolicyIncidentID,
			})

			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				if !h.waitBeforeRetry(c.Request.Context()) {
					return
				}
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		relayCodexTurnStateResponseHeader(c, affinityKey, account, resp.Header)
		SyncCodexUsageState(h.store, account, resp)
		// 成功！透传响应并跟踪 TTFT / usage
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", logModel)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var usage *UsageInfo
		var actualServiceTier string
		ttftRecorded := false
		gotTerminal := false // 是否收到 response.completed 或 response.failed
		deltaCharCount := 0  // 累计 delta 字符数（用于断流时估算 token）
		var readErr error
		var writeErr error
		wroteAnyBody := false
		// 首 token 前收到不可重试的 response.failed 时置位:中止 SSE 转发、
		// 不做 transport flush(避免提前提交 200 header),循环外按真实错误码返回 JSON。
		abortedForHTTPError := false
		// contentTokenSeen: 是否已出现真正的内容事件（严格判定，与 first_token_mode 无关）。
		// loose 模式下 codex.rate_limits 等前置事件会置位 ttftRecorded，"首 token 前"
		// 的失败抑制/真实错误码/事件缓冲决策改用本标志，避免在 loose 部署上失效。
		contentTokenSeen := false
		var responseJSON []byte
		var imageLogInfo imageUsageLogInfo
		var terminalFailurePayload []byte
		var streamedOutputItems []json.RawMessage
		promptPolicyIncidentID := ""
		upstreamCyberPolicyLogged := false
		// 断流现场判据(issue #491):区分下游背压拖停上游读取 vs 上游自己重置。
		streamDiag := newStreamPhaseDiagnostics()

		if isStream {
			// 流式透传 + TTFT 跟踪
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				ttftGuard.Stop()
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			streamWriter := h.newStreamFlushWriter(c, c.Writer, flusher)
			streamWriter.diag = streamDiag

			// clientGone：客户端写失败后置位，后续事件不再写客户端，
			// 但继续读上游直到 response.completed/failed，以拿到准确 usage。
			clientGone := false
			// downstreamMu 串行化下游写路径与其共享状态(clientGone/writeErr/
			// wroteAnyBody/streamWriter):自动续想的保活 goroutine(issue #458)
			// 与 forward 并发写同一个 ResponseWriter,必须互斥。续想关闭时无
			// 并发方,锁零竞争。
			var downstreamMu sync.Mutex
			var pendingFirstTokenEvents bytes.Buffer
			contEnabled, contMaxRounds := codexContinueThinkingSettings()
			// 前置元数据事件立即透传（旧版兼容，issue #425）：每个 attempt 取一次快照，
			// 热更新对新请求生效，流转发中途不切换缓冲策略。
			preflightPassthrough := CurrentRuntimeSettings().CodexPreflightSSEPassthrough
			forward := func(data []byte) bool {
				streamDiag.markUpstreamFrame()
				h.recordCompactionProvenanceFromPayload(context.Background(), account, data)
				downstreamMu.Lock()
				defer downstreamMu.Unlock()
				// 上游 context 为了提取 usage 会在客户端断开后再排空最多 5 秒；
				// 但下游 context 一旦取消，绝不能再尝试写 SSE，否则下一帧必然
				// 变成 broken pipe。继续解析帧只用于拿 response.completed/usage。
				if c.Request.Context().Err() != nil {
					clientGone = true
				}
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()

				// TTFT: 记录第一个实际内容事件的时间
				ttftGuard.MarkProgress(eventType)
				isFirstToken := isFirstTokenResultForMode(parsed, currentFirstTokenMode())
				if !ttftRecorded && isFirstToken {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// contentTokenSeen 用严格判定（与 first_token_mode 无关）：loose 模式下
				// codex.rate_limits 等前置事件也会置位 ttftRecorded，若用它做"首 token 前"
				// 判断，失败抑制/真实错误码/超窗压缩重试在 loose 部署上全部失效。
				if !contentTokenSeen && isFirstTokenResult(parsed) {
					contentTokenSeen = true
				}

				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if image, ok := extractImageFromOutputItemDone(data, logModel); ok {
					imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
				}
				if eventType == "response.output_item.done" {
					item := parsed.Get("item")
					if isCodexToolCallContextType(item.Get("type").String()) {
						streamedOutputItems = append(streamedOutputItems, json.RawMessage(item.Raw))
					}
				}

				// 提取 usage + service_tier
				if isResponsesSuccessTerminalEvent(eventType) {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					if eventType == "response.completed" {
						// 缓存响应上下文，供后续 previous_response_id 展开使用。
						// 截断态不入缓存：它不是完整回合，展开后会把半截输出当历史。
						cacheCompletedResponseWithOutputItems(respCacheOwner, []byte(expandedInputRaw), data, streamedOutputItems)
					}
					gotTerminal = true
				}
				if eventType == "response.failed" {
					var incidentID string
					var logged bool
					data, incidentID, logged = h.attachUpstreamCyberPolicyStreamDecision(c, "/v1/responses", logModel, data, upstreamCyberPolicyAttempt{
						Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: classifyResponseFailedOutcome(data).logStatusCode,
						AccountID: account.ID(), AttemptIndex: attempt + 1,
					})
					if logged {
						upstreamCyberPolicyLogged = true
						promptPolicyIncidentID = incidentID
					}
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
				}

				if !clientGone && shouldSuppressRetryableResponseFailedBeforeFirstToken(eventType, terminalFailurePayload, contentTokenSeen, wroteAnyBody, attempt, maxRetries, c.Request.Context().Err(), writeErr) {
					pendingFirstTokenEvents.Reset()
					return false
				}

				// 首 token 前的 response.failed 不写进下游流:不可重试(如 context_length_exceeded)
				// 或已达重试上限时,交由循环外按真实错误码返回,而不是 200 + [DONE] 让中转层误计费。
				if shouldReturnHTTPErrorForResponseFailed(eventType, contentTokenSeen, wroteAnyBody, clientGone) {
					pendingFirstTokenEvents.Reset()
					abortedForHTTPError = true
					return false
				}

				if !clientGone {
					// codex.* 前置元数据事件（rate_limits / response.metadata）与生命周期
					// 事件一样延迟到首 token 一起冲刷：立即写出会提交 200 header 并置位
					// wroteAnyBody，使首 token 前的 response.failed（如 context_length_exceeded）
					// 既无法按真实错误码返回，也无法走超窗压缩重试。
					// preflightPassthrough（issue #425）恢复旧版语义：元数据事件立即下发，
					// 管理员显式接受上述代价；生命周期事件（created/in_progress）不受开关影响。
					// 可重试的 error 帧（上游降载先导帧）不受 preflightPassthrough 影响，
					// 始终缓冲：立即写出会置位 wroteAnyBody，随后的 response.failed 就
					// 进不了首包前静默换号/超窗压缩分支。必须写出时改写降载码。
					shouldDefer := shouldDeferPreContentSSEEvent(eventType, contentTokenSeen, gotTerminal, preflightPassthrough) ||
						(!contentTokenSeen && !gotTerminal && isRetryableUpstreamErrorFrame(eventType, data))
					wrote, err := writeDeferredSSEData(streamWriter, &pendingFirstTokenEvents, sanitizeCapacityShedEventForClient(eventType, data), shouldDefer)
					if err != nil {
						writeErr = err
						clientGone = true
					} else if wrote {
						wroteAnyBody = true
					}
				}
				return !isResponsesTerminalEvent(eventType)
			}

			// 思考截断自动续想（默认关闭）：开启时用折叠状态机包裹 forward，
			// 命中 518n-2 截断指纹则用同一账号续发上游并折叠成单响应；
			// 关闭时保持原有逐事件透传路径，字节级零变化。
			// 默认（未启用自动续想）路径也可能在 xhigh/max 的长推理阶段数十秒
			// 没有可转发帧。定期写标准 SSE 注释，避免本机反代/Tailscale
			// 把健康长流误判为空闲连接。自动续想路径已有自己的隐藏轮保活，
			// 不重复启动第二个 ticker。
			stopDownstreamKeepalive := func() {}
			if !contEnabled {
				stopDownstreamKeepalive = startDownstreamSSEKeepalive(c.Request.Context(), downstreamSSEKeepaliveInterval, func() bool {
					downstreamMu.Lock()
					defer downstreamMu.Unlock()
					if c.Request.Context().Err() != nil {
						clientGone = true
						return false
					}
					if clientGone {
						return false
					}
					// 首个真实字节前不能写注释，否则会提前提交 HTTP 200，
					// 破坏首包前 response.failed 的真实状态码与换号重试语义。
					if !wroteAnyBody {
						return true
					}
					if err := streamWriter.WriteSSEComment(downstreamSSEKeepaliveComment); err != nil {
						writeErr = err
						clientGone = true
						return false
					}
					return true
				})
			}
			if contEnabled {
				fold := &continueFold{
					baseBody:  upstreamBody,
					maxRounds: contMaxRounds,
					forward:   forward,
					observe: func(data []byte) {
						// 被缓冲（暂未转发给客户端）的事件只用来保活首字超时 guard，
						// 避免纯 message 响应在整体缓冲期间被误判超时。这里不置位
						// ttftRecorded/firstTokenMs：客户端此刻尚未收到任何字节，真正的
						// 首 token 计时在 flushBuffered 经 forward 冲刷时才发生，
						// 否则会破坏首包前 response.failed 的抑制/换号语义。
						ttftGuard.MarkProgress(gjson.GetBytes(data, "type").String())
					},
					clientGone: func() bool {
						downstreamMu.Lock()
						gone := clientGone
						downstreamMu.Unlock()
						return gone || c.Request.Context().Err() != nil
					},
					keepalive: func() bool {
						downstreamMu.Lock()
						defer downstreamMu.Unlock()
						// 首个真实字节写出前绝不保活:注释一旦落笔就提交 200 header,
						// 首 token 前 response.failed 按真实错误码返回/换号重试的全部
						// 语义会被摧毁(PR #318 同类坑)。此时也无 200 可保,直接跳过。
						if clientGone || !wroteAnyBody {
							return !clientGone
						}
						if err := streamWriter.WriteSSEComment(continueKeepaliveComment); err != nil {
							writeErr = err
							clientGone = true
							return false
						}
						return true
					},
					openRound: func(body []byte) (*http.Response, error) {
						// 续想轮复用同一账号与上游通道（reasoning encrypted_content 绑定账号，
						// 换号会被上游拒绝），沿用与客户端解耦的 drainable context。
						if lastUpstreamCancel != nil {
							lastUpstreamCancel()
						}
						rctx, rcancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
						// 续想轮复用同一账号，沿用带账号维度的 attempt 身份。
						rctx = WithPayloadRuleIdentity(rctx, attemptIdentity)
						lastUpstreamCancel = rcancel
						roundBody := body
						if useWebsocket {
							roundBody = stripResponsesImageGenerationTool(body)
						}
						roundResp, roundErr := ExecuteRequest(rctx, account, roundBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
						// 续想轮同样消耗账号额度：成功开轮后同步上游用量头，
						// 否则多轮隐藏请求的额度对自动暂停/配速不可见。
						if roundErr == nil && roundResp != nil && roundResp.StatusCode == http.StatusOK {
							SyncCodexUsageState(h.store, account, roundResp)
						}
						return roundResp, roundErr
					},
				}
				foldRes := runContinueThinkingFold(resp, fold)
				readErr = foldRes.ReadErr
				// 折叠可能产出合成/重构的 response.incomplete 终态（续想失败/EOF），
				// forward 只对 completed/failed 置位 gotTerminal，这里据折叠结果补齐，
				// 否则正常收尾的折叠流会被误判为断流：惩罚账号、解绑亲和、用估算值覆盖真实 usage。
				if foldRes.GotTerminal {
					gotTerminal = true
				}
				// 折叠拦截了各轮真实终态，forward 未必看到 response.completed，
				// 用折叠汇总的最终轮真实 usage 作为本 attempt 收尾计费值。
				if foldRes.FinalUsage != nil {
					usage = foldRes.FinalUsage
				}
				// 除最终轮外的各真实轮 + 失败的续想开轮各补记一条真实用量，
				// 最终轮由本 attempt 收尾统一记账，避免重复或漏记。
				h.logContinueThinkingRounds(c, foldRes, account, logModel, logEffectiveModel, reasoningEffort, useWebsocket, serviceTier)
				if foldRes.FinalResponse != nil {
					resp = foldRes.FinalResponse
				}
			} else {
				readErr = ReadSSEStream(resp.Body, forward)
			}
			stopDownstreamKeepalive()
			// 仅在真的写过 body 时才做收尾 flush:flusher.Flush 会先提交 HTTP 200 header,
			// 零写入时提前 flush 会让循环外的 c.JSON(4xx) 失效(status 已定型为 200)。
			if writeErr == nil && wroteAnyBody {
				writeErr = streamWriter.Flush()
			}
			// 流结束但未收到终止事件（上游断流）：已写过正文时无法整段静默重试，
			// 合成 response.failed（code=upstream_stream_break）给下游一个可编程
			// 识别的失败终态，而不是静默 EOF 的"假 200"(issue #473)。续想折叠
			// 路径的 EOF 已自带合成终态（gotTerminal=true），不会走到这里。
			if shouldWriteStreamBreakEvent(gotTerminal, wroteAnyBody, c.Request.Context().Err(), writeErr) {
				if err := writeResponsesStreamBreakEvent(streamWriter); err != nil {
					log.Printf("写入合成 response.failed 断流事件失败 (/v1/responses): %v", err)
				}
			}
		} else {
			// 非流式收集
			var lastResponseData []byte
			outputItems := make([]json.RawMessage, 0, 2)
			seenOutputItems := make(map[string]struct{})
			imageOutputs := make([]json.RawMessage, 0, 1)
			seenImageOutputs := make(map[string]struct{})
			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				h.recordCompactionProvenanceFromPayload(context.Background(), account, data)
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()
				if outputItem, ok := extractResponseOutputItemDone(data, seenOutputItems); ok {
					outputItems = append(outputItems, outputItem)
				}
				if imageOutput, ok := extractResponseImageGenerationOutput(data, seenImageOutputs); ok {
					imageOutputs = append(imageOutputs, imageOutput)
				}
				ttftGuard.MarkProgress(eventType)
				if !ttftRecorded && isFirstTokenResultForMode(parsed, currentFirstTokenMode()) {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if isResponsesSuccessTerminalEvent(eventType) {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					if eventType == "response.completed" {
						// 截断态不入缓存，理由同流式分支。
						cacheCompletedResponseWithOutputItems(respCacheOwner, []byte(expandedInputRaw), data, outputItems)
					}
					gotTerminal = true
					lastResponseData = data
					return false
				}
				if eventType == "response.failed" {
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
					lastResponseData = data
					return false
				}
				return true
			})

			if lastResponseData != nil {
				responseObj := gjson.GetBytes(lastResponseData, "response")
				if responseObj.Exists() {
					responseJSON = []byte(responseObj.Raw)
					responseJSON = restoreMissingResponseOutputs(responseJSON, outputItems)
					responseJSON = appendMissingResponseImageOutputs(responseJSON, imageOutputs)
					imageLogInfo = imageUsageLogInfoFromResponseJSON(responseJSON)
				}
			}
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
			outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
		}
		outcome = annotateStreamBreakDiagnostics(outcome, streamDiag)
		ttftGuard.Stop()
		if outcome.verifyAccountAuth {
			h.store.VerifyAccountAuthAsync(account)
		}
		var responseFailedDecision codex429Decision
		if len(terminalFailurePayload) > 0 {
			outcome = classifyResponseFailedOutcome(terminalFailurePayload)
			responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, effectiveModel)
			if responseFailedDecision.Reason != "" {
				outcome.failureKind = upstreamErrorKind(outcome.logStatusCode, responseFailedErrorBody(terminalFailurePayload), responseFailedDecision)
			}
			// 流式 response.failed（HTTP 200）里的 cyber_policy 处罚也要记录，
			// 否则只有非 2xx 错误体才会被记入提示词过滤日志。
			if !upstreamCyberPolicyLogged {
				promptPolicyIncidentID = acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, responseFailedErrorBody(terminalFailurePayload), upstreamCyberPolicyAttempt{
					Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: outcome.logStatusCode,
					AccountID: account.ID(), AttemptIndex: attempt + 1,
				}))
			}
			if isExplicitUpstreamCyberPolicy(terminalFailurePayload) {
				outcome.failureMessage = upstreamCyberPolicyResponseMessage(c)
			}
		}
		if wsHTTPFallback.ForceHTTP() && !useWebsocket {
			wsHTTPFallback.LogHTTPAttemptCompletion("/v1/responses", account.ID(), attempt+1, totalDuration, firstTokenMs, outcome.logStatusCode)
		}
		if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, useWebsocket, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			wsElapsed := time.Since(start)
			resp.Body.Close()
			globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
			wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(outcome.failureMessage))
			log.Printf("上游 WebSocket 1009，首包前保留账号租约并降级 HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/responses, ws_elapsed_ms=%d): %s", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), outcome.failureMessage)
			continue
		}
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			h.logPromptPolicyRetryUsage(c, database.UsageLogInput{
				AccountID: account.ID(), Endpoint: "/v1/responses", Model: logModel, EffectiveModel: logEffectiveModel,
				StatusCode: outcome.logStatusCode, DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
				InboundEndpoint: "/v1/responses", UpstreamEndpoint: "/v1/responses", Stream: isStream, ViaWebsocket: useWebsocket,
				AttemptIndex: attempt + 1, UpstreamErrorKind: outcome.failureKind,
				ErrorMessage: usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
			}, promptPolicyIncidentID)
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/responses): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			if isFirstTokenTimeoutOutcome(outcome) {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
			} else {
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			}
			resp.Body.Close()
			h.store.Release(account)
			h.unbindOrRetainAffinityForCapacityShed(retryExclusions, affinityKey, account, outcome, capacityShedRetries)
			// 首字超时已白等一轮,不再叠加重试间隔;其余首包前断流按配置间隔等待
			if !isFirstTokenTimeoutOutcome(outcome) && !h.waitBeforeRetry(c.Request.Context()) {
				return
			}
			continue
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		logStatusCode := outcome.logStatusCode
		if logStatusCode != http.StatusOK {
			c.Set(AccessLogStatusContextKey, logStatusCode)
		}
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("流异常结束 (account %d, /v1/responses, status %d): %s，已转发约 %d 字符", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3 // 粗略估算: 约 3 字符 = 1 token
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		accountReleasedForOverflow := false
		// 流内报上下文超窗（HTTP SSE 与 WS 上游同路径）+ Key 开启自动压缩：
		// 未向下游写过任何字节时，摘要旧轮次后同参重试一次 (issue #415)。
		if overflowCompactEnabled && !overflowCompactRetried && !wroteAnyBody &&
			(!isStream || abortedForHTTPError) &&
			isContextLengthExceededFailedPayload(terminalFailurePayload) {
			resp.Body.Close()
			h.ReleaseAPIKeyScopeConcurrency(c)
			h.store.Release(account)
			accountReleasedForOverflow = true
			if compacted, ok := h.compactOverflowResponsesBodyForRequest(c, codexBody); ok {
				overflowCompactRetried = true
				codexBody = compacted
				expandedInputRaw = responsesInputRaw(codexBody)
				log.Printf("上游流内报上下文超窗，已压缩旧轮次并重试一次 (attempt %d)", attempt+1)
				continue
			}
		}

		if isStream && abortedForHTTPError && !wroteAnyBody {
			// 流式:首 token 前上游失败、未向下游写过任何内容,HTTP 200 header 尚未提交,
			// 覆盖预设的 SSE Content-Type 后按真实错误码返回 JSON,
			// 避免下游中转/计费方把它当成功并按预估 input token 计费(与回调内 reset 呼应)。
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(logStatusCode, gin.H{
				"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
			})
		} else if isStream && !wroteAnyBody && logStatusCode == logStatusUpstreamStreamBreak &&
			c.Request.Context().Err() == nil && writeErr == nil {
			// 首包前断流/首字超时且重试耗尽：原先没有任何写出分支命中，下游
			// 收到空 body 的"假 200"，失败完全不可感知 (issue #473)。598 是内部
			// 日志状态，对外按真实 502 + 稳定错误码返回，下游可编程识别并重试。
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{"message": outcome.failureMessage, "type": ErrorTypeUpstreamError, "code": ErrorCodeUpstreamStreamBreak},
			})
		} else if !isStream {
			if len(terminalFailurePayload) > 0 {
				c.JSON(logStatusCode, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
				})
			} else if responseJSON != nil {
				c.Data(http.StatusOK, "application/json", responseJSON)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
				})
			}
		}

		usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)
		c.Set("x-service-tier", usageTiers.ServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:              account.ID(),
			Endpoint:               "/v1/responses",
			Model:                  logModel,
			EffectiveModel:         logEffectiveModel,
			StatusCode:             logStatusCode,
			DurationMs:             totalDuration,
			FirstTokenMs:           firstTokenMs,
			ReasoningEffort:        reasoningEffort,
			InboundEndpoint:        "/v1/responses",
			UpstreamEndpoint:       "/v1/responses",
			Stream:                 isStream,
			ViaWebsocket:           useWebsocket,
			ServiceTier:            usageTiers.ServiceTier,
			RequestedServiceTier:   usageTiers.RequestedServiceTier,
			ActualServiceTier:      usageTiers.ActualServiceTier,
			BillingServiceTier:     usageTiers.BillingServiceTier,
			PromptPolicyIncidentID: promptPolicyIncidentID,
			AttemptIndex:           attempt + 1,
		}
		if logStatusCode != http.StatusOK {
			logInput.ErrorMessage = usageLogFailureMessage(logStatusCode, outcome.failureMessage)
			logInput.UpstreamErrorKind = outcome.failureKind
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		applyImageUsageLogInfo(logInput, imageLogInfo)
		h.logUsageForRequest(c, logInput)

		if !accountReleasedForOverflow {
			resp.Body.Close()
		}
		if outcome.penalize {
			recyclePooledClient(account, proxyURL)
			h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ClearModelCooldown(account, effectiveModel)
			h.store.ConfirmResponsesAvailableSince(account, start)
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		if !accountReleasedForOverflow {
			if outcome.logStatusCode == http.StatusOK {
				h.store.ReleaseForSession(account, affinityKey)
			} else {
				h.store.Release(account)
			}
		}
		return
	}
}

// ResponsesCompact 处理 /v1/responses/compact 请求（非流式压缩接口，透传到上游 /responses/compact）
func (h *Handler) ResponsesCompact(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	h.capturePromptRequestIngress(c, rawBody)
	cacheRequestCompactionMeta(c, requestCompactionMetaForHTTP(c, rawBody))

	supportedModels := h.supportedModelIDs(c.Request.Context())
	// 先让全局/渠道映射看到客户端原始模型（包括 -openai-compact 别名）；
	// 没有命中映射时，再按兼容规则剥离后缀。
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredCompactModelMappingToBody(rawBody, supportedModels)
	rawBody, _ = normalizePortableResponsesCompactionHistory(rawBody)
	setRawRequestBody(c, rawBody)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRulesForModel(mappedModel)
	if requestUpstreamChannel(c) != database.UpstreamChannelGrok {
		// grok 渠道 Key 的模型由 Grok 上游校验，跳过网关侧模型白名单
		rules["model"] = append(rules["model"], h.modelValidator(supportedModels))
	}
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	// routingModel 保留客户端原始模型名（可能带 -openai-compact 后缀），供账号级
	// compact 映射与账号过滤匹配别名规则；logModel 用于统计与日志展示，别名后缀
	// 只是端点路由约定，展示时一律折算成基础模型名（仅剥后缀不算映射，不显示箭头）。
	routingModel := requestModel
	if routingModel == "" {
		routingModel = model
	}
	logModel := routingModel
	if baseModel, stripped := stripCompactModelSuffix(logModel); stripped {
		logModel = baseModel
	}
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}
	if model == "" {
		api.SendMissingFieldError(c, "model")
		return
	}
	if isMediaOnlyModel(model) {
		sendImageOnlyModelError(c, model)
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/responses/compact", model) {
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	if err := ValidateResponsesFunctionNames(rawBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	// compact 强制非流式
	rawBody, _ = sjson.SetBytes(rawBody, "stream", false)

	// 准备上游请求体（previous_response_id 缓存按下游 API Key 隔离）
	bodyPreparation := prepareCompactResponsesBodyForOwnerDetailed(rawBody, responseCacheOwner(apiKeyID))
	codexBody := bodyPreparation.Body
	continuationStatus, continuationReason, continuationUnavailable := responseCachePreparationFailure(bodyPreparation)
	// strip 策略：剥离图片工具能力声明后作为普通文本请求继续（issue #411）。
	codexBody = applyImageGenerationStripPolicy(c, codexBody)
	if err := validateResponsesImageGenerationSizes(codexBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	effectiveModel := effectiveRequestModel(codexBody, model)
	logEffectiveModel := usageEffectiveModelForMapping(logModel, effectiveModel, mappingApplied)
	if h.enforceAPIKeyLimitsAndReply(c, effectiveModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	// compact 同时允许官方 Codex OAuth 账号与中转（OpenAI Responses API）账号：
	// 中转账号会命中上游自身的 /responses/compact，使仅接入中转的用户也能压缩（issue #174）。
	accountFilter := accountFilterForCompactResponsesModelWithOriginal(routingModel, effectiveModel, modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db)))
	accountFilter = h.withModelCooldownFilter(effectiveModel, accountFilter)
	if continuationUnavailable {
		accountFilter = relayOnlyAccountFilter(accountFilter)
	}
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	// resolveCompactionAffinity 只在已知来源相互冲突时报错；缓存故障按未知
	// 来源处理，保持正常调度。
	compactionAffinity, compactionAffinityErr := h.resolveCompactionAffinity(c.Request.Context(), rawBody)
	if compactionAffinityErr != nil {
		sendCompactionProvenanceConflict(c)
		return
	}
	if compactionAffinity.Known {
		accountFilter = compactionDomainFilter(compactionAffinity.CompatibilityDomain, accountFilter)
	}
	// scope 并发位在选中账号后才能占，请求退出时统一释放（issue #439 v2）。
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	// compact 走中转账号时需要 OpenAI Responses 形态的请求体
	openAIResponsesBody := PrepareOpenAIResponsesCompactBody(rawBody)

	// 带重试的上游请求
	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool)
	invalidEncryptedContentRetried := false
	relayContinuationAttempted := false

	dispatchPolicy := dispatchPolicyForModel(effectiveModel)
	for attempt := 0; ; attempt++ {
		var account *auth.Account
		var stickyProxyURL string
		if attempt == 0 && compactionAffinity.Known {
			account = h.store.TakePreferredAccountWithDispatch(compactionAffinity.PreferredAccountID, apiKeyID, excludeAccounts, accountFilter, dispatchPolicy)
			if account != nil {
				stickyProxyURL = account.GetProxyURL()
			}
		}
		if account == nil {
			account, stickyProxyURL = h.nextAccountForSessionWithDispatch(affinityKey, apiKeyID, excludeAccounts, accountFilter, dispatchPolicy)
		}
		if account == nil {
			if compactionAffinity.Known {
				sendCompactionUpstreamUnavailable(c)
				return
			}
			if continuationUnavailable && !relayContinuationAttempted {
				if msg := scopeBudgetExhaustedMessage(c); msg != "" {
					SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
					return
				}
				sendResponseContextUnavailable(c, continuationStatus, continuationReason)
				return
			}
			account, stickyProxyURL = h.store.WaitForSessionAvailableWithDispatch(c.Request.Context(), affinityKey, 30*time.Second, apiKeyID, excludeAccounts, accountFilter, dispatchPolicy)
			if account == nil {
				if (lastStatusCode == http.StatusTooManyRequests || lastStatusCode == http.StatusBadGateway) && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				if msg := scopeBudgetExhaustedMessage(c); msg != "" {
					SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
					return
				}
				if compactionAffinity.Known {
					sendCompactionUpstreamUnavailable(c)
					return
				}
				c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(effectiveModel))
				return
			}
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		attemptEffectiveModel := effectiveModel
		attemptLogEffectiveModel := logEffectiveModel

		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()

		if account.IsOpenAIResponsesAPI() {
			relayContinuationAttempted = true
			baseURL, _ := account.OpenAIResponsesCredentials()
			upstreamEndpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/responses/compact")
			upstreamBody := openAIResponsesBody
			if mappedBody, mappedModel, ok := h.applyAccountCompactModelMappingToBody(upstreamBody, account, routingModel, effectiveModel); ok {
				upstreamBody = mappedBody
				attemptEffectiveModel = mappedModel
				attemptLogEffectiveModel = usageEffectiveModelForMapping(logModel, attemptEffectiveModel, true)
			}
			resp, reqErr := ExecuteOpenAIResponsesCompactRequest(c.Request.Context(), account, upstreamBody, proxyURL, downstreamHeaders)
			durationMs := int(time.Since(start).Milliseconds())

			if reqErr != nil {
				if kind := classifyTransportFailure(reqErr); shouldPenalizeTransportKind(kind) {
					h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
				}
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				excludeAccounts[account.ID()] = true

				if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
					ErrorToGinResponse(c, reqErr)
					return
				}

				log.Printf("OpenAI Responses compact 上游请求失败 (attempt %d): %v", attempt+1, reqErr)
				if shouldRetryRequestError(reqErr, &generalRetries, maxRetries) {
					continue
				}
				ErrorToGinResponse(c, reqErr)
				return
			}

			if resp.StatusCode != http.StatusOK {
				errBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				if !invalidEncryptedContentRetried && isInvalidEncryptedContentError(resp.StatusCode, errBody) {
					strippedRawBody, rawChanged := stripInvalidEncryptedContentFromResponsesBody(rawBody)
					strippedCodexBody, codexChanged := stripInvalidEncryptedContentFromResponsesBody(codexBody)
					if rawChanged || codexChanged {
						invalidEncryptedContentRetried = true
						if rawChanged {
							rawBody = strippedRawBody
							openAIResponsesBody = PrepareOpenAIResponsesCompactBody(rawBody)
						}
						if codexChanged {
							codexBody = strippedCodexBody
						}
						log.Printf("OpenAI Responses compact 上游拒绝 encrypted_content，已移除加密 reasoning 上下文并重试一次 (attempt %d)", attempt+1)
						h.store.Release(account)
						h.store.UnbindSessionAffinity(affinityKey, account.ID())
						continue
					}
				}

				if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
					h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
				}
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				excludeAccounts[account.ID()] = true

				logUpstreamError("/v1/responses/compact", resp.StatusCode, logModel, account.ID(), errBody)
				promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses/compact", logModel, errBody, upstreamCyberPolicyAttempt{
					Transport: "http", StatusCode: resp.StatusCode, AccountID: account.ID(), AttemptIndex: attempt + 1,
				}))
				decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, attemptEffectiveModel)
				shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries))
				usageTiers := resolveUsageServiceTiers("", serviceTier)
				h.logUsageForRequest(c, &database.UsageLogInput{
					AccountID:              account.ID(),
					Endpoint:               "/v1/responses/compact",
					Model:                  logModel,
					EffectiveModel:         attemptLogEffectiveModel,
					StatusCode:             resp.StatusCode,
					DurationMs:             durationMs,
					ReasoningEffort:        reasoningEffort,
					InboundEndpoint:        "/v1/responses/compact",
					UpstreamEndpoint:       upstreamEndpoint,
					ServiceTier:            usageTiers.ServiceTier,
					RequestedServiceTier:   usageTiers.RequestedServiceTier,
					ActualServiceTier:      usageTiers.ActualServiceTier,
					BillingServiceTier:     usageTiers.BillingServiceTier,
					IsRetryAttempt:         shouldRetry,
					AttemptIndex:           attempt + 1,
					UpstreamErrorKind:      upstreamErrorKind(resp.StatusCode, errBody, decision),
					ErrorMessage:           usageLogErrorMessage(resp.StatusCode, errBody),
					PromptPolicyIncidentID: promptPolicyIncidentID,
				})

				if shouldRetry {
					lastStatusCode = resp.StatusCode
					lastBody = errBody
					if !h.waitBeforeRetry(c.Request.Context()) {
						return
					}
					continue
				}

				h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
				return
			}

			respBody, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				totalDuration := int(time.Since(start).Milliseconds())
				kind := classifyTransportFailure(readErr)
				if kind == "" {
					kind = "transport"
				}
				h.store.ReportRequestFailure(account, kind, time.Duration(totalDuration)*time.Millisecond)
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				excludeAccounts[account.ID()] = true

				shouldRetry := shouldRetryRequestError(readErr, &generalRetries, maxRetries)
				usageTiers := resolveUsageServiceTiers("", serviceTier)
				h.logUsageForRequest(c, &database.UsageLogInput{
					AccountID:            account.ID(),
					Endpoint:             "/v1/responses/compact",
					Model:                logModel,
					EffectiveModel:       attemptLogEffectiveModel,
					StatusCode:           http.StatusBadGateway,
					DurationMs:           totalDuration,
					ReasoningEffort:      reasoningEffort,
					InboundEndpoint:      "/v1/responses/compact",
					UpstreamEndpoint:     upstreamEndpoint,
					ServiceTier:          usageTiers.ServiceTier,
					RequestedServiceTier: usageTiers.RequestedServiceTier,
					ActualServiceTier:    usageTiers.ActualServiceTier,
					BillingServiceTier:   usageTiers.BillingServiceTier,
					IsRetryAttempt:       shouldRetry,
					AttemptIndex:         attempt + 1,
					UpstreamErrorKind:    kind,
					ErrorMessage:         fmt.Sprintf("上游响应读取失败: %v", readErr),
				})
				log.Printf("OpenAI Responses compact 上游响应读取失败 (attempt %d): %v", attempt+1, readErr)
				if shouldRetry {
					lastStatusCode = http.StatusBadGateway
					lastBody = []byte(`{"error":{"message":"Failed to read upstream response","type":"upstream_error","code":"upstream_read_error"}}`)
					continue
				}
				api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeUpstreamError, "Failed to read upstream response", api.ErrorTypeUpstream), http.StatusBadGateway)
				return
			}
			h.recordCompactionProvenanceFromPayload(context.Background(), account, respBody)

			h.store.ClearModelCooldown(account, attemptEffectiveModel)
			h.store.ReportRequestSuccess(account, time.Duration(durationMs)*time.Millisecond)

			promptTokens := int(gjson.GetBytes(respBody, "usage.input_tokens").Int())
			completionTokens := int(gjson.GetBytes(respBody, "usage.output_tokens").Int())
			totalTokens := int(gjson.GetBytes(respBody, "usage.total_tokens").Int())
			reasoningTokens := int(gjson.GetBytes(respBody, "usage.output_tokens_details.reasoning_tokens").Int())
			cachedTokens := int(gjson.GetBytes(respBody, "usage.input_tokens_details.cached_tokens").Int())

			actualServiceTier := gjson.GetBytes(respBody, "service_tier").String()
			usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)

			c.Set("x-account-email", baseURL)
			c.Set("x-account-proxy", proxyURL)
			c.Set("x-model", logModel)
			c.Set("x-reasoning-effort", reasoningEffort)
			c.Set("x-service-tier", usageTiers.ServiceTier)

			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:            account.ID(),
				Endpoint:             "/v1/responses/compact",
				Model:                logModel,
				EffectiveModel:       attemptLogEffectiveModel,
				StatusCode:           http.StatusOK,
				DurationMs:           durationMs,
				PromptTokens:         promptTokens,
				CompletionTokens:     completionTokens,
				TotalTokens:          totalTokens,
				InputTokens:          promptTokens,
				OutputTokens:         completionTokens,
				ReasoningTokens:      reasoningTokens,
				CachedTokens:         cachedTokens,
				ReasoningEffort:      reasoningEffort,
				InboundEndpoint:      "/v1/responses/compact",
				UpstreamEndpoint:     upstreamEndpoint,
				ServiceTier:          usageTiers.ServiceTier,
				RequestedServiceTier: usageTiers.RequestedServiceTier,
				ActualServiceTier:    usageTiers.ActualServiceTier,
				BillingServiceTier:   usageTiers.BillingServiceTier,
			})

			h.store.ReleaseForSession(account, affinityKey)
			contentType := resp.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "application/json"
			}
			c.Data(http.StatusOK, contentType, respBody)
			return
		}

		// compact（会话压缩续写）刻意保留确定性 IsolateCodexSessionID、不走 resolveUpstreamSessionID
		// 的默认隔离：压缩本身是对同一会话的延续，需要稳定的 prompt_cache_key 维持缓存连续性。
		upstreamSessionID := IsolateCodexSessionID(apiKeyID, sessionIdentity.upstreamSeed)
		// compact_via_responses_enabled：上游已下线 /responses/compact 专用端点（404），
		// 开启后官方账号改走 /responses + compaction_trigger 的 body-signal 形态
		// （强制 HTTP SSE），成功后聚合回 compact 的一次性 JSON。
		compactViaResponses := CurrentRuntimeSettings().CompactViaResponses
		upstreamEndpointLabel := "/v1/responses/compact"
		var resp *http.Response
		var reqErr error
		if compactViaResponses {
			upstreamEndpointLabel = "/v1/responses"
			resp, reqErr = ExecuteRequest(c.Request.Context(), account, appendCompactionTriggerToResponsesBody(codexBody), upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, false)
		} else {
			resp, reqErr = ExecuteCompactRequest(c.Request.Context(), account, codexBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders)
		}
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); shouldPenalizeTransportKind(kind) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("compact 上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			if shouldRetryRequestError(reqErr, &generalRetries, maxRetries) {
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if !invalidEncryptedContentRetried && isInvalidEncryptedContentError(resp.StatusCode, errBody) {
				strippedRawBody, rawChanged := stripInvalidEncryptedContentFromResponsesBody(rawBody)
				strippedCodexBody, codexChanged := stripInvalidEncryptedContentFromResponsesBody(codexBody)
				if rawChanged || codexChanged {
					invalidEncryptedContentRetried = true
					if rawChanged {
						rawBody = strippedRawBody
						openAIResponsesBody = PrepareOpenAIResponsesCompactBody(rawBody)
					}
					if codexChanged {
						codexBody = strippedCodexBody
					}
					log.Printf("compact 上游拒绝 encrypted_content，已移除加密 reasoning 上下文并重试一次 (attempt %d)", attempt+1)
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			logUpstreamError("/v1/responses/compact", resp.StatusCode, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses/compact", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: "http", StatusCode: resp.StatusCode, AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries))
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/responses/compact",
				Model:                  logModel,
				EffectiveModel:         logEffectiveModel,
				StatusCode:             resp.StatusCode,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/responses/compact",
				UpstreamEndpoint:       upstreamEndpointLabel,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				IsRetryAttempt:         shouldRetry,
				AttemptIndex:           attempt + 1,
				UpstreamErrorKind:      upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:           usageLogErrorMessage(resp.StatusCode, errBody),
				PromptPolicyIncidentID: promptPolicyIncidentID,
			})

			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				if !h.waitBeforeRetry(c.Request.Context()) {
					return
				}
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功：直接透传响应体（body-signal 兼容模式先把 SSE 聚合成一次性 JSON）
		var respBody []byte
		var readErr error
		var compactFailedPayload []byte
		if compactViaResponses {
			respBody, compactFailedPayload, readErr = collectCompactResponsesSSE(resp.Body)
		} else {
			respBody, readErr = io.ReadAll(resp.Body)
		}
		resp.Body.Close()
		if readErr != nil {
			totalDuration := int(time.Since(start).Milliseconds())
			kind := classifyTransportFailure(readErr)
			if kind == "" {
				kind = "transport"
			}
			h.store.ReportRequestFailure(account, kind, time.Duration(totalDuration)*time.Millisecond)
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			shouldRetry := shouldRetryRequestError(readErr, &generalRetries, maxRetries)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:            account.ID(),
				Endpoint:             "/v1/responses/compact",
				Model:                logModel,
				EffectiveModel:       logEffectiveModel,
				StatusCode:           http.StatusBadGateway,
				DurationMs:           totalDuration,
				ReasoningEffort:      reasoningEffort,
				InboundEndpoint:      "/v1/responses/compact",
				UpstreamEndpoint:     upstreamEndpointLabel,
				ServiceTier:          usageTiers.ServiceTier,
				RequestedServiceTier: usageTiers.RequestedServiceTier,
				ActualServiceTier:    usageTiers.ActualServiceTier,
				BillingServiceTier:   usageTiers.BillingServiceTier,
				IsRetryAttempt:       shouldRetry,
				AttemptIndex:         attempt + 1,
				UpstreamErrorKind:    kind,
				ErrorMessage:         fmt.Sprintf("上游响应读取失败: %v", readErr),
			})
			log.Printf("compact 上游响应读取失败 (attempt %d): %v", attempt+1, readErr)
			if shouldRetry {
				lastStatusCode = http.StatusBadGateway
				lastBody = []byte(`{"error":{"message":"Failed to read upstream response","type":"upstream_error","code":"upstream_read_error"}}`)
				continue
			}
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeUpstreamError, "Failed to read upstream response", api.ErrorTypeUpstream), http.StatusBadGateway)
			return
		}
		h.recordCompactionProvenanceFromPayload(context.Background(), account, respBody)

		// body-signal 兼容模式：SSE 内的 response.failed 终态按上游错误处理，
		// 语义对齐传统 compact 链路的 HTTP 非 200 分支（含 encrypted_content 剥离重试）。
		if compactViaResponses && len(compactFailedPayload) > 0 {
			failStatus := responseFailedStatusCode(compactFailedPayload)
			if isExplicitUpstreamCyberPolicy(compactFailedPayload) {
				failStatus = http.StatusBadRequest
			}
			errBody := responseFailedErrorBody(compactFailedPayload)

			if !invalidEncryptedContentRetried && isInvalidEncryptedContentError(failStatus, errBody) {
				strippedRawBody, rawChanged := stripInvalidEncryptedContentFromResponsesBody(rawBody)
				strippedCodexBody, codexChanged := stripInvalidEncryptedContentFromResponsesBody(codexBody)
				if rawChanged || codexChanged {
					invalidEncryptedContentRetried = true
					if rawChanged {
						rawBody = strippedRawBody
						openAIResponsesBody = PrepareOpenAIResponsesCompactBody(rawBody)
					}
					if codexChanged {
						codexBody = strippedCodexBody
					}
					log.Printf("compact(body-signal) 上游拒绝 encrypted_content，已移除加密 reasoning 上下文并重试一次 (attempt %d)", attempt+1)
					h.store.Release(account)
					h.store.UnbindSessionAffinity(affinityKey, account.ID())
					continue
				}
			}

			if kind := classifyHTTPFailure(failStatus); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			excludeAccounts[account.ID()] = true

			logUpstreamError("/v1/responses/compact", failStatus, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/responses/compact", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: "http", StatusCode: failStatus, AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyResponseFailedCooldown(account, compactFailedPayload, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(failStatus, errBody, &generalRetries, &rateLimitRetries, maxRetries, h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries))
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/responses/compact",
				Model:                  logModel,
				EffectiveModel:         logEffectiveModel,
				StatusCode:             failStatus,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/responses/compact",
				UpstreamEndpoint:       upstreamEndpointLabel,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				IsRetryAttempt:         shouldRetry,
				AttemptIndex:           attempt + 1,
				UpstreamErrorKind:      upstreamErrorKind(failStatus, errBody, decision),
				ErrorMessage:           usageLogErrorMessage(failStatus, errBody),
				PromptPolicyIncidentID: promptPolicyIncidentID,
			})

			if shouldRetry {
				lastStatusCode = failStatus
				lastBody = errBody
				if !h.waitBeforeRetry(c.Request.Context()) {
					return
				}
				continue
			}

			h.sendFinalUpstreamError(c, failStatus, errBody)
			return
		}

		SyncCodexUsageState(h.store, account, resp)
		h.store.ClearModelCooldown(account, effectiveModel)

		// 提取 usage 用于日志
		promptTokens := int(gjson.GetBytes(respBody, "usage.input_tokens").Int())
		completionTokens := int(gjson.GetBytes(respBody, "usage.output_tokens").Int())
		totalTokens := int(gjson.GetBytes(respBody, "usage.total_tokens").Int())
		reasoningTokens := int(gjson.GetBytes(respBody, "usage.output_tokens_details.reasoning_tokens").Int())
		cachedTokens := int(gjson.GetBytes(respBody, "usage.input_tokens_details.cached_tokens").Int())

		actualServiceTier := gjson.GetBytes(respBody, "service_tier").String()
		usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)

		totalDuration := int(time.Since(start).Milliseconds())
		h.logUsageForRequest(c, &database.UsageLogInput{
			AccountID:            account.ID(),
			Endpoint:             "/v1/responses/compact",
			Model:                logModel,
			EffectiveModel:       logEffectiveModel,
			StatusCode:           http.StatusOK,
			DurationMs:           totalDuration,
			PromptTokens:         promptTokens,
			CompletionTokens:     completionTokens,
			TotalTokens:          totalTokens,
			InputTokens:          promptTokens,
			OutputTokens:         completionTokens,
			ReasoningTokens:      reasoningTokens,
			CachedTokens:         cachedTokens,
			ReasoningEffort:      reasoningEffort,
			InboundEndpoint:      "/v1/responses/compact",
			UpstreamEndpoint:     upstreamEndpointLabel,
			ServiceTier:          usageTiers.ServiceTier,
			RequestedServiceTier: usageTiers.RequestedServiceTier,
			ActualServiceTier:    usageTiers.ActualServiceTier,
			BillingServiceTier:   usageTiers.BillingServiceTier,
		})

		h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		h.store.ReleaseForSession(account, affinityKey)
		c.Data(http.StatusOK, "application/json", respBody)
		return
	}
}

func (h *Handler) ChatCompletions(c *gin.Context) {
	// 1. 读取请求体
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	h.capturePromptRequestIngress(c, rawBody)

	supportedModels := h.supportedModelIDs(c.Request.Context())
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredModelMappingToBody(rawBody, supportedModels)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ChatCompletionValidationRules()
	if requestUpstreamChannel(c) != database.UpstreamChannelGrok {
		// grok 渠道 Key 的模型由 Grok 上游校验，跳过网关侧模型白名单
		rules["model"] = append(rules["model"], h.modelValidator(supportedModels))
	}
	result := validator.ValidateRequest(rules)
	if !result.Valid {
		api.SendError(c, validator.ToAPIError())
		return
	}

	// 检查请求体大小
	if len(rawBody) > security.MaxRequestBodySize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"message": "请求体过大", "type": "invalid_request_error"},
		})
		return
	}

	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if mappedModel != "" {
		model = mappedModel
	}
	logModel := requestModel
	if logModel == "" {
		logModel = model
	}
	responseModel := logModel
	if model == "" {
		model = "gpt-5.4"
		logModel = model
		responseModel = model
	}
	if isMediaOnlyModel(model) {
		sendImageOnlyModelError(c, model)
		return
	}

	// 验证 model 参数
	if err := security.ValidateModelName(model); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "model 参数无效", "type": "invalid_request_error"},
		})
		return
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/chat/completions", model) {
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	reasoningEffort := extractReasoningEffort(rawBody)
	ruleIdentity := h.payloadRuleIdentity(c)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	// 2. 翻译请求：OpenAI Chat → Codex Responses
	codexBody, err := TranslateRequest(rawBody)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Request translation failed: "+err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	effectiveModel := effectiveRequestModel(codexBody, model)
	logEffectiveModel := usageEffectiveModelForMapping(logModel, effectiveModel, mappingApplied)
	if h.enforceAPIKeyLimitsAndReply(c, effectiveModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	// /v1/chat/completions 同时允许官方 Codex OAuth 账号与中转（OpenAI Responses API）账号：
	// 翻译后的请求体本身就是 Responses 形态，中转账号直接以 HTTP 转发（issue #181）。
	accountFilter := accountFilterForResponsesModelWithOriginal(logModel, effectiveModel, modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db)))
	accountFilter = h.withModelCooldownFilter(effectiveModel, accountFilter)
	accountFilter = h.applyUpstreamChannelFilter(c, effectiveModel, accountFilter)
	accountFilter = h.applyScopeBudgetFilter(c, accountFilter)
	// scope 并发位在选中账号后才能占，请求退出时统一释放（issue #439 v2）。
	defer h.ReleaseAPIKeyScopeConcurrency(c)

	sessionIdentity := resolveRequestSessionIdentity(c.Request.Header, codexBody)
	accountFilter = applyAffinityGroupRouting(c, sessionIdentity, accountFilter)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(sessionIdentity.affinityID, apiKeyID)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	var wsHTTPFallback websocketHTTPFallbackState

	// 上游 ctx 生命周期：每次 attempt 开始前用新的 drainable ctx 替换，
	// defer 兜底确保函数退出时上游被释放。
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	capacityShedRetries := map[int64]int{}
	dispatchPolicy := dispatchPolicyForModel(effectiveModel)
	for attempt := 0; ; attempt++ {
		account, stickyProxyURL, retainedHTTPFallback := wsHTTPFallback.Take()
		if !retainedHTTPFallback {
			account, stickyProxyURL = h.nextRetryAccountForSessionWithDispatch(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter, dispatchPolicy)
		}
		if account == nil {
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			// 候选被 scope 预算剔空时给出真实原因，而不是含糊的「无可用账号」。
			if msg := scopeBudgetExhaustedMessage(c); msg != "" {
				SendAPIKeyLimitError(c, http.StatusTooManyRequests, msg)
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(effectiveModel))
			return
		}

		h.AcquireAPIKeyScopeConcurrency(c, account)
		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		if !retainedHTTPFallback {
			h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		}
		if wsHTTPFallback.ForceHTTP() {
			log.Printf("上游 WebSocket 1009 后启动 HTTP 降级尝试 (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/chat/completions, ws_elapsed_ms=%d)", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsHTTPFallback.WSElapsed().Milliseconds())
		}
		isRelayAccount := account.IsRelayStyle()
		attemptEffectiveModel := effectiveModel
		attemptLogEffectiveModel := logEffectiveModel
		useWebsocket := h.shouldUseWebsocketForHTTP() && !wsHTTPFallback.ForceHTTP() && !isRelayAccount
		// 真实生图意图强制走 HTTP：WebSocket 传输大体积图片数据会卡死（issue #220）。
		// 仅凭注入的 image_generation 工具不触发降级，普通请求继续走 WS（issue #304）。
		if useWebsocket && rawResponsesBodyShouldForceHTTPForImageGeneration(codexBody) {
			useWebsocket = false
		}
		// 体积达到已学习的 1009 阈值时直接首发 HTTP,跳过 WS 必败等待(issue #404)。
		if useWebsocket && globalWSSizeRouter.PreferHTTP(len(codexBody)) {
			useWebsocket = false
			if attempt == 0 {
				log.Printf("[WS] 请求体 %dKB 达到已学习的 1009 体积阈值，直接走 HTTP 上游 (endpoint=/v1/chat/completions)", len(codexBody)/1024)
			}
		}
		upstreamEndpoint := "/v1/responses"
		if isRelayAccount {
			upstreamEndpoint = relayUpstreamEndpointForProtocol(account, GrokProtocolChatCompletions, attemptEffectiveModel)
		}

		// 提取 API Key 用于设备指纹稳定化
		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)

		// 使用注入的设备指纹配置
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{
				StabilizeDeviceProfile: false, // 默认关闭
			}
		}

		// 透传下游请求头用于指纹学习
		downstreamHeaders := c.Request.Header.Clone()

		// 身份按 attempt 附加实际选中账号维度：account_* 门随重试换号重新匹配（issue #410）。
		attemptIdentity := ruleIdentity.WithSelectedAccount(account, h.store)
		// service_tier 记账按 payload 规则改写后的值归因（覆写 service_tier 的规则才生效）。
		// 仅 Codex 路径（ExecuteRequest）套用规则；relay 账号不套用，保持原值。
		// 按尝试重算：不同尝试的生效模型/账号可能不同，规则按模型或账号门匹配则结果随之变化。
		if !isRelayAccount {
			serviceTier = EffectiveRequestedServiceTier(codexBody, attemptEffectiveModel, downstreamHeaders, attemptIdentity)
		}

		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, useWebsocket)
		// 上游使用与客户端解耦的 context：客户端中途断开时仍能继续读完
		// response.completed 拿到 usage（流式计费的关键）。
		// lastUpstreamCancel 在 attempt loop 顶部声明 + defer 兜底，
		// 这里覆盖前先 cancel 上一轮（重试时）。
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		upstreamCtx = WithPayloadRuleIdentity(upstreamCtx, attemptIdentity)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(currentFirstTokenTimeout(), upstreamCancel)
		var resp *http.Response
		var reqErr error
		if isRelayAccount {
			upstreamBody := codexBody
			if mappedBody, mappedModel, ok := h.applyAccountModelMappingToBodyForModels(upstreamBody, account, logModel, effectiveModel); ok {
				upstreamBody = mappedBody
				attemptEffectiveModel = mappedModel
				attemptLogEffectiveModel = usageEffectiveModelForMapping(logModel, attemptEffectiveModel, true)
			}
			resp, reqErr = ExecuteRelayStyleProtocolRequest(upstreamCtx, account, GrokProtocolChatCompletions, rawBody, upstreamBody, proxyURL, downstreamHeaders)
		} else {
			// WebSocket 上游下剥离自动注入的图片工具，防止模型自主生图卡死 WS 流（issue #220）。
			upstreamBody := codexBody
			if useWebsocket {
				upstreamBody = stripResponsesImageGenerationTool(codexBody)
			}
			resp, reqErr = ExecuteRequest(upstreamCtx, account, upstreamBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		}
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			timedOut := ttftGuard.TimedOut()
			ttftGuard.Stop()
			if timedOut {
				reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
			}
			kind := classifyTransportFailure(reqErr)
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/chat/completions", account.ID(), attempt+1, durationMs, 0, logStatusUpstreamStreamBreak)
			}
			if useWebsocket && kind == upstreamErrorKindMessageTooBig {
				wsElapsed := time.Since(start)
				globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
				wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(reqErr.Error()))
				log.Printf("上游 WebSocket 1009，保留账号租约并降级 HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/chat/completions, ws_elapsed_ms=%d): %v", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), reqErr)
				continue
			}
			retryable := IsRetryableError(reqErr) || kind != ""
			shouldRetry := false
			if retryable {
				shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
			}
			// 传输类失败粘滞同号重试:不记账号失败、不解绑亲和、不硬排除(issue #331)
			// busy acquire 超时不粘滞同号：同 key 再等只会重复排队，直接换号（issue #413）
			stickyRetry := shouldRetry && !timedOut && kind != "" && kind != upstreamErrorKindWsBusyAcquire && h.stickyTransportRetryEnabled()
			if shouldPenalizeTransportKind(kind) && !(timedOut && shouldRetry) && !stickyRetry {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			if !stickyRetry {
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			}
			if timedOut && shouldRetry {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("上游首字超时，断开并重试 (attempt %d/%d, account %d, /v1/chat/completions): %v", attempt+1, maxRetries+1, account.ID(), reqErr)
				continue
			}
			if !timedOut && !stickyRetry {
				retryExclusions.MarkHard(account.ID())
			}

			// 不可重试的结构化错误直接返回
			if !retryable {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			if shouldRetry {
				if stickyRetry {
					log.Printf("传输错误粘滞重试：保留账号 %d 与会话亲和 (attempt %d/%d, /v1/chat/completions)", account.ID(), attempt+1, maxRetries+1)
				}
				if !h.waitBeforeRetry(c.Request.Context()) {
					return
				}
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
			if wsHTTPFallback.ForceHTTP() && !useWebsocket {
				wsHTTPFallback.LogHTTPAttemptCompletion("/v1/chat/completions", account.ID(), attempt+1, durationMs, 0, resp.StatusCode)
			}
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			retryExclusions.MarkHard(account.ID())

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			logUpstreamError("/v1/chat/completions", resp.StatusCode, logModel, account.ID(), errBody)
			promptPolicyIncidentID := acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/chat/completions", logModel, errBody, upstreamCyberPolicyAttempt{
				Transport: upstreamPromptPolicyTransport(isStream, useWebsocket), StatusCode: resp.StatusCode,
				AccountID: account.ID(), AttemptIndex: attempt + 1,
			}))
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, attemptEffectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, errBody, &generalRetries, &rateLimitRetries, maxRetries, h.effectiveMaxRateLimitRetries(account, maxRateLimitRetries))
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:              account.ID(),
				Endpoint:               "/v1/chat/completions",
				Model:                  logModel,
				EffectiveModel:         attemptLogEffectiveModel,
				StatusCode:             resp.StatusCode,
				DurationMs:             durationMs,
				ReasoningEffort:        reasoningEffort,
				InboundEndpoint:        "/v1/chat/completions",
				UpstreamEndpoint:       upstreamEndpoint,
				Stream:                 isStream,
				ViaWebsocket:           useWebsocket,
				ServiceTier:            usageTiers.ServiceTier,
				RequestedServiceTier:   usageTiers.RequestedServiceTier,
				ActualServiceTier:      usageTiers.ActualServiceTier,
				BillingServiceTier:     usageTiers.BillingServiceTier,
				IsRetryAttempt:         shouldRetry,
				AttemptIndex:           attempt + 1,
				UpstreamErrorKind:      upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:           usageLogErrorMessage(resp.StatusCode, errBody),
				PromptPolicyIncidentID: promptPolicyIncidentID,
			})

			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				if !h.waitBeforeRetry(c.Request.Context()) {
					return
				}
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		SyncCodexUsageState(h.store, account, resp)
		if isGrokNativeRouteResponse(resp) {
			usage, outcome, wroteAnyBody, firstTokenMs := forwardGrokNativeResponse(c, resp, GrokProtocolChatCompletions, isStream, start, ttftGuard.Stop)
			totalDuration := int(time.Since(start).Milliseconds())
			ttftGuard.Stop()
			resp.Body.Close()
			if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), nil) {
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				retryExclusions.MarkHard(account.ID())
				continue
			}
			if !wroteAnyBody && outcome.logStatusCode != http.StatusOK && c.Request.Context().Err() == nil {
				h.sendGrokNativeHTTPError(c, GrokProtocolChatCompletions, outcome)
			}
			logInput := &database.UsageLogInput{
				AccountID: account.ID(), Endpoint: "/v1/chat/completions", Model: logModel,
				EffectiveModel: attemptLogEffectiveModel, StatusCode: outcome.logStatusCode,
				DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
				InboundEndpoint: "/v1/chat/completions", UpstreamEndpoint: upstreamEndpoint,
				Stream: isStream, ViaWebsocket: false, AttemptIndex: attempt + 1,
			}
			if usage != nil {
				logInput.PromptTokens, logInput.CompletionTokens, logInput.TotalTokens = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
				logInput.InputTokens, logInput.OutputTokens = usage.InputTokens, usage.OutputTokens
				logInput.ReasoningTokens, logInput.CachedTokens = usage.ReasoningTokens, usage.CachedTokens
			}
			if outcome.logStatusCode != http.StatusOK {
				logInput.UpstreamErrorKind = outcome.failureKind
				logInput.ErrorMessage = usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage)
			}
			h.logUsageForRequest(c, logInput)
			if outcome.penalize {
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			} else if outcome.logStatusCode == http.StatusOK {
				h.store.ClearModelCooldown(account, attemptEffectiveModel)
				h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
			}
			if outcome.logStatusCode == http.StatusOK {
				h.store.ReleaseForSession(account, affinityKey)
			} else {
				h.store.Release(account)
			}
			return
		}

		// 成功！翻译响应 + TTFT 跟踪
		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", logModel)
		c.Set("x-reasoning-effort", reasoningEffort)
		var firstTokenMs int
		var usage *UsageInfo
		var actualServiceTier string
		ttftRecorded := false
		gotTerminal := false // 是否收到 response.completed 或 response.failed
		deltaCharCount := 0  // 累计 delta 字符数（用于断流时估算 token）
		var readErr error
		var writeErr error
		wroteAnyBody := false
		// 首 token 前收到不可重试的 response.failed 时置位:中止 SSE 转发、
		// 不做 transport flush(避免提前提交 200 header),循环外按真实错误码返回 JSON。
		abortedForHTTPError := false
		var compactResult []byte
		var terminalFailurePayload []byte
		promptPolicyIncidentID := ""
		upstreamCyberPolicyLogged := false

		chunkID := "chatcmpl-" + uuid.New().String()[:8]
		created := time.Now().Unix()

		if isStream {
			streamTranslator := NewStreamTranslator(chunkID, responseModel, created)
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")

			flusher, ok := c.Writer.(http.Flusher)
			if !ok {
				ttftGuard.Stop()
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "streaming not supported", "type": "server_error"},
				})
				resp.Body.Close()
				h.store.Release(account)
				return
			}
			streamWriter := h.newStreamFlushWriter(c, c.Writer, flusher)

			// clientGone：客户端写失败后置位，后续事件不再写客户端，
			// 但继续读上游直到 response.completed/failed，以拿到准确 usage。
			clientGone := false
			var pendingFirstTokenChunks bytes.Buffer
			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()
				if eventType == "response.failed" {
					statusCode := classifyResponseFailedOutcome(data).logStatusCode
					var incidentID string
					var logged bool
					data, incidentID, logged = h.attachUpstreamCyberPolicyStreamDecision(c, "/v1/chat/completions", logModel, data, upstreamCyberPolicyAttempt{
						Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: statusCode,
						AccountID: account.ID(), AttemptIndex: attempt + 1,
					})
					if logged {
						upstreamCyberPolicyLogged = true
						promptPolicyIncidentID = incidentID
						parsed = gjson.ParseBytes(data)
					}
				}
				translation := streamTranslator.TranslateParsedResult(parsed)
				chunk, done := translation.Chunk, translation.Terminal

				ttftGuard.MarkProgress(eventType)
				isFirstToken := isFirstTokenResultForMode(parsed, currentFirstTokenMode())
				if !ttftRecorded && isFirstToken {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				// 累计 delta 字符数（文本 + function call 参数）
				if eventType == "response.output_text.delta" || isCodexToolInputDeltaEvent(eventType) {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if isResponsesSuccessTerminalEvent(eventType) {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					gotTerminal = true
				}
				if eventType == "response.failed" {
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
				}

				if !clientGone && shouldSuppressRetryableResponseFailedBeforeFirstToken(eventType, terminalFailurePayload, ttftRecorded, wroteAnyBody, attempt, maxRetries, c.Request.Context().Err(), writeErr) {
					pendingFirstTokenChunks.Reset()
					return false
				}

				// 首 token 前的 response.failed 不写进下游流:不可重试(如 context_length_exceeded)
				// 或已达重试上限时,交由循环外按真实错误码返回,而不是 200 + [DONE] 让中转层误计费。
				if shouldReturnHTTPErrorForResponseFailed(eventType, ttftRecorded, wroteAnyBody, clientGone) {
					pendingFirstTokenChunks.Reset()
					abortedForHTTPError = true
					return false
				}

				if !clientGone && chunk != nil {
					shouldDefer := !ttftRecorded && !gotTerminal && isPreContentLifecycleEvent(eventType)
					wrote, err := writeDeferredSSEData(streamWriter, &pendingFirstTokenChunks, chunk, shouldDefer)
					if err != nil {
						writeErr = err
						clientGone = true
					} else if wrote {
						wroteAnyBody = true
					}
					if shouldDefer && !wrote {
						return !isResponsesTerminalEvent(eventType)
					}
				}
				if !clientGone && done {
					// response.failed is already represented by the translated stream
					// error chunk above. [DONE] is a successful Chat Completions
					// sentinel; appending it here would disguise a failed generation as
					// a clean terminal response to downstream gateways.
					if translation.Failed {
						writeErr = streamWriter.Flush()
						if writeErr != nil {
							clientGone = true
						} else {
							wroteAnyBody = true
						}
						return false
					}
					if pendingFirstTokenChunks.Len() > 0 {
						pendingFirstTokenChunks.WriteString("data: [DONE]\n\n")
						writeErr = streamWriter.WriteBytes(pendingFirstTokenChunks.Bytes())
						pendingFirstTokenChunks.Reset()
					} else {
						writeErr = streamWriter.WriteString("data: [DONE]\n\n")
					}
					if writeErr != nil {
						clientGone = true
					} else if err := streamWriter.Flush(); err != nil {
						writeErr = err
						clientGone = true
					} else {
						wroteAnyBody = true
					}
					if !clientGone {
						return false
					}
				}
				// 客户端断开后，要等到 terminal 事件才退出，确保拿到 usage。
				if gotTerminal {
					return false
				}
				return true
			})
			// 仅在真的写过 body 时才做收尾 flush:flusher.Flush 会先提交 HTTP 200 header,
			// 零写入时提前 flush 会让循环外的 c.JSON(4xx) 失效(status 已定型为 200)。
			if writeErr == nil && wroteAnyBody {
				writeErr = streamWriter.Flush()
			}
			// 已写正文后的上游断流：合成流内 error 对象（code=upstream_stream_break）
			// 且不补 [DONE]，让下游可编程识别失败并重试，而不是把截断流当成功
			// (issue #473)。
			if shouldWriteStreamBreakEvent(gotTerminal, wroteAnyBody, c.Request.Context().Err(), writeErr) {
				if err := writeChatCompletionsStreamBreakEvent(streamWriter); err != nil {
					log.Printf("写入合成 error 断流事件失败 (/v1/chat/completions): %v", err)
				}
			}
		} else {
			var fullContent strings.Builder
			var fullReasoning strings.Builder
			var toolCalls []ToolCallResult
			var finishReasonOverride string

			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()
				ttftGuard.MarkProgress(eventType)
				if !ttftRecorded && isFirstTokenResultForMode(parsed, currentFirstTokenMode()) {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}
				switch eventType {
				case "response.output_text.delta":
					delta := parsed.Get("delta").String()
					deltaCharCount += len(delta)
					fullContent.WriteString(delta)
				case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
					fullReasoning.WriteString(parsed.Get("delta").String())
				case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
					deltaCharCount += len(parsed.Get("delta").String())
				case "response.completed", "response.incomplete":
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					finishReasonOverride = responsesIncompleteFinishReason(eventType,
						parsed.Get("response.incomplete_details.reason").String())
					// 从 response.output 提取 function_call 项
					toolCalls = ExtractToolCallsFromOutput(data)
					gotTerminal = true
					return false
				case "response.failed":
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
					return false
				}
				return true
			})

			compactResult = BuildCompactResponseWithFinishReason(chunkID, responseModel, created, fullContent.String(), fullReasoning.String(), toolCalls, usage, finishReasonOverride)
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
			outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
		}
		ttftGuard.Stop()
		if outcome.verifyAccountAuth {
			h.store.VerifyAccountAuthAsync(account)
		}
		var responseFailedDecision codex429Decision
		if len(terminalFailurePayload) > 0 {
			outcome = classifyResponseFailedOutcome(terminalFailurePayload)
			responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, attemptEffectiveModel)
			if responseFailedDecision.Reason != "" {
				outcome.failureKind = upstreamErrorKind(outcome.logStatusCode, responseFailedErrorBody(terminalFailurePayload), responseFailedDecision)
			}
			// 流式 response.failed（HTTP 200）里的 cyber_policy 处罚也要记录，
			// 否则只有非 2xx 错误体才会被记入提示词过滤日志。
			if !upstreamCyberPolicyLogged {
				promptPolicyIncidentID = acceptedPromptPolicyIncidentID(h.logUpstreamCyberPolicy(c, "/v1/chat/completions", logModel, responseFailedErrorBody(terminalFailurePayload), upstreamCyberPolicyAttempt{
					Transport: upstreamPromptPolicyTransport(true, useWebsocket), StatusCode: outcome.logStatusCode,
					AccountID: account.ID(), AttemptIndex: attempt + 1,
				}))
			}
			if isExplicitUpstreamCyberPolicy(terminalFailurePayload) {
				outcome.failureMessage = upstreamCyberPolicyResponseMessage(c)
			}
		}
		if wsHTTPFallback.ForceHTTP() && !useWebsocket {
			wsHTTPFallback.LogHTTPAttemptCompletion("/v1/chat/completions", account.ID(), attempt+1, totalDuration, firstTokenMs, outcome.logStatusCode)
		}
		if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, useWebsocket, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			wsElapsed := time.Since(start)
			resp.Body.Close()
			globalWSSizeRouter.RecordMessageTooBig(len(codexBody))
			wsHTTPFallback.Retain(account, proxyURL, wsElapsed, websocketMessageTooBigSource(outcome.failureMessage))
			log.Printf("上游 WebSocket 1009，首包前保留账号租约并降级 HTTP (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=/v1/chat/completions, ws_elapsed_ms=%d): %s", wsHTTPFallback.ID(), wsHTTPFallback.Source(), attempt+1, account.ID(), wsElapsed.Milliseconds(), outcome.failureMessage)
			continue
		}
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			h.logPromptPolicyRetryUsage(c, database.UsageLogInput{
				AccountID: account.ID(), Endpoint: "/v1/chat/completions", Model: logModel, EffectiveModel: attemptLogEffectiveModel,
				StatusCode: outcome.logStatusCode, DurationMs: totalDuration, FirstTokenMs: firstTokenMs, ReasoningEffort: reasoningEffort,
				InboundEndpoint: "/v1/chat/completions", UpstreamEndpoint: upstreamEndpoint, Stream: isStream, ViaWebsocket: useWebsocket,
				AttemptIndex: attempt + 1, UpstreamErrorKind: outcome.failureKind,
				ErrorMessage: usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage),
			}, promptPolicyIncidentID)
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/chat/completions): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			if isFirstTokenTimeoutOutcome(outcome) {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
			} else {
				h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			}
			resp.Body.Close()
			h.store.Release(account)
			h.unbindOrRetainAffinityForCapacityShed(retryExclusions, affinityKey, account, outcome, capacityShedRetries)
			// 首字超时已白等一轮,不再叠加重试间隔;其余首包前断流按配置间隔等待
			if !isFirstTokenTimeoutOutcome(outcome) && !h.waitBeforeRetry(c.Request.Context()) {
				return
			}
			continue
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		logStatusCode := outcome.logStatusCode
		if outcome.logStatusCode != http.StatusOK {
			log.Printf("流异常结束 (account %d, /v1/chat/completions, status %d): %s，已转发约 %d 字符", account.ID(), outcome.logStatusCode, outcome.failureMessage, deltaCharCount)
			if deltaCharCount > 0 {
				estOutputTokens := deltaCharCount / 3
				if estOutputTokens < 1 {
					estOutputTokens = 1
				}
				usage = &UsageInfo{
					OutputTokens:     estOutputTokens,
					CompletionTokens: estOutputTokens,
					TotalTokens:      estOutputTokens,
				}
			}
		}
		if isStream && abortedForHTTPError && !wroteAnyBody {
			// 流式:首 token 前上游失败、未向下游写过任何内容,HTTP 200 header 尚未提交,
			// 覆盖预设的 SSE Content-Type 后按真实错误码返回 JSON,
			// 避免下游中转/计费方把它当成功并按预估 input token 计费(与回调内 reset 呼应)。
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(logStatusCode, gin.H{
				"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
			})
		} else if isStream && !wroteAnyBody && logStatusCode == logStatusUpstreamStreamBreak &&
			c.Request.Context().Err() == nil && writeErr == nil {
			// 首包前断流/首字超时且重试耗尽：原先没有任何写出分支命中，下游
			// 收到空 body 的"假 200"，失败完全不可感知 (issue #473)。598 是内部
			// 日志状态，对外按真实 502 + 稳定错误码返回，下游可编程识别并重试。
			c.Header("Content-Type", "application/json; charset=utf-8")
			c.JSON(http.StatusBadGateway, gin.H{
				"error": gin.H{"message": outcome.failureMessage, "type": ErrorTypeUpstreamError, "code": ErrorCodeUpstreamStreamBreak},
			})
		} else if !isStream {
			if len(terminalFailurePayload) > 0 {
				c.JSON(logStatusCode, gin.H{
					"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
				})
			} else if compactResult != nil {
				c.Data(http.StatusOK, "application/json", compactResult)
			} else {
				c.JSON(http.StatusBadGateway, gin.H{
					"error": gin.H{"message": "未收到完整的上游响应", "type": "upstream_error"},
				})
			}
		}

		usageTiers := resolveUsageServiceTiers(actualServiceTier, serviceTier)
		c.Set("x-service-tier", usageTiers.ServiceTier)

		logInput := &database.UsageLogInput{
			AccountID:              account.ID(),
			Endpoint:               "/v1/chat/completions",
			Model:                  logModel,
			EffectiveModel:         attemptLogEffectiveModel,
			StatusCode:             logStatusCode,
			DurationMs:             totalDuration,
			FirstTokenMs:           firstTokenMs,
			ReasoningEffort:        reasoningEffort,
			InboundEndpoint:        "/v1/chat/completions",
			UpstreamEndpoint:       upstreamEndpoint,
			Stream:                 isStream,
			ViaWebsocket:           useWebsocket,
			ServiceTier:            usageTiers.ServiceTier,
			RequestedServiceTier:   usageTiers.RequestedServiceTier,
			ActualServiceTier:      usageTiers.ActualServiceTier,
			BillingServiceTier:     usageTiers.BillingServiceTier,
			PromptPolicyIncidentID: promptPolicyIncidentID,
			AttemptIndex:           attempt + 1,
		}
		if logStatusCode != http.StatusOK {
			logInput.ErrorMessage = usageLogFailureMessage(logStatusCode, outcome.failureMessage)
			logInput.UpstreamErrorKind = outcome.failureKind
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		h.logUsageForRequest(c, logInput)

		resp.Body.Close()
		if outcome.penalize {
			recyclePooledClient(account, proxyURL)
			h.reportStreamOutcomeFailure(account, outcome, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ClearModelCooldown(account, attemptEffectiveModel)
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		if outcome.logStatusCode == http.StatusOK {
			h.store.ReleaseForSession(account, affinityKey)
		} else {
			h.store.Release(account)
		}
		return
	}
}

// handleStreamResponse 处理流式响应（翻译 Codex → OpenAI）
func (h *Handler) handleStreamResponse(c *gin.Context, body io.Reader, model, chunkID string, created int64) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"message": "streaming not supported", "type": "server_error"},
		})
		return
	}

	streamWriter := h.newStreamFlushWriter(c, c.Writer, flusher)
	err := ReadSSEStream(body, func(data []byte) bool {
		translation := TranslateStreamChunkResult(data, model, chunkID, created)
		if translation.Chunk != nil {
			if err := streamWriter.WriteSSEData(translation.Chunk); err != nil {
				return false
			}
		}
		if translation.Terminal {
			if !translation.Failed {
				if err := streamWriter.WriteString("data: [DONE]\n\n"); err != nil {
					return false
				}
			}
			_ = streamWriter.Flush()
			return false
		}
		return true
	})
	_ = streamWriter.Flush()

	if err != nil {
		log.Printf("读取上游流失败: %v", err)
	}
}

// handleCompactResponse 处理非流式响应
func (h *Handler) handleCompactResponse(c *gin.Context, body io.Reader, model, chunkID string, created int64) {
	var fullContent strings.Builder
	var fullReasoning strings.Builder
	var usage *UsageInfo

	_ = ReadSSEStream(body, func(data []byte) bool {
		eventType := gjson.GetBytes(data, "type").String()
		switch eventType {
		case "response.output_text.delta":
			delta := gjson.GetBytes(data, "delta").String()
			fullContent.WriteString(delta)
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			fullReasoning.WriteString(gjson.GetBytes(data, "delta").String())
		case "response.completed":
			usage = extractUsage(data)
			return false
		case "response.failed":
			return false
		}
		return true
	})

	result := BuildCompactResponse(chunkID, model, created, fullContent.String(), fullReasoning.String(), nil, usage)

	c.Data(http.StatusOK, "application/json", result)
}

// ==================== 通用辅助 ====================

// parseRetryAfter 解析上游 429 响应中的重试时间（参考 CLIProxyAPI codex_executor.go:689-708）
func parseRetryAfter(body []byte) time.Duration {
	if len(body) == 0 {
		return 2 * time.Minute
	}

	// 解析 error.resets_at (Unix timestamp)
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(time.Now()) {
			d := time.Until(resetTime)
			if d > 0 {
				return d
			}
		}
	}

	// 解析 error.resets_in_seconds
	if secs := gjson.GetBytes(body, "error.resets_in_seconds").Int(); secs > 0 {
		return time.Duration(secs) * time.Second
	}

	// 默认 2 分钟
	return 2 * time.Minute
}

func isMissingScopeUnauthorized(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()))
	if code != "missing_scope" {
		return false
	}

	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	if strings.Contains(msg, "api.responses.write") {
		return true
	}

	return strings.Contains(msg, "scope")
}

func parseRetryAfterResetAt(body []byte, now time.Time) (time.Time, bool) {
	if len(body) == 0 {
		return time.Time{}, false
	}

	if resetsAt := firstGJSONInt(body, "error.resets_at", "response.error.resets_at", "response.status_details.error.resets_at"); resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(now) {
			return resetTime, true
		}
	}

	if secs := firstGJSONInt(body, "error.resets_in_seconds", "response.error.resets_in_seconds", "response.status_details.error.resets_in_seconds"); secs > 0 {
		return now.Add(time.Duration(secs) * time.Second), true
	}

	return time.Time{}, false
}

func parseUsageLimitResetAt(body []byte, now time.Time) (time.Time, bool) {
	if !IsUsageLimitReachedError(body) {
		return time.Time{}, false
	}
	return parseRetryAfterResetAt(body, now)
}

// IsCodexModelUnsupportedError 是 isCodexModelUnsupportedError 的导出包装，
// 供管理端模型探测复用同一套"账号不支持该模型"识别逻辑。
func IsCodexModelUnsupportedError(body []byte) bool {
	return isCodexModelUnsupportedError(body)
}

// isCodexModelUnsupportedError 判断 400 响应是否为"当前账号不支持该模型"。
// 该错误由账号套餐权益决定而非请求内容，换到支持该模型的账号即可成功，
// 因此按 (账号, 模型) 维度冷却并换号重试，而不是原样透传给客户端（issue #408）。
func isCodexModelUnsupportedError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	candidates := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "message").String(),
		string(body),
	}
	for _, candidate := range candidates {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if lower == "" {
			continue
		}
		if strings.Contains(lower, "model is not supported when using codex") {
			return true
		}
		if strings.Contains(lower, "unknown provider for model") {
			return true
		}
	}
	return false
}

func isCodexModelCapacityError(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	candidates := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "message").String(),
		string(body),
	}
	for _, candidate := range candidates {
		lower := strings.ToLower(strings.TrimSpace(candidate))
		if lower == "" {
			continue
		}
		if strings.Contains(lower, "selected model is at capacity") ||
			strings.Contains(lower, "model is at capacity. please try a different model") {
			return true
		}
	}
	return false
}

func codexWindowType(windowMinutes float64) codexRateLimitWindow {
	switch {
	case windowMinutes >= 1440:
		return codexRateLimitWindow7d
	case windowMinutes >= 60:
		return codexRateLimitWindow5h
	case windowMinutes > 0:
		return codexRateLimitWindowShort
	default:
		return codexRateLimitWindowUnknown
	}
}

type codexWindowUsage struct {
	usedPct   float64
	resetSec  float64
	windowMin float64
	valid     bool
}

func parseCodexWindowUsage(usedStr, windowStr, resetStr string) codexWindowUsage {
	if strings.TrimSpace(usedStr) == "" || strings.TrimSpace(windowStr) == "" {
		return codexWindowUsage{}
	}
	var usedPct, windowMin, resetSec float64
	if _, err := fmt.Sscanf(usedStr, "%f", &usedPct); err != nil {
		return codexWindowUsage{}
	}
	if _, err := fmt.Sscanf(windowStr, "%f", &windowMin); err != nil || windowMin <= 0 {
		return codexWindowUsage{}
	}
	if strings.TrimSpace(resetStr) != "" {
		if _, err := fmt.Sscanf(resetStr, "%f", &resetSec); err != nil {
			resetSec = 0
		}
	}
	return codexWindowUsage{
		usedPct:   usedPct,
		windowMin: windowMin,
		resetSec:  resetSec,
		valid:     true,
	}
}

func classifyCodex429Window(resp *http.Response, now time.Time) (codexRateLimitWindow, time.Time, bool) {
	if resp == nil {
		return codexRateLimitWindowUnknown, time.Time{}, false
	}

	primary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-primary-used-percent"),
		resp.Header.Get("x-codex-primary-window-minutes"),
		resp.Header.Get("x-codex-primary-reset-after-seconds"),
	)
	secondary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-secondary-used-percent"),
		resp.Header.Get("x-codex-secondary-window-minutes"),
		resp.Header.Get("x-codex-secondary-reset-after-seconds"),
	)

	var exhausted []codexWindowUsage
	if primary.valid && primary.usedPct >= 100 {
		exhausted = append(exhausted, primary)
	}
	if secondary.valid && secondary.usedPct >= 100 {
		exhausted = append(exhausted, secondary)
	}
	if len(exhausted) == 0 {
		return codexRateLimitWindowUnknown, time.Time{}, false
	}

	chosen := exhausted[0]
	for _, candidate := range exhausted[1:] {
		if candidate.windowMin > chosen.windowMin {
			chosen = candidate
		}
	}

	var resetAt time.Time
	if chosen.resetSec > 0 {
		resetAt = now.Add(time.Duration(chosen.resetSec) * time.Second)
	}
	return codexWindowType(chosen.windowMin), resetAt, !resetAt.IsZero()
}

func responseHasCodex5hHeaders(resp *http.Response) bool {
	if resp == nil {
		return false
	}

	primary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-primary-used-percent"),
		resp.Header.Get("x-codex-primary-window-minutes"),
		resp.Header.Get("x-codex-primary-reset-after-seconds"),
	)
	if primary.valid && codexWindowType(primary.windowMin) == codexRateLimitWindow5h {
		return true
	}

	secondary := parseCodexWindowUsage(
		resp.Header.Get("x-codex-secondary-used-percent"),
		resp.Header.Get("x-codex-secondary-window-minutes"),
		resp.Header.Get("x-codex-secondary-reset-after-seconds"),
	)
	return secondary.valid && codexWindowType(secondary.windowMin) == codexRateLimitWindow5h
}

func classify429RateLimit(account *auth.Account, body []byte, resp *http.Response, now time.Time, model string) codex429Decision {
	if IsUsageLimitReachedError(body) {
		if resetAt, ok := parseUsageLimitResetAt(body, now); ok {
			reason := "usage_limit"
			if account != nil && account.IsPremium5hPlan() && responseHasCodex5hHeaders(resp) {
				reason = "rate_limited_5h"
			}
			return codex429Decision{
				Scope:    rateLimitScopeAccount,
				Reason:   reason,
				ResetAt:  resetAt,
				Cooldown: resetAt.Sub(now),
			}
		}

		windowType, resetAt, hasWindowReset := classifyCodex429Window(resp, now)
		switch windowType {
		case codexRateLimitWindow5h:
			if !hasWindowReset {
				resetAt = now.Add(5 * time.Hour)
			}
			return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited_5h", ResetAt: resetAt, Cooldown: resetAt.Sub(now)}
		case codexRateLimitWindow7d:
			if !hasWindowReset {
				resetAt = now.Add(7 * 24 * time.Hour)
			}
			return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited_7d", ResetAt: resetAt, Cooldown: resetAt.Sub(now)}
		}

		cooldown := usageLimitFallbackCooldown(account, body)
		resetAt = now.Add(cooldown)
		return codex429Decision{Scope: rateLimitScopeAccount, Reason: "usage_limit", ResetAt: resetAt, Cooldown: cooldown}
	}

	windowType, resetAt, hasWindowReset := classifyCodex429Window(resp, now)
	switch windowType {
	case codexRateLimitWindow5h:
		if !hasWindowReset {
			resetAt = now.Add(5 * time.Hour)
		}
		return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited_5h", ResetAt: resetAt, Cooldown: resetAt.Sub(now)}
	case codexRateLimitWindow7d:
		if !hasWindowReset {
			resetAt = now.Add(7 * 24 * time.Hour)
		}
		return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited_7d", ResetAt: resetAt, Cooldown: resetAt.Sub(now)}
	}

	model = strings.TrimSpace(model)
	if model != "" {
		reason := "rate_limited_model"
		if isCodexModelCapacityError(body) {
			reason = "model_capacity"
		}
		return codex429Decision{
			Scope:    rateLimitScopeModel,
			Reason:   reason,
			Model:    model,
			Cooldown: 5 * time.Minute,
		}
	}

	cooldown := 5 * time.Minute
	resetAt = now.Add(cooldown)
	return codex429Decision{Scope: rateLimitScopeAccount, Reason: "rate_limited", ResetAt: resetAt, Cooldown: cooldown}
}

func usageLimitFallbackCooldown(account *auth.Account, body []byte) time.Duration {
	planType := ""
	if details, ok := parseUsageLimitDetails(body); ok {
		planType = details.planType
	}
	if planType == "" && account != nil {
		planType = account.GetPlanType()
	}
	switch auth.NormalizePlanType(planType) {
	case "free":
		return 7 * 24 * time.Hour
	default:
		return 5 * time.Hour
	}
}

// Apply429Cooldown 统一处理 429 对账号状态的影响。
func Apply429Cooldown(store *auth.Store, account *auth.Account, body []byte, resp *http.Response, model string) codex429Decision {
	// Grok 上游的 429 语义（免费额度耗尽/超支限制/Retry-After）与 Codex 不同，且需要落
	// grok_free_quota 权威快照——批量测试/连通性测试也走这里，必须同样路由到 Grok 专用映射，
	// 否则免费额度耗尽会被误标 rate_limited 且丢失用量快照。
	if account != nil && account.IsGrokAPI() {
		return applyGrokCooldown(store, account, http.StatusTooManyRequests, body, resp, model)
	}
	// OpenAI Responses/API relay 是直连上游，不是 OAuth 订阅账号。裸 429 通常只是
	// 瞬时负载抑制，默认不应写入 Redis/DB 并把整个模型摘掉 5~30 分钟。
	if account != nil && account.IsRelayStyle() {
		reason := "rate_limited_model"
		if isCodexModelCapacityError(body) {
			reason = "model_capacity"
		}
		decision := codex429Decision{
			Scope:  rateLimitScopeModel,
			Reason: reason,
			Model:  strings.TrimSpace(model),
		}
		if store == nil || decision.Model == "" {
			return decision
		}
		policy := store.ResolveModelCooldownPolicy(account)
		if policy.Mode == database.ModelCooldownModeOff || policy.Seconds <= 0 {
			return decision
		}
		backoff := policy.Mode == database.ModelCooldownModeAdaptive && policy.BackoffEnabled
		cooldown := store.MarkModelCooldownWithBackoff(
			account,
			decision.Model,
			time.Duration(policy.Seconds)*time.Second,
			decision.Reason,
			backoff,
		)
		decision.ResetAt = cooldown.ResetAt
		decision.Cooldown = time.Until(cooldown.ResetAt)
		return decision
	}
	decision := classify429RateLimit(account, body, resp, time.Now(), model)
	if store == nil || account == nil {
		return decision
	}
	if details, ok := parseUsageLimitDetails(body); ok {
		store.ApplyUsageLimitMetadata(account, details.planType, decision.ResetAt)
	}
	if decision.Scope == rateLimitScopeModel {
		policy := store.ResolveModelCooldownPolicy(account)
		if policy.Mode == database.ModelCooldownModeOff || policy.Seconds <= 0 {
			decision.ResetAt = time.Time{}
			decision.Cooldown = 0
			return decision
		}
		backoff := policy.Mode == database.ModelCooldownModeAdaptive && policy.BackoffEnabled
		cooldown := store.MarkModelCooldownWithBackoff(
			account,
			decision.Model,
			time.Duration(policy.Seconds)*time.Second,
			decision.Reason,
			backoff,
		)
		decision.ResetAt = cooldown.ResetAt
		decision.Cooldown = time.Until(cooldown.ResetAt)
		return decision
	}
	if isProOnlyModel(model) && IsUsageLimitReachedError(body) && decision.Scope == rateLimitScopeAccount {
		store.MarkSparkUsageExhausted(account, decision.ResetAt)
		return decision
	}
	if account.IsPremium5hPlan() && decision.Scope == rateLimitScopeAccount && decision.Reason == "rate_limited_5h" {
		store.MarkResponsesPremium5hRateLimited(account, decision.ResetAt)
		return decision
	}
	store.MarkResponsesRateLimited(account, decision.Cooldown)
	return decision
}

// applyCooldown 根据上游状态码设置智能冷却
func (h *Handler) applyCooldown(account *auth.Account, statusCode int, body []byte, resp *http.Response) {
	h.applyCooldownForModel(account, statusCode, body, resp, "")
}

func (h *Handler) applyCooldownForModel(account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	// Grok 上游的错误语义与 Codex 不同（免费额度耗尽/超支限制/Retry-After），单独映射。
	if account.IsGrokAPI() {
		return h.applyGrokCooldownForModel(account, statusCode, body, resp, model)
	}
	if IsUsageLimitReachedError(body) {
		decision := Apply429Cooldown(h.store, account, body, resp, model)
		log.Printf("账号 %d 触发用量上限 (status=%d, plan=%s, reason=%s)，冷却到 %s", account.ID(), statusCode, account.GetPlanType(), decision.Reason, decision.ResetAt.Format(time.RFC3339))
		return decision
	}
	switch statusCode {
	case http.StatusBadRequest:
		// 账号套餐不支持该模型：按 (账号, 模型) 冷却，调度器随后会跳过该组合；
		// 其余 400 属请求内容问题，不动账号状态。
		if model != "" && isCodexModelUnsupportedError(body) {
			cooldown := h.store.MarkModelCooldown(account, model, 30*time.Minute, "model_not_supported")
			log.Printf("账号 %d (plan=%s) 不支持模型 %s，该模型冷却到 %s", account.ID(), account.GetPlanType(), model, cooldown.ResetAt.Format(time.RFC3339))
			return codex429Decision{
				Scope:    rateLimitScopeModel,
				Reason:   "model_not_supported",
				Model:    model,
				ResetAt:  cooldown.ResetAt,
				Cooldown: time.Until(cooldown.ResetAt),
			}
		}
		return codex429Decision{}
	case http.StatusTooManyRequests:
		decision := Apply429Cooldown(h.store, account, body, resp, model)
		if decision.Scope == rateLimitScopeModel {
			if decision.ResetAt.IsZero() {
				log.Printf("账号 %d 模型 %s 触发短时限流 (reason=%s)，按策略不持久化冷却", account.ID(), decision.Model, decision.Reason)
			} else {
				log.Printf("账号 %d 模型 %s 触发短时限流 (reason=%s)，冷却到 %s", account.ID(), decision.Model, decision.Reason, decision.ResetAt.Format(time.RFC3339))
			}
			return decision
		}
		log.Printf("账号 %d 被限速 (plan=%s, reason=%s)，冷却到 %s", account.ID(), account.GetPlanType(), decision.Reason, decision.ResetAt.Format(time.RFC3339))
		return decision
	case http.StatusUnauthorized:
		// 原子标志瞬间置位，阻止其他并发请求再选到该账号
		atomic.StoreInt32(&account.Disabled, 1)

		if isMissingScopeUnauthorized(body) {
			log.Printf("账号 %d 收到 missing_scope 401，保留在号池", account.ID())
			atomic.StoreInt32(&account.Disabled, 0)
			return codex429Decision{}
		}

		if h.store.GetAutoCleanUnauthorized() {
			// 开启自动清理时，401 立即从号池删除
			log.Printf("账号 %d 收到 401，立即清理", account.ID())
			if h.db != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = h.db.SetError(ctx, account.ID(), "deleted")
				cancel()
				h.db.InsertAccountEventAsync(account.ID(), "deleted", "auto_clean_401")
			}
			h.store.RemoveAccount(account.ID())
		} else {
			h.store.MarkCooldown(account, 5*time.Minute, "unauthorized")
		}
	case http.StatusPaymentRequired, http.StatusForbidden:
		if statusCode == http.StatusForbidden && IsAgentRuntimeDeletedError(body) {
			atomic.StoreInt32(&account.Disabled, 1)
			errorMsg := upstreamAccountErrorMessage(statusCode, body)
			log.Printf("账号 %d 的 Agent runtime 已删除，标记为封禁", account.ID())
			h.store.MarkCooldownWithErrorExactDuration(account, 24*time.Hour, "unauthorized", errorMsg)
			return codex429Decision{}
		}
		if IsDeactivatedWorkspaceError(body) {
			log.Printf("账号 %d 工作区已停用，标记为错误", account.ID())
			if h.store != nil {
				h.store.MarkDeactivatedWorkspace(account, upstreamAccountErrorMessage(statusCode, body))
			}
			return codex429Decision{}
		}
		h.store.MarkCooldown(account, 30*time.Minute, "payment_required")
	}
	return codex429Decision{}
}

// compute429Cooldown 根据计划类型和 Codex 响应精确计算 429 冷却时间
func (h *Handler) compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
	return compute429Cooldown(account, body, resp)
}

func compute429Cooldown(account *auth.Account, body []byte, resp *http.Response) time.Duration {
	// 1. 优先使用 Codex 响应体中的精确重置时间
	if resetDuration := parseRetryAfter(body); resetDuration > 2*time.Minute {
		// parseRetryAfter 默认返回 2min（无数据），超过 2min 说明解析到了真实的 resets_at/resets_in_seconds
		if resetDuration > 7*24*time.Hour {
			resetDuration = 7 * 24 * time.Hour // 最多 7 天
		}
		return resetDuration
	}

	// 2. 没有精确重置时间，根据套餐类型 + 用量窗口推断
	planType := auth.NormalizePlanType(account.GetPlanType())

	switch planType {
	case "free":
		// Free 只有 7d 窗口，429 = 额度耗尽，冷却 7 天
		return 7 * 24 * time.Hour

	case "team", "teamplus", "pro", "plus", "enterprise", "k12", "edu", "education":
		// Team/Pro/Plus 及教育版(k12/edu)有 5h + 7d 双窗口，需要判断是哪个窗口触发了限制
		return detectTeamCooldownWindow(resp)

	default:
		// 未知套餐，保守默认 5 小时
		return 5 * time.Hour
	}
}

// detectTeamCooldownWindow 通过响应头判断 Team/Pro/Plus 账号是哪个窗口触发的限制
func (h *Handler) detectTeamCooldownWindow(resp *http.Response) time.Duration {
	return detectTeamCooldownWindow(resp)
}

func detectTeamCooldownWindow(resp *http.Response) time.Duration {
	if resp == nil {
		return 5 * time.Hour // 保守默认
	}

	// Codex 返回两组窗口头：primary 和 secondary
	// x-codex-primary-window-minutes / x-codex-primary-used-percent
	// x-codex-secondary-window-minutes / x-codex-secondary-used-percent
	// 用量 >= 100% 的窗口就是触发限制的窗口

	primaryUsed := parseFloat(resp.Header.Get("x-codex-primary-used-percent"))
	primaryWindowMin := parseFloat(resp.Header.Get("x-codex-primary-window-minutes"))
	secondaryUsed := parseFloat(resp.Header.Get("x-codex-secondary-used-percent"))
	secondaryWindowMin := parseFloat(resp.Header.Get("x-codex-secondary-window-minutes"))

	// 找到 used >= 100% 的窗口
	primaryExhausted := primaryUsed >= 100
	secondaryExhausted := secondaryUsed >= 100

	switch {
	case primaryExhausted && secondaryExhausted:
		// 两个窗口都满了，取较大窗口的冷却时间
		return windowMinutesToCooldown(max(primaryWindowMin, secondaryWindowMin))
	case primaryExhausted:
		return windowMinutesToCooldown(primaryWindowMin)
	case secondaryExhausted:
		return windowMinutesToCooldown(secondaryWindowMin)
	default:
		// 都没满但还是 429，可能是短时 burst 限制
		return 5 * time.Hour
	}
}

// windowMinutesToCooldown 根据窗口分钟数决定冷却时长
func windowMinutesToCooldown(windowMinutes float64) time.Duration {
	switch {
	case windowMinutes >= 1440: // >= 1 天 → 7d 窗口
		return 7 * 24 * time.Hour
	case windowMinutes >= 60: // >= 1 小时 → 5h 窗口
		return 5 * time.Hour
	default:
		return 30 * time.Minute // 短窗口
	}
}

// SyncCodexUsageState 解析 Codex 响应头并完成 7d / 5h 快照持久化与 premium 5h 提前限流。
func SyncCodexUsageState(store *auth.Store, account *auth.Account, resp *http.Response) CodexUsageSyncResult {
	result := CodexUsageSyncResult{}
	if account == nil || resp == nil {
		return result
	}
	observedAt := time.Now()
	if store != nil {
		planHeader := resp.Header.Get("x-codex-plan-type")
		store.UpdateAccountPlanType(account, planHeader)
		// 权威付费 plan_type 与「订阅已过期」互斥，借每次响应校正陈旧到期时间。(issue #360)
		if planHeader != "" {
			store.ClearStaleSubscriptionExpiresAt(account)
		}
	}
	result.UsageWindowLimitsIgnored = account.SkipsUsageWindowLimits()

	observation := parseCodexUsageHeaderObservation(resp)
	result.Used5hHeaders = observation.w5h.valid
	usageApplied := false
	if observation.authoritative {
		usageApplied = account.ApplyUsageObservation(observedAt, func() {
			parsed := applyCodexUsageHeaderObservation(store, account, observation, observedAt)
			result.UsagePct7d = parsed.usagePct7d
			result.HasUsage7d = parsed.hasUsage7d
			result.Cleared5h = parsed.cleared5h
			if store == nil {
				return
			}
			if result.HasUsage7d {
				store.PersistUsageSnapshot(account, result.UsagePct7d)
				if result.UsagePct7d >= 100 {
					result.Usage7dRateLimited = store.MarkUsage7dRateLimited(account)
				}
			} else if result.Used5hHeaders {
				store.PersistUsageSnapshot5hOnly(account)
				result.Persisted5hOnly = true
			}
		})
	}

	result.UsagePct5h, result.Reset5hAt, result.HasUsage5h = account.GetUsageSnapshot5h()
	if usageApplied && store != nil && result.HasUsage5h {
		// 被动 /responses 头刷新了 5h 窗口重置时刻：武装「到点即探」，窗口翻新即刷新进度条。
		store.WakeBoundaryProbe(result.Reset5hAt)
	}
	if usageApplied && result.Used5hHeaders && account.IsPremium5hPlan() && result.HasUsage5h && result.UsagePct5h >= 100 && !account.SkipsUsageWindowLimits() {
		if store != nil {
			result.Premium5hRateLimited = store.MarkPremium5hRateLimitedAt(account, result.Reset5hAt, observedAt)
		} else {
			result.Premium5hRateLimited = true
		}
	}

	return result
}

type codexUsageHeaderParseResult struct {
	usagePct7d float64
	hasUsage7d bool
	cleared5h  bool
}

type codexUsageHeaderObservation struct {
	w5h           codexWindowUsage
	w7d           codexWindowUsage
	authoritative bool
}

// parseCodexUsageHeaders 从 Codex 响应头解析 5h/7d 用量百分比
func parseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	if resp == nil || account == nil {
		return 0, false
	}
	observedAt := time.Now()
	observation := parseCodexUsageHeaderObservation(resp)
	if !observation.authoritative {
		return 0, false
	}
	parsed := codexUsageHeaderParseResult{}
	account.ApplyUsageObservation(observedAt, func() {
		parsed = applyCodexUsageHeaderObservation(nil, account, observation, observedAt)
	})
	return parsed.usagePct7d, parsed.hasUsage7d
}

// parseCodexUsageHeaderObservation classifies only windows with a positive,
// recognizable duration. Used-percent-only partial headers are not authoritative
// evidence that the optional 5h window disappeared.
func parseCodexUsageHeaderObservation(resp *http.Response) codexUsageHeaderObservation {
	out := codexUsageHeaderObservation{}
	if resp == nil {
		return out
	}

	// 解析 primary 和 secondary 窗口
	primaryUsedStr := resp.Header.Get("x-codex-primary-used-percent")
	primaryWindowStr := resp.Header.Get("x-codex-primary-window-minutes")
	primaryResetStr := resp.Header.Get("x-codex-primary-reset-after-seconds")
	secondaryUsedStr := resp.Header.Get("x-codex-secondary-used-percent")
	secondaryWindowStr := resp.Header.Get("x-codex-secondary-window-minutes")
	secondaryResetStr := resp.Header.Get("x-codex-secondary-reset-after-seconds")

	primary := parseCodexWindowUsage(primaryUsedStr, primaryWindowStr, primaryResetStr)
	secondary := parseCodexWindowUsage(secondaryUsedStr, secondaryWindowStr, secondaryResetStr)

	for _, window := range []codexWindowUsage{primary, secondary} {
		if !window.valid {
			continue
		}
		switch codexWindowType(window.windowMin) {
		case codexRateLimitWindow5h:
			if !out.w5h.valid {
				out.w5h = window
			}
		case codexRateLimitWindow7d:
			if !out.w7d.valid {
				out.w7d = window
			}
		}
	}
	out.authoritative = out.w5h.valid || out.w7d.valid
	return out
}

func applyCodexUsageHeaderObservation(store *auth.Store, account *auth.Account, observation codexUsageHeaderObservation, observedAt time.Time) codexUsageHeaderParseResult {
	out := codexUsageHeaderParseResult{}
	if observation.w5h.valid {
		resetAt := observedAt.Add(time.Duration(observation.w5h.resetSec) * time.Second)
		account.SetUsageSnapshot5hAt(observation.w5h.usedPct, resetAt, observedAt)
	} else if observation.authoritative {
		out.cleared5h = store.ClearAbsentUsageSnapshot5hAt(account, observedAt)
	}

	if observation.w7d.valid {
		resetAt := observedAt.Add(time.Duration(observation.w7d.resetSec) * time.Second)
		account.SetReset7dAt(resetAt)
		account.SetWindow7dSeconds(int64(observation.w7d.windowMin * 60))
		account.SetUsagePercent7d(observation.w7d.usedPct)
		out.usagePct7d = observation.w7d.usedPct
		out.hasUsage7d = true
	}
	return out
}

// ParseCodexUsageHeaders 从响应头提取并更新账号用量信息
func ParseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	return parseCodexUsageHeaders(resp, account)
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v := 0.0
	fmt.Sscanf(s, "%f", &v)
	return v
}

// sendUpstreamError 发送上游错误响应给客户端
func (h *Handler) sendUpstreamError(c *gin.Context, statusCode int, body []byte) {
	if isExplicitUpstreamCyberPolicy(body) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": upstreamCyberPolicyResponseMessage(c),
				"type":    "upstream_error",
				"code":    newAPIUpstreamCyberPolicyReasonCode,
			},
		})
		return
	}
	message := usageLogErrorMessage(statusCode, body)
	if message == "" || message == fmt.Sprintf("HTTP %d", statusCode) {
		message = fmt.Sprintf("Upstream returned status %d", statusCode)
	}
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "upstream_error",
			"code":    fmt.Sprintf("upstream_%d", statusCode),
		},
	})
}

// sendFinalUpstreamError 重试用尽后的最终错误响应：识别 usage_limit_reached 改写为 503，其余透传
func (h *Handler) sendFinalUpstreamError(c *gin.Context, statusCode int, body []byte) {
	if details, ok := parseUsageLimitDetails(body); ok {
		if details.resetsInSeconds > 0 {
			c.Header("Retry-After", fmt.Sprintf("%d", details.resetsInSeconds))
		}

		message := "账号池额度已耗尽，请稍后重试"
		if details.message != "" {
			message = fmt.Sprintf("%s：%s", message, details.message)
		}

		errInfo := gin.H{
			"message": message,
			"type":    "server_error",
			"code":    "account_pool_usage_limit_reached",
		}
		if details.planType != "" {
			errInfo["plan_type"] = details.planType
		}
		if details.resetsAt != 0 {
			errInfo["resets_at"] = details.resetsAt
		}
		if details.resetsInSeconds != 0 {
			errInfo["resets_in_seconds"] = details.resetsInSeconds
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errInfo})
		return
	}

	// 上游账号 401（OAuth token 失效/撤销）是账号侧问题，不是下游客户端 key 无效。
	// 若原样以 401 透传，客户端会误判自己的凭证失效（issue #323）。改写为 503 池级
	// 错误，用独立 code/type 与客户端鉴权失败（invalid_api_key）明确区分。
	if statusCode == http.StatusUnauthorized && !isMissingScopeUnauthorized(body) {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "账号池暂无可用账号（上游账号鉴权失效），请稍后重试",
				"type":    "server_error",
				"code":    "account_pool_unauthorized",
			},
		})
		return
	}

	// 402 工作区停用（deactivated_workspace）：重试已换过号仍拿到它说明池内暂无
	// 可服务账号，与 403 一样改写为 503 池级错误（带 Retry-After 提示退避）。
	// 文案末尾附上游原始错误体，便于下游直接看到封禁原因。坏账号已被标错隔离，
	// 稍后重试可落到健康账号。裸 402 保持原样：可能携带用量/计费语义，上面已单独处理。
	if statusCode == http.StatusPaymentRequired && IsDeactivatedWorkspaceError(body) {
		if c.Writer.Header().Get("Retry-After") == "" {
			c.Header("Retry-After", "30")
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": deactivatedPoolErrorMessage(strings.TrimSpace(string(body))),
				"type":    "server_error",
				"code":    "account_pool_deactivated",
			},
		})
		return
	}

	// 上游账号 403（payment_required / deactivated_workspace / codex_access_restricted）
	// 同样是账号侧问题：重试已换过号仍拿到 403 说明池内暂无可用账号。原样透传 403 会让
	// 客户端（如 Claude Code）误判为自身无权限而直接停工（issue #396），改写为 503 池级错误。
	if statusCode == http.StatusForbidden {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "账号池暂无可用账号（上游账号被拒绝访问：额度/套餐或工作区受限），请稍后重试",
				"type":    "server_error",
				"code":    "account_pool_forbidden",
			},
		})
		return
	}

	if statusCode == http.StatusTooManyRequests && c.Writer.Header().Get("Retry-After") == "" {
		// 上游偶发 429 在重试池耗尽后仍应以标准限流语义返回，不能伪装成
		// no_available_account/503。没有明确 reset 信息时给客户端一个最小退避提示。
		c.Header("Retry-After", "1")
	}

	h.sendUpstreamError(c, statusCode, body)
}

// handleUpstreamError 统一处理上游错误（兼容旧调用）
func (h *Handler) handleUpstreamError(c *gin.Context, account *auth.Account, statusCode int, body []byte) {
	h.applyCooldown(account, statusCode, body, nil)
	h.sendUpstreamError(c, statusCode, body)
}

// ListModels 列出可用模型
// listModelsOrManifest 按客户端形态分发模型列表：带 client_version 查询参数的是
// Codex 客户端在刷新模型选单（期望 manifest 格式，解析失败会静默冻结在本地缓存），
// 其余客户端返回 OpenAI 兼容列表。
func (h *Handler) listModelsOrManifest(c *gin.Context) {
	if strings.TrimSpace(c.Query("client_version")) != "" {
		h.CodexModelsManifestHandler(c)
		return
	}
	h.ListModels(c)
}

func (h *Handler) ListModels(c *gin.Context) {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	// Authenticated routes always carry the complete APIKeyRow. Keep the
	// historical global list only for direct/internal calls that bypass the
	// middleware (including older embedders); a real key always gets an
	// isolated, read-only account snapshot.
	if row := apiKeyRowFromContext(c); row != nil {
		api.SendList(c, "list", h.scopedModels(ctx, row))
		return
	}
	modelIDs := h.supportedModelIDs(ctx)
	models := make([]api.Model, 0, len(modelIDs))
	for _, id := range modelIDs {
		models = append(models, api.Model{
			ID:      id,
			Object:  "model",
			Created: modelCompatibilityCreatedUnix,
			OwnedBy: "openai",
		})
	}
	api.SendList(c, "list", models)
}

func (h *Handler) supportedModelIDs(ctx context.Context) []string {
	models := SupportedModelIDs(ctx, h.db)
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		seen[strings.ToLower(strings.TrimSpace(model))] = struct{}{}
	}
	if h != nil && h.store != nil {
		for _, account := range h.store.Accounts() {
			declared := account.OpenAIResponsesModels()
			// 未声明 models 白名单的 Grok 账号：补默认 Grok 模型集，让 grok-4.5 等
			// 出现在 /v1/models（否则下游客户端拉不到可用的 Grok 模型名）。
			if len(declared) == 0 && account.IsGrokAPI() {
				declared = DefaultGrokModelIDsForAccount(account)
			}
			// Grok 账号额外补媒体模型集(生图/生视频走独立准入,不受文本白名单约束)。
			if account.IsGrokAPI() {
				declared = append(append([]string{}, declared...), grokMediaModelsForAccount(account)...)
			}
			for _, model := range declared {
				key := strings.ToLower(strings.TrimSpace(model))
				if key == "" {
					continue
				}
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				models = append(models, model)
			}
			for _, alias := range accountModelMappingAliases(account) {
				key := strings.ToLower(strings.TrimSpace(alias))
				if key == "" {
					continue
				}
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				models = append(models, alias)
			}
		}
		// 全局模型映射的 from 键（Claude 模型映射：claude-* → gpt-*，用于 /v1/messages）
		// 也要出现在 /v1/models，否则下游客户端拉不到可用的 Claude 模型名。
		// 仅列非通配的显式映射键；通配规则（含 *）不作为具体模型暴露。
		for _, rule := range parseModelMappingRules(h.store.GetModelMapping()) {
			if rule.Wildcard {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(rule.From))
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, rule.From)
		}
	}
	return models
}
