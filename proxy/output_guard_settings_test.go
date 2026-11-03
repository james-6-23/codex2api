// proxy/output_guard_settings_test.go
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func ResetOutputGuardStateForTest() {
	globalOutputGuardState.mu.Lock()
	defer globalOutputGuardState.mu.Unlock()
	globalOutputGuardState.config = OutputGuardAppliedConfig{
		Enabled: false, MaxToolArgumentBytes: 262144, MaxToolArgumentDeltas: 4096,
		MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, Generation: 0,
	}
	globalOutputGuardState.lastSyncAt = time.Time{}
	globalOutputGuardState.lastSyncErr = ""
	globalOutputGuardState.isReady.Store(false)
}

func SetOutputGuardReadyForTest(ready bool) {
	globalOutputGuardState.isReady.Store(ready)
}

func PollOutputGuardConfigOnceForTest(ctx context.Context, db *database.DB) error {
	return pollOutputGuardConfigOnce(ctx, db, OutputGuardConfigReadTimeout)
}

type controlledOutputGuardSettingsReader struct {
	mu       sync.Mutex
	requests chan chan outputGuardSettingsReadResult
}

type outputGuardSettingsReadResult struct {
	settings database.OutputGuardSettings
	err      error
}

func newControlledOutputGuardSettingsReader() *controlledOutputGuardSettingsReader {
	return &controlledOutputGuardSettingsReader{requests: make(chan chan outputGuardSettingsReadResult, 16)}
}

func (r *controlledOutputGuardSettingsReader) GetOutputGuardSettings(ctx context.Context) (database.OutputGuardSettings, error) {
	respCh := make(chan outputGuardSettingsReadResult, 1)
	select {
	case <-ctx.Done():
		return database.OutputGuardSettings{}, ctx.Err()
	case r.requests <- respCh:
	}
	select {
	case <-ctx.Done():
		return database.OutputGuardSettings{}, ctx.Err()
	case res := <-respCh:
		return res.settings, res.err
	}
}

func (r *controlledOutputGuardSettingsReader) respond(t *testing.T, res outputGuardSettingsReadResult) {
	t.Helper()
	select {
	case respCh := <-r.requests:
		respCh <- res
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for GetOutputGuardSettings request")
	}
}

func waitForOutputGuardConfigGeneration(t *testing.T, targetGen int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if CurrentOutputGuardConfig().Generation == targetGen {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for generation %d", targetGen)
}

func waitForOutputGuardSyncError(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := GetOutputGuardConfigSyncStatus()
		if status.LastSyncError != "" && (substr == "" || strings.Contains(status.LastSyncError, substr)) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for sync error: %s", substr)
}

func TestOutputGuardColdStartNotReadyReturns503(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	if IsOutputGuardReady() {
		t.Fatal("expected cold start IsOutputGuardReady = false")
	}

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	if RequireOutputGuardReadyHTTP(c) {
		t.Fatal("expected RequireOutputGuardReadyHTTP to reject cold start")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503, got %d", w.Code)
	}

	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error failed: %v", err)
	}
	if errResp.Error.Code != string(api.ErrCodeServiceUnavailable) {
		t.Fatalf("expected error code %q, got %q", api.ErrCodeServiceUnavailable, errResp.Error.Code)
	}
}

func TestOutputGuardExplicitDisabledSemanticVsColdStart(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	if IsOutputGuardReady() {
		t.Fatal("expected cold start not ready")
	}

	disabled := false
	dbSettings := database.OutputGuardSettings{
		Enabled: &disabled, MaxToolArgumentBytes: 262144, MaxToolArgumentDeltas: 4096,
		MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, ConfigGeneration: 1,
	}

	if !ApplyOutputGuardSettings(dbSettings) {
		t.Fatal("ApplyOutputGuardSettings failed")
	}
	if !IsOutputGuardReady() {
		t.Fatal("expected IsOutputGuardReady = true after explicit disabled settings loaded")
	}

	cfg := CurrentOutputGuardConfig()
	if cfg.Enabled != false {
		t.Fatalf("expected applied config Enabled = false, got %v", cfg.Enabled)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	if !RequireOutputGuardReadyHTTP(c) {
		t.Fatal("expected pass when system is ready and explicitly disabled")
	}
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("did not expect 503 when explicitly disabled and ready")
	}
}

