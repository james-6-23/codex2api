package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func TestInjectClaudeCodeSystemPrompt_Absent(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[]}`)
	out := injectClaudeCodeSystemPrompt(body)
	if !json.Valid(out) {
		t.Fatalf("注入后必须是严格合法 JSON(Anthropic 按严格 JSON 解析,尾逗号即 400): %s", out)
	}
	sys := gjson.GetBytes(out, "system")
	arr := sys.Array()
	if !sys.IsArray() || len(arr) != 2 {
		t.Fatalf("应为 [计费块, 声明块], got=%s", sys.Raw)
	}
	if !strings.HasPrefix(arr[0].Get("text").String(), claudeBillingHeaderPrefix) {
		t.Errorf("首块应为计费块, got=%s", arr[0].Raw)
	}
	if arr[1].Get("text").String() != claudeCodeSystemPreamble {
		t.Errorf("次块应为 Claude Code 声明, got=%s", arr[1].Raw)
	}
}

func TestInjectClaudeCodeSystemPrompt_EmptySystemArray(t *testing.T) {
	out := injectClaudeCodeSystemPrompt([]byte(`{"model":"claude-x","system":[],"messages":[]}`))
	if !json.Valid(out) {
		t.Fatalf("空 system 数组注入后必须是严格合法 JSON: %s", out)
	}
	arr := gjson.GetBytes(out, "system").Array()
	if len(arr) != 2 || !strings.HasPrefix(arr[0].Get("text").String(), claudeBillingHeaderPrefix) ||
		arr[1].Get("text").String() != claudeCodeSystemPreamble {
		t.Fatalf("应为 [计费块, 声明块], got=%s", gjson.GetBytes(out, "system").Raw)
	}
}

func TestInjectClaudeCodeSystemPrompt_String(t *testing.T) {
	body := []byte(`{"system":"be helpful","messages":[]}`)
	out := injectClaudeCodeSystemPrompt(body)
	arr := gjson.GetBytes(out, "system").Array()
	if len(arr) != 3 {
		t.Fatalf("应为 [计费块, 声明块, 原文本块], got len=%d raw=%s", len(arr), gjson.GetBytes(out, "system").Raw)
	}
	if !strings.HasPrefix(arr[0].Get("text").String(), claudeBillingHeaderPrefix) {
		t.Errorf("首块应为计费块, got=%s", arr[0].Raw)
	}
	if arr[1].Get("text").String() != claudeCodeSystemPreamble {
		t.Errorf("次块应为声明, got=%s", arr[1].Raw)
	}
	if arr[2].Get("text").String() != "be helpful" {
		t.Errorf("末块应保留原文本, got=%s", arr[2].Raw)
	}
}

func TestInjectClaudeCodeSystemPrompt_Array(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"custom"}],"messages":[]}`)
	out := injectClaudeCodeSystemPrompt(body)
	arr := gjson.GetBytes(out, "system").Array()
	if len(arr) != 3 || !strings.HasPrefix(arr[0].Get("text").String(), claudeBillingHeaderPrefix) ||
		arr[1].Get("text").String() != claudeCodeSystemPreamble || arr[2].Get("text").String() != "custom" {
		t.Fatalf("应在数组首位插入 [计费块, 声明块], got=%s", gjson.GetBytes(out, "system").Raw)
	}
}

