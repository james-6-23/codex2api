// Package proxy: API Key 级别的细粒度限流与配额校验。
//
// 入口:enforceAPIKeyLimits()。在请求鉴权后、转发上游前调用。失败返回 (httpStatus, message),
// handler 直接以该响应短路;成功则返回 (0, "")。
//
// 6 类限制:
//   - ModelAllow / ModelDeny: O(1) string set,本机内存即可,无副作用。
//   - RPM:  滑动 60s 内请求数。Redis INCR + EXPIRE 60s 计数器(没 Redis 时回退 DB 聚合 + 短缓存)。
//   - RPD:  滑动 24h 内请求数。同上,EXPIRE 86400。
//   - CostLimit5h / CostLimit7d:    滑动 5h / 7d 内 user_billed 累计。Redis 60s 缓存 + DB 聚合兜底。
//   - TokenLimit5h / TokenLimit7d:  同 cost,聚合 total_tokens。
//
// Redis 失效或不存在时一律退到 DB 聚合 + 1 分钟缓存,保证可用性优先。
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	apiKeyLimitsCacheNamespace = "api-key-limits"
	apiKeyLimitsCacheTTL       = 60 * time.Second
	apiKeyRPMWindow            = time.Minute
	apiKeyRPDWindow            = 24 * time.Hour
	apiKey5hWindow             = 5 * time.Hour
	apiKey7dWindow             = 7 * 24 * time.Hour
	apiKey30dWindow            = 30 * 24 * time.Hour
)

// apiKeyRowFromContext 从 gin context 中取出鉴权时已存的 APIKeyRow。
// 若不存在(例如内部调用未经鉴权路径),返回 nil。
func apiKeyRowFromContext(c *gin.Context) *database.APIKeyRow {
	if c == nil {
		return nil
	}
	v, ok := c.Get(contextAPIKeyRow)
	if !ok || v == nil {
		return nil
	}
	row, _ := v.(*database.APIKeyRow)
	return row
}

// APIKeyRowFromContext returns the API key metadata stored by the /v1 auth middleware.
func APIKeyRowFromContext(c *gin.Context) *database.APIKeyRow {
	return apiKeyRowFromContext(c)
}

// EnforceAPIKeyLimits checks API key scoped model and rate/cost limits.
func (h *Handler) EnforceAPIKeyLimits(c *gin.Context, model string) (int, string) {
	return h.enforceAPIKeyLimits(c, model)
}

