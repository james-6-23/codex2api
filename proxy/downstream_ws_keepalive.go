package proxy

import (
	"context"
	"time"

	"github.com/gorilla/websocket"
)

const defaultDownstreamWSKeepaliveInterval = 45 * time.Second

var downstreamWSKeepaliveInterval = downstreamWSKeepaliveIntervalFromEnv()

// downstreamWSKeepaliveIntervalFromEnv 读取下游 WebSocket Ping 周期配置。
func downstreamWSKeepaliveIntervalFromEnv() time.Duration {
	return durationFromEnv("DOWNSTREAM_WS_KEEPALIVE_INTERVAL", defaultDownstreamWSKeepaliveInterval)
}

// startDownstreamWSKeepalive 周期发送 WebSocket Ping，并在写入失败时取消连接。
func startDownstreamWSKeepalive(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) func() {
	if conn == nil {
		return func() {}
	}
	return startDownstreamSSEKeepalive(ctx, downstreamWSKeepaliveInterval, func() bool {
		if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(responsesWSWriteTimeout)); err != nil {
			if cancel != nil {
				cancel()
			}
			_ = conn.Close()
			return false
		}
		return true
	})
}
