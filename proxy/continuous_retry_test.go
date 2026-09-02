package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type continuousRetryTestHTTPError struct {
	status int
	body   []byte
}

func TestContinuousRetryPolicyForRequestKeepsInitialSnapshot(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	c := &gin.Context{}
	rememberContinuousRetryPolicyForRequest(c, database.ContinuousRetryPolicy{})

	updated := previous
	updated.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	ApplyRuntimeSettings(updated)

	if policy := continuousRetryPolicyForRequest(c); policy.Enabled || policy.CatchAll {
		t.Fatalf("request policy changed after hot reload: %#v", policy)
	}
}

func (e continuousRetryTestHTTPError) Error() string             { return "upstream websocket handshake failed" }
func (e continuousRetryTestHTTPError) UpstreamStatusCode() int   { return e.status }
func (e continuousRetryTestHTTPError) UpstreamErrorBody() []byte { return e.body }

func TestContinuousRetryPreflightPassthroughStopsBeforeAnyRetryPolicy(t *testing.T) {
	settings := RuntimeSettings{CodexPreflightSSEPassthrough: true}
	if !continuousRetryPreflightPassthrough(settings) {
		t.Fatal("disabled continuous retry unexpectedly blocked preflight passthrough")
	}
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	if continuousRetryPreflightPassthrough(settings) {
		t.Fatal("selective continuous retry leaked preflight metadata before the retry boundary")
	}
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	if continuousRetryPreflightPassthrough(settings) {
		t.Fatal("catch-all continuous retry leaked preflight metadata before the retry boundary")
	}
}

func TestContinuousRetryBuffersEveryEnabledSelectorMode(t *testing.T) {
	if continuousRetryBuffersAttempts(database.ContinuousRetryPolicy{}) {
		t.Fatal("disabled policy unexpectedly enabled attempt buffering")
	}
	selective := database.ContinuousRetryPolicy{
		Enabled:     true,
		StatusCodes: []int{http.StatusServiceUnavailable},
	}
	if !continuousRetryBuffersAttempts(selective) {
		t.Fatal("selective continuous retry must buffer the full attempt")
	}
	if !continuousRetryBuffersAttempts(database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}) {
		t.Fatal("catch-all continuous retry must buffer the full attempt")
	}
}

func TestContinuousRetryHTTPSelectionIsOptInAndExact(t *testing.T) {
	disabled := database.ContinuousRetryPolicy{
		Enabled:     false,
		Categories:  []string{database.ContinuousRetryCategoryHTTP4xx},
		StatusCodes: []int{404},
	}
	general, rate := 0, 0
	if shouldRetryHTTPStatus(http.StatusNotFound, nil, &general, &rate, 0, 0, disabled) {
		t.Fatal("disabled continuous policy enabled a 404 retry")
	}

	selected := database.ContinuousRetryPolicy{
		Enabled:     true,
		StatusCodes: []int{http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented},
	}
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented} {
		general, rate = 0, 0
		if !shouldRetryHTTPStatus(status, nil, &general, &rate, 0, 0, selected) {
			t.Fatalf("selected status %d was not retried", status)
		}
		if general != 1 || rate != 0 {
			t.Fatalf("selected status %d consumed budgets general=%d rate=%d", status, general, rate)
		}
	}
	general, rate = 0, 0
	if shouldRetryHTTPStatus(http.StatusBadRequest, nil, &general, &rate, 0, 0, selected) {
		t.Fatal("unselected 400 was retried")
	}
}

func TestContinuousRetryHTTPSelectionSupportsContextCategory(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryContextError},
	}
	general, rate := 0, 0
	body := []byte(`{"error":{"code":"context_length_exceeded"}}`)
	if !shouldRetryHTTPStatus(http.StatusBadRequest, body, &general, &rate, 0, 0, policy) {
		t.Fatal("context category did not select a context-length HTTP error")
	}
	if shouldRetryHTTPStatus(http.StatusBadRequest, []byte(`{"error":{"code":"invalid_request"}}`), &general, &rate, 0, 0, policy) {
		t.Fatal("context category selected an unrelated 400")
	}
}

