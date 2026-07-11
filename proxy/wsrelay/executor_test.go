package wsrelay

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestPrepareWebsocketHeadersUsesConfiguredDefaultsAndBetaFeatures(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "true")
	exec := NewExecutor()
	cfg := &proxy.DeviceProfileConfig{
		UserAgent:              "codex_cli_rs/0.120.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464",
		PackageVersion:         "0.120.0",
		RuntimeVersion:         "0.120.0",
		OS:                     "MacOS",
		Arch:                   "arm64",
		StabilizeDeviceProfile: true,
		BetaFeatures:           "multi_agent",
	}
	ginHeaders := http.Header{
		"Originator": []string{"custom-originator"},
	}

	headers := exec.prepareWebsocketHeaders("token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", cfg, ginHeaders)

	if got := headers.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("OpenAI-Beta"); got != responsesWebsocketBetaHeader {
		t.Fatalf("OpenAI-Beta = %q", got)
	}
	if got := headers.Get("X-Codex-Beta-Features"); got != "multi_agent" {
		t.Fatalf("X-Codex-Beta-Features = %q", got)
	}
	if got := headers.Get("User-Agent"); got != cfg.UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := headers.Get("Version"); got != "0.120.0" {
		t.Fatalf("Version = %q", got)
	}
	if got := headers.Get("Originator"); got != proxy.Originator {
		t.Fatalf("Originator = %q", got)
	}
	if got := headers.Get("Chatgpt-Account-Id"); got != "42" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := headers.Get("Conversation_id"); got != "session-123" {
		t.Fatalf("Conversation_id = %q", got)
	}
	if got := headers.Get("Session_id"); got != "session-123" {
		t.Fatalf("Session_id = %q", got)
	}
}

func TestPrepareWebsocketHeadersAppliesAccountCustomHeadersLast(t *testing.T) {
	exec := NewExecutor()
	account := &auth.Account{
		DBID:      42,
		AccountID: "42",
		CustomHeaders: map[string]string{
			"Authorization":      "Bearer websocket-override",
			"Chatgpt-Account-Id": "acct-override",
			"X-Custom-Header":    "custom-value",
		},
	}

	headers := exec.prepareWebsocketHeaders("token-123", account, "42", "session-123", "api-key-1", nil, http.Header{})

	if got := headers.Get("Authorization"); got != "Bearer websocket-override" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("Chatgpt-Account-Id"); got != "acct-override" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := headers.Get("X-Custom-Header"); got != "custom-value" {
		t.Fatalf("X-Custom-Header = %q", got)
	}
}

func TestPrepareWebsocketHeadersSendsUserAgentByDefault(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "")
	exec := NewExecutor()
	ginHeaders := http.Header{
		"X-Codex-Turn-State":                    []string{"turn-state"},
		"X-Codex-Turn-Metadata":                 []string{"turn-metadata"},
		"X-Client-Request-Id":                   []string{"req-123"},
		"X-Responsesapi-Include-Timing-Metrics": []string{"true"},
	}

	headers := exec.prepareWebsocketHeaders("token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", nil, ginHeaders)

	if got := headers.Get("User-Agent"); got != proxy.MinimalCodexCLIUserAgentForHeaders() {
		t.Fatalf("User-Agent = %q, want %q", got, proxy.MinimalCodexCLIUserAgentForHeaders())
	}
	if got := headers.Get("Version"); got != proxy.LatestCodexCLIVersionForHeaders() {
		t.Fatalf("Version = %q, want %q", got, proxy.LatestCodexCLIVersionForHeaders())
	}
	if got := headers.Get("OpenAI-Beta"); got != responsesWebsocketBetaHeader {
		t.Fatalf("OpenAI-Beta = %q", got)
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id", "X-Responsesapi-Include-Timing-Metrics"} {
		if got := headers.Get(name); got != ginHeaders.Get(name) {
			t.Fatalf("%s = %q, want %q", name, got, ginHeaders.Get(name))
		}
	}
	if got := headers.Get("Session_id"); got != "session-123" {
		t.Fatalf("Session_id = %q", got)
	}
	if got := headers.Get("Conversation_id"); got != "session-123" {
		t.Fatalf("Conversation_id = %q", got)
	}
}

func TestPrepareWebsocketHeadersCanOptOutOfUserAgent(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "false")
	exec := NewExecutor()

	headers := exec.prepareWebsocketHeaders("token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", nil, http.Header{})

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %q, want empty", got)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
}

