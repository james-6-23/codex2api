package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type apiKeyModelRequestContextKey struct{}

// A charge identity belongs to one incoming request, not its transport or
// account attempts. It is never taken from a client-controlled request ID.
type apiKeyModelRequestQuota struct {
	db        *database.DB
	keyID     int64
	requestID string
	rules     []database.APIKeyModelRequestLimit
	mu        sync.Mutex
	admitted  bool
}

type apiKeyModelRequestQuotaError struct {
	status int
	apiErr *api.APIError
}

func (e *apiKeyModelRequestQuotaError) Error() string { return e.apiErr.Error() }

func apiKeyModelRequestError(err error) *apiKeyModelRequestQuotaError {
	var quotaErr *apiKeyModelRequestQuotaError
	if errors.As(err, &quotaErr) {
		return quotaErr
	}
	return nil
}

func (h *Handler) attachAPIKeyModelRequestQuota(c *gin.Context, newTurn bool) {
	if c == nil || c.Request == nil {
		return
	}
	if !newTurn && c.Request.Context().Value(apiKeyModelRequestContextKey{}) != nil {
		return
	}
	row := apiKeyRowFromContext(c)
	if row == nil {
		return
	}
	state := &apiKeyModelRequestQuota{
		db: h.db, keyID: row.ID, requestID: NewUpstreamSessionUUID(),
		rules: append([]database.APIKeyModelRequestLimit(nil), row.Limits.ModelRequestLimits...),
	}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), apiKeyModelRequestContextKey{}, state))
}

func (h *Handler) refreshAPIKeyModelRequestQuotaTurn(c *gin.Context) error {
	row := apiKeyRowFromContext(c)
	if row != nil && row.ID > 0 && h.db != nil {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		fresh, err := h.db.GetAPIKeyByID(ctx, row.ID)
		if err != nil {
			return &apiKeyModelRequestQuotaError{http.StatusServiceUnavailable,
				api.NewAPIError(api.ErrCodeServiceUnavailable, "Model request quota configuration is unavailable", api.ErrorTypeServer)}
		}
		// A long-lived socket must observe newly added budgets as well as limit
		// changes. Keep other existing connection admission behavior unchanged.
		frameRow := *row
		frameRow.Limits.ModelRequestLimits = fresh.Limits.ModelRequestLimits
		c.Set(contextAPIKeyRow, &frameRow)
		defer c.Set(contextAPIKeyRow, row)
	}
	h.attachAPIKeyModelRequestQuota(c, true)
	return nil
}

// ConsumeAPIKeyModelRequestQuota is called at the last local forwarding
// boundary, after model mapping and validation. A transport failure after this
// boundary is conservatively counted; retries share the durable charge ID.
func ConsumeAPIKeyModelRequestQuota(ctx context.Context, model string) error {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(apiKeyModelRequestContextKey{}).(*apiKeyModelRequestQuota)
	if state == nil || state.keyID <= 0 || (state.db == nil && len(state.rules) == 0) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.db == nil {
		return &apiKeyModelRequestQuotaError{http.StatusServiceUnavailable,
			api.NewAPIError(api.ErrCodeServiceUnavailable, "Model request quota storage is unavailable", api.ErrorTypeServer)}
	}
	quotaCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	hit, err := state.db.ConsumeAPIKeyModelRequest(quotaCtx, state.keyID, state.requestID, strings.TrimSpace(model), state.rules, time.Now())
	if err != nil {
		log.Printf("model request quota check failed (key_id=%d): %v", state.keyID, err)
		return &apiKeyModelRequestQuotaError{http.StatusServiceUnavailable,
			api.NewAPIError(api.ErrCodeServiceUnavailable, "Model request quota storage is unavailable; please retry later", api.ErrorTypeServer)}
	}
	if hit != nil {
		return &apiKeyModelRequestQuotaError{http.StatusTooManyRequests,
			api.NewAPIErrorWithDetails(api.ErrCodeRateLimitReached,
				fmt.Sprintf("API key weekly model request limit reached for %q (%d/%d)", hit.Model, hit.Used, hit.Limit),
				api.ErrorTypeRateLimit, hit.APIKeyModelRequestUsage)}
	}
	state.admitted = true
	return nil
}

// Keep retry heartbeats from committing HTTP 200 before the first quota
// admission. Requests without model budgets retain their existing behavior.
func apiKeyModelRequestAdmissionPending(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	state, _ := ctx.Value(apiKeyModelRequestContextKey{}).(*apiKeyModelRequestQuota)
	if state == nil || len(state.rules) == 0 {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return !state.admitted
}

func sendAPIKeyModelRequestQuotaError(c *gin.Context, err error) bool {
	quotaErr := apiKeyModelRequestError(err)
	if quotaErr == nil {
		return false
	}
	protocol := continuousRetryProtocolOpenAI
	if c.Request != nil {
		switch {
		case strings.HasSuffix(c.Request.URL.Path, "/messages"):
			protocol = continuousRetryProtocolAnthropic
		case strings.HasSuffix(c.Request.URL.Path, "/chat/completions"):
			protocol = continuousRetryProtocolChat
		case strings.HasSuffix(c.Request.URL.Path, "/responses"):
			protocol = continuousRetryProtocolResponses
		}
	}
	if !claimContinuousRetryTerminal(c, protocol) {
		return true
	}
	if details, ok := quotaErr.apiErr.Details.(database.APIKeyModelRequestUsage); ok {
		seconds := int64(time.Until(details.ResetAt).Seconds()) + 1
		if seconds < 1 {
			seconds = 1
		}
		c.Header("Retry-After", strconv.FormatInt(seconds, 10))
	}
	if c.Writer.Written() {
		// A later attempt can target a different model after an earlier attempt
		// already emitted a keepalive. Preserve quota details in the SSE error.
		payload, _ := json.Marshal(struct {
			Type  string        `json:"type"`
			Error *api.APIError `json:"error"`
		}{Type: "error", Error: quotaErr.apiErr})
		_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
		c.Writer.Flush()
		return true
	}
	api.SendErrorWithStatus(c, quotaErr.apiErr, quotaErr.status)
	return true
}
