package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestApplyCodexRequestHeadersAuditsFinalCustomUserAgent(t *testing.T) {
	ctx := withUserAgentAudit(context.Background())
	req := httptest.NewRequest(http.MethodPost, "https://example.test/responses", nil).WithContext(ctx)
	account := &auth.Account{
		DBID: 42,
		CustomHeaders: map[string]string{
			"User-Agent": "account-custom-agent/1.0",
		},
	}

	applyCodexRequestHeaders(req, account, "token", "", "", nil, http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	})

	got, ok := upstreamUserAgentAudit(ctx)
	if !ok {
		t.Fatal("upstream User-Agent audit was not recorded")
	}
	if got != "account-custom-agent/1.0" {
		t.Fatalf("audited User-Agent = %q, want final account custom header", got)
	}
}

func TestUsageLogCapturesClientAndActualUpstreamUserAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousSettings := CurrentRuntimeSettings()
	normalizedUA, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"codex-audit-upstream/1.0"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	nextSettings := previousSettings
	nextSettings.ClientCompatMode = ClientCompatModeForce
	nextSettings.CodexUserAgentConfig = normalizedUA
	ApplyRuntimeSettings(nextSettings)
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	upstreamUserAgent := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamUserAgent <- r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_ua_audit","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`))
	}))
	defer upstream.Close()

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite) error = %v", err)
	}
	defer db.Close()
	db.SetUsageLogConfig(database.UsageLogModeFull, 1, 1)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0, MaxRateLimitRetries: 0})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "sk-upstream",
		Models:       []string{"gpt-5.4"},
		PlanType:     "api",
	})
	handler := NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "curl/8.7.1")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	select {
	case got := <-upstreamUserAgent:
		if got != "codex-audit-upstream/1.0" {
			t.Fatalf("upstream received User-Agent = %q, want configured override", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		logs, listErr := db.ListRecentUsageLogs(context.Background(), 10)
		if listErr != nil {
			t.Fatalf("ListRecentUsageLogs error = %v", listErr)
		}
		if len(logs) > 0 {
			log := logs[0]
			if log.ClientUserAgent != "curl/8.7.1" {
				t.Fatalf("ClientUserAgent = %q, want curl/8.7.1", log.ClientUserAgent)
			}
			if log.UpstreamUserAgent != "codex-audit-upstream/1.0" {
				t.Fatalf("UpstreamUserAgent = %q, want actual upstream value", log.UpstreamUserAgent)
			}
			if !log.UserAgentOverridden {
				t.Fatal("UserAgentOverridden = false, want true")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for usage log flush")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
