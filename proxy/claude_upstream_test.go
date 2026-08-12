package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestClaudeMessagesEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "空 = 官方域", baseURL: "", want: "https://api.anthropic.com/v1/messages"},
		{name: "裸域名", baseURL: "https://relay.example.com", want: "https://relay.example.com/v1/messages"},
		{name: "尾部斜杠", baseURL: "https://relay.example.com/", want: "https://relay.example.com/v1/messages"},
		{name: "已含 /v1 不重复拼接", baseURL: "https://relay.example.com/v1", want: "https://relay.example.com/v1/messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeMessagesEndpoint(tt.baseURL); got != tt.want {
				t.Errorf("claudeMessagesEndpoint(%q) = %q, want %q", tt.baseURL, got, tt.want)
			}
		})
	}
}

// 下游自带的 beta 标记要保留，OAuth 准入标记不能被覆盖掉——覆盖即 401。
func TestClaudeBetaHeaderValue(t *testing.T) {
	tests := []struct {
		name       string
		downstream []string
		want       string
	}{
		{name: "无下游声明", downstream: nil, want: auth.ClaudeOAuthBetaHeader},
		{name: "合并下游 beta", downstream: []string{"fine-grained-tool-streaming-2025-05-14"}, want: auth.ClaudeOAuthBetaHeader + ",fine-grained-tool-streaming-2025-05-14"},
		{name: "逗号分隔多值", downstream: []string{"a-beta, b-beta"}, want: auth.ClaudeOAuthBetaHeader + ",a-beta,b-beta"},
		{name: "去重 oauth 标记", downstream: []string{auth.ClaudeOAuthBetaHeader + ",a-beta"}, want: auth.ClaudeOAuthBetaHeader + ",a-beta"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := http.Header{}
			for _, v := range tt.downstream {
				header.Add("anthropic-beta", v)
			}
			if got := claudeBetaHeaderValue(header); got != tt.want {
				t.Errorf("claudeBetaHeaderValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

// 上游按 system 首块是否等于身份串放行 OAuth 请求，不一致直接 403。
func TestEnsureClaudeIdentitySystemPrompt(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantLen    int
		wantSecond string
	}{
		{name: "缺省 system", body: `{"model":"claude-opus-4-5"}`, wantLen: 1},
		{name: "字符串 system", body: `{"system":"be terse"}`, wantLen: 2, wantSecond: "be terse"},
		{name: "字符串已自带身份串则不重复", body: `{"system":"` + ClaudeCodeIdentityPrompt + `"}`, wantLen: 1},
		{name: "数组 system 前置身份块", body: `{"system":[{"type":"text","text":"be terse"}]}`, wantLen: 2, wantSecond: "be terse"},
		{
			name:    "数组首块已是身份串则原样返回",
			body:    `{"system":[{"type":"text","text":"` + ClaudeCodeIdentityPrompt + `"},{"type":"text","text":"be terse"}]}`,
			wantLen: 2, wantSecond: "be terse",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := ensureClaudeIdentitySystemPrompt([]byte(tt.body))
			system := gjson.GetBytes(out, "system")
			if !system.IsArray() {
				t.Fatalf("system 应为内容块数组，实际: %s", system.Raw)
			}
			blocks := system.Array()
			if len(blocks) != tt.wantLen {
				t.Fatalf("system 块数 = %d, want %d (%s)", len(blocks), tt.wantLen, system.Raw)
			}
			if got := blocks[0].Get("text").String(); got != ClaudeCodeIdentityPrompt {
				t.Errorf("首块 text = %q, want %q", got, ClaudeCodeIdentityPrompt)
			}
			if tt.wantSecond != "" {
				if got := blocks[1].Get("text").String(); got != tt.wantSecond {
					t.Errorf("第二块 text = %q, want %q", got, tt.wantSecond)
				}
			}
		})
	}
}

// 身份块带 cache_control，让上游把固定前缀计入提示缓存而不是每轮重新计费。
func TestClaudeIdentityBlockCarriesCacheControl(t *testing.T) {
	out := ensureClaudeIdentitySystemPrompt([]byte(`{"model":"claude-opus-4-5"}`))
	if got := gjson.GetBytes(out, "system.0.cache_control.type").String(); got != "ephemeral" {
		t.Errorf("cache_control.type = %q, want %q", got, "ephemeral")
	}
}

func TestPrepareClaudeUpstreamBodyKeepsExplicitMetadata(t *testing.T) {
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at", ClaudeCLIUserID: "cli", ClaudeAccountUUID: "acct"}
	out := prepareClaudeUpstreamBody([]byte(`{"metadata":{"user_id":"caller-supplied"}}`), acc, "sess")
	if got := gjson.GetBytes(out, "metadata.user_id").String(); got != "caller-supplied" {
		t.Errorf("metadata.user_id = %q, 调用方已显式指定时不应覆盖", got)
	}
}

func TestPrepareClaudeUpstreamBodyInjectsMetadataUserID(t *testing.T) {
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at", ClaudeCLIUserID: "cli", ClaudeAccountUUID: "acct"}
	out := prepareClaudeUpstreamBody([]byte(`{"model":"claude-opus-4-5"}`), acc, "sess")
	want := "user_cli_account_acct_session_sess"
	if got := gjson.GetBytes(out, "metadata.user_id").String(); got != want {
		t.Errorf("metadata.user_id = %q, want %q", got, want)
	}
}

// 没有 cli_user_id（历史账号）时不该塞一个残缺的 user_id 上去。
func TestPrepareClaudeUpstreamBodySkipsMetadataWithoutCLIUserID(t *testing.T) {
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at"}
	out := prepareClaudeUpstreamBody([]byte(`{"model":"claude-opus-4-5"}`), acc, "sess")
	if gjson.GetBytes(out, "metadata.user_id").Exists() {
		t.Error("缺 cli_user_id 时不应写入 metadata.user_id")
	}
}

func TestIsClaudeOverloadedError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{name: "529 状态码", statusCode: 529, body: `{}`, want: true},
		{name: "错误体标记 overloaded", statusCode: 500, body: `{"error":{"type":"overloaded_error"}}`, want: true},
		{name: "普通 429", statusCode: 429, body: `{"error":{"type":"rate_limit_error"}}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsClaudeOverloadedError(tt.statusCode, []byte(tt.body)); got != tt.want {
				t.Errorf("IsClaudeOverloadedError() = %v, want %v", got, tt.want)
			}
		})
	}
}

// 上游没给重置时刻时按 5 小时滚动窗口兜底：分钟级重试只会把同一个号反复撞在同一堵墙上。
func TestApplyClaudeCooldownFallsBackToRollingWindow(t *testing.T) {
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at"}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	decision := applyClaudeCooldown(nil, acc, http.StatusTooManyRequests, []byte(`{}`), resp, "claude-opus-4-5")
	if decision.Reason != "rate_limited_5h" {
		t.Errorf("Reason = %q, want %q", decision.Reason, "rate_limited_5h")
	}
	if decision.Cooldown < 4*time.Hour {
		t.Errorf("Cooldown = %v, 应接近 5 小时", decision.Cooldown)
	}
}

func TestApplyClaudeCooldownHonorsRetryAfter(t *testing.T) {
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at"}
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	resp.Header.Set("Retry-After", "120")
	decision := applyClaudeCooldown(nil, acc, http.StatusTooManyRequests, []byte(`{}`), resp, "claude-opus-4-5")
	if decision.Cooldown > 130*time.Second || decision.Cooldown < 110*time.Second {
		t.Errorf("Cooldown = %v, 应贴近 Retry-After 的 120s", decision.Cooldown)
	}
}

func TestApplyClaudeCooldownOverloadedIsShort(t *testing.T) {
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at"}
	resp := &http.Response{StatusCode: 529, Header: http.Header{}}
	decision := applyClaudeCooldown(nil, acc, 529, []byte(`{}`), resp, "claude-opus-4-5")
	if decision.Reason != "upstream_overloaded" {
		t.Errorf("Reason = %q, want %q", decision.Reason, "upstream_overloaded")
	}
	if decision.Cooldown > time.Minute {
		t.Errorf("Cooldown = %v, 上游容量问题不应长冷却", decision.Cooldown)
	}
}

func TestClaudeStreamUsage(t *testing.T) {
	usage := &claudeStreamUsage{}
	usage.observe(`{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":10,"output_tokens":1}}}`)
	usage.observe(`{"type":"content_block_delta","delta":{"text":"hi"}}`)
	usage.observe(`{"type":"message_delta","usage":{"output_tokens":250}}`)

	got := usage.toUsageInfo()
	if got == nil {
		t.Fatal("toUsageInfo() = nil, 已观测到 usage 事件")
	}
	if got.InputTokens != 150 {
		t.Errorf("InputTokens = %d, want 150 (input + cache_read + cache_creation)", got.InputTokens)
	}
	// output_tokens 在 message_delta 里是累计快照，取最后一次而不是相加。
	if got.OutputTokens != 250 {
		t.Errorf("OutputTokens = %d, want 250", got.OutputTokens)
	}
	if got.CachedTokens != 40 {
		t.Errorf("CachedTokens = %d, want 40", got.CachedTokens)
	}
	if got.TotalTokens != 400 {
		t.Errorf("TotalTokens = %d, want 400", got.TotalTokens)
	}
}

func TestClaudeStreamUsageNilWithoutObservation(t *testing.T) {
	usage := &claudeStreamUsage{}
	usage.observe(`{"type":"content_block_delta","delta":{"text":"hi"}}`)
	if usage.toUsageInfo() != nil {
		t.Error("未观测到 usage 事件时应返回 nil，而不是一条全零的记账")
	}
}

func TestClaudeUsageFromJSONBody(t *testing.T) {
	got := claudeUsageFromJSONBody([]byte(`{"usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":10,"output_tokens":250}}`))
	if got == nil {
		t.Fatal("claudeUsageFromJSONBody() = nil")
	}
	if got.InputTokens != 150 || got.OutputTokens != 250 || got.CachedTokens != 40 {
		t.Errorf("usage = %+v, want input=150 output=250 cached=40", got)
	}
	if claudeUsageFromJSONBody([]byte(`{}`)) != nil {
		t.Error("响应体没有 usage 时应返回 nil")
	}
}

// Claude 账号只吃原生 Anthropic Messages 体；relay 收口处必须把它排除，
// 否则 Responses 形态的请求体会被路由过去并必然 400。
func TestRelayAccountSupportsModelExcludesClaude(t *testing.T) {
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at", Models: []string{"claude-opus-4-5"}}
	if relayAccountSupportsModel(acc, "claude-opus-4-5") {
		t.Error("Claude 账号不应被 relay 路径选中")
	}
}

func TestClaudeChannelAccountFilter(t *testing.T) {
	tests := []struct {
		name    string
		account *auth.Account
		model   string
		want    bool
	}{
		{name: "非 Claude 账号", account: &auth.Account{UpstreamType: auth.UpstreamGrok, AccessToken: "at"}, model: "claude-opus-4-5", want: false},
		{name: "无白名单 + 默认模型集命中", account: &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at"}, model: "claude-opus-4-5", want: true},
		{name: "无白名单 + 非 Claude 模型", account: &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at"}, model: "gpt-5", want: false},
		{name: "有白名单则以白名单为准", account: &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "at", Models: []string{"claude-custom"}}, model: "claude-custom", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeChannelAccountFilter(tt.model)(tt.account); got != tt.want {
				t.Errorf("claudeChannelAccountFilter(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}