func TestOutputGuardConfigGenerationIncrementsAndLKGMaintained(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	enabled := true
	initial := database.OutputGuardSettings{
		Enabled: &enabled, MaxToolArgumentBytes: 262144, MaxToolArgumentDeltas: 4096,
		MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, ConfigGeneration: 1,
	}
	if !ApplyOutputGuardSettings(initial) {
		t.Fatal("failed to apply initial generation 1")
	}

	cfg1 := CurrentOutputGuardConfig()
	if cfg1.Generation != 1 || cfg1.MaxToolArgumentBytes != 262144 {
		t.Fatalf("initial mismatch: %+v", cfg1)
	}

	stale := initial
	stale.MaxToolArgumentBytes = 524288
	if ApplyOutputGuardSettings(stale) {
		t.Fatal("stale generation 1 should be rejected")
	}
	if CurrentOutputGuardConfig().MaxToolArgumentBytes != 262144 {
		t.Fatal("LKG overwritten by stale generation")
	}

	gen2 := initial
	gen2.MaxToolArgumentBytes = 524288
	gen2.ConfigGeneration = 2
	if !ApplyOutputGuardSettings(gen2) {
		t.Fatal("failed to apply generation 2")
	}
	cfg2 := CurrentOutputGuardConfig()
	if cfg2.Generation != 2 || cfg2.MaxToolArgumentBytes != 524288 {
		t.Fatalf("gen 2 mismatch: %+v", cfg2)
	}

	illegal := initial
	illegal.MaxToolArgumentBytes = 50000000
	illegal.ConfigGeneration = 3
	if ApplyOutputGuardSettings(illegal) {
		t.Fatal("illegal configuration should be rejected")
	}
	cfgPreserved := CurrentOutputGuardConfig()
	if cfgPreserved.Generation != 2 || cfgPreserved.MaxToolArgumentBytes != 524288 {
		t.Fatalf("LKG not preserved after illegal config: %+v", cfgPreserved)
	}
}

func TestOutputGuardPollerLifecycleRecoveryAndStop(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	reader := newControlledOutputGuardSettingsReader()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		runOutputGuardConfigPoller(ctx, reader, 5*time.Millisecond, 100*time.Millisecond)
	}()

	enabled := true
	reader.respond(t, outputGuardSettingsReadResult{
		settings: database.OutputGuardSettings{
			Enabled: &enabled, MaxToolArgumentBytes: 524288, MaxToolArgumentDeltas: 4096,
			MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, ConfigGeneration: 2,
		},
	})
	waitForOutputGuardConfigGeneration(t, 2)
	if !IsOutputGuardReady() {
		t.Fatal("expected IsOutputGuardReady = true after successful poll")
	}

	reader.respond(t, outputGuardSettingsReadResult{err: errors.New("simulated database disconnect")})
	waitForOutputGuardSyncError(t, "simulated database disconnect")
	if CurrentOutputGuardConfig().Generation != 2 {
		t.Fatal("LKG generation lost during DB outage")
	}

	reader.respond(t, outputGuardSettingsReadResult{
		settings: database.OutputGuardSettings{
			Enabled: &enabled, MaxToolArgumentBytes: 524288, MaxToolArgumentDeltas: 4096,
			MaxChannelBytes: 2097152, MaxRequestBytes: 4194304, ConfigGeneration: 3,
		},
	})
	waitForOutputGuardConfigGeneration(t, 3)
	if CurrentOutputGuardConfig().MaxToolArgumentBytes != 524288 {
		t.Fatal("failed to update config after recovery")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("poller failed to stop cleanly after context cancel")
	}
}

