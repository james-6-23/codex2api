package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// WS 上游握手 401 经 wsrelay 还原成真实状态码响应后，Responses 主链路必须
// 走既有非 2xx 分支：usage log 记 401、账号按 unauthorized 冷却、对下游按
// issue #323 约定改写为 503 account_pool_unauthorized（而不是把 401 埋进
// transport/598）。
func TestResponsesWebsocketHandshake401CoolsAccountAndLogs401(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	upstreamBody := `{"error":{"message":"Provided authentication token is expired. Please try signing in again.","type":"invalid_request_error","code":"token_expired"}}`
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		// 模拟 wsrelay.ExecuteRequestWebsocket 对握手 401 的转换产物。
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		}, nil
	}

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	db.SetUsageLogConfig("all", 1, 1)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	account := &auth.Account{DBID: 1, AccessToken: "expired-token", PlanType: "plus"}
	store.AddAccount(account)
	handler := NewHandler(store, db, nil, nil)

	body := []byte(`{"model":"gpt-5.4","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	// 下游收到 503 池级错误，而不是裸 401（issue #323 语义：上游账号 401 是
	// 账号侧问题，不能让下游误判自己的 key 失效）。具体 code 取决于收尾路径
	// （重试耗尽 vs 无可用账号），两者都可接受。
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("downstream status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
	if code := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); code != "account_pool_unauthorized" && code != "no_available_account" {
		t.Fatalf("downstream error.code = %q, want pool-level error; body=%s", code, recorder.Body.String())
	}

	// 账号必须按 unauthorized 处置（禁用/冷却），不能留在调度池里被反复拨号。
	if account.IsAvailable() {
		t.Fatal("account should be disabled/cooled down after handshake 401")
	}

	// usage log 必须出现真实的 401 行（原缺陷：只会记成 598/transport 或被静默吞掉）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		logs, err := db.ListUsageLogsByTimeRange(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("ListUsageLogsByTimeRange: %v", err)
		}
		var found bool
		for _, l := range logs {
			if l.StatusCode == http.StatusUnauthorized && l.Endpoint == "/v1/responses" {
				found = true
				if !strings.Contains(l.ErrorMessage, "token is expired") {
					t.Fatalf("401 row error message = %q, want upstream token_expired message", l.ErrorMessage)
				}
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no 401 usage log row recorded; rows=%d", len(logs))
		}
		time.Sleep(200 * time.Millisecond)
	}
}
