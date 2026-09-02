package proxy

import (
	"context"
	"sync"
	"time"
)

const (
	// 普通 Responses SSE 在上游长时间只思考、不产出可转发事件时，也要持续
	// 刷新下游链路的 idle timer。10 秒低于常见的 30/60 秒反代超时，同时
	// 每分钟仅增加几十字节；SSE 注释不会被 Codex 客户端当作模型输出。
	defaultDownstreamSSEKeepaliveInterval = 10 * time.Second
	downstreamSSEKeepaliveComment         = ": keepalive\n\n"
)

// 变量形式只为处理器级测试缩短等待；生产运行保持默认 10 秒。
var downstreamSSEKeepaliveInterval = defaultDownstreamSSEKeepaliveInterval

// startDownstreamSSEKeepalive 周期执行 writeKeepalive，直到请求取消、写失败
// 或调用 stop。stop 会等待 goroutine 完整退出，保证流收尾后不再并发写入。
func startDownstreamSSEKeepalive(ctx context.Context, interval time.Duration, writeKeepalive func() bool) func() {
	if interval <= 0 || writeKeepalive == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	stopCh := make(chan struct{})
	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !writeKeepalive() {
					return
				}
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopCh)
			<-done
		})
	}
}
