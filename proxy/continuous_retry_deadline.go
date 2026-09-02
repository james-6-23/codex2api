package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

var errContinuousRetryDeadlineExceeded = errors.New("continuous retry time limit exceeded")

const continuousRetryTimeoutMessage = "Continuous retry time limit exceeded"

const continuousRetryTimeoutWrittenKey = "continuous_retry_timeout_written"

type continuousRetryDeadlineContextKey struct{}

type continuousRetryDeadline struct {
	duration        time.Duration
	cancel          context.CancelCauseFunc
	once            sync.Once
	mu              sync.Mutex
	timer           *time.Timer
	stopped         bool
	fired           bool
	succeeded       bool
	lastFailure     continuousRetryFailure
	hasFailure      bool
	timeoutCleanups []func()
}

type continuousRetryFailure struct {
	status      int
	body        []byte
	contentType string
}

func (d *continuousRetryDeadline) Activate() {
	if d == nil || d.duration <= 0 || d.cancel == nil {
		return
	}
	d.once.Do(func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.stopped {
			return
		}
		d.timer = time.AfterFunc(d.duration, func() {
			d.mu.Lock()
			if d.stopped || d.succeeded {
				d.mu.Unlock()
				return
			}
			d.stopped = true
			d.fired = true
			cleanups := append([]func(){}, d.timeoutCleanups...)
			d.timeoutCleanups = nil
			d.cancel(errContinuousRetryDeadlineExceeded)
			d.mu.Unlock()
			for _, cleanup := range cleanups {
				cleanup()
			}
		})
	})
}

// ClaimSuccess makes the successful logical request and the retry deadline
// compete under the same lock. Once it returns true the timer cannot fire, so
// callers may safely publish output and perform success-only side effects.
func (d *continuousRetryDeadline) ClaimSuccess() bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fired {
		return false
	}
	if d.succeeded {
		return true
	}
	d.succeeded = true
	d.stopped = true
	d.timeoutCleanups = nil
	if d.timer != nil {
		d.timer.Stop()
	}
	return true
}

func (d *continuousRetryDeadline) Stop() {
	if d == nil {
		return
	}
	d.Settle()
}

// Settle atomically prevents a pending timer or observes that it already won.
// It closes the cleanup check-then-stop window at handler return.
func (d *continuousRetryDeadline) Settle() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	if !d.fired {
		d.stopped = true
		if d.timer != nil {
			d.timer.Stop()
		}
	}
	fired := d.fired
	d.mu.Unlock()
	return fired
}

func continuousRetryDeadlineForContext(ctx context.Context) *continuousRetryDeadline {
	if ctx == nil {
		return nil
	}
	deadline, _ := ctx.Value(continuousRetryDeadlineContextKey{}).(*continuousRetryDeadline)
	return deadline
}

func activateContinuousRetryDeadlineForLimit(ctx context.Context, retryLimit int) {
	if retryLimit != -1 {
		return
	}
	if deadline := continuousRetryDeadlineForContext(ctx); deadline != nil {
		deadline.Activate()
	}
}

func continuousRetryDeadlineExceeded(ctx context.Context) bool {
	return ctx != nil && errors.Is(context.Cause(ctx), errContinuousRetryDeadlineExceeded)
}

func continuousRetryDeadlineActive(ctx context.Context) bool {
	deadline := continuousRetryDeadlineForContext(ctx)
	if deadline == nil {
		return false
	}
	deadline.mu.Lock()
	defer deadline.mu.Unlock()
	return deadline.timer != nil && !deadline.stopped && !deadline.fired && !deadline.succeeded
}

func settleContinuousRetryDeadline(ctx context.Context) bool {
	deadline := continuousRetryDeadlineForContext(ctx)
	if deadline == nil {
		return false
	}
	return deadline.Settle() || continuousRetryDeadlineExceeded(ctx)
}

