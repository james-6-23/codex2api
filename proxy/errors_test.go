package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// TestErrorMissingAPIKey 测试缺失 API Key 错误
func TestErrorMissingAPIKey(t *testing.T) {
	err := ErrMissingAPIKey()

	if err.Code != ErrorCodeMissingAPIKey {
		t.Errorf("expected code %s, got %s", ErrorCodeMissingAPIKey, err.Code)
	}
	if err.Type != ErrorTypeAuthentication {
		t.Errorf("expected type %s, got %s", ErrorTypeAuthentication, err.Type)
	}
	if err.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, err.HTTPStatus)
	}
	if err.Retryable {
		t.Error("missing API key error should not be retryable")
	}
}

// TestErrorRateLimited 测试限流错误
func TestErrorRateLimited(t *testing.T) {
	err := ErrRateLimited("custom rate limit message")

	if err.Code != ErrorCodeRateLimited {
		t.Errorf("expected code %s, got %s", ErrorCodeRateLimited, err.Code)
	}
	if err.Message != "custom rate limit message" {
		t.Errorf("expected message %s, got %s", "custom rate limit message", err.Message)
	}
	if err.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("expected status %d, got %d", http.StatusTooManyRequests, err.HTTPStatus)
	}
	if !err.Retryable {
		t.Error("rate limited error should be retryable")
	}
}

// TestErrorAccountPoolUsageLimit 测试账号池使用限制错误
func TestErrorAccountPoolUsageLimit(t *testing.T) {
	err := ErrAccountPoolUsageLimit("quota exhausted", "plus", 0, 60)

	if err.Code != ErrorCodeAccountPoolUsageLimit {
		t.Errorf("expected code %s, got %s", ErrorCodeAccountPoolUsageLimit, err.Code)
	}
	// 应包含 plan type 和 reset 信息
	if err.Message == "" {
		t.Error("message should not be empty")
	}
	if err.HTTPStatus != http.StatusServiceUnavailable {
		t.Errorf("expected status %d, got %d", http.StatusServiceUnavailable, err.HTTPStatus)
	}
	if !err.Retryable {
		t.Error("account pool usage limit error should be retryable")
	}
}

// TestErrorUpstream 测试上游错误
func TestErrorUpstream(t *testing.T) {
	cause := errors.New("connection refused")
	err := ErrUpstream(http.StatusBadGateway, "upstream failed", cause)

	if err.Code != ErrorCodeUpstreamError {
		t.Errorf("expected code %s, got %s", ErrorCodeUpstreamError, err.Code)
	}
	if err.HTTPStatus != http.StatusBadGateway {
		t.Errorf("expected status %d, got %d", http.StatusBadGateway, err.HTTPStatus)
	}
	if err.Cause != cause {
		t.Error("cause should match")
	}
	if err.Retryable {
		t.Error("502 upstream error should not be legacy retryable")
	}

	if timeout := ErrUpstream(http.StatusGatewayTimeout, "upstream timeout", cause); timeout.Retryable {
		t.Error("504 upstream error should not be legacy retryable")
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		if upstream := ErrUpstream(status, "legacy retryable", cause); !upstream.Retryable {
			t.Errorf("status %d upstream error should remain legacy retryable", status)
		}
	}
	if notFound := ErrUpstream(http.StatusNotFound, "not found", cause); notFound.Retryable {
		t.Error("404 upstream error should not be retryable without a narrow classification")
	}
}

func TestUpstreamErrorBodyIsValidJSONForInvalidUTF8(t *testing.T) {
	err := &Error{
		Code:    "upstream_error",
		Message: string([]byte{'b', 0xff, 'd'}),
		Type:    ErrorTypeUpstreamError,
	}
	body := err.UpstreamErrorBody()
	if !json.Valid(body) {
		t.Fatalf("UpstreamErrorBody returned invalid JSON: %q", body)
	}
}

// TestErrorUnwrap 测试错误链 Unwrap
func TestErrorUnwrap(t *testing.T) {
	cause := errors.New("underlying error")
	err := ErrUpstream(http.StatusInternalServerError, "server error", cause)

	// 使用 errors.Unwrap
	unwrapped := errors.Unwrap(err)
	if unwrapped != cause {
		t.Error("Unwrap should return the cause")
	}

	// 使用 errors.Is
	if !errors.Is(err, cause) {
		t.Error("errors.Is should match the cause")
	}
}

// TestIsRetryableError 测试 IsRetryableError 函数
func TestIsRetryableError(t *testing.T) {
	// 可重试错误
	retryable := ErrRateLimited("rate limited")
	if !IsRetryableError(retryable) {
		t.Error("rate limited error should be retryable")
	}

	// 不可重试错误
	notRetryable := ErrMissingAPIKey()
	if IsRetryableError(notRetryable) {
		t.Error("missing API key error should not be retryable")
	}

	// nil 错误
	if IsRetryableError(nil) {
		t.Error("nil error should not be retryable")
	}

	// wrapped error
	wrapped := errors.Join(retryable, errors.New("additional context"))
	if !IsRetryableError(wrapped) {
		t.Error("wrapped retryable error should be retryable")
	}
}

// TestStatusCodeFromError 测试 StatusCodeFromError 函数
func TestStatusCodeFromError(t *testing.T) {
	// 有状态码的错误
	err := ErrRateLimited("rate limited")
	status := StatusCodeFromError(err)
	if status != http.StatusTooManyRequests {
		t.Errorf("expected status %d, got %d", http.StatusTooManyRequests, status)
	}

	// nil 错误
	status = StatusCodeFromError(nil)
	if status != http.StatusOK {
		t.Errorf("expected status %d for nil error, got %d", http.StatusOK, status)
	}

	// wrapped error
	wrapped := errors.Join(err, errors.New("wrapped"))
	status = StatusCodeFromError(wrapped)
	if status != http.StatusTooManyRequests {
		t.Errorf("expected status %d for wrapped error, got %d", http.StatusTooManyRequests, status)
	}
}

// TestErrorErrorMethod 测试 Error 方法的字符串输出
func TestErrorErrorMethod(t *testing.T) {
	// 无 cause 的错误
	err := ErrMissingAPIKey()
	errStr := err.Error()
	expected := "missing_api_key: Missing Authorization header"
	if errStr != expected {
		t.Errorf("expected %s, got %s", expected, errStr)
	}

	// 有 cause 的错误
	cause := errors.New("connection timeout")
	err = ErrUpstream(http.StatusGatewayTimeout, "timeout", cause)
	errStr = err.Error()
	expected = "upstream_error: timeout (caused by: connection timeout)"
	if errStr != expected {
		t.Errorf("expected %s, got %s", expected, errStr)
	}
}
