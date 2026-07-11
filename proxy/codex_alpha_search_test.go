package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// standalone 搜索透传：请求体、上游状态码与响应体原样带回，OAuth 凭据注入。
func TestCodexAlphaSearchHandler_PassesThroughRequestAndResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const searchBody = `{"id":"srch_1","model":"gpt-5.6-sol","commands":{"search_query":[{"q":"golang generics"}]}}`
	const upstreamResult = `{"output":[{"type":"message","content":[{"type":"output_text","text":"results"}]}],"encrypted_output":"gAAAA"}`

	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		if got := r.Header.Get("Authorization"); got != "Bearer at-search" {
			t.Errorf("Authorization = %q, want Bearer at-search", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "acc-s" {
			t.Errorf("chatgpt-account-id = %q, want acc-s", got)
		}
		if got := r.Header.Get("Originator"); got != Originator {
			t.Errorf("Originator = %q, want %q", got, Originator)
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "codex-") {
			t.Errorf("User-Agent = %q, want codex client UA", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamResult))
	}))
	defer upstream.Close()

	codexAlphaSearchURLForTest = upstream.URL
	defer func() { codexAlphaSearchURLForTest = "" }()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-search", AccountID: "acc-s", PlanType: "plus"})
	handler := NewHandler(store, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", bytes.NewReader([]byte(searchBody)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = req

	handler.CodexAlphaSearchHandler(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if string(seenBody) != searchBody {
		t.Errorf("upstream body = %s, want verbatim passthrough", seenBody)
	}
	if rec.Body.String() != upstreamResult {
		t.Errorf("response body = %s, want verbatim upstream result", rec.Body.String())
	}
}

// 上游 4xx 原样透传（保留 CLI 能理解的真实错误语义），不包装成 502。
func TestCodexAlphaSearchHandler_PassesThroughUpstreamError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_exceeded"}}`))
	}))
	defer upstream.Close()

	codexAlphaSearchURLForTest = upstream.URL
	defer func() { codexAlphaSearchURLForTest = "" }()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-search", PlanType: "plus"})
	handler := NewHandler(store, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"id":"srch_2","model":"gpt-5.6-sol"}`))
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = req

	handler.CodexAlphaSearchHandler(ctx)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 passthrough; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Errorf("body = %s, want upstream error payload", rec.Body.String())
	}
}

// 池中只有中转（OpenAI Responses API）账号时 fast-fail 503：搜索端点只存在于 ChatGPT 后端。
func TestCodexAlphaSearchHandler_RelayOnlyPoolFastFails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "http://example.invalid",
		APIKey:       "sk-relay",
		Models:       []string{"gpt-5.5"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"id":"srch_3","model":"gpt-5.5"}`))
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = req

	handler.CodexAlphaSearchHandler(ctx)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}
