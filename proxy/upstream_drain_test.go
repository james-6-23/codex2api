package proxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDrainableUpstreamContextSurvivesAfterClientCancel(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())

	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, 50*time.Millisecond)
	defer cancelUpstream()

	cancelClient()

	// 客户端取消瞬间上游仍应活着，给后续 SSE drain 留时间。
	select {
	case <-upstreamCtx.Done():
		t.Fatal("upstream ctx canceled too early after client cancel")
	case <-time.After(10 * time.Millisecond):
	}

	// 等到 drain timeout 到达，上游应该被取消。
	select {
	case <-upstreamCtx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("upstream ctx not canceled after drain timeout")
	}

	if !errors.Is(upstreamCtx.Err(), context.Canceled) {
		t.Fatalf("upstream ctx err = %v, want context.Canceled", upstreamCtx.Err())
	}
}

func TestDrainableUpstreamContextManualCancelStopsTimer(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()

	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, time.Hour)

	cancelUpstream()
	select {
	case <-upstreamCtx.Done():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("manual cancel did not propagate")
	}
}

func TestDrainableUpstreamContextStaysAliveWhileClientAlive(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()

	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, 10*time.Millisecond)
	defer cancelUpstream()

	select {
	case <-upstreamCtx.Done():
		t.Fatal("upstream ctx canceled while client still alive")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestDrainableUpstreamContextPreservesRequestValues(t *testing.T) {
	type contextKey struct{}
	clientCtx := context.WithValue(context.Background(), contextKey{}, "audit-value")

	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, time.Second)
	defer cancelUpstream()

	if got := upstreamCtx.Value(contextKey{}); got != "audit-value" {
		t.Fatalf("upstream context value = %v, want audit-value", got)
	}
}
