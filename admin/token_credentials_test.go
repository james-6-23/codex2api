package admin

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func makeAdminTestJWT(t *testing.T, claims map[string]interface{}) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".fake_signature"
}

func TestNormalizeTokenCredentialSeedPrefersAccessTokenExpiry(t *testing.T) {
	accessExpiresAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	rawExpired := time.Now().Add(-time.Hour).Format(time.RFC3339)
	accessToken := makeAdminTestJWT(t, map[string]interface{}{
		"exp": accessExpiresAt.Unix(),
	})

	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
		accessToken:  accessToken,
		expiresAtRaw: rawExpired,
	})

	if !seed.expiresAt.Equal(accessExpiresAt) {
		t.Fatalf("expiresAt = %s, want access token expiry %s", seed.expiresAt, accessExpiresAt)
	}
}

func TestNormalizeTokenCredentialSeedTreatsCodexATAsOpaque(t *testing.T) {
	accessExpiresAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	rawJWT := makeAdminTestJWT(t, map[string]interface{}{
		"exp": accessExpiresAt.Unix(),
		"https://api.openai.com/profile": map[string]interface{}{
			"email": "jwt@example.com",
		},
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "acc-from-jwt",
			"chatgpt_plan_type":  "team",
		},
	})
	codexAT := "at-" + rawJWT
	before := time.Now()

	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{accessToken: codexAT})

	if seed.accessTokenType != accessTokenTypeCodexAT {
		t.Fatalf("accessTokenType = %q, want %q", seed.accessTokenType, accessTokenTypeCodexAT)
	}
	if seed.email != "" || seed.accountID != "" || seed.planType != "" {
		t.Fatalf("codex_at parsed JWT fields: email=%q accountID=%q planType=%q", seed.email, seed.accountID, seed.planType)
	}
	if seed.expiresAt.Before(before.Add(50*time.Minute)) || seed.expiresAt.After(before.Add(70*time.Minute)) {
		t.Fatalf("expiresAt = %s, want fallback around 1h from now", seed.expiresAt)
	}

	credentials := tokenCredentialMap(seed)
	if got := credentials["access_token_type"]; got != accessTokenTypeCodexAT {
		t.Fatalf("credentials access_token_type = %q, want %q", got, accessTokenTypeCodexAT)
	}
	if _, ok := credentials["email"]; ok {
		t.Fatalf("credentials should not include email for opaque codex_at: %#v", credentials)
	}
	if _, ok := credentials["account_id"]; ok {
		t.Fatalf("credentials should not include account_id for opaque codex_at: %#v", credentials)
	}
}

func TestTokenCredentialMapStoresJWTWorkspaceSeparately(t *testing.T) {
	idToken := makeAdminTestJWT(t, map[string]interface{}{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "workspace-from-jwt",
		},
	})
	credentials := tokenCredentialMap(tokenCredentialSeed{
		idToken:        idToken,
		email:          "USER@example.com",
		accountID:      "legacy-account-id",
		allowDuplicate: true,
	})

	if got := credentials["workspace_id"]; got != "workspace-from-jwt" {
		t.Fatalf("workspace_id = %q, want workspace-from-jwt", got)
	}
	if got := credentials["account_id"]; got != "legacy-account-id" {
		t.Fatalf("account_id = %q, want legacy-account-id", got)
	}
	if _, ok := credentials["allow_duplicate"]; ok {
		t.Fatalf("known workspace retained allow_duplicate: %#v", credentials)
	}
}

func TestTokenCredentialMapDoesNotUseLegacyIDAsWorkspace(t *testing.T) {
	credentials := tokenCredentialMap(tokenCredentialSeed{
		email:          "user@example.com",
		accountID:      "legacy-account-id",
		allowDuplicate: true,
	})
	if _, ok := credentials["workspace_id"]; ok {
		t.Fatalf("legacy account_id became workspace_id: %#v", credentials)
	}
	if got := credentials["allow_duplicate"]; got != "true" {
		t.Fatalf("allow_duplicate = %q, want true", got)
	}
}

func TestTokenCredentialMapRejectsWorkspaceFromDifferentEmail(t *testing.T) {
	idToken := makeAdminTestJWT(t, map[string]interface{}{
		"email": "jwt@example.com",
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "workspace-from-jwt",
		},
	})
	credentials := tokenCredentialMap(tokenCredentialSeed{
		idToken: idToken,
		email:   "import@example.com",
	})
	if _, ok := credentials["workspace_id"]; ok {
		t.Fatalf("mismatched JWT email supplied workspace_id: %#v", credentials)
	}
}
