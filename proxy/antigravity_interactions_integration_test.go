package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

const (
	antigravityInteractionsTestKeyEnv    = "ANTIGRAVITY_INTERACTIONS_TEST_API_KEY"
	antigravityInteractionsTestModelEnv  = "ANTIGRAVITY_INTERACTIONS_TEST_MODEL"
	antigravityInteractionsTestInputEnv  = "ANTIGRAVITY_INTERACTIONS_TEST_INPUT"
	antigravityInteractionsTestStreamEnv = "ANTIGRAVITY_INTERACTIONS_TEST_STREAM"
	antigravityInteractionsTestBodyLimit = 256 << 10
)

var safeAntigravityIntegrationModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func TestAntigravityInteractionsRealUpstream(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv(antigravityInteractionsTestKeyEnv))
	if apiKey == "" {
		t.Skip("real Google Interactions test skipped: set " + antigravityInteractionsTestKeyEnv + " explicitly to run it")
	}
	model := strings.TrimSpace(os.Getenv(antigravityInteractionsTestModelEnv))
	if model == "" {
		model = "gemini-2.5-flash"
	}
	if !safeAntigravityIntegrationModel.MatchString(model) {
		t.Fatalf("%s is not a safe model identifier", antigravityInteractionsTestModelEnv)
	}
	input := strings.TrimSpace(os.Getenv(antigravityInteractionsTestInputEnv))
	if input == "" {
		input = "Reply with OK."
	}
	if len(input) > 512 {
		t.Fatalf("%s exceeds 512 bytes", antigravityInteractionsTestInputEnv)
	}
	payload, err := json.Marshal(map[string]any{"input": input, "max_output_tokens": 8})
	if err != nil {
		t.Fatal(err)
	}
	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamAntigravity, APIKey: apiKey}

	t.Run("json", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		response, requestErr := ExecuteAntigravityResponsesRequest(ctx, account, model, payload, false, "")
		if requestErr != nil {
			t.Fatalf("Google Interactions JSON request failed: %s", redactedAntigravityIntegrationDiagnostic(requestErr.Error(), apiKey))
		}
		defer response.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(response.Body, antigravityInteractionsTestBodyLimit+1))
		if readErr != nil {
			t.Fatalf("read Google Interactions JSON response: %v", readErr)
		}
		if len(body) > antigravityInteractionsTestBodyLimit {
			t.Fatalf("Google Interactions JSON response exceeded %d bytes", antigravityInteractionsTestBodyLimit)
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			t.Fatalf("Google Interactions JSON status=%d content-type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), boundedAntigravityIntegrationBody(body, apiKey))
		}
		mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if parseErr != nil || !strings.Contains(strings.ToLower(mediaType), "json") {
			t.Fatalf("Google Interactions JSON content-type=%q", response.Header.Get("Content-Type"))
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil || len(envelope) == 0 {
			t.Fatalf("Google Interactions returned an invalid/empty JSON envelope: %s", boundedAntigravityIntegrationBody(body, apiKey))
		}
	})

	streamEnabled, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv(antigravityInteractionsTestStreamEnv)))
	t.Run("sse", func(t *testing.T) {
		if !streamEnabled {
			t.Skip("real Google Interactions SSE test skipped: set " + antigravityInteractionsTestStreamEnv + "=true to enable it")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		response, requestErr := ExecuteAntigravityResponsesRequest(ctx, account, model, payload, true, "")
		if requestErr != nil {
			t.Fatalf("Google Interactions SSE request failed: %s", redactedAntigravityIntegrationDiagnostic(requestErr.Error(), apiKey))
		}
		defer response.Body.Close()
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			t.Fatalf("Google Interactions SSE status=%d content-type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), boundedAntigravityIntegrationBody(body, apiKey))
		}
		mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if parseErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
			t.Fatalf("Google Interactions SSE content-type=%q", response.Header.Get("Content-Type"))
		}
		if err := validateAntigravityInteractionSSE(response.Body); err != nil {
			t.Fatalf("Google Interactions SSE envelope invalid: %v", err)
		}
	})
}

func validateAntigravityInteractionSSE(reader io.Reader) error {
	scanner := bufio.NewScanner(io.LimitReader(reader, antigravityInteractionsTestBodyLimit+1))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	dataEvents := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil || len(envelope) == 0 {
			return fmt.Errorf("invalid data event JSON")
		}
		dataEvents++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if dataEvents == 0 {
		return fmt.Errorf("no JSON data events observed")
	}
	return nil
}

func boundedAntigravityIntegrationBody(body []byte, apiKey string) string {
	body = bytes.TrimSpace(body)
	if len(body) > 4096 {
		body = body[:4096]
	}
	return redactedAntigravityIntegrationDiagnostic(string(body), apiKey)
}

func redactedAntigravityIntegrationDiagnostic(value, apiKey string) string {
	if apiKey != "" {
		value = strings.ReplaceAll(value, apiKey, "[REDACTED_API_KEY]")
	}
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 4096 {
		value = value[:4096]
	}
	return value
}
