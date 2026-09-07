package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

// NormalizeClaudeBaseURL accepts an API root or a root ending in /v1.
// Credentials, query strings and fragments are not part of a service base URL.
func NormalizeClaudeBaseURL(raw string) (string, error) {
	if raw == "" || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("base_url must be a non-empty HTTP(S) URL without whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("base_url must be an HTTP(S) URL without credentials, query or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// ClaudeAPIEndpoint keeps a path prefix and adds /v1 unless it is already the
// last path segment: /gateway -> /gateway/v1/messages; /gateway/v1 -> /gateway/v1/messages.
func ClaudeAPIEndpoint(baseURL, resource string) (string, error) {
	base, err := NormalizeClaudeBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base + "/" + resource, nil
}

func (o *ClaudeAuth) FetchModelsForAccount(ctx context.Context, account *Account) ([]string, error) {
	if account == nil {
		return nil, fmt.Errorf("missing Claude account")
	}
	account.mu.RLock()
	kind, token, base := account.ClaudeAuthKind, account.AccessToken, account.ClaudeBaseURL
	customHeaders, identityMode := cloneStringMap(account.CustomHeaders), account.ClaudeFingerprintMode
	account.mu.RUnlock()
	return o.FetchModelsWithCredentialsAndHeaders(ctx, token, kind, base, customHeaders, identityMode)
}

// FetchModelsWithCredentials uses API Key authentication only for explicitly
// marked accounts. OAuth and Setup Token keep their existing discovery path.
func (o *ClaudeAuth) FetchModelsWithCredentials(ctx context.Context, token, kind, baseURL string) ([]string, error) {
	return o.FetchModelsWithCredentialsAndHeaders(ctx, token, kind, baseURL, nil, "")
}

// FetchModelsWithCredentialsAndHeaders is FetchModelsWithCredentials for API
// Key accounts that carry operator custom headers and/or the optional Claude
// Code client identity: a gateway that gates /v1/messages on those
// characteristics gates /v1/models the same way, so discovery must present the
// identical request shape. Both extras are ignored for OAuth/Setup Token.
func (o *ClaudeAuth) FetchModelsWithCredentialsAndHeaders(ctx context.Context, token, kind, baseURL string, customHeaders map[string]string, identityMode string) ([]string, error) {
	if NormalizeClaudeAuthKind(kind, false) != ClaudeAuthKindAPIKey {
		return o.FetchModels(ctx, token)
	}
	endpoint, err := ClaudeAPIEndpoint(baseURL, "models")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("missing Claude API key")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := *o.fallback
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var ids []string
	seen := make(map[string]bool)
	after := ""
	for page := 0; page < 10; page++ {
		query := url.Values{"limit": {"100"}}
		if after != "" {
			query.Set("after_id", after)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", token)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("User-Agent", "Codex2API")
		ApplyClaudeAPIKeyIdentityHeaders(req.Header, nil, identityMode)
		ApplyClaudeAPIKeyCustomHeaders(req.Header, customHeaders)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Claude model discovery request failed: %w", err)
		}
		body, readErr := readClaudeOAuthResponseBody(resp)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Claude model discovery returned status %d", resp.StatusCode)
		}
		var parsed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("invalid Claude model discovery response")
		}
		for _, model := range parsed.Data {
			id := strings.TrimSpace(model.ID)
			if strings.HasPrefix(strings.ToLower(id), "claude-") && !seen[strings.ToLower(id)] {
				seen[strings.ToLower(id)] = true
				ids = append(ids, id)
			}
		}
		if !parsed.HasMore || parsed.LastID == "" || parsed.LastID == after {
			break
		}
		after = parsed.LastID
	}
	return ids, nil
}

// claudeAPIKeyReservedHeaders are request headers an API Key account's
// custom_headers may never override. The gateway owns authentication (the key
// lives in access_token), body framing, and the Accept negotiation derived
// from the request body's stream flag; letting operators rewrite these would
// either leak a second secret into a plain config map or break SSE parsing.
var claudeAPIKeyReservedHeaders = map[string]struct{}{
	"authorization":     {},
	"x-api-key":         {},
	"content-type":      {},
	"content-length":    {},
	"host":              {},
	"transfer-encoding": {},
	"connection":        {},
	"accept":            {},
	"accept-encoding":   {},
}

