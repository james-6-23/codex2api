package admin

import (
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/openaiidentity"
)

type tokenCredentialSeed struct {
	refreshToken    string
	sessionToken    string
	accessToken     string
	accessTokenType string
	idToken         string
	workspaceID     string
	// userID 是 OpenAI 用户 ID，仅作为账号元数据保存，不参与 workspace 身份去重。
	userID string
	// allowDuplicate 仅允许 workspace_id 为空的凭证绕过 RT/ST/AT 原文去重；
	// 已确认 workspace 身份时仍必须保持 email + workspace_id 唯一。
	allowDuplicate        bool
	email                 string
	planType              string
	expiresAt             time.Time
	expiresAtRaw          string
	expiresIn             int64
	subscriptionExpiresAt time.Time
	codex7DUsedPercent    string
	codex7DResetAt        string
	codex5HUsedPercent    string
	codex5HResetAt        string
	codex5HUsageUpdatedAt string
	codexUsageUpdatedAt   string
	customHeaders         map[string]string
}

func normalizeTokenCredentialSeed(seed tokenCredentialSeed) tokenCredentialSeed {
	seed.refreshToken = strings.TrimSpace(seed.refreshToken)
	seed.sessionToken = strings.TrimSpace(seed.sessionToken)
	seed.accessToken = strings.TrimSpace(seed.accessToken)
	seed.accessTokenType = strings.TrimSpace(seed.accessTokenType)
	seed.idToken = strings.TrimSpace(seed.idToken)
	seed.workspaceID = openaiidentity.NormalizeWorkspaceID(seed.workspaceID)
	seed.userID = strings.TrimSpace(seed.userID)
	seed.email = strings.TrimSpace(seed.email)
	seed.planType = strings.TrimSpace(seed.planType)
	seed.expiresAtRaw = strings.TrimSpace(seed.expiresAtRaw)
	seed.codex7DUsedPercent = strings.TrimSpace(seed.codex7DUsedPercent)
	seed.codex7DResetAt = strings.TrimSpace(seed.codex7DResetAt)
	seed.codex5HUsedPercent = strings.TrimSpace(seed.codex5HUsedPercent)
	seed.codex5HResetAt = strings.TrimSpace(seed.codex5HResetAt)
	seed.codex5HUsageUpdatedAt = strings.TrimSpace(seed.codex5HUsageUpdatedAt)
	seed.codexUsageUpdatedAt = strings.TrimSpace(seed.codexUsageUpdatedAt)
	if seed.accessTokenType == "" {
		seed.accessTokenType = accessTokenTypeForToken(seed.accessToken)
	}

	accessTokenForJWT := seed.accessToken
	if seed.accessTokenType == accessTokenTypeCodexAT {
		accessTokenForJWT = ""
	}
	if info := accountInfoFromTokens(seed.idToken, accessTokenForJWT); info != nil {
		if seed.workspaceID == "" {
			seed.workspaceID = info.ChatGPTAccountID
		}
		if seed.userID == "" {
			seed.userID = info.UserID
		}
		if seed.email == "" {
			seed.email = info.Email
		}
		if seed.planType == "" {
			seed.planType = info.PlanType
		}
		if seed.subscriptionExpiresAt.IsZero() && !info.SubscriptionExpiresAt.IsZero() {
			seed.subscriptionExpiresAt = info.SubscriptionExpiresAt
		}
	}
	// 付费套餐下已过去的到期时间是续费前的陈旧 JWT claim，导入时不落库。(issue #360)
	if auth.StaleSubscriptionExpiry(seed.planType, seed.subscriptionExpiresAt, time.Now()) {
		seed.subscriptionExpiresAt = time.Time{}
	}

	if seed.expiresAt.IsZero() && seed.expiresIn > 0 {
		seed.expiresAt = time.Now().Add(time.Duration(seed.expiresIn) * time.Second)
	}
	if seed.expiresAt.IsZero() && seed.accessToken != "" && seed.accessTokenType != accessTokenTypeCodexAT {
		if info := auth.ParseAccessToken(seed.accessToken); info != nil && !info.ExpiresAt.IsZero() {
			seed.expiresAt = info.ExpiresAt
		}
	}
	if seed.expiresAt.IsZero() && seed.expiresAtRaw != "" {
		seed.expiresAt = parseCredentialExpiresAt(seed.expiresAtRaw)
	}
	if seed.expiresAt.IsZero() && seed.accessToken != "" {
		seed.expiresAt = time.Now().Add(time.Hour)
	}

	return seed
}

const accessTokenTypeCodexAT = "codex_at"

func accessTokenTypeForToken(accessToken string) string {
	if strings.HasPrefix(strings.TrimSpace(accessToken), "at-") {
		return accessTokenTypeCodexAT
	}
	return ""
}

