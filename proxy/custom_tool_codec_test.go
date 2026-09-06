package proxy

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func customToolTestEvent(eventType string, fields map[string]any) []byte {
	fields["type"] = eventType
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestCustomToolChatRoundTrip(t *testing.T) {
	for _, input := range []string{"print(\"你好\")\ntext('\\path')", "", `{"input":"already JSON but still a custom text input"}`} {
		t.Run(input, func(t *testing.T) {
			st := NewStreamTranslator("chatcmpl-test", "gpt-6-astra", 0)
			added, _ := st.Translate(customToolTestEvent("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "custom_tool_call", "id": "ctc_test", "call_id": "call_test", "name": "exec"}}))
			if gjson.GetBytes(added, "choices.0.delta.tool_calls.0.type").String() != "custom" {
				t.Fatalf("custom tool was retyped: %s", added)
			}
			var reconstructed strings.Builder
			runes := []rune(input)
			for _, fragment := range []string{string(runes[:len(runes)/2]), string(runes[len(runes)/2:])} {
				chunk, _ := st.Translate(customToolTestEvent("response.custom_tool_call_input.delta", map[string]any{"item_id": "ctc_test", "delta": fragment}))
				reconstructed.WriteString(gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.custom.input").String())
			}
			chunk, done := st.Translate(customToolTestEvent("response.custom_tool_call_input.done", map[string]any{"item_id": "ctc_test", "input": input}))
			if done || len(chunk) != 0 {
				t.Fatalf("done duplicated input or failed: %s", chunk)
			}
			if reconstructed.String() != input {
				t.Fatal("custom input changed during streaming")
			}
			request := map[string]any{"model": "gpt-6-astra", "tools": []any{map[string]any{"type": "custom", "custom": map[string]any{"name": "exec", "format": map[string]any{"type": "text"}}}}, "tool_choice": map[string]any{"type": "custom", "custom": map[string]any{"name": "exec"}}, "messages": []any{
				map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_test", "type": "custom", "custom": map[string]any{"name": "exec", "input": input}}}},
				map[string]any{"role": "tool", "tool_call_id": "call_test", "content": "ok"},
			}}
			raw, _ := json.Marshal(request)
			translated, err := TranslateRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(translated, "input.0.type").String() != "custom_tool_call" || gjson.GetBytes(translated, "input.0.input").String() != input || gjson.GetBytes(translated, "input.1.type").String() != "custom_tool_call_output" {
				t.Fatalf("custom history lost: %s", translated)
			}
			if gjson.GetBytes(translated, "tools.0.name").String() != "exec" || gjson.GetBytes(translated, "tool_choice.name").String() != "exec" {
				t.Fatalf("custom declaration or choice was not lowered: %s", translated)
			}
			completed := customToolTestEvent("response.completed", map[string]any{"response": map[string]any{"output": []any{map[string]any{"type": "custom_tool_call", "call_id": "call_test", "name": "exec", "input": input}}}})
			calls, err := ExtractToolCallsFromOutputValidated(completed)
			if err != nil {
				t.Fatal(err)
			}
			compact := BuildCompactResponse("chatcmpl-test", "gpt-6-astra", 0, "", "", calls, nil)
			if gjson.GetBytes(compact, "choices.0.message.tool_calls.0.type").String() != "custom" || gjson.GetBytes(compact, "choices.0.message.tool_calls.0.custom.input").String() != input {
				t.Fatalf("custom compact output lost: %s", compact)
			}
		})
	}
}

func TestCustomToolStreamReconcilesDone(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-6-astra", 0)
	st.Translate([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_test","call_id":"call_test","name":"exec"}}`))
	st.Translate([]byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_test","delta":"print("}`))
	chunk, done := st.Translate([]byte(`{"type":"response.custom_tool_call_input.done","item_id":"ctc_test","input":"print(1)"}`))
	if done || gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.custom.input").String() != "1)" {
		t.Fatalf("missing suffix not restored: %s", chunk)
	}
	_, done = st.Translate([]byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_test","delta":"poison"}`))
	if !done || st.ToolArgumentsError() == nil {
		t.Fatal("input continued after done")
	}
}

func TestCustomToolAnthropicRoundTrip(t *testing.T) {
	for _, input := range []string{"print(\"你好\")\ntext('\\path')", "", `{"query":"hello"}`} {
		t.Run(input, func(t *testing.T) {
			translator := newAnthropicStreamTranslator("gpt-6-astra")
			accumulator := newAnthropicResponseAccumulator("gpt-6-astra")
			item := map[string]any{"type": "custom_tool_call", "id": "ctc_test", "call_id": "call_original", "namespace": "functions", "name": "exec", "input": input}
			accumulator.apply(translator.translateEvent(customToolTestEvent("response.output_item.added", map[string]any{"item": item})))
			runes := []rune(input)
			prefix := string(runes[:len(runes)/2])
			accumulator.apply(translator.translateEvent(customToolTestEvent("response.custom_tool_call_input.delta", map[string]any{"item_id": "ctc_test", "delta": prefix})))
			// The canonical done event supplies a deliberately omitted suffix.
			accumulator.apply(translator.translateEvent(customToolTestEvent("response.custom_tool_call_input.done", map[string]any{"item_id": "ctc_test", "input": input})))
			accumulator.apply(translator.translateEvent(customToolTestEvent("response.output_item.done", map[string]any{"item": item})))
			completed := []byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`)
			accumulator.apply(translator.translateEvent(completed))
			if translator.toolInputError != nil {
				t.Fatal(translator.toolInputError)
			}
			response := accumulator.build(completed)
			if len(response.Content) != 1 || !json.Valid(response.Content[0].Input) || gjson.GetBytes(response.Content[0].Input, "input").String() != input {
				t.Fatalf("custom input wrapper failed: %+v", response.Content)
			}
			block := response.Content[0]
			messages := []any{map[string]any{"role": "assistant", "content": response.Content}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": block.ID, "content": "ok"}}}}
			request, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "messages": messages, "tool_choice": map[string]any{"type": "tool", "name": "exec"}, "tools": []any{map[string]any{"name": "exec", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}}}}})
			body, _, err := TranslateAnthropicToCodexWithModels(request, "", []string{"gpt-6-astra"})
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(body, "input.0.type").String() != "custom_tool_call" || gjson.GetBytes(body, "input.0.call_id").String() != "call_original" || gjson.GetBytes(body, "input.0.input").String() != input || gjson.GetBytes(body, "input.0.namespace").String() != "functions" || gjson.GetBytes(body, "input.1.type").String() != "custom_tool_call_output" || gjson.GetBytes(body, "input.1.call_id").String() != "call_original" {
				t.Fatalf("custom Messages history failed to round-trip: %s", body)
			}
			if gjson.GetBytes(body, "tool_choice.type").String() != "custom" || gjson.GetBytes(body, "tool_choice.namespace").String() != "functions" {
				t.Fatalf("custom tool choice not restored: %s", body)
			}
			if gjson.GetBytes(body, "tools.0.type").String() != "namespace" || gjson.GetBytes(body, "tools.0.name").String() != "functions" || gjson.GetBytes(body, "tools.0.tools.0.type").String() != "custom" {
				t.Fatalf("wrapper declaration was not restored: %s", body)
			}
			nonStream := buildAnthropicResponseFromCompleted(customToolTestEvent("response.completed", map[string]any{"response": map[string]any{"output": []any{item}}}), "gpt-6-astra")
			if len(nonStream.Content) != 1 || nonStream.Content[0].ID != block.ID || gjson.GetBytes(nonStream.Content[0].Input, "input").String() != input {
				t.Fatal("streaming and non-streaming custom representations differ")
			}
		})
	}
}

func TestCustomToolAnthropicRejectsDivergentDone(t *testing.T) {
	translator := newAnthropicStreamTranslator("gpt-6-astra")
	translator.translateEvent([]byte(`{"type":"response.output_item.added","item":{"type":"custom_tool_call","id":"ctc_test","call_id":"call_test","name":"exec"}}`))
	translator.translateEvent([]byte(`{"type":"response.custom_tool_call_input.delta","delta":"print("}`))
	translator.translateEvent([]byte(`{"type":"response.custom_tool_call_input.done","input":"different()"}`))
	if translator.toolInputError == nil {
		t.Fatal("divergent canonical input was accepted")
	}
	if events := translator.translateEvent([]byte(`{"type":"response.completed","response":{"output":[]}}`)); len(events) != 0 {
		t.Fatal("invalid input produced a success terminal")
	}
}

func TestCustomToolChatNonStreamRebuildsEmptyTerminalOutput(t *testing.T) {
	handler, calls := newChatStreamTerminalTestHandler(t, []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_test","call_id":"call_test","name":"exec","input":"print(\"你好\")"}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.4","stream":false,"messages":[{"role":"user","content":"run the custom tool"}],"tools":[{"type":"custom","custom":{"name":"exec","format":{"type":"text"}}}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.ChatCompletions(ctx)
	if recorder.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("unexpected request outcome: %d %s", recorder.Code, recorder.Body.String())
	}
	tool := gjson.GetBytes(recorder.Body.Bytes(), "choices.0.message.tool_calls.0")
	if tool.Get("type").String() != "custom" || tool.Get("custom.input").String() != `print("你好")` {
		t.Fatalf("custom call was lost from the empty completed output: %s", recorder.Body.String())
	}
}
