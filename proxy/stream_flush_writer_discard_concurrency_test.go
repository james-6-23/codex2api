package proxy

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/security/promptfilter"
)

const discardRaceEvent = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"runaway repetition\"}\n\n"

// concurrentSafeBuffer 是并发回归专用 sink。streamFlushWriter 把下游写放在
// 内部状态锁之外执行（慢速客户端不得阻塞 DiscardBuffered），因此测试夹具自己
// 必须线程安全，避免把夹具的数据竞争误判成生产代码问题。
type concurrentSafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *concurrentSafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *concurrentSafeBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *concurrentSafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// guard 的 discard 回调在 HandleTripped 的 once 内锁外执行，与持 downstreamMu
// 的保活/forward 并发共享同一个 streamFlushWriter。回归要求：不 panic（不再并
// 发读写 bytes.Buffer/扫描器）、丢弃状态只生效一次、discard 完成后任何入口都
// 不再向 sink 写字节。
func TestStreamFlushWriterDiscardBufferedConcurrent(t *testing.T) {
	sink := &concurrentSafeBuffer{}
	w := &streamFlushWriter{writer: sink, policy: StreamFlushPolicyCoalesce, interval: time.Hour, lastFlush: time.Now()}
	event := []byte(discardRaceEvent)
	// 缓冲里先囤住“已判定失控”的残留字节：discard 必须把它们彻底丢弃。
	if err := w.WriteBytes(event); err != nil {
		t.Fatalf("prefill WriteBytes: %v", err)
	}
	if w.buffer.Len() == 0 {
		t.Fatalf("expected prefill to stay buffered")
	}

	const (
		writers    = 50
		iterations = 200
	)
	start := make(chan struct{})
	midway := make(chan struct{})
	var midwayOnce sync.Once
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				_ = w.Flush()
				switch {
				case j%3 == 0:
					_ = w.WriteBytes(event)
				case j%7 == 0:
					_ = w.WriteSSEData(event)
				case j%11 == 0:
					_ = w.WriteString(discardRaceEvent)
				}
				if j%13 == 0 {
					_ = w.Finalize()
				}
				if j == 10 {
					// 保证 discard 发生在写 goroutine 仍在跑的时候。
					midwayOnce.Do(func() { close(midway) })
				}
			}
		}()
	}
	discarded := make(chan struct{})
	go func() {
		defer close(discarded)
		<-start
		<-midway
		w.DiscardBuffered()
	}()
	close(start)
	wg.Wait()
	<-discarded

	if !w.aborted.Load() {
		t.Fatalf("expected aborted after DiscardBuffered")
	}
	if w.buffer.Len() != 0 || w.scanCarry != nil || w.outputScanner != nil {
		t.Fatalf("expected buffer/scanCarry/outputScanner cleared")
	}

	// discard 之后所有写入口都必须短路：不得再向 sink 写任何字节。
	before := sink.Len()
	for i := 0; i < 100; i++ {
		if err := w.Flush(); err != nil {
			t.Fatalf("Flush after discard: %v", err)
		}
		if err := w.Finalize(); err != nil {
			t.Fatalf("Finalize after discard: %v", err)
		}
		if err := w.WriteBytes(event); err != nil {
			t.Fatalf("WriteBytes after discard: %v", err)
		}
		if err := w.WriteString(discardRaceEvent); err != nil {
			t.Fatalf("WriteString after discard: %v", err)
		}
		if err := w.WriteSSEData(event); err != nil {
			t.Fatalf("WriteSSEData after discard: %v", err)
		}
		if err := w.WriteSSEComment(": keepalive\n\n"); err != nil {
			t.Fatalf("WriteSSEComment after discard: %v", err)
		}
	}
	if got := sink.Len(); got != before {
		t.Fatalf("discard 后仍有字节写入下游: before=%d after=%d", before, got)
	}
	if w.buffer.Len() != 0 {
		t.Fatalf("discard 后缓冲又被写入: %q", w.buffer.String())
	}

	// 重复/并发调用 DiscardBuffered 必须幂等：不 panic、状态不回退、无新字节。
	var repeat sync.WaitGroup
	for i := 0; i < writers; i++ {
		repeat.Add(1)
		go func() {
			defer repeat.Done()
			w.DiscardBuffered()
		}()
	}
	repeat.Wait()
	if !w.aborted.Load() || w.buffer.Len() != 0 || w.scanCarry != nil || w.outputScanner != nil {
		t.Fatalf("repeated discard must stay idempotent")
	}
	if got := sink.Len(); got != before {
		t.Fatalf("repeated discard wrote bytes: before=%d after=%d", before, got)
	}
}

// blockingWriteSink 让第一次下游写停在 Write 里，用于验证 DiscardBuffered 不会
// 被慢速/阻塞的下游写拖住（guard 终止编排“不被下游写锁延迟”的约束）。
type blockingWriteSink struct {
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	mu        sync.Mutex
	buf       bytes.Buffer
}

