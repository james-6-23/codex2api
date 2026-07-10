package wsrelay

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ==================== 心跳配置常量 ====================

const (
	// 心跳间隔：每 30 秒发送 Ping
	HeartbeatPingInterval = 30 * time.Second

	// 读超时：120 秒无响应则断开。
	// 心跳 30s 仍提供 4 个窗口容错；放宽到 120s 避免长推理 / priority 排队的
	// 活跃 turn（上游一段时间不下发增量帧）被误判为连接断开。
	ReadTimeout = 120 * time.Second

	// 写超时：30 秒
	WriteTimeout = 30 * time.Second

	// 空闲超时：5 分钟无活动则断开
	IdleTimeout = 5 * time.Minute

	// 连接最大寿命：上游 chatgpt backend 对每条 Responses WS 连接强制 60 分钟
	// 寿命上限，超限后该连接上的 response.create 一律返回
	// websocket_connection_limit_reached（且 Ping 探活仍成功，无法靠探活识别）。
	// 提前到 50 分钟在空闲时主动轮转销毁，避免活跃会话撞线（issue #346）。
	MaxConnLifetime = 50 * time.Minute

	// 握手超时：30 秒
	HandshakeTimeout = 30 * time.Second

	// Pending 请求超时：2 分钟
	PendingRequestTimeout = 2 * time.Minute

	// 复用同一 session 的连接时，等待其空闲的轮询退避参数。
	AcquireInitialBackoff = 10 * time.Millisecond  // 初始退避
	AcquireMaxBackoff     = 200 * time.Millisecond // 退避封顶
	AcquireMaxWait        = 30 * time.Second       // 最大累计等待，超时返回错误
)

// ==================== Pending 请求管理 ====================

// PendingRequest 等待响应的请求
type PendingRequest struct {
	// 请求 ID
	RequestID string

	// 会话 ID
	SessionID string

	// 创建时间
	CreatedAt time.Time

	// 响应通道
	ResponseChan chan *Message

	// 流式数据通道
	StreamChan chan *Message

	// 上下文（用于取消）
	Ctx context.Context

	// 取消函数
	Cancel context.CancelFunc

	// 关闭标志，防止重复关闭
	closed  bool
	closeMu sync.Mutex
}

// NewPendingRequest 创建新的等待请求
func NewPendingRequest(sessionID string) *PendingRequest {
	ctx, cancel := context.WithTimeout(context.Background(), PendingRequestTimeout)
	return &PendingRequest{
		RequestID:    uuid.New().String(),
		SessionID:    sessionID,
		CreatedAt:    time.Now(),
		ResponseChan: make(chan *Message, 1),
		StreamChan:   make(chan *Message, 64), // 流式数据缓冲
		Ctx:          ctx,
		Cancel:       cancel,
	}
}

// Close 关闭请求，释放资源（幂等）
func (pr *PendingRequest) Close() {
	pr.closeMu.Lock()
	defer pr.closeMu.Unlock()

	if pr.closed {
		return
	}
	pr.closed = true

	pr.Cancel()
	close(pr.ResponseChan)
	close(pr.StreamChan)
}

// ==================== 会话管理 ====================

// Session WebSocket 会话
type Session struct {
	// 会话 ID
	ID string

	// 账号 ID
	AccountID int64

	// 创建时间
	CreatedAt time.Time

	// 最后活跃时间
	LastActiveAt time.Time

	// 连接状态
	Connected bool

	// 读写锁保护内部状态
	mu sync.RWMutex

	// Pending 请求映射（requestID -> *PendingRequest）
	pending sync.Map

	// 心跳计时器
	heartbeatTimer *time.Timer

	// 连接关闭回调
	onClose func()

	// 会话管理器引用
	manager *Manager
}

// NewSession 创建新会话
func NewSession(accountID int64, manager *Manager) *Session {
	return &Session{
		ID:           uuid.New().String(),
		AccountID:    accountID,
		CreatedAt:    time.Now(),
		LastActiveAt: time.Now(),
		Connected:    false,
		manager:      manager,
	}
}

// Touch 更新最后活跃时间
func (s *Session) Touch() {
	s.mu.Lock()
	s.LastActiveAt = time.Now()
	s.mu.Unlock()
}

// IsExpired 检查会话是否过期（空闲超时）
func (s *Session) IsExpired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Since(s.LastActiveAt) > IdleTimeout
}

// SetConnected 设置连接状态
func (s *Session) SetConnected(connected bool) {
	s.mu.Lock()
	s.Connected = connected
	if connected {
		s.LastActiveAt = time.Now()
	}
	s.mu.Unlock()
}

// IsConnected 检查是否已连接
func (s *Session) IsConnected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Connected
}

