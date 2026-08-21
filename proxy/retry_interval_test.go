package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func newRetryTestHandler(t *testing.T) (*Handler, *auth.Store) {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 2})
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	return handler, store
}

func TestWaitBeforeRetry(t *testing.T) {
	h, store := newRetryTestHandler(t)

	t.Run("间隔为 0 立即返回", func(t *testing.T) {
		store.SetRetryIntervalMS(0)
		start := time.Now()
		if !h.waitBeforeRetry(context.Background()) {
			t.Fatal("want true")
		}
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Fatalf("interval 0 should not wait, took %v", elapsed)
		}
	})

	t.Run("ctx 已取消返回 false", func(t *testing.T) {
		store.SetRetryIntervalMS(0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if h.waitBeforeRetry(ctx) {
			t.Fatal("want false for canceled ctx")
		}
	})

	t.Run("按配置间隔等待", func(t *testing.T) {
		store.SetRetryIntervalMS(60)
		start := time.Now()
		if !h.waitBeforeRetry(context.Background()) {
			t.Fatal("want true")
		}
		if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
			t.Fatalf("should wait ~60ms, took %v", elapsed)
		}
	})

	t.Run("等待中客户端断开返回 false", func(t *testing.T) {
		store.SetRetryIntervalMS(5000)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		if h.waitBeforeRetry(ctx) {
			t.Fatal("want false when canceled mid-wait")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("cancel should abort the wait promptly, took %v", elapsed)
		}
	})

	t.Run("unlimited Retry-After 等待可被客户端取消", func(t *testing.T) {
		store.SetRetryIntervalMS(0)
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("Retry-After", "60")
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		if h.waitBeforeRetryWithBudget(ctx, 1, -1, resp) {
			t.Fatal("want false when canceled during Retry-After wait")
		}
		elapsed := time.Since(start)
		if elapsed < 10*time.Millisecond {
			t.Fatalf("Retry-After was ignored, returned after %v", elapsed)
		}
		if elapsed > time.Second {
			t.Fatalf("cancel should abort Retry-After promptly, took %v", elapsed)
		}
	})

	t.Run("finite retry ignores upstream Retry-After", func(t *testing.T) {
		store.SetRetryIntervalMS(0)
		resp := &http.Response{Header: make(http.Header)}
		resp.Header.Set("Retry-After", "60")
		started := time.Now()
		if !h.waitBeforeRetryWithBudget(context.Background(), 1, 2, resp) {
			t.Fatal("finite retry was unexpectedly canceled")
		}
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			t.Fatalf("finite retry inherited upstream Retry-After, took %v", elapsed)
		}
	})
}

func TestParseRetryAfterHeaderAt(t *testing.T) {
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "delta seconds", value: "12", want: 12 * time.Second},
		{name: "HTTP date", value: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second},
		{name: "expired HTTP date", value: now.Add(-time.Second).Format(http.TimeFormat), want: 0},
		{name: "zero", value: "0", want: 0},
		{name: "negative", value: "-1", want: 0},
		{name: "invalid", value: "later", want: 0},
		{name: "overflow", value: "999999999999999999999999", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfterHeaderAt(tc.value, now); got != tc.want {
				t.Fatalf("parseRetryAfterHeaderAt(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestRetryableUpstreamStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusUpgradeRequired,
	} {
		if !isRetryableStatus(status) {
			t.Errorf("legacy status %d should be retryable", status)
		}
	}
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusUnprocessableEntity,
		http.StatusBadGateway,
		http.StatusGatewayTimeout,
	} {
		if isRetryableStatus(status) {
			t.Errorf("status %d should not be retried by the legacy classifier", status)
		}
	}
}

func TestRetryBudgetAvailable(t *testing.T) {
	cases := []struct {
		used, limit int
		want        bool
	}{
		{used: 0, limit: -1, want: true},
		{used: 100_000, limit: -1, want: true},
		{used: 0, limit: 0, want: false},
		{used: 0, limit: 1, want: true},
		{used: 1, limit: 1, want: false},
		{used: 0, limit: -2, want: false},
	}
	for _, tc := range cases {
		if got := retryBudgetAvailable(tc.used, tc.limit); got != tc.want {
			t.Errorf("retryBudgetAvailable(%d, %d) = %v, want %v", tc.used, tc.limit, got, tc.want)
		}
	}
}

func TestUnlimitedRetryBackoff(t *testing.T) {
	cases := []struct {
		ordinal int
		jitter  float64
		want    time.Duration
	}{
		{ordinal: 1, jitter: 0, want: 250 * time.Millisecond},
		{ordinal: 2, jitter: 0, want: 500 * time.Millisecond},
		{ordinal: 3, jitter: 0, want: time.Second},
		{ordinal: 1, jitter: 1, want: 375 * time.Millisecond},
		{ordinal: 1, jitter: -1, want: 250 * time.Millisecond},
		{ordinal: 100, jitter: 1, want: 30 * time.Second},
	}
	for _, tc := range cases {
		if got := unlimitedRetryBackoff(tc.ordinal, tc.jitter); got != tc.want {
			t.Errorf("unlimitedRetryBackoff(%d, %v) = %v, want %v", tc.ordinal, tc.jitter, got, tc.want)
		}
	}
}

