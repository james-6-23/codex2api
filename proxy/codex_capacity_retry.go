package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func isCodexCapacityCodeOrMessage(code, message string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "server_is_overloaded", "slow_down":
		return true
	}
	message = strings.ToLower(message)
	return strings.Contains(message, "selected model is at capacity") ||
		strings.Contains(message, "model is at capacity. please try a different model")
}

func codexCapacityErrorForClient(body []byte) *api.APIError {
	if !gjson.ValidBytes(body) {
		return nil
	}
	parsed := gjson.ParseBytes(body)
	for _, path := range []string{"error", "response.error", "response.status_details.error", "detail", ""} {
		object := parsed
		if path != "" {
			object = object.Get(path)
		}
		code := strings.TrimSpace(object.Get("code").String())
		message := strings.TrimSpace(object.Get("message").String())
		if object.Type == gjson.String {
			message = strings.TrimSpace(object.String())
		}
		if !isCodexCapacityCodeOrMessage(code, message) {
			continue
		}
		errorType := strings.TrimSpace(object.Get("type").String())
		if errorType == "" {
			errorType = string(api.ErrorTypeUpstream)
		}
		if message == "" {
			message = code
		}
		return api.NewAPIError(api.ErrorCode(code), security.SafeTruncate(security.SanitizeLog(message), 600), api.ErrorType(errorType))
	}
	return nil
}

func codexCapacityRequestError(err error) *api.APIError {
	var upstreamError continuousRetryHTTPError
	if errors.As(err, &upstreamError) {
		return codexCapacityErrorForClient(upstreamError.UpstreamErrorBody())
	}
	return nil
}

func codexCapacityRetryDisabled(body []byte) bool {
	return !CurrentRuntimeSettings().CodexCapacityRetryEnabled && codexCapacityErrorForClient(body) != nil
}

func codexCapacityRequestRetryDisabled(err error) bool {
	return !CurrentRuntimeSettings().CodexCapacityRetryEnabled && codexCapacityRequestError(err) != nil
}

func writeCodexCapacityError(c *gin.Context, body []byte, protocol continuousRetryHTTPProtocol) bool {
	capacityError := codexCapacityErrorForClient(body)
	if capacityError == nil {
		return false
	}
	c.Writer.Header().Del("Retry-After")
	var envelope any = api.ErrorResponse{Error: *capacityError}
	if protocol == continuousRetryProtocolAnthropic {
		envelope = gin.H{"type": "error", "error": capacityError}
	}
	if !c.Writer.Written() {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadRequest, envelope)
		return true
	}
	if protocol == continuousRetryProtocolResponses {
		envelope = gin.H{"type": "response.failed", "response": gin.H{
			"created_at": time.Now().Unix(), "status": "failed", "error": capacityError,
		}}
	}
	payload, _ := json.Marshal(envelope)
	prefix := "data: "
	if protocol == continuousRetryProtocolAnthropic {
		prefix = "event: error\ndata: "
	}
	_, _ = c.Writer.WriteString(prefix + string(payload) + "\n\n")
	c.Writer.Flush()
	return true
}

func writeCommittedCodexCapacityError(c *gin.Context, protocol continuousRetryHTTPProtocol) bool {
	if c.Request == nil {
		return false
	}
	failure, exists := continuousRetryLastFailure(c.Request.Context())
	return exists && writeCodexCapacityError(c, failure.body, protocol)
}

func writeResponseFailedHTTPError(c *gin.Context, status int, body []byte, message string) {
	if writeCodexCapacityError(c, body, continuousRetryProtocolOpenAI) {
		return
	}
	c.JSON(status, gin.H{"error": gin.H{"message": message, "type": "upstream_error"}})
}
