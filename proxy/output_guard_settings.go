// proxy/output_guard_settings.go
package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	OutputGuardConfigPollInterval = 5 * time.Second
	OutputGuardConfigReadTimeout  = 3 * time.Second
)

// OutputGuardAppliedConfig 是模型输出失控防护运行时配置的不可变值快照。
// 调用方持有副本后，后台轮询器应用新 generation 也不会篡改在途请求的视图。
type OutputGuardAppliedConfig struct {
	Enabled               bool
	MaxToolArgumentBytes  int64
	MaxToolArgumentDeltas int
	MaxChannelBytes       int64
	MaxRequestBytes       int64
	Generation            int64
}

type OutputGuardConfigSyncStatus struct {
	LastSuccessfulSyncAt time.Time
	LastSyncError        string
}

type OutputGuardSettingsReader interface {
	GetOutputGuardSettings(context.Context) (database.OutputGuardSettings, error)
}

type outputGuardState struct {
	mu          sync.RWMutex
	config      OutputGuardAppliedConfig
	lastSyncAt  time.Time
	lastSyncErr string
	isReady     atomic.Bool
}

var globalOutputGuardState = &outputGuardState{
	config: OutputGuardAppliedConfig{
		Enabled:               false,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
		Generation:            0,
	},
}

func IsOutputGuardReady() bool {
	return globalOutputGuardState.isReady.Load()
}

func CurrentOutputGuardConfig() OutputGuardAppliedConfig {
	globalOutputGuardState.mu.RLock()
	defer globalOutputGuardState.mu.RUnlock()
	return globalOutputGuardState.config
}

func GetOutputGuardConfigSyncStatus() OutputGuardConfigSyncStatus {
	globalOutputGuardState.mu.RLock()
	defer globalOutputGuardState.mu.RUnlock()
	return OutputGuardConfigSyncStatus{
		LastSuccessfulSyncAt: globalOutputGuardState.lastSyncAt,
		LastSyncError:        globalOutputGuardState.lastSyncErr,
	}
}

// ApplyOutputGuardSettings 校验并应用一份数据库快照。
// 非法配置或陈旧/重复 generation 均被拒绝，保留 last-known-good (LKG)。
func ApplyOutputGuardSettings(settings database.OutputGuardSettings) bool {
	if err := database.ValidateOutputGuardSettings(settings); err != nil {
		log.Printf("[output-guard] 非法配置已被拒绝，保留 LKG: %v", err)
		recordOutputGuardConfigSyncError(err)
		return false
	}

	globalOutputGuardState.mu.Lock()
	defer globalOutputGuardState.mu.Unlock()

	if settings.ConfigGeneration <= globalOutputGuardState.config.Generation {
		return false
	}

	enabledVal := false
	if settings.Enabled != nil {
		enabledVal = *settings.Enabled
	}

	globalOutputGuardState.config = OutputGuardAppliedConfig{
		Enabled:               enabledVal,
		MaxToolArgumentBytes:  settings.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: settings.MaxToolArgumentDeltas,
		MaxChannelBytes:       settings.MaxChannelBytes,
		MaxRequestBytes:       settings.MaxRequestBytes,
		Generation:            settings.ConfigGeneration,
	}
	globalOutputGuardState.isReady.Store(true)
	return true
}

// LoadOutputGuardSettings 执行冷启动同步读取。失败时保持未就绪，
// 由后台轮询器在数据库恢复后自愈，绝不阻断进程启动。
func LoadOutputGuardSettings(ctx context.Context, reader OutputGuardSettingsReader) error {
	settings, err := readOutputGuardSettings(ctx, reader, OutputGuardConfigReadTimeout)
	if err != nil {
		// 冷启动失败必须留下同步错误：outputGuardHTTPGateArmed 依据
		// LastSyncError 判断 readiness 已有结论。若此处因父 ctx 超时/取消而
		// 跳过记录，首次后台轮询（约 5s）之前 HTTP 入口会以 guard 关闭状态
		// 放行请求，冷启动 503 门禁形同虚设。
		recordOutputGuardConfigSyncError(err)
		return err
	}
	if err := database.ValidateOutputGuardSettings(settings); err != nil {
		recordOutputGuardConfigSyncError(err)
		return err
	}
	ApplyOutputGuardSettings(settings)
	recordOutputGuardConfigSyncSuccess(time.Now())
	return nil
}

