package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const pendingFirstTokenFlushBytes = 1024 * 1024

var (
	sseDataPrefix = []byte("data: ")
	sseDataSuffix = []byte("\n\n")
)

// streamFlushWriter 负责 SSE 流的缓冲合并与冲刷传输。
//
// 并发契约：同一实例的 Flush/WriteBytes/WriteString/WriteSSEData/Finalize 等写入方法
// 不得跨 goroutine 并发调用，调用方必须通过外部互斥（例如 handler 中的 downstreamMu）
// 进行串行化保护。内部 mu 与 atomic aborted 仅用于协调输出失控丢弃（DiscardBuffered）
// 与写入/冲刷之间的并发状态清理，避免 DiscardBuffered 等待慢速下游 I/O。
type streamFlushWriter struct {
	writer        io.Writer
	flusher       http.Flusher
	policy        string
	interval      time.Duration
	lastFlush     time.Time
	buffer        bytes.Buffer
	outputScanner *promptfilter.OutputScanner
	// scanCarry 暂存扫描器已放行但尚未凑满一个完整 SSE 事件的尾巴。扫描器按
	// 字节数扣安全窗,切点会落在事件正中间;若把半个事件写到底层,任何绕过本
	// 写入器直写 ResponseWriter 的字节(请求级保活注释、续想心跳)都会插进
	// JSON 里(issue #696)。这里把底层写入对齐到事件边界,保证底层流永远停在
	// 两个事件之间。
	scanCarry    []byte
	writtenBytes atomic.Int64
	diag         *streamPhaseDiagnostics
	// aborted 标记缓冲已被输出失控防护强制丢弃。置位后 Flush/Finalize 必须
	// 静默短路，绝不能再把残留字节写回下游或释放扫描器安全窗。
	// 用 atomic.Bool 而不是普通字段：DiscardBuffered 必须把置位放在所有锁竞争
	// 之前（先让新进入的写入短路，再清理缓冲），并且与 Flush/WriteBytes/
	// Finalize 的读取天然无数据竞争。CAS 保证并发触发时只有第一个调用真正执行
	// 清理。
	aborted atomic.Bool
	// mu 保护 buffer / scanCarry / outputScanner / lastFlush 的读写与清理。
	// 保活 goroutine（issue #458）与 forward goroutine 会并发共享同一个 writer
	// 调 Flush/WriteBytes，guard 的 discard 回调又在 HandleTripped 的 once 里
	// 锁外执行；没有这把锁时 bytes.Buffer 会被并发读写（数据竞争、输出错乱，
	// 最坏 panic）。
	//
	// 下游写（w.writer.Write / flusher.Flush）一律在锁外做：慢速客户端可能让
	// 写入长期阻塞，若在锁内调用就等于让 DiscardBuffered 等待下游 I/O，会破坏
	// guard 终止编排“不被下游写锁延迟”的约束，也可能与用户回调互相重入。
	mu sync.Mutex
}

// Small buffers are reused across token events; an exceptional large event
// must not keep its allocation alive for the rest of a long stream.
// 调用方必须持有 w.mu。
func (w *streamFlushWriter) resetBuffer() {
	if w.buffer.Cap() > 256<<10 {
		w.buffer = bytes.Buffer{}
	} else {
		w.buffer.Reset()
	}
}

// takeBufferedLocked 取走合并缓冲当前内容的快照并重置缓冲。返回的切片在解锁
// 后仍可安全写下游：resetBuffer 只复用 w.buffer 自己的底层数组，快照独立。
// 调用方必须持有 w.mu。
func (w *streamFlushWriter) takeBufferedLocked() []byte {
	if w.buffer.Len() == 0 {
		return nil
	}
	data := append([]byte(nil), w.buffer.Bytes()...)
	w.resetBuffer()
	return data
}

// flushDueLocked 报告合并缓冲是否已到冲刷时间点。调用方必须持有 w.mu。
func (w *streamFlushWriter) flushDueLocked() bool {
	return w.lastFlush.IsZero() || time.Since(w.lastFlush) >= w.interval
}