func accountInfoFromTokens(idToken, accessToken string) *auth.AccountInfo {
	info := auth.ParseIDToken(strings.TrimSpace(idToken))
	if info == nil {
		info = &auth.AccountInfo{}
	}
	if atInfo := auth.ParseAccessToken(strings.TrimSpace(accessToken)); atInfo != nil {
		if info.ChatGPTAccountID == "" {
			info.ChatGPTAccountID = atInfo.ChatGPTAccountID
		}
		if info.UserID == "" {
			info.UserID = atInfo.UserID
		}
		if info.Email == "" {
			info.Email = atInfo.Email
		}
		if info.PlanType == "" {
			info.PlanType = atInfo.PlanType
		}
		if info.SubscriptionExpiresAt.IsZero() && !atInfo.SubscriptionExpiresAt.IsZero() {
			info.SubscriptionExpiresAt = atInfo.SubscriptionExpiresAt
		}
	}
	return info
}

func tokenCredentialMap(seed tokenCredentialSeed) map[string]interface{} {
	seed = normalizeTokenCredentialSeed(seed)
	credentials := make(map[string]interface{})
	if seed.refreshToken != "" {
		credentials["refresh_token"] = seed.refreshToken
	}
	if seed.sessionToken != "" {
		credentials["session_token"] = seed.sessionToken
	}
	if seed.accessToken != "" {
		credentials["access_token"] = seed.accessToken
	}
	if seed.accessTokenType != "" {
		credentials["access_token_type"] = seed.accessTokenType
	}
	if seed.idToken != "" {
		credentials["id_token"] = seed.idToken
	}
	if !seed.expiresAt.IsZero() {
		credentials["expires_at"] = seed.expiresAt.Format(time.RFC3339)
	}
	if seed.workspaceID != "" {
		credentials["workspace_id"] = seed.workspaceID
	}
	if seed.userID != "" {
		credentials["user_id"] = seed.userID
	}
	if seed.allowDuplicate && seed.workspaceID == "" {
		credentials["allow_duplicate"] = "true"
	}
	if seed.email != "" {
		credentials["email"] = seed.email
	}
	if seed.planType != "" {
		credentials["plan_type"] = seed.planType
	}
	if !seed.subscriptionExpiresAt.IsZero() {
		credentials["subscription_expires_at"] = seed.subscriptionExpiresAt.Format(time.RFC3339)
	}
	if seed.codex7DUsedPercent != "" {
		credentials["codex_7d_used_percent"] = seed.codex7DUsedPercent
	}
	if seed.codex7DResetAt != "" {
		credentials["codex_7d_reset_at"] = seed.codex7DResetAt
	}
	if seed.codex5HUsedPercent != "" {
		credentials["codex_5h_used_percent"] = seed.codex5HUsedPercent
	}
	if seed.codex5HResetAt != "" {
		credentials["codex_5h_reset_at"] = seed.codex5HResetAt
	}
	if seed.codex5HUsageUpdatedAt != "" {
		credentials["codex_5h_usage_updated_at"] = seed.codex5HUsageUpdatedAt
	}
	if seed.codexUsageUpdatedAt != "" {
		credentials["codex_usage_updated_at"] = seed.codexUsageUpdatedAt
	}
	if len(seed.customHeaders) > 0 {
		credentials["custom_headers"] = cloneCustomHeaders(seed.customHeaders)
	}
	return credentials
}

func accountFromCredentialSeed(id int64, proxyURL string, seed tokenCredentialSeed) *auth.Account {
	seed = normalizeTokenCredentialSeed(seed)
	account := &auth.Account{
		DBID:                  id,
		RefreshToken:          seed.refreshToken,
		SessionToken:          seed.sessionToken,
		AccessToken:           seed.accessToken,
		ExpiresAt:             seed.expiresAt,
		AccountID:             seed.workspaceID,
		Email:                 seed.email,
		PlanType:              seed.planType,
		ProxyURL:              proxyURL,
		CustomHeaders:         cloneCustomHeaders(seed.customHeaders),
		Status:                auth.StatusReady,
		SubscriptionExpiresAt: seed.subscriptionExpiresAt,
	}
	if pct, ok := parseSeedUsagePercent(seed.codex7DUsedPercent); ok {
		updatedAt := parseSeedRFC3339(seed.codexUsageUpdatedAt)
		account.SetUsageSnapshot(pct, updatedAt)
		if resetAt := parseSeedRFC3339(seed.codex7DResetAt); !resetAt.IsZero() {
			account.SetReset7dAt(resetAt)
		}
	}
	if pct, ok := parseSeedUsagePercent(seed.codex5HUsedPercent); ok {
		account.SetUsageSnapshot5hAt(pct, parseSeedRFC3339(seed.codex5HResetAt), parseSeedRFC3339(seed.codex5HUsageUpdatedAt))
	}
	return account
}

func parseSeedUsagePercent(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func parseSeedRFC3339(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func parseCredentialExpiresAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	if unixSeconds, err := strconv.ParseFloat(raw, 64); err == nil && unixSeconds > 0 {
		if unixSeconds >= 1e12 {
			return time.UnixMilli(int64(unixSeconds))
		}
		return time.Unix(int64(unixSeconds), 0)
	}

	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}
