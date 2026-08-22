package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIResponsesBalanceQueryURLCredential = "balance_query_url"
const openAIResponsesBalanceCacheTTL = 30 * time.Second
const openAIResponsesBalanceAttemptTimeout = 8 * time.Second

type openAIResponsesBalanceCacheEntry struct {
	Response  openAIResponsesBalanceResponse
	ExpiresAt time.Time
}

type openAIResponsesBalanceResponse struct {
	Balance   float64 `json:"balance"`
	Unit      string  `json:"unit"`
	Source    string  `json:"source"`
	Unlimited bool    `json:"unlimited,omitempty"`
	QueriedAt string  `json:"queried_at"`
}

// GetOpenAIResponsesBalance queries the upstream platform with the API key that
// is already stored on the account. The key is never returned to the browser.
// GET /api/admin/accounts/:id/openai-responses/balance
func (h *Handler) GetOpenAIResponsesBalance(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	forceRefresh := c.Query("refresh") == "1" || strings.EqualFold(c.Query("refresh"), "true")
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamOpenAIResponses) {
		writeError(c, http.StatusBadRequest, "仅 OpenAI Responses API 账号支持查询平台余额")
		return
	}
	if !forceRefresh {
		h.openAIResponsesBalanceMu.RLock()
		cached, ok := h.openAIResponsesBalanceCache[id]
		h.openAIResponsesBalanceMu.RUnlock()
		if ok && time.Now().Before(cached.ExpiresAt) {
			c.JSON(http.StatusOK, cached.Response)
			return
		}
	}

	result, err := queryOpenAIResponsesBalance(
		ctx,
		row.GetCredential("base_url"),
		row.GetCredential("api_key"),
		row.ProxyURL,
		row.GetCredentialStringMap("custom_headers"),
		row.GetCredential(openAIResponsesBalanceQueryURLCredential),
	)
	if err != nil {
		writeError(c, http.StatusBadGateway, "查询上游余额失败: "+err.Error())
		return
	}
	result.QueriedAt = time.Now().Format(time.RFC3339)
	h.openAIResponsesBalanceMu.Lock()
	if h.openAIResponsesBalanceCache == nil {
		h.openAIResponsesBalanceCache = make(map[int64]openAIResponsesBalanceCacheEntry)
	}
	h.openAIResponsesBalanceCache[id] = openAIResponsesBalanceCacheEntry{
		Response:  result,
		ExpiresAt: time.Now().Add(openAIResponsesBalanceCacheTTL),
	}
	h.openAIResponsesBalanceMu.Unlock()
	c.JSON(http.StatusOK, result)
}

func normalizeOpenAIResponsesBalanceQueryURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("余额查询接口不是合法 URL")
	}
	if parsed.IsAbs() {
		if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return "", fmt.Errorf("余额查询接口仅支持 http/https")
		}
		parsed.Fragment = ""
		return parsed.String(), nil
	}
	if parsed.Host != "" || parsed.User != nil || !strings.HasPrefix(parsed.Path, "/") {
		return "", fmt.Errorf("余额查询接口请填写完整 http/https URL 或以 / 开头的路径")
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

func resolveOpenAIResponsesBalanceQueryURL(baseURL, configured string) (string, error) {
	configured, err := normalizeOpenAIResponsesBalanceQueryURL(configured)
	if err != nil || configured == "" {
		return configured, err
	}
	ref, _ := url.Parse(configured)
	if ref.IsAbs() {
		return ref.String(), nil
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("Base URL 不是合法 URL")
	}
	base.Path = "/"
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base.ResolveReference(ref).String(), nil
}

func openAIResponsesOriginEndpoint(baseURL, endpointPath string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("Base URL 不是合法 URL")
	}
	ref, err := url.Parse(endpointPath)
	if err != nil {
		return "", err
	}
	base.Path = "/"
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base.ResolveReference(ref).String(), nil
}

func queryOpenAIResponsesBalance(
	ctx context.Context,
	baseURL, apiKey, proxyURL string,
	customHeaders map[string]string,
	configuredURL string,
) (openAIResponsesBalanceResponse, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return openAIResponsesBalanceResponse{}, fmt.Errorf("账号未保存 API Key")
	}
	client, err := newOpenAIResponsesBalanceClient(proxyURL)
	if err != nil {
		return openAIResponsesBalanceResponse{}, err
	}

	if strings.TrimSpace(configuredURL) != "" {
		endpoint, err := resolveOpenAIResponsesBalanceQueryURL(baseURL, configuredURL)
		if err != nil {
			return openAIResponsesBalanceResponse{}, err
		}
		body, err := fetchOpenAIResponsesBalancePayload(ctx, client, endpoint, apiKey, customHeaders)
		if err != nil {
			return openAIResponsesBalanceResponse{}, err
		}
		result, err := parseOpenAIResponsesBalancePayload(body)
		if err != nil {
			return openAIResponsesBalanceResponse{}, err
		}
		result.Source = "custom"
		return result, nil
	}

	var attempts []string
	sub2APIEndpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/usage")
	if body, fetchErr := fetchOpenAIResponsesBalancePayload(ctx, client, sub2APIEndpoint, apiKey, customHeaders); fetchErr == nil {
		if result, parseErr := parseOpenAIResponsesBalancePayload(body); parseErr == nil {
			result.Source = "sub2api"
			if result.Unit == "" {
				result.Unit = "USD"
			}
			return result, nil
		} else {
			attempts = append(attempts, "sub2api: "+parseErr.Error())
		}
	} else {
		attempts = append(attempts, "sub2api: "+fetchErr.Error())
	}

	result, newAPIErr := queryNewAPIBalance(ctx, client, baseURL, apiKey, customHeaders)
	if newAPIErr == nil {
		return result, nil
	}
	attempts = append(attempts, "new-api: "+newAPIErr.Error())
	return openAIResponsesBalanceResponse{}, fmt.Errorf("自动识别失败（%s）", strings.Join(attempts, "; "))
}

