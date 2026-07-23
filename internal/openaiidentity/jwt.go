package openaiidentity

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

type tokenClaims struct {
	Email string `json:"email"`
	Auth  *struct {
		WorkspaceID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
	Profile *struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
}

// TokenIdentity returns email and workspace ID only when both come from one JWT.
func TokenIdentity(idToken, accessToken string) (string, string) {
	if email, workspaceID := identityFromJWT(idToken, false); email != "" && workspaceID != "" {
		return email, workspaceID
	}
	return identityFromJWT(accessToken, true)
}

func identityFromJWT(token string, accessToken bool) (string, string) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}

	var claims tokenClaims
	if json.Unmarshal(payload, &claims) != nil || claims.Auth == nil {
		return "", ""
	}
	email := claims.Email
	if accessToken {
		if claims.Profile == nil {
			return "", ""
		}
		email = claims.Profile.Email
	}
	email = strings.TrimSpace(email)
	workspaceID := NormalizeWorkspaceID(claims.Auth.WorkspaceID)
	if email == "" || workspaceID == "" {
		return "", ""
	}
	return email, workspaceID
}

func NormalizeWorkspaceID(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "user-") {
		return ""
	}
	return value
}