func TestContinuousRetryCatchAllSelectsUnknownUpstreamFailures(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}

	for _, status := range []int{http.StatusTeapot, 499, 520, 599, 600, 701} {
		general, rate := 0, 0
		if !shouldRetryHTTPStatus(status, []byte(`{"error":{"code":"never_seen_before"}}`), &general, &rate, 0, 0, policy) {
			t.Fatalf("catch-all policy did not retry unknown HTTP status %d", status)
		}
	}

	quota := []byte(`{"error":{"code":"insufficient_quota"}}`)
	if !continuousRetryHTTPSelected(policy, http.StatusTooManyRequests, quota) {
		t.Fatal("catch-all policy did not explicitly override the permanent-quota guard")
	}

	unknownFrame := []byte(`{"type":"error","error":{"code":"future_upstream_failure"}}`)
	outcome := classifyResponseFailedOutcome(unknownFrame)
	if !continuousRetryStreamSelected(outcome, unknownFrame, "error", policy) {
		t.Fatal("catch-all policy did not retry an unknown upstream error frame")
	}

	general, rate := 0, 0
	if shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, 0, 0, true, nil, nil, policy) {
		t.Fatal("catch-all policy replayed a failure after downstream output")
	}
	if shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, 0, 0, false, context.Canceled, nil, policy) {
		t.Fatal("catch-all policy ignored downstream cancellation")
	}

	success := streamOutcome{logStatusCode: http.StatusOK}
	if continuousRetryStreamSelected(success, nil, "response.completed", policy) {
		t.Fatal("catch-all policy selected a successful terminal response")
	}
	if shouldTransparentRetryStreamWithBudgets(success, &general, &rate, 0, 0, false, nil, nil, policy) {
		t.Fatal("catch-all policy retried a successful terminal response")
	}
}

func TestContinuousRetrySelectivePolicyNeverSelectsStructuredSafetyRefusals(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:     true,
		Categories:  []string{database.ContinuousRetryCategoryHTTP4xx, database.ContinuousRetryCategoryHTTP5xx, database.ContinuousRetryCategoryResponseFailed},
		StatusCodes: []int{http.StatusBadRequest, http.StatusInternalServerError},
		ErrorCodes:  []string{"content_policy_violation"},
	}
	for _, body := range [][]byte{
		[]byte(`{"error":{"code":"cyber_policy"}}`),
		[]byte(`{"error":{"type":"content_policy_violation"}}`),
		[]byte(`{"type":"response.failed","response":{"error":{"code":"moderation_blocked"}}}`),
		[]byte(`{"error":{"code":"invalid_prompt"}}`),
		[]byte(`{"error":{"type":"jailbreak"}}`),
		[]byte(`{"type":"response.failed","response":{"error":{"code":"refusal"}}}`),
		[]byte(`{"response":{"status_details":{"error":{"type":"sanitizer_error"}}}}`),
		[]byte(`{"code":"image_generation_user_error"}`),
		[]byte(`{"type":"unsupported_country_region_territory"}`),
		[]byte(`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"}}}`),
	} {
		if continuousRetryHTTPSelected(policy, http.StatusInternalServerError, body) {
			t.Fatalf("structured safety refusal selected for HTTP retry: %s", body)
		}
		outcome := classifyResponseFailedOutcome(body)
		if continuousRetryStreamSelected(outcome, body, "response.failed", policy) {
			t.Fatalf("structured safety refusal selected for stream retry: %s", body)
		}
		general, rate := 0, 0
		if shouldRetryHTTPStatus(http.StatusInternalServerError, body, &general, &rate, 2, 2, policy) {
			t.Fatalf("structured safety refusal used a legacy HTTP retry: %s", body)
		}
		if general != 0 || rate != 0 {
			t.Fatalf("structured safety refusal consumed retry counters: general=%d rate=%d", general, rate)
		}
		if shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, 2, 2, false, nil, nil, policy) {
			t.Fatalf("structured safety refusal used a legacy stream retry: %s", body)
		}
	}

	ordinary := []byte(`{"error":{"code":"server_error","message":"content policy service temporarily unavailable"}}`)
	if !continuousRetryHTTPSelected(policy, http.StatusInternalServerError, ordinary) {
		t.Fatal("an unstructured message mention suppressed an otherwise selected 500")
	}
}

