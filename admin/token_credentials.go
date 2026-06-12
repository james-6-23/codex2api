package admin

import (
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

type tokenCredentialSeed struct {
	refreshToken          string
	sessionToken          string
	accessToken           string
	idToken               string
	accountID             string
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
	codexUsageUpdatedAt   string
}

func normalizeTokenCredentialSeed(seed tokenCredentialSeed) tokenCredentialSeed {
	seed.refreshToken = strings.TrimSpace(seed.refreshToken)
	seed.sessionToken = strings.TrimSpace(seed.sessionToken)
	seed.accessToken = strings.TrimSpace(seed.accessToken)
	seed.idToken = strings.TrimSpace(seed.idToken)
	seed.accountID = strings.TrimSpace(seed.accountID)
	seed.email = strings.TrimSpace(seed.email)
	seed.planType = strings.TrimSpace(seed.planType)
	seed.expiresAtRaw = strings.TrimSpace(seed.expiresAtRaw)
	seed.codex7DUsedPercent = strings.TrimSpace(seed.codex7DUsedPercent)
	seed.codex7DResetAt = strings.TrimSpace(seed.codex7DResetAt)
	seed.codex5HUsedPercent = strings.TrimSpace(seed.codex5HUsedPercent)
	seed.codex5HResetAt = strings.TrimSpace(seed.codex5HResetAt)
	seed.codexUsageUpdatedAt = strings.TrimSpace(seed.codexUsageUpdatedAt)

	if info := accountInfoFromTokens(seed.idToken, seed.accessToken); info != nil {
		if seed.accountID == "" {
			seed.accountID = info.ChatGPTAccountID
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

	if seed.expiresAt.IsZero() && seed.expiresIn > 0 {
		seed.expiresAt = time.Now().Add(time.Duration(seed.expiresIn) * time.Second)
	}
	if seed.expiresAt.IsZero() && seed.accessToken != "" {
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

func accountInfoFromTokens(idToken, accessToken string) *auth.AccountInfo {
	info := auth.ParseIDToken(strings.TrimSpace(idToken))
	if info == nil {
		info = &auth.AccountInfo{}
	}
	if atInfo := auth.ParseAccessToken(strings.TrimSpace(accessToken)); atInfo != nil {
		if info.ChatGPTAccountID == "" {
			info.ChatGPTAccountID = atInfo.ChatGPTAccountID
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
	if seed.idToken != "" {
		credentials["id_token"] = seed.idToken
	}
	if !seed.expiresAt.IsZero() {
		credentials["expires_at"] = seed.expiresAt.Format(time.RFC3339)
	}
	if seed.accountID != "" {
		credentials["account_id"] = seed.accountID
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
	if seed.codexUsageUpdatedAt != "" {
		credentials["codex_usage_updated_at"] = seed.codexUsageUpdatedAt
	}
	return credentials
}

func accountFromCredentialSeed(id int64, proxyURL string, proxyID *int64, seed tokenCredentialSeed) *auth.Account {
	seed = normalizeTokenCredentialSeed(seed)
	account := &auth.Account{
		DBID:                  id,
		RefreshToken:          seed.refreshToken,
		SessionToken:          seed.sessionToken,
		AccessToken:           seed.accessToken,
		ExpiresAt:             seed.expiresAt,
		AccountID:             seed.accountID,
		Email:                 seed.email,
		PlanType:              seed.planType,
		ProxyURL:              proxyURL,
		ProxyID:               cloneOptionalInt64(proxyID),
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
		account.SetUsageSnapshot5h(pct, parseSeedRFC3339(seed.codex5HResetAt))
	}
	return account
}

func cloneOptionalInt64(value *int64) *int64 {
	if value == nil || *value <= 0 {
		return nil
	}
	cloned := *value
	return &cloned
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
