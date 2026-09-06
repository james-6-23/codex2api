package proxy

import (
	"fmt"
	"net/http"
	"testing"
)

func TestContinuationUnavailableErrorEnvelopes(t *testing.T) {
	const message = "previous_response_id is not available for this user"
	for _, payload := range []string{
		fmt.Sprintf(`{"error":{"message":%q}}`, message),
		fmt.Sprintf(`{"type":"error","message":%q}`, message),
		fmt.Sprintf(`{"type":"response.failed","response":{"error":{"message":%q}}}`, message),
	} {
		if !isPreviousResponseNotFoundBody([]byte(payload)) {
			t.Errorf("continuation error was not recognized: %s", payload)
		}
		if status := responseFailedStatusCode([]byte(payload)); status != http.StatusBadRequest {
			t.Errorf("continuation error status = %d, want 400", status)
		}
	}
	for _, payload := range []string{
		fmt.Sprintf(`{"input":%q,"error":{"message":"server unavailable"}}`, message),
		fmt.Sprintf(`{"error":{"message":%q}}`, "explanation: "+message),
		`{"error":{"message":"model is not available for this user"}}`,
		`{"type":"response.completed","response":{"output":[{"text":"previous_response_id is not available for this user"}]}}`,
	} {
		if isPreviousResponseNotFoundBody([]byte(payload)) {
			t.Errorf("unrelated error was treated as continuation failure: %s", payload)
		}
	}
}
