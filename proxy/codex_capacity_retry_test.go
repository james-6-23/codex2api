package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const codexCapacityTestMessage = "Selected model is at capacity. Please try a different model."
const codexCapacityTestBody = `{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model.","type":"service_unavailable_error"}}`

func setCodexCapacityRetryForTest(test *testing.T, enabled bool) {
	test.Helper()
	previous := CurrentRuntimeSettings()
	test.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexCapacityRetryEnabled = enabled
	ApplyRuntimeSettings(next)
}

func TestCodexCapacityRetryFiniteBudgets(test *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, policy := range []database.ContinuousRetryPolicy{
			{},
			{Enabled: true, CatchAll: true},
			{Enabled: true, ErrorCodes: []string{"server_is_overloaded"}, StatusCodes: []int{429, 500}},
		} {
			test.Run(fmt.Sprintf("enabled=%t/policy=%v", enabled, policy), func(test *testing.T) {
				setCodexCapacityRetryForTest(test, enabled)
				body := []byte(codexCapacityTestBody)
				for _, status := range []int{429, 500, 502, 503, 504} {
					generalRetries, rateRetries := 0, 0
					handler := &Handler{}
					if got := handler.shouldRetryUpstreamHTTPStatus(status, body, &generalRetries, &rateRetries, 1, 1, policy); got != enabled {
						test.Fatalf("status %d first retry=%t, want %t", status, got, enabled)
					}
					if handler.shouldRetryUpstreamHTTPStatus(status, body, &generalRetries, &rateRetries, 1, 1, policy) {
						test.Fatalf("status %d exceeded one retry", status)
					}
					if general, rate := continuousRetryLimitsForHTTP(status, body, 1, 1, policy); general != 1 || rate != 1 {
						test.Fatalf("capacity budget became unlimited: %d/%d", general, rate)
					}
					if !enabled && generalRetries+rateRetries != 0 {
						test.Fatal("disabled retries consumed a budget")
					}
				}
				for _, payload := range []string{
					`{"type":"error",` + codexCapacityTestBody[1:],
					`{"type":"response.failed","response":` + codexCapacityTestBody + `}`,
				} {
					outcome := classifyResponseFailedOutcome([]byte(payload))
					generalRetries, rateRetries := 0, 0
					event := gjson.Get(payload, "type").String()
					if got := shouldTransparentRetryStreamEventWithBudgets(outcome, event, &generalRetries, &rateRetries, 1, 1, false, nil, nil, policy); got != enabled {
						test.Fatalf("%s first retry=%t, want %t", event, got, enabled)
					}
					if shouldTransparentRetryStreamEventWithBudgets(outcome, event, &generalRetries, &rateRetries, 1, 1, false, nil, nil, policy) {
						test.Fatal("stream exceeded one retry")
					}
					if shouldTransparentRetryStreamEventWithBudgets(outcome, event, &generalRetries, &rateRetries, 10, 10, true, nil, nil, policy) {
						test.Fatal("already-visible output was retried")
					}
				}
				requestErr := continuousRetryTestHTTPError{status: 500, body: body}
				generalRetries := 0
				if got := shouldRetryRequestError(requestErr, &generalRetries, 1, policy); got != enabled {
					test.Fatalf("handshake retry=%t, want %t", got, enabled)
				}
				if shouldRetryRequestError(requestErr, &generalRetries, 1, policy) {
					test.Fatal("handshake exceeded one retry")
				}
			})
		}
	}
}

func TestCodexCapacityErrorRecognition(test *testing.T) {
	for _, sample := range []struct {
		body string
		want bool
	}{
		{codexCapacityTestBody, true},
		{`{"response":{"status_details":{"error":{"code":"slow_down","message":"slow down"}}}}`, true},
		{`{"detail":"` + codexCapacityTestMessage + `"}`, true},
		{`{"error":{"message":"` + codexCapacityTestMessage + `"}}`, true},
		{`{"input":[{"text":"` + codexCapacityTestMessage + `"}]}`, false},
		{`{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`, false},
		{`{"error":{"code":"rate_limit_exceeded","message":"rate limited"}}`, false},
		{`{"error":{"code":"server_error","message":"internal failure"}}`, false},
		{`not json`, false},
	} {
		if got := codexCapacityErrorForClient([]byte(sample.body)); (got != nil) != sample.want {
			test.Errorf("recognition=%v for %s", got, sample.body)
		}
	}
	setCodexCapacityRetryForTest(test, false)
	for _, status := range []int{429, 500} {
		generalRetries, rateRetries := 0, 0
		if !shouldRetryHTTPStatus(status, nil, &generalRetries, &rateRetries, 1, 1, database.ContinuousRetryPolicy{}) {
			test.Fatalf("ordinary %d retry changed", status)
		}
	}
}