// enforceAPIKeyLimits 检查 API Key 的所有限制条件。
// 命中限制时返回 (status, errorMessage),handler 应立即以该响应短路;
// 全部通过返回 (0, "")。
//
// model 参数用于模型白/黑名单判定,传空字符串表示跳过模型校验。
//
// 返回 status:
//   - http.StatusForbidden (403): 模型不在白名单 / 在黑名单
//   - http.StatusTooManyRequests (429): rpm/rpd/cost/token 任一窗口超额
func (h *Handler) enforceAPIKeyLimits(c *gin.Context, model string) (int, string) {
	row := apiKeyRowFromContext(c)
	if row == nil {
		return 0, ""
	}
	limits := row.Limits
	if limits.IsZero() {
		return 0, ""
	}

	// 1. 模型白/黑名单 (O(1) 本机校验,无 I/O)
	if model != "" {
		if msg := checkAPIKeyModel(model, limits); msg != "" {
			return http.StatusForbidden, msg
		}
	}

	// 1b. 生图禁用 (本机校验:模型/端点/请求体意图,无 I/O)
	if limits.DisableImageGeneration {
		if msg := checkAPIKeyImageGeneration(c, model); msg != "" {
			return http.StatusForbidden, msg
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	// 2. RPM
	if limits.RPM > 0 {
		count, err := h.apiKeyWindowRequests(ctx, row.ID, "rpm", apiKeyRPMWindow)
		if err == nil && count >= int64(limits.RPM) {
			return http.StatusTooManyRequests,
				fmt.Sprintf("API key rate limit exceeded: %d requests per minute", limits.RPM)
		}
	}

	// 3. RPD
	if limits.RPD > 0 {
		count, err := h.apiKeyWindowRequests(ctx, row.ID, "rpd", apiKeyRPDWindow)
		if err == nil && count >= int64(limits.RPD) {
			return http.StatusTooManyRequests,
				fmt.Sprintf("API key rate limit exceeded: %d requests per day", limits.RPD)
		}
	}

	// 4. cost / token 5h & 7d
	if limits.CostLimit5h > 0 || limits.TokenLimit5h > 0 {
		usage, err := h.apiKeyWindowUsage(ctx, row.ID, "5h", apiKey5hWindow)
		if err == nil && usage != nil {
			if limits.CostLimit5h > 0 && usage.UserBilled >= limits.CostLimit5h {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key cost limit exceeded: $%.2f / $%.2f in last 5h",
						usage.UserBilled, limits.CostLimit5h)
			}
			if limits.TokenLimit5h > 0 && usage.Tokens >= limits.TokenLimit5h {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key token limit exceeded: %d / %d in last 5h",
						usage.Tokens, limits.TokenLimit5h)
			}
		}
	}
	if limits.CostLimit7d > 0 || limits.TokenLimit7d > 0 {
		usage, err := h.apiKeyWindowUsage(ctx, row.ID, "7d", apiKey7dWindow)
		if err == nil && usage != nil {
			if limits.CostLimit7d > 0 && usage.UserBilled >= limits.CostLimit7d {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key cost limit exceeded: $%.2f / $%.2f in last 7d",
						usage.UserBilled, limits.CostLimit7d)
			}
			if limits.TokenLimit7d > 0 && usage.Tokens >= limits.TokenLimit7d {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key token limit exceeded: %d / %d in last 7d",
						usage.Tokens, limits.TokenLimit7d)
			}
		}
	}
	if limits.CostLimit30d > 0 || limits.TokenLimit30d > 0 {
		usage, err := h.apiKeyWindowUsage(ctx, row.ID, "30d", apiKey30dWindow)
		if err == nil && usage != nil {
			if limits.CostLimit30d > 0 && usage.UserBilled >= limits.CostLimit30d {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key cost limit exceeded: $%.2f / $%.2f in last 30d",
						usage.UserBilled, limits.CostLimit30d)
			}
			if limits.TokenLimit30d > 0 && usage.Tokens >= limits.TokenLimit30d {
				return http.StatusTooManyRequests,
					fmt.Sprintf("API key token limit exceeded: %d / %d in last 30d",
						usage.Tokens, limits.TokenLimit30d)
			}
		}
	}

	return 0, ""
}

// SendAPIKeyLimitError writes a standard /v1 API key limit error response.
func SendAPIKeyLimitError(c *gin.Context, status int, msg string) {
	errType := api.ErrorTypeRateLimit
	errCode := api.ErrCodeRateLimitReached
	if status == http.StatusForbidden {
		errType = api.ErrorTypePermission
		errCode = api.ErrCodeInvalidRequest
	}
	api.SendErrorWithStatus(c, api.NewAPIError(errCode, msg, errType), status)
}

// enforceAPIKeyLimitsAndReply 在请求转发前调用,命中限制时已向客户端写入响应并返回 true。
// handler 应在 true 时立即 return,无需再处理。
func (h *Handler) enforceAPIKeyLimitsAndReply(c *gin.Context, model string) bool {
	status, msg := h.enforceAPIKeyLimits(c, model)
	if status == 0 {
		return false
	}
	SendAPIKeyLimitError(c, status, msg)
	return true
}

