package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexAlphaSearchURL 是 ChatGPT 后端的 Codex standalone 联网搜索端点。
// Codex CLI 开启 web_search = "live"（或 --search）后会调用
// POST {base_url}/alpha/search 完成独立搜索；这与 Responses 请求里的
// hosted tool（tools: [{"type":"web_search"}]）不是同一条链路。
const codexAlphaSearchURL = "https://chatgpt.com/backend-api/codex/alpha/search"

// codexAlphaSearchURLForTest 允许测试替换默认 URL。生产代码不要赋值。
var codexAlphaSearchURLForTest = ""

// 搜索请求与结果均为结构化 JSON，正常远小于该值，仅作读取护栏。
const codexAlphaSearchBodyLimit int64 = 4 << 20

// codexAlphaSearchUnsupportedFields 是 standalone 搜索端点不接受、转发前需剥离的字段。
// 新版 codex 客户端会把 Responses 风格的会话/缓存字段也塞进搜索体，而上游
// /alpha/search 的 schema 更窄，会以 400 "Unknown parameter" 拒绝（issue #433）：
// 同批 /responses 请求接受 prompt_cache_key，唯独搜索端点拒绝。这些字段对一次检索
// 调用无意义，剥离不改变搜索语义。
var codexAlphaSearchUnsupportedFields = []string{"prompt_cache_key"}

// sanitizeCodexAlphaSearchBody 在转发前移除搜索端点不支持的字段，避免上游 400。
// 只删已知不兼容字段，其余请求体保持原样透传。
func sanitizeCodexAlphaSearchBody(rawBody []byte) []byte {
	sanitized := rawBody
	for _, field := range codexAlphaSearchUnsupportedFields {
		if !gjson.GetBytes(sanitized, field).Exists() {
			continue
		}
		if next, err := sjson.DeleteBytes(sanitized, field); err == nil {
			sanitized = next
		}
	}
	return sanitized
}

// CodexAlphaSearchHandler 透传 Codex CLI 的 standalone 联网搜索（issue #359）。
//
// 端点只存在于 ChatGPT 后端，用一个可调度的 ChatGPT OAuth 账号的凭据实时转发，
// 请求体与响应（含上游错误状态码）原样透传，不在本地解析或伪造搜索结果。
func (h *Handler) CodexAlphaSearchHandler(c *gin.Context) {
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, codexAlphaSearchBodyLimit))
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	h.capturePromptRequestIngress(c, rawBody)
	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/alpha/search", model) {
		return
	}

	apiKeyID := requestAPIKeyID(c)
	// 搜索端点只存在于 ChatGPT 后端，relay/Grok 账号无从代答。
	account := h.store.NextExcludingWithFilter(apiKeyID, nil, func(a *auth.Account) bool {
		return !a.IsRelayStyle()
	})
	if account == nil {
		api.SendError(c, api.ErrServiceUnavailable)
		return
	}
	defer h.store.Release(account)

	apiKey := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	resp, err := ForwardCodexAlphaSearch(
		c.Request.Context(),
		account,
		h.store.ResolveProxyForAccount(account),
		rawBody,
		c.Request.Header,
		h.deviceCfg,
		apiKey,
	)
	if err != nil {
		api.SendErrorWithStatus(c,
			api.NewAPIError(api.ErrCodeUpstreamError, fmt.Sprintf("codex alpha search: %v", err), api.ErrorTypeUpstream),
			http.StatusBadGateway)
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logUpstreamError("/v1/alpha/search", resp.StatusCode, "", account.ID(), resp.Body)
	}
	contentType := resp.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(resp.StatusCode, contentType, resp.Body)
}

// CodexAlphaSearchResponse 承载上游搜索响应原文，供 handler 原样透传。
type CodexAlphaSearchResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// ForwardCodexAlphaSearch 用账号凭据向 ChatGPT 后端转发 standalone 搜索请求。
// 上游状态码与响应体原样带回（含 4xx/5xx），让 CLI 拿到真实的错误语义；
// 仅传输层失败返回 error。
func ForwardCodexAlphaSearch(ctx context.Context, account *auth.Account, proxyURL string, rawBody []byte, downstreamHeaders http.Header, deviceCfg *DeviceProfileConfig, apiKey string) (*CodexAlphaSearchResponse, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("account has no access token")
	}

	// 剥离搜索端点不支持的字段（如客户端塞进来的 prompt_cache_key），否则上游 400（issue #433）。
	rawBody = sanitizeCodexAlphaSearchBody(rawBody)

	endpoint := codexAlphaSearchURL
	if codexAlphaSearchURLForTest != "" {
		endpoint = codexAlphaSearchURLForTest
	}

	// standalone 搜索是模型驱动的检索回合，上游耗时可达数十秒。
	reqCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(string(rawBody)))
	if err != nil {
		return nil, fmt.Errorf("build codex search request: %w", err)
	}

	if deviceCfg == nil {
		deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
	}
	userAgent, version := ResolveCodexOutboundClientHeaders(account, apiKey, deviceCfg, downstreamHeaders)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Originator", Originator)
	if version != "" {
		req.Header.Set("Version", version)
	}
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}

	// 复用网关同款 transport（支持 uTLS Chrome 指纹），与 /responses、清单透传一致。
	client := &http.Client{Transport: newCodexTransport(proxyURL)}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex search request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, codexAlphaSearchBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read codex search response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("codex alpha search 上游返回 %d: %s", resp.StatusCode, truncateForLog(body, 512))
	}
	return &CodexAlphaSearchResponse{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}, nil
}

func truncateForLog(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}