func withContinuousRetryDeadlinePending(ctx context.Context, action func()) bool {
	deadline := continuousRetryDeadlineForContext(ctx)
	if deadline == nil {
		action()
		return true
	}
	deadline.mu.Lock()
	if deadline.fired {
		deadline.mu.Unlock()
		return false
	}
	// Do not hold the deadline mutex while running account/database side effects:
	// the timer must be able to cancel the request at the wall-clock boundary.
	deadline.mu.Unlock()
	action()
	deadline.mu.Lock()
	fired := deadline.fired
	deadline.mu.Unlock()
	return !fired
}

func withContinuousRetryDeadlinePendingCleanup(ctx context.Context, action, cleanup func()) bool {
	deadline := continuousRetryDeadlineForContext(ctx)
	if deadline == nil {
		action()
		return true
	}
	deadline.mu.Lock()
	if deadline.fired {
		deadline.mu.Unlock()
		return false
	}
	// Side effects may touch the store or database. Keep them outside the mutex
	// so the timer can fire and cancel an over-budget request concurrently.
	deadline.mu.Unlock()
	action()
	deadline.mu.Lock()
	fired := deadline.fired
	succeeded := deadline.succeeded
	if !fired && !succeeded && cleanup != nil {
		deadline.timeoutCleanups = append(deadline.timeoutCleanups, cleanup)
	}
	deadline.mu.Unlock()
	if fired && !succeeded && cleanup != nil {
		cleanup()
	}
	return !fired
}

func bindContinuousRetrySessionAffinity(ctx context.Context, store *auth.Store, key string, account *auth.Account, proxyURL string) bool {
	return bindContinuousRetrySessionAffinityWithGuard(ctx, store, key, account, proxyURL, auth.SessionAffinityGuard{})
}

func bindContinuousRetrySessionAffinityWithGuard(ctx context.Context, store *auth.Store, key string, account *auth.Account, proxyURL string, guard auth.SessionAffinityGuard) bool {
	if store == nil || account == nil {
		return false
	}
	if guard.PreservesExisting() {
		return withContinuousRetryDeadlinePendingCleanup(ctx, func() {}, nil)
	}
	preserveExisting := false
	return withContinuousRetryDeadlinePendingCleanup(ctx, func() {
		boundID, bound := store.SessionAffinityAccountID(key)
		preserveExisting = bound && boundID == account.ID()
		store.BindSessionAffinityWithGuard(key, account, proxyURL, guard)
	}, func() {
		if !preserveExisting {
			store.UnbindSessionAffinity(key, account.ID())
		}
	})
}

func rememberContinuousRetryHTTPFailure(ctx context.Context, resp *http.Response, body []byte) {
	if resp == nil || resp.StatusCode == http.StatusOK {
		return
	}
	rememberContinuousRetryFailure(ctx, continuousRetryFailure{
		status:      resp.StatusCode,
		body:        body,
		contentType: resp.Header.Get("Content-Type"),
	})
}

func rememberContinuousRetryRequestFailure(ctx context.Context, err error) {
	if err == nil || continuousRetryDeadlineExceeded(ctx) {
		return
	}
	if status, body, ok := continuousRetryHTTPErrorDetails(err); ok {
		contentType := ""
		if json.Valid(body) {
			contentType = "application/json"
		}
		rememberContinuousRetryFailure(ctx, continuousRetryFailure{status: status, body: body, contentType: contentType})
		return
	}
	message := continuousRetryRequestErrorMessage(err)
	body, _ := json.Marshal(gin.H{"error": gin.H{
		"message": message,
		"type":    ErrorTypeUpstreamError,
		"code":    "upstream_502",
	}})
	rememberContinuousRetryFailure(ctx, continuousRetryFailure{
		status:      http.StatusBadGateway,
		body:        body,
		contentType: "application/json",
	})
}

