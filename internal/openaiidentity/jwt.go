package openaiidentity

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type Claims struct {
	Email         string         `json:"email"`
	Subject       string         `json:"sub"`
	ExpiresAt     int64          `json:"exp"`
	OpenAIAuth    *OpenAIAuth    `json:"https://api.openai.com/auth"`
	OpenAIProfile *OpenAIProfile `json:"https://api.openai.com/profile"`
}

type OpenAIAuth struct {
	ChatGPTAccountID               string `json:"chatgpt_account_id"`
	UserID                         string `json:"user_id"`
	ChatGPTUserID                  string `json:"chatgpt_user_id"`
	PlanType                       string `json:"chatgpt_plan_type"`
	ChatGPTSubscriptionActiveUntil string `json:"chatgpt_subscription_active_until"`
}

type OpenAIProfile struct {
	Email string `json:"email"`
}

func ParseJWT(token string) (*Claims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		payload, err = base64.RawStdEncoding.DecodeString(parts[1])
	}
	if err != nil {
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("decode JWT payload: %w", err)
		}
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("decode JWT claims: %w", err)
	}
	return &claims, nil
}

func (c *Claims) WorkspaceID() string {
	if c == nil || c.OpenAIAuth == nil {
		return ""
	}
	return NormalizeWorkspaceID(c.OpenAIAuth.ChatGPTAccountID)
}

func NormalizeWorkspaceID(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "user-") {
		return ""
	}
	return value
}