func TestContinuousRetryCatchAllSelectsStructuredUpstreamSafetyRefusals(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	body := []byte(`{"error":{"code":"content_policy_violation","message":"blocked"}}`)
	if !continuousRetryHTTPSelected(policy, http.StatusForbidden, body) {
		t.Fatal("catch-all did not select a structured upstream safety HTTP failure")
	}
	outcome := classifyResponseFailedOutcome([]byte(`{"type":"response.failed","response":{"status_code":400,"error":{"code":"content_policy_violation"}}}`))
	if !continuousRetryStreamSelected(outcome, outcome.failurePayload, "response.failed", policy) {
		t.Fatal("catch-all did not select a structured upstream safety stream failure")
	}
	err := continuousRetryTestHTTPError{status: http.StatusForbidden, body: body}
	general := 0
	if !shouldRetryRequestError(err, &general, 0, policy) {
		t.Fatal("catch-all did not select a structured upstream safety handshake failure")
	}
}

func TestContinuousRetryCatchAllNeverRetriesExplicitCyberPolicy(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	httpPayload := []byte(`{"error":{"code":"cyber_policy","message":"blocked"}}`)
	if continuousRetryHTTPSelected(policy, http.StatusInternalServerError, httpPayload) {
		t.Fatal("catch-all selected an explicit CYB HTTP failure")
	}
	general, rate := 0, 0
	if shouldRetryHTTPStatus(http.StatusInternalServerError, httpPayload, &general, &rate, -1, -1, policy) {
		t.Fatal("explicit CYB used an already-unlimited HTTP retry budget")
	}

	streamPayload := []byte(`{"type":"response.failed","response":{"status_code":500,"error":{"code":"cyber_policy"}}}`)
	outcome := classifyResponseFailedOutcome(streamPayload)
	outcome.penalize = true // A legacy retryable classification must not bypass the CYB stop.
	if continuousRetryStreamSelected(outcome, streamPayload, "response.failed", policy) {
		t.Fatal("catch-all selected an explicit CYB stream failure")
	}
	if continuousRetryStreamFailureSelected(outcome, streamPayload, "response.failed", policy) {
		t.Fatal("legacy penalize classification bypassed the explicit CYB stream stop")
	}
	if shouldTransparentRetryStreamEventWithBudgets(outcome, "response.failed", &general, &rate, -1, -1, false, nil, nil, policy) {
		t.Fatal("explicit CYB used an already-unlimited stream retry budget")
	}
	kindOnlyOutcome := streamOutcome{logStatusCode: http.StatusInternalServerError, failureKind: "cyber_policy", penalize: true}
	if continuousRetryStreamSelected(kindOnlyOutcome, nil, "error", policy) || continuousRetryStreamFailureSelected(kindOnlyOutcome, nil, "error", policy) {
		t.Fatal("catch-all selected a classified CYB stream outcome without a retained payload")
	}

	handshakeErr := continuousRetryTestHTTPError{status: http.StatusForbidden, body: httpPayload}
	if continuousRetryRequestErrorSelected(policy, handshakeErr) || isRetryableRequestErrorForContext(context.Background(), handshakeErr, policy) {
		t.Fatal("catch-all selected an explicit CYB handshake failure")
	}
	if limit := continuousRetryLimitForRequestError(handshakeErr, 2, policy); limit != 2 {
		t.Fatalf("explicit CYB handshake retry limit = %d, want 2", limit)
	}
	if shouldRetryRequestError(handshakeErr, &general, -1, policy) {
		t.Fatal("explicit CYB handshake used an already-unlimited retry budget")
	}

	statuslessErr := &Error{Code: "cyber_policy", Message: "blocked", Type: ErrorTypeUpstreamError, Retryable: true}
	if continuousRetryRequestErrorSelected(policy, statuslessErr) || isRetryableRequestErrorForContext(context.Background(), statuslessErr, policy) {
		t.Fatal("catch-all selected a statusless explicit CYB error")
	}
	if shouldRetryRequestError(statuslessErr, &general, -1, policy) {
		t.Fatal("statusless explicit CYB used an already-unlimited retry budget")
	}

	imageFailure := newImageResponseFailedError(streamPayload)
	if shouldRetryImageStreamError(imageFailure, &general, -1, 0, maxImageAttempts, policy) {
		t.Fatal("catch-all selected an explicit CYB image stream failure")
	}
	if shouldRetryImageStreamError(statuslessErr, &general, -1, 0, maxImageAttempts, policy) {
		t.Fatal("catch-all selected a statusless explicit CYB image failure")
	}
	if grokMediaInvalidSuccessSelected(policy, httpPayload, nil) {
		t.Fatal("catch-all selected an explicit CYB Grok media error envelope")
	}
	if !grokMediaInvalidSuccessSelected(policy, []byte(`{"error":{"code":"future_media_failure"}}`), nil) {
		t.Fatal("catch-all stopped selecting a non-CYB Grok media failure")
	}
	if general != 0 || rate != 0 {
		t.Fatalf("explicit CYB consumed retry counters: general=%d rate=%d", general, rate)
	}
}

