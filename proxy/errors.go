package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ==================== Error Type Constants ====================

const (
	// Error type categories
	ErrorTypeAuthentication = "authentication_error"
	ErrorTypeInvalidRequest = "invalid_request_error"
	ErrorTypeServerError    = "server_error"
	ErrorTypeUpstreamError  = "upstream_error"
	ErrorTypeRateLimitError = "rate_limit_error"
)

// ==================== Error Code Constants ====================

const (
	// Authentication errors
	ErrorCodeMissingAPIKey = "missing_api_key"
	ErrorCodeInvalidAPIKey = "invalid_api_key"

	// Rate limiting errors
	ErrorCodeRateLimited           = "rate_limited"
	ErrorCodeAccountPoolUsageLimit = "account_pool_usage_limit_reached"

	// Upstream errors
	ErrorCodeUpstreamError       = "upstream_error"
	ErrorCodeUpstreamTimeout     = "upstream_timeout"
	ErrorCodeUpstreamStreamBreak = "upstream_stream_break"

	// Server errors
	ErrorCodeNoAvailableAccount = "no_available_account"
	ErrorCodeInternalError      = "internal_error"

	// Request errors
	ErrorCodeBadRequest   = "bad_request"
	ErrorCodeMissingModel = "missing_model"
)

// ==================== Error Struct ====================

// Error represents a structured API error with full context
type Error struct {
	// Code is a machine-readable error code (e.g., "missing_api_key")
	Code string

	// Message is a human-readable error message
	Message string

	// Type is the error category (e.g., "authentication_error")
	Type string

	// Retryable indicates whether the client can retry the request
	Retryable bool

	// HTTPStatus is the HTTP status code to return
	HTTPStatus int

	// Cause is the underlying error (for error chain)
	Cause error
}

// Error implements the error interface
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (caused by: %v)", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap implements the errors.Unwrap interface for error chain traversal
func (e *Error) Unwrap() error {
	return e.Cause
}

// StatusCode returns the HTTP status code for this error
func (e *Error) StatusCode() int {
	return e.HTTPStatus
}

// UpstreamStatusCode/UpstreamErrorBody expose the upstream dimensions needed
// by opt-in retry selectors when an upstream failure arrives as an error
// rather than a normal HTTP response (for example a WebSocket handshake).
// They intentionally use distinct names so the existing StatusCode API stays
// unchanged.
func (e *Error) UpstreamStatusCode() int {
	if e == nil || e.Type != ErrorTypeUpstreamError || e.HTTPStatus < 100 || e.HTTPStatus > 999 {
		return 0
	}
	return e.HTTPStatus
}

func (e *Error) UpstreamErrorBody() []byte {
	if e == nil || e.Type != ErrorTypeUpstreamError {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"error": map[string]string{"code": e.Code, "type": e.Type, "message": e.Message},
	})
	if err != nil {
		return nil
	}
	return body
}

// ToGinH converts the error to a gin.H map for JSON response
// Format matches OpenAI API error response format
func (e *Error) ToGinH() gin.H {
	errInfo := gin.H{
		"message": e.Message,
		"type":    e.Type,
		"code":    e.Code,
	}
	return gin.H{"error": errInfo}
}

// ==================== Constructor Functions ====================

// ErrMissingAPIKey creates a missing API key error
func ErrMissingAPIKey() *Error {
	return &Error{
		Code:       ErrorCodeMissingAPIKey,
		Message:    "Missing Authorization header",
		Type:       ErrorTypeAuthentication,
		Retryable:  false,
		HTTPStatus: http.StatusUnauthorized,
	}
}

// ErrInvalidAPIKey creates an invalid API key error
func ErrInvalidAPIKey() *Error {
	return &Error{
		Code:       ErrorCodeInvalidAPIKey,
		Message:    "Invalid API Key",
		Type:       ErrorTypeAuthentication,
		Retryable:  false,
		HTTPStatus: http.StatusUnauthorized,
	}
}

// ErrRateLimited creates a rate limited error with optional cooldown info
func ErrRateLimited(message string) *Error {
	if message == "" {
		message = "Rate limit exceeded"
	}
	return &Error{
		Code:       ErrorCodeRateLimited,
		Message:    message,
		Type:       ErrorTypeRateLimitError,
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
	}
}