func newBlockingWriteSink() *blockingWriteSink {
	return &blockingWriteSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingWriteSink) Write(p []byte) (int, error) {
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *blockingWriteSink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func TestStreamFlushWriterDiscardBufferedNotBlockedBySlowDownstream(t *testing.T) {
	sink := newBlockingWriteSink()
	w := &streamFlushWriter{writer: sink, policy: StreamFlushPolicyCoalesce, interval: time.Hour, lastFlush: time.Now()}
	event := []byte(discardRaceEvent)
	if err := w.WriteBytes(event); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if w.buffer.Len() == 0 {
		t.Fatalf("expected prefill to stay buffered")
	}

	flushDone := make(chan error, 1)
	go func() { flushDone <- w.Flush() }()
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("Flush 未进入下游写入")
	}

	// 此刻下游写仍阻塞在 sink 内；discard 必须立刻返回而不是等下游 I/O。
	discardDone := make(chan struct{})
	go func() {
		defer close(discardDone)
		w.DiscardBuffered()
	}()
	select {
	case <-discardDone:
	case <-time.After(5 * time.Second):
		close(sink.release)
		t.Fatalf("DiscardBuffered 被慢速下游写阻塞")
	}
	if !w.aborted.Load() || w.buffer.Len() != 0 || w.scanCarry != nil || w.outputScanner != nil {
		t.Fatalf("discard 未完成清理: aborted=%t buffer=%d carry=%v scanner=%v", w.aborted.Load(), w.buffer.Len(), w.scanCarry != nil, w.outputScanner != nil)
	}

	close(sink.release)
	if err := <-flushDone; err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}

	// 已进入 Write 的那一笔无法撤回；但 discard 之后的新入口一律不得再写。
	before := sink.Len()
	for i := 0; i < 20; i++ {
		if err := w.Flush(); err != nil {
			t.Fatalf("Flush after discard: %v", err)
		}
		if err := w.Finalize(); err != nil {
			t.Fatalf("Finalize after discard: %v", err)
		}
		if err := w.WriteBytes(event); err != nil {
			t.Fatalf("WriteBytes after discard: %v", err)
		}
		if err := w.WriteSSEData(event); err != nil {
			t.Fatalf("WriteSSEData after discard: %v", err)
		}
		if err := w.WriteString(discardRaceEvent); err != nil {
			t.Fatalf("WriteString after discard: %v", err)
		}
	}
	if got := sink.Len(); got != before {
		t.Fatalf("discard 后仍有字节写入下游: before=%d after=%d", before, got)
	}
}

// TestStreamFlushWriterDiscardBufferedConcurrentWithScanner 是挂有 outputScanner
// 时的并发回归变体。DiscardBuffered 与 Flush/WriteSSEData/WriteBytes/Finalize
// 并发交错，验证 -race 下无 panic、outputScanner 清理安全，且 discard 后零新字节。
func TestStreamFlushWriterDiscardBufferedConcurrentWithScanner(t *testing.T) {
	sink := &concurrentSafeBuffer{}
	cfg := promptfilter.DefaultConfig()
	cfg.Enabled = true
	cfg.Advanced.Output.Enabled = true
	cfg.Advanced.Output.BufferBytes = 256
	cfg.Advanced.Output.OverlapBytes = 64
	scanner := promptfilter.NewOutputScanner(cfg)
	if scanner == nil {
		t.Fatal("expected non-nil output scanner")
	}

	w := &streamFlushWriter{
		writer:        sink,
		policy:        StreamFlushPolicyCoalesce,
		interval:      time.Hour,
		lastFlush:     time.Now(),
		outputScanner: scanner,
	}
	event := []byte(discardRaceEvent)
	if err := w.WriteBytes(event); err != nil {
		t.Fatalf("prefill WriteBytes: %v", err)
	}

	const (
		writers    = 50
		iterations = 200
	)
	start := make(chan struct{})
	midway := make(chan struct{})
	var midwayOnce sync.Once
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				_ = w.Flush()
				switch {
				case j%2 == 0:
					_ = w.WriteSSEData(event)
				case j%3 == 0:
					_ = w.WriteBytes(event)
				case j%5 == 0:
					_ = w.WriteString(discardRaceEvent)
				}
				if j%7 == 0 {
					_ = w.Finalize()
				}
				if j == 10 {
					midwayOnce.Do(func() { close(midway) })
				}
			}
		}()
	}
	discarded := make(chan struct{})
	go func() {
		defer close(discarded)
		<-start
		<-midway
		w.DiscardBuffered()
	}()
	close(start)
	wg.Wait()
	<-discarded

	if !w.aborted.Load() {
		t.Fatalf("expected aborted after DiscardBuffered")
	}
	if w.buffer.Len() != 0 || w.scanCarry != nil || w.outputScanner != nil {
		t.Fatalf("expected buffer/scanCarry/outputScanner cleared")
	}

	// discard 之后所有写入口都必须短路：不得再向 sink 写任何字节。
	before := sink.Len()
	for i := 0; i < 100; i++ {
		if err := w.Flush(); err != nil {
			t.Fatalf("Flush after discard: %v", err)
		}
		if err := w.Finalize(); err != nil {
			t.Fatalf("Finalize after discard: %v", err)
		}
		if err := w.WriteBytes(event); err != nil {
			t.Fatalf("WriteBytes after discard: %v", err)
		}
		if err := w.WriteString(discardRaceEvent); err != nil {
			t.Fatalf("WriteString after discard: %v", err)
		}
		if err := w.WriteSSEData(event); err != nil {
			t.Fatalf("WriteSSEData after discard: %v", err)
		}
		if err := w.WriteSSEComment(": keepalive\n\n"); err != nil {
			t.Fatalf("WriteSSEComment after discard: %v", err)
		}
	}
	if got := sink.Len(); got != before {
		t.Fatalf("discard 后仍有字节写入下游: before=%d after=%d", before, got)
	}
	if w.buffer.Len() != 0 {
		t.Fatalf("discard 后缓冲又被写入: %q", w.buffer.String())
	}
}