// IsClaudeAPIKeyReservedHeader reports whether name is owned by the gateway on
// the Claude API Key outbound path and therefore rejected from custom_headers.
func IsClaudeAPIKeyReservedHeader(name string) bool {
	_, reserved := claudeAPIKeyReservedHeaders[strings.ToLower(strings.TrimSpace(name))]
	return reserved
}

// DefaultClaudeIdentityHeaderValue returns the deterministic Claude Code CLI
// identity value for one of ClaudeIdentityHeaderNames. It is provider-shaped and
// fixed (only the CLI version follows EffectiveClaudeCLIVersion) so both the
// OAuth force-mode fallback for legacy accounts and the API Key client-identity
// emulation present a stable identity instead of a per-request random one.
func DefaultClaudeIdentityHeaderValue(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "user-agent":
		return "claude-cli/" + EffectiveClaudeCLIVersion() + " (external, cli)"
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

// ApplyClaudeAPIKeyIdentityHeaders optionally gives an API Key request the
// basic client characteristics of Claude Code (issue #647). mode reuses the
// account-level claude_fingerprint_mode values with API Key semantics:
//
//   - ""       — off (default): no identity headers are added; the request keeps
//     the neutral API Key contract.
//   - preserve — a real Claude Code CLI client keeps its own identity headers and
//     only missing ones are filled; any other client is treated like force so the
//     upstream never sees a half-CLI identity.
//   - force    — every identity header is set to the deterministic CLI identity.
//
// Only headers are touched: no OAuth session state, system prompt preamble or
// metadata identity is copied. Operator custom_headers are applied afterwards
// by the caller and win over these values.
func ApplyClaudeAPIKeyIdentityHeaders(dst http.Header, incoming http.Header, mode string) {
	if dst == nil {
		return
	}
	mode = NormalizeClaudeFingerprintMode(mode)
	if mode == "" {
		return
	}
	setIfAbsentFromIncoming := func(name, value string) {
		if v := strings.TrimSpace(incoming.Get(name)); v != "" {
			dst.Set(name, v)
			return
		}
		dst.Set(name, value)
	}
	// Fixed SDK behaviour headers the real CLI always sends.
	setIfAbsentFromIncoming("X-Stainless-Retry-Count", "0")
	setIfAbsentFromIncoming("X-Stainless-Timeout", "600")
	setIfAbsentFromIncoming("anthropic-dangerous-direct-browser-access", "true")
	force := mode == ClaudeFingerprintModeForce
	if !force {
		if _, isCLI := ParseClaudeClientVersion(strings.TrimSpace(incoming.Get("User-Agent"))); !isCLI {
			force = true
		}
	}
	for _, name := range ClaudeIdentityHeaderNames {
		if !force {
			if v := strings.TrimSpace(incoming.Get(name)); v != "" {
				dst.Set(name, v)
				continue
			}
		}
		if v := DefaultClaudeIdentityHeaderValue(name); v != "" {
			dst.Set(name, v)
		}
	}
}

// ApplyClaudeAPIKeyCustomHeaders applies operator-configured account headers
// last so they override both the neutral defaults and the optional identity
// emulation, mirroring how Codex and Grok accounts treat custom_headers.
// Reserved gateway-owned headers are skipped even if a legacy row contains them.
func ApplyClaudeAPIKeyCustomHeaders(dst http.Header, headers map[string]string) {
	if dst == nil {
		return
	}
	for rawName, value := range headers {
		name := strings.TrimSpace(rawName)
		if name == "" || IsClaudeAPIKeyReservedHeader(name) {
			continue
		}
		dst.Set(name, value)
	}
}

// ClaudeAPIKeyUpstreamUserAgent predicts the User-Agent an API Key account
// presents upstream when the downstream client sends none: an explicit
// custom header wins, then the emulated CLI identity, otherwise empty (the
// neutral passthrough contract: inbound UA or "Codex2API").
func ClaudeAPIKeyUpstreamUserAgent(customHeaders map[string]string, mode string) string {
	for name, value := range customHeaders {
		if strings.EqualFold(strings.TrimSpace(name), "user-agent") && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if NormalizeClaudeFingerprintMode(mode) != "" {
		return DefaultClaudeIdentityHeaderValue("user-agent")
	}
	return ""
}
