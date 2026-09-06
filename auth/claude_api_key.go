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
	account.mu.RUnlock()
	return o.FetchModelsWithCredentials(ctx, token, kind, base)
}

// FetchModelsWithCredentials uses API Key authentication only for explicitly
// marked accounts. OAuth and Setup Token keep their existing discovery path.
func (o *ClaudeAuth) FetchModelsWithCredentials(ctx context.Context, token, kind, baseURL string) ([]string, error) {
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
			if id != "" && !seen[strings.ToLower(id)] {
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