// streamPhaseDiagnostics 采集流阶段的断流现场判据。
//
// 上游 RST_STREAM（如 "INTERNAL_ERROR; received from peer"）有两类互斥成因，
// 事后只看错误文本无法区分（issue #491）：
//   - 下游背压：SSE 是在读上游的回调里同步写回下游的，客户端读得慢会让
//     body.Read 停摆，HTTP/2 流控窗口不再补充，上游边缘可能因此重置该流。
//     此时 writeBlockedNanos 会显著偏大。
//   - 上游自身：后端被回收/长推理期间安静，边缘直接重置。此时下游写阻塞
//     接近 0，而距上游末帧的间隔偏大。
//
// 两个计数器随断流信息一并落到用量日志，运维在错误明细页即可判读，无需加库表。
type streamPhaseDiagnostics struct {
	writeBlockedNanos   atomic.Int64
	lastUpstreamFrameAt atomic.Int64 // UnixNano
	upstreamFrames      atomic.Int64
}

func newStreamPhaseDiagnostics() *streamPhaseDiagnostics {
	return &streamPhaseDiagnostics{}
}

// annotateStreamBreakDiagnostics 只给断流类失败追加现场判据。客户端主动断开
// （499）与上游显式 response.failed 有各自明确的成因，不需要这两个数字。
func annotateStreamBreakDiagnostics(outcome streamOutcome, diag *streamPhaseDiagnostics) streamOutcome {
	if outcome.logStatusCode != logStatusUpstreamStreamBreak {
		return outcome
	}
	summary := diag.summary()
	if summary == "" {
		return outcome
	}
	outcome.failureMessage = strings.TrimSpace(strings.TrimSpace(outcome.failureMessage) + " " + summary)
	return outcome
}

// markUpstreamFrame 在每个上游 SSE 事件到达时打点。
func (d *streamPhaseDiagnostics) markUpstreamFrame() {
	if d == nil {
		return
	}
	d.lastUpstreamFrameAt.Store(time.Now().UnixNano())
	d.upstreamFrames.Add(1)
}

func (d *streamPhaseDiagnostics) addWriteBlocked(start time.Time) {
	if d == nil || start.IsZero() {
		return
	}
	d.writeBlockedNanos.Add(int64(time.Since(start)))
}

// summary 生成人类可读的现场判据；无任何上游帧时返回空串（连响应头都没读到，
// 与背压无关，加了反而噪声）。
func (d *streamPhaseDiagnostics) summary() string {
	if d == nil {
		return ""
	}
	frames := d.upstreamFrames.Load()
	if frames == 0 {
		return ""
	}
	blockedMs := time.Duration(d.writeBlockedNanos.Load()).Milliseconds()
	sinceLastMs := int64(-1)
	if last := d.lastUpstreamFrameAt.Load(); last > 0 {
		sinceLastMs = time.Since(time.Unix(0, last)).Milliseconds()
	}
	if sinceLastMs < 0 {
		return fmt.Sprintf("[上游帧 %d, 下游写阻塞 %dms]", frames, blockedMs)
	}
	return fmt.Sprintf("[上游帧 %d, 下游写阻塞 %dms, 距上游末帧 %dms]", frames, blockedMs, sinceLastMs)
}

func (h *Handler) newStreamFlushWriter(c *gin.Context, writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	w := newStreamFlushWriter(writer, flusher)
	if h != nil && h.store != nil {
		cfg := h.promptFilterConfigForRequest(c)
		if cfg.Enabled && cfg.Advanced.Output.Enabled {
			w.outputScanner = promptfilter.NewOutputScannerFromNormalizedConfig(cfg)
		}
	}
	return w
}

// newAttemptStreamFlushWriter keeps a continuously retried attempt free of
// local output-policy side effects until the upstream terminal is known. The
// winning attempt is scanned exactly once by commitStreamAttempt below.
func (h *Handler) newAttemptStreamFlushWriter(c *gin.Context, attempt *continuousRetryStreamAttempt, writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	if attempt != nil {
		return newStreamFlushWriter(attempt.writerOr(writer), attempt.flusherOr(flusher))
	}
	return h.newStreamFlushWriter(c, writer, flusher)
}

type streamFlushWriterAdapter struct {
	writer *streamFlushWriter
}

func (a streamFlushWriterAdapter) Write(data []byte) (int, error) {
	if a.writer == nil {
		return 0, io.ErrClosedPipe
	}
	if err := a.writer.WriteBytes(data); err != nil {
		return 0, err
	}
	return len(data), nil
}

