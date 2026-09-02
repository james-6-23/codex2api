package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

const antigravityImportFileMaxBytes = 4 << 20

type antigravityImportDefaults struct {
	OAuthClientKey string
	ClientID       string
	ClientSecret   string
}

// antigravityImportDocument preserves account-level metadata that cannot be
// represented by auth.AntigravityCredential. In particular, exported API-key
// accounts must bypass the OAuth sync path and retain their model catalog,
// mapping, enabled state, display name, and pinned proxy.
type antigravityImportDocument struct {
	AuthKind        string
	Credential      auth.AntigravityCredential
	APIKey          string
	Models          []string
	ModelMapping    string
	Disabled        bool
	DisabledPresent bool
	Name            string
	ProxyURL        string
	// ProxyEnabled 是源端导出的代理启用状态，nil 表示文件没写。只用于导入时的
	// 告警统计：代理一律以启用态注册，见 countDisabledAtSource。
	ProxyEnabled *bool
}

func parseAntigravityImportContent(content string, defaults antigravityImportDefaults) ([]auth.AntigravityCredential, error) {
	documents, err := parseAntigravityImportDocuments(content, defaults)
	if err != nil {
		return nil, err
	}
	credentials := make([]auth.AntigravityCredential, 0, len(documents))
	for _, document := range documents {
		if document.AuthKind != auth.AntigravityAuthKindOAuth {
			// The legacy caller always runs OAuth synchronization. Fail closed so
			// an API key is never mistaken for a Google OAuth bearer.
			return nil, errors.New("credential JSON contains an API-key account that requires the document import path")
		}
		credentials = append(credentials, document.Credential)
	}
	return credentials, nil
}

// parseAntigravityImportDocuments is the lossless parser used for portable
// Antigravity exports. It also accepts the legacy manager, credential-store,
// raw refresh-token, JSON string, array, and {"accounts": [...]} formats.
func parseAntigravityImportDocuments(content string, defaults antigravityImportDefaults) ([]antigravityImportDocument, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("credential content is empty")
	}
	if len(content) > antigravityImportFileMaxBytes {
		return nil, fmt.Errorf("credential content exceeds %d bytes", antigravityImportFileMaxBytes)
	}
	if content[0] != '{' && content[0] != '[' && content[0] != '"' {
		return []antigravityImportDocument{{
			AuthKind: auth.AntigravityAuthKindOAuth,
			Credential: auth.AntigravityCredential{
				RefreshToken: content, OAuthClientKey: defaults.OAuthClientKey,
				ClientID: defaults.ClientID, ClientSecret: defaults.ClientSecret,
			},
		}}, nil
	}

	decoder := json.NewDecoder(bytes.NewBufferString(content))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse credential JSON: %w", err)
	}
	documents, err := collectAntigravityImportDocuments(value, defaults)
	if err != nil {
		return nil, err
	}
	if len(documents) == 0 {
		return nil, errors.New("credential JSON contains no Antigravity credential")
	}
	return documents, nil
}

func collectAntigravityImportDocuments(value any, defaults antigravityImportDefaults) ([]antigravityImportDocument, error) {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, nil
		}
		return []antigravityImportDocument{{
			AuthKind: auth.AntigravityAuthKindOAuth,
			Credential: auth.AntigravityCredential{
				RefreshToken: strings.TrimSpace(typed), OAuthClientKey: defaults.OAuthClientKey,
				ClientID: defaults.ClientID, ClientSecret: defaults.ClientSecret,
			},
		}}, nil
	case []any:
		result := make([]antigravityImportDocument, 0, len(typed))
		for index, item := range typed {
			documents, err := collectAntigravityImportDocuments(item, defaults)
			if err != nil {
				return nil, fmt.Errorf("credential item %d: %w", index+1, err)
			}
			result = append(result, documents...)
		}
		return result, nil
	case map[string]any:
		if accounts, ok := antigravityMapValue(typed, "accounts").([]any); ok {
			return collectAntigravityImportDocuments(accounts, defaults)
		}
		document, err := antigravityImportDocumentFromMap(typed, defaults)
		if err != nil {
			return nil, err
		}
		return []antigravityImportDocument{document}, nil
	default:
		return nil, fmt.Errorf("unsupported credential JSON type %T", value)
	}
}