// checkAPIKeyModel 校验模型是否被允许。
// 白名单非空 → 必须在白名单内;否则黑名单生效,模型不能在黑名单内。
// 大小写不敏感比较。
func checkAPIKeyModel(model string, limits database.APIKeyLimits) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if len(limits.ModelAllow) > 0 {
		for _, m := range limits.ModelAllow {
			if strings.ToLower(strings.TrimSpace(m)) == model {
				return ""
			}
		}
		return fmt.Sprintf("Model %q is not allowed for this API key", model)
	}
	for _, m := range limits.ModelDeny {
		if strings.ToLower(strings.TrimSpace(m)) == model {
			return fmt.Sprintf("Model %q is denied for this API key", model)
		}
	}
	return ""
}

// checkAPIKeyImageGeneration 在该 Key 禁用生图时，判断本次请求是否触及生图能力。
// 命中(生图模型 / /v1/images 端点 / 请求体带 image_generation 工具或生图意图)返回错误文案，
// 否则返回 ""。纯本机校验：模型名 + 端点路径 + 客户端原始请求体（自动注入的图片工具在此之后才发生，
// 不会误伤普通文本请求）。
func checkAPIKeyImageGeneration(c *gin.Context, model string) string {
	const msg = "Image generation is disabled for this API key (gpt-image models and the image_generation tool are not permitted)."

	if isImageOnlyModel(strings.TrimSpace(model)) {
		return msg
	}

	if c != nil && c.Request != nil && c.Request.URL != nil {
		path := c.Request.URL.Path
		if strings.Contains(path, "/images/generations") || strings.Contains(path, "/images/edits") {
			return msg
		}
	}

	if body, ok := rawRequestBodyFromContext(c); ok && len(body) > 0 {
		if responsesBodyHasImageGenerationTool(body) || responsesBodyRequestsImageGeneration(body) {
			return msg
		}
	}
	return ""
}

// apiKeyWindowRequests 返回某 API Key 在指定窗口内的请求数。
// 优先读 Redis 缓存,失败回退 DB 聚合并缓存 60s。
func (h *Handler) apiKeyWindowRequests(ctx context.Context, apiKeyID int64, label string, window time.Duration) (int64, error) {
	cacheKey := apiKeyLimitsCacheKey(apiKeyID, "req", label)
	if v, ok := h.readAPIKeyLimitCache(ctx, cacheKey); ok {
		return v.Requests, nil
	}
	usage, err := h.db.GetAPIKeyWindowUsage(ctx, apiKeyID, window)
	if err != nil {
		return 0, err
	}
	h.writeAPIKeyLimitCache(ctx, cacheKey, usage)
	return usage.Requests, nil
}

func (h *Handler) apiKeyWindowUsage(ctx context.Context, apiKeyID int64, label string, window time.Duration) (*database.APIKeyWindowUsage, error) {
	cacheKey := apiKeyLimitsCacheKey(apiKeyID, "usage", label)
	if v, ok := h.readAPIKeyLimitCache(ctx, cacheKey); ok {
		return v, nil
	}
	usage, err := h.db.GetAPIKeyWindowUsage(ctx, apiKeyID, window)
	if err != nil {
		return nil, err
	}
	h.writeAPIKeyLimitCache(ctx, cacheKey, usage)
	return usage, nil
}

func apiKeyLimitsCacheKey(apiKeyID int64, kind, label string) string {
	return fmt.Sprintf("%d:%s:%s", apiKeyID, kind, label)
}

func (h *Handler) readAPIKeyLimitCache(ctx context.Context, key string) (*database.APIKeyWindowUsage, bool) {
	if h == nil || h.cache == nil {
		return nil, false
	}
	raw, ok, err := h.cache.GetRuntime(ctx, apiKeyLimitsCacheNamespace, key)
	if err != nil || !ok || len(raw) == 0 {
		return nil, false
	}
	var usage database.APIKeyWindowUsage
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil, false
	}
	return &usage, true
}

func (h *Handler) writeAPIKeyLimitCache(ctx context.Context, key string, usage *database.APIKeyWindowUsage) {
	if h == nil || h.cache == nil || usage == nil {
		return
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		return
	}
	_ = h.cache.SetRuntime(ctx, apiKeyLimitsCacheNamespace, key, raw, apiKeyLimitsCacheTTL)
}
