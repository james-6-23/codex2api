package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeServiceTierField(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.4","serviceTier":"fast"}`)

	got := normalizeServiceTierField(raw)

	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "fast" {
		t.Fatalf("service_tier mismatch: got %q want %q", tier, "fast")
	}
	if gjson.GetBytes(got, "serviceTier").Exists() {
		t.Fatal("serviceTier should be removed after normalization")
	}
}

func TestResolveServiceTier(t *testing.T) {
	if got := resolveServiceTier("fast", "default"); got != "fast" {
		t.Fatalf("expected actual tier to win, got %q", got)
	}
	if got := resolveServiceTier("", "fast"); got != "fast" {
		t.Fatalf("expected requested tier fallback, got %q", got)
	}
	if got := resolveServiceTier("default", "fast"); got != "fast" {
		t.Fatalf("expected requested fast to win for logging, got %q", got)
	}
}

func TestSanitizeServiceTierForUpstream_FastToPriority(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"service_tier":"fast"
	}`)

	got := sanitizeServiceTierForUpstream(raw)

	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("fast should be mapped to priority for upstream, got %q", tier)
	}
}

func TestTranslateRequest_PreservesSupportedServiceTier(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hello"}],
		"serviceTier":"priority",
		"reasoning_effort":"high"
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("service_tier mismatch: got %q want %q", tier, "priority")
	}
	if gjson.GetBytes(got, "serviceTier").Exists() {
		t.Fatal("serviceTier should not be present after translation")
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "high" {
		t.Fatalf("reasoning.effort mismatch: got %q want %q", effort, "high")
	}
}

// ==================== Function Calling 测试 ====================

func TestConvertMessagesToInput_ToolRole(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"tool","tool_call_id":"call_abc","content":"{\"temp\":72}"}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}

	input := gjson.GetBytes(got, "input")
	if !input.IsArray() {
		t.Fatal("input should be an array")
	}

	item := input.Array()[0]
	if item.Get("type").String() != "function_call_output" {
		t.Fatalf("expected type function_call_output, got %q", item.Get("type").String())
	}
	if item.Get("call_id").String() != "call_abc" {
		t.Fatalf("expected call_id call_abc, got %q", item.Get("call_id").String())
	}
	if item.Get("output").String() != `{"temp":72}` {
		t.Fatalf("expected output to match, got %q", item.Get("output").String())
	}
}

func TestConvertMessagesToInput_AssistantWithToolCalls(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_123","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}
			]}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}

	input := gjson.GetBytes(got, "input")
	items := input.Array()
	if len(items) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(items))
	}

	fc := items[0]
	if fc.Get("type").String() != "function_call" {
		t.Fatalf("expected type function_call, got %q", fc.Get("type").String())
	}
	if fc.Get("call_id").String() != "call_123" {
		t.Fatalf("expected call_id call_123, got %q", fc.Get("call_id").String())
	}
	if fc.Get("name").String() != "get_weather" {
		t.Fatalf("expected name get_weather, got %q", fc.Get("name").String())
	}
	if fc.Get("arguments").String() != `{"city":"NYC"}` {
		t.Fatalf("expected arguments to match, got %q", fc.Get("arguments").String())
	}
}

func TestConvertMessagesToInput_FullMultiTurn(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"user","content":"What is the weather in NYC?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_001","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"NYC\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_001","content":"{\"temperature\":72}"},
			{"role":"user","content":"Thanks!"}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}

	input := gjson.GetBytes(got, "input")
	items := input.Array()
	if len(items) != 4 {
		t.Fatalf("expected 4 input items, got %d", len(items))
	}

	// 用户消息
	if items[0].Get("type").String() != "message" || items[0].Get("role").String() != "user" {
		t.Fatal("first item should be user message")
	}
	// function_call
	if items[1].Get("type").String() != "function_call" {
		t.Fatalf("second item should be function_call, got %q", items[1].Get("type").String())
	}
	// function_call_output
	if items[2].Get("type").String() != "function_call_output" {
		t.Fatalf("third item should be function_call_output, got %q", items[2].Get("type").String())
	}
	// 用户消息
	if items[3].Get("type").String() != "message" || items[3].Get("role").String() != "user" {
		t.Fatal("fourth item should be user message")
	}
}

func TestStreamTranslator_FunctionCall(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	// 1. output_item.added: function_call
	addedEvent := []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","id":"fc_001","call_id":"call_abc","name":"get_weather","arguments":"","status":"in_progress"}
	}`)
	chunk, done := st.Translate(addedEvent)
	if done {
		t.Fatal("should not be done after output_item.added")
	}
	if chunk == nil {
		t.Fatal("should emit chunk for function_call added")
	}
	// 验证首块包含 tool_calls
	tc := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0")
	if tc.Get("id").String() != "call_abc" {
		t.Fatalf("expected call_id call_abc, got %q", tc.Get("id").String())
	}
	if tc.Get("function.name").String() != "get_weather" {
		t.Fatalf("expected function name get_weather, got %q", tc.Get("function.name").String())
	}
	if tc.Get("index").Int() != 0 {
		t.Fatalf("expected index 0, got %d", tc.Get("index").Int())
	}

	// 2. function_call_arguments.delta
	deltaEvent := []byte(`{
		"type":"response.function_call_arguments.delta",
		"item_id":"fc_001",
		"output_index":0,
		"delta":"{\"city\":"
	}`)
	chunk, done = st.Translate(deltaEvent)
	if done {
		t.Fatal("should not be done after arguments delta")
	}
	if chunk == nil {
		t.Fatal("should emit chunk for arguments delta")
	}
	argsDelta := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments").String()
	if argsDelta != `{"city":` {
		t.Fatalf("expected arguments delta, got %q", argsDelta)
	}

	// 3. function_call_arguments.done
	doneEvent := []byte(`{
		"type":"response.function_call_arguments.done",
		"item_id":"fc_001",
		"output_index":0,
		"arguments":"{\"city\":\"NYC\"}"
	}`)
	chunk, done = st.Translate(doneEvent)
	if done || chunk != nil {
		t.Fatal("function_call_arguments.done should be ignored")
	}

	// 4. response.completed
	completedEvent := []byte(`{
		"type":"response.completed",
		"response":{
			"usage":{"input_tokens":10,"output_tokens":5},
			"output":[{"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"NYC\"}"}]
		}
	}`)
	chunk, done = st.Translate(completedEvent)
	if !done {
		t.Fatal("should be done after response.completed")
	}
	if chunk == nil {
		t.Fatal("should emit final chunk")
	}
	finishReason := gjson.GetBytes(chunk, "choices.0.finish_reason").String()
	if finishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q", finishReason)
	}

	if !st.HasToolCalls {
		t.Fatal("HasToolCalls should be true")
	}
}

