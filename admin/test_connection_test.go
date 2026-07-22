package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestConnectionTestModelValidation(t *testing.T) {
	if !isSupportedConnectionTestModel("gpt-5.5") {
		t.Fatal("gpt-5.5 should be allowed for connection tests")
	}
	if isSupportedConnectionTestModel("gpt-image-2") {
		t.Fatal("image models should not be allowed for connection tests")
	}
	if isSupportedConnectionTestModel("unknown-model") {
		t.Fatal("unknown models should not be allowed for connection tests")
	}
}

func TestBuildTestPayloadUsesSelectedModel(t *testing.T) {
	payload := buildTestPayload("gpt-5.5")
	if got := gjson.GetBytes(payload, "model").String(); got != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", got)
	}
	if !gjson.GetBytes(payload, "stream").Bool() {
		t.Fatal("stream should be true")
	}
}

func TestBuildConnectionTestPayloadUsesStoreContent(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetTestContent("say pong")

	payload := buildConnectionTestPayload(store, "gpt-5.5")
	if got := gjson.GetBytes(payload, "input.0.content.0.text").String(); got != "say pong" {
		t.Fatalf("test content = %q, want say pong", got)
	}
}

// TestBuildConnectionTestPayloadRandomizesMultiLineContent 验证多行测活内容
// 按行随机抽取并展开变量（issue #320）。
func TestBuildConnectionTestPayloadRandomizesMultiLineContent(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetTestContent("ping-a\nping-b\ncount {{rand:1-1}}")

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		payload := buildConnectionTestPayload(store, "gpt-5.5")
		seen[gjson.GetBytes(payload, "input.0.content.0.text").String()] = true
	}
	for _, want := range []string{"ping-a", "ping-b", "count 1"} {
		if !seen[want] {
			t.Fatalf("candidate %q never sent in 200 draws; seen=%v", want, seen)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("unexpected payload variants: %v", seen)
	}
}

func TestFormatUsageLimitedTestErrorReportsSuccessfulProbeAsLimited(t *testing.T) {
	msg, limited := formatUsageLimitedTestError(proxy.CodexUsageSyncResult{
		Premium5hRateLimited: true,
		UsagePct5h:           100,
		Reset5hAt:            time.Now().Add(time.Hour),
	})

	if !limited {
		t.Fatal("limited = false, want true")
	}
	for _, want := range []string{"返回 200", "5h 用量头", "限流状态"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q does not contain %q", msg, want)
		}
	}
}

func TestFormatUsageLimitedTestErrorAcceptsSuccessfulProbeWhenIgnored(t *testing.T) {
	msg, limited := formatUsageLimitedTestError(proxy.CodexUsageSyncResult{
		Premium5hRateLimited:     false,
		UsagePct5h:               100,
		HasUsage5h:               true,
		UsageWindowLimitsIgnored: true,
	})

	if limited || msg != "" {
		t.Fatalf("formatUsageLimitedTestError() = (%q, %v), want empty successful result", msg, limited)
	}
}

func TestConnectionUnauthorizedRecordsErrorMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamBody := `{"error":{"message":"Your authentication token has been invalidated.","code":"token_invalidated"},"status":401}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         42,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "sk-test",
		Models:       []string{"gpt-4o-mini"},
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := &Handler{store: store}
	router := gin.New()
	router.GET("/api/admin/accounts/:id/test", handler.TestConnection)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/accounts/42/test", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "token_invalidated") {
		t.Fatalf("SSE response %q does not contain token_invalidated", recorder.Body.String())
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	account.Mu().RLock()
	errorMsg := account.ErrorMsg
	account.Mu().RUnlock()
	if !strings.Contains(errorMsg, "token_invalidated") {
		t.Fatalf("ErrorMsg = %q, want token_invalidated", errorMsg)
	}
}

func TestConnectionDeletedAgentRuntimeMarksBanned(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamBody := `{"error":{"message":"Agent runtime has been deleted.","type":null,"code":"biscuit_baker_service_agent_error_status","param":null},"status":403}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         42,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "sk-test",
		Models:       []string{"gpt-4o-mini"},
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := &Handler{store: store}
	router := gin.New()
	router.GET("/api/admin/accounts/:id/test", handler.TestConnection)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/accounts/42/test", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	account.Mu().RLock()
	errorMsg := account.ErrorMsg
	account.Mu().RUnlock()
	if !strings.Contains(errorMsg, "Agent runtime has been deleted") {
		t.Fatalf("ErrorMsg = %q, want deleted runtime message", errorMsg)
	}
}

func TestExtractCompletedOutputText(t *testing.T) {
	event := []byte(`{
		"type":"response.completed",
		"response":{
			"status":"completed",
			"output":[
				{"type":"message","content":[{"type":"output_text","text":"hello from completed"}]}
			]
		}
	}`)

	if got := extractCompletedOutputText(event); got != "hello from completed" {
		t.Fatalf("output text = %q, want completed text", got)
	}
}

func TestFormatUpstreamTestErrorIncludesMessageAndEvent(t *testing.T) {
	event := []byte(`{
		"type":"response.failed",
		"response":{
			"error":{"message":"model unavailable","code":"model_not_available"}
		}
	}`)

	got := formatUpstreamTestError(event, "fallback")
	for _, want := range []string{"model unavailable", "model_not_available", "上游事件", "response.failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted error %q does not contain %q", got, want)
		}
	}
}

func TestFormatNoOutputUpstreamErrorIncludesCompletedEvent(t *testing.T) {
	event := []byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`)

	got := formatNoOutputUpstreamError(event)
	for _, want := range []string{"没有返回文本输出", "上游事件", `"output": []`} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted no-output error %q does not contain %q", got, want)
		}
	}
}