func TestInjectClaudeCodeSystemPrompt_AlreadyPresent(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"x"}],"messages":[]}`)
	out := injectClaudeCodeSystemPrompt(body)
	arr := gjson.GetBytes(out, "system").Array()
	// 声明块已存在不重复注入,但计费块缺失需补到首位。
	if len(arr) != 3 {
		t.Fatalf("应只补计费块, got len=%d raw=%s", len(arr), gjson.GetBytes(out, "system").Raw)
	}
	if !strings.HasPrefix(arr[0].Get("text").String(), claudeBillingHeaderPrefix) {
		t.Fatalf("首块应为计费块, got=%s", arr[0].Raw)
	}
	if arr[1].Get("text").String() != claudeCodeSystemPreamble || arr[2].Get("text").String() != "x" {
		t.Fatalf("原有块应保持顺序, got=%s", gjson.GetBytes(out, "system").Raw)
	}
}

func TestInjectClaudeCodeSystemPrompt_PreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	out := injectClaudeCodeSystemPrompt(body)
	if gjson.GetBytes(out, "model").String() != "claude-x" || gjson.GetBytes(out, "max_tokens").Int() != 100 {
		t.Fatal("注入不应破坏其它字段")
	}
	if gjson.GetBytes(out, "messages.0.content").String() != "hi" {
		t.Fatal("messages 应保留")
	}
}

func TestMergeAnthropicBeta(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-beta", "foo-1, bar-2")
	cfg := auth.DefaultClaudeSecurityConfig()
	cfg.AllowedBetaHeaders = []string{"foo-1", "bar-2"}
	got := mergeAnthropicBetaWithConfig(h, cfg)
	// 必须包含 oauth beta 且入站的两个 beta 都在
	for _, want := range []string{"oauth-2025-04-20", "foo-1", "bar-2"} {
		if !strings.Contains(got, want) {
			t.Errorf("合并结果缺少 %s: %s", want, got)
		}
	}
}

func TestMergeAnthropicBeta_Dedup(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-beta", "oauth-2025-04-20")
	got := mergeAnthropicBetaWithConfig(h, auth.DefaultClaudeSecurityConfig())
	if strings.Count(got, "oauth-2025-04-20") != 1 {
		t.Fatalf("oauth beta 应去重, got=%s", got)
	}
}

func TestMergeAnthropicBeta_Empty(t *testing.T) {
	got := mergeAnthropicBeta(nil)
	if got != auth.ClaudeCodeBeta+","+auth.ClaudeOAuthBeta {
		t.Fatalf("空入站时应带 claude-code + oauth 核心标记, got=%s", got)
	}
}

func TestBuildClaudeBetaHeader_HaikuOmitsClaudeCode(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5","messages":[]}`)
	got := buildClaudeBetaHeader(nil, auth.DefaultClaudeSecurityConfig(), body)
	if strings.Contains(got, auth.ClaudeCodeBeta) {
		t.Fatalf("haiku 模型不应带 claude-code beta, got=%s", got)
	}
	if !strings.Contains(got, auth.ClaudeOAuthBeta) {
		t.Fatalf("oauth beta 必须始终存在, got=%s", got)
	}
}