func TestContinuousRetryNeverRetriesStructuredHandshakeSafetyRefusal(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:     true,
		Categories:  []string{database.ContinuousRetryCategoryTransport, database.ContinuousRetryCategoryHTTP4xx},
		StatusCodes: []int{http.StatusForbidden},
		ErrorCodes:  []string{"content_policy_violation"},
	}
	err := continuousRetryTestHTTPError{
		status: http.StatusForbidden,
		body:   []byte(`{"error":{"code":"content_policy_violation"}}`),
	}
	general := 0
	if isRetryableRequestErrorForContext(context.Background(), err, policy) {
		t.Fatal("structured handshake safety refusal was classified as retryable")
	}
	if shouldRetryRequestError(err, &general, 2, policy) {
		t.Fatal("structured handshake safety refusal used a finite or continuous retry")
	}
	if general != 0 {
		t.Fatalf("structured handshake safety refusal consumed general retries: %d", general)
	}
}

func TestContinuousRetryTransportAndStreamSelection(t *testing.T) {
	transport := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	general := 0
	if !shouldRetryRequestError(errors.New("unexpected EOF"), &general, 0, transport) {
		t.Fatal("transport category did not enable transport retry")
	}
	if shouldRetryRequestError(ErrBadRequest("invalid request"), &general, 0, transport) {
		t.Fatal("transport category selected a non-transport error")
	}
	statusless := ErrUpstream(0, "request failed", errors.New("connection reset"))
	if !shouldRetryRequestError(statusless, &general, 0, transport) {
		t.Fatal("transport category did not select a statusless upstream request error")
	}
	serverOnly := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryHTTP5xx},
	}
	if isRetryableRequestErrorForContext(context.Background(), ErrInternalError("serialization failed", errors.New("bad state")), serverOnly) {
		t.Fatal("an internal 500 was misclassified as an upstream 5xx")
	}

	stream := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	outcome := streamOutcome{logStatusCode: http.StatusBadRequest, failureKind: "client", failurePayload: []byte(`{"type":"response.failed"}`)}
	general, rate := 0, 0
	if !shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, 0, 0, false, nil, nil, stream) {
		t.Fatal("response.failed category did not enable a pre-output retry")
	}
	if shouldTransparentRetryStreamWithBudgets(outcome, &general, &rate, -1, -1, true, nil, nil, stream) {
		t.Fatal("response.failed was replayed after downstream output")
	}
}

