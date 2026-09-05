// Package api provides standardized API error handling and response formats
package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// ErrorCode represents a standardized API error code
type ErrorCode string

// Standard error codes following OpenAI API conventions
const (
	// Authentication errors
	ErrCodeMissingAPIKey     ErrorCode = "missing_api_key"
	ErrCodeInvalidAPIKey     ErrorCode = "invalid_api_key"
	ErrCodeInsufficientScope ErrorCode = "insufficient_scope"
	ErrCodeInvalidAuth       ErrorCode = "invalid_auth"

	// Request errors
	ErrCodeInvalidRequest             ErrorCode = "invalid_request"
	ErrCodeInvalidParameter           ErrorCode = "invalid_parameter"
	ErrCodeMissingField               ErrorCode = "missing_field"
	ErrCodeInvalidFieldType           ErrorCode = "invalid_field_type"
	ErrCodeInvalidFieldFormat         ErrorCode = "invalid_field_format"
	ErrCodeContextLengthExceeded      ErrorCode = "context_length_exceeded"
	ErrCodeResponseContextUnavailable ErrorCode = "response_context_unavailable"
	ErrCodeUnsupportedModel           ErrorCode = "unsupported_model"
	ErrCodeRateLimitReached           ErrorCode = "rate_limit_reached"
	ErrCodeAccountSessionCapacity     ErrorCode = "account_session_capacity_exceeded"

	// Server errors
	ErrCodeServerError        ErrorCode = "server_error"
	ErrCodeServiceUnavailable ErrorCode = "service_unavailable"
	ErrCodeUpstreamError      ErrorCode = "upstream_error"
	ErrCodeUpstreamTimeout    ErrorCode = "upstream_timeout"

	// Resource errors
	ErrCodeResourceNotFound ErrorCode = "resource_not_found"
	ErrCodeResourceConflict ErrorCode = "resource_conflict"
)

// ErrorType represents the category of error
type ErrorType string

const (
	ErrorTypeInvalidRequest ErrorType = "invalid_request_error"
	ErrorTypeAuthentication ErrorType = "authentication_error"
	ErrorTypePermission     ErrorType = "permission_error"
	ErrorTypeNotFound       ErrorType = "not_found_error"
	ErrorTypeRateLimit      ErrorType = "rate_limit_error"
	ErrorTypeServer         ErrorType = "server_error"
	ErrorTypeUpstream       ErrorType = "upstream_error"
)

// ErrorDetail contains additional error context
type ErrorDetail struct {
	Field   string `json:"field,omitempty"`
	Value   string `json:"value,omitempty"`
	Message string `json:"message,omitempty"`
}

// APIError represents a standardized API error response
type APIError struct {
	Code    ErrorCode   `json:"code"`
	Message string      `json:"message"`
	Type    ErrorType   `json:"type"`
	Details interface{} `json:"details,omitempty"`
}

// ErrorResponse wraps APIError for consistent JSON structure
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// Error implements the error interface
func (e *APIError) Error() string {
	return fmt.Sprintf("[%s] %s: %s", e.Type, e.Code, e.Message)
}

// NewAPIError creates a new API error
func NewAPIError(code ErrorCode, message string, errType ErrorType) *APIError {
	return &APIError{
		Code:    code,
		Message: message,
		Type:    errType,
	}
}

// NewAPIErrorWithDetails creates a new API error with details
func NewAPIErrorWithDetails(code ErrorCode, message string, errType ErrorType, details interface{}) *APIError {
	return &APIError{
		Code:    code,
		Message: message,
		Type:    errType,
		Details: details,
	}
}

