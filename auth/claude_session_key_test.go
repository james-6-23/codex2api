package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeClaudeSessionKey(t *testing.T) {
	cases := map[string]string{
		"sk-ant-sid01-abc":                      "sk-ant-sid01-abc",
		"  sessionKey=sk-ant-sid01-abc; Path=/": "sk-ant-sid01-abc",
		"\"sk-ant-sid01-abc\"":                  "sk-ant-sid01-abc",
		"Cookie: sessionKey=sk-ant-sid01-abc":   "sk-ant-sid01-abc",
	}
	for in, want := range cases {
		if got := NormalizeClaudeSessionKey(in); got != want {
			t.Errorf("NormalizeClaudeSessionKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickClaudeWebOrganization(t *testing.T) {
	orgs := []claudeWebOrganization{
		{UUID: "api-only", Capabilities: []string{"api"}},
		{UUID: "chat-small", Capabilities: []string{"chat"}},
		{UUID: "chat-big", Capabilities: []string{"chat", "claude_pro", "voice"}},
	}
	got, err := pickClaudeWebOrganization(orgs)
	if err != nil || got.UUID != "chat-big" {
		t.Fatalf("pick = %+v err=%v, want chat-big", got, err)
	}
	if _, err := pickClaudeWebOrganization([]claudeWebOrganization{{UUID: "x", Capabilities: []string{"api"}}}); err == nil {
		t.Fatal("organization without chat capability must be rejected")
	}
}

// TestClaudeSessionKeyAuthorizeSteps 用 httptest 模拟 claude.ai 网页端,验证前两步
// (组织列表 → cookie authorize)的请求形态与 code/state 解析。第三步 token 交换
// 走固定的 platform.claude.com 常量,不在此覆盖。
func TestClaudeSessionKeyAuthorizeSteps(t *testing.T) {
	var sawCookie, sawAuthorizeBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCookie = r.Header.Get("Cookie")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/organizations":
			_, _ = w.Write([]byte(`[{"uuid":"org-1","name":"Personal","capabilities":["chat","claude_max"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth/org-1/authorize":
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			raw, _ := json.Marshal(payload)
			sawAuthorizeBody = string(raw)
			_, _ = w.Write([]byte(`{"redirect_uri":"https://platform.claude.com/oauth/code/callback?code=code-xyz&state=` + payload["state"].(string) + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	prev := claudeWebBaseURL
	claudeWebBaseURL = server.URL
	defer func() { claudeWebBaseURL = prev }()

	client := NewClaudeAuth("")
	org, err := client.fetchClaudeWebOrganization(context.Background(), "sk-ant-sid01-test")
	if err != nil {
		t.Fatalf("fetch organization: %v", err)
	}
	if org.UUID != "org-1" || sawCookie != "sessionKey=sk-ant-sid01-test" {
		t.Fatalf("org=%+v cookie=%q", org, sawCookie)
	}
	code, verifier, state, err := client.authorizeClaudeWithSessionKey(context.Background(), "sk-ant-sid01-test", org.UUID, ClaudeOAuthScopeInference)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if code != "code-xyz" || verifier == "" || state == "" {
		t.Fatalf("authorize result = (%q,%q,%q)", code, verifier, state)
	}
	for _, needle := range []string{`"client_id":"` + ClaudeOAuthClientID + `"`, `"organization_uuid":"org-1"`, `"redirect_uri":"` + ClaudeOAuthRedirectURI + `"`, `"scope":"user:inference"`, `"code_challenge_method":"S256"`} {
		if !strings.Contains(sawAuthorizeBody, needle) {
			t.Fatalf("authorize body %s lacks %s", sawAuthorizeBody, needle)
		}
	}
}

func TestClaudeSessionKeyRejectsInvalidCookie(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	prev := claudeWebBaseURL
	claudeWebBaseURL = server.URL
	defer func() { claudeWebBaseURL = prev }()
	_, err := NewClaudeAuth("").ExchangeSessionKey(context.Background(), "sessionKey=bad", false)
	if err == nil || !strings.Contains(err.Error(), "sessionKey 无效") {
		t.Fatalf("expected invalid sessionKey error, got %v", err)
	}
	if _, err := NewClaudeAuth("").ExchangeSessionKey(context.Background(), "   ", false); err == nil {
		t.Fatal("empty sessionKey must be rejected before any network call")
	}
}
