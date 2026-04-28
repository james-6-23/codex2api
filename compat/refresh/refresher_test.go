package refresh

import (
	"context"
	"testing"
	"time"
)

func TestParseJWTExp(t *testing.T) {
	// 测试有效的 JWT token
	// eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjE3MzU0NTkyMDB9.xxx
	// payload: {"exp":1735459200} = 2024-12-29 08:00:00 UTC
	token := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJleHAiOjE3MzU0NTkyMDB9.signature"
	exp := parseJWTExp(token)

	expectedTime := time.Unix(1735459200, 0)
	if !exp.Equal(expectedTime) {
		t.Errorf("解析 JWT exp 失败: expected %v, got %v", expectedTime, exp)
	}
}

func TestParseJWTExpInvalid(t *testing.T) {
	// 测试无效的 JWT token
	token := "invalid.token.format"
	exp := parseJWTExp(token)

	// 应该返回默认值（当前时间 + 24 小时）
	now := time.Now()
	if exp.Before(now) || exp.After(now.Add(25*time.Hour)) {
		t.Errorf("无效 JWT 应返回默认过期时间（24小时后）: got %v", exp)
	}
}

func TestFriendlyRefreshErr(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"http=401 invalid_grant", "RT 已失效（401）"},
		{"http=403 forbidden", "上游拒绝访问（403）"},
		{"http=429 rate limit", "触发速率限制（429）"},
		{"timeout exceeded", "刷新请求超时"},
		{"no such host", "DNS 解析失败"},
		{"connection refused", "连接被拒绝"},
		{"connection reset", "连接被重置"},
		{"unexpected EOF", "连接被对端关闭"},
		{"tls handshake failed", "TLS 握手失败"},
		{"missing access_token", "missing access_token"},
		{"ST 已过期或无效", "ST 已过期或无效"},
		{"unknown error", "刷新失败：unknown error"},
	}

	for _, tt := range tests {
		result := friendlyRefreshErr(&testError{msg: tt.input})
		if result != tt.expected {
			t.Errorf("friendlyRefreshErr(%q) = %q, expected %q", tt.input, result, tt.expected)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		limit    int
		expected string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"this is a very long string", 10, "this is a …"},
	}

	for _, tt := range tests {
		result := truncate(tt.input, tt.limit)
		if result != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, expected %q", tt.input, tt.limit, result, tt.expected)
		}
	}
}

func TestNewRefresher(t *testing.T) {
	clientID := "test-client-id"
	refresher := NewRefresher(clientID)

	if refresher == nil {
		t.Fatal("NewRefresher 返回 nil")
	}

	if refresher.clientID != clientID {
		t.Errorf("clientID 不匹配: expected %s, got %s", clientID, refresher.clientID)
	}

	if refresher.client == nil {
		t.Error("HTTP client 未初始化")
	}

	if refresher.client.Timeout != 30*time.Second {
		t.Errorf("HTTP client timeout 不正确: expected 30s, got %v", refresher.client.Timeout)
	}
}

func TestRefreshAutoEmptyTokens(t *testing.T) {
	refresher := NewRefresher("test-client-id")
	ctx := context.Background()

	result, err := refresher.RefreshAuto(ctx, 123, "test@example.com", "", "")

	if err != nil {
		t.Errorf("RefreshAuto 不应返回错误: %v", err)
	}

	if result.OK {
		t.Error("空 token 不应刷新成功")
	}

	if result.Source != "failed" {
		t.Errorf("Source 应为 'failed': got %s", result.Source)
	}

	if result.Error == "" {
		t.Error("Error 不应为空")
	}
}

// testError 用于测试的错误类型
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}
