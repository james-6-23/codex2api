package openaiidentity

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func testJWT(t *testing.T, claims map[string]interface{}) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestTokenIdentity(t *testing.T) {
	idToken := testJWT(t, map[string]interface{}{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": " workspace-1 ",
		},
	})
	if email, workspaceID := TokenIdentity(idToken, ""); email != "user@example.com" || workspaceID != "workspace-1" {
		t.Fatalf("identity = (%q, %q)", email, workspaceID)
	}
}

func TestTokenIdentityUsesAccessTokenProfile(t *testing.T) {
	accessToken := testJWT(t, map[string]interface{}{
		"https://api.openai.com/profile": map[string]interface{}{"email": "user@example.com"},
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "workspace-1",
		},
	})
	if email, workspaceID := TokenIdentity("", accessToken); email != "user@example.com" || workspaceID != "workspace-1" {
		t.Fatalf("identity = (%q, %q)", email, workspaceID)
	}
}

func TestTokenIdentityRequiresOneCompleteJWT(t *testing.T) {
	idToken := testJWT(t, map[string]interface{}{"email": "user@example.com"})
	accessToken := testJWT(t, map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "workspace-1",
		},
	})
	if email, workspaceID := TokenIdentity(idToken, accessToken); email != "" || workspaceID != "" {
		t.Fatalf("identity = (%q, %q), want empty", email, workspaceID)
	}
}

func TestTokenIdentityRejectsUserID(t *testing.T) {
	idToken := testJWT(t, map[string]interface{}{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "user-123",
		},
	})
	if email, workspaceID := TokenIdentity(idToken, ""); email != "" || workspaceID != "" {
		t.Fatalf("identity = (%q, %q), want empty", email, workspaceID)
	}
}
