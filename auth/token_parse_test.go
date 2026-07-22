package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestParseIDTokenUsesProfileEmailFallback(t *testing.T) {
	jwt := makeTestJWT(map[string]interface{}{
		"https://api.openai.com/profile": map[string]interface{}{
			"email": "profile@example.com",
		},
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "workspace-profile-email",
		},
	})

	info := parseIDToken(jwt)
	if info.Email != "profile@example.com" {
		t.Fatalf("Email = %q, want profile@example.com", info.Email)
	}
	if info.ChatGPTAccountID != "workspace-profile-email" {
		t.Fatalf("ChatGPTAccountID = %q, want workspace-profile-email", info.ChatGPTAccountID)
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

func TestRefreshAccessTokenSupplementsWorkspaceFromAccessToken(t *testing.T) {
	idToken := makeTestJWT(map[string]interface{}{
		"email": "id@example.com",
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_plan_type":                 "pro",
			"chatgpt_subscription_active_until": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		},
	})
	accessToken := makeTestJWT(map[string]interface{}{
		"exp":                            time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/profile": map[string]interface{}{"email": "id@example.com"},
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": "workspace-from-at",
			"chatgpt_plan_type":  "pro",
		},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt-new","id_token":%q,"expires_in":3600}`, accessToken, idToken)
	}))
	defer server.Close()

	oldDecorator := ResinRequestDecorator
	ResinRequestDecorator = func(targetURL, accountID string) string { return server.URL }
	defer func() { ResinRequestDecorator = oldDecorator }()

	_, info, err := RefreshAccessToken(context.Background(), "rt-old", "", "account-1")
	if err != nil {
		t.Fatalf("RefreshAccessToken returned error: %v", err)
	}
	if info.ChatGPTAccountID != "workspace-from-at" {
		t.Fatalf("ChatGPTAccountID = %q, want workspace-from-at", info.ChatGPTAccountID)
	}
}

func TestRefreshTokenReusedIsNonRetryable(t *testing.T) {
	reusedErr := errors.New(`刷新失败 (status 401): {"error":{"code":"refresh_token_reused"}}`)
	if !isNonRetryable(reusedErr) {
		t.Fatal("refresh_token_reused should be treated as non-retryable")
	}
}

func TestRefreshWithSessionToken(t *testing.T) {
	accessToken := makeTestJWT(map[string]interface{}{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/profile": map[string]interface{}{
			"email": "session@example.com",
		},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Resin-Account"); got != "account-1" {
			t.Fatalf("X-Resin-Account = %q, want account-1", got)
		}
		cookie, err := r.Cookie("__Secure-next-auth.session-token")
		if err != nil {
			t.Fatalf("missing session cookie: %v", err)
		}
		if cookie.Value != "st-old" {
			t.Fatalf("session cookie = %q, want st-old", cookie.Value)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"` + accessToken + `","expires":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `","user":{"email":"session@example.com"}}`))
	}))
	defer server.Close()

	oldDecorator := ResinRequestDecorator
	ResinRequestDecorator = func(targetURL, accountID string) string {
		return server.URL
	}
	defer func() {
		ResinRequestDecorator = oldDecorator
	}()

	td, info, err := RefreshWithSessionToken(context.Background(), "st-old", "", "account-1")
	if err != nil {
		t.Fatalf("RefreshWithSessionToken returned error: %v", err)
	}
	if td.AccessToken != accessToken {
		t.Fatalf("AccessToken = %q, want %q", td.AccessToken, accessToken)
	}
	if info.Email != "session@example.com" {
		t.Fatalf("Email = %q, want session@example.com", info.Email)
	}
}

// 个人账号 JWT 可能没有 chatgpt_account_id，只有 user_id；解析必须带出它，
// 供 AT 导入保存用户元数据；user_id 不再作为身份去重键。
func TestParseAccessTokenExtractsUserID(t *testing.T) {
	jwt := makeTestJWT(map[string]interface{}{
		"exp": 9999999999,
		"https://api.openai.com/auth": map[string]interface{}{
			"user_id":           "user-QJuZktEjr1Sbbiq19lRnZTow",
			"chatgpt_plan_type": "pro",
		},
		"https://api.openai.com/profile": map[string]interface{}{
			"email": "solo@example.com",
		},
	})

	info := ParseAccessToken(jwt)
	if info == nil {
		t.Fatal("ParseAccessToken returned nil")
	}
	if info.UserID != "user-QJuZktEjr1Sbbiq19lRnZTow" {
		t.Fatalf("UserID = %q, want user-QJuZktEjr1Sbbiq19lRnZTow", info.UserID)
	}
	if info.ChatGPTAccountID != "" {
		t.Fatalf("ChatGPTAccountID = %q, want empty", info.ChatGPTAccountID)
	}
}

func TestParseAccessTokenChatGPTUserIDFallback(t *testing.T) {
	jwt := makeTestJWT(map[string]interface{}{
		"exp": 9999999999,
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_user_id": "user-fallback-1",
		},
	})

	info := ParseAccessToken(jwt)
	if info == nil || info.UserID != "user-fallback-1" {
		t.Fatalf("UserID = %v, want user-fallback-1", info)
	}
}