func TestStreamTranslator_TextOnly(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	// 文本 delta
	textEvent := []byte(`{"type":"response.output_text.delta","delta":"Hello"}`)
	chunk, done := st.Translate(textEvent)
	if done {
		t.Fatal("should not be done")
	}
	if chunk == nil {
		t.Fatal("should emit chunk")
	}
	if gjson.GetBytes(chunk, "choices.0.delta.content").String() != "Hello" {
		t.Fatal("content mismatch")
	}

	// completed
	completedEvent := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":3}}}`)
	chunk, done = st.Translate(completedEvent)
	if !done {
		t.Fatal("should be done")
	}
	finishReason := gjson.GetBytes(chunk, "choices.0.finish_reason").String()
	if finishReason != "stop" {
		t.Fatalf("expected finish_reason stop for text-only, got %q", finishReason)
	}

	if st.HasToolCalls {
		t.Fatal("HasToolCalls should be false for text-only")
	}
}

func TestStreamTranslator_MultipleFunctionCalls(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	// 第一个 function call
	event1 := []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","id":"fc_001","call_id":"call_1","name":"func_a","arguments":""}
	}`)
	chunk, _ := st.Translate(event1)
	if gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.index").Int() != 0 {
		t.Fatal("first tool call should have index 0")
	}

	// 第二个 function call
	event2 := []byte(`{
		"type":"response.output_item.added",
		"output_index":1,
		"item":{"type":"function_call","id":"fc_002","call_id":"call_2","name":"func_b","arguments":""}
	}`)
	chunk, _ = st.Translate(event2)
	if gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.index").Int() != 1 {
		t.Fatal("second tool call should have index 1")
	}

	if st.nextIdx != 2 {
		t.Fatalf("expected nextIdx 2, got %d", st.nextIdx)
	}
}

func TestExtractToolCallsFromOutput(t *testing.T) {
	event := []byte(`{
		"type":"response.completed",
		"response":{
			"output":[
				{"type":"message","content":[{"type":"output_text","text":"hi"}]},
				{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"NYC\"}"},
				{"type":"function_call","call_id":"call_2","name":"get_time","arguments":"{}"}
			],
			"usage":{"input_tokens":10,"output_tokens":5}
		}
	}`)

	tcs := ExtractToolCallsFromOutput(event)
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tcs))
	}
	if tcs[0].ID != "call_1" || tcs[0].Name != "get_weather" {
		t.Fatalf("first tool call mismatch: %+v", tcs[0])
	}
	if tcs[1].ID != "call_2" || tcs[1].Name != "get_time" {
		t.Fatalf("second tool call mismatch: %+v", tcs[1])
	}
}