func TestCodexCapacityFinalErrorsPreserveOriginal(test *testing.T) {
	for _, enabled := range []bool{false, true} {
		test.Run(fmt.Sprint(enabled), func(test *testing.T) {
			setCodexCapacityRetryForTest(test, enabled)
			for _, write := range []func(*gin.Context){
				func(ctx *gin.Context) { (&Handler{}).sendFinalUpstreamError(ctx, 500, []byte(codexCapacityTestBody)) },
				func(ctx *gin.Context) {
					ErrorToGinResponse(ctx, continuousRetryTestHTTPError{status: 500, body: []byte(codexCapacityTestBody)})
				},
				func(ctx *gin.Context) {
					writeResponseFailedHTTPError(ctx, 500, []byte(codexCapacityTestBody), "generic")
				},
			} {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				ctx.Header("Retry-After", "30")
				write(ctx)
				if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Retry-After") != "" {
					test.Fatalf("retryable HTTP response: status=%d headers=%v", recorder.Code, recorder.Header())
				}
				assertCodexCapacityError(test, recorder.Body.Bytes(), "error")
			}
			apiError := responsesWSClientUpstreamAPIError(responsesWSUpstreamAPIError(500, []byte(codexCapacityTestBody)), true)
			if string(apiError.Code) != "server_is_overloaded" || apiError.Message != codexCapacityTestMessage {
				test.Fatalf("WS hid original capacity error: %+v", apiError)
			}
			if responsesWSTerminalCloseCode(apiError, websocket.CloseTryAgainLater) != websocket.ClosePolicyViolation {
				test.Fatal("WS terminal suggests retry")
			}
		})
	}
}

func assertCodexCapacityError(test *testing.T, body []byte, path string) {
	test.Helper()
	if gjson.GetBytes(body, path+".code").String() != "server_is_overloaded" ||
		gjson.GetBytes(body, path+".message").String() != codexCapacityTestMessage ||
		gjson.GetBytes(body, path+".type").String() != "service_unavailable_error" {
		test.Fatalf("original error was changed: %s", body)
	}
}

func TestCodexCapacityCommittedErrorPreserved(test *testing.T) {
	setCodexCapacityRetryForTest(test, true)
	for _, protocol := range []continuousRetryHTTPProtocol{continuousRetryProtocolResponses, continuousRetryProtocolChat, continuousRetryProtocolAnthropic} {
		test.Run(fmt.Sprint(protocol), func(test *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			stop := installContinuousRetryHTTPDeadline(ctx, database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}, protocol)
			defer stop()
			rememberContinuousRetryHTTPFailure(ctx.Request.Context(), &http.Response{StatusCode: 500}, []byte(codexCapacityTestBody))
			ctx.Header("Content-Type", "text/event-stream")
			ctx.Writer.WriteHeaderNow()
			switch protocol {
			case continuousRetryProtocolResponses:
				writeCommittedResponsesRetryError(ctx, "generic")
			case continuousRetryProtocolChat:
				writeCommittedChatRetryError(ctx, "generic")
			case continuousRetryProtocolAnthropic:
				writeCommittedAnthropicRetryError(ctx, "api_error", "generic")
			}
			body := recorder.Body.String()
			position := strings.Index(body, "data: ")
			if position < 0 {
				test.Fatalf("missing SSE error: %s", body)
			}
			path := "error"
			if protocol == continuousRetryProtocolResponses {
				path = "response.error"
			}
			assertCodexCapacityError(test, []byte(strings.TrimSpace(body[position+6:])), path)
			if failure, exists := continuousRetryLastFailure(ctx.Request.Context()); !exists || failure.status != 500 {
				test.Fatalf("original upstream status was changed: %+v", failure)
			}
		})
	}
}
