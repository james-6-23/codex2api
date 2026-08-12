package auth

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseClaudeAuthorizationInput(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantCode  string
		wantState string
	}{
		{name: "裸 code", raw: "ac_abc123", wantCode: "ac_abc123"},
		{name: "回调页 code#state 复合串", raw: "ac_abc123#st_xyz", wantCode: "ac_abc123", wantState: "st_xyz"},
		{name: "首尾空白", raw: "  ac_abc123#st_xyz \n", wantCode: "ac_abc123", wantState: "st_xyz"},
		{
			name:      "完整回调 URL",
			raw:       "https://platform.claude.com/oauth/code/callback?code=ac_abc123&state=st_xyz",
			wantCode:  "ac_abc123",
			wantState: "st_xyz",
		},
		{
			// 回调 URL 的 code 参数本身也可能带 #state；此时以内嵌值为准。
			name:      "回调 URL 内嵌复合 code",
			raw:       "https://platform.claude.com/oauth/code/callback?code=ac_abc123%23st_inner&state=st_outer",
			wantCode:  "ac_abc123",
			wantState: "st_inner",
		},
		{name: "空串", raw: "   ", wantCode: "", wantState: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseClaudeAuthorizationInput(tt.raw)
			if got.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tt.wantCode)
			}
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
		})
	}
}

func TestBuildClaudeAuthorizationURL(t *testing.T) {
	raw, err := BuildClaudeAuthorizationURL(ClaudeAuthURLParams{
		Challenge: "chal",
		State:     "st",
	})
	if err != nil {
		t.Fatalf("BuildClaudeAuthorizationURL() error = %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析授权 URL 失败: %v", err)
	}
	q := u.Query()
	want := map[string]string{
		"code":                  "true",
		"client_id":             ClaudeDefaultOAuthClientID,
		"response_type":         "code",
		"redirect_uri":          ClaudeDefaultRedirectURI,
		"scope":                 ClaudeDefaultOAuthScope,
		"code_challenge":        "chal",
		"code_challenge_method": "S256",
		"state":                 "st",
		// prompt=login 是多账号池的关键，缺了会让上游作废前一个账号的 RT 家族。
		"prompt": "login",
	}
	for key, expected := range want {
		if got := q.Get(key); got != expected {
			t.Errorf("query %s = %q, want %q", key, got, expected)
		}
	}
}

func TestBuildClaudeAuthorizationURLRejectsEmptyChallenge(t *testing.T) {
	if _, err := BuildClaudeAuthorizationURL(ClaudeAuthURLParams{State: "st"}); err == nil {
		t.Fatal("空 code_challenge 应当报错")
	}
}

func TestGenerateClaudePKCE(t *testing.T) {
	pkce, err := GenerateClaudePKCE()
	if err != nil {
		t.Fatalf("GenerateClaudePKCE() error = %v", err)
	}
	if pkce.Verifier == "" || pkce.Challenge == "" || pkce.State == "" {
		t.Fatalf("PKCE 材料不完整: %+v", pkce)
	}
	if pkce.Verifier == pkce.Challenge {
		t.Error("challenge 应为 verifier 的 S256 摘要，不应相同")
	}
	// RFC 7636 要求 verifier 长度在 43..128 之间。
	if len(pkce.Verifier) < 43 || len(pkce.Verifier) > 128 {
		t.Errorf("verifier 长度 = %d，超出 RFC 7636 的 43..128", len(pkce.Verifier))
	}
	if strings.ContainsAny(pkce.Challenge, "+/=") {
		t.Errorf("challenge 必须是 base64url 无填充形态: %q", pkce.Challenge)
	}
}