func TestOutputGuardWSReadinessGate(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if !RequireOutputGuardReadyWS(conn) {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ready"))
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial WS failed: %v", err)
	}
	defer conn.Close()

	msgType, msg, err := conn.ReadMessage()
	if err == nil {
		if msgType != websocket.TextMessage {
			t.Fatalf("expected text message error frame, got %d", msgType)
		}
		var errResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(msg, &errResp); err != nil {
			t.Fatalf("unmarshal error frame failed: %v", err)
		}
		if errResp.Error.Code != string(api.ErrCodeServiceUnavailable) {
			t.Fatalf("expected error frame %s, got %s", api.ErrCodeServiceUnavailable, errResp.Error.Code)
		}
		_, _, err = conn.ReadMessage()
	}

	if err == nil {
		t.Fatal("expected connection closed with 1013 CloseTryAgainLater, got nil error")
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code != websocket.CloseTryAgainLater {
			t.Fatalf("expected 1013 CloseTryAgainLater, got %d", closeErr.Code)
		}
	} else {
		t.Fatalf("expected websocket.CloseError, got %v", err)
	}
}

func TestOutputGuardStreamReadinessGate(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = RequireOutputGuardReadyStream(conn, "stream-test-123")
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial WS failed: %v", err)
	}
	defer conn.Close()

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read stream error frame: %v", err)
	}

	var streamErrResp struct {
		Type     string        `json:"type"`
		StreamID string        `json:"stream_id"`
		Status   int           `json:"status"`
		Error    *api.APIError `json:"error"`
	}
	if err := json.Unmarshal(msg, &streamErrResp); err != nil {
		t.Fatalf("unmarshal stream error failed: %v", err)
	}
	if streamErrResp.Type != "error" || streamErrResp.StreamID != "stream-test-123" {
		t.Fatalf("unexpected stream envelope: %+v", streamErrResp)
	}
	if streamErrResp.Error == nil || streamErrResp.Error.Code != api.ErrCodeServiceUnavailable {
		t.Fatalf("expected service_unavailable, got %+v", streamErrResp.Error)
	}
}

// blockingOutputGuardSettingsReader 模拟数据库读挂起，直到 ctx 超时/取消。
type blockingOutputGuardSettingsReader struct{}

func (blockingOutputGuardSettingsReader) GetOutputGuardSettings(ctx context.Context) (database.OutputGuardSettings, error) {
	<-ctx.Done()
	return database.OutputGuardSettings{}, ctx.Err()
}

// TestLoadOutputGuardSettingsFailureRecordsSyncError 验证冷启动加载失败（父 ctx
// 超时/取消）同样记录 LastSyncError，使 outputGuardHTTPGateArmed 在首次后台
// 轮询前保持门禁并按 503 拦截。修复前父 ctx 超时路径会跳过记录，冷启动到首次
// 轮询（约 5s）期间 HTTP 请求会以 guard 关闭状态放行。
func TestLoadOutputGuardSettingsFailureRecordsSyncError(t *testing.T) {
	cases := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"parent deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 20*time.Millisecond)
		}},
		{"parent canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, func() {}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetOutputGuardStateForTest()
			t.Cleanup(ResetOutputGuardStateForTest)

			ctx, cancel := tc.ctx()
			defer cancel()

			if err := LoadOutputGuardSettings(ctx, blockingOutputGuardSettingsReader{}); err == nil {
				t.Fatal("expected cold-start load to fail")
			}
			if IsOutputGuardReady() {
				t.Fatal("failed cold-start load must not mark output guard ready")
			}
			if status := GetOutputGuardConfigSyncStatus(); status.LastSyncError == "" {
				t.Fatal("failed cold-start load must record LastSyncError so the readiness gate stays armed")
			}
			if !outputGuardHTTPGateArmed() {
				t.Fatal("gate must be armed after failed cold-start load")
			}

			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if RequireOutputGuardReadyHTTP(c) {
				t.Fatal("expected HTTP request to be blocked with 503 during cold start")
			}
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected HTTP 503, got %d", w.Code)
			}
		})
	}
}
