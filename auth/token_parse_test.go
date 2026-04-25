package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// makeTestJWT 构造一个不签名的测试 JWT（header.payload.signature）
func makeTestJWT(claims interface{}) string {
	payload, _ := json.Marshal(claims)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return "eyJhbGciOiJSUzI1NiJ9." + encoded + ".fake_signature"
}

func TestParseIDTokenExtractsPlanType(t *testing.T) {
	jwt := makeTestJWT(map[string]interface{}{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "acc_123",
			"chatgpt_plan_type":  "plus",
		},
	})

	info := parseIDToken(jwt)
	if info.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want %q", info.PlanType, "plus")
	}
	if info.Email != "user@example.com" {
		t.Fatalf("Email = %q, want %q", info.Email, "user@example.com")
	}
}

func TestParseIDTokenMissingAuthClaim(t *testing.T) {
	jwt := makeTestJWT(map[string]interface{}{
		"email": "user@example.com",
	})

	info := parseIDToken(jwt)
	if info.PlanType != "" {
		t.Fatalf("PlanType = %q, want empty", info.PlanType)
	}
}

func TestParseAccessTokenExtractsPlanType(t *testing.T) {
	jwt := makeTestJWT(map[string]interface{}{
		"exp": 9999999999,
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "acc_456",
			"chatgpt_plan_type":  "pro",
		},
		"https://api.openai.com/profile": map[string]interface{}{
			"email": "pro@example.com",
		},
	})

	info := ParseAccessToken(jwt)
	if info == nil {
		t.Fatal("ParseAccessToken returned nil")
	}
	if info.PlanType != "pro" {
		t.Fatalf("PlanType = %q, want %q", info.PlanType, "pro")
	}
}

func TestParseIDTokenEmptyReturnsEmptyInfo(t *testing.T) {
	info := parseIDToken("")
	if info == nil {
		t.Fatal("parseIDToken(\"\") should return non-nil AccountInfo")
	}
	if info.PlanType != "" {
		t.Fatalf("PlanType = %q, want empty", info.PlanType)
	}
}

func TestRefreshAccessTokenRejectsEmptyAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refresh_token":"rt-new","expires_in":3600}`))
	}))
	defer server.Close()

	oldDecorator := ResinRequestDecorator
	ResinRequestDecorator = func(targetURL, accountID string) string {
		return server.URL
	}
	defer func() {
		ResinRequestDecorator = oldDecorator
	}()

	_, _, err := RefreshAccessToken(context.Background(), "rt-old", "", "account-1")
	if err == nil {
		t.Fatal("expected empty access_token error, got nil")
	}
	if !strings.Contains(err.Error(), "access_token") {
		t.Fatalf("error = %q, want access_token detail", err.Error())
	}
}
