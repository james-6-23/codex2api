package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// applyClaudeAPIKeyHeaders builds the neutral API Key request headers, then
// layers two optional account-level features on top (issue #647):
//
//  1. identityMode — Claude Code client identity emulation (claude_fingerprint_mode
//     with API Key semantics: ""=off, preserve, force), see
//     auth.ApplyClaudeAPIKeyIdentityHeaders.
//  2. customHeaders — operator-configured credentials.custom_headers, applied
//     last so they win; reserved gateway-owned headers are never overridden.
//
// With neither configured the request is byte-for-byte the historical contract.
func applyClaudeAPIKeyHeaders(req *http.Request, key string, incoming http.Header, stream bool, customHeaders map[string]string, identityMode string) {
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
	auth.ApplyClaudeAPIKeyIdentityHeaders(req.Header, incoming, identityMode)
	auth.ApplyClaudeAPIKeyCustomHeaders(req.Header, customHeaders)
	// Record only the final User-Agent so the Usage page shows what the upstream
	// actually saw after identity emulation and custom headers were applied.
	RecordUpstreamUserAgent(req.Context(), req.Header.Get("User-Agent"))
}

// executeClaudeAPIKeyMessages preserves the native body, including system,
// metadata and provider-specific thinking signatures. Only account routing,
// authentication and the optional account-level header features (custom
// headers / Claude Code client identity) are supplied by the gateway.
func executeClaudeAPIKeyMessages(ctx context.Context, account *auth.Account, body []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	account.Mu().RLock()
	key, baseURL, proxyURL := strings.TrimSpace(account.AccessToken), account.ClaudeBaseURL, account.ProxyURL
	customHeaders := cloneStringMap(account.CustomHeaders)
	account.Mu().RUnlock()
	// The identity mode is read from the account itself rather than from the
	// caller: for API Key accounts an unset mode must stay "off" even when the
	// caller resolved the OAuth global default.
	identityMode := account.EffectiveClaudeFingerprintMode("")
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
	applyClaudeAPIKeyHeaders(req, key, headers, gjson.GetBytes(body, "stream").Bool(), customHeaders, identityMode)
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
