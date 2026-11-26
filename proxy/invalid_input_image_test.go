package proxy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

const invalidInputImageTestProblem = `{"code":"invalid_value","type":"invalid_request_error","param":"input","message":"The image data you provided does not represent a valid image. Please check your input and try again."}`

func TestInvalidInputImageClassification(t *testing.T) {
	for name, body := range map[string]string{
		"http":            `{"error":` + invalidInputImageTestProblem + `}`,
		"error event":     `{"type":"error","error":` + invalidInputImageTestProblem + `}`,
		"response failed": `{"type":"response.failed","response":{"error":` + invalidInputImageTestProblem + `}}`,
		"status details":  `{"response":{"status_details":{"error":` + invalidInputImageTestProblem + `}}}`,
		"bare":            invalidInputImageTestProblem,
	} {
		t.Run(name, func(t *testing.T) {
			if !isInvalidInputImagePayload([]byte(body)) {
				t.Fatal("invalid image was not recognized")
			}
		})
	}
	for name, body := range map[string]string{
		"generic 400":           `{"detail":"Bad Request"}`,
		"generic invalid value": strings.Replace(invalidInputImageTestProblem, "The image data you provided does not represent a valid image. Please check your input and try again.", "Unsupported parameter value", 1),
		"different code":        strings.Replace(invalidInputImageTestProblem, "invalid_value", "server_error", 1),
		"different type":        strings.Replace(invalidInputImageTestProblem, "invalid_request_error", "server_error", 1),
		"different param":       strings.Replace(invalidInputImageTestProblem, `"input"`, `"model"`, 1),
		"mention only":          strings.Replace(invalidInputImageTestProblem, "The image data", "Diagnostics mentioned: The image data", 1),
		"non error field":       `{"metadata":` + invalidInputImageTestProblem + `}`,
		"malformed":             `{"error":` + invalidInputImageTestProblem,
	} {
		t.Run(name, func(t *testing.T) {
			if isInvalidInputImagePayload([]byte(body)) {
				t.Fatal("unrelated failure was classified as invalid image")
			}
		})
	}
}

func TestInvalidInputImageNeverRetries(t *testing.T) {
	body := []byte(`{"error":` + invalidInputImageTestProblem + `}`)
	frame := []byte(`{"type":"response.failed","response":{"status_code":400,"error":` + invalidInputImageTestProblem + `}}`)
	for name, policy := range map[string]database.ContinuousRetryPolicy{
		"disabled":   {},
		"400":        {Enabled: true, StatusCodes: []int{400}},
		"error code": {Enabled: true, ErrorCodes: []string{"invalid_value"}},
		"all":        {Enabled: true, CatchAll: true},
	} {
		t.Run(name, func(t *testing.T) {
			general, rate := 0, 0
			if continuousRetryHTTPSelected(policy, 400, body) || shouldRetryHTTPStatus(400, body, &general, &rate, -1, -1, policy) {
				t.Fatal("invalid image entered HTTP retry")
			}
			for _, status := range []int{0, 400} {
				err := fmt.Errorf("wrapped: %w", continuousRetryTestHTTPError{status: status, body: body})
				if continuousRetryRequestErrorSelected(policy, err) || isRetryableRequestErrorForContext(context.Background(), err, policy) || shouldRetryRequestError(err, &general, -1, policy) {
					t.Fatal("invalid image entered request-error retry")
				}
				if got := continuousRetryLimitForRequestError(err, 3, policy); got != 3 {
					t.Fatalf("invalid image promoted budget to %d", got)
				}
			}
			outcome := classifyResponseFailedOutcome(frame)
			// Even an independently misclassified legacy outcome must not bypass the guard.
			outcome.penalize = true
			if continuousRetryStreamSelected(outcome, frame, "response.failed", policy) || continuousRetryStreamFailureSelected(outcome, frame, "response.failed", policy) || shouldTransparentRetryStreamEventWithBudgets(outcome, "response.failed", &general, &rate, -1, -1, false, nil, nil, policy) {
				t.Fatal("invalid image entered stream retry")
			}
			if isRetryableUpstreamErrorFrame("error", body, policy) || continuousRetryStreamFailureSelected(outcome, nil, "response.failed", policy) {
				t.Fatal("invalid image entered error-frame or saved-outcome retry")
			}
			if shouldRetryImageStreamError(newImageResponseFailedError(frame), &general, -1, 0, 5, policy) {
				t.Fatal("invalid image entered Images retry")
			}
			if got := imageErrorStatusCode(newImageResponseFailedError(frame)); got != 400 {
				t.Fatalf("Images invalid input status = %d, want 400", got)
			}
			if general != 0 || rate != 0 {
				t.Fatalf("terminal failure consumed retry budgets: %d/%d", general, rate)
			}
		})
	}
}

func TestInvalidInputImageGuardPreservesOtherRetries(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, StatusCodes: []int{400, 429, 503}}
	for _, status := range []int{400, 429, 503} {
		general, rate := 0, 0
		if !shouldRetryHTTPStatus(status, []byte(`{"error":{"code":"invalid_value","message":"another failure"}}`), &general, &rate, 0, 0, policy) {
			t.Fatalf("unrelated selected status %d no longer retries", status)
		}
	}
	// Without an explicit selector a transient failure still consumes only its finite retry budget.
	general, rate := 0, 0
	body := []byte(`{"error":{"code":"server_error"}}`)
	if !shouldRetryHTTPStatus(503, body, &general, &rate, 1, 0, database.ContinuousRetryPolicy{}) ||
		shouldRetryHTTPStatus(503, body, &general, &rate, 1, 0, database.ContinuousRetryPolicy{}) || general != 1 {
		t.Fatal("ordinary 503 did not retain its single finite retry")
	}
}
