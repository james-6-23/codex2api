package proxy

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRestoreResponseOutputPreservesUnmodifiedRawFields(t *testing.T) {
	response := []byte(`{"id":"resp_test","usage":{"attribution":{"large_integer":9007199254740993,"decimal":1.2300}},"metadata": { "literal": "<unchanged>" },"output":[]}`)
	items := []json.RawMessage{json.RawMessage(`{"type":"message","id":"msg_test","metadata":{"integer":9007199254740993},"content":[]}`)}
	got := restoreMissingResponseOutputs(response, items)
	for _, path := range []string{"usage", "metadata"} {
		if gjson.GetBytes(got, path).Raw != gjson.GetBytes(response, path).Raw {
			t.Fatalf("unmodified %s was re-encoded", path)
		}
	}
	if gjson.GetBytes(got, "output.0.metadata.integer").Raw != "9007199254740993" {
		t.Fatal("output item integer lost precision")
	}
	complete := restoreMissingResponseOutputs(got, items)
	if len(complete) != len(got) || &complete[0] != &got[0] {
		t.Fatal("complete output was needlessly copied")
	}
}

func TestRestoreResponseOutputRejectsInvalidResponse(t *testing.T) {
	items := []json.RawMessage{json.RawMessage(`{"type":"message","content":[]}`)}
	for _, raw := range []string{"null", "[]", "not-json", `{"output":[]`, `{"output":[]} trailing`} {
		input := []byte(raw)
		if got := restoreMissingResponseOutputs(input, items); !bytes.Equal(got, input) {
			t.Fatalf("invalid response changed: %q", raw)
		}
	}
}

func TestResponsesPreparedInputMatchesFinalBody(t *testing.T) {
	for _, raw := range []string{
		`{"model":"gpt-6-astra","input":"hello"}`,
		`{"model":"gpt-6-astra","input":null}`,
		`{"model":"gpt-6-astra"}`,
		`{"model":"gpt-6-astra","previous_response_id":"resp_prev","input":[{"type":"custom_tool_call_output","call_id":"call_test","output":"<result>"}]}`,
		`{"model":"gpt-6-astra","input":[{"type":"message","role":"user","content":"hello"},{"type":"compaction_trigger"}]}`,
	} {
		body, input := PrepareResponsesWebSocketBody([]byte(raw))
		if input != gjson.GetBytes(body, "input").Raw {
			t.Fatalf("cached input differs from final request input for %s", raw)
		}
	}
}