func TestUnlimitedRetryWaitIsCancellable(t *testing.T) {
	h, store := newRetryTestHandler(t)
	store.SetRetryIntervalMS(0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if h.waitBeforeRetryWithBudget(ctx, 1, -1) {
		t.Fatal("unlimited retry wait returned true after context cancellation")
	}
	elapsed := time.Since(started)
	if elapsed < 10*time.Millisecond {
		t.Fatalf("unlimited retry backoff was skipped: %v", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("context did not interrupt unlimited retry backoff promptly: %v", elapsed)
	}
}

func TestFirstTokenTimeoutRetryWaitPolicy(t *testing.T) {
	h, store := newRetryTestHandler(t)
	store.SetRetryIntervalMS(5000)

	t.Run("finite retry preserves immediate shortcut", func(t *testing.T) {
		started := time.Now()
		if !h.waitBeforeRetryWithFirstTokenTimeout(context.Background(), true, 1, 2) {
			t.Fatal("finite first-token timeout retry was canceled")
		}
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			t.Fatalf("finite first-token timeout added a retry delay: %v", elapsed)
		}
	})

	t.Run("finite retry still honors cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if h.waitBeforeRetryWithFirstTokenTimeout(ctx, true, 1, 2) {
			t.Fatal("canceled finite first-token timeout retry continued")
		}
	})

	t.Run("unlimited retry uses cancellable backoff", func(t *testing.T) {
		store.SetRetryIntervalMS(0)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		if h.waitBeforeRetryWithFirstTokenTimeout(ctx, true, 1, -1) {
			t.Fatal("unlimited first-token timeout retry ignored cancellation")
		}
		if elapsed := time.Since(started); elapsed < 10*time.Millisecond {
			t.Fatalf("unlimited first-token timeout backoff was skipped: %v", elapsed)
		}
	})
}

func TestUnlimitedRetrySmallRetryAfterDoesNotBypassBackoff(t *testing.T) {
	h, store := newRetryTestHandler(t)
	store.SetRetryIntervalMS(0)
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("Retry-After", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
	defer cancel()

	if h.waitBeforeRetryWithBudget(ctx, 8, -1, resp) {
		t.Fatal("small Retry-After bypassed the larger unlimited retry backoff")
	}
}

func TestRetryAttemptProgress(t *testing.T) {
	if got := retryAttemptProgress(2, 4); got != "3/5" {
		t.Fatalf("finite retry progress = %q, want 3/5", got)
	}
	if got := retryAttemptProgress(2, -1); got != "3/unlimited" {
		t.Fatalf("unlimited retry progress = %q, want 3/unlimited", got)
	}
}

func TestRetrySettingsNormalization(t *testing.T) {
	_, store := newRetryTestHandler(t)

	store.SetRetryIntervalMS(-5)
	if got := store.GetRetryIntervalMS(); got != 0 {
		t.Errorf("negative interval → %d, want 0", got)
	}
	store.SetRetryIntervalMS(99999)
	if got := store.GetRetryIntervalMS(); got != 30000 {
		t.Errorf("oversized interval → %d, want 30000", got)
	}

	store.SetTransportRetryPolicy(" STICKY ")
	if got := store.GetTransportRetryPolicy(); got != "sticky" {
		t.Errorf("policy STICKY → %q, want sticky", got)
	}
	store.SetTransportRetryPolicy("whatever")
	if got := store.GetTransportRetryPolicy(); got != "rotate" {
		t.Errorf("unknown policy → %q, want rotate", got)
	}
}

// runWSTransportRetryScenario 驱动入站 WS:首次上游连接报传输错误,第二次成功,
// 返回两次尝试使用的账号 ID。用于验证 rotate/sticky 两种传输错误重试策略。
func runWSTransportRetryScenario(t *testing.T, policy string) (first, second int64) {
	t.Helper()
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

	var calls atomic.Int64
	attemptCh := make(chan int64, 4)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attemptCh <- account.ID()
		if calls.Add(1) == 1 {
			return nil, errors.New("read tcp 127.0.0.1:443: connection reset by peer")
		}
		sse := `data: {"type":"response.created"}` + "\n\n" +
			`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	handler, store := newRetryTestHandler(t)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	store.SetRetryIntervalMS(10)
	store.SetTransportRetryPolicy(policy)

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

	// 读到 response.completed 为止,确认第二次尝试的流被正常转发
	deadline := time.Now().Add(2 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		_, frame, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read stream frame: %v", err)
		}
		if gjson.GetBytes(frame, "type").String() == "response.completed" {
			break
		}
	}

	readAttempt := func() int64 {
		select {
		case id := <-attemptCh:
			return id
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for attempt")
			return 0
		}
	}
	return readAttempt(), readAttempt()
}

// 传输错误 + sticky 策略:同号重试(不换号、保留会话亲和)。issue #331
func TestResponsesWebSocketTransportRetrySticky(t *testing.T) {
	first, second := runWSTransportRetryScenario(t, "sticky")
	if first != second {
		t.Fatalf("sticky 策略应同号重试: first=%d second=%d", first, second)
	}
}

// 传输错误 + rotate 策略(默认):换号重试,保持旧行为。
func TestResponsesWebSocketTransportRetryRotate(t *testing.T) {
	first, second := runWSTransportRetryScenario(t, "rotate")
	if first == second {
		t.Fatalf("rotate 策略应换号重试: first=%d second=%d", first, second)
	}
}

func TestResponsesWebSocketUnlimitedRetryStopsAfterClientClose(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = false
	nextSettings.CodexWSHideErrors = false
	nextSettings.CodexWSSilentRetries = 0
	nextSettings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	ApplyRuntimeSettings(nextSettings)

	var calls atomic.Int64
	firstAttempt := make(chan struct{}, 1)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		if calls.Add(1) == 1 {
			firstAttempt <- struct{}{}
		}
		return nil, errors.New("connection reset by peer")
	}

	handler, store := newRetryTestHandler(t)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "pro", AccountID: "test-account"}
	store.AddAccount(account)
	store.SetRetryIntervalMS(0)
	store.SetTransportRetryPolicy("sticky")

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"hello"}`)); err != nil {
		_ = conn.Close()
		t.Fatalf("write request: %v", err)
	}

	select {
	case <-firstAttempt:
	case <-time.After(time.Second):
		_ = conn.Close()
		t.Fatal("timed out waiting for first upstream attempt")
	}
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&account.ActiveRequests) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("ActiveRequests after client close = %d, want 0", got)
	}
	time.Sleep(600 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream attempts continued after client close: got %d, want 1", got)
	}
}

