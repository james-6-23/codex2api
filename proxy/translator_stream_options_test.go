package proxy

import (
	"encoding/json"
	"testing"
)

func TestPrepareResponsesBodyPreservesReasoningSummaryDelivery(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","input":[],"stream_options":{"reasoning_summary_delivery":"sequential_cutoff","include_usage":true}}`)
	result, _ := prepareResponsesBodyWithOptions(raw, responsesBodyPrepareOptions{skipExpandedInput: true})
	var got map[string]any
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	options, ok := got["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing after translation: %s", result)
	}
	if options["reasoning_summary_delivery"] != "sequential_cutoff" {
		t.Fatalf("reasoning_summary_delivery not preserved: %v", options)
	}
	if _, exists := options["include_usage"]; exists {
		t.Fatalf("unsupported stream_options subfield must still be stripped: %v", options)
	}

	rawNoDelivery := []byte(`{"model":"gpt-5","input":[],"stream_options":{"include_usage":true}}`)
	result, _ = prepareResponsesBodyWithOptions(rawNoDelivery, responsesBodyPrepareOptions{skipExpandedInput: true})
	got = nil
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, exists := got["stream_options"]; exists {
		t.Fatalf("stream_options should be dropped without reasoning_summary_delivery: %s", result)
	}
}

func TestValidateCodexRelayResponsesCompatibilityAllowsReasoningSummaryDelivery(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","stream_options":{"reasoning_summary_delivery":"sequential_cutoff"}}`)
	if err := validateCodexRelayResponsesCompatibility(raw); err != nil {
		t.Fatalf("known subfield should not be flagged in strict mode: %v", err)
	}
	raw = []byte(`{"model":"gpt-5","stream_options":{"reasoning_summary_delivery":"sequential_cutoff","include_usage":true}}`)
	if err := validateCodexRelayResponsesCompatibility(raw); err == nil {
		t.Fatalf("unsupported stream_options subfields must still fail strict validation")
	}
}