// commitStreamAttempt applies the request's output policy only after the
// attempt has reached a successful protocol terminal. A filter or downstream
// write failure is local and must never trigger another upstream request.
func (h *Handler) commitStreamAttempt(c *gin.Context, attempt *continuousRetryStreamAttempt) error {
	if attempt == nil {
		return nil
	}
	if attempt.closed || attempt.replay == nil {
		return errContinuousRetryReplayClosed
	}
	flusher, _ := c.Writer.(http.Flusher)
	filtered := h.newStreamFlushWriter(c, c.Writer, flusher)
	if err := attempt.replay.CommitTo(streamFlushWriterAdapter{writer: filtered}, nil); err != nil {
		return err
	}
	return filtered.Finalize()
}

// scanOutput 把待输出字节送进输出扫描器并只返回可立刻落地的部分。
// 它会读写 outputScanner 与 scanCarry，调用方必须持有 w.mu。
func (w *streamFlushWriter) scanOutput(data []byte) ([]byte, error) {
	if w == nil || w.outputScanner == nil {
		return data, nil
	}
	released, err := w.outputScanner.Push(data)
	if err != nil {
		// 扫描器判定拦截时会丢弃自己的安全窗;尚未落地的尾巴同样不能再出去。
		w.scanCarry = nil
		return nil, err
	}
	return w.alignToEventBoundary(released), nil
}

// alignToEventBoundary 把扫描器放行的字节与之前暂存的尾巴拼接后,只返回到最后
// 一个事件分隔符(空行)为止的部分,余下的半个事件继续暂存。没有完整事件时返回
// nil,调用方视同扫描器仍在扣留。多扣一点只会让输出晚到,绝不会放出扫描器还没
// 看过的字节,所以安全窗语义不变。
func (w *streamFlushWriter) alignToEventBoundary(released []byte) []byte {
	if len(released) == 0 && len(w.scanCarry) == 0 {
		return nil
	}
	w.scanCarry = append(w.scanCarry, released...)
	idx := bytes.LastIndex(w.scanCarry, sseDataSuffix)
	if idx < 0 {
		return nil
	}
	cut := idx + len(sseDataSuffix)
	out := append([]byte(nil), w.scanCarry[:cut]...)
	rest := w.scanCarry[cut:]
	if len(rest) == 0 {
		w.scanCarry = w.scanCarry[:0]
	} else {
		w.scanCarry = append(w.scanCarry[:0], rest...)
	}
	return out
}

func newStreamFlushWriter(writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	settings := CurrentRuntimeSettings()
	return &streamFlushWriter{
		writer:   writer,
		flusher:  flusher,
		policy:   settings.StreamFlushPolicy,
		interval: currentStreamFlushInterval(),
	}
}

func appendSSEData(buf *bytes.Buffer, data []byte) {
	if buf == nil {
		return
	}
	buf.Write(sseDataPrefix)
	buf.Write(data)
	buf.Write(sseDataSuffix)
}

func writeDeferredSSEData(streamWriter *streamFlushWriter, pending *bytes.Buffer, data []byte, shouldDefer bool) (bool, error) {
	if streamWriter == nil {
		return false, nil
	}
	if shouldDefer {
		appendSSEData(pending, data)
		if pending != nil && pending.Len() <= pendingFirstTokenFlushBytes {
			return false, nil
		}
	}
	if pending != nil && pending.Len() > 0 {
		if !shouldDefer {
			appendSSEData(pending, data)
		}
		before := streamWriter.deliveredBytes()
		if err := streamWriter.WriteBytes(pending.Bytes()); err != nil {
			return false, err
		}
		pending.Reset()
		return streamWriter.deliveredBytes() > before, nil
	}
	if shouldDefer {
		return false, nil
	}
	before := streamWriter.deliveredBytes()
	if err := streamWriter.WriteSSEData(data); err != nil {
		return false, err
	}
	return streamWriter.deliveredBytes() > before, nil
}

func (w *streamFlushWriter) deliveredBytes() int64 {
	if w == nil {
		return 0
	}
	return w.writtenBytes.Load()
}

func (w *streamFlushWriter) writeUnderlying(data []byte) error {
	if w == nil || w.writer == nil || len(data) == 0 {
		return nil
	}
	blockStart := w.diagnosticsClock()
	written, err := w.writer.Write(data)
	w.diag.addWriteBlocked(blockStart)
	if written > 0 {
		w.writtenBytes.Add(int64(written))
	}
	return err
}