func newOpenAIResponsesBalanceClient(proxyURL string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = dialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, proxyURL, dialer); err != nil {
		return nil, fmt.Errorf("代理URL无效: %w", err)
	}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}, nil
}

func fetchOpenAIResponsesBalancePayload(
	ctx context.Context,
	client *http.Client,
	endpoint, apiKey string,
	customHeaders map[string]string,
) ([]byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, openAIResponsesBalanceAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("创建余额请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	for name, value := range customHeaders {
		name = strings.TrimSpace(name)
		if name != "" {
			req.Header.Set(name, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s 请求失败: %w", req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if readErr != nil {
		return nil, fmt.Errorf("读取 %s 响应失败: %w", req.URL.Path, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
		if message == "" {
			message = strings.TrimSpace(gjson.GetBytes(body, "message").String())
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("%s 返回 %d: %s", req.URL.Path, resp.StatusCode, message)
	}
	return body, nil
}

func parseOpenAIResponsesBalancePayload(body []byte) (openAIResponsesBalanceResponse, error) {
	paths := []string{
		"balance", "remaining", "quota.remaining", "total_available",
		"data.balance", "data.remaining", "data.quota.remaining", "data.total_available",
	}
	var amount float64
	found := false
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if !value.Exists() {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value.String()), 64)
		if err != nil {
			continue
		}
		amount = parsed
		found = true
		break
	}
	if !found {
		return openAIResponsesBalanceResponse{}, fmt.Errorf("响应中未找到 balance、remaining 或 total_available")
	}
	unit := firstBalanceString(body, "unit", "currency", "quota.unit", "data.unit", "data.currency", "data.quota.unit")
	if unit == "" && (gjson.GetBytes(body, "data.total_available").Exists() || gjson.GetBytes(body, "total_available").Exists()) {
		unit = "quota"
	}
	unlimited := firstBalanceBool(body, "unlimited", "unlimited_quota", "data.unlimited", "data.unlimited_quota")
	return openAIResponsesBalanceResponse{Balance: amount, Unit: unit, Unlimited: unlimited}, nil
}

func queryNewAPIBalance(
	ctx context.Context,
	client *http.Client,
	baseURL, apiKey string,
	customHeaders map[string]string,
) (openAIResponsesBalanceResponse, error) {
	tokenURL, err := openAIResponsesOriginEndpoint(baseURL, "/api/usage/token/")
	if err != nil {
		return openAIResponsesBalanceResponse{}, err
	}
	tokenAttempt := ""
	if tokenBody, tokenErr := fetchOpenAIResponsesBalancePayload(ctx, client, tokenURL, apiKey, customHeaders); tokenErr == nil {
		if result, parseErr := parseOpenAIResponsesBalancePayload(tokenBody); parseErr == nil {
			result.Source = "new-api"
			if result.Unit == "" {
				result.Unit = "quota"
			}
			return result, nil
		} else {
			tokenAttempt = "token: " + parseErr.Error()
		}
	} else {
		tokenAttempt = "token: " + tokenErr.Error()
	}

	subscriptionURL, err := openAIResponsesOriginEndpoint(baseURL, "/v1/dashboard/billing/subscription")
	if err != nil {
		return openAIResponsesBalanceResponse{}, fmt.Errorf("%s; subscription endpoint: %w", tokenAttempt, err)
	}
	usageURL, err := openAIResponsesOriginEndpoint(baseURL, "/v1/dashboard/billing/usage")
	if err != nil {
		return openAIResponsesBalanceResponse{}, fmt.Errorf("%s; usage endpoint: %w", tokenAttempt, err)
	}
	subscriptionBody, err := fetchOpenAIResponsesBalancePayload(ctx, client, subscriptionURL, apiKey, customHeaders)
	if err != nil {
		return openAIResponsesBalanceResponse{}, fmt.Errorf("%s; subscription: %w", tokenAttempt, err)
	}
	usageBody, err := fetchOpenAIResponsesBalancePayload(ctx, client, usageURL, apiKey, customHeaders)
	if err != nil {
		return openAIResponsesBalanceResponse{}, fmt.Errorf("%s; usage: %w", tokenAttempt, err)
	}
	hardLimit := gjson.GetBytes(subscriptionBody, "hard_limit_usd")
	totalUsage := gjson.GetBytes(usageBody, "total_usage")
	if !hardLimit.Exists() || !totalUsage.Exists() {
		return openAIResponsesBalanceResponse{}, fmt.Errorf("%s; 账单响应缺少 hard_limit_usd 或 total_usage", tokenAttempt)
	}
	balance := hardLimit.Float() - totalUsage.Float()/100
	if balance < 0 {
		balance = 0
	}
	return openAIResponsesBalanceResponse{Balance: balance, Unit: "USD", Source: "new-api"}, nil
}

func firstBalanceString(body []byte, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func firstBalanceBool(body []byte, paths ...string) bool {
	for _, path := range paths {
		if value := gjson.GetBytes(body, path); value.Exists() {
			return value.Bool()
		}
	}
	return false
}