// Predefined errors for common scenarios
var (
	// Authentication errors
	ErrMissingAPIKey = NewAPIError(ErrCodeMissingAPIKey, "Missing Authorization header", ErrorTypeAuthentication)
	ErrInvalidAPIKey = NewAPIError(ErrCodeInvalidAPIKey, "Invalid API key provided", ErrorTypeAuthentication)

	// Request validation errors
	ErrInvalidRequest = NewAPIError(ErrCodeInvalidRequest, "Invalid request", ErrorTypeInvalidRequest)
	ErrMissingField   = NewAPIError(ErrCodeMissingField, "Required field is missing", ErrorTypeInvalidRequest)
	ErrInvalidField   = NewAPIError(ErrCodeInvalidParameter, "Invalid field value", ErrorTypeInvalidRequest)

	// Server errors
	ErrServerError        = NewAPIError(ErrCodeServerError, "Internal server error", ErrorTypeServer)
	ErrServiceUnavailable = NewAPIError(ErrCodeServiceUnavailable, "Service temporarily unavailable", ErrorTypeServer)
	ErrUpstreamError      = NewAPIError(ErrCodeUpstreamError, "Upstream service error", ErrorTypeUpstream)
)

// HTTPStatusCode returns the appropriate HTTP status code for an error code
func HTTPStatusCode(code ErrorCode) int {
	switch code {
	case ErrCodeMissingAPIKey, ErrCodeInvalidAPIKey, ErrCodeInvalidAuth, ErrCodeInsufficientScope:
		return http.StatusUnauthorized
	case ErrCodeRateLimitReached:
		return http.StatusTooManyRequests
	case ErrCodeResourceNotFound:
		return http.StatusNotFound
	case ErrCodeResourceConflict:
		return http.StatusConflict
	case ErrCodeResponseContextUnavailable:
		return http.StatusConflict
	case ErrCodeInvalidRequest, ErrCodeInvalidParameter, ErrCodeMissingField, ErrCodeInvalidFieldType,
		ErrCodeInvalidFieldFormat, ErrCodeContextLengthExceeded, ErrCodeUnsupportedModel,
		ErrCodeAccountSessionCapacity:
		return http.StatusBadRequest
	case ErrCodeServiceUnavailable:
		return http.StatusServiceUnavailable
	case ErrCodeServerError, ErrCodeUpstreamError, ErrCodeUpstreamTimeout:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// SendError sends a standardized error response
func SendError(c *gin.Context, err *APIError) {
	status := HTTPStatusCode(err.Code)
	c.JSON(status, ErrorResponse{Error: *err})
}

// SendErrorWithStatus sends an error response with a specific HTTP status
func SendErrorWithStatus(c *gin.Context, err *APIError, status int) {
	c.JSON(status, ErrorResponse{Error: *err})
}

// SendValidationError sends a validation error response
func SendValidationError(c *gin.Context, field, message string) {
	err := NewAPIErrorWithDetails(
		ErrCodeInvalidParameter,
		"Request validation failed",
		ErrorTypeInvalidRequest,
		ErrorDetail{
			Field:   field,
			Message: message,
		},
	)
	SendError(c, err)
}

// SendMissingFieldError sends a missing field error
func SendMissingFieldError(c *gin.Context, field string) {
	err := NewAPIErrorWithDetails(
		ErrCodeMissingField,
		"Required field is missing",
		ErrorTypeInvalidRequest,
		ErrorDetail{
			Field:   field,
			Message: fmt.Sprintf("Field '%s' is required", field),
		},
	)
	SendError(c, err)
}

// ErrorCodeToLegacy converts new error codes to legacy format for backward compatibility
func ErrorCodeToLegacy(code ErrorCode) string {
	switch code {
	case ErrCodeMissingAPIKey:
		return "missing_api_key"
	case ErrCodeInvalidAPIKey:
		return "invalid_api_key"
	case ErrCodeInvalidRequest:
		return "invalid_request_error"
	case ErrCodeServerError:
		return "server_error"
	case ErrCodeUpstreamError:
		return "upstream_error"
	default:
		return string(code)
	}
}

// LegacyErrorToAPIError converts legacy error format to standardized APIError
func LegacyErrorToAPIError(message string, errType string, code string) *APIError {
	return &APIError{
		Code:    ErrorCode(code),
		Message: message,
		Type:    ErrorType(errType),
	}
}

// MarshalJSON implements custom JSON marshaling to ensure consistent format
func (e ErrorResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]interface{}{"error": e.Error})
}