func TestBuildClaudeBetaHeader_BodyDriven(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","thinking":{"type":"enabled","budget_tokens":1000},"tools":[{"name":"a"}],"context_management":{"edits":[]},"output_config":{"effort":"high"},"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`)
	got := buildClaudeBetaHeader(nil, auth.DefaultClaudeSecurityConfig(), body)
	for _, want := range []string{
		auth.ClaudeCodeBeta, auth.ClaudeOAuthBeta,
		"interleaved-thinking-2025-05-14", "redact-thinking-2026-02-12", "thinking-token-count-2026-05-13",
		"context-management-2025-06-27", "advanced-tool-use-2025-11-20",
		"effort-2025-11-24", "extended-cache-ttl-2025-04-11",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺 body 驱动 beta %s: %s", want, got)
		}
	}
}

func TestBuildClaudeBetaHeader_FiltersUnknownIncoming(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-beta", "some-random-beta-2099, context-management-2025-06-27")
	body := []byte(`{"model":"claude-opus-5","messages":[],"context_management":{"edits":[]}}`)
	got := buildClaudeBetaHeader(h, auth.DefaultClaudeSecurityConfig(), body)
	if strings.Contains(got, "some-random-beta-2099") {
		t.Fatalf("白名单外 beta 应被过滤: %s", got)
	}
	if strings.Count(got, "context-management-2025-06-27") != 1 {
		t.Fatalf("body 驱动的 beta 应去重: %s", got)
	}
}

func TestSanitizeClaudeRequestText_StripsBidiControls(t *testing.T) {
	// 双向覆盖符(U+202E)/弹出符(U+202C)是视觉欺骗的经典载体,在提示词里没有
	// 正当用途,必须剔除(runtime 构造,源码不含控制符)。
	content := "he" + string(rune(0x202E)) + "llo" + string(rune(0x202C)) + " world"
	body := []byte(`{"messages":[{"role":"user","content":"` + content + `"}]}`)
	out := sanitizeClaudeRequestText(body)
	got := gjson.GetBytes(out, "messages.0.content").String()
	if got != "hello world" {
		t.Fatalf("双向控制符未被清理: %q", got)
	}
	if !gjson.ValidBytes(out) {
		t.Fatal("净化后应仍是合法 JSON")
	}
}

// TestSanitizeClaudeRequestText_KeepsZeroWidthContent 钉住"净化不许改写正文"这条
// 边界:零宽字符是内容不是控制信号,ZWJ 连着 emoji 序列、ZWNJ 连着波斯语词形,
// 剔除它们等于静默把 👩‍💻 拆成两个字符、把 می‌روم 改成另一个词。
func TestSanitizeClaudeRequestText_KeepsZeroWidthContent(t *testing.T) {
	zwj := string(rune(0x200D))
	zwnj := string(rune(0x200C))
	zwsp := string(rune(0x200B))
	content := "\U0001F469" + zwj + "\U0001F4BB a" + zwsp + "b mi" + zwnj + "ravam"
	body := []byte(`{"messages":[{"role":"user","content":"` + content + `"}]}`)
	out := sanitizeClaudeRequestText(body)
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != content {
		t.Fatalf("零宽正文被改写:\n want %q\n got  %q", content, got)
	}
}

// TestSanitizeClaudeRequestText_KeepsDecomposedForm 钉住不做 NFC 归一:Claude Code
// 会原样送 macOS 的 NFD 文件路径,归一成预组合形态后模型抄回来的路径就打不开了。
func TestSanitizeClaudeRequestText_KeepsDecomposedForm(t *testing.T) {
	decomposed := "cafe" + string(rune(0x0301)) + ".txt" // NFD 的 café.txt
	body := []byte(`{"messages":[{"role":"user","content":"` + decomposed + `"}]}`)
	out := sanitizeClaudeRequestText(body)
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != decomposed {
		t.Fatalf("NFD 文本被归一改写:\n want %q\n got  %q", decomposed, got)
	}
}

func TestSanitizeClaudeRequestText_KeepsNormal(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[{"role":"user","content":"正常中文与English混排"}]}`)
	out := sanitizeClaudeRequestText(body)
	if gjson.GetBytes(out, "messages.0.content").String() != "正常中文与English混排" {
		t.Fatal("正常文字不应被改动")
	}
}

func TestApplyClaudeMessagesHeaders_PreservesIncoming(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	incoming := http.Header{}
	incoming.Set("user-agent", "claude-cli/9.9.9 (external, cli)")
	incoming.Set("x-stainless-os", "MacOS")
	fp := map[string]string{"User-Agent": "claude-cli/1.0.0 (external, cli)", "X-Stainless-OS": "Linux"}
	applyClaudeMessagesHeaders(req, "tok", incoming, false, nil, fp, "")
	// 入站真实客户端头应优先保留,不被指纹覆盖。
	if req.Header.Get("User-Agent") != "claude-cli/9.9.9 (external, cli)" {
		t.Fatalf("应保留入站 UA, got %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-Stainless-Os") != "MacOS" {
		t.Fatalf("应保留入站 x-stainless-os, got %s", req.Header.Get("X-Stainless-Os"))
	}
	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Fatal("Authorization 应被设置")
	}
}