func rememberContinuousRetryStreamFailure(ctx context.Context, outcome streamOutcome, body []byte) {
	if outcome.logStatusCode == http.StatusOK || continuousRetryDeadlineExceeded(ctx) {
		return
	}
	status := safeGrokNativeHTTPStatus(outcome.logStatusCode)
	if len(body) == 0 || !json.Valid(body) {
		body, _ = json.Marshal(gin.H{"error": gin.H{
			"message": outcome.failureMessage,
			"type":    ErrorTypeUpstreamError,
			"code":    fmt.Sprintf("upstream_%d", status),
		}})
	}
	rememberContinuousRetryFailure(ctx, continuousRetryFailure{
		status:      status,
		body:        body,
		contentType: "application/json",
	})
}

func rememberContinuousRetryFailure(ctx context.Context, failure continuousRetryFailure) {
	deadline := continuousRetryDeadlineForContext(ctx)
	if deadline == nil || failure.status == http.StatusOK {
		return
	}
	deadline.mu.Lock()
	if deadline.fired {
		deadline.mu.Unlock()
		return
	}
	if len(failure.body) == 0 && deadline.hasFailure && deadline.lastFailure.status == failure.status && len(deadline.lastFailure.body) > 0 {
		deadline.mu.Unlock()
		return
	}
	deadline.lastFailure = continuousRetryFailure{
		status:      failure.status,
		body:        append([]byte(nil), failure.body...),
		contentType: failure.contentType,
	}
	deadline.hasFailure = true
	deadline.mu.Unlock()
}

func continuousRetryLastFailure(ctx context.Context) (continuousRetryFailure, bool) {
	deadline := continuousRetryDeadlineForContext(ctx)
	if deadline == nil {
		return continuousRetryFailure{}, false
	}
	deadline.mu.Lock()
	defer deadline.mu.Unlock()
	if !deadline.hasFailure {
		return continuousRetryFailure{}, false
	}
	failure := deadline.lastFailure
	failure.body = append([]byte(nil), failure.body...)
	return failure, true
}

type continuousRetryHTTPProtocol int

const (
	continuousRetryProtocolOpenAI continuousRetryHTTPProtocol = iota
	continuousRetryProtocolResponses
	continuousRetryProtocolChat
	continuousRetryProtocolAnthropic
)

func installContinuousRetryHTTPDeadline(c *gin.Context, policy database.ContinuousRetryPolicy, protocol continuousRetryHTTPProtocol) func() {
	stop := installContinuousRetryDeadlineContext(c, policy)
	if c == nil || c.Request == nil {
		return stop
	}
	return func() {
		if settleContinuousRetryDeadline(c.Request.Context()) {
			writeContinuousRetryTimeoutResponse(c, protocol)
		}
		stop()
	}
}

func installContinuousRetryDeadlineContext(c *gin.Context, policy database.ContinuousRetryPolicy) func() {
	if c == nil || c.Request == nil {
		return func() {}
	}
	policy = database.NormalizeContinuousRetryPolicy(policy)
	if !policy.Enabled {
		return func() {}
	}
	original := c.Request
	requestCtx, cancel := context.WithCancelCause(original.Context())
	deadline := &continuousRetryDeadline{duration: time.Duration(policy.MaxDurationSeconds) * time.Second, cancel: cancel}
	c.Request = original.WithContext(context.WithValue(requestCtx, continuousRetryDeadlineContextKey{}, deadline))
	return func() {
		deadline.Stop()
		cancel(nil)
		c.Request = original
	}
}

