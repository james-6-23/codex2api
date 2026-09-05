package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestNormalizeClaudeRequestBodyCanonicalizesBeforeReview(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"he` + string(rune(0x202E)) + `llo"}],"service_tier":"priority","inference_geo":"us","speed":"fast","safety_identifier":"user-42"}`)
	out, err := normalizeClaudeRequestBody(body, auth.DefaultClaudeSecurityConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hello" {
		t.Fatalf("canonical text = %q, want hello", got)
	}
	for _, field := range []string{"service_tier", "inference_geo", "speed", "safety_identifier"} {
		if gjson.GetBytes(out, field).Exists() {
			t.Fatalf("default security policy kept %s: %s", field, out)
		}
	}
}

// TestMessagesIngressKeepsCodexPriorityTierOffNativeRoute 钉住跨渠道边界。
//
// /v1/messages 同时服务 Codex 翻译、Grok 与 Antigravity 中转。Claude 的出站策略
// 默认要删掉 speed / service_tier / inference_geo / safety_identifier —— 无差别套
// 用就会把 Codex 账号的 priority 档位连同用量归因一起静默吞掉:Anthropic 侧请求
// 加速用的是 speed:"fast",路由 stub 正是读它才写出 service_tier:priority,
// 而规范化默认把 speed 删了,stub 就再也拿不到档位。
func TestMessagesIngressKeepsCodexPriorityTierOffNativeRoute(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","speed":"fast","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	h := &Handler{}
	models := []string{"gpt-5.4"}

	// 非 Claude 路由:handler 用未改写的 rawBody,speed 存活 → 档位到得了 Codex。
	if got := extractServiceTier(h.resolveMessagesRoutingBody(body, "claude-sonnet-4-5", models)); got != "priority" {
		t.Fatalf("非原生路由应保住 priority 档位, got %q", got)
	}

	// Claude 出站策略本身照旧剥离(它对 Anthropic 上游确实不合法)。
	normalized, err := normalizeClaudeRequestBody(body, auth.DefaultClaudeSecurityConfig())
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(normalized, "speed").Exists() {
		t.Fatalf("Claude 原生出站仍应剥离 speed: %s", normalized)
	}
	// 而这正是无差别套用的后果:档位没了。回归就长这样。
	if got := extractServiceTier(h.resolveMessagesRoutingBody(normalized, "claude-sonnet-4-5", models)); got != "" {
		t.Fatalf("规范化后的体本就不该还带档位, got %q", got)
	}
}

// TestNativeClaudeRouteDecisionIsMemoizedPerRequest 钉住"每请求只判一次"。
//
// 入站是否套用 Claude 出站策略、模型路由是否保留 claude-* 原生 ID,问的是同一个
// 判定。两次独立计算若不一致(中间账号变得不可用),就会出现"按 Claude 剥过字段
// 的体被送去 Codex 账号"。判定本身还要扫号池,万级号池下重复全扫是热路径开销。
func TestNativeClaudeRouteDecisionIsMemoizedPerRequest(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	h := &Handler{store: store}
	gin.SetMode(gin.TestMode)

	requestCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	if h.nativeClaudeRouteForRequest(requestCtx, "claude-sonnet-4-5") {
		t.Fatal("空号池不该判定为 Claude 原生路由")
	}

	store.AddAccount(&auth.Account{
		DBID:         92,
		UpstreamType: auth.UpstreamClaude,
		AccessToken:  "claude-token",
		Status:       auth.StatusReady,
		Models:       []string{"claude-sonnet-4-5"},
	})
	if h.nativeClaudeRouteForRequest(requestCtx, "claude-sonnet-4-5") {
		t.Fatal("同一请求内的路由判定必须稳定,不能中途翻转")
	}

	nextRequestCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	if !h.nativeClaudeRouteForRequest(nextRequestCtx, "claude-sonnet-4-5") {
		t.Fatal("新请求看到可用 Claude 账号时应判定为原生路由")
	}
}

func TestNormalizeClaudeRequestBodyDoesNotInjectOAuthPreamble(t *testing.T) {
	out, err := normalizeClaudeRequestBody([]byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}]}`), auth.DefaultClaudeSecurityConfig())
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "system").Exists() {
		t.Fatalf("request canonicalizer should not add native OAuth system metadata: %s", out)
	}
}

