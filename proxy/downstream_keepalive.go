package proxy

import (
	"context"
	"sync"
	"time"
)

const (
	// 普通 SSE 在上游长时间只思考、不产出可转发事件时，也要持续刷新下游
	// 链路的 idle timer。SSE 注释不会被客户端当作模型输出。
	defaultDownstreamHTTPKeepaliveInterval = 30 * time.Second
	defaultDownstreamSSEKeepaliveInterval  = defaultDownstreamHTTPKeepaliveInterval
	downstreamSSEKeepaliveComment          = ": keepalive\n\n"
	downstreamMessagesKeepaliveEvent       = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
)

// 变量形式只为处理器级测试缩短等待；生产运行从环境变量读取。
var downstreamSSEKeepaliveInterval = downstreamSSEKeepaliveIntervalFromEnv()

// downstreamSSEKeepaliveIntervalFromEnv 读取下游 HTTP/SSE 保活周期配置。
func downstreamSSEKeepaliveIntervalFromEnv() time.Duration {
	return durationFromEnv("DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL", defaultDownstreamSSEKeepaliveInterval)
}

// ConfigureDownstreamKeepaliveFromEnv 在 config.Load 读取 .env 后刷新保活配置。
// 进程环境变量仍会在包初始化时生效。
func ConfigureDownstreamKeepaliveFromEnv() {
	downstreamSSEKeepaliveInterval = downstreamSSEKeepaliveIntervalFromEnv()
	continuousRetryKeepaliveInterval = downstreamSSEKeepaliveInterval
	downstreamWSKeepaliveInterval = downstreamWSKeepaliveIntervalFromEnv()
}

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