func writeContinuousRetryTimeoutResponse(c *gin.Context, protocol continuousRetryHTTPProtocol) bool {
	if c == nil || c.Request == nil || !continuousRetryDeadlineExceeded(c.Request.Context()) {
		return false
	}
	if written, ok := c.Get(continuousRetryTimeoutWrittenKey); ok && written == true {
		return true
	}
	c.Set(continuousRetryTimeoutWrittenKey, true)
	if failure, ok := continuousRetryLastFailure(c.Request.Context()); ok {
		writeContinuousRetryLastFailure(c, protocol, failure)
		return true
	}
	if retryKeepaliveCommitted(c) {
		switch protocol {
		case continuousRetryProtocolResponses:
			return writeCommittedResponsesRetryError(c, continuousRetryTimeoutMessage)
		case continuousRetryProtocolChat:
			return writeCommittedChatRetryError(c, continuousRetryTimeoutMessage)
		case continuousRetryProtocolAnthropic:
			return writeCommittedAnthropicRetryError(c, "api_error", continuousRetryTimeoutMessage)
		}
		return true
	}
	if c.Writer != nil && c.Writer.Written() {
		return true
	}
	if protocol == continuousRetryProtocolAnthropic {
		c.JSON(http.StatusGatewayTimeout, gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": continuousRetryTimeoutMessage}})
		return true
	}
	c.JSON(http.StatusGatewayTimeout, ErrUpstreamTimeout(errContinuousRetryDeadlineExceeded).ToGinH())
	return true
}

func writeContinuousRetryLastFailure(c *gin.Context, protocol continuousRetryHTTPProtocol, failure continuousRetryFailure) {
	status := failure.status
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	message := usageLogErrorMessage(status, failure.body)
	if message == "" {
		message = fmt.Sprintf("Upstream returned HTTP %d", status)
	}
	code := fmt.Sprintf("upstream_%d", status)
	if retryKeepaliveCommitted(c) {
		var payload []byte
		switch protocol {
		case continuousRetryProtocolAnthropic:
			payload, _ = json.Marshal(gin.H{"type": "error", "error": gin.H{"type": mapHTTPStatusToAnthropicError(status), "message": message}})
			_, _ = c.Writer.WriteString("event: error\ndata: " + string(payload) + "\n\n")
		case continuousRetryProtocolChat:
			payload, _ = json.Marshal(gin.H{"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": code}})
			_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
		default:
			payload, _ = json.Marshal(gin.H{"type": "response.failed", "response": gin.H{"created_at": time.Now().Unix(), "status": "failed", "error": gin.H{"message": message, "type": "upstream_error", "code": code}}})
			_, _ = c.Writer.WriteString("data: " + string(payload) + "\n\n")
		}
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	if protocol == continuousRetryProtocolAnthropic {
		c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": mapHTTPStatusToAnthropicError(status), "message": message}})
		return
	}
	if len(failure.body) > 0 && json.Valid(failure.body) {
		contentType := failure.contentType
		if contentType == "" {
			contentType = "application/json"
		}
		c.Data(status, contentType, failure.body)
		return
	}
	c.JSON(status, gin.H{"error": gin.H{"message": message, "type": ErrorTypeUpstreamError, "code": code}})
}

func continuousRetryCommitExpired(c *gin.Context, protocol continuousRetryHTTPProtocol) bool {
	if c == nil || c.Request == nil || !continuousRetryDeadlineExceeded(c.Request.Context()) {
		return false
	}
	writeContinuousRetryTimeoutResponse(c, protocol)
	return true
}

func claimContinuousRetrySuccess(c *gin.Context, protocol continuousRetryHTTPProtocol) bool {
	return claimContinuousRetryTerminal(c, protocol)
}

func claimContinuousRetryTerminal(c *gin.Context, protocol continuousRetryHTTPProtocol) bool {
	if c == nil || c.Request == nil {
		return true
	}
	if claimContinuousRetrySuccessContext(c.Request.Context()) {
		return true
	}
	writeContinuousRetryTimeoutResponse(c, protocol)
	return false
}

func claimContinuousRetrySuccessContext(ctx context.Context) bool {
	if continuousRetryDeadlineExceeded(ctx) {
		return false
	}
	deadline := continuousRetryDeadlineForContext(ctx)
	return deadline == nil || deadline.ClaimSuccess()
}
