package proxy

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const (
	contextInternalReason             = "internalReason"
	contextParentRequestID            = "parentRequestID"
	contextDisableOverflowAutoCompact = "disableOverflowAutoCompact"
	contextAPIKeyConcurrencyInherited = "apiKeyConcurrencyInherited"
	internalReasonOverflowCompact     = "overflow_compact_summary"
)

// internalResponseAttribution carries the safe subset of parent-request
// identity needed by an in-process Responses request. It deliberately omits
// the raw API key: the child request is already trusted and must not copy a
// credential into an Authorization header merely to preserve accounting.
type internalResponseAttribution struct {
	APIKeyID                     int64
	APIKeyName                   string
	APIKeyMasked                 string
	APIKeyRow                    *database.APIKeyRow
	ClientIP                     string
	ClientUserAgent              string
	Reason                       string
	ParentRequestID              string
	ShareParentAPIKeyConcurrency bool
}

func internalResponseAttributionFromRequest(c *gin.Context, reason string) *internalResponseAttribution {
	if c == nil {
		return nil
	}
	attribution := &internalResponseAttribution{
		APIKeyID:                     requestAPIKeyID(c),
		APIKeyRow:                    apiKeyRowFromContext(c),
		ClientIP:                     strings.TrimSpace(c.ClientIP()),
		Reason:                       strings.TrimSpace(reason),
		ShareParentAPIKeyConcurrency: true,
	}
	if c.Request != nil {
		attribution.ClientUserAgent = strings.TrimSpace(c.Request.UserAgent())
	}
	if value, exists := c.Get(contextAPIKeyName); exists {
		attribution.APIKeyName, _ = value.(string)
	}
	if value, exists := c.Get(contextAPIKeyMasked); exists {
		attribution.APIKeyMasked, _ = value.(string)
	}
	if c.Request != nil {
		attribution.ParentRequestID = snapshotUpstreamTrace(c.Request.Context()).RequestID
	}
	if attribution.ParentRequestID == "" {
		if requestContext := api.GetRequestContext(c); requestContext != nil {
			attribution.ParentRequestID = strings.TrimSpace(requestContext.RequestID)
		}
	}
	if attribution.ParentRequestID == "" {
		attribution.ParentRequestID = strings.TrimSpace(c.GetHeader("X-Request-ID"))
	}
	return attribution
}

func applyInternalResponseAttribution(c *gin.Context, req *http.Request, attribution *internalResponseAttribution) {
	if c == nil || req == nil || attribution == nil {
		return
	}
	if attribution.APIKeyID > 0 {
		c.Set(contextAPIKeyID, attribution.APIKeyID)
	}
	c.Set(contextAPIKeyName, strings.TrimSpace(attribution.APIKeyName))
	c.Set(contextAPIKeyMasked, strings.TrimSpace(attribution.APIKeyMasked))
	if attribution.APIKeyRow != nil {
		c.Set(contextAPIKeyRow, attribution.APIKeyRow)
	}
	if clientIP := strings.TrimSpace(attribution.ClientIP); clientIP != "" {
		req.RemoteAddr = net.JoinHostPort(clientIP, "0")
	}
	if userAgent := strings.TrimSpace(attribution.ClientUserAgent); userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if reason := strings.TrimSpace(attribution.Reason); reason != "" {
		c.Set(contextInternalReason, reason)
	}
	if parentRequestID := strings.TrimSpace(attribution.ParentRequestID); parentRequestID != "" {
		c.Set(contextParentRequestID, parentRequestID)
	}
	// Recursion prevention is explicit instead of relying on the historical
	// side effect that internal calls had no API-key identity.
	c.Set(contextDisableOverflowAutoCompact, true)
	if attribution.ShareParentAPIKeyConcurrency {
		c.Set(contextAPIKeyConcurrencyInherited, true)
	}
}

// ExecuteInternalResponse performs one Responses request through the configured
// account pool. It is intended for bounded administrative jobs such as prompt
// intelligence analysis and bypasses only the inbound prompt filter to avoid
// the defensive analysis prompt blocking itself.
func (h *Handler) ExecuteInternalResponse(ctx context.Context, body []byte) (int, []byte) {
	return h.executeInternalResponseWithAttribution(ctx, body, nil)
}

// executeInternalResponseWithAttribution performs an internal Responses call
// while preserving billing/routing identity from a parent request. The child
// still receives a fresh Gin context and never passes through inbound auth.
func (h *Handler) executeInternalResponseWithAttribution(ctx context.Context, body []byte, attribution *internalResponseAttribution) (int, []byte) {
	if ctx == nil {
		ctx = context.Background()
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("prompt_intelligence_internal", true)
	applyInternalResponseAttribution(c, req, attribution)
	h.Responses(c)
	return recorder.Code, recorder.Body.Bytes()
}
