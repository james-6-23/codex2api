package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestDownstreamSSEKeepaliveIntervalFromEnv 验证 HTTP/SSE 保活周期的默认、覆盖和禁用值。
func TestDownstreamSSEKeepaliveIntervalFromEnv(t *testing.T) {
	const fallback = defaultDownstreamSSEKeepaliveInterval
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "default", want: fallback},
		{name: "custom", raw: "45s", want: 45 * time.Second},
		{name: "disabled", raw: "0", want: 0},
		{name: "invalid", raw: "later", want: fallback},
		{name: "negative", raw: "-1s", want: fallback},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL", test.raw)
			if got := downstreamSSEKeepaliveIntervalFromEnv(); got != test.want {
				t.Fatalf("interval = %s, want %s", got, test.want)
			}
		})
	}
}

// TestDownstreamMessagesKeepaliveEvent 验证 Messages 使用兼容协议的 ping 事件载荷。
func TestDownstreamMessagesKeepaliveEvent(t *testing.T) {
	const want = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	if downstreamMessagesKeepaliveEvent != want {
		t.Fatalf("Messages keepalive = %q, want %q", downstreamMessagesKeepaliveEvent, want)
	}
}

func TestDownstreamSSEKeepaliveStopsAndJoins(t *testing.T) {
	var writes atomic.Int32
	firstWrite := make(chan struct{}, 1)
	stop := startDownstreamSSEKeepalive(context.Background(), time.Millisecond, func() bool {
		writes.Add(1)
		select {
		case firstWrite <- struct{}{}:
		default:
		}
		return true
	})

	select {
	case <-firstWrite:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("keepalive did not fire")
	}
	stop()
	stoppedAt := writes.Load()
	time.Sleep(10 * time.Millisecond)
	if got := writes.Load(); got != stoppedAt {
		t.Fatalf("keepalive wrote after stop returned: %d -> %d", stoppedAt, got)
	}
}

func TestDownstreamSSEKeepaliveStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var writes atomic.Int32
	firstWrite := make(chan struct{}, 1)
	stop := startDownstreamSSEKeepalive(ctx, time.Millisecond, func() bool {
		writes.Add(1)
		select {
		case firstWrite <- struct{}{}:
		default:
		}
		return true
	})
	defer stop()

	select {
	case <-firstWrite:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("keepalive did not fire")
	}
	cancel()
	stop()
	stoppedAt := writes.Load()
	time.Sleep(10 * time.Millisecond)
	if got := writes.Load(); got != stoppedAt {
		t.Fatalf("keepalive wrote after context cancellation: %d -> %d", stoppedAt, got)
	}
}

func TestDownstreamSSEKeepaliveStopsWhenWriterFails(t *testing.T) {
	var writes atomic.Int32
	firstWrite := make(chan struct{}, 1)
	stop := startDownstreamSSEKeepalive(context.Background(), time.Millisecond, func() bool {
		writes.Add(1)
		select {
		case firstWrite <- struct{}{}:
		default:
		}
		return false
	})
	defer stop()

	select {
	case <-firstWrite:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("keepalive did not fire")
	}
	stop()
	if got := writes.Load(); got != 1 {
		t.Fatalf("writer failure must stop keepalive after one write, got %d", got)
	}
}