func TestNormalizeClaudeOAuthClientID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "正常 UUID", input: " 9d1c250a-e61b-44d9-88ed-5944d1962f5e ", want: "9d1c250a-e61b-44d9-88ed-5944d1962f5e"},
		{name: "空串", input: "   ", want: ""},
		{name: "含内部空白", input: "abc def", want: ""},
		{name: "含控制字符", input: "abc\ndef", want: ""},
		{name: "超长", input: strings.Repeat("a", ClaudeOAuthClientIDMaxLen+1), want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeClaudeOAuthClientID(tt.input); got != tt.want {
				t.Errorf("NormalizeClaudeOAuthClientID(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestClaudePlanFromRateLimitTier(t *testing.T) {
	tests := []struct {
		name string
		tier string
		want string
	}{
		{name: "max 20x", tier: "default_claude_max_20x", want: "max20"},
		{name: "max 5x", tier: "default_claude_max_5x", want: "max5"},
		{name: "pro", tier: "default_claude_pro", want: "pro"},
		{name: "team", tier: "default_claude_team", want: "team"},
		{name: "enterprise", tier: "claude_enterprise", want: "enterprise"},
		{name: "free", tier: "default_claude_free", want: "free"},
		{name: "空", tier: "  ", want: ""},
		{name: "未知档位保持空白", tier: "something_else", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClaudePlanFromRateLimitTier(tt.tier); got != tt.want {
				t.Errorf("ClaudePlanFromRateLimitTier(%q) = %q, want %q", tt.tier, got, tt.want)
			}
		})
	}
}

func TestNormalizeClaudeBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "空 = 用官方默认", input: "  ", want: ""},
		{name: "去尾部斜杠", input: "https://relay.example.com/api/", want: "https://relay.example.com/api"},
		{name: "拒绝非 http 协议", input: "ftp://relay.example.com", wantErr: true},
		{name: "拒绝查询参数", input: "https://relay.example.com?x=1", wantErr: true},
		{name: "拒绝缺主机", input: "https://", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeClaudeBaseURL(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeClaudeBaseURL(%q) 应当报错", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeClaudeBaseURL(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeClaudeBaseURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// token 端点被限制在 Anthropic 官方域内：误配的 token_url 不能把刷新凭据带走。
func TestClaudeResolveTokenURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "空 = 官方默认", input: "", want: ClaudeDefaultTokenURL},
		{name: "console 域", input: "https://console.anthropic.com/v1/oauth/token", want: "https://console.anthropic.com/v1/oauth/token"},
		{name: "拒绝第三方域", input: "https://evil.example.com/token", wantErr: true},
		{name: "拒绝明文 http", input: "http://api.anthropic.com/v1/oauth/token", wantErr: true},
		{name: "拒绝相似域名后缀", input: "https://api.anthropic.com.evil.example/token", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := claudeResolveTokenURL(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("claudeResolveTokenURL(%q) 应当报错", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("claudeResolveTokenURL(%q) error = %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("claudeResolveTokenURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestAccountIsClaudeAPI(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "nil", account: nil, want: false},
		{name: "OAuth 凭据", account: &Account{UpstreamType: UpstreamClaude, AccessToken: "at"}, want: true},
		{name: "仅 RT 也算（AT 待刷新）", account: &Account{UpstreamType: UpstreamClaude, RefreshToken: "rt"}, want: true},
		{name: "大小写不敏感", account: &Account{UpstreamType: "Claude", AccessToken: "at"}, want: true},
		{name: "无凭据", account: &Account{UpstreamType: UpstreamClaude}, want: false},
		{name: "Grok 账号", account: &Account{UpstreamType: UpstreamGrok, AccessToken: "at"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.account.IsClaudeAPI(); got != tt.want {
				t.Errorf("IsClaudeAPI() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Claude 账号必须被归入 relay 风格，否则会被拉去跑 Codex 专属探针（wham/WS/manifest）。
func TestClaudeAccountIsRelayStyle(t *testing.T) {
	acc := &Account{UpstreamType: UpstreamClaude, AccessToken: "at"}
	if !acc.IsRelayStyle() {
		t.Error("Claude 账号应被 IsRelayStyle 判为 true")
	}
}

func TestAccountClaudeChannelSupportsModel(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		model   string
		want    bool
	}{
		{name: "未声明白名单则全放行", account: &Account{UpstreamType: UpstreamClaude}, model: "claude-opus-4-5", want: true},
		{name: "命中白名单", account: &Account{UpstreamType: UpstreamClaude, Models: []string{"claude-opus-4-5"}}, model: "claude-opus-4-5", want: true},
		{name: "白名单大小写不敏感", account: &Account{UpstreamType: UpstreamClaude, Models: []string{"Claude-Opus-4-5"}}, model: "claude-opus-4-5", want: true},
		{name: "未命中白名单", account: &Account{UpstreamType: UpstreamClaude, Models: []string{"claude-haiku-4-5"}}, model: "claude-opus-4-5", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.account.ClaudeChannelSupportsModel(tt.model); got != tt.want {
				t.Errorf("ClaudeChannelSupportsModel(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestClaudeCredentialsFallsBackToOfficialBaseURL(t *testing.T) {
	acc := &Account{UpstreamType: UpstreamClaude, AccessToken: "at"}
	baseURL, bearer := acc.ClaudeCredentials()
	if baseURL != ClaudeDefaultAPIBaseURL {
		t.Errorf("baseURL = %q, want %q", baseURL, ClaudeDefaultAPIBaseURL)
	}
	if bearer != "at" {
		t.Errorf("bearer = %q, want %q", bearer, "at")
	}
}