// AddPendingRequest 添加等待请求
func (s *Session) AddPendingRequest(sessionID string) *PendingRequest {
	pr := NewPendingRequest(sessionID)
	s.pending.Store(pr.RequestID, pr)
	return pr
}

// GetPendingRequest 获取等待请求
func (s *Session) GetPendingRequest(requestID string) (*PendingRequest, bool) {
	if v, ok := s.pending.Load(requestID); ok {
		return v.(*PendingRequest), true
	}
	return nil, false
}

// RemovePendingRequest 移除等待请求
func (s *Session) RemovePendingRequest(requestID string) {
	if v, ok := s.pending.LoadAndDelete(requestID); ok {
		pr := v.(*PendingRequest)
		pr.Close()
	}
}

// PendingCount returns the number of in-flight requests bound to this session.
func (s *Session) PendingCount() int {
	if s == nil {
		return 0
	}
	count := 0
	s.pending.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

// DeliverResponse 投递响应到等待请求
func (s *Session) DeliverResponse(msg *Message) bool {
	if pr, ok := s.GetPendingRequest(msg.RequestID); ok {
		pr.closeMu.Lock()
		if pr.closed {
			pr.closeMu.Unlock()
			return false
		}
		select {
		case pr.ResponseChan <- msg:
			pr.closeMu.Unlock()
			return true
		default:
			// 通道已满或已关闭
			pr.closeMu.Unlock()
			return false
		}
	}
	return false
}

// DeliverStreamChunk 投递流式数据块
func (s *Session) DeliverStreamChunk(msg *Message) bool {
	if pr, ok := s.GetPendingRequest(msg.RequestID); ok {
		pr.closeMu.Lock()
		if pr.closed {
			pr.closeMu.Unlock()
			return false
		}
		select {
		case pr.StreamChan <- msg:
			pr.closeMu.Unlock()
			return true
		default:
			// 通道已满，丢弃当前流式数据块并返回 false
			log.Printf("DeliverStreamChunk stream channel full: account=%d session=%s requestID=%s capacity=%d length=%d; dropping current chunk", s.AccountID, s.ID, msg.RequestID, cap(pr.StreamChan), len(pr.StreamChan))
			pr.closeMu.Unlock()
			return false
		}
	}
	return false
}

// StartHeartbeat 启动心跳（防重入）
func (s *Session) StartHeartbeat(sendPing func() error) {
	s.mu.Lock()
	// 防重入：如果已有 timer 则直接返回
	if s.heartbeatTimer != nil {
		s.mu.Unlock()
		return
	}
	s.heartbeatTimer = time.AfterFunc(HeartbeatPingInterval, func() {
		s.mu.RLock()
		connected := s.Connected
		s.mu.RUnlock()

		if !connected {
			return
		}

		// 发送 Ping
		if err := sendPing(); err != nil {
			s.Close()
			return
		}

		// 检查 timer 是否仍存在（可能已被 StopHeartbeat 清除）
		s.mu.Lock()
		timer := s.heartbeatTimer
		s.mu.Unlock()

		// 安全重置计时器
		if timer != nil {
			timer.Reset(HeartbeatPingInterval)
		}
	})
	s.mu.Unlock()
}

// StopHeartbeat 停止心跳
func (s *Session) StopHeartbeat() {
	s.mu.Lock()
	if s.heartbeatTimer != nil {
		s.heartbeatTimer.Stop()
		s.heartbeatTimer = nil
	}
	s.mu.Unlock()
}

// HandlePong 处理 Pong 响应，重置读超时
func (s *Session) HandlePong() {
	s.Touch()
}

// Close 关闭会话
func (s *Session) Close() {
	s.StopHeartbeat()
	s.SetConnected(false)

	// 关闭所有等待请求
	s.pending.Range(func(key, value any) bool {
		pr := value.(*PendingRequest)
		pr.Close()
		s.pending.Delete(key)
		return true
	})

	// 调用关闭回调
	if s.onClose != nil {
		s.onClose()
	}
}

// SetOnClose 设置关闭回调
func (s *Session) SetOnClose(fn func()) {
	s.mu.Lock()
	s.onClose = fn
	s.mu.Unlock()
}

// ClearPendingRequests 清理所有等待请求
func (s *Session) ClearPendingRequests() {
	s.pending.Range(func(key, value any) bool {
		pr := value.(*PendingRequest)
		// 发送错误响应
		errMsg := NewErrorMessage(pr.RequestID, 503, "session closed")
		select {
		case pr.ResponseChan <- errMsg:
		default:
		}
		pr.Close()
		s.pending.Delete(key)
		return true
	})
}
