package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

func TestGenerateClaudePKCE(t *testing.T) {
	pkce, err := GenerateClaudePKCE()
	if err != nil {
		t.Fatalf("GenerateClaudePKCE 出错: %v", err)
	}
	if pkce.CodeVerifier == "" || pkce.CodeChallenge == "" {
		t.Fatal("verifier/challenge 不应为空")
	}
	// verifier 应满足 RFC 7636 长度 43-128。
	if l := len(pkce.CodeVerifier); l < 43 || l > 128 {
		t.Fatalf("verifier 长度 %d 不在 [43,128]", l)
	}
	// challenge 必须是 verifier 的 S256（RawURL 无填充）。
	sum := sha256.Sum256([]byte(pkce.CodeVerifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if pkce.CodeChallenge != want {
		t.Fatalf("challenge 不是 verifier 的 S256:\n got=%s\nwant=%s", pkce.CodeChallenge, want)
	}
	// 不应含有 base64 填充或非 URL 安全字符。
	if strings.ContainsAny(pkce.CodeVerifier+pkce.CodeChallenge, "=+/") {
		t.Fatal("PKCE 值含有非 URL 安全字符或填充")
	}
}

func TestGenerateClaudePKCEUnique(t *testing.T) {
	a, _ := GenerateClaudePKCE()
	b, _ := GenerateClaudePKCE()
	if a.CodeVerifier == b.CodeVerifier {
		t.Fatal("两次生成的 verifier 不应相同")
	}
}

func TestBuildClaudeAuthURL(t *testing.T) {
	pkce := &ClaudePKCECodes{CodeVerifier: "v", CodeChallenge: "challenge-xyz"}
	raw, err := BuildClaudeAuthURL("state-123", pkce)
	if err != nil {
		t.Fatalf("BuildClaudeAuthURL 出错: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("生成的 URL 无法解析: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != ClaudeOAuthAuthURL {
		t.Fatalf("授权端点错误: %s", got)
	}
	q := u.Query()
	checks := map[string]string{
		"client_id":             ClaudeOAuthClientID,
		"response_type":         "code",
		"redirect_uri":          ClaudeOAuthRedirectURI,
		"scope":                 ClaudeOAuthScope,
		"code_challenge":        "challenge-xyz",
		"code_challenge_method": "S256",
		"state":                 "state-123",
		"code":                  "true",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("查询参数 %s = %q, 期望 %q", k, got, want)
		}
	}
}

func TestBuildClaudeAuthURLNilPKCE(t *testing.T) {
	if _, err := BuildClaudeAuthURL("s", nil); err == nil {
		t.Fatal("PKCE 为 nil 时应报错")
	}
}

func TestParseClaudeCodeAndState(t *testing.T) {
	cases := []struct {
		in        string
		wantCode  string
		wantState string
	}{
		{"abc", "abc", ""},
		{"abc#xyz", "abc", "xyz"},
		{"  abc#xyz  ", "abc", "xyz"},
		{"abc#xyz#extra", "abc", "xyz"},
	}
	for _, c := range cases {
		code, state := parseClaudeCodeAndState(c.in)
		if code != c.wantCode || state != c.wantState {
			t.Errorf("parseClaudeCodeAndState(%q) = (%q,%q), 期望 (%q,%q)",
				c.in, code, state, c.wantCode, c.wantState)
		}
	}
}

func TestStartClaudeLogin(t *testing.T) {
	s, err := StartClaudeLogin()
	if err != nil {
		t.Fatalf("StartClaudeLogin 出错: %v", err)
	}
	if s.State == "" || s.Verifier == "" || s.AuthURL == "" {
		t.Fatal("登录会话字段不应为空")
	}
	if !strings.Contains(s.AuthURL, url.QueryEscape(s.State)) {
		t.Fatal("AuthURL 应包含 state")
	}
	// AuthURL 里的 challenge 必须与返回的 verifier 对得上。
	u, _ := url.Parse(s.AuthURL)
	sum := sha256.Sum256([]byte(s.Verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if u.Query().Get("code_challenge") != wantChallenge {
		t.Fatal("AuthURL 中的 code_challenge 与 verifier 不匹配")
	}
}