func antigravityImportDocumentFromMap(root map[string]any, defaults antigravityImportDefaults) (antigravityImportDocument, error) {
	token := root
	if nested, ok := antigravityMapValue(root, "token", "credential", "credentials").(map[string]any); ok {
		token = nested
	}
	credential := antigravityCredentialFromMap(root, defaults)
	apiKey := firstAntigravityString(root, token, "api_key", "apiKey")
	authKind, authKindPresent, err := antigravityImportAuthKind(root)
	if err != nil {
		return antigravityImportDocument{}, err
	}
	if !authKindPresent {
		if apiKey != "" {
			authKind = auth.AntigravityAuthKindAPIKey
		} else {
			authKind = auth.AntigravityAuthKindOAuth
		}
	}

	hasOAuthToken := credential.AccessToken != "" || credential.RefreshToken != ""
	switch authKind {
	case auth.AntigravityAuthKindAPIKey:
		if apiKey == "" {
			return antigravityImportDocument{}, errors.New("API-key credential object has no api_key")
		}
		if hasOAuthToken {
			return antigravityImportDocument{}, errors.New("credential object mixes API-key and OAuth secrets")
		}
		// Defaults belong only to OAuth. Keep the identity metadata while
		// ensuring later integration cannot accidentally treat these as OAuth
		// client credentials.
		credential.OAuthClientKey = ""
		credential.ClientID = ""
		credential.ClientSecret = ""
	case auth.AntigravityAuthKindOAuth:
		if apiKey != "" {
			return antigravityImportDocument{}, errors.New("credential object mixes OAuth and API-key secrets")
		}
		if !hasOAuthToken {
			return antigravityImportDocument{}, errors.New("credential object has no access_token or refresh_token")
		}
	default:
		return antigravityImportDocument{}, errors.New("credential object has unsupported auth_kind")
	}

	models, err := antigravityImportModels(root)
	if err != nil {
		return antigravityImportDocument{}, err
	}
	modelMapping, err := antigravityImportModelMapping(root)
	if err != nil {
		return antigravityImportDocument{}, err
	}
	disabled, disabledPresent, err := antigravityImportDisabled(root)
	if err != nil {
		return antigravityImportDocument{}, err
	}
	document := antigravityImportDocument{
		AuthKind: authKind, Credential: credential, APIKey: apiKey,
		Models: models, ModelMapping: modelMapping,
		Disabled: disabled, DisabledPresent: disabledPresent,
		Name:     credential.Name,
		ProxyURL: antigravityString(root, "proxy_url", "proxyUrl", "proxy"),
	}
	if proxyEnabled, present := antigravityBool(root, "proxy_enabled", "proxyEnabled"); present {
		document.ProxyEnabled = &proxyEnabled
	}
	return document, nil
}

func antigravityImportAuthKind(values map[string]any) (string, bool, error) {
	for _, key := range []string{"auth_kind", "authKind"} {
		raw, ok := values[key]
		if !ok || raw == nil {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return "", true, errors.New("credential object has invalid auth_kind")
		}
		switch {
		case strings.EqualFold(strings.TrimSpace(value), auth.AntigravityAuthKindOAuth):
			return auth.AntigravityAuthKindOAuth, true, nil
		case strings.EqualFold(strings.TrimSpace(value), auth.AntigravityAuthKindAPIKey):
			return auth.AntigravityAuthKindAPIKey, true, nil
		default:
			return "", true, errors.New("credential object has unsupported auth_kind")
		}
	}
	return "", false, nil
}

func antigravityImportModels(values map[string]any) ([]string, error) {
	raw, ok := values["models"]
	if !ok || raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("credential object has invalid models")
	}
	models := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		model, ok := item.(string)
		if !ok {
			return nil, errors.New("credential object has invalid models")
		}
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return models, nil
}

func antigravityImportModelMapping(values map[string]any) (string, error) {
	raw := antigravityMapValue(values, "model_mapping", "modelMapping")
	if raw == nil {
		return "", nil
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed), nil
	case map[string]any:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "", errors.New("credential object has invalid model_mapping")
		}
		return string(encoded), nil
	default:
		return "", errors.New("credential object has invalid model_mapping")
	}
}