func TestContinuousRetryStreamErrorDoesNotSelectDeterministicResponseFailed(t *testing.T) {
	defaultPolicy := database.DefaultContinuousRetryPolicy()
	defaultPolicy.Enabled = true
	invalidRequest := []byte(`{"type":"response.failed","response":{"status_code":400,"error":{"code":"invalid_request"}}}`)
	outcome := classifyResponseFailedOutcome(invalidRequest)
	if continuousRetryStreamSelected(outcome, invalidRequest, "response.failed", defaultPolicy) {
		t.Fatal("default stream-error policy selected a deterministic invalid_request response.failed event")
	}

	responseFailedPolicy := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	if !continuousRetryStreamSelected(outcome, invalidRequest, "response.failed", responseFailedPolicy) {
		t.Fatal("explicit response.failed category did not select the event")
	}

	readFailure := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failurePayload: []byte("unexpected EOF"),
	}
	streamErrorPolicy := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryStreamError},
	}
	explicitError := []byte(`{"type":"error","error":{"code":"future_stream_error"}}`)
	if !continuousRetryStreamSelected(classifyResponseFailedOutcome(explicitError), explicitError, "error", streamErrorPolicy) {
		t.Fatal("stream-error category did not select an explicit in-stream error event")
	}
	responseFailed := []byte(`{"type":"response.failed","response":{"status_code":598,"error":{"code":"future_failure"}}}`)
	if continuousRetryStreamSelected(classifyResponseFailedOutcome(responseFailed), responseFailed, "response.failed", streamErrorPolicy) {
		t.Fatal("stream-error category selected response.failed without its own selector")
	}
	if !continuousRetryStreamSelected(readFailure, readFailure.failurePayload, "", streamErrorPolicy) {
		t.Fatal("stream-error category did not select a real stream read failure")
	}
}

func TestContinuousRetryHTTPSelectorsRequireStatusEvidenceForStreamFailures(t *testing.T) {
	serverOnly := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryHTTP5xx},
	}
	for _, payload := range [][]byte{
		[]byte(`{"type":"provider_specific_failure","message":"bad input"}`),
		[]byte(`{"type":"error","error":{"code":"future_unknown_failure"}}`),
	} {
		outcome := classifyResponseFailedOutcome(payload)
		if outcome.logStatusCode != http.StatusInternalServerError {
			t.Fatalf("log fallback status = %d, want 500 for %s", outcome.logStatusCode, payload)
		}
		if continuousRetryStreamSelected(outcome, payload, "error", serverOnly) {
			t.Fatalf("http_5xx selected a stream failure without status evidence: %s", payload)
		}
	}

	transportOutcome := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failurePayload: []byte("unexpected EOF"),
		penalize:       true,
	}
	if continuousRetryStreamSelected(transportOutcome, transportOutcome.failurePayload, "", serverOnly) {
		t.Fatal("http_5xx selected the synthetic stream-break status")
	}

	evidenced := []byte(`{"type":"error","error":{"status_code":503,"code":"future_unknown_failure"}}`)
	if !continuousRetryStreamSelected(classifyResponseFailedOutcome(evidenced), evidenced, "error", serverOnly) {
		t.Fatal("http_5xx did not select an explicit stream status")
	}

	mapped := []byte(`{"type":"error","code":"temporarily_unavailable"}`)
	if !continuousRetryStreamSelected(classifyResponseFailedOutcome(mapped), mapped, "error", serverOnly) {
		t.Fatal("http_5xx did not select a reliably mapped top-level error code")
	}

	exactCode := database.ContinuousRetryPolicy{Enabled: true, ErrorCodes: []string{"future_unknown_failure"}}
	unknown := []byte(`{"type":"error","error":{"code":"future_unknown_failure"}}`)
	if !continuousRetryStreamSelected(classifyResponseFailedOutcome(unknown), unknown, "error", exactCode) {
		t.Fatal("status-evidence guard disabled an exact error-code selector")
	}
}

