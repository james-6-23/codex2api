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
	apiKeyGate   *apiKeyConcurrencyLimiter
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
	if h == nil || h.store == nil {
		return nil, ""
	}
	return h.store.NextForSessionWithFilter(sessionID, apiKeyID, exclude, filter)
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
	if proxyURL := strings.TrimSpace(stickyProxyURL); proxyURL != "" {
		return proxyURL
	}
	if h == nil || h.store == nil {
		return ""
	}
	return h.store.ResolveProxyForAccount(account)
}

type usageLimitDetails struct {
	message         string
	planType        string
	resetsAt        int64
	resetsInSeconds int64
}

type CodexUsageSyncResult struct {
	UsagePct7d           float64
	HasUsage7d           bool
	Usage7dRateLimited   bool
	UsagePct5h           float64
	Reset5hAt            time.Time
	HasUsage5h           bool
	Used5hHeaders        bool
	Persisted5hOnly      bool
	Premium5hRateLimited bool
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
		if account.IsOpenAIResponsesAPI() {
			return false
		}
		if model != "" && account.IsModelRateLimited(model) {
			return false
		}
		if isProOnlyModel(model) {
			return isSparkPlanCandidate(account.GetPlanType())
		}
		return true
	}
}

func accountFilterForResponsesModel(model string, allowCodexAccounts bool) auth.AccountFilter {
	model = strings.TrimSpace(model)
	codexFilter := accountFilterForModel(model)
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		if account.IsOpenAIResponsesAPI() {
			return account.SupportsOpenAIResponsesModel(model) && (model == "" || !account.IsModelRateLimited(model))
		}
		if !allowCodexAccounts {
			return false
		}
		return codexFilter(account)
	}
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

func noAvailableAnthropicAccountMessage(model string) string {
	if isProOnlyModel(model) {
		return "No available paid or unknown-plan account for gpt-5.3-codex-spark"
	}
	return "No available accounts, please retry later"
}

// NewHandler 创建处理器
func NewHandler(store *auth.Store, db *database.DB, cfg *config.Config, deviceCfg *DeviceProfileConfig) *Handler {
	return &Handler{
		store:      store,
		configKeys: make(map[string]bool), // 不再使用硬编码，但保留结构以向后兼容逻辑
		db:         db,
		cfg:        cfg,
		deviceCfg:  deviceCfg,
		apiKeyGate: newAPIKeyConcurrencyLimiter(),
	}
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

func (h *Handler) resolveAPIKey(key string) (*database.APIKeyRow, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, false
	}
	if h.configKeys[key] {
		return &database.APIKeyRow{
			ID:   0,
			Name: "config",
			Key:  key,
		}, true
	}
	if row, ok := h.resolveAPIKeyFromRuntimeCache(key); ok {
		h.syncAPIKeyAllowedGroups(row)
		return row, true
	}
	if h.db == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	row, err := h.db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("查询 API Key 失败: %v", err)
		}
		return nil, false
	}
	h.setAPIKeyRuntimeCache(row)
	h.syncAPIKeyAllowedGroups(row)
	return row, true
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
}

// isValidKey 检查 key 是否有效（配置文件 + DB）
func (h *Handler) isValidKey(key string) bool {
	_, ok := h.resolveAPIKey(key)
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
	_ = h.db.InsertUsageLog(context.Background(), input)
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

func (h *Handler) logUsageForRequest(c *gin.Context, input *database.UsageLogInput) {
	populateAPIKeyMetaFromContext(c, input)
	populateClientIPFromRequest(c, input)
	populateCompactUsageMetaFromRequest(c, input)
	markCyberPolicyUsageKind(input)
	h.logUsage(input)
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
	if input == nil || input.Compact {
		return
	}
	if isCompactUsageEndpoint(input.Endpoint) || isCompactUsageEndpoint(input.InboundEndpoint) || isCompactUsageEndpoint(input.UpstreamEndpoint) {
		input.Compact = true
		return
	}
	if c == nil {
		return
	}
	if body, ok := rawRequestBodyFromContext(c); ok && requestBodyHasCompactionInput(body) {
		input.Compact = true
	}
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

func setRawRequestBody(c *gin.Context, body []byte) {
	if c != nil {
		c.Set("raw_body", body)
	}
}

func requestBodyHasCompactionInput(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	return gjsonResultHasCompactionInput(input)
}

func gjsonResultHasCompactionInput(result gjson.Result) bool {
	if !result.Exists() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(result.Get("type").String())) {
	case "compaction", "compaction_trigger", "context_compaction":
		return true
	}
	if !result.IsArray() && !result.IsObject() {
		return false
	}
	found := false
	result.ForEach(func(_, value gjson.Result) bool {
		if gjsonResultHasCompactionInput(value) {
			found = true
			return false
		}
		return true
	})
	return found
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

// extractServiceTier 从请求体提取服务等级
func extractServiceTier(body []byte) string {
	if tier := gjson.GetBytes(body, "service_tier").String(); tier != "" {
		return tier
	}
	return gjson.GetBytes(body, "serviceTier").String()
}

const upstreamErrorKindMessageTooBig = "message_too_big"

func isWebsocketMessageTooBigError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "message too big") || strings.Contains(msg, "close 1009")
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
	return outcome.penalize
}

