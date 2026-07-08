package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// 流式请求在首 token 之前收到 response.failed 时,应中止 SSE 转发、由循环外按真实
// HTTP 错误码返回,而不是把失败包成 200 + [DONE](后者会被上游中转/计费方误判为
// 成功流并按预估 input token 计费)。见 shouldReturnHTTPErrorForResponseFailed。
func TestShouldReturnHTTPErrorForResponseFailed(t *testing.T) {
	cases := []struct {
		name         string
		eventType    string
		ttftRecorded bool
		wroteAnyBody bool
		clientGone   bool
		want         bool
	}{
		{"首 token 前的 response.failed 应返回错误码", "response.failed", false, false, false, true},
		{"response.completed 不拦截", "response.completed", false, false, false, false},
		{"已产出首 token 不拦截(维持流式收尾)", "response.failed", true, false, false, false},
		{"已向下游写过 body 不拦截(200 已发出)", "response.failed", false, true, false, false},
		{"客户端已断开不拦截(继续读上游取 usage)", "response.failed", false, false, true, false},
		{"普通内容事件不拦截", "response.output_text.delta", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldReturnHTTPErrorForResponseFailed(tc.eventType, tc.ttftRecorded, tc.wroteAnyBody, tc.clientGone)
			if got != tc.want {
				t.Errorf("shouldReturnHTTPErrorForResponseFailed(%q, ttft=%v, wrote=%v, gone=%v) = %v, want %v",
					tc.eventType, tc.ttftRecorded, tc.wroteAnyBody, tc.clientGone, got, tc.want)
			}
		})
	}
}

// streamEpilogue 复刻 Responses/ChatCompletions 流式路径的收尾时序:
// 逐事件走 defer/拦截逻辑 → 仅在写过 body 时收尾 flush → 拦截命中时循环外 c.JSON。
// 用真实 HTTP 连接验证,因为致命点在 gin 层:flusher.Flush 会先 WriteHeaderNow
// 提交 200,零写入时提前 flush 会让后续 c.JSON(4xx) 的状态码永远无法送达下游。
func streamEpilogue(t *testing.T, c *gin.Context, events [][]byte) {
	t.Helper()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Error("c.Writer 未实现 http.Flusher")
		return
	}
	streamWriter := newStreamFlushWriter(c.Writer, flusher)
	var pending bytes.Buffer
	ttftRecorded := false
	wroteAnyBody := false
	abortedForHTTPError := false
	gotTerminal := false

	for _, data := range events {
		eventType := gjson.GetBytes(data, "type").String()
		if eventType == "response.output_text.delta" {
			ttftRecorded = true
		}
		if eventType == "response.completed" || eventType == "response.failed" {
			gotTerminal = true
		}
		if shouldReturnHTTPErrorForResponseFailed(eventType, ttftRecorded, wroteAnyBody, false) {
			pending.Reset()
			abortedForHTTPError = true
			break
		}
		shouldDefer := !ttftRecorded && !gotTerminal && isPreContentLifecycleEvent(eventType)
		wrote, err := writeDeferredSSEData(streamWriter, &pending, data, shouldDefer)
		if err != nil {
			t.Errorf("writeDeferredSSEData: %v", err)
			return
		}
		if wrote {
			wroteAnyBody = true
		}
	}
	if wroteAnyBody {
		_ = streamWriter.Flush()
	}
	if abortedForHTTPError && !wroteAnyBody {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "context_length_exceeded", "type": "upstream_error"},
		})
	}
}

func TestStreamFirstTokenFailureWireBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)

	failedEvent := []byte(`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"input too long"}}}`)
	cases := []struct {
		name         string
		events       [][]byte
		wantStatus   int
		wantJSONType bool
		wantBodyHas  string
	}{
		{
			name: "首 token 前 response.failed → 下游收到真实 4xx JSON",
			events: [][]byte{
				[]byte(`{"type":"response.created"}`),
				[]byte(`{"type":"response.in_progress"}`),
				failedEvent,
			},
			wantStatus:   http.StatusBadRequest,
			wantJSONType: true,
			wantBodyHas:  "context_length_exceeded",
		},
		{
			name: "已产出首 token → 维持 200 SSE 流式收尾",
			events: [][]byte{
				[]byte(`{"type":"response.created"}`),
				[]byte(`{"type":"response.output_text.delta","delta":"hi"}`),
				failedEvent,
			},
			wantStatus:   http.StatusOK,
			wantJSONType: false,
			wantBodyHas:  "response.output_text.delta",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/stream", func(c *gin.Context) {
				streamEpilogue(t, c, tc.events)
			})
			srv := httptest.NewServer(router)
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/stream")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%q)", resp.StatusCode, tc.wantStatus, body)
			}
			ct := resp.Header.Get("Content-Type")
			if tc.wantJSONType && !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if !tc.wantJSONType && !strings.Contains(ct, "text/event-stream") {
				t.Errorf("Content-Type = %q, want text/event-stream", ct)
			}
			if !strings.Contains(string(body), tc.wantBodyHas) {
				t.Errorf("body = %q, want contains %q", body, tc.wantBodyHas)
			}
		})
	}
}

// 入站 WS 路径的等价修复:首 token 前不可重试的 response.failed 不透传原始失败帧,
// 改为 error 帧 + 按错误类别的非正常 close code(与 SSE 路径返回真实 4xx 对齐)。
func TestResponsesWSCloseCodeForStatus(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{http.StatusTooManyRequests, websocket.CloseTryAgainLater},
		{http.StatusBadRequest, websocket.ClosePolicyViolation},
		{http.StatusUnauthorized, websocket.ClosePolicyViolation},
		{http.StatusForbidden, websocket.ClosePolicyViolation},
		{http.StatusPaymentRequired, websocket.ClosePolicyViolation},
		{http.StatusInternalServerError, websocket.CloseInternalServerErr},
		{http.StatusBadGateway, websocket.CloseInternalServerErr},
		{http.StatusOK, websocket.CloseInternalServerErr},
	}
	for _, tc := range cases {
		if got := responsesWSCloseCodeForStatus(tc.status); got != tc.want {
			t.Errorf("responsesWSCloseCodeForStatus(%d) = %d, want %d", tc.status, got, tc.want)
		}
	}
}

// 端到端:入站 WS 首 token 前收到不可重试的 response.failed(context_length_exceeded)
// 时,客户端应收到结构化 error 帧 + ClosePolicyViolation 关闭,而不是原始失败帧 +
// 正常收尾(后者会被下游中转/计费方当成一次成功会话)。且确定性客户端错误不换号重试。
func TestResponsesWebSocketNonRetryableFailureReturnsErrorClose(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = true
	nextSettings.CodexWSHideErrors = false
	nextSettings.CodexWSSilentRetries = 2
	ApplyRuntimeSettings(nextSettings)

	attemptCh := make(chan int64, 4)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attemptCh <- account.ID()
		sse := `data: {"type":"response.created"}` + "\n\n" +
			`data: {"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"input too long"}}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read error frame: %v", err)
	}
	if eventType := gjson.GetBytes(first, "type").String(); eventType != "error" {
		t.Fatalf("first event type = %q, want \"error\" (原始 response.failed 帧不应透传) body=%s", eventType, first)
	}
	if !strings.Contains(string(first), "input too long") {
		t.Fatalf("error frame should carry upstream message when hiding disabled: %s", first)
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("expected close %d (policy violation for deterministic 4xx), got err=%v", websocket.ClosePolicyViolation, err)
	}

	select {
	case <-attemptCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first attempt")
	}
	select {
	case got := <-attemptCh:
		t.Fatalf("unexpected retry on account %d for non-retryable failure", got)
	case <-time.After(100 * time.Millisecond):
	}
}