func TestTerminalUpstreamErrorPayloadSynthesizesEmptyEvent(t *testing.T) {
	payload := terminalUpstreamErrorPayload(nil)
	if gjson.GetBytes(payload, "type").String() != "error" ||
		gjson.GetBytes(payload, "error.code").String() != "upstream_error" {
		t.Fatalf("empty error payload was not normalized: %s", payload)
	}
	original := []byte(`{"type":"error","error":{"code":"custom"}}`)
	if got := terminalUpstreamErrorPayload(original); string(got) != string(original) {
		t.Fatalf("non-empty error payload changed: %s", got)
	}
}

func TestContinuousRetryResponseFailedCategoryUsesTopLevelEventType(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	messageMention := []byte(`{"type":"error","error":{"code":"invalid_request","message":"provider mentioned response.failed in diagnostics"}}`)
	outcome := classifyResponseFailedOutcome(messageMention)
	if continuousRetryStreamSelected(outcome, messageMention, "", policy) {
		t.Fatal("response.failed category matched a diagnostic message instead of the top-level event type")
	}

	actualFailure := []byte(`{"type":"response.failed","response":{"error":{"code":"invalid_request"}}}`)
	outcome = classifyResponseFailedOutcome(actualFailure)
	if !continuousRetryStreamSelected(outcome, actualFailure, "", policy) {
		t.Fatal("response.failed category did not match the top-level response.failed event")
	}
}

func TestContinuousRetrySelectedTransportBypassesStickyAccount(t *testing.T) {
	h, store := newRetryTestHandler(t)
	store.SetTransportRetryPolicy("sticky")
	err := errors.New("connection reset by peer")

	if !h.shouldStickyTransportRetry(err, "transport", false, true, database.ContinuousRetryPolicy{}) {
		t.Fatal("finite retry lost the configured sticky-account behavior")
	}
	transport := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	if h.shouldStickyTransportRetry(err, "transport", false, true, transport) {
		t.Fatal("selected continuous transport failure stayed on the same account")
	}
	if h.shouldStickyTransportRetry(err, "transport", false, true, database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}) {
		t.Fatal("catch-all transport failure stayed on the same account")
	}
}