func TestNormalizeClaudeRequestBodyAllowsExplicitSensitiveFields(t *testing.T) {
	cfg := auth.DefaultClaudeSecurityConfig()
	cfg.AllowServiceTier = true
	cfg.AllowInferenceGeo = true
	cfg.AllowSpeed = true
	cfg.AllowSafetyIdentifier = true
	out, err := normalizeClaudeRequestBody([]byte(`{"model":"claude-sonnet-5","messages":[],"service_tier":"priority","inference_geo":"us","speed":"fast","safety_identifier":"user-42"}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"service_tier", "inference_geo", "speed", "safety_identifier"} {
		if !gjson.GetBytes(out, field).Exists() {
			t.Fatalf("explicitly allowed field %s was removed", field)
		}
	}
}

func TestNormalizeClaudeRequestBodyRejectsResourceLimits(t *testing.T) {
	cfg := auth.DefaultClaudeSecurityConfig()
	cfg.MaxOutputTokens = 8
	cfg.MaxToolCount = 1
	cfg.MaxToolSchemaBytes = 32
	tooManyTokens := []byte(`{"model":"claude-sonnet-5","max_tokens":9,"messages":[]}`)
	if _, err := normalizeClaudeRequestBody(tooManyTokens, cfg); err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("max_tokens overflow error = %v", err)
	}
	if _, err := normalizeClaudeRequestBody([]byte(`{"model":"claude-sonnet-5","max_tokens":8.5,"messages":[]}`), cfg); err == nil || !strings.Contains(err.Error(), "integer") {
		t.Fatalf("fractional max_tokens error = %v", err)
	}
	tooManyTools := []byte(`{"model":"claude-sonnet-5","messages":[],"tools":[{"name":"one","input_schema":{"type":"object"}},{"name":"two","input_schema":{"type":"object"}}]}`)
	if _, err := normalizeClaudeRequestBody(tooManyTools, cfg); err == nil || !strings.Contains(err.Error(), "tools") {
		t.Fatalf("tool count overflow error = %v", err)
	}
	tooLargeSchema := []byte(`{"model":"claude-sonnet-5","messages":[],"tools":[{"name":"one","input_schema":{"description":"this schema is intentionally longer than thirty-two bytes"}}]}`)
	if _, err := normalizeClaudeRequestBody(tooLargeSchema, cfg); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("tool schema overflow error = %v", err)
	}
}

func TestNormalizeClaudeRequestBodyDefaultsDoNotCapSub2APIRequests(t *testing.T) {
	tools := strings.Repeat(`{"name":"tool","input_schema":{"type":"object"}},`, 24)
	tools = strings.TrimSuffix(tools, ",")
	body := []byte(`{"model":"claude-opus-4-7","max_tokens":13100,"messages":[],"tools":[` + tools + `]}`)
	out, err := normalizeClaudeRequestBody(body, auth.DefaultClaudeSecurityConfig())
	if err != nil {
		t.Fatalf("Sub2API-compatible request was rejected: %v", err)
	}
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 13100 {
		t.Fatalf("max_tokens = %d, want 13100", got)
	}
	if got := len(gjson.GetBytes(out, "tools").Array()); got != 24 {
		t.Fatalf("tool count = %d, want 24", got)
	}
}

func TestNormalizeClaudeRequestBodyNormalizesLegacyMaxTokensAlias(t *testing.T) {
	out, err := normalizeClaudeRequestBody([]byte(`{"model":"claude-opus-4-7","max_tokens_to_sample":13100,"messages":[]}`), auth.DefaultClaudeSecurityConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 13100 {
		t.Fatalf("max_tokens = %d, want 13100", got)
	}
	if gjson.GetBytes(out, "max_tokens_to_sample").Exists() {
		t.Fatalf("legacy max_tokens_to_sample should not reach Anthropic: %s", out)
	}
}

func TestNormalizeClaudeRequestBodyKeepsContextManagement(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","max_tokens":13100,"messages":[],"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`)
	out, err := normalizeClaudeRequestBody(body, auth.DefaultClaudeSecurityConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(out, "context_management").Exists() {
		t.Fatalf("context_management is sent with its paired beta, must survive normalization: %s", out)
	}
	for _, field := range []string{"thinking", "output_config"} {
		if !gjson.GetBytes(out, field).Exists() {
			t.Fatalf("supported field %s was removed: %s", field, out)
		}
	}
}

func TestMergeAnthropicBetaUsesRequiredAndAllowlist(t *testing.T) {
	incoming := http.Header{}
	incoming.Set("anthropic-beta", "unknown-beta, approved-beta, oauth-2025-04-20")
	cfg := auth.DefaultClaudeSecurityConfig()
	cfg.AllowedBetaHeaders = []string{"approved-beta"}
	got := mergeAnthropicBetaWithConfig(incoming, cfg)
	if !strings.Contains(got, "oauth-2025-04-20") || !strings.Contains(got, "approved-beta") || strings.Contains(got, "unknown-beta") {
		t.Fatalf("filtered Beta headers = %q", got)
	}
}
