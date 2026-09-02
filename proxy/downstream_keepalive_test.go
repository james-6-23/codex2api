package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

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