func antigravityImportDisabled(values map[string]any) (bool, bool, error) {
	for _, key := range []string{"disabled", "is_disabled", "isDisabled"} {
		raw, ok := values[key]
		if !ok || raw == nil {
			continue
		}
		if disabled, parsed := antigravityBool(map[string]any{key: raw}, key); parsed {
			return disabled, true, nil
		}
		return false, true, errors.New("credential object has invalid disabled state")
	}
	return false, false, nil
}

func antigravityCredentialFromMap(root map[string]any, defaults antigravityImportDefaults) auth.AntigravityCredential {
	token := root
	if nested, ok := antigravityMapValue(root, "token", "credential", "credentials").(map[string]any); ok {
		token = nested
	}
	credential := auth.AntigravityCredential{
		AccessToken:    antigravityString(token, "access_token", "accessToken"),
		RefreshToken:   antigravityString(token, "refresh_token", "refreshToken"),
		IDToken:        antigravityString(token, "id_token", "idToken"),
		ProjectID:      firstAntigravityString(token, root, "project_id", "projectId"),
		OAuthClientKey: firstAntigravityString(token, root, "oauth_client_key", "oauthClientKey"),
		ClientID:       firstAntigravityString(token, root, "client_id", "clientId"),
		ClientSecret:   firstAntigravityString(token, root, "client_secret", "clientSecret"),
		Scope:          firstAntigravityString(token, root, "scope", "scopes"),
		Email:          firstAntigravityString(root, token, "email"),
		Name:           firstAntigravityString(root, token, "name"),
		AvatarURL:      firstAntigravityString(root, token, "avatar_url", "avatarUrl", "picture"),
	}
	if verified, ok := antigravityBool(token, "verified_email", "verifiedEmail"); ok {
		credential.VerifiedEmail = verified
		credential.VerifiedEmailPresent = true
	} else if verified, ok := antigravityBool(root, "verified_email", "verifiedEmail"); ok {
		credential.VerifiedEmail = verified
		credential.VerifiedEmailPresent = true
	}
	if credential.OAuthClientKey == "" {
		credential.OAuthClientKey = defaults.OAuthClientKey
	}
	if credential.ClientID == "" {
		credential.ClientID = defaults.ClientID
	}
	if credential.ClientSecret == "" {
		credential.ClientSecret = defaults.ClientSecret
	}
	credential.ExpiresAt = antigravityExpiry(token)
	if credential.ExpiresAt.IsZero() {
		credential.ExpiresAt = antigravityExpiry(root)
	}
	return credential
}

func antigravityExpiry(values map[string]any) time.Time {
	for _, key := range []string{"expiry_timestamp", "expiryTimestamp", "expiry_date", "expiryDate"} {
		if seconds, ok := antigravityNumber(values[key]); ok {
			if seconds > 1_000_000_000_000 {
				seconds /= 1000
			}
			if seconds > 0 {
				return time.Unix(int64(seconds), 0).UTC()
			}
		}
	}
	for _, key := range []string{"expiry", "expires_at", "expiresAt", "expiration", "expiry_date", "expiryDate"} {
		if raw, ok := values[key].(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
				return parsed.UTC()
			}
		}
	}
	if seconds, ok := antigravityNumber(values["expires_in"]); ok && seconds > 0 {
		return time.Now().Add(time.Duration(seconds) * time.Second).UTC()
	}
	return time.Time{}
}

func antigravityNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case float64:
		return typed, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func antigravityBool(values map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		value, ok := values[key]
		if !ok || value == nil {
			continue
		}
		switch typed := value.(type) {
		case bool:
			return typed, true
		case string:
			parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
			if err == nil {
				return parsed, true
			}
		case json.Number:
			parsed, err := typed.Int64()
			if err == nil && (parsed == 0 || parsed == 1) {
				return parsed == 1, true
			}
		case float64:
			if typed == 0 || typed == 1 {
				return typed == 1, true
			}
		}
	}
	return false, false
}

func antigravityMapValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func antigravityString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstAntigravityString(primary, fallback map[string]any, keys ...string) string {
	if value := antigravityString(primary, keys...); value != "" {
		return value
	}
	return antigravityString(fallback, keys...)
}
