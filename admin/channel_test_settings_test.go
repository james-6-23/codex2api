package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newChannelTestSettingsHandler(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	router := gin.New()
	router.GET("/api/admin/settings/channel-tests", handler.GetChannelTestSettings)
	router.PUT("/api/admin/settings/channel-tests", handler.UpdateChannelTestSettings)
	return handler, router
}

func TestChannelTestSettingsRoundTripAndPartialUpdate(t *testing.T) {
	handler, router := newChannelTestSettingsHandler(t)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/settings/channel-tests", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"default_test_content"`) {
		t.Fatalf("GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// 号池为空时下拉仍要有默认候选。
	if body := recorder.Body.String(); !strings.Contains(body, `"gemini-3.8-flash-low"`) || !strings.Contains(body, `"claude-haiku-4-5"`) {
		t.Fatalf("GET body missing default model choices: %s", body)
	}

	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/admin/settings/channel-tests", strings.NewReader(`{"antigravity":{"test_model":" gemini-3.8-flash-low ","test_content":" 你的知识截止日期? ","test_concurrency":7}}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	cfg := handler.channelTestConfig(context.Background())
	if cfg.Antigravity.TestModel != "gemini-3.8-flash-low" || cfg.Antigravity.TestContent != "你的知识截止日期?" || cfg.Antigravity.TestConcurrency != 7 {
		t.Fatalf("antigravity settings = %+v, want trimmed values", cfg.Antigravity)
	}

	// 只传 claude 不得清掉 antigravity。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPut, "/api/admin/settings/channel-tests", strings.NewReader(`{"claude":{"test_model":"claude-haiku-4-5","test_content":""}}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT claude status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	persisted, err := handler.db.LoadChannelTestConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Antigravity.TestModel != "gemini-3.8-flash-low" || persisted.Claude.TestModel != "claude-haiku-4-5" || persisted.Claude.TestContent != "" {
		t.Fatalf("persisted = %+v, want both channels kept", persisted)
	}
}

func TestChannelTestSettingsRejectsInvalidModels(t *testing.T) {
	_, router := newChannelTestSettingsHandler(t)
	for _, body := range []string{
		`{"claude":{"test_model":"gpt-5.5"}}`,
		`{"antigravity":{"test_model":"gemini-image-2"}}`,
		`{"antigravity":{"test_concurrency":201}}`,
		`{"claude":{"test_concurrency":-1}}`,
		`{}`,
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/api/admin/settings/channel-tests", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("PUT %s status=%d, want 400 (body=%s)", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestConnectionTestUsesChannelSettingsForAntigravity(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := newAntigravityConnectionTestAccount()
	account.Models = []string{"gemini-3.5-flash-extra-low", "gemini-3.5-flash-low", "gemini-3-flash-agent", "claude-sonnet-4-6"}
	store.AddAccount(account)
	handler := &Handler{store: store}
	handler.channelTestCfg.Store(&database.ChannelTestConfig{
		Antigravity: database.ChannelTestSettings{TestModel: "claude-sonnet-4-6", TestContent: "渠道专用测活 {{date}}"},
	})

	model, err := handler.connectionTestModelForAccount(context.Background(), account, "")
	if err != nil || model != "claude-sonnet-4-6" {
		t.Fatalf("model=%q err=%v, want configured channel default", model, err)
	}
	payload := handler.buildAccountConnectionTestPayload(context.Background(), account, model, auth.ClaudeSecurityConfig{})
	if !strings.Contains(string(payload), "渠道专用测活") || strings.Contains(string(payload), "{{date}}") {
		t.Fatalf("payload %s must carry the rendered channel test content", payload)
	}

	// 配置的模型不在账号目录时退回自动选模，而不是报错。
	handler.channelTestCfg.Store(&database.ChannelTestConfig{Antigravity: database.ChannelTestSettings{TestModel: "gemini-9-flash-low"}})
	model, err = handler.connectionTestModelForAccount(context.Background(), account, "")
	if err != nil || model != "gemini-3.5-flash-low" {
		t.Fatalf("fallback model=%q err=%v", model, err)
	}
}

func TestBatchTestConcurrencyUsesChannelValueForHomogeneousBatches(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetTestConcurrency(5)
	handler := &Handler{store: store}
	handler.channelTestCfg.Store(&database.ChannelTestConfig{
		Antigravity: database.ChannelTestSettings{TestConcurrency: 2},
	})
	antigravity := newAntigravityConnectionTestAccount()
	codex := &auth.Account{DBID: 9, AccessToken: "codex-token"}

	if got := handler.batchTestConcurrency(context.Background(), []*auth.Account{antigravity, antigravity}); got != 2 {
		t.Fatalf("antigravity-only batch concurrency = %d, want channel value 2", got)
	}
	if got := handler.batchTestConcurrency(context.Background(), []*auth.Account{antigravity, codex}); got != 5 {
		t.Fatalf("mixed batch concurrency = %d, want global 5", got)
	}
	if got := handler.batchTestConcurrency(context.Background(), nil); got != 5 {
		t.Fatalf("empty batch concurrency = %d, want global 5", got)
	}
	handler.channelTestCfg.Store(&database.ChannelTestConfig{})
	if got := handler.batchTestConcurrency(context.Background(), []*auth.Account{antigravity}); got != 5 {
		t.Fatalf("unconfigured channel concurrency = %d, want global 5", got)
	}
}
