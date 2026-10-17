package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestAnthropicFunctionInputDoneRecoversArguments(t *testing.T) {
	arguments := `{"query":"select:WebSearch","max_results":1}`
	added := []byte(`{"type":"response.output_item.added","item":{"id":"fc_search","type":"function_call","call_id":"fc_toolu_search","name":"ToolSearch"}}`)
	done := customToolTestEvent("response.function_call_arguments.done", map[string]any{"item_id": "fc_search", "arguments": arguments})
	itemDone := customToolTestEvent("response.output_item.done", map[string]any{"item": map[string]any{"id": "fc_search", "type": "function_call", "call_id": "fc_toolu_search", "name": "ToolSearch", "arguments": arguments}})
	completed := customToolTestEvent("response.completed", map[string]any{"response": map[string]any{"status": "completed", "output": []any{map[string]any{"id": "fc_search", "type": "function_call", "call_id": "fc_toolu_search", "name": "ToolSearch", "arguments": arguments}}}})
	for name, events := range map[string][][]byte{
		"arguments done only": {added, done, itemDone, completed},
		"item done only":      {added, itemDone, completed},
		"completed only":      {added, completed},
		"partial delta":       {added, customToolTestEvent("response.function_call_arguments.delta", map[string]any{"item_id": "fc_search", "delta": `{"query":"`}), done, itemDone, completed},
		"complete deltas":     {added, customToolTestEvent("response.function_call_arguments.delta", map[string]any{"item_id": "fc_search", "delta": arguments}), done, done, itemDone, completed},
	} {
		t.Run(name, func(t *testing.T) {
			translator := newAnthropicStreamTranslator("claude-sonnet-4-6")
			accumulator := newAnthropicResponseAccumulator("claude-sonnet-4-6")
			var streamed strings.Builder
			for _, event := range events {
				translated := translator.translateEvent(event)
				for _, item := range translated {
					if item.Delta != nil && item.Delta.Type == "input_json_delta" {
						streamed.WriteString(item.Delta.PartialJSON)
					}
				}
				accumulator.apply(translated)
			}
			if translator.toolInputError != nil {
				t.Fatal(translator.toolInputError)
			}
			if streamed.String() != arguments {
				t.Fatalf("streamed arguments = %q, want %q", streamed.String(), arguments)
			}
			response := accumulator.build(completed)
			if len(response.Content) != 1 || string(response.Content[0].Input) != arguments || response.Content[0].ID != "toolu_search" {
				t.Fatalf("tool input or call identity was lost: %+v", response.Content)
			}
		})
	}
}

func TestAnthropicFunctionInputDoneRejectsInvalidArguments(t *testing.T) {
	for name, final := range map[string]any{
		"divergent":  `{"other":1}`,
		"malformed":  `{"query":`,
		"array":      `[1,2]`,
		"non-string": map[string]any{"query": "read"},
	} {
		t.Run(name, func(t *testing.T) {
			translator := newAnthropicStreamTranslator("claude-sonnet-4-6")
			translator.translateEvent([]byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_search","call_id":"fc_toolu_search","name":"ToolSearch"}}`))
			translator.translateEvent(customToolTestEvent("response.function_call_arguments.delta", map[string]any{"delta": `{"query":"`}))
			translator.translateEvent(customToolTestEvent("response.function_call_arguments.done", map[string]any{"item_id": "fc_search", "arguments": final}))
			if translator.toolInputError == nil {
				t.Fatal("invalid canonical arguments were accepted")
			}
			if events := translator.translateEvent([]byte(`{"type":"response.completed","response":{"output":[]}}`)); len(events) != 0 {
				t.Fatal("invalid arguments produced a successful terminal event")
			}
		})
	}
	t.Run("delta after done", func(t *testing.T) {
		translator := newAnthropicStreamTranslator("claude-sonnet-4-6")
		translator.translateEvent([]byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_search","name":"ToolSearch"}}`))
		translator.translateEvent([]byte(`{"type":"response.function_call_arguments.done","item_id":"fc_search","arguments":"{}"}`))
		translator.translateEvent([]byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_search","delta":"{}"}`))
		if translator.toolInputError == nil {
			t.Fatal("arguments continued after the done event")
		}
	})
	t.Run("different item", func(t *testing.T) {
		translator := newAnthropicStreamTranslator("claude-sonnet-4-6")
		translator.translateEvent([]byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_search","name":"ToolSearch"}}`))
		translator.translateEvent([]byte(`{"type":"response.function_call_arguments.done","item_id":"fc_other","arguments":"{}"}`))
		if translator.toolInputError == nil {
			t.Fatal("arguments from another item were attributed to the current call")
		}
	})
	t.Run("oversized final", func(t *testing.T) {
		translator := newAnthropicStreamTranslator("claude-sonnet-4-6")
		translator.finishFunctionToolInput(gjson.Result{Type: gjson.String, Str: strings.Repeat("x", responseCacheMaxEntry+1)})
		if translator.toolInputError == nil {
			t.Fatal("oversized completed arguments were accepted")
		}
	})
	t.Run("oversized delta", func(t *testing.T) {
		translator := newAnthropicStreamTranslator("claude-sonnet-4-6")
		translator.translateEvent([]byte(`{"type":"response.output_item.added","item":{"type":"function_call","name":"ToolSearch"}}`))
		translator.currentToolInputBuffer.WriteString(strings.Repeat("x", responseCacheMaxEntry))
		translator.translateEvent([]byte(`{"type":"response.function_call_arguments.delta","delta":"x"}`))
		if translator.toolInputError == nil {
			t.Fatal("oversized streamed arguments were accepted")
		}
	})
}

func TestMessagesFunctionInputDoneRecoversArguments(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			handler, _ := newAnthropicStreamFailureTestHandler(t, func(_ int32, w http.ResponseWriter) {
				writeCodexSSE(w,
					`{"type":"response.output_item.added","item":{"id":"fc_search","type":"function_call","call_id":"fc_toolu_search","name":"ToolSearch"}}`,
					`{"type":"response.function_call_arguments.done","item_id":"fc_search","arguments":"{\"query\":\"select:WebSearch\"}"}`,
					`{"type":"response.output_item.done","item":{"id":"fc_search","type":"function_call","call_id":"fc_toolu_search","name":"ToolSearch","arguments":"{\"query\":\"select:WebSearch\"}"}}`,
					`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
				)
			})
			previous := CurrentRuntimeSettings()
			runtime := previous
			runtime.CodexForceWebsocket = false
			ApplyRuntimeSettings(runtime)
			t.Cleanup(func() { ApplyRuntimeSettings(previous) })
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"claude-sonnet-4-6","stream":%t,"messages":[{"role":"user","content":"Find WebSearch."}]}`, stream)))
			handler.Messages(ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if stream {
				if !strings.Contains(recorder.Body.String(), `\"query\":\"select:WebSearch\"`) || !strings.Contains(recorder.Body.String(), "event: message_stop") {
					t.Fatalf("stream lost completed tool arguments: %s", recorder.Body.String())
				}
			} else if gjson.GetBytes(recorder.Body.Bytes(), "content.0.input.query").String() != "select:WebSearch" {
				t.Fatalf("nonstream response lost completed tool arguments: %s", recorder.Body.String())
			}
		})
	}
}