func TestContinuousRetryImageStreamSelectionHonorsPolicyAndSafety(t *testing.T) {
	policy := database.DefaultContinuousRetryPolicy()
	policy.Enabled = true

	if limit := imageStreamRetryLimit(errors.New("unexpected EOF"), 0, policy); limit != -1 {
		t.Fatalf("image transport retry limit = %d, want -1", limit)
	}
	serverFailure := newImageResponseFailedError([]byte(`{"type":"response.failed","response":{"status_code":503,"error":{"code":"server_error"}}}`))
	if limit := imageStreamRetryLimit(serverFailure, 0, policy); limit != -1 {
		t.Fatalf("image response.failed 503 retry limit = %d, want -1", limit)
	}
	safetyFailure := newImageResponseFailedError([]byte(`{"type":"response.failed","response":{"status_code":500,"error":{"code":"content_policy_violation"}}}`))
	if limit := imageStreamRetryLimit(safetyFailure, 2, policy); limit != 2 {
		t.Fatalf("image safety failure retry limit = %d, want finite limit 2", limit)
	}
	moderationFailure := newImageResponseFailedError([]byte(`{"type":"response.failed","response":{"status_code":500,"error":{"code":"moderation_blocked"}}}`))
	general := 0
	if shouldRetryImageStreamError(moderationFailure, &general, 1, 0, maxImageAttempts, database.ContinuousRetryPolicy{}) {
		t.Fatal("disabled policy retried an explicit image moderation refusal")
	}
	quotaFailure := newImageResponseFailedError([]byte(`{"type":"response.failed","response":{"status_code":429,"error":{"code":"insufficient_quota"}}}`))
	general = 0
	if shouldRetryImageStreamError(moderationFailure, &general, 2, 0, maxImageAttempts, policy) {
		t.Fatal("default continuous policy retried an explicit image moderation refusal")
	}
	general = 0
	if !shouldRetryImageStreamError(quotaFailure, &general, 2, 0, maxImageAttempts, policy) {
		t.Fatal("unselected permanent image quota failure did not preserve the finite legacy retry")
	}
	general = 0
	policy.ErrorCodes = []string{"insufficient_quota"}
	if !shouldRetryImageStreamError(quotaFailure, &general, 0, 0, maxImageAttempts, policy) {
		t.Fatal("explicitly selected image quota code did not use the bounded continuous retry")
	}
	catchAll := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	if !shouldRetryImageStreamError(quotaFailure, &general, 0, 0, maxImageAttempts, catchAll) {
		t.Fatal("catch-all did not select a permanent image quota failure")
	}
	if shouldRetryImageStreamError(moderationFailure, &general, 0, 0, maxImageAttempts, catchAll) {
		t.Fatal("catch-all retried an explicit image moderation refusal")
	}
	if !shouldRetryImageStreamError(quotaFailure, &general, 0, maxImageAttempts-1, maxImageAttempts, catchAll) {
		t.Fatal("catch-all did not bypass the ordinary image attempt cap")
	}
}

func TestContinuousRetryErrorFrameUsesSelectedPolicyBeforeResponseFailed(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:     true,
		StatusCodes: []int{http.StatusForbidden},
	}
	errorFrame := []byte(`{"type":"error","error":{"status_code":403,"code":"forbidden","message":"account restricted"}}`)
	if !isRetryableUpstreamErrorFrame("error", errorFrame, policy) {
		t.Fatal("selected 403 error frame was not classified as retryable")
	}
	if isRetryableUpstreamErrorFrame("error", errorFrame, database.ContinuousRetryPolicy{}) {
		t.Fatal("disabled policy unexpectedly selected a deterministic error frame")
	}
	if isRetryableUpstreamErrorFrame("response.output_text.delta", errorFrame, policy) {
		t.Fatal("non-error event was classified as a retryable error frame")
	}
	typelessError := []byte(`{"error":{"status_code":403,"code":"forbidden"}}`)
	if !isRetryableUpstreamErrorFrame("error", typelessError, policy) {
		t.Fatal("SSE event:error without a JSON lifecycle type was not selected")
	}
	if !isRetryableUpstreamErrorFrame("", typelessError, policy) {
		t.Fatal("top-level error object without a lifecycle type was not selected")
	}
}

func TestContinuousRetryBackoffStateUsesSelectedBodyPolicy(t *testing.T) {
	policy := database.ContinuousRetryPolicy{
		Enabled:    true,
		ErrorCodes: []string{"account_temporarily_unavailable"},
	}
	general, rate := 1, 0
	ordinal, limit := retryStateForHTTPStatusWithBody(http.StatusBadRequest,
		[]byte(`{"error":{"code":"account_temporarily_unavailable"}}`),
		general, rate, 0, 0, policy)
	if ordinal != general || limit != -1 {
		t.Fatalf("selected exact-code HTTP state = (%d, %d), want (%d, -1)", ordinal, limit, general)
	}

	outcome := streamOutcome{
		logStatusCode:  http.StatusForbidden,
		failureKind:    "forbidden",
		failurePayload: []byte(`{"type":"response.failed","response":{"status_code":403}}`),
	}
	ordinal, limit = retryStateForStreamOutcome(outcome, 2, 0, 0, 0, database.ContinuousRetryPolicy{
		Enabled:     true,
		StatusCodes: []int{http.StatusForbidden},
	})
	if ordinal != 2 || limit != -1 {
		t.Fatalf("selected stream state = (%d, %d), want (2, -1)", ordinal, limit)
	}
}