func classifyTransportFailure(err error) string {
	if err == nil {
		return ""
	}

	if isWebsocketMessageTooBigError(err) {
		return upstreamErrorKindMessageTooBig
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
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
		return streamOutcome{
			logStatusCode:  logStatusUpstreamStreamBreak,
			failureKind:    kind,
			failureMessage: fmt.Sprintf("上游流读取失败: %v", readErr),
			penalize:       true,
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
	if strings.TrimSpace(message) == "" || message == fmt.Sprintf("HTTP %d", statusCode) {
		message = "上游返回 response.failed"
	}
	kind := upstreamErrorKind(statusCode, payload, codex429Decision{})
	if kind == "" {
		if statusCode >= 500 {
			kind = "server"
		} else {
			kind = "client"
		}
	}
	return streamOutcome{
		logStatusCode:  statusCode,
		failureKind:    kind,
		failureMessage: message,
		penalize:       statusCode == http.StatusUnauthorized || statusCode == http.StatusTooManyRequests || statusCode >= 500,
	}
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
	v1.POST("/chat/completions", h.ChatCompletions)
	v1.POST("/responses", h.Responses)
	v1.GET("/responses", h.ResponsesWebSocket)
	v1.POST("/responses/compact", h.ResponsesCompact)
	v1.POST("/images/generations", h.ImagesGenerations)
	v1.POST("/images/edits", h.ImagesEdits)
	v1.POST("/messages", h.Messages)
	v1.POST("/messages/count_tokens", h.CountTokens)
	v1.POST("/responses/input_tokens", h.ResponsesInputTokens)
	v1.GET("/models", h.ListModels)

	// 无前缀路由（兼容 base_url 已包含 /v1 的客户端）
	r.POST("/chat/completions", auth, h.ChatCompletions)
	r.POST("/responses", auth, h.Responses)
	r.GET("/responses", auth, h.ResponsesWebSocket)
	r.POST("/responses/compact", auth, h.ResponsesCompact)
	r.POST("/images/generations", auth, h.ImagesGenerations)
	r.POST("/images/edits", auth, h.ImagesEdits)
	r.POST("/messages", auth, h.Messages)
	r.POST("/messages/count_tokens", auth, h.CountTokens)
	r.POST("/responses/input_tokens", auth, h.ResponsesInputTokens)
	r.GET("/models", auth, h.ListModels)

	codexDirect := r.Group("/backend-api/codex")
	codexDirect.Use(auth)
	codexDirect.POST("/responses", h.Responses)
	codexDirect.GET("/responses", h.ResponsesWebSocket)
	codexDirect.POST("/responses/*subpath", func(c *gin.Context) {
		subpath := strings.TrimSpace(c.Param("subpath"))
		if subpath == "/compact" || strings.HasPrefix(subpath, "/compact/") {
			h.ResponsesCompact(c)
			return
		}
		h.Responses(c)
	})
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
		apiKeyRow, ok := h.resolveAPIKey(key)
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
		c.Next()
	}
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

const (
	logStatusClientClosed        = 499
	logStatusUpstreamStreamBreak = 598
)

// isRetryableStatus 检查是否可重试的上游状态码
func isRetryableStatus(code int) bool {
	return code == http.StatusServiceUnavailable || code == http.StatusUnauthorized || code == http.StatusInternalServerError
}

func shouldRetryHTTPStatus(statusCode int, generalRetries *int, rateLimitRetries *int, maxGeneralRetries, maxRateLimitRetries int) bool {
	if statusCode == http.StatusTooManyRequests {
		if rateLimitRetries == nil || *rateLimitRetries >= maxRateLimitRetries {
			return false
		}
		*rateLimitRetries++
		return true
	}
	if !isRetryableStatus(statusCode) {
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

func IsDeactivatedWorkspaceError(body []byte) bool {
	for _, path := range []string{"detail.code", "error.code", "code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if code == "deactivated_workspace" {
			return true
		}
	}
	return strings.Contains(strings.ToLower(string(body)), "deactivated_workspace")
}

func upstreamAccountErrorMessage(statusCode int, body []byte) string {
	if IsDeactivatedWorkspaceError(body) {
		return fmt.Sprintf("上游返回 %d: deactivated_workspace", statusCode)
	}
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(body, "detail.message").String())
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
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
	case http.StatusPaymentRequired, http.StatusForbidden:
		if IsDeactivatedWorkspaceError(body) {
			return "deactivated_workspace"
		}
		return "payment_required"
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
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}

	supportedModels := h.supportedModelIDs(c.Request.Context())
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredModelMappingToBody(rawBody, supportedModels)
	setRawRequestBody(c, rawBody)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRulesForModel(mappedModel)
	rules["model"] = append(rules["model"], api.ModelValidator(supportedModels))
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
	if probeCapture, handled := h.prepareProbeShortCircuit(c, rawBody, "/v1/responses", logModel, gjson.GetBytes(rawBody, "stream").Bool(), "responses"); handled {
		return
	} else if probeCapture != nil {
		defer probeCapture.finish()
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
	sessionID := ResolveSessionID(c.Request.Header, rawBody)
	explicitSessionID := ResolveExplicitSessionID(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(explicitSessionID, apiKeyID)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	// 2. 准备 Codex 上游请求体（Unmarshal→map→Marshal，一次序列化）。
	// OpenAI Responses relay body 仅在实际命中 relay 账号时惰性生成，避免 Codex 路径重复转换。
	codexBody, expandedInputRaw := PrepareResponsesBody(rawBody)
	var openAIResponsesBody []byte
	resetOpenAIResponsesBody := func() {
		openAIResponsesBody = nil
	}
	getOpenAIResponsesBody := func() []byte {
		if openAIResponsesBody == nil {
			openAIResponsesBody = PrepareOpenAIResponsesBody(rawBody)
		}
		return openAIResponsesBody
	}
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
	accountFilter := accountFilterForResponsesModel(effectiveModel, modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db)))
	accountFilter = h.withModelCooldownFilter(effectiveModel, accountFilter)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	forceHTTPAfterWSMessageTooBig := false
	invalidEncryptedContentRetried := false

	// 上游 ctx 生命周期：每次 attempt 开始前用新的 drainable ctx 替换，
	// defer 兜底确保函数退出时上游被释放。
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	for attempt := 0; ; attempt++ {
		account, stickyProxyURL := h.nextRetryAccountForSession(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter)
		if account == nil {
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(effectiveModel))
			return
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		useWebsocket := h.shouldUseWebsocketForHTTP() && !forceHTTPAfterWSMessageTooBig
		// 生图请求强制走 HTTP：WebSocket 传输大体积图片数据会卡死（issue #220）；
		// 自然语言生图意图也需保留 image_generation 工具（issue #288）。
		if useWebsocket && rawResponsesBodyShouldForceHTTPForImageGeneration(rawBody) {
			useWebsocket = false
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

		if account.IsOpenAIResponsesAPI() {
			if lastUpstreamCancel != nil {
				lastUpstreamCancel()
			}
			upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
			lastUpstreamCancel = upstreamCancel
			ttftGuard := (*firstTokenTimeoutGuard)(nil)
			if isStream {
				ttftGuard = newFirstTokenTimeoutGuard(currentFirstTokenTimeout(), upstreamCancel)
			}
			stopTTFTGuard := func() {
				if ttftGuard != nil {
					ttftGuard.Stop()
				}
			}
			ttftTimedOut := func() bool {
				return ttftGuard != nil && ttftGuard.TimedOut()
			}
			baseURL, _ := account.OpenAIResponsesCredentials()
			upstreamEndpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/responses")
			resp, reqErr := ExecuteOpenAIResponsesRequest(upstreamCtx, account, getOpenAIResponsesBody(), proxyURL, downstreamHeaders)
			durationMs := int(time.Since(start).Milliseconds())

			if reqErr != nil {
				timedOut := ttftTimedOut()
				stopTTFTGuard()
				if timedOut {
					reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
				}
				kind := classifyTransportFailure(reqErr)
				retryable := IsRetryableError(reqErr) || kind != ""
				shouldRetry := false
				if retryable {
					shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
				}
				if kind != "" && !(timedOut && shouldRetry) {
					h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
				}
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				if timedOut && shouldRetry {
					retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
					log.Printf("OpenAI Responses 上游首字超时，断开并重试 (attempt %d/%d, account %d): %v", attempt+1, maxRetries+1, account.ID(), reqErr)
					continue
				}
				if !timedOut {
					retryExclusions.MarkHard(account.ID())
				}

				if !retryable {
					ErrorToGinResponse(c, reqErr)
					return
				}

				log.Printf("OpenAI Responses 上游请求失败 (attempt %d): %v", attempt+1, reqErr)
				if shouldRetry {
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
				h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, errBody)
				decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
				shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
				usageTiers := resolveUsageServiceTiers("", serviceTier)
				h.logUsageForRequest(c, &database.UsageLogInput{
					AccountID:            account.ID(),
					Endpoint:             "/v1/responses",
					Model:                logModel,
					EffectiveModel:       logEffectiveModel,
					StatusCode:           resp.StatusCode,
					DurationMs:           durationMs,
					ReasoningEffort:      reasoningEffort,
					InboundEndpoint:      "/v1/responses",
					UpstreamEndpoint:     upstreamEndpoint,
					Stream:               isStream,
					ViaWebsocket:         useWebsocket,
					ServiceTier:          usageTiers.ServiceTier,
					RequestedServiceTier: usageTiers.RequestedServiceTier,
					ActualServiceTier:    usageTiers.ActualServiceTier,
					BillingServiceTier:   usageTiers.BillingServiceTier,
					IsRetryAttempt:       shouldRetry,
					AttemptIndex:         attempt + 1,
					UpstreamErrorKind:    upstreamErrorKind(resp.StatusCode, errBody, decision),
					ErrorMessage:         usageLogErrorMessage(resp.StatusCode, errBody),
				})

				if shouldRetry {
					lastStatusCode = resp.StatusCode
					lastBody = errBody
					continue
				}

				h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
				return
			}

			c.Set("x-account-email", baseURL)
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
			var imageLogInfo imageUsageLogInfo
			var terminalFailurePayload []byte

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
				streamWriter := newStreamFlushWriter(c.Writer, flusher)
				clientGone := false
				var pendingFirstTokenEvents bytes.Buffer
				readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
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
					if eventType == "response.completed" {
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
						pendingFirstTokenEvents.Reset()
						return false
					}
					if image, ok := extractImageFromOutputItemDone(data, logModel); ok {
						imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
					}
					if !clientGone {
						shouldDefer := !ttftRecorded && !gotTerminal && isPreContentLifecycleEvent(eventType)
						wrote, err := writeDeferredSSEData(streamWriter, &pendingFirstTokenEvents, data, shouldDefer)
						if err != nil {
							writeErr = err
							clientGone = true
						} else if wrote {
							wroteAnyBody = true
						}
					}
					return eventType != "response.completed" && eventType != "response.failed"
				})
				if writeErr == nil {
					writeErr = streamWriter.Flush()
				}
			} else {
				var respBody []byte
				respBody, readErr = io.ReadAll(resp.Body)
				if readErr == nil {
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
			ttftGuard.Stop()
			var responseFailedDecision codex429Decision
			if len(terminalFailurePayload) > 0 {
				outcome = classifyResponseFailedOutcome(terminalFailurePayload)
				responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, effectiveModel)
				if responseFailedDecision.Reason != "" {
					outcome.failureKind = upstreamErrorKind(outcome.logStatusCode, responseFailedErrorBody(terminalFailurePayload), responseFailedDecision)
				}
				// 流式 response.failed（HTTP 200）里的 cyber_policy 处罚也要记录，
				// 否则只有非 2xx 错误体才会被记入提示词过滤日志。
				h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, responseFailedErrorBody(terminalFailurePayload))
			}
			if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
				log.Printf("OpenAI Responses 上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
				recyclePooledClient(account, proxyURL)
				if isFirstTokenTimeoutOutcome(outcome) {
					retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				} else {
					h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
				}
				resp.Body.Close()
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				continue
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
				AccountID:            account.ID(),
				Endpoint:             "/v1/responses",
				Model:                logModel,
				EffectiveModel:       logEffectiveModel,
				StatusCode:           outcome.logStatusCode,
				DurationMs:           totalDuration,
				FirstTokenMs:         firstTokenMs,
				ReasoningEffort:      reasoningEffort,
				InboundEndpoint:      "/v1/responses",
				UpstreamEndpoint:     upstreamEndpoint,
				Stream:               isStream,
				ViaWebsocket:         useWebsocket,
				ServiceTier:          usageTiers.ServiceTier,
				RequestedServiceTier: usageTiers.RequestedServiceTier,
				ActualServiceTier:    usageTiers.ActualServiceTier,
				BillingServiceTier:   usageTiers.BillingServiceTier,
			}
			if outcome.logStatusCode != http.StatusOK {
				logInput.ErrorMessage = usageLogErrorMessage(outcome.logStatusCode, []byte(outcome.failureMessage))
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
				h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
			} else if outcome.logStatusCode == http.StatusOK {
				h.store.ClearModelCooldown(account, effectiveModel)
				h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
			}
			h.store.Release(account)
			return
		}

		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionID, explicitSessionID, useWebsocket)
		// 上游使用与客户端解耦的 context：客户端中途断开时仍能继续读完
		// response.completed 拿到 usage（流式计费的关键）。
		// lastUpstreamCancel 在 attempt loop 顶部声明 + defer 兜底，
		// 这里覆盖前先 cancel 上一轮（重试时）。
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(currentFirstTokenTimeout(), upstreamCancel)
		// WebSocket 上游下剥离自动注入的图片工具，防止模型自主生图产生大体积
		// 数据卡死 WS 流（issue #220）。显式生图请求已在上面强制走 HTTP。
		upstreamBody := codexBody
		if useWebsocket {
			upstreamBody = stripResponsesImageGenerationTool(codexBody)
		}
		resp, reqErr := ExecuteRequest(upstreamCtx, account, upstreamBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders, useWebsocket)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			timedOut := ttftGuard.TimedOut()
			ttftGuard.Stop()
			if timedOut {
				reqErr = firstTokenTimeoutError(currentFirstTokenTimeout())
			}
			kind := classifyTransportFailure(reqErr)
			if useWebsocket && kind == upstreamErrorKindMessageTooBig {
				log.Printf("上游 WebSocket 请求帧过大，自动降级 HTTP 重试 (attempt %d, account %d, /v1/responses): %v", attempt+1, account.ID(), reqErr)
				forceHTTPAfterWSMessageTooBig = true
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				continue
			}
			retryable := IsRetryableError(reqErr) || kind != ""
			shouldRetry := false
			if retryable {
				shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
			}
			if kind != "" && !(timedOut && shouldRetry) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if timedOut && shouldRetry {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("上游首字超时，断开并重试 (attempt %d/%d, account %d, /v1/responses): %v", attempt+1, maxRetries+1, account.ID(), reqErr)
				continue
			}
			if !timedOut {
				retryExclusions.MarkHard(account.ID())
			}

			// 不可重试的结构化错误直接返回
			if !retryable {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			if shouldRetry {
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
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
					log.Printf("上游拒绝 encrypted_content，已移除加密 reasoning 上下文并重试一次 (attempt %d)", attempt+1)
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
			retryExclusions.MarkHard(account.ID())

			log.Printf("上游返回错误 (attempt %d, status %d): %s", attempt+1, resp.StatusCode, upstreamErrorConsoleBody(errBody))
			logUpstreamError("/v1/responses", resp.StatusCode, logModel, account.ID(), errBody)
			h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:            account.ID(),
				Endpoint:             "/v1/responses",
				Model:                logModel,
				EffectiveModel:       logEffectiveModel,
				StatusCode:           resp.StatusCode,
				DurationMs:           durationMs,
				ReasoningEffort:      reasoningEffort,
				InboundEndpoint:      "/v1/responses",
				UpstreamEndpoint:     "/v1/responses",
				Stream:               isStream,
				ViaWebsocket:         useWebsocket,
				ServiceTier:          usageTiers.ServiceTier,
				RequestedServiceTier: usageTiers.RequestedServiceTier,
				ActualServiceTier:    usageTiers.ActualServiceTier,
				BillingServiceTier:   usageTiers.BillingServiceTier,
				IsRetryAttempt:       shouldRetry,
				AttemptIndex:         attempt + 1,
				UpstreamErrorKind:    upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:         usageLogErrorMessage(resp.StatusCode, errBody),
			})

			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

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
		var responseJSON []byte
		var imageLogInfo imageUsageLogInfo
		var terminalFailurePayload []byte

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
			streamWriter := newStreamFlushWriter(c.Writer, flusher)

			// clientGone：客户端写失败后置位，后续事件不再写客户端，
			// 但继续读上游直到 response.completed/failed，以拿到准确 usage。
			clientGone := false
			var pendingFirstTokenEvents bytes.Buffer
			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				eventType := parsed.Get("type").String()

				// TTFT: 记录第一个实际内容事件的时间
				ttftGuard.MarkProgress(eventType)
				isFirstToken := isFirstTokenResultForMode(parsed, currentFirstTokenMode())
				if !ttftRecorded && isFirstToken {
					firstTokenMs = int(time.Since(start).Milliseconds())
					ttftRecorded = true
				}

				// 累计 delta 字符数
				if eventType == "response.output_text.delta" {
					deltaCharCount += len(parsed.Get("delta").String())
				}
				if image, ok := extractImageFromOutputItemDone(data, logModel); ok {
					imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
				}

				// 提取 usage + service_tier
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 缓存响应上下文，供后续 previous_response_id 展开使用
					cacheCompletedResponse([]byte(expandedInputRaw), data)
					gotTerminal = true
				}
				if eventType == "response.failed" {
					terminalFailurePayload = append([]byte(nil), data...)
					gotTerminal = true
				}

				if !clientGone && shouldSuppressRetryableResponseFailedBeforeFirstToken(eventType, terminalFailurePayload, ttftRecorded, wroteAnyBody, attempt, maxRetries, c.Request.Context().Err(), writeErr) {
					pendingFirstTokenEvents.Reset()
					return false
				}

				if !clientGone {
					shouldDefer := !ttftRecorded && !gotTerminal && isPreContentLifecycleEvent(eventType)
					wrote, err := writeDeferredSSEData(streamWriter, &pendingFirstTokenEvents, data, shouldDefer)
					if err != nil {
						writeErr = err
						clientGone = true
					} else if wrote {
						wroteAnyBody = true
					}
				}
				return eventType != "response.completed" && eventType != "response.failed"
			})
			if writeErr == nil {
				writeErr = streamWriter.Flush()
			}
		} else {
			// 非流式收集
			var lastResponseData []byte
			outputItems := make([]json.RawMessage, 0, 2)
			seenOutputItems := make(map[string]struct{})
			imageOutputs := make([]json.RawMessage, 0, 1)
			seenImageOutputs := make(map[string]struct{})
			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
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
				if eventType == "response.completed" {
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
					// 缓存响应上下文，供后续 previous_response_id 展开使用
					cacheCompletedResponse([]byte(expandedInputRaw), data)
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
		ttftGuard.Stop()
		var responseFailedDecision codex429Decision
		if len(terminalFailurePayload) > 0 {
			outcome = classifyResponseFailedOutcome(terminalFailurePayload)
			responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, effectiveModel)
			if responseFailedDecision.Reason != "" {
				outcome.failureKind = upstreamErrorKind(outcome.logStatusCode, responseFailedErrorBody(terminalFailurePayload), responseFailedDecision)
			}
			// 流式 response.failed（HTTP 200）里的 cyber_policy 处罚也要记录，
			// 否则只有非 2xx 错误体才会被记入提示词过滤日志。
			h.logUpstreamCyberPolicy(c, "/v1/responses", logModel, responseFailedErrorBody(terminalFailurePayload))
		}
		if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, useWebsocket, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游 WebSocket 消息过大，首包前自动降级 HTTP 重试 (attempt %d, account %d, /v1/responses): %s", attempt+1, account.ID(), outcome.failureMessage)
			forceHTTPAfterWSMessageTooBig = true
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			continue
		}
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/responses): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			if isFirstTokenTimeoutOutcome(outcome) {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
			} else {
				h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			}
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			continue
		}

		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		logStatusCode := outcome.logStatusCode
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
		if !isStream {
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
			AccountID:            account.ID(),
			Endpoint:             "/v1/responses",
			Model:                logModel,
			EffectiveModel:       logEffectiveModel,
			StatusCode:           logStatusCode,
			DurationMs:           totalDuration,
			FirstTokenMs:         firstTokenMs,
			ReasoningEffort:      reasoningEffort,
			InboundEndpoint:      "/v1/responses",
			UpstreamEndpoint:     "/v1/responses",
			Stream:               isStream,
			ViaWebsocket:         useWebsocket,
			ServiceTier:          usageTiers.ServiceTier,
			RequestedServiceTier: usageTiers.RequestedServiceTier,
			ActualServiceTier:    usageTiers.ActualServiceTier,
			BillingServiceTier:   usageTiers.BillingServiceTier,
		}
		if logStatusCode != http.StatusOK {
			logInput.ErrorMessage = usageLogErrorMessage(logStatusCode, []byte(outcome.failureMessage))
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
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ClearModelCooldown(account, effectiveModel)
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
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

	supportedModels := h.supportedModelIDs(c.Request.Context())
	// newapi 等聚合网关会给 compact 请求追加 -openai-compact 后缀（如 gpt-5.4-openai-compact）。
	// 在映射与校验之前剥除后缀，让 newapi 渠道保持该命名，而内部按基础模型 gpt-5.4 处理并转发上游。
	rawBody, _, _ = stripCompactModelSuffixFromBody(rawBody)
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredModelMappingToBody(rawBody, supportedModels)
	setRawRequestBody(c, rawBody)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ResponsesAPIValidationRulesForModel(mappedModel)
	rules["model"] = append(rules["model"], api.ModelValidator(supportedModels))
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
	logModel := requestModel
	if logModel == "" {
		logModel = model
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
	if isImageOnlyModel(model) {
		sendImageOnlyModelError(c, model)
		return
	}
	if probeCapture, handled := h.prepareProbeShortCircuit(c, rawBody, "/v1/responses/compact", logModel, false, "responses_compact"); handled {
		return
	} else if probeCapture != nil {
		defer probeCapture.finish()
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/responses/compact", model) {
		return
	}

	rawBody = normalizeServiceTierField(rawBody)
	if err := ValidateResponsesFunctionNames(rawBody); err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidParameter, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	sessionID := ResolveSessionID(c.Request.Header, rawBody)
	explicitSessionID := ResolveExplicitSessionID(c.Request.Header, rawBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(explicitSessionID, apiKeyID)
	reasoningEffort := extractReasoningEffort(rawBody)
	serviceTier := extractServiceTier(rawBody)
	if serviceTier != "" {
		c.Set("x-service-tier", resolveServiceTier("", serviceTier))
	}

	// compact 强制非流式
	rawBody, _ = sjson.SetBytes(rawBody, "stream", false)

	// 准备上游请求体
	codexBody, _ := PrepareCompactResponsesBody(rawBody)
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
	accountFilter := accountFilterForResponsesModel(effectiveModel, modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db)))
	accountFilter = h.withModelCooldownFilter(effectiveModel, accountFilter)

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

	for attempt := 0; ; attempt++ {
		account, stickyProxyURL := h.nextAccountForSessionWithFilter(affinityKey, apiKeyID, excludeAccounts, accountFilter)
		if account == nil {
			account, stickyProxyURL = h.store.WaitForSessionAvailableWithFilter(c.Request.Context(), affinityKey, 30*time.Second, apiKeyID, excludeAccounts, accountFilter)
			if account == nil {
				if (lastStatusCode == http.StatusTooManyRequests || lastStatusCode == http.StatusBadGateway) && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(effectiveModel))
				return
			}
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)

		apiKey := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		apiKey = strings.TrimSpace(apiKey)
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}
		downstreamHeaders := c.Request.Header.Clone()

		if account.IsOpenAIResponsesAPI() {
			baseURL, _ := account.OpenAIResponsesCredentials()
			upstreamEndpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/responses/compact")
			resp, reqErr := ExecuteOpenAIResponsesCompactRequest(c.Request.Context(), account, openAIResponsesBody, proxyURL, downstreamHeaders)
			durationMs := int(time.Since(start).Milliseconds())

			if reqErr != nil {
				if kind := classifyTransportFailure(reqErr); kind != "" {
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
				h.logUpstreamCyberPolicy(c, "/v1/responses/compact", logModel, errBody)
				decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
				shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
				usageTiers := resolveUsageServiceTiers("", serviceTier)
				h.logUsageForRequest(c, &database.UsageLogInput{
					AccountID:            account.ID(),
					Endpoint:             "/v1/responses/compact",
					Model:                logModel,
					EffectiveModel:       logEffectiveModel,
					StatusCode:           resp.StatusCode,
					DurationMs:           durationMs,
					ReasoningEffort:      reasoningEffort,
					InboundEndpoint:      "/v1/responses/compact",
					UpstreamEndpoint:     upstreamEndpoint,
					ServiceTier:          usageTiers.ServiceTier,
					RequestedServiceTier: usageTiers.RequestedServiceTier,
					ActualServiceTier:    usageTiers.ActualServiceTier,
					BillingServiceTier:   usageTiers.BillingServiceTier,
					IsRetryAttempt:       shouldRetry,
					AttemptIndex:         attempt + 1,
					UpstreamErrorKind:    upstreamErrorKind(resp.StatusCode, errBody, decision),
					ErrorMessage:         usageLogErrorMessage(resp.StatusCode, errBody),
				})

				if shouldRetry {
					lastStatusCode = resp.StatusCode
					lastBody = errBody
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
					EffectiveModel:       logEffectiveModel,
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
					lastBody = []byte(fmt.Sprintf("Failed to read upstream response: %v", readErr))
					continue
				}
				api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeUpstreamError, "Failed to read upstream response", api.ErrorTypeUpstream), http.StatusBadGateway)
				return
			}

			h.store.ClearModelCooldown(account, effectiveModel)
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
				EffectiveModel:       logEffectiveModel,
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

			h.store.Release(account)
			contentType := resp.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "application/json"
			}
			c.Data(http.StatusOK, contentType, respBody)
			return
		}

		// compact（会话压缩续写）刻意保留确定性 IsolateCodexSessionID、不走 resolveUpstreamSessionID
		// 的默认隔离：压缩本身是对同一会话的延续，需要稳定的 prompt_cache_key 维持缓存连续性。
		upstreamSessionID := IsolateCodexSessionID(apiKeyID, sessionID)
		resp, reqErr := ExecuteCompactRequest(c.Request.Context(), account, codexBody, upstreamSessionID, proxyURL, apiKey, deviceCfg, downstreamHeaders)
		durationMs := int(time.Since(start).Milliseconds())

		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
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
			h.logUpstreamCyberPolicy(c, "/v1/responses/compact", logModel, errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:            account.ID(),
				Endpoint:             "/v1/responses/compact",
				Model:                logModel,
				EffectiveModel:       logEffectiveModel,
				StatusCode:           resp.StatusCode,
				DurationMs:           durationMs,
				ReasoningEffort:      reasoningEffort,
				InboundEndpoint:      "/v1/responses/compact",
				UpstreamEndpoint:     "/v1/responses/compact",
				ServiceTier:          usageTiers.ServiceTier,
				RequestedServiceTier: usageTiers.RequestedServiceTier,
				ActualServiceTier:    usageTiers.ActualServiceTier,
				BillingServiceTier:   usageTiers.BillingServiceTier,
				IsRetryAttempt:       shouldRetry,
				AttemptIndex:         attempt + 1,
				UpstreamErrorKind:    upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:         usageLogErrorMessage(resp.StatusCode, errBody),
			})

			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		// 成功：直接透传响应体
		respBody, readErr := io.ReadAll(resp.Body)
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
				UpstreamEndpoint:     "/v1/responses/compact",
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
				lastBody = []byte(fmt.Sprintf("Failed to read upstream response: %v", readErr))
				continue
			}
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeUpstreamError, "Failed to read upstream response", api.ErrorTypeUpstream), http.StatusBadGateway)
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
			UpstreamEndpoint:     "/v1/responses/compact",
			ServiceTier:          usageTiers.ServiceTier,
			RequestedServiceTier: usageTiers.RequestedServiceTier,
			ActualServiceTier:    usageTiers.ActualServiceTier,
			BillingServiceTier:   usageTiers.BillingServiceTier,
		})

		h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		h.store.Release(account)
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

	supportedModels := h.supportedModelIDs(c.Request.Context())
	rawBody, requestModel, mappedModel, mappingApplied := h.applyConfiguredModelMappingToBody(rawBody, supportedModels)

	// Validate request
	validator := api.NewValidator(rawBody)
	rules := api.ChatCompletionValidationRules()
	rules["model"] = append(rules["model"], api.ModelValidator(supportedModels))
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
	if isImageOnlyModel(model) {
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
	if probeCapture, handled := h.prepareProbeShortCircuit(c, rawBody, "/v1/chat/completions", responseModel, gjson.GetBytes(rawBody, "stream").Bool(), "chat"); handled {
		return
	} else if probeCapture != nil {
		defer probeCapture.finish()
	}
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/chat/completions", model) {
		return
	}

	isStream := gjson.GetBytes(rawBody, "stream").Bool()
	reasoningEffort := extractReasoningEffort(rawBody)
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
	accountFilter := accountFilterForResponsesModel(effectiveModel, modelIDInList(effectiveModel, SupportedModelIDs(c.Request.Context(), h.db)))
	accountFilter = h.withModelCooldownFilter(effectiveModel, accountFilter)

	sessionID := ResolveSessionID(c.Request.Header, codexBody)
	explicitSessionID := ResolveExplicitSessionID(c.Request.Header, codexBody)
	apiKeyID := requestAPIKeyID(c)
	affinityKey := sessionAffinityKey(explicitSessionID, apiKeyID)

	// 3. 带重试的上游请求
	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	retryExclusions := newRetryAccountExclusions()
	forceHTTPAfterWSMessageTooBig := false

	// 上游 ctx 生命周期：每次 attempt 开始前用新的 drainable ctx 替换，
	// defer 兜底确保函数退出时上游被释放。
	var lastUpstreamCancel context.CancelFunc
	defer func() {
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
	}()

	for attempt := 0; ; attempt++ {
		account, stickyProxyURL := h.nextRetryAccountForSession(c.Request.Context(), affinityKey, apiKeyID, retryExclusions, accountFilter)
		if account == nil {
			if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
				h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
				return
			}
			c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(effectiveModel))
			return
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		h.store.BindSessionAffinity(affinityKey, account, proxyURL)
		isRelayAccount := account.IsOpenAIResponsesAPI()
		useWebsocket := h.shouldUseWebsocketForHTTP() && !forceHTTPAfterWSMessageTooBig && !isRelayAccount
		// 真实生图意图强制走 HTTP：WebSocket 传输大体积图片数据会卡死（issue #220）。
		// 仅凭注入的 image_generation 工具不触发降级，普通请求继续走 WS（issue #304）。
		if useWebsocket && rawResponsesBodyShouldForceHTTPForImageGeneration(codexBody) {
			useWebsocket = false
		}
		upstreamEndpoint := "/v1/responses"
		if isRelayAccount {
			relayBaseURL, _ := account.OpenAIResponsesCredentials()
			upstreamEndpoint = auth.OpenAIResponsesEndpoint(relayBaseURL, "/v1/responses")
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

		upstreamSessionID := resolveUpstreamSessionID(apiKeyID, sessionID, explicitSessionID, useWebsocket)
		// 上游使用与客户端解耦的 context：客户端中途断开时仍能继续读完
		// response.completed 拿到 usage（流式计费的关键）。
		// lastUpstreamCancel 在 attempt loop 顶部声明 + defer 兜底，
		// 这里覆盖前先 cancel 上一轮（重试时）。
		if lastUpstreamCancel != nil {
			lastUpstreamCancel()
		}
		upstreamCtx, upstreamCancel := newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
		lastUpstreamCancel = upstreamCancel
		ttftGuard := newFirstTokenTimeoutGuard(currentFirstTokenTimeout(), upstreamCancel)
		var resp *http.Response
		var reqErr error
		if isRelayAccount {
			resp, reqErr = ExecuteOpenAIResponsesRequest(upstreamCtx, account, codexBody, proxyURL, downstreamHeaders)
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
			if useWebsocket && kind == upstreamErrorKindMessageTooBig {
				log.Printf("上游 WebSocket 请求帧过大，自动降级 HTTP 重试 (attempt %d, account %d, /v1/chat/completions): %v", attempt+1, account.ID(), reqErr)
				forceHTTPAfterWSMessageTooBig = true
				h.store.Release(account)
				h.store.UnbindSessionAffinity(affinityKey, account.ID())
				continue
			}
			retryable := IsRetryableError(reqErr) || kind != ""
			shouldRetry := false
			if retryable {
				shouldRetry = shouldRetryRequestError(reqErr, &generalRetries, maxRetries)
			}
			if kind != "" && !(timedOut && shouldRetry) {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			if timedOut && shouldRetry {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
				log.Printf("上游首字超时，断开并重试 (attempt %d/%d, account %d, /v1/chat/completions): %v", attempt+1, maxRetries+1, account.ID(), reqErr)
				continue
			}
			if !timedOut {
				retryExclusions.MarkHard(account.ID())
			}

			// 不可重试的结构化错误直接返回
			if !retryable {
				ErrorToGinResponse(c, reqErr)
				return
			}

			log.Printf("上游请求失败 (attempt %d): %v", attempt+1, reqErr)
			if shouldRetry {
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			ttftGuard.Stop()
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
			h.logUpstreamCyberPolicy(c, "/v1/chat/completions", logModel, errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, effectiveModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
			usageTiers := resolveUsageServiceTiers("", serviceTier)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:            account.ID(),
				Endpoint:             "/v1/chat/completions",
				Model:                logModel,
				EffectiveModel:       logEffectiveModel,
				StatusCode:           resp.StatusCode,
				DurationMs:           durationMs,
				ReasoningEffort:      reasoningEffort,
				InboundEndpoint:      "/v1/chat/completions",
				UpstreamEndpoint:     upstreamEndpoint,
				Stream:               isStream,
				ViaWebsocket:         useWebsocket,
				ServiceTier:          usageTiers.ServiceTier,
				RequestedServiceTier: usageTiers.RequestedServiceTier,
				ActualServiceTier:    usageTiers.ActualServiceTier,
				BillingServiceTier:   usageTiers.BillingServiceTier,
				IsRetryAttempt:       shouldRetry,
				AttemptIndex:         attempt + 1,
				UpstreamErrorKind:    upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:         usageLogErrorMessage(resp.StatusCode, errBody),
			})

			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}

			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		SyncCodexUsageState(h.store, account, resp)

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
		var compactResult []byte
		var terminalFailurePayload []byte

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
			streamWriter := newStreamFlushWriter(c.Writer, flusher)

			// clientGone：客户端写失败后置位，后续事件不再写客户端，
			// 但继续读上游直到 response.completed/failed，以拿到准确 usage。
			clientGone := false
			var pendingFirstTokenChunks bytes.Buffer
			readErr = ReadSSEStream(resp.Body, func(data []byte) bool {
				parsed := gjson.ParseBytes(data)
				chunk, done := streamTranslator.TranslateParsed(parsed)

				eventType := parsed.Get("type").String()
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
				if eventType == "response.completed" {
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
						return eventType != "response.completed" && eventType != "response.failed"
					}
				}
				if !clientGone && done {
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
			if writeErr == nil {
				writeErr = streamWriter.Flush()
			}
		} else {
			var fullContent strings.Builder
			var fullReasoning strings.Builder
			var toolCalls []ToolCallResult

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
				case "response.completed":
					usage = extractUsageFromResult(parsed.Get("response.usage"))
					if tier := parsed.Get("response.service_tier").String(); tier != "" {
						actualServiceTier = tier
					}
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

			compactResult = BuildCompactResponse(chunkID, responseModel, created, fullContent.String(), fullReasoning.String(), toolCalls, usage)
		}

		// 断流检测 + token 估算
		totalDuration := int(time.Since(start).Milliseconds())
		outcome := classifyStreamOutcome(c.Request.Context().Err(), readErr, writeErr, gotTerminal)
		if ttftGuard.TimedOut() && !ttftRecorded && !gotTerminal {
			outcome = firstTokenTimeoutOutcome(currentFirstTokenTimeout())
		}
		ttftGuard.Stop()
		var responseFailedDecision codex429Decision
		if len(terminalFailurePayload) > 0 {
			outcome = classifyResponseFailedOutcome(terminalFailurePayload)
			responseFailedDecision = h.applyResponseFailedCooldown(account, terminalFailurePayload, resp, effectiveModel)
			if responseFailedDecision.Reason != "" {
				outcome.failureKind = upstreamErrorKind(outcome.logStatusCode, responseFailedErrorBody(terminalFailurePayload), responseFailedDecision)
			}
			// 流式 response.failed（HTTP 200）里的 cyber_policy 处罚也要记录，
			// 否则只有非 2xx 错误体才会被记入提示词过滤日志。
			h.logUpstreamCyberPolicy(c, "/v1/chat/completions", logModel, responseFailedErrorBody(terminalFailurePayload))
		}
		if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, useWebsocket, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游 WebSocket 消息过大，首包前自动降级 HTTP 重试 (attempt %d, account %d, /v1/chat/completions): %s", attempt+1, account.ID(), outcome.failureMessage)
			forceHTTPAfterWSMessageTooBig = true
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
			continue
		}
		if shouldTransparentRetryStream(outcome, attempt, maxRetries, wroteAnyBody, c.Request.Context().Err(), writeErr) {
			log.Printf("上游流在首包前断开，重置连接并重试 (attempt %d/%d, account %d, /v1/chat/completions): %s", attempt+1, maxRetries+1, account.ID(), outcome.failureMessage)
			recyclePooledClient(account, proxyURL)
			if isFirstTokenTimeoutOutcome(outcome) {
				retryExclusions.MarkSoftFirstTokenTimeout(account.ID())
			} else {
				h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			}
			resp.Body.Close()
			h.store.Release(account)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
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
		if !isStream {
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
			AccountID:            account.ID(),
			Endpoint:             "/v1/chat/completions",
			Model:                logModel,
			EffectiveModel:       logEffectiveModel,
			StatusCode:           logStatusCode,
			DurationMs:           totalDuration,
			FirstTokenMs:         firstTokenMs,
			ReasoningEffort:      reasoningEffort,
			InboundEndpoint:      "/v1/chat/completions",
			UpstreamEndpoint:     upstreamEndpoint,
			Stream:               isStream,
			ViaWebsocket:         useWebsocket,
			ServiceTier:          usageTiers.ServiceTier,
			RequestedServiceTier: usageTiers.RequestedServiceTier,
			ActualServiceTier:    usageTiers.ActualServiceTier,
			BillingServiceTier:   usageTiers.BillingServiceTier,
		}
		if logStatusCode != http.StatusOK {
			logInput.ErrorMessage = usageLogErrorMessage(logStatusCode, []byte(outcome.failureMessage))
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
			h.store.ReportRequestFailure(account, outcome.failureKind, time.Duration(totalDuration)*time.Millisecond)
			h.store.UnbindSessionAffinity(affinityKey, account.ID())
		} else if outcome.logStatusCode == http.StatusOK {
			h.store.ClearModelCooldown(account, effectiveModel)
			h.store.ReportRequestSuccess(account, time.Duration(totalDuration)*time.Millisecond)
		}
		h.store.Release(account)
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

	streamWriter := newStreamFlushWriter(c.Writer, flusher)
	err := ReadSSEStream(body, func(data []byte) bool {
		chunk, done := TranslateStreamChunk(data, model, chunkID, created)
		if chunk != nil {
			if err := streamWriter.WriteSSEData(chunk); err != nil {
				return false
			}
		}
		if done {
			if err := streamWriter.WriteString("data: [DONE]\n\n"); err != nil {
				return false
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
	if usedStr == "" {
		return codexWindowUsage{}
	}
	return codexWindowUsage{
		usedPct:   parseFloat(usedStr),
		windowMin: parseFloat(windowStr),
		resetSec:  parseFloat(resetStr),
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
	decision := classify429RateLimit(account, body, resp, time.Now(), model)
	if store == nil || account == nil {
		return decision
	}
	if details, ok := parseUsageLimitDetails(body); ok {
		store.ApplyUsageLimitMetadata(account, details.planType, decision.ResetAt)
	}
	if decision.Scope == rateLimitScopeModel {
		cooldown := store.MarkModelCooldown(account, decision.Model, decision.Cooldown, decision.Reason)
		decision.ResetAt = cooldown.ResetAt
		decision.Cooldown = time.Until(cooldown.ResetAt)
		return decision
	}
	if account.IsPremium5hPlan() && decision.Scope == rateLimitScopeAccount && decision.Reason == "rate_limited_5h" {
		store.MarkPremium5hRateLimited(account, decision.ResetAt)
		return decision
	}
	store.MarkCooldown(account, decision.Cooldown, "rate_limited")
	return decision
}

// applyCooldown 根据上游状态码设置智能冷却
func (h *Handler) applyCooldown(account *auth.Account, statusCode int, body []byte, resp *http.Response) {
	h.applyCooldownForModel(account, statusCode, body, resp, "")
}

func (h *Handler) applyCooldownForModel(account *auth.Account, statusCode int, body []byte, resp *http.Response, model string) codex429Decision {
	if IsUsageLimitReachedError(body) {
		decision := Apply429Cooldown(h.store, account, body, resp, model)
		log.Printf("账号 %d 触发用量上限 (status=%d, plan=%s, reason=%s)，冷却到 %s", account.ID(), statusCode, account.GetPlanType(), decision.Reason, decision.ResetAt.Format(time.RFC3339))
		return decision
	}
	switch statusCode {
	case http.StatusTooManyRequests:
		decision := Apply429Cooldown(h.store, account, body, resp, model)
		if decision.Scope == rateLimitScopeModel {
			log.Printf("账号 %d 模型 %s 触发短时限流 (reason=%s)，冷却到 %s", account.ID(), decision.Model, decision.Reason, decision.ResetAt.Format(time.RFC3339))
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
		if IsDeactivatedWorkspaceError(body) {
			log.Printf("账号 %d 工作区已停用，标记为错误", account.ID())
			if h.store != nil {
				h.store.MarkError(account, upstreamAccountErrorMessage(statusCode, body))
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
	if store != nil {
		store.UpdateAccountPlanType(account, resp.Header.Get("x-codex-plan-type"))
	}

	result.Used5hHeaders = responseHasCodex5hHeaders(resp)
	result.UsagePct7d, result.HasUsage7d = parseCodexUsageHeaders(resp, account)
	if store != nil {
		if result.HasUsage7d {
			store.PersistUsageSnapshot(account, result.UsagePct7d)
			if result.UsagePct7d >= 100 {
				result.Usage7dRateLimited = store.MarkUsage7dRateLimited(account)
			}
		} else if result.Used5hHeaders {
			store.PersistUsageSnapshot5hOnly(account)
			result.Persisted5hOnly = true
		}
	}

	result.UsagePct5h, result.Reset5hAt, result.HasUsage5h = account.GetUsageSnapshot5h()
	if result.Used5hHeaders && account.IsPremium5hPlan() && result.HasUsage5h && result.UsagePct5h >= 100 {
		if store != nil {
			store.MarkPremium5hRateLimited(account, result.Reset5hAt)
		}
		result.Premium5hRateLimited = true
	}

	return result
}

// parseCodexUsageHeaders 从 Codex 响应头解析 5h/7d 用量百分比
func parseCodexUsageHeaders(resp *http.Response, account *auth.Account) (float64, bool) {
	if resp == nil {
		return 0, false
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

	// 归一化：小窗口 (≤360min) → 5h，大窗口 (>360min) → 7d
	var w5h, w7d codexWindowUsage
	now := time.Now()

	if primary.valid && secondary.valid {
		if primary.windowMin >= secondary.windowMin {
			w7d, w5h = primary, secondary
		} else {
			w7d, w5h = secondary, primary
		}
	} else if primary.valid {
		if primary.windowMin <= 360 && primary.windowMin > 0 {
			w5h = primary
		} else {
			w7d = primary
		}
	} else if secondary.valid {
		if secondary.windowMin <= 360 && secondary.windowMin > 0 {
			w5h = secondary
		} else {
			w7d = secondary
		}
	}

	// 写入 5h
	if w5h.valid {
		resetAt := now.Add(time.Duration(w5h.resetSec) * time.Second)
		account.SetUsageSnapshot5hAt(w5h.usedPct, resetAt, now)
	}

	// 写入 7d
	if w7d.valid {
		resetAt := now.Add(time.Duration(w7d.resetSec) * time.Second)
		account.SetReset7dAt(resetAt)
		account.SetUsagePercent7d(w7d.usedPct)
		return w7d.usedPct, true
	}

	return 0, false
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
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"message": fmt.Sprintf("上游返回错误 (status %d): %s", statusCode, string(body)),
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

	h.sendUpstreamError(c, statusCode, body)
}

// handleUpstreamError 统一处理上游错误（兼容旧调用）
func (h *Handler) handleUpstreamError(c *gin.Context, account *auth.Account, statusCode int, body []byte) {
	h.applyCooldown(account, statusCode, body, nil)
	h.sendUpstreamError(c, statusCode, body)
}

// ListModels 列出可用模型
func (h *Handler) ListModels(c *gin.Context) {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	modelIDs := h.supportedModelIDs(ctx)
	models := make([]api.Model, 0, len(modelIDs))
	now := time.Now().Unix()
	for _, id := range modelIDs {
		models = append(models, api.Model{
			ID:      id,
			Object:  "model",
			Created: now,
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
			for _, model := range account.OpenAIResponsesModels() {
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
		}
	}
	return models
}