func (w *streamFlushWriter) writeUnderlyingString(data string) error {
	if w == nil || w.writer == nil || data == "" {
		return nil
	}
	blockStart := w.diagnosticsClock()
	written, err := io.WriteString(w.writer, data)
	w.diag.addWriteBlocked(blockStart)
	if written > 0 {
		w.writtenBytes.Add(int64(written))
	}
	return err
}

// diagnosticsClock 仅在挂了诊断器时取时间戳，未开启时零开销。
func (w *streamFlushWriter) diagnosticsClock() time.Time {
	if w == nil || w.diag == nil {
		return time.Time{}
	}
	return time.Now()
}

// WriteString 向流中写入字符串。
// 契约：同一实例的 Flush/WriteBytes/WriteString 等方法不得跨 goroutine 并发调用。
func (w *streamFlushWriter) WriteString(data string) error {
	if w == nil || w.writer == nil {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	w.mu.Lock()
	if w.aborted.Load() {
		// 入口检查后 DiscardBuffered 已置位：绝不能再把字节写回缓冲，否则
		// 清理后的缓冲会被重新填满，“缓冲彻底丢弃”不成立。
		w.mu.Unlock()
		return nil
	}
	filtered, err := w.scanOutput([]byte(data))
	coalesce := w.policy == StreamFlushPolicyCoalesce
	due := false
	if err == nil && len(filtered) > 0 && coalesce {
		_, _ = w.buffer.WriteString(string(filtered))
		due = w.flushDueLocked()
	}
	w.mu.Unlock()
	if err != nil || len(filtered) == 0 {
		return err
	}
	if !coalesce {
		if w.aborted.Load() {
			return nil
		}
		if err := w.writeUnderlyingString(string(filtered)); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if due {
		return w.Flush()
	}
	return nil
}

// WriteBytes 向流中写入字节切片。
// 契约：同一实例的 Flush/WriteBytes/WriteString 等方法不得跨 goroutine 并发调用。
func (w *streamFlushWriter) WriteBytes(data []byte) error {
	if w == nil || w.writer == nil || len(data) == 0 {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	w.mu.Lock()
	if w.aborted.Load() {
		w.mu.Unlock()
		return nil
	}
	filtered, err := w.scanOutput(data)
	coalesce := w.policy == StreamFlushPolicyCoalesce
	due := false
	if err == nil && len(filtered) > 0 && coalesce {
		_, _ = w.buffer.Write(filtered)
		due = w.flushDueLocked()
	}
	w.mu.Unlock()
	if err != nil || len(filtered) == 0 {
		return err
	}
	if !coalesce {
		if w.aborted.Load() {
			return nil
		}
		if err := w.writeUnderlying(filtered); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if due {
		return w.Flush()
	}
	return nil
}

// WriteSSEData 向流中写入一条 SSE 数据帧（带 "data: " 前缀与 "\n\n" 后缀）。
// 契约：同一实例的 Flush/WriteBytes/WriteSSEData 等方法不得跨 goroutine 并发调用。
func (w *streamFlushWriter) WriteSSEData(data []byte) error {
	if w == nil || w.writer == nil {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	framed := make([]byte, 0, len(sseDataPrefix)+len(data)+len(sseDataSuffix))
	framed = append(framed, sseDataPrefix...)
	framed = append(framed, data...)
	framed = append(framed, sseDataSuffix...)
	w.mu.Lock()
	if w.aborted.Load() {
		w.mu.Unlock()
		return nil
	}
	filtered, err := w.scanOutput(framed)
	coalesce := w.policy == StreamFlushPolicyCoalesce
	due := false
	if err == nil && len(filtered) > 0 && coalesce {
		w.buffer.Write(filtered)
		due = w.flushDueLocked()
	}
	w.mu.Unlock()
	if err != nil || len(filtered) == 0 {
		return err
	}
	if !coalesce {
		if w.aborted.Load() {
			return nil
		}
		if err := w.writeUnderlying(filtered); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if due {
		return w.Flush()
	}
	return nil
}

// WriteSSEComment 写一条 SSE 注释(如 ": keepalive\n\n")并立即冲刷传输。
// 注释不是模型输出,不进扫描器:先排空合并缓冲(里面只有完整事件)再直写底层。
// 输出过滤开启时,scanOutput 已把底层写入对齐到事件边界(见 scanCarry),
// 底层流不会停在半个事件里,直写是安全的;若改走扫描器,注释会和安全窗一起
// 被扣到下一个终态帧,上游长时间静默时保活就完全失效。
func (w *streamFlushWriter) WriteSSEComment(comment string) error {
	if w == nil || w.writer == nil || comment == "" {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	w.mu.Lock()
	buffered := w.takeBufferedLocked()
	w.mu.Unlock()
	if w.aborted.Load() {
		return nil
	}
	if err := w.writeUnderlying(buffered); err != nil {
		return err
	}
	if w.aborted.Load() {
		return nil
	}
	if err := w.writeUnderlyingString(comment); err != nil {
		return err
	}
	w.flushTransport()
	return nil
}

// DiscardBuffered 彻底丢弃本地缓冲：清空合并 buffer、扫描暂存尾巴并解绑输出
// 扫描器，同时置位 aborted。guard 命中后调用一次即可保证后续 Flush/Finalize
// 不再输出任何数据。
//
// 并发契约：aborted 用 CAS 最先置位，保证多路触发（guard 判定、保活/forward
// 竞态）下只有第一个调用执行清理；CAS 失败说明已有调用完成过丢弃，直接返回。
// 清理过程只拿内部状态锁，不碰下游写锁，因此不会被慢速客户端拖住。
func (w *streamFlushWriter) DiscardBuffered() {
	if w == nil {
		return
	}
	if !w.aborted.CompareAndSwap(false, true) {
		return
	}
	w.mu.Lock()
	w.resetBuffer()
	w.scanCarry = nil
	w.outputScanner = nil
	w.mu.Unlock()
}

// Flush 冲刷当前合并缓冲与输出扫描器已放行的安全窗。
// 契约：同一实例的 Flush/WriteBytes 等方法不得跨 goroutine 并发调用。
func (w *streamFlushWriter) Flush() error {
	if w == nil {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	// 锁内只做缓冲/安全窗的快照与清理，写下游在锁外完成。
	w.mu.Lock()
	buffered := w.takeBufferedLocked()
	var released []byte
	var scanErr error
	if w.outputScanner != nil {
		released, scanErr = w.outputScanner.Flush()
	}
	w.mu.Unlock()
	if w.aborted.Load() {
		// 已判定失控的缓冲快照就地丢弃，不再释放到下游。
		return nil
	}
	if err := w.writeUnderlying(buffered); err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	if w.aborted.Load() {
		return nil
	}
	if err := w.writeUnderlying(released); err != nil {
		return err
	}
	w.flushTransport()
	return nil
}

// Finalize releases the retained safety window at a real semantic end-of-stream.
// A transport Flush must not call this because an unsafe phrase may span chunks.
func (w *streamFlushWriter) Finalize() error {
	if w == nil || w.aborted.Load() {
		return nil
	}
	// 第一阶段：只取走合并缓冲快照，写下游在锁外。
	w.mu.Lock()
	buffered := w.takeBufferedLocked()
	w.mu.Unlock()
	if w.aborted.Load() {
		return nil
	}
	if err := w.writeUnderlying(buffered); err != nil {
		return err
	}
	// 第二阶段：释放扫描器安全窗与暂存的半个事件。仍不持锁写下游。
	w.mu.Lock()
	if w.aborted.Load() {
		w.mu.Unlock()
		return nil
	}
	var released, carry []byte
	var scanErr error
	if w.outputScanner != nil {
		released, scanErr = w.outputScanner.Finalize()
		if scanErr != nil {
			// 判定拦截时连同暂存的半个事件一起丢弃。
			w.scanCarry = nil
		} else {
			carry = w.scanCarry
			w.scanCarry = nil
		}
	}
	w.mu.Unlock()
	if scanErr != nil {
		return scanErr
	}
	if w.aborted.Load() {
		return nil
	}
	// 流真正结束:先补齐暂存的半个事件,再写扫描器释放的安全窗。
	if err := w.writeUnderlying(carry); err != nil {
		return err
	}
	if err := w.writeUnderlying(released); err != nil {
		return err
	}
	w.flushTransport()
	return nil
}

func (w *streamFlushWriter) flushTransport() {
	if w == nil || w.flusher == nil {
		return
	}
	w.flusher.Flush()
	w.mu.Lock()
	w.lastFlush = time.Now()
	w.mu.Unlock()
}
