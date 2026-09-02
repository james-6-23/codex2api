package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const continuousRetryKeepaliveComment = ": keepalive\n\n"

var continuousRetryKeepaliveInterval = 15 * time.Second

type continuousRetryKeepalive interface {
	Activate()
	Active() bool
	Keepalive() error
}

type continuousRetryKeepaliveContextKey struct{}

type requestContinuousRetryKeepalive struct {
	active bool
	last   time.Time
	write  func() error
	cancel context.CancelCauseFunc
}

func (k *requestContinuousRetryKeepalive) Activate() {
	if k != nil && !k.active {
		k.active = true
		// Start the heartbeat window when unlimited retry actually begins.
		// Short backoff calls then accumulate toward the same deadline instead
		// of restarting a fresh interval on every retry.
		k.last = time.Now()
	}
}

func (k *requestContinuousRetryKeepalive) Active() bool {
	return k != nil && k.active
}

func (k *requestContinuousRetryKeepalive) Keepalive() error {
	if k == nil || !k.active || k.write == nil {
		return nil
	}
	if !k.last.IsZero() && time.Since(k.last) < continuousRetryKeepaliveInterval {
		return nil
	}
	if err := k.write(); err != nil {
		if k.cancel != nil {
			k.cancel(err)
		}
		return err
	}
	k.last = time.Now()
	return nil
}

func installContinuousRetrySSEKeepalive(c *gin.Context, stream bool, contentType string) func() {
	if c == nil || c.Request == nil || c.Writer == nil || !stream {
		return func() {}
	}
	if _, ok := c.Writer.(http.Flusher); !ok {
		return func() {}
	}
	if contentType == "" {
		contentType = "text/event-stream"
	}
	original := c.Request
	requestCtx, cancel := context.WithCancelCause(original.Context())
	keepalive := &requestContinuousRetryKeepalive{write: func() error {
		setSSEStreamHeaders(c, contentType)
		if _, err := c.Writer.WriteString(continuousRetryKeepaliveComment); err != nil {
			return err
		}
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}, cancel: cancel}
	c.Request = original.WithContext(context.WithValue(requestCtx, continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive)))
	return func() {
		cancel(nil)
		c.Request = original
	}
}

// installContinuousRetryHTTPInformationalKeepalive installs a non-committing
// keepalive for ordinary JSON endpoints. HTTP 102 is sent through the
// unwrapped net/http writer so the final JSON status/body can still be chosen
// later; calling Gin's WriteHeader or Flush here would commit an accidental
// 200 response. 非流式 JSON 不能插入 SSE 注释，因此用标准 HTTP 102
// Processing 保活，同时保留最终 JSON 的状态码和响应体语义。
func installContinuousRetryHTTPInformationalKeepalive(c *gin.Context) func() {
	if c == nil || c.Request == nil || c.Writer == nil || !c.Request.ProtoAtLeast(1, 1) {
		return func() {}
	}
	writer, ok := c.Writer.(http.ResponseWriter)
	if !ok {
		return func() {}
	}
	// Gin exposes one Unwrap layer, but middleware may add another. Resolve a
	// short chain while refusing to guess when the final writer is unknown.
	unwrapped := false
	for depth := 0; depth < 8; depth++ {
		unwrapper, canUnwrap := writer.(interface{ Unwrap() http.ResponseWriter })
		if !canUnwrap {
			break
		}
		next := unwrapper.Unwrap()
		if next == nil {
			return func() {}
		}
		writer = next
		unwrapped = true
	}
	if !unwrapped {
		return func() {}
	}
	if _, stillWrapped := writer.(interface{ Unwrap() http.ResponseWriter }); stillWrapped {
		return func() {}
	}
	original := c.Request
	requestCtx, cancel := context.WithCancelCause(original.Context())
	keepalive := &requestContinuousRetryKeepalive{
		write: func() error {
			// net/http flushes informational headers immediately. Do not call
			// Flush: it would implicitly send the final 200 status.
			writer.WriteHeader(http.StatusProcessing)
			return nil
		},
		cancel: cancel,
	}
	c.Request = original.WithContext(context.WithValue(requestCtx, continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive)))
	return func() {
		cancel(nil)
		c.Request = original
	}
}