func StartOutputGuardConfigPoller(parent context.Context, db *database.DB) bool {
	if db == nil {
		return false
	}
	if parent == nil {
		parent = context.Background()
	}
	return db.RunBackgroundTask(func(lifecycle context.Context) {
		taskCtx, cancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(parent, cancel)
		defer func() {
			stopParent()
			cancel()
		}()
		runOutputGuardConfigPoller(
			taskCtx,
			db,
			OutputGuardConfigPollInterval,
			OutputGuardConfigReadTimeout,
		)
	})
}

func runOutputGuardConfigPoller(
	ctx context.Context,
	reader OutputGuardSettingsReader,
	interval time.Duration,
	readTimeout time.Duration,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = OutputGuardConfigPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = pollOutputGuardConfigOnce(ctx, reader, readTimeout)
		}
	}
}

func pollOutputGuardConfigOnce(
	ctx context.Context,
	reader OutputGuardSettingsReader,
	readTimeout time.Duration,
) error {
	settings, err := readOutputGuardSettings(ctx, reader, readTimeout)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		recordOutputGuardConfigSyncError(err)
		return err
	}
	if err := database.ValidateOutputGuardSettings(settings); err != nil {
		recordOutputGuardConfigSyncError(err)
		return err
	}
	ApplyOutputGuardSettings(settings)
	recordOutputGuardConfigSyncSuccess(time.Now())
	return nil
}

func readOutputGuardSettings(
	parent context.Context,
	reader OutputGuardSettingsReader,
	timeout time.Duration,
) (database.OutputGuardSettings, error) {
	if reader == nil {
		return database.OutputGuardSettings{}, fmt.Errorf("output guard settings reader is nil")
	}
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		return database.OutputGuardSettings{}, err
	}
	if timeout <= 0 || timeout > OutputGuardConfigReadTimeout {
		timeout = OutputGuardConfigReadTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return reader.GetOutputGuardSettings(ctx)
}

func recordOutputGuardConfigSyncSuccess(at time.Time) {
	globalOutputGuardState.mu.Lock()
	globalOutputGuardState.lastSyncAt = at
	globalOutputGuardState.lastSyncErr = ""
	globalOutputGuardState.mu.Unlock()
}

func recordOutputGuardConfigSyncError(err error) {
	if err == nil {
		return
	}
	globalOutputGuardState.mu.Lock()
	globalOutputGuardState.lastSyncErr = err.Error()
	globalOutputGuardState.mu.Unlock()
}

// RequireOutputGuardReadyHTTP 在冷启动配置未加载成功时以 503 强阻断请求。
func RequireOutputGuardReadyHTTP(c *gin.Context) bool {
	if IsOutputGuardReady() {
		return true
	}
	apiErr := api.NewAPIError(
		api.ErrCodeServiceUnavailable,
		"Service configuration unavailable; failed to load output guard settings",
		api.ErrorTypeServer,
	)
	api.SendErrorWithStatus(c, apiErr, http.StatusServiceUnavailable)
	return false
}

// RequireOutputGuardReadyWS 在未就绪时先写协议 JSON 错误帧，
// 再以 1013 CloseTryAgainLater 控制帧关闭连接。
func RequireOutputGuardReadyWS(conn *websocket.Conn) bool {
	if IsOutputGuardReady() {
		return true
	}
	if conn == nil {
		return false
	}
	apiErr := api.NewAPIError(
		api.ErrCodeServiceUnavailable,
		"Service configuration unavailable; failed to load output guard settings",
		api.ErrorTypeServer,
	)
	_ = writeResponsesWSErrorWithin(conn, apiErr, 2*time.Second)
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "Service configuration unavailable"),
		time.Now().Add(time.Second),
	)
	_ = conn.Close()
	return false
}

// RequireOutputGuardReadyStream 在未就绪时向关联 stream 发送 503 错误帧。
func RequireOutputGuardReadyStream(conn *websocket.Conn, streamID string) bool {
	if IsOutputGuardReady() {
		return true
	}
	if conn == nil {
		return false
	}
	apiErr := api.NewAPIError(
		api.ErrCodeServiceUnavailable,
		"Service configuration unavailable; failed to load output guard settings",
		api.ErrorTypeServer,
	)
	_ = writeResponsesWSErrorForStream(conn, apiErr, streamID, 2*time.Second)
	return false
}
