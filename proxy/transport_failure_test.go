package proxy

import (
	"errors"
	"net/http"
	"testing"
)

func TestClassifyTransportFailureWsBusyAcquire(t *testing.T) {
	err := errors.New("acquire websocket connection timed out after 30s waiting for busy session")
	if got := classifyTransportFailure(err); got != upstreamErrorKindWsBusyAcquire {
		t.Fatalf("classifyTransportFailure = %q, want %q", got, upstreamErrorKindWsBusyAcquire)
	}
}

// "timed out" 文案（如容量等待超时）应归为 timeout 而不是笼统的 transport。
func TestClassifyTransportFailureTimedOutPhrase(t *testing.T) {
	err := errors.New("acquire websocket connection timed out after 30s waiting for account connection capacity")
	if got := classifyTransportFailure(err); got != "timeout" {
		t.Fatalf("classifyTransportFailure = %q, want timeout", got)
	}
}

func TestClassifyTransportFailureDoesNotTurnStructuredInvalidRequestIntoTransport(t *testing.T) {
	err := &Error{
		Code:       "adapter_invalid_request",
		Message:    "unsupported request feature",
		Type:       ErrorTypeInvalidRequest,
		Retryable:  false,
		HTTPStatus: http.StatusBadRequest,
		Cause:      errors.New("decoder reported an ordinary validation detail"),
	}
	if got := classifyTransportFailure(err); got != "" {
		t.Fatalf("classifyTransportFailure = %q, want empty for deterministic 4xx", got)
	}
	retries := 0
	if shouldRetryRequestError(err, &retries, 2) {
		t.Fatal("structured invalid request must not be retried against another account")
	}
	if retries != 0 {
		t.Fatalf("retries = %d, want 0", retries)
	}
}

func TestClassifyTransportFailureKeepsStructuredTransportCauses(t *testing.T) {
	err := &Error{
		Code:       ErrorCodeUpstreamError,
		Message:    "request failed",
		Type:       ErrorTypeUpstreamError,
		Retryable:  true,
		HTTPStatus: http.StatusBadGateway,
		Cause:      errors.New("connection reset by peer"),
	}
	if got := classifyTransportFailure(err); got != "transport" {
		t.Fatalf("classifyTransportFailure = %q, want transport", got)
	}
}

func TestClassifyTransportFailureUnwrapsStatuslessUpstreamDoFailure(t *testing.T) {
	err := ErrUpstream(0, "请求 OpenAI Responses API 失败", errors.New("EOF"))
	if got := classifyTransportFailure(err); got != "transport" {
		t.Fatalf("classifyTransportFailure = %q, want transport for status-less Do failure", got)
	}
	if !isRetryableRequestError(err) {
		t.Fatal("status-less upstream Do failure must remain retryable")
	}
}

func TestShouldPenalizeTransportKind(t *testing.T) {
	if shouldPenalizeTransportKind(upstreamErrorKindWsBusyAcquire) {
		t.Fatal("busy acquire timeout must not penalize account health (issue #413)")
	}
	if shouldPenalizeTransportKind("") {
		t.Fatal("empty kind must not penalize")
	}
	if !shouldPenalizeTransportKind("transport") {
		t.Fatal("generic transport failure should penalize")
	}
	if !shouldPenalizeTransportKind("timeout") {
		t.Fatal("timeout failure should penalize")
	}
}
