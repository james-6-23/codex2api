package admin

import (
	"strings"
	"time"

	"github.com/codex2api/auth"
)

type tokenCredentialSeed struct {
	refreshToken string
	accessToken  string
	idToken      string
	accountID    string
	email        string
	planType     string
	expiresAt    time.Time
	expiresAtRaw string
	expiresIn    int64
}

func normalizeTokenCredentialSeed(seed tokenCredentialSeed) tokenCredentialSeed {
	seed.refreshToken = strings.TrimSpace(seed.refreshToken)
	seed.accessToken = strings.TrimSpace(seed.accessToken)
	seed.idToken = strings.TrimSpace(seed.idToken)
	seed.accountID = strings.TrimSpace(seed.accountID)
	seed.email = strings.TrimSpace(seed.email)
	seed.planType = strings.TrimSpace(seed.planType)
	seed.expiresAtRaw = strings.TrimSpace(seed.expiresAtRaw)

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
	}

	if seed.expiresAt.IsZero() && seed.expiresAtRaw != "" {
		seed.expiresAt = parseCredentialExpiresAt(seed.expiresAtRaw)
	}
	if seed.expiresAt.IsZero() && seed.expiresIn > 0 {
		seed.expiresAt = time.Now().Add(time.Duration(seed.expiresIn) * time.Second)
	}
	if seed.expiresAt.IsZero() && seed.accessToken != "" {
		if info := auth.ParseAccessToken(seed.accessToken); info != nil && !info.ExpiresAt.IsZero() {
			seed.expiresAt = info.ExpiresAt
		}
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
	}
	return info
}

func tokenCredentialMap(seed tokenCredentialSeed) map[string]interface{} {
	seed = normalizeTokenCredentialSeed(seed)
	credentials := make(map[string]interface{})
	if seed.refreshToken != "" {
		credentials["refresh_token"] = seed.refreshToken
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
	return credentials
}

func accountFromCredentialSeed(id int64, proxyURL string, seed tokenCredentialSeed) *auth.Account {
	seed = normalizeTokenCredentialSeed(seed)
	return &auth.Account{
		DBID:         id,
		RefreshToken: seed.refreshToken,
		AccessToken:  seed.accessToken,
		ExpiresAt:    seed.expiresAt,
		AccountID:    seed.accountID,
		Email:        seed.email,
		PlanType:     seed.planType,
		ProxyURL:     proxyURL,
		Status:       auth.StatusReady,
	}
}

func parseCredentialExpiresAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
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