func TestResponsesWebSocketInboundOverflowCancelsActiveTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = false
	nextSettings.CodexWSHideErrors = false
	nextSettings.CodexWSSilentRetries = 0
	nextSettings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	ApplyRuntimeSettings(nextSettings)

	var calls atomic.Int64
	firstAttempt := make(chan struct{}, 1)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		if calls.Add(1) == 1 {
			firstAttempt <- struct{}{}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	handler, store := newRetryTestHandler(t)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "pro", AccountID: "test-account"}
	store.AddAccount(account)
	store.SetRetryIntervalMS(0)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"first"}`)); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	select {
	case <-firstAttempt:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active upstream turn")
	}
	for i := 0; i <= responsesWSInboundQueueCapacity; i++ {
		err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"queued"}`))
		if err != nil {
			// The final write may race with the server closing immediately after
			// observing the overflowing frame. Earlier failures are unexpected.
			if i < responsesWSInboundQueueCapacity {
				t.Fatalf("write queued request %d: %v", i+1, err)
			}
			break
		}
	}

	deadline := time.Now().Add(upstreamDrainTimeout + 2*time.Second)
	for atomic.LoadInt64(&account.ActiveRequests) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("ActiveRequests after inbound overflow drain = %d, want 0", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream attempts continued after inbound overflow: got %d, want 1", got)
	}
}

func TestResponsesWSReadPumpAllowsRealtimeBurst(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverConnCh := make(chan *websocket.Conn, 1)
	upgradeErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			upgradeErrCh <- err
			return
		}
		serverConnCh <- conn
	}))
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}

	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverConnCh:
	case err := <-upgradeErrCh:
		_ = clientConn.Close()
		t.Fatalf("upgrade websocket failed: %v", err)
	case <-time.After(time.Second):
		_ = clientConn.Close()
		t.Fatal("timed out waiting for server websocket")
	}

	readCtx, messages, readPumpDone, cancel := startResponsesWSReadPump(context.Background(), serverConn)
	defer func() {
		cancel()
		_ = clientConn.Close()
		_ = serverConn.Close()
		select {
		case <-readPumpDone:
		case <-time.After(time.Second):
			t.Error("read pump did not stop")
		}
	}()

	frames := [][]byte{
		[]byte(`{"type":"session.update","session":{"model":"gpt-5.4"}}`),
		[]byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[]}}`),
		[]byte(`{"type":"response.create","response":{}}`),
	}
	for i, frame := range frames {
		if err := clientConn.WriteMessage(websocket.TextMessage, frame); err != nil {
			t.Fatalf("write burst frame %d: %v", i+1, err)
		}
	}

	deadline := time.Now().Add(time.Second)
	for len(messages) < len(frames) && readCtx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := readCtx.Err(); err != nil {
		t.Fatalf("normal Realtime burst canceled read pump: %v", err)
	}
	if got := len(messages); got != len(frames) {
		t.Fatalf("queued burst frames = %d, want %d", got, len(frames))
	}

	for i, want := range frames {
		select {
		case message := <-messages:
			message.releaseQueueBudget()
			if message.err != nil {
				t.Fatalf("burst frame %d returned read error: %v", i+1, message.err)
			}
			if message.messageType != websocket.TextMessage || string(message.payload) != string(want) {
				t.Fatalf("burst frame %d = type %d payload %q, want type %d payload %q", i+1, message.messageType, message.payload, websocket.TextMessage, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out reading burst frame %d", i+1)
		}
	}
}
