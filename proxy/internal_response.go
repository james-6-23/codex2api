package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
)

// ExecuteInternalResponse performs one Responses request through the configured
// account pool. It is intended for bounded administrative jobs such as prompt
// intelligence analysis and bypasses only the inbound prompt filter to avoid
// the defensive analysis prompt blocking itself.
func (h *Handler) ExecuteInternalResponse(ctx context.Context, body []byte) (int, []byte) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("prompt_intelligence_internal", true)
	h.Responses(c)
	return recorder.Code, recorder.Body.Bytes()
}
