package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func applyClaudeAPIKeyHeaders(req *http.Request, key string, incoming http.Header, stream bool) {
	req.Header.Set("x-api-key", key)
	req.Header.Del("Authorization")
	req.Header.Set("Content-Type", "application/json")
	setIfAbsentFromIncoming(req.Header, incoming, "anthropic-version", claudeAnthropicVersion)
	setIfAbsentFromIncoming(req.Header, incoming, "User-Agent", "Codex2API")
	var betas []string
	for _, value := range incoming.Values("anthropic-beta") {
		for _, beta := range strings.Split(value, ",") {
			beta = strings.TrimSpace(beta)
			lower := strings.ToLower(beta)
			// OAuth authentication and CLI identity betas are not API features.
			if beta != "" && !strings.HasPrefix(lower, "oauth-") && !strings.HasPrefix(lower, "claude-code-") {
				betas = append(betas, beta)
			}
		}
	}
	if len(betas) > 0 {
		req.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	RecordUpstreamUserAgent(req.Context(), req.Header.Get("User-Agent"))
}

// executeClaudeAPIKeyMessages preserves the native body, including system,
// metadata and provider-specific thinking signatures. Only account routing and
// authentication are supplied by the gateway.
func executeClaudeAPIKeyMessages(ctx context.Context, account *auth.Account, body []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	account.Mu().RLock()
	key, baseURL, proxyURL := strings.TrimSpace(account.AccessToken), account.ClaudeBaseURL, account.ProxyURL
	account.Mu().RUnlock()
	if key == "" {
		return nil, ErrNoAvailableAccount()
	}
	endpoint, err := auth.ClaudeAPIEndpoint(baseURL, "messages")
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, ErrBadRequest("Claude request body must be a JSON object")
	}
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInternalError("创建 Claude 请求失败", err)
	}
	applyClaudeAPIKeyHeaders(req, key, headers, gjson.GetBytes(body, "stream").Bool())
	if err := ConsumeAPIKeyModelRequestQuota(ctx, strings.TrimSpace(gjson.GetBytes(body, "model").String())); err != nil {
		return nil, err
	}
	client := *getPooledClient(account, proxyURL)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := doTracedUpstreamRequest(&client, req, account, proxyURL)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 Anthropic Messages API 失败", err)
	}
	return resp, nil
}
