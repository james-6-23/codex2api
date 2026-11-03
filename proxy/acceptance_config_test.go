// proxy/acceptance_config_test.go
package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// 场景 22: 5 秒后台 Revision 轮询、在途快照隔离与冷启动 503/1013 强阻断、LKG 保留
func TestAcceptanceScenario22_RuntimePollerLKGAndColdStart503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ResetOutputGuardStateForTest()

	// 1. 冷启动未就绪拦截：HTTP 返回 503 service_unavailable
	t.Run("ColdStart503Block", func(t *testing.T) {
		SetOutputGuardReadyForTest(false)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

		if RequireOutputGuardReadyHTTP(c) {
			t.Fatalf("expected cold start HTTP request to be blocked, but allowed")
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected HTTP 503, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), string(api.ErrCodeServiceUnavailable)) {
			t.Fatalf("expected error body to contain %q, got %s", api.ErrCodeServiceUnavailable, w.Body.String())
		}
	})

	// 2. 冷启动 WebSocket 未就绪：先发送协议 error 帧，随后发送 1013 Close 帧
	t.Run("ColdStartWebSocket1013Block", func(t *testing.T) {
		SetOutputGuardReadyForTest(false)
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = RequireOutputGuardReadyWS(conn)
		}))
		defer s.Close()

		wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
		clientConn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("websocket dial failed: %v", err)
		}
		defer clientConn.Close()
		if resp != nil {
			_ = resp.Body.Close()
		}

		// 必须先读取到 JSON error frame
		msgType, msg, err := clientConn.ReadMessage()
		if err != nil {
			t.Fatalf("expected error frame before close, got err: %v", err)
		}
		if msgType != websocket.TextMessage || !strings.Contains(string(msg), string(api.ErrCodeServiceUnavailable)) {
			t.Fatalf("expected service_unavailable error frame, got: %s", string(msg))
		}

		// 随后必须收到 1013 CloseTryAgainLater 控制帧
		_, _, err = clientConn.ReadMessage()
		if err == nil {
			t.Fatalf("expected close error 1013, got nil")
		}
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseTryAgainLater {
			t.Fatalf("expected CloseTryAgainLater (1013), got %v", err)
		}
	})

	// 3. 在途旧快照隔离与两次 Apply 检验旧 value copy，以及非法配置被拒绝保留旧 snapshot / Ready
	t.Run("InFlightSnapshotIsolationAndValueCopy", func(t *testing.T) {
		ResetOutputGuardStateForTest()
		enabledVal := true
		initial := database.OutputGuardSettings{
			Enabled:               &enabledVal,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      1,
		}
		if !ApplyOutputGuardSettings(initial) {
			t.Fatalf("initial ApplyOutputGuardSettings failed")
		}

		// 在途请求在 turn 开始时获取快照
		inFlightSnapshot := CurrentOutputGuardConfig()
		if inFlightSnapshot.Generation != 1 || inFlightSnapshot.MaxToolArgumentBytes != 262144 {
			t.Fatalf("unexpected in-flight snapshot: %+v", inFlightSnapshot)
		}

		// 系统后台应用新配置 (Generation 2)
		gen2Bytes := int64(524288)
		gen2 := database.OutputGuardSettings{
			Enabled:               &enabledVal,
			MaxToolArgumentBytes:  gen2Bytes,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      2,
		}
		if !ApplyOutputGuardSettings(gen2) {
			t.Fatalf("second ApplyOutputGuardSettings failed")
		}

		// 断言在途请求持有的旧快照是不可变 value copy，绝不被新应用配置篡改
		if inFlightSnapshot.Generation != 1 {
			t.Fatalf("in-flight snapshot generation was mutated! want 1, got %d", inFlightSnapshot.Generation)
		}
		if inFlightSnapshot.MaxToolArgumentBytes != 262144 {
			t.Fatalf("in-flight snapshot bytes was mutated! want 262144, got %d", inFlightSnapshot.MaxToolArgumentBytes)
		}

		// 新请求获取到的则是最新快照
		newTurnSnapshot := CurrentOutputGuardConfig()
		if newTurnSnapshot.Generation != 2 || newTurnSnapshot.MaxToolArgumentBytes != 524288 {
			t.Fatalf("new turn snapshot want gen 2 / 524288 bytes, got %+v", newTurnSnapshot)
		}

		// 场景 21/22 补强: runtime 应用非法值（例如 tool > channel，或超出上限范围）被拒绝，且严格保留旧 snapshot 与 Ready 状态
		illegalSettings := database.OutputGuardSettings{
			Enabled:               &enabledVal,
			MaxToolArgumentBytes:  50000000, // 超过 4MiB 上限且大于 channel
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      3,
		}
		if ApplyOutputGuardSettings(illegalSettings) {
			t.Fatalf("expected ApplyOutputGuardSettings to reject illegal settings")
		}
		afterIllegalSnapshot := CurrentOutputGuardConfig()
		if afterIllegalSnapshot.Generation != 2 || afterIllegalSnapshot.MaxToolArgumentBytes != 524288 {
			t.Fatalf("LKG snapshot violated after applying illegal settings: got %+v, want gen 2 / 524288 bytes", afterIllegalSnapshot)
		}
		if !IsOutputGuardReady() {
			t.Fatalf("expected IsOutputGuardReady to remain true after illegal settings rejected")
		}
	})

	// 4. 5 秒后台轮询器、同步刷新与 DB 故障保留 LKG
	t.Run("PollerLKGAndFailurePreservation", func(t *testing.T) {
		ResetOutputGuardStateForTest()
		db, err := database.New("sqlite", filepath.Join(t.TempDir(), "poller_test.db"))
		if err != nil {
			t.Fatal(err)
		}
		// database.DB.Close 不是幂等操作（内部 close(db.logStop) 二次调用会 panic），
		// 因此用标志位保证 cleanup 只在故障注入未提前关闭时兜底关闭一次。
		dbClosed := false
		t.Cleanup(func() {
			if !dbClosed {
				_ = db.Close()
			}
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		started := StartOutputGuardConfigPoller(ctx, db)
		if !started {
			t.Fatalf("failed to start poller")
		}

		// 触发首次 poll 确保就绪
		_ = PollOutputGuardConfigOnceForTest(ctx, db)
		if !IsOutputGuardReady() {
			t.Fatalf("expected output guard to be ready after poller initial load")
		}

		// 记录当前 LKG 配置
		lkgBefore := CurrentOutputGuardConfig()

		// 更新 DB 配置
		newBytes := int64(524288)
		_, err = db.UpdateOutputGuardSettings(ctx, database.OutputGuardSettingsUpdate{
			MaxToolArgumentBytes: &newBytes,
		})
		if err != nil {
			t.Fatal(err)
		}

		// 提供单次注入 tick poll 接口快速同步，同时支持带超时的 generation 轮询同步，杜绝无断言 sleep
		_ = PollOutputGuardConfigOnceForTest(ctx, db)

		timeout := time.Now().Add(2 * time.Second)
		updated := false
		for time.Now().Before(timeout) {
			cfg := CurrentOutputGuardConfig()
			if cfg.MaxToolArgumentBytes == newBytes && cfg.Generation > lkgBefore.Generation {
				updated = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !updated {
			t.Fatalf("timed out waiting for poller generation update; current=%+v", CurrentOutputGuardConfig())
		}

		lkgCurrent := CurrentOutputGuardConfig()

		// 模拟 DB 发生严重故障（如直接关闭连接）
		if err := db.Close(); err != nil {
			t.Fatalf("close db for fault injection: %v", err)
		}
		dbClosed = true

		// 执行单次 poll 触发读取失败
		err = PollOutputGuardConfigOnceForTest(ctx, db)
		if err == nil {
			t.Fatalf("expected poll on closed DB to fail")
		}

		// 断言 DB 故障后保留 last-known-good 配置，不崩溃、不清空、不降为零值
		preserved := CurrentOutputGuardConfig()
		if preserved.Generation != lkgCurrent.Generation || preserved.MaxToolArgumentBytes != lkgCurrent.MaxToolArgumentBytes {
			t.Fatalf("LKG violated after DB failure: want %+v, got %+v", lkgCurrent, preserved)
		}
		if !IsOutputGuardReady() {
			t.Fatalf("output guard must remain ready on LKG after DB fault")
		}
	})
}

// 场景 23: 显式 Guard 关闭时，传输 Framing 依然对超大行/多行 Event 进行硬上限拦截，且保持现有 WS 48MB 容量
func TestAcceptanceScenario23_FramingGuardOffStillBounded(t *testing.T) {
	// 显式关闭语义 Guard 开关：配置中 Enabled=false，但传输层 Framing 硬上限必须独立生效。
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	disabledVal := false
	if !ApplyOutputGuardSettings(database.OutputGuardSettings{
		Enabled:               &disabledVal,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
		ConfigGeneration:      1,
	}) {
		t.Fatalf("failed to apply explicitly disabled output guard settings")
	}
	if CurrentOutputGuardConfig().Enabled {
		t.Fatalf("expected output guard Enabled=false for framing independence check")
	}

	// 1. 关闭语义 Guard 开关，SSE 超大无换行单行 (8 MiB) 依然被拦截
	t.Run("SSELineCapRemainsActiveWithGuardOff", func(t *testing.T) {
		oversizedLine := strings.Repeat("a", MaxSSELineBytes+1024)
		err := ReadSSEStreamWithEvent(strings.NewReader(oversizedLine), func(event string, data []byte) bool {
			return true
		})
		if err == nil || !errors.Is(err, ErrSSELineTooLong) {
			t.Fatalf("expected ErrSSELineTooLong regardless of guard switch, got: %v", err)
		}
	})

	// 2. 多行 Event 累积超 8 MiB 依然被拦截
	t.Run("SSEEventCapRemainsActiveWithGuardOff", func(t *testing.T) {
		chunk := "data: " + strings.Repeat("b", 1<<20) + "\n"
		oversizedEvent := strings.Repeat(chunk, 9) + "\n"
		err := ReadSSEStreamWithEvent(strings.NewReader(oversizedEvent), func(event string, data []byte) bool {
			return true
		})
		if err == nil || !errors.Is(err, ErrSSEEventTooLarge) {
			t.Fatalf("expected ErrSSEEventTooLarge regardless of guard switch, got: %v", err)
		}
	})

	// 3. 非流式超大 Response Body (16 MiB) 缺失路径有界拦截
	t.Run("ResponseBodyCapActive", func(t *testing.T) {
		oversizedBody := strings.Repeat("c", 1024)
		_, err := ReadBoundedResponseBody(strings.NewReader(oversizedBody), 512)
		if err == nil || !errors.Is(err, ErrResponseBodyTooLarge) {
			t.Fatalf("expected ErrResponseBodyTooLarge, got: %v", err)
		}
	})

	// 4. 验证 WebSocket 容量依然为 48MB (security.MaxRequestBodySize)，未被误收紧为 8MB
	t.Run("WSWireBudgetRemains48MB", func(t *testing.T) {
		if security.MaxRequestBodySize != 48*1024*1024 {
			t.Fatalf("WebSocket wire limit must remain 48MB (%d), got %d", 48*1024*1024, security.MaxRequestBodySize)
		}
	})
}