func TestApplyClaudeMessagesHeaders_UsesFingerprintWhenAbsent(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	fp := map[string]string{
		"User-Agent":     "claude-cli/2.1.220 (external, cli)",
		"X-App":          "cli",
		"X-Stainless-OS": "Linux",
	}
	applyClaudeMessagesHeaders(req, "tok", http.Header{}, false, nil, fp, "")
	if req.Header.Get("User-Agent") != "claude-cli/2.1.220 (external, cli)" {
		t.Fatalf("缺入站头时应用指纹 UA, got %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-App") != "cli" {
		t.Fatalf("应用指纹 x-app, got %s", req.Header.Get("X-App"))
	}
	if req.Header.Get("Anthropic-Beta") == "" || !strings.Contains(req.Header.Get("Anthropic-Beta"), "oauth-2025-04-20") {
		t.Fatal("anthropic-beta 应含 oauth")
	}
}

func TestApplyClaudeMessagesHeaders_ForceOverridesIncoming(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	incoming := http.Header{}
	incoming.Set("user-agent", "claude-cli/9.9.9 (external, cli)")
	incoming.Set("x-stainless-os", "MacOS")
	fp := map[string]string{"User-Agent": "claude-cli/1.0.0 (external, cli)", "X-Stainless-OS": "Linux"}
	applyClaudeMessagesHeaders(req, "tok", incoming, false, nil, fp, "force")
	// force 模式:账号指纹无条件覆盖入站身份头。
	if req.Header.Get("User-Agent") != "claude-cli/1.0.0 (external, cli)" {
		t.Fatalf("force 应用指纹 UA, got %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-Stainless-Os") != "Linux" {
		t.Fatalf("force 应用指纹 x-stainless-os, got %s", req.Header.Get("X-Stainless-Os"))
	}
}

func TestApplyClaudeMessagesHeadersRecordsFinalUserAgent(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req = req.WithContext(withUserAgentAudit(context.Background()))
	incoming := http.Header{}
	incoming.Set("User-Agent", "curl/8.7.1")
	fingerprint := map[string]string{"User-Agent": "claude-cli/2.1.220 (external, cli)"}

	applyClaudeMessagesHeaders(req, "tok", incoming, false, nil, fingerprint, "force")

	got, ok := upstreamUserAgentAudit(req.Context())
	if !ok {
		t.Fatal("Claude 出站请求应记录最终 User-Agent")
	}
	if got != "claude-cli/2.1.220 (external, cli)" {
		t.Fatalf("审计的 upstream User-Agent = %q, want stable fingerprint", got)
	}
}

func TestExecuteClaudeMessagesRequestClearsStaleUserAgentAudit(t *testing.T) {
	ctx := withUserAgentAudit(context.Background())
	RecordUpstreamUserAgent(ctx, "stale-client/1.0")
	account := &auth.Account{UpstreamType: auth.UpstreamClaude}
	_, _ = ExecuteClaudeMessagesRequest(ctx, account, []byte(`{"model":"claude-haiku-4-5","messages":[]}`), "", nil, "force")

	if _, ok := upstreamUserAgentAudit(ctx); ok {
		t.Fatal("Claude attempt should clear a previous attempt's User-Agent audit before transport")
	}
}

func TestApplyClaudeMessagesHeadersForceCompletesPartialFingerprint(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	incoming := http.Header{}
	incoming.Set("User-Agent", "curl/8.7.1")
	incoming.Set("X-Stainless-OS", "Linux")
	fingerprint := map[string]string{"User-Agent": "claude-cli/2.1.220 (external, cli)"}

	applyClaudeMessagesHeaders(req, "tok", incoming, false, nil, fingerprint, "force")
	if req.Header.Get("User-Agent") != "claude-cli/2.1.220 (external, cli)" {
		t.Fatalf("force UA = %q", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("X-Stainless-OS") == "Linux" || strings.TrimSpace(req.Header.Get("X-Stainless-OS")) == "" {
		t.Fatalf("force must not inherit a partial fingerprint's inbound OS: %q", req.Header.Get("X-Stainless-OS"))
	}
	for _, name := range auth.ClaudeIdentityHeaderNames {
		if strings.TrimSpace(req.Header.Get(name)) == "" {
			t.Fatalf("force fingerprint missing %s", name)
		}
	}
}

func TestApplyClaudeMessagesHeaders_EmptyUAFallsBackToEffectiveClaudeCLIVersion(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	auth.SetClaudeSyncedCLIVersion("2.1.300")

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	applyClaudeMessagesHeaders(req, "tok", http.Header{}, false, nil, nil, "preserve")
	if got := req.Header.Get("User-Agent"); got != "claude-cli/2.1.300 (external, cli)" {
		t.Fatalf("empty-UA fallback = %q, want claude-cli/2.1.300 (external, cli)", got)
	}
}

func TestApplyClaudeMessagesHeadersRewritesFixedClaudeCLIVersion(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	incoming := http.Header{}
	incoming.Set("User-Agent", "claude-cli/2.1.205 (external, cli)")
	applyClaudeMessagesHeadersWithVersion(req, "tok", incoming, false, nil, nil, "preserve", "2.1.251")
	if got := req.Header.Get("User-Agent"); got != "claude-cli/2.1.251 (external, cli)" {
		t.Fatalf("fixed Claude CLI UA = %q", got)
	}
}

func TestAlignClaudeOutboundUserAgent(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	auth.SetClaudeSyncedCLIVersion("")
	cases := []struct {
		name, outbound, required, wantUA string
		wantDeny                         bool
	}{
		{"no requirement", "claude-cli/2.1.219 (external, cli)", "", "claude-cli/2.1.219 (external, cli)", false},
		{"already satisfied", "claude-cli/2.1.258 (external, cli)", "2.1.251", "claude-cli/2.1.258 (external, cli)", false},
		{"stale fingerprint bumped to effective", "claude-cli/2.1.219 (external, cli)", "2.1.251", "claude-cli/" + auth.BuiltinClaudeCLIVersion + " (external, cli)", false},
		{"non-cli untouched", "Go-http-client/1.1", "2.1.251", "Go-http-client/1.1", false},
		{"effective still too old", "claude-cli/2.1.219 (external, cli)", "9.9.9", "claude-cli/2.1.219 (external, cli)", true},
	}
	for _, tc := range cases {
		gotUA, deny := alignClaudeOutboundUserAgent(tc.outbound, tc.required)
		if gotUA != tc.wantUA || (deny != "") != tc.wantDeny {
			t.Errorf("%s: ua=%q deny=%q wantUA=%q wantDeny=%v", tc.name, gotUA, deny, tc.wantUA, tc.wantDeny)
		}
	}
}

func TestApplyClaudeOutboundVersionAlignment_BumpsForcedFingerprintForFable(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	auth.SetClaudeSyncedCLIVersion("")

	ctx := withUserAgentAudit(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.219 (external, cli)")
	RecordUpstreamUserAgent(ctx, req.Header.Get("User-Agent")) // mimic applyClaudeMessagesHeaders

	required := claudeOutboundRequiredVersion(auth.ClaudeClientDecision{RequiredVersion: "2.1.251", IsCLI: true}, "claude-fable-5-1")
	if perr := applyClaudeOutboundVersionAlignment(req, required); perr != nil {
		t.Fatalf("expected no deny, got %v", perr)
	}
	wantUA := "claude-cli/" + auth.BuiltinClaudeCLIVersion + " (external, cli)"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("req User-Agent = %q, want %q", got, wantUA)
	}
	if audited, ok := upstreamUserAgentAudit(ctx); !ok || audited != wantUA {
		t.Fatalf("upstreamUserAgentAudit = (%q, %v), want (%q, true)", audited, ok, wantUA)
	}

	// Already-satisfied UA must be left untouched, and the audit must not be
	// rewritten either.
	ctx2 := withUserAgentAudit(context.Background())
	req2, _ := http.NewRequestWithContext(ctx2, "POST", "https://api.anthropic.com/v1/messages", nil)
	satisfiedUA := "claude-cli/2.1.258 (external, cli)"
	req2.Header.Set("User-Agent", satisfiedUA)
	RecordUpstreamUserAgent(ctx2, satisfiedUA) // sentinel: must survive unchanged

	required2 := claudeOutboundRequiredVersion(auth.ClaudeClientDecision{RequiredVersion: "2.1.251", IsCLI: true}, "claude-fable-5-1")
	if perr := applyClaudeOutboundVersionAlignment(req2, required2); perr != nil {
		t.Fatalf("expected no deny for already-satisfied UA, got %v", perr)
	}
	if got := req2.Header.Get("User-Agent"); got != satisfiedUA {
		t.Fatalf("req2 User-Agent = %q, want unchanged %q", got, satisfiedUA)
	}
	if audited, ok := upstreamUserAgentAudit(ctx2); !ok || audited != satisfiedUA {
		t.Fatalf("upstreamUserAgentAudit(ctx2) = (%q, %v), want unchanged (%q, true)", audited, ok, satisfiedUA)
	}
}

func TestExecuteClaudeMessagesRequestWithPolicy_DeniesWhenForcedFingerprintTooOld(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	auth.SetClaudeSyncedCLIVersion("")
	ctx := withUserAgentAudit(context.Background())
	account := &auth.Account{DBID: 251, UpstreamType: auth.UpstreamClaude, AccessToken: "tok", CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/9.9.9 (external, cli)")
	policy := auth.ClaudeClientPolicy{Platform: auth.ClaudeClientPlatformAny, VersionPolicy: auth.ClaudeVersionPolicyMinimum, ClientVersion: "9.9.9"}
	_, err := ExecuteClaudeMessagesRequestWithPolicy(ctx, account, []byte(`{"model":"claude-opus-5","messages":[]}`), "", headers, "force", policy)
	var perr *Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusUpgradeRequired || perr.Code != "claude_client_policy" {
		t.Fatalf("expected local 426 claude_client_policy, got %v", err)
	}
	if !strings.Contains(perr.Message, "2.1.219") || !strings.Contains(perr.Message, "9.9.9") {
		t.Fatalf("message should name outbound and required versions: %s", perr.Message)
	}
}

func TestClaudeOutboundRequiredVersion_UsesModelFloorForNonCLIInbound(t *testing.T) {
	// 入站不是 CLI(无 required),但 force 指纹是旧 CLI UA 且模型有下限:出站仍需对齐。
	gotUA, deny := alignClaudeOutboundUserAgent("claude-cli/2.1.219 (external, cli)", claudeOutboundRequiredVersion(auth.ClaudeClientDecision{}, "claude-fable-5-1"))
	if deny != "" || !strings.Contains(gotUA, auth.BuiltinClaudeCLIVersion) {
		t.Fatalf("ua=%q deny=%q", gotUA, deny)
	}
}

func TestIsClaudeClientCompatibilityError(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"Claude Code 2.1.205 does not support this model; version 2.1.251 or newer is required."}}`)
	if !isClaudeClientCompatibilityError(http.StatusBadRequest, body) {
		t.Fatal("version-gated Claude 400 should be classified as client compatibility")
	}
	if isClaudeClientCompatibilityError(http.StatusBadRequest, []byte(`{"error":{"type":"invalid_request_error","message":"invalid max_tokens"}}`)) {
		t.Fatal("ordinary invalid request must not be classified as client compatibility")
	}
}

func TestApplyClaudeMessagesHeaders_SDKFixedHeaders(t *testing.T) {
	ctx := WithClaudeSessionID(context.Background(), "8f1c3a6e-1111-7222-8333-444455556666")
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", nil)
	fp := map[string]string{"User-Agent": "claude-cli/2.1.259 (external, cli)"}
	body := []byte(`{"model":"claude-opus-5","messages":[]}`)
	applyClaudeMessagesHeaders(req, "tok", http.Header{}, false, body, fp, "")
	if got := req.Header.Get("X-Stainless-Retry-Count"); got != "0" {
		t.Fatalf("X-Stainless-Retry-Count = %q, want 0", got)
	}
	if got := req.Header.Get("X-Stainless-Timeout"); got != "600" {
		t.Fatalf("X-Stainless-Timeout = %q, want 600", got)
	}
	if got := req.Header.Get("Anthropic-Dangerous-Direct-Browser-Access"); got != "true" {
		t.Fatalf("browser-access header = %q, want true", got)
	}
	if got := req.Header.Get("X-Claude-Code-Session-Id"); got != "8f1c3a6e-1111-7222-8333-444455556666" {
		t.Fatalf("X-Claude-Code-Session-Id = %q, want ctx value", got)
	}
}

func TestApplyClaudeMessagesHeaders_PreservesIncomingSessionID(t *testing.T) {
	incoming := http.Header{}
	incoming.Set("X-Claude-Code-Session-Id", "11111111-2222-7333-8444-555555555555")
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	applyClaudeMessagesHeaders(req, "tok", incoming, false, nil, nil, "")
	if got := req.Header.Get("X-Claude-Code-Session-Id"); got != "11111111-2222-7333-8444-555555555555" {
		t.Fatalf("入站真实会话 ID 应保留, got %q", got)
	}
}

func TestInjectClaudeMetadataUserID(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","messages":[]}`)
	out := injectClaudeMetadataUserID(body, claudeBodyIdentity{deviceID: "d1", accountUUID: "a1", sessionID: "s1"})
	raw := gjson.GetBytes(out, "metadata.user_id").String()
	var parsed struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("user_id 应为 JSON 字符串, got %q: %v", raw, err)
	}
	if parsed.DeviceID != "d1" || parsed.AccountUUID != "a1" || parsed.SessionID != "s1" {
		t.Fatalf("user_id 字段不符: %+v", parsed)
	}
}

func TestInjectClaudeMetadataUserID_KeepsExisting(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","metadata":{"user_id":"business-user-keep"},"messages":[]}`)
	out := injectClaudeMetadataUserID(body, claudeBodyIdentity{deviceID: "d2", sessionID: "s2"})
	if got := gjson.GetBytes(out, "metadata.user_id").String(); got != "business-user-keep" {
		t.Fatalf("下游已带 user_id 时应保留, got %q", got)
	}
}

func TestClaudeBillingBlockJSON_Stable(t *testing.T) {
	a := claudeBillingBlockJSON("2.1.259")
	b := claudeBillingBlockJSON("2.1.259")
	if a != b {
		t.Fatalf("计费块应确定性生成: %s vs %s", a, b)
	}
	if !strings.Contains(a, "cc_version=2.1.259.") || !strings.Contains(a, "cc_entrypoint=cli;") {
		t.Fatalf("计费块形态不符: %s", a)
	}
	if gjson.Get(a, "cache_control").Exists() {
		t.Fatalf("计费块不应带 cache_control: %s", a)
	}
}

func TestAlignClaudeBillingBlockMatchesFinalUserAgent(t *testing.T) {
	body := []byte(`{"system":[` + claudeBillingBlockJSON("2.1.258") + `,{"type":"text","text":"preamble"}]}`)

	// UA 版本与计费块一致:原样返回。
	if out := alignClaudeBillingBlock(body, "claude-cli/2.1.258 (external, cli)"); !bytes.Equal(out, body) {
		t.Fatalf("版本一致时不应改写: %s", out)
	}
	// UA 是账号指纹自带的新版本:计费块 cc_version 对齐到 UA。
	out := alignClaudeBillingBlock(body, "claude-cli/2.1.260 (external, cli)")
	if !bytes.Equal(out, body) {
		if v := gjson.GetBytes(out, "system.0.text").String(); !strings.Contains(v, "cc_version=2.1.260.") {
			t.Fatalf("计费块应对齐到 UA 版本: %s", v)
		}
		if !json.Valid(out) {
			t.Fatalf("对齐后 body 必须仍是严格合法 JSON: %s", out)
		}
		if got := gjson.GetBytes(out, "system.1.text").String(); got != "preamble" {
			t.Fatalf("对齐不应破坏其余 system 块: %q", got)
		}
	} else {
		t.Fatal("UA 版本不同时必须改写计费块")
	}
	// 非 CLI UA:不动。
	if out := alignClaudeBillingBlock(body, "curl/8.4.0"); !bytes.Equal(out, body) {
		t.Fatalf("非 CLI UA 不应改写: %s", out)
	}
	// system 首块不是计费块:不动。
	noBilling := []byte(`{"system":[{"type":"text","text":"hello"}]}`)
	if out := alignClaudeBillingBlock(noBilling, "claude-cli/2.1.260 (external, cli)"); !bytes.Equal(out, noBilling) {
		t.Fatalf("首块非计费块时不应改写: %s", out)
	}
}

func TestClaudeUpstreamSessionID(t *testing.T) {
	if got := claudeUpstreamSessionID("11111111-2222-7333-8444-555555555555"); got != "11111111-2222-7333-8444-555555555555" {
		t.Fatalf("UUID 应原样透传, got %q", got)
	}
	derived := claudeUpstreamSessionID("api-key:42:prompt-cache-key")
	if derived != claudeUpstreamSessionID("api-key:42:prompt-cache-key") {
		t.Fatal("派生会话 ID 应稳定")
	}
	if _, err := uuid.Parse(derived); err != nil {
		t.Fatalf("派生会话 ID 应为 UUID: %q", derived)
	}
	if claudeUpstreamSessionID("") != "" {
		t.Fatal("空输入应返回空串")
	}
}