// ErrAccountPoolUsageLimit creates an account pool usage limit error
func ErrAccountPoolUsageLimit(message string, planType string, resetsAt int64, resetsInSeconds int64) *Error {
	if message == "" {
		message = "Account pool quota exhausted, please retry later"
	}
	// Include plan type and reset info if provided
	if planType != "" {
		message = fmt.Sprintf("%s (plan: %s)", message, planType)
	}
	if resetsInSeconds > 0 {
		message = fmt.Sprintf("%s, resets in %d seconds", message, resetsInSeconds)
	}
	return &Error{
		Code:       ErrorCodeAccountPoolUsageLimit,
		Message:    message,
		Type:       ErrorTypeServerError,
		Retryable:  true,
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

// ErrUpstream creates an upstream error with cause
func ErrUpstream(statusCode int, message string, cause error) *Error {
	if message == "" {
		message = fmt.Sprintf("Upstream request failed (status %d)", statusCode)
	}
	return &Error{
		Code:    ErrorCodeUpstreamError,
		Message: message,
		Type:    ErrorTypeUpstreamError,
		Retryable: statusCode == http.StatusTooManyRequests ||
			statusCode == http.StatusInternalServerError ||
			statusCode == http.StatusServiceUnavailable,
		HTTPStatus: statusCode,
		Cause:      cause,
	}
}

// ErrUpstreamTimeout creates an upstream timeout error
func ErrUpstreamTimeout(cause error) *Error {
	return &Error{
		Code:       ErrorCodeUpstreamTimeout,
		Message:    "Upstream request timeout",
		Type:       ErrorTypeUpstreamError,
		Retryable:  true,
		HTTPStatus: http.StatusGatewayTimeout,
		Cause:      cause,
	}
}

// ErrUpstreamStreamBreak creates an upstream stream break error
func ErrUpstreamStreamBreak(message string) *Error {
	if message == "" {
		message = "Upstream stream ended prematurely"
	}
	return &Error{
		Code:       ErrorCodeUpstreamStreamBreak,
		Message:    message,
		Type:       ErrorTypeUpstreamError,
		Retryable:  true,
		HTTPStatus: http.StatusBadGateway,
	}
}

// ErrNoAvailableAccount creates a no available account error
func ErrNoAvailableAccount() *Error {
	return &Error{
		Code:       ErrorCodeNoAvailableAccount,
		Message:    "No available account, please retry later",
		Type:       ErrorTypeServerError,
		Retryable:  true,
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

// ErrInternalError creates an internal server error
func ErrInternalError(message string, cause error) *Error {
	if message == "" {
		message = "Internal server error"
	}
	return &Error{
		Code:       ErrorCodeInternalError,
		Message:    message,
		Type:       ErrorTypeServerError,
		Retryable:  false,
		HTTPStatus: http.StatusInternalServerError,
		Cause:      cause,
	}
}

// ErrBadRequest creates a bad request error
func ErrBadRequest(message string) *Error {
	if message == "" {
		message = "Invalid request"
	}
	return &Error{
		Code:       ErrorCodeBadRequest,
		Message:    message,
		Type:       ErrorTypeInvalidRequest,
		Retryable:  false,
		HTTPStatus: http.StatusBadRequest,
	}
}

// ErrMissingModel creates a missing model error
func ErrMissingModel() *Error {
	return &Error{
		Code:       ErrorCodeMissingModel,
		Message:    "model is required",
		Type:       ErrorTypeInvalidRequest,
		Retryable:  false,
		HTTPStatus: http.StatusBadRequest,
	}
}

// ==================== Helper Functions ====================

// IsRetryableError checks if an error is retryable
// Uses errors.As to support wrapped errors
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Use errors.As to support wrapped errors
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}

	// For non-structured errors, check common retryable conditions
	return false
}

// StatusCodeFromError extracts the HTTP status code from an error
// Uses errors.As to support wrapped errors
// Returns http.StatusInternalServerError if no status can be determined
func StatusCodeFromError(err error) int {
	if err == nil {
		return http.StatusOK
	}

	// Use errors.As to support wrapped errors
	var e *Error
	if errors.As(err, &e) {
		return e.HTTPStatus
	}

	// Default to 500 for unknown errors
	return http.StatusInternalServerError
}

// ErrorToGinResponse writes the error as a JSON response to the gin context
func ErrorToGinResponse(c *gin.Context, err error) {
	if sendAPIKeyModelRequestQuotaError(c, err) {
		return
	}
	if err == nil {
		return
	}
	if !claimContinuousRetryTerminal(c, continuousRetryProtocolOpenAI) {
		return
	}

	var e *Error
	if errors.As(err, &e) {
		c.JSON(e.HTTPStatus, e.ToGinH())
		return
	}

	// 兜底识别:WS 握手阶段的工作区停用错误若因任何原因未在 wsrelay 层转换成
	// 结构化响应(文本形如 "websocket handshake failed: ... deactivated_workspace"),
	// 也绝不能以 500 + 原始握手内部细节漏给下游——按 503 池级错误返回(带
	// Retry-After 提示退避),文案附上游原始错误体。坏账号会由采样探针/主路径
	// 标错隔离,稍后重试可落到健康账号。
	if message := err.Error(); strings.Contains(message, "websocket handshake failed") &&
		strings.Contains(message, "deactivated_workspace") {
		if c.Writer.Header().Get("Retry-After") == "" {
			c.Header("Retry-After", "30")
		}
		// 握手错误文本尾部携带上游 JSON 错误体,只取该部分,握手内部细节
		// (Cf-Ray/X-Request-Id 等)不外漏。
		detail := ""
		if idx := strings.Index(message, "{"); idx >= 0 {
			detail = strings.TrimSpace(message[idx:])
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": deactivatedPoolErrorMessage(detail),
				"type":    "server_error",
				"code":    "account_pool_deactivated",
			},
		})
		return
	}

	// Fallback for non-structured errors
	c.JSON(http.StatusInternalServerError, gin.H{
		"error": gin.H{
			"message": err.Error(),
			"type":    ErrorTypeServerError,
			"code":    ErrorCodeInternalError,
		},
	})
}

// deactivatedPoolErrorMessage 组装工作区停用的池级错误文案(英文,面向下游
// 客户端展示),末尾附上游原始错误体(截断,避免超长 body 污染日志/客户端展示)。
func deactivatedPoolErrorMessage(upstreamDetail string) string {
	const message = "No available account in the pool (upstream workspace deactivated), please retry later"
	upstreamDetail = strings.TrimSpace(upstreamDetail)
	if upstreamDetail == "" {
		return message
	}
	if len(upstreamDetail) > 500 {
		upstreamDetail = upstreamDetail[:500] + "…"
	}
	return message + ". Upstream response: " + upstreamDetail
}
