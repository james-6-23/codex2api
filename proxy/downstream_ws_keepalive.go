package proxy

import (
	"context"
	"time"

	"github.com/gorilla/websocket"
)

const defaultDownstreamWSKeepaliveInterval = 45 * time.Second

var downstreamWSKeepaliveInterval = downstreamWSKeepaliveIntervalFromEnv()

func downstreamWSKeepaliveIntervalFromEnv() time.Duration {
	return durationFromEnv("DOWNSTREAM_WS_KEEPALIVE_INTERVAL", defaultDownstreamWSKeepaliveInterval)
}

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
