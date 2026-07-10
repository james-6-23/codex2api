package wsrelay

import (
	"context"
	"log"
	"sync"
	"time"
)

// ==================== 上游 WS 空闲连接保活（prewarm 保活） ====================
//
// 设计原则（默认关闭，零额外账号消耗）：
//   - 只对【已经存在】的空闲上游 WS 连接发送 Ping 控制帧保活；
//   - 不主动建立新连接、不发送任何 response.create 业务帧，因此不消耗账号额度；
//   - 目的：避免空闲连接被上游/中间网络断开，下次复用时省去 TLS+握手冷启动，
//     更接近官方 Codex CLI 的“长连接 + 低首字延迟”体验。
//
// 当保活开关关闭时，KeepaliveTask 根本不会启动 goroutine，对现有系统零影响。

// PingIdleConnections 对连接池中所有【已连接且空闲】的 WS 连接发送一次 Ping 保活。
// 空闲 = 该连接的 session 当前没有 pending 请求（PendingCount() == 0），
// 避免与正在进行的请求争用写锁或干扰其读写时序。
// 返回 (pinged, failed)：成功 Ping 数 与 失败数（失败的连接会被 SendHeartbeat 关闭并移除）。
func (m *Manager) PingIdleConnections() (pinged int, failed int) {
	if m == nil {
		return 0, 0
	}
	// 实际发送 Ping 的函数，可被测试替换（keepalivePingFunc 为 nil 时用默认 SendHeartbeat）。
	ping := m.keepalivePingFunc
	if ping == nil {
		ping = m.SendHeartbeat
	}
	// 先收集快照，避免在 Range 回调里直接做可能修改 map 的操作（SendHeartbeat 失败会 Delete）。
	var idle []*WsConnection
	m.connections.Range(func(_, value any) bool {
		wc, ok := value.(*WsConnection)
		if !ok || wc == nil {
			return true
		}
		if !wc.IsConnected() {
			return true
		}
		// 到龄连接不续命：Pong 会刷新 lastUsed 让连接永不空闲过期，最终撞上游
		// 60 分钟寿命上限；跳过 Ping，交给 evictExpired 按到龄轮转销毁。
		if wc.IsOverAge() {
			return true
		}
		// 只保活空闲连接：有 pending 请求时跳过（该连接本就活跃，无需额外 Ping）。
		if wc.session != nil && wc.session.PendingCount() > 0 {
			return true
		}
		idle = append(idle, wc)
		return true
	})

	for _, wc := range idle {
		if err := ping(wc); err != nil {
			failed++
			continue
		}
		pinged++
	}
	return pinged, failed
}

// KeepaliveTask 周期性对空闲上游 WS 连接发送 Ping 保活的常驻任务。
// 仅在系统设置 codex_ws_keepalive_enabled 开启时由 main 启动。
type KeepaliveTask struct {
	manager     *Manager
	intervalSec func() int // 动态读取间隔（秒），支持运行时热更新
	enabled     func() bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewKeepaliveTask 创建保活任务。
//   - manager: 目标连接池（通常为 GetManager()）
//   - enabled: 返回当前是否启用保活（读 store 开关）
//   - intervalSec: 返回当前 Ping 间隔秒数（读 store 配置）
func NewKeepaliveTask(manager *Manager, enabled func() bool, intervalSec func() int) *KeepaliveTask {
	ctx, cancel := context.WithCancel(context.Background())
	return &KeepaliveTask{
		manager:     manager,
		enabled:     enabled,
		intervalSec: intervalSec,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Start 启动常驻 goroutine。即使保活当前关闭也可安全启动：
// goroutine 每个 tick 会检查 enabled()，关闭时仅空转、不发送任何帧。
func (t *KeepaliveTask) Start() {
	if t == nil || t.manager == nil {
		return
	}
	t.wg.Add(1)
	go t.loop()
	log.Printf("[WS-Keepalive] 保活常驻任务已启动（实际是否发送 Ping 取决于运行时开关）")
}

func (t *KeepaliveTask) loop() {
	defer t.wg.Done()
	// tick 粒度固定为较小值，循环内根据配置的 intervalSec 决定是否真正执行，
	// 这样间隔配置热更新无需重启 ticker。
	const tickGranularity = 5 * time.Second
	ticker := time.NewTicker(tickGranularity)
	defer ticker.Stop()

	var lastRun time.Time
	for {
		select {
		case <-t.ctx.Done():
			return
		case now := <-ticker.C:
			if t.enabled == nil || !t.enabled() {
				continue
			}
			interval := time.Duration(t.intervalSecOrDefault()) * time.Second
			if !lastRun.IsZero() && now.Sub(lastRun) < interval {
				continue
			}
			lastRun = now
			pinged, failed := t.manager.PingIdleConnections()
			if pinged > 0 || failed > 0 {
				log.Printf("[WS-Keepalive] 保活完成: pinged=%d failed=%d", pinged, failed)
			}
		}
	}
}

func (t *KeepaliveTask) intervalSecOrDefault() int {
	if t.intervalSec == nil {
		return 60
	}
	sec := t.intervalSec()
	if sec <= 0 {
		return 60
	}
	return sec
}

// Stop 停止常驻任务并等待 goroutine 退出。
func (t *KeepaliveTask) Stop() {
	if t == nil {
		return
	}
	t.cancel()
	t.wg.Wait()
}