func TestContinuousRetryRequestErrorCanSelectHandshakeStatus(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, StatusCodes: []int{http.StatusForbidden}}
	err := continuousRetryTestHTTPError{
		status: http.StatusForbidden,
		body:   []byte(`{"error":{"code":"cloudflare_forbidden","message":"blocked"}}`),
	}
	general := 0
	if !shouldRetryRequestError(err, &general, 0, policy) {
		t.Fatal("selected handshake 403 was not retried with the legacy budget disabled")
	}
	if general != 1 {
		t.Fatalf("handshake retry counter = %d, want 1", general)
	}

	transportOnly := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	general = 0
	if !shouldRetryRequestError(err, &general, 1, transportOnly) {
		t.Fatal("unselected handshake 403 lost its legacy finite transport retry")
	}
	if shouldRetryRequestError(err, &general, 1, transportOnly) {
		t.Fatal("transport category promoted an unselected handshake 403 to continuous retry")
	}
}

func TestContinuousRetryRequestErrorCanPromoteStructuredStatusFailure(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, StatusCodes: []int{http.StatusForbidden}}
	err := ErrUpstream(http.StatusForbidden, "account restricted", errors.New("upstream denied"))
	if isRetryableRequestErrorForContext(context.Background(), err, policy) != true {
		t.Fatal("selected structured 403 was not classified as retryable")
	}
	if isRetryableRequestErrorForContext(context.Background(), err) {
		t.Fatal("structured 403 became retryable without an opt-in policy")
	}
}

func TestContinuousRetryCatchAllSelectsOnlyStructuredStatuslessUpstreamErrors(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	upstreamErr := ErrUpstream(0, "upstream failed without a classified cause", nil)
	if !isRetryableRequestErrorForContext(context.Background(), upstreamErr, policy) {
		t.Fatal("catch-all did not select a statusless structured upstream error")
	}
	general := 0
	if !shouldRetryRequestError(upstreamErr, &general, 0, policy) {
		t.Fatal("catch-all did not override the finite budget for a statusless structured upstream error")
	}
	if isRetryableRequestErrorForContext(context.Background(), ErrBadRequest("invalid local request"), policy) {
		t.Fatal("catch-all selected an internal bad-request error")
	}
	if isRetryableRequestErrorForContext(context.Background(), ErrInternalError("internal failure", nil), policy) {
		t.Fatal("catch-all selected an internal server error")
	}
	safetyErr := &Error{
		Code:    "cyber_policy",
		Message: "blocked",
		Type:    ErrorTypeUpstreamError,
	}
	if isRetryableRequestErrorForContext(context.Background(), safetyErr, policy) {
		t.Fatal("catch-all selected a statusless explicit CYB refusal")
	}
	canceled := ErrUpstream(0, "upstream request canceled", context.Canceled)
	if isRetryableRequestErrorForContext(context.Background(), canceled, policy) {
		t.Fatal("catch-all selected a canceled upstream request")
	}
	deadline := ErrUpstream(0, "upstream request timed out", context.DeadlineExceeded)
	general = 0
	if !shouldRetryRequestError(deadline, &general, 0, policy) {
		t.Fatal("catch-all did not select an upstream deadline while the downstream context remained active")
	}
	downstreamCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if isRetryableRequestErrorForContext(downstreamCtx, deadline, policy) {
		t.Fatal("catch-all ignored a canceled downstream context")
	}
}

func TestContinuousRetryCatchAllSelectsNonstandardHandshakeStatus(t *testing.T) {
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	err := continuousRetryTestHTTPError{
		status: 701,
		body:   []byte(`{"error":{"code":"future_handshake_failure"}}`),
	}
	general := 0
	if !shouldRetryRequestError(err, &general, 0, policy) {
		t.Fatal("catch-all did not select a nonstandard upstream handshake status")
	}
}
