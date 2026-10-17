package proxy

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

func (t *anthropicStreamTranslator) checkToolInputItem(id string) bool {
	if id == "" || t.currentToolItemID == "" || id == t.currentToolItemID {
		return true
	}
	t.toolInputError = fmt.Errorf("tool arguments refer to a different output item")
	return false
}

func (t *anthropicStreamTranslator) finishFunctionToolInput(final gjson.Result) []anthropicStreamEvent {
	current := t.currentToolInputBuffer.String()
	candidate := current
	if final.Exists() {
		if final.Type != gjson.String {
			t.toolInputError = fmt.Errorf("function tool arguments must be a JSON object encoded as text")
			return nil
		}
		if final.String() != "" {
			candidate = final.String()
		}
	}
	if len(candidate) > responseCacheMaxEntry {
		t.toolInputError = fmt.Errorf("function tool arguments exceed the size limit")
		return nil
	}
	if !strings.HasPrefix(candidate, current) || (t.currentToolInputFinalized && candidate != current) {
		t.toolInputError = fmt.Errorf("function tool arguments do not match streamed deltas")
		return nil
	}
	if candidate != "" && (!gjson.Valid(candidate) || !gjson.Parse(candidate).IsObject()) {
		t.toolInputError = fmt.Errorf("function tool arguments must contain a valid JSON object")
		return nil
	}
	var events []anthropicStreamEvent
	if candidate != current {
		// Only send bytes absent from the stream; complete deltas must never be replayed.
		suffix := candidate[len(current):]
		t.currentToolInputBuffer.WriteString(suffix)
		events = t.contentBlockDelta(anthropicDelta{Type: "input_json_delta", PartialJSON: suffix})
	}
	t.currentToolInputFinalized = true
	return events
}