func installContinuousRetryWSKeepalive(c *gin.Context, conn *websocket.Conn) func() {
	if c == nil || c.Request == nil || conn == nil {
		return func() {}
	}
	original := c.Request
	requestCtx, cancel := context.WithCancelCause(original.Context())
	keepalive := &requestContinuousRetryKeepalive{write: func() error {
		return conn.WriteControl(websocket.PingMessage, []byte("continuous-retry"), time.Now().Add(responsesWSWriteTimeout))
	}, cancel: cancel}
	c.Request = original.WithContext(context.WithValue(requestCtx, continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive)))
	return func() {
		cancel(nil)
		c.Request = original
	}
}

func setSSEStreamHeaders(c *gin.Context, contentType string) {
	if c == nil {
		return
	}
	if contentType == "" {
		contentType = "text/event-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
}

func continuousRetryKeepaliveForContext(ctx context.Context) continuousRetryKeepalive {
	if ctx == nil {
		return nil
	}
	keepalive, _ := ctx.Value(continuousRetryKeepaliveContextKey{}).(continuousRetryKeepalive)
	return keepalive
}

func activateContinuousRetryKeepalive(ctx context.Context) {
	if keepalive := continuousRetryKeepaliveForContext(ctx); keepalive != nil {
		keepalive.Activate()
	}
}

func activateContinuousRetryKeepaliveForLimit(ctx context.Context, retryLimit int) {
	if retryLimit == -1 {
		activateContinuousRetryKeepalive(ctx)
	}
}

func continuousRetryKeepaliveActive(ctx context.Context) bool {
	if keepalive := continuousRetryKeepaliveForContext(ctx); keepalive != nil {
		return keepalive.Active()
	}
	return false
}

func continuousRetryKeepaliveDelay(keepalive continuousRetryKeepalive) time.Duration {
	if continuousRetryKeepaliveInterval <= 0 {
		return 0
	}
	requestKeepalive, ok := keepalive.(*requestContinuousRetryKeepalive)
	if !ok || requestKeepalive.last.IsZero() {
		return continuousRetryKeepaliveInterval
	}
	delay := continuousRetryKeepaliveInterval - time.Since(requestKeepalive.last)
	if delay < 0 {
		return 0
	}
	return delay
}

func continuousRetryContextError(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

func stopContinuousRetryTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

type continuousRetryHTTPResult struct {
	response *http.Response
	err      error
}

// executeHTTPWithContinuousRetryKeepalive keeps the downstream alive while a
// retry attempt is waiting for upstream response headers. The worker never
// touches the downstream writer; that remains owned by the handler goroutine.
func executeHTTPWithContinuousRetryKeepalive(ctx context.Context, execute func() (*http.Response, error)) (*http.Response, error) {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if execute == nil {
		return nil, errors.New("nil upstream executor")
	}
	if keepalive == nil || !keepalive.Active() || continuousRetryKeepaliveInterval <= 0 {
		return execute()
	}

	result := make(chan continuousRetryHTTPResult)
	abandoned := make(chan struct{})
	go func() {
		response, err := execute()
		select {
		case result <- continuousRetryHTTPResult{response: response, err: err}:
		case <-abandoned:
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
		}
	}()
	defer close(abandoned)

	for {
		timer := time.NewTimer(continuousRetryKeepaliveDelay(keepalive))
		select {
		case callResult := <-result:
			stopContinuousRetryTimer(timer)
			return callResult.response, callResult.err
		case <-timer.C:
			if err := keepalive.Keepalive(); err != nil {
				return nil, err
			}
		case <-ctx.Done():
			stopContinuousRetryTimer(timer)
			return nil, continuousRetryContextError(ctx)
		}
	}
}

type continuousRetryReadResult struct {
	data []byte
	err  error
}

// readAllWithContinuousRetryKeepalive keeps an active JSON retry alive while
// the upstream response body is still being read. The worker owns the read;
// the caller goroutine remains the sole downstream writer. 上游 JSON body
// 读取也要纳入保活窗口，避免已进入持续重试后长时间卡在读体阶段无任何字节。
func readAllWithContinuousRetryKeepalive(ctx context.Context, reader io.Reader) ([]byte, error) {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil || !keepalive.Active() || continuousRetryKeepaliveInterval <= 0 {
		return io.ReadAll(reader)
	}
	result := make(chan continuousRetryReadResult, 1)
	go func() {
		data, err := io.ReadAll(reader)
		result <- continuousRetryReadResult{data: data, err: err}
	}()
	for {
		delay := continuousRetryKeepaliveDelay(keepalive)
		if delay <= 0 {
			if err := keepalive.Keepalive(); err != nil {
				return nil, err
			}
			delay = continuousRetryKeepaliveDelay(keepalive)
			if delay <= 0 {
				delay = continuousRetryKeepaliveInterval
			}
		}
		timer := time.NewTimer(delay)
		select {
		case callResult := <-result:
			stopContinuousRetryTimer(timer)
			return callResult.data, callResult.err
		case <-timer.C:
			if err := keepalive.Keepalive(); err != nil {
				return nil, err
			}
		case <-ctx.Done():
			stopContinuousRetryTimer(timer)
			return nil, continuousRetryContextError(ctx)
		}
	}
}

type continuousRetryStreamItem[T any] struct {
	value T
	ack   chan bool
}

// readStreamWithContinuousRetryKeepalive pumps upstream reads through the
// handler goroutine. That goroutine remains the sole downstream writer and can
// therefore emit heartbeats without racing real SSE or WebSocket output.
func readStreamWithContinuousRetryKeepalive[T any](ctx context.Context, read func(func(T) bool) error, callback func(T) bool) error {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil || !keepalive.Active() || continuousRetryKeepaliveInterval <= 0 {
		return read(callback)
	}

	items := make(chan continuousRetryStreamItem[T])
	done := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		done <- read(func(value T) bool {
			ack := make(chan bool, 1)
			select {
			case items <- continuousRetryStreamItem[T]{value: value, ack: ack}:
			case <-stop:
				return false
			case <-ctx.Done():
				return false
			}
			select {
			case keepReading := <-ack:
				return keepReading
			case <-stop:
				return false
			case <-ctx.Done():
				return false
			}
		})
	}()
	defer close(stop)

	for {
		timer := time.NewTimer(continuousRetryKeepaliveDelay(keepalive))
		select {
		case item := <-items:
			stopContinuousRetryTimer(timer)
			keepReading := callback(item.value)
			item.ack <- keepReading
			if !keepReading {
				return <-done
			}
		case err := <-done:
			stopContinuousRetryTimer(timer)
			return err
		case <-timer.C:
			if err := keepalive.Keepalive(); err != nil {
				return err
			}
		case <-ctx.Done():
			stopContinuousRetryTimer(timer)
			return continuousRetryContextError(ctx)
		}
	}
}

type continuousRetrySSEEvent struct {
	event string
	data  []byte
}

func readSSEStreamWithContinuousRetryKeepalive(ctx context.Context, body io.Reader, callback func(event string, data []byte) bool) error {
	return readStreamWithContinuousRetryKeepalive(ctx, func(yield func(continuousRetrySSEEvent) bool) error {
		return ReadSSEStreamWithEvent(body, func(event string, data []byte) bool {
			return yield(continuousRetrySSEEvent{event: event, data: data})
		})
	}, func(item continuousRetrySSEEvent) bool {
		return callback(item.event, item.data)
	})
}

func readRawGrokSSEFramesWithContinuousRetryKeepalive(ctx context.Context, body io.Reader, callback func(rawGrokSSEFrame) bool) error {
	return readStreamWithContinuousRetryKeepalive(ctx, func(yield func(rawGrokSSEFrame) bool) error {
		return readRawGrokSSEFrames(body, yield)
	}, callback)
}

// waitWithContinuousRetryKeepalive waits in short chunks only after a request
// has entered the unlimited retry path. It keeps the write on the handler
// goroutine, so it never races normal stream output. A failed heartbeat is a
// downstream write failure and stops the retry immediately.
func waitWithContinuousRetryKeepalive(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil || continuousRetryKeepaliveInterval <= 0 {
		return waitForRetryInterval(ctx, interval)
	}
	keepalive.Activate()
	remaining := interval
	for remaining > 0 {
		step := continuousRetryKeepaliveDelay(keepalive)
		if step <= 0 {
			if err := keepalive.Keepalive(); err != nil {
				return false
			}
			// A disabled or custom heartbeat may not advance its deadline.
			// 心跳未推进截止时间时必须实际等待，避免处理协程零延迟忙等。
			step = continuousRetryKeepaliveDelay(keepalive)
			if step <= 0 {
				step = continuousRetryKeepaliveInterval
			}
		}
		if step > remaining {
			step = remaining
		}
		if !waitForRetryInterval(ctx, step) {
			return false
		}
		remaining -= step
		if err := keepalive.Keepalive(); err != nil {
			return false
		}
	}
	return true
}

func waitForRetryInterval(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	if ctx == nil {
		time.Sleep(interval)
		return true
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// retryKeepaliveCommitted reports that a retry heartbeat has already committed
// the streaming HTTP response. Callers must send a protocol event from that
// point on; appending a fresh JSON HTTP response would corrupt the stream.
func retryKeepaliveCommitted(c *gin.Context) bool {
	return c != nil && c.Writer != nil && c.Writer.Written()
}

func writeCommittedResponsesRetryError(c *gin.Context, message string) bool {
	if !retryKeepaliveCommitted(c) {
		if c != nil && c.Request != nil && !claimContinuousRetryTerminal(c, continuousRetryProtocolResponses) {
			return true
		}
		return false
	}
	timedOut := c.Request != nil && continuousRetryDeadlineExceeded(c.Request.Context())
	if !timedOut && c.Request != nil && !claimContinuousRetrySuccessContext(c.Request.Context()) {
		timedOut = true
	}
	if c.Request != nil && c.Request.Context().Err() != nil && !timedOut {
		return true
	}
	code := "upstream_error"
	if timedOut {
		c.Set(continuousRetryTimeoutWrittenKey, true)
		if failure, ok := continuousRetryLastFailure(c.Request.Context()); ok {
			writeContinuousRetryLastFailure(c, continuousRetryProtocolResponses, failure)
			return true
		}
		code = ErrorCodeUpstreamTimeout
		message = continuousRetryTimeoutMessage
	}
	payload, _ := json.Marshal(gin.H{
		"type": "response.failed",
		"response": gin.H{
			"created_at": time.Now().Unix(),
			"status":     "failed",
			"error":      gin.H{"message": message, "type": "upstream_error", "code": code},
		},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeCommittedChatRetryError(c *gin.Context, message string) bool {
	if !retryKeepaliveCommitted(c) {
		if c != nil && c.Request != nil && !claimContinuousRetryTerminal(c, continuousRetryProtocolChat) {
			return true
		}
		return false
	}
	timedOut := c.Request != nil && continuousRetryDeadlineExceeded(c.Request.Context())
	if !timedOut && c.Request != nil && !claimContinuousRetrySuccessContext(c.Request.Context()) {
		timedOut = true
	}
	if c.Request != nil && c.Request.Context().Err() != nil && !timedOut {
		return true
	}
	code := ErrorCodeUpstreamStreamBreak
	if timedOut {
		c.Set(continuousRetryTimeoutWrittenKey, true)
		if failure, ok := continuousRetryLastFailure(c.Request.Context()); ok {
			writeContinuousRetryLastFailure(c, continuousRetryProtocolChat, failure)
			return true
		}
		code = ErrorCodeUpstreamTimeout
		message = continuousRetryTimeoutMessage
	}
	payload, _ := json.Marshal(gin.H{
		"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": code},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeCommittedAnthropicRetryError(c *gin.Context, errorType, message string) bool {
	if !retryKeepaliveCommitted(c) {
		if c != nil && c.Request != nil && !claimContinuousRetryTerminal(c, continuousRetryProtocolAnthropic) {
			return true
		}
		return false
	}
	timedOut := c.Request != nil && continuousRetryDeadlineExceeded(c.Request.Context())
	if !timedOut && c.Request != nil && !claimContinuousRetrySuccessContext(c.Request.Context()) {
		timedOut = true
	}
	if c.Request != nil && c.Request.Context().Err() != nil && !timedOut {
		return true
	}
	if timedOut {
		c.Set(continuousRetryTimeoutWrittenKey, true)
		if failure, ok := continuousRetryLastFailure(c.Request.Context()); ok {
			writeContinuousRetryLastFailure(c, continuousRetryProtocolAnthropic, failure)
			return true
		}
		errorType = "api_error"
		message = continuousRetryTimeoutMessage
	}
	payload, _ := json.Marshal(gin.H{
		"type":  "error",
		"error": gin.H{"type": errorType, "message": message},
	})
	_, _ = c.Writer.WriteString("event: error\ndata: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

// Replay can fail before a retry heartbeat commits HTTP. These helpers write a
// protocol terminal to the real writer in either state: normal JSON after SSE
// would corrupt the stream, while silence would look like success.
// 回放可能在重试心跳提交 HTTP 前失败；这些 helper 会在两种状态下都向真实 writer
// 写入协议终态，避免在 SSE 后追加普通 JSON 或静默返回造成错误语义。
func writeContinuousRetryLocalResponsesError(c *gin.Context) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	if !c.Writer.Written() {
		c.Status(http.StatusInternalServerError)
	}
	payload, _ := json.Marshal(gin.H{
		"type": "response.failed",
		"response": gin.H{
			"created_at": time.Now().Unix(),
			"status":     "failed",
			"error": gin.H{
				"message": continuousRetryLocalFailureMessage,
				"type":    "server_error",
				"code":    ErrorCodeInternalError,
			},
		},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeContinuousRetryLocalChatError(c *gin.Context) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	if !c.Writer.Written() {
		c.Status(http.StatusInternalServerError)
	}
	payload, _ := json.Marshal(gin.H{
		"error": gin.H{
			"message": continuousRetryLocalFailureMessage,
			"type":    "server_error",
			"code":    ErrorCodeInternalError,
		},
	})
	_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

func writeContinuousRetryLocalAnthropicError(c *gin.Context) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	if !c.Writer.Written() {
		c.Status(http.StatusInternalServerError)
	}
	payload, _ := json.Marshal(gin.H{
		"type": "error",
		"error": gin.H{
			"type":    "api_error",
			"message": continuousRetryLocalFailureMessage,
		},
	})
	_, _ = c.Writer.WriteString("event: error\ndata: " + string(payload) + "\n\n")
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

// abortContinuousRetryCommitFailure closes a winning attempt that could not
// be replayed to the downstream. Replay/storage/write failures are local
// proxy failures, not upstream failures: retrying would only duplicate a paid
// request and could turn a broken client or filesystem into an infinite loop.
func abortContinuousRetryCommitFailure(h *Handler, account *auth.Account, resp *http.Response, attempt *continuousRetryStreamAttempt) {
	if attempt != nil {
		_ = attempt.Close()
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if h != nil && h.store != nil && account != nil {
		h.store.Release(account)
	}
	log.Printf("continuous retry replay commit failed; request aborted")
}

func continuousRetryRequestErrorMessage(err error) string {
	var structured *Error
	if errors.As(err, &structured) && structured != nil && structured.Message != "" {
		return structured.Message
	}
	return "Upstream request failed"
}