func TestPrepareWebsocketHeadersHonorsForcedGeneratedUserAgent(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "true")
	prev := proxy.CurrentRuntimeSettings()
	proxy.ApplyRuntimeSettings(proxy.RuntimeSettings{ClientCompatMode: proxy.ClientCompatModeForce})
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(prev) })
	exec := NewExecutor()
	account := &auth.Account{DBID: 43, AccountID: "42"}
	ginHeaders := http.Header{
		"User-Agent": []string{"codex_vscode/1.2.3"},
		"Originator": []string{"codex_vscode"},
		"Version":    []string{"1.2.3"},
	}

	headers := exec.prepareWebsocketHeaders("token-123", account, "42", "session-123", "api-key-1", nil, ginHeaders)

	got := headers.Get("User-Agent")
	if got == ginHeaders.Get("User-Agent") {
		t.Fatalf("User-Agent preserved client UA %q in forced mode", got)
	}
	if got != proxy.ProfileForAccount(account.DBID).UserAgent {
		t.Fatalf("User-Agent = %q, want real account profile %q", got, proxy.ProfileForAccount(account.DBID).UserAgent)
	}
	if !strings.HasPrefix(got, "codex-tui/") || !strings.Contains(got, " (") {
		t.Fatalf("User-Agent = %q, want generated full codex-tui profile", got)
	}
	if version := headers.Get("Version"); version != proxy.LatestCodexCLIVersionForHeaders() {
		t.Fatalf("Version = %q, want %q", version, proxy.LatestCodexCLIVersionForHeaders())
	}
	if originator := headers.Get("Originator"); originator != proxy.Originator {
		t.Fatalf("Originator = %q, want %q", originator, proxy.Originator)
	}
}

func TestPrepareWebsocketBodyPreservesPreviousResponseID(t *testing.T) {
	exec := NewExecutor()

	got := exec.prepareWebsocketBody([]byte(`{"model":"gpt-5.4","previous_response_id":"resp_123","input":[{"role":"user","content":"continue"}]}`), "session-123")

	if prev := gjson.GetBytes(got, "previous_response_id").String(); prev != "resp_123" {
		t.Fatalf("previous_response_id = %q, want resp_123; body=%s", prev, got)
	}
	if cacheKey := gjson.GetBytes(got, "prompt_cache_key").String(); cacheKey != "session-123" {
		t.Fatalf("prompt_cache_key = %q, want session-123; body=%s", cacheKey, got)
	}
	if typ := gjson.GetBytes(got, "type").String(); typ != "response.create" {
		t.Fatalf("type = %q, want response.create; body=%s", typ, got)
	}
	if !gjson.GetBytes(got, "stream").Bool() {
		t.Fatalf("stream should be true; body=%s", got)
	}
}

func TestPrepareWebsocketBodyKeepsCacheKeyForStatelessSession(t *testing.T) {
	exec := NewExecutor()

	got := exec.prepareWebsocketBody([]byte(`{"model":"gpt-5.4","prompt_cache_key":"deterministic-key","input":[]}`), "stateless-abc123")

	if cacheKey := gjson.GetBytes(got, "prompt_cache_key").String(); cacheKey != "deterministic-key" {
		t.Fatalf("prompt_cache_key = %q, want deterministic-key (stateless sessionID must not overwrite); body=%s", cacheKey, got)
	}
}

func TestPrepareWebsocketBodyStatelessSessionWithoutCacheKey(t *testing.T) {
	exec := NewExecutor()

	got := exec.prepareWebsocketBody([]byte(`{"model":"gpt-5.4","input":[]}`), "stateless-abc123")

	if cacheKey := gjson.GetBytes(got, "prompt_cache_key").String(); cacheKey != "" {
		t.Fatalf("prompt_cache_key = %q, want empty (stateless sessionID must not be injected); body=%s", cacheKey, got)
	}
}

func TestNormalizeWebsocketHandshakeResponse(t *testing.T) {
	t.Run("switching protocols is successful websocket handshake", func(t *testing.T) {
		statusCode, _, failed := normalizeWebsocketHandshakeResponse(&http.Response{
			StatusCode: http.StatusSwitchingProtocols,
		})
		if failed {
			t.Fatal("failed = true, want false")
		}
		if statusCode != http.StatusOK {
			t.Fatalf("statusCode = %d, want %d", statusCode, http.StatusOK)
		}
	})

	t.Run("http 2xx is normalized for downstream handler", func(t *testing.T) {
		statusCode, _, failed := normalizeWebsocketHandshakeResponse(&http.Response{
			StatusCode: http.StatusNoContent,
		})
		if failed {
			t.Fatal("failed = true, want false")
		}
		if statusCode != http.StatusOK {
			t.Fatalf("statusCode = %d, want %d", statusCode, http.StatusOK)
		}
	})

	t.Run("non success status remains a handshake failure", func(t *testing.T) {
		statusCode, _, failed := normalizeWebsocketHandshakeResponse(&http.Response{
			StatusCode: http.StatusUnauthorized,
		})
		if !failed {
			t.Fatal("failed = false, want true")
		}
		if statusCode != http.StatusUnauthorized {
			t.Fatalf("statusCode = %d, want %d", statusCode, http.StatusUnauthorized)
		}
	})
}

