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
		t.Fatalf("json.Marshal: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestWorkspaceID(t *testing.T) {
	claims, err := ParseJWT(testJWT(t, map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": " workspace-1 ",
		},
	}))
	if err != nil {
		t.Fatalf("ParseJWT: %v", err)
	}
	if got := claims.WorkspaceID(); got != "workspace-1" {
		t.Fatalf("WorkspaceID = %q, want workspace-1", got)
	}
}

func TestWorkspaceIDRejectsUserID(t *testing.T) {
	claims, err := ParseJWT(testJWT(t, map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "user-not-a-workspace",
		},
	}))
	if err != nil {
		t.Fatalf("ParseJWT: %v", err)
	}
	if got := claims.WorkspaceID(); got != "" {
		t.Fatalf("WorkspaceID = %q, want empty for user id", got)
	}
}
