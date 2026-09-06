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

	headers := exec.prepareWebsocketHeaders("token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", cfg, ginHeaders, nil)

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
	// 握手会话头已改为真实客户端形态：build_websocket_headers 发 session-id /
	// thread-id / x-client-request-id，不发下划线写法，也没有 Conversation_id。
	if got := headers.Get("Session-Id"); got != "session-123" {
		t.Fatalf("Session-Id = %q", got)
	}
	if got := headers.Get("Thread-Id"); got != "session-123" {
		t.Fatalf("Thread-Id = %q, want 与 session 同值", got)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty", got)
	}
	if got := headers.Get("Session_id"); got != "" {
		t.Fatalf("Session_id = %q, want empty", got)
	}
}

// TestPrepareWebsocketHeadersForwardsAttestationOnlyWhenPresent 验证 WS 路径同样
// 只在下游携带 DeviceCheck token（openai/codex#20619）时透传，缺失不伪造。
func TestPrepareWebsocketHeadersForwardsAttestationOnlyWhenPresent(t *testing.T) {
	exec := NewExecutor()
	acc := &auth.Account{DBID: 42, AccountID: "42"}

	withToken := exec.prepareWebsocketHeaders("token-123", acc, "42", "session-123", "api-key-1", nil, http.Header{
		"X-Oai-Attestation": []string{"v1.real-devicecheck-token"},
	}, nil)
	if got := withToken.Get("X-Oai-Attestation"); got != "v1.real-devicecheck-token" {
		t.Fatalf("X-Oai-Attestation = %q, want passthrough of downstream token", got)
	}

	without := exec.prepareWebsocketHeaders("token-123", acc, "42", "session-123", "api-key-1", nil, http.Header{}, nil)
	if got := without.Get("X-Oai-Attestation"); got != "" {
		t.Fatalf("X-Oai-Attestation = %q, want empty (never fabricate)", got)
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

	headers := exec.prepareWebsocketHeaders("token-123", account, "42", "session-123", "api-key-1", nil, http.Header{}, nil)

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

func TestPrepareWebsocketHeadersSendsRoutingHintFromBody(t *testing.T) {
	exec := NewExecutor()
	account := &auth.Account{DBID: 42, AccountID: "42"}
	wsBody := []byte(`{"model":"gpt-5.6-codex","service_tier":"fast"}`)

	headers := exec.prepareWebsocketHeaders("token-123", account, "42", "session-123", "api-key-1", nil, http.Header{}, wsBody)
	if got := headers.Get("X-Codex-Routing-Hint"); got != "model=gpt-5.6-codex;tier=priority" {
		t.Fatalf("X-Codex-Routing-Hint = %q, want priority hint from final WS body", got)
	}

	// 无 body 时不发。
	headers = exec.prepareWebsocketHeaders("token-123", account, "42", "session-123", "api-key-1", nil, http.Header{}, []byte(`{"model":"gpt-5.6-sol","service_tier":"ultrafast"}`))
	if got := headers.Get("X-Codex-Routing-Hint"); got != "model=gpt-5.6-sol;tier=ultrafast" {
		t.Fatalf("Ultrafast routing hint = %q", got)
	}

	// 无 body 时不发。
	headers = exec.prepareWebsocketHeaders("token-123", account, "42", "session-123", "api-key-1", nil, http.Header{}, nil)
	if got := headers.Get("X-Codex-Routing-Hint"); got != "" {
		t.Fatalf("X-Codex-Routing-Hint = %q, want empty without body", got)
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

	headers := exec.prepareWebsocketHeaders("token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", nil, ginHeaders, nil)

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
	if got := headers.Get("Session-Id"); got != "session-123" {
		t.Fatalf("Session-Id = %q", got)
	}
	if got := headers.Get("Thread-Id"); got != "session-123" {
		t.Fatalf("Thread-Id = %q, want 与 session 同值", got)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty（真实握手头里没有这个头）", got)
	}
}

func TestPrepareWebsocketHeadersCanOptOutOfUserAgent(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "false")
	exec := NewExecutor()

	headers := exec.prepareWebsocketHeaders("token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "session-123", "api-key-1", nil, http.Header{}, nil)

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %q, want empty", got)
	}
	if values, ok := headers["User-Agent"]; !ok || len(values) != 1 || values[0] != "" {
		t.Fatalf("User-Agent header entry = %#v, want explicit empty value to suppress Go default", values)
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

	headers := exec.prepareWebsocketHeaders("token-123", account, "42", "session-123", "api-key-1", nil, ginHeaders, nil)

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

	headers := exec.prepareWebsocketHeaders("token-123", &auth.Account{DBID: 42, AccountID: "42"}, "42", "", "api-key-1", nil, http.Header{}, nil)

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

// TestPrepareWebsocketHeadersConvergesForwardedClientRequestID 是 HTTP 侧
// TestApplyCodexRequestHeadersConvergesForwardedClientRequestID 的 WS 对照：
// 握手头有自己的一份透传列表和组装顺序，同一组不变量必须独立锁定，
// 否则 issue #536 的泄漏会只在 WS 路径上复活。
func TestPrepareWebsocketHeadersConvergesForwardedClientRequestID(t *testing.T) {
	const (
		clientUUID    = "01a00e75-8856-7542-89bf-35812620690f"
		installUUID   = "341596ee-ab98-43f8-82e2-08ecdfb56db4"
		workspacePath = "/Users/kyx/code_project/codex2api"
		remoteURL     = "https://github.com/james-6-23/codex2api.git"
		commitHash    = "3cd12a685fe3ea23b84a9097fd4563927857ea21"
	)
	rawMetadata := `{"installation_id":"` + installUUID + `","session_id":"` + clientUUID +
		`","thread_id":"` + clientUUID + `","window_id":"` + clientUUID +
		`:0","request_kind":"turn","workspaces":{"` + workspacePath +
		`":{"associated_remote_urls":{"origin":"` + remoteURL + `"},"latest_git_commit_hash":"` + commitHash + `","has_changes":false}}}`

	// 复刻真实 codex-tui 的握手头集合（见 wss://chatgpt.com/backend-api/codex/responses）。
	ginHeaders := http.Header{}
	ginHeaders.Set("X-Codex-Turn-Metadata", rawMetadata)
	ginHeaders.Set("Session-Id", clientUUID)
	ginHeaders.Set("Thread-Id", clientUUID)
	ginHeaders.Set("X-Client-Request-Id", clientUUID)
	ginHeaders.Set("X-Codex-Window-Id", clientUUID+":0")
	ginHeaders.Set("Originator", "codex-tui")

	account := &auth.Account{DBID: 42, AccountID: "42", CodexFingerprintMode: auth.CodexFingerprintModeSession}
	exec := NewExecutor()
	headers := exec.prepareWebsocketHeaders("token-123", account, "42", "upstream-session-id", "api-key-1", nil, ginHeaders, nil)

	if got := headers.Get("X-Client-Request-Id"); got == clientUUID {
		t.Fatal("X-Client-Request-Id still carries the downstream thread id after convergence")
	} else if got == "" {
		t.Fatal("X-Client-Request-Id was dropped, want a converged value")
	}
	// 握手的会话键归调用方决定，收敛默认不得介入（对齐需显式开
	// CODEX_SESSION_HEADER_ALIGN_CONVERGED）。头名换成真实形态，取值语义不变。
	if got := headers.Get("Session-Id"); got != "upstream-session-id" {
		t.Fatalf("Session-Id = %q, want the caller value untouched", got)
	}
	// thread-id 必须与已收敛的 x-client-request-id 同值，否则两个头各说各话。
	if got, want := headers.Get("Thread-Id"), headers.Get("X-Client-Request-Id"); got != want {
		t.Fatalf("Thread-Id = %q, want 等于 X-Client-Request-Id %q", got, want)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty", got)
	}
	// 下游没发 installation 头，出站也不该凭空多出一个。
	if got := headers.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset", got)
	}

	var dump strings.Builder
	for name, values := range headers {
		for _, value := range values {
			dump.WriteString(name + ": " + value + "\n")
		}
	}
	for _, leaked := range []string{clientUUID, installUUID, workspacePath, remoteURL, "james-6-23", commitHash} {
		if strings.Contains(dump.String(), leaked) {
			t.Fatalf("original identifier %q survived the websocket handshake headers:\n%s", leaked, dump.String())
		}
	}
}