func TestWebsocketResponseToHTTPClosesBodyOnContextCancel(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		time.Sleep(5 * time.Second)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	session := NewSession(1, nil)
	pr := session.AddPendingRequest("session-1")
	wc := NewWsConnection(conn, session, wsURL)
	manager := NewManager()
	defer manager.Stop()
	wsResp := &WsResponse{
		conn:        wc,
		pendingReq:  pr,
		sessionID:   "session-1",
		manager:     manager,
		readErrChan: make(chan error, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	resp := websocketResponseToHTTP(ctx, wsResp, http.StatusOK, http.Header{})
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := resp.Body.Read(make([]byte, 1))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Body.Read returned nil error after context cancellation")
		}
		if err != context.Canceled && err != io.ErrClosedPipe && !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "closed") {
			t.Fatalf("Body.Read error = %v, want context cancellation or closed pipe", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Body.Read stayed blocked after context cancellation")
	}
}

func newClosedTestWebsocketConn(t *testing.T) *websocket.Conn {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	handshakeDone := make(chan struct{})
	go func() {
		defer close(handshakeDone)
		defer serverConn.Close()
		req, err := http.ReadRequest(bufio.NewReader(serverConn))
		if err != nil {
			return
		}
		acceptHash := sha1.Sum([]byte(req.Header.Get("Sec-Websocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(serverConn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(acceptHash[:]))
	}()

	wsURL, err := url.Parse("ws://example.test/responses")
	if err != nil {
		t.Fatalf("parse websocket URL: %v", err)
	}
	conn, _, err := websocket.NewClient(clientConn, wsURL, nil, 1024, 1024)
	if err != nil {
		t.Fatalf("create test websocket client: %v", err)
	}
	<-handshakeDone
	return conn
}

func TestExecuteRequestViaWebsocketSendFailureRemovesEffectiveProxyConnection(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)

	account := &auth.Account{
		DBID:        42,
		AccessToken: "token-123",
		ProxyURL:    "http://account-proxy.test:8080",
	}
	sessionID := "session-1"
	wsURL, err := buildWebsocketURL(proxy.CodexBaseURL + CodexWsEndpoint)
	if err != nil {
		t.Fatalf("buildWebsocketURL: %v", err)
	}
	effectiveProxy := effectiveProxyURL(account, "")
	key := manager.poolKey(account.ID(), wsURL, sessionID, effectiveProxy)
	session := NewSession(account.ID(), manager)
	session.SetConnected(true)
	conn := &WsConnection{
		conn:    newClosedTestWebsocketConn(t),
		session: session,
		URL:     wsURL,
		PoolKey: key,
	}
	conn.SetState(StateConnected)
	conn.Touch()
	manager.connections.Store(key, conn)
	manager.sessions.Store(key, session)
	manager.probeFunc = func(wc *WsConnection) bool { return true }

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	exec := NewExecutorWithManager(manager)
	_, err = exec.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"gpt-5.4","input":"hi"}`), sessionID, "", "", nil, http.Header{}, "")
	if err == nil {
		t.Fatal("expected final send failure")
	}
	if _, ok := manager.connections.Load(key); ok {
		t.Fatal("expected failed connection keyed by effective account proxy to be removed")
	}
	if _, ok := manager.sessions.Load(key); ok {
		t.Fatal("expected failed session keyed by effective account proxy to be removed")
	}
	if conn.IsConnected() {
		t.Fatal("expected failed connection to be closed")
	}
}

func TestSendRequestWritesResponseCreatePayloadDirectly(t *testing.T) {
	received := make(chan []byte, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read websocket message: %v", err)
			return
		}
		received <- payload
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	exec := NewExecutor()
	wc := NewWsConnection(conn, NewSession(1, nil), wsURL)
	body := []byte(`{"type":"response.create","model":"gpt-5.4","input":"hi","stream":true}`)
	if err := exec.sendRequest(wc, body, "request-1"); err != nil {
		t.Fatalf("sendRequest: %v", err)
	}

	got := <-received
	if string(got) != string(body) {
		t.Fatalf("sent payload = %s, want %s", got, body)
	}
	if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.create" {
		t.Fatalf("sent type = %q, want response.create; payload=%s", eventType, got)
	}
	if gjson.GetBytes(got, "request_id").Exists() {
		t.Fatalf("payload should not contain internal request_id wrapper: %s", got)
	}
}

// TestResolveHandshakeSessionID 验证握手头 Session_id/Conversation_id 的取值策略。
// 该头逐连接冻结、复用连接时永不更新，因此 stateless 复用连接绝不能携带任何
// 单个请求的身份，否则第一个请求的会话身份会泄漏给后续复用该连接的所有用户
// （跨用户上下文污染，"用户2串到用户1的上下文"）。
func TestResolveHandshakeSessionID(t *testing.T) {
	t.Run("explicit session keeps original behavior", func(t *testing.T) {
		got := resolveHandshakeSessionID("session-123", "route-key", []byte(`{"prompt_cache_key":"whatever"}`))
		if got != "session-123" {
			t.Fatalf("headerSessionID = %q, want session-123", got)
		}
	})

	t.Run("stateless isolated mode must not send any session identity", func(t *testing.T) {
		// 默认隔离模式：帧体是每请求随机 prompt_cache_key，poolRouteKey 非空。
		// 若把随机 key 冻结进握手头，复用连接的后续用户都会顶着第一个请求的身份。
		got := resolveHandshakeSessionID("stateless-abc", "route-key", []byte(`{"prompt_cache_key":"per-request-random-uuid"}`))
		if got != "" {
			t.Fatalf("headerSessionID = %q, want empty (no connection-level session identity)", got)
		}
	})

	t.Run("stateless per-api-key mode keeps deterministic cache key", func(t *testing.T) {
		got := resolveHandshakeSessionID("stateless-abc", "", []byte(`{"prompt_cache_key":"deterministic-key"}`))
		if got != "deterministic-key" {
			t.Fatalf("headerSessionID = %q, want deterministic-key", got)
		}
	})

	t.Run("stateless without cache key falls back to stateless id", func(t *testing.T) {
		got := resolveHandshakeSessionID("stateless-abc", "", []byte(`{}`))
		if got != "stateless-abc" {
			t.Fatalf("headerSessionID = %q, want stateless-abc", got)
		}
	})
}

// TestPrepareWebsocketHeadersOmitsSessionHeadersWhenEmpty 验证 headerSessionID 为空时
// 不发送 Session_id/Conversation_id 握手头（隔离模式的 stateless 复用连接）。
func TestPrepareWebsocketHeadersOmitsSessionHeadersWhenEmpty(t *testing.T) {
	exec := NewExecutor()

	headers := exec.prepareWebsocketHeaders("token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "", "api-key-1", nil, http.Header{})

	if got := headers.Get("Session_id"); got != "" {
		t.Fatalf("Session_id = %q, want unset", got)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want unset", got)
	}
}

func TestStatelessOneShotEnabled(t *testing.T) {
	t.Setenv("CODEX_WS_STATELESS_ONESHOT", "")
	if statelessOneShotEnabled() {
		t.Fatal("default must be false (slot reuse on)")
	}
	t.Setenv("CODEX_WS_STATELESS_ONESHOT", "1")
	if !statelessOneShotEnabled() {
		t.Fatal("CODEX_WS_STATELESS_ONESHOT=1 must disable slot reuse")
	}
}

func TestShouldRetryWebsocketSendError(t *testing.T) {
	messageTooBig := fmt.Errorf("send interrupted: %w", &websocket.CloseError{
		Code: websocket.CloseMessageTooBig,
		Text: "message too big",
	})
	if shouldRetryWebsocketSendError(messageTooBig) {
		t.Fatal("close 1009 must return to the handler for HTTP fallback without rebuilding WebSocket connections")
	}

	if !shouldRetryWebsocketSendError(errors.New("temporary write failure")) {
		t.Fatal("ordinary transport write failures should retain the bounded reconnect retry")
	}
	if !shouldRetryWebsocketSendError(fmt.Errorf("send interrupted: %w", &websocket.CloseError{
		Code: websocket.CloseInternalServerErr,
		Text: "temporary upstream failure",
	})) {
		t.Fatal("retryable peer closes other than 1009 should retain the bounded reconnect retry")
	}
	if shouldRetryWebsocketSendError(nil) {
		t.Fatal("nil is not a retryable send error")
	}
}
