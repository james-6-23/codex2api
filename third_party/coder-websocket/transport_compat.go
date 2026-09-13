//go:build !js

package websocket

import (
	"context"
	"fmt"
)

// WritePing writes a Ping without waiting for Pong. The application's reader
// owns Pong correlation and liveness decisions independently of data writes.
func (c *Conn) WritePing(ctx context.Context, payload []byte) error {
	if len(payload) > maxControlPayload {
		return fmt.Errorf("ping payload exceeds %d bytes", maxControlPayload)
	}
	return c.writeControl(ctx, opPing, payload)
}

func (c *Conn) writeDataFrames(ctx context.Context, fin, flate bool, op opcode, payload []byte) (int, error) {
	if c.disableFragmentation {
		return c.writeFrame(ctx, fin, flate, op, payload)
	}
	const maxFramePayload = 16 << 10
	written := 0
	for len(payload) > maxFramePayload {
		n, err := c.writeFrame(ctx, false, flate, op, payload[:maxFramePayload])
		written += n
		if err != nil {
			return written, err
		}
		payload = payload[maxFramePayload:]
		op = opContinuation
	}
	n, err := c.writeFrame(ctx, fin, flate, op, payload)
	return written + n, err
}
