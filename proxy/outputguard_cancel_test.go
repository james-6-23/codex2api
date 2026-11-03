package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/security/outputguard"
)

type recordingCloser struct {
	mu     sync.Mutex
	closed bool
}

func (r *recordingCloser) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func (r *recordingCloser) IsClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func TestGuardTerminationPrecedesDownstreamLock(t *testing.T) {
	cfg := outputguard.Config{Enabled: true, MaxToolArgumentBytes: 128 * 1024, MaxToolArgumentDeltas: 4096, MaxChannelBytes: 2 * 1024 * 1024, MaxRequestBytes: 4 * 1024 * 1024}
	rg := outputguard.NewRequestGuard(cfg)
	upstreamCtx, upstreamCancel := context.WithCancel(context.Background())
	defer upstreamCancel()
	closer := &recordingCloser{}
	coordinator := NewGuardTerminationCoordinator(rg, upstreamCancel, closer)

	// 下游写锁在整个 guard 终止流程中保持占用：终止协调必须完全不受它影响。
	var downstreamMu sync.Mutex
	downstreamMu.Lock()
	var discardCalls int32
	outcome := coordinator.HandleTripped("repetition", func() { atomic.AddInt32(&discardCalls, 1) })

	select {
	case <-upstreamCtx.Done():
	default:
		t.Fatalf("expected upstream context to be canceled immediately")
	}
	if !closer.IsClosed() {
		t.Fatalf("expected upstream body to be closed immediately")
	}
	if got := atomic.LoadInt32(&discardCalls); got != 1 {
		t.Fatalf("expected exactly 1 discard callback, got %d", got)
	}
	if !outcome.guardTripped || outcome.guardReason != "repetition" || outcome.logStatusCode != http.StatusBadGateway || outcome.penalize || !outcome.terminalLocal || outcome.failurePayload != nil {
		t.Fatalf("unexpected guard outcome: %+v", outcome)
	}
	downstreamMu.Unlock()
}

func TestGuardTerminationIdempotentReasonFreeze(t *testing.T) {
	cfg := outputguard.Config{Enabled: true, MaxToolArgumentBytes: 128 * 1024, MaxToolArgumentDeltas: 4096, MaxChannelBytes: 2 * 1024 * 1024, MaxRequestBytes: 4 * 1024 * 1024}
	rg := outputguard.NewRequestGuard(cfg)
	cancelCalls := int32(0)
	closer := &recordingCloser{}
	coordinator := NewGuardTerminationCoordinator(rg, func() { atomic.AddInt32(&cancelCalls, 1) }, closer)

	var discardCalls int32
	first := coordinator.HandleTripped("repetition", func() { atomic.AddInt32(&discardCalls, 1) })
	if first.guardReason != "repetition" {
		t.Fatalf("expected first reason repetition, got %s", first.guardReason)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := coordinator.HandleTripped("channel_bytes", func() { atomic.AddInt32(&discardCalls, 1) })
			if out.guardReason != "repetition" {
				t.Errorf("expected frozen repetition, got %s", out.guardReason)
			}
			if out.logStatusCode != http.StatusBadGateway || !out.guardTripped || out.penalize {
				t.Errorf("unexpected concurrent outcome: %+v", out)
			}
		}()
	}
	wg.Wait()

	if calls := atomic.LoadInt32(&cancelCalls); calls != 1 {
		t.Fatalf("expected 1 cancel call, got %d", calls)
	}
	if !closer.IsClosed() {
		t.Fatalf("expected body closed")
	}
	if got := atomic.LoadInt32(&discardCalls); got != 1 {
		t.Fatalf("expected exactly 1 discard callback, got %d", got)
	}
}

func TestStreamFlushWriterDiscardBuffered(t *testing.T) {
	var sink bytes.Buffer
	w := &streamFlushWriter{writer: &sink, policy: StreamFlushPolicyCoalesce, interval: time.Hour, lastFlush: time.Now()}
	if err := w.WriteBytes([]byte("data: partial runaway malicious repetition\n\n")); err != nil {
		t.Fatalf("WriteBytes returned error: %v", err)
	}
	if sink.Len() > 0 {
		t.Fatalf("buffer should not flush prematurely")
	}
	if w.buffer.Len() == 0 {
		t.Fatalf("expected data to be buffered before discard")
	}

	w.DiscardBuffered()
	if w.buffer.Len() != 0 || w.scanCarry != nil || w.outputScanner != nil {
		t.Fatalf("expected buffer, scanCarry and outputScanner cleared")
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}
	if err := w.Finalize(); err != nil {
		t.Fatalf("Finalize returned error: %v", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("expected sink empty after DiscardBuffered, got: %q", sink.String())
	}
}

func TestOverlayGuardOutcomePreservesIdentity(t *testing.T) {
	cfg := outputguard.Config{Enabled: true, MaxToolArgumentBytes: 128 * 1024, MaxToolArgumentDeltas: 4096, MaxChannelBytes: 2 * 1024 * 1024, MaxRequestBytes: 100}
	rg := outputguard.NewRequestGuard(cfg)
	_ = rg.ObserveAppend("c1", "", outputguard.KindText, bytes.Repeat([]byte("A"), 200))
	if !rg.IsTripped() {
		t.Fatalf("expected guard tripped")
	}

	rawOutcome := classifyStreamOutcome(context.Canceled, errors.New("connection reset"), nil, false)
	if rawOutcome.logStatusCode != logStatusClientClosed {
		t.Fatalf("raw outcome want 499, got %d", rawOutcome.logStatusCode)
	}
	// 模拟断流分类残留的账号/请求级标记：guard 终态必须把它们全部清零，
	// 否则 2596/5120/6021 等分支仍会按普通上游故障处理（容量降载重试、
	// 模型不支持换号、请求级归因、WS 鉴权探针）。
	rawOutcome.capacityShed = true
	rawOutcome.modelUnsupported = true
	rawOutcome.requestScoped = true
	rawOutcome.verifyAccountAuth = true

	finalOutcome := OverlayGuardOutcome(rawOutcome, rg)
	if !finalOutcome.guardTripped || finalOutcome.guardReason != "request_bytes" || finalOutcome.logStatusCode != http.StatusBadGateway || finalOutcome.penalize || !finalOutcome.terminalLocal {
		t.Fatalf("unexpected final outcome: %+v", finalOutcome)
	}
	if finalOutcome.capacityShed || finalOutcome.modelUnsupported || finalOutcome.requestScoped || finalOutcome.verifyAccountAuth {
		t.Fatalf("guard outcome must clear residual classification flags: %+v", finalOutcome)
	}
	if finalOutcome.failureKind != "model_output_guard" || finalOutcome.failureMessage != GuardTriggeredPublicMessage || finalOutcome.failurePayload != nil {
		t.Fatalf("unexpected failure identity: %+v", finalOutcome)
	}

	// 未触发时保持原 outcome 完全不变。
	untripped := outputguard.NewRequestGuard(cfg)
	if got := OverlayGuardOutcome(rawOutcome, untripped); !reflect.DeepEqual(got, rawOutcome) {
		t.Fatalf("untripped guard must not alter outcome: %+v", got)
	}
}
