package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestKiroCatalogModelsPassResponsesValidation(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{DBID: 9301, UpstreamType: auth.UpstreamKiro, AccessToken: "at", Models: []string{"claude-haiku-5.5"}})
	handler := &Handler{store: store}
	if !handler.modelSupportedByAccountMapping("claude-haiku-5.5") {
		t.Fatal("a model in the Kiro account catalog must pass /v1/responses and /v1/chat/completions validation")
	}
	if handler.modelSupportedByAccountMapping("claude-opus-5.5") {
		t.Fatal("models outside the Kiro account catalog must stay rejected")
	}
}

func TestKiroStaysOffWebSocketAndDedicatedCompact(t *testing.T) {
	account := &auth.Account{DBID: 9300, UpstreamType: auth.UpstreamKiro, AccessToken: "at", Status: auth.StatusReady}
	if !accountFilterForInlineCompactionModelWithOriginal("claude-sonnet-4.6", "claude-sonnet-4.6", false)(account) {
		t.Fatal("inline compaction is handled by the Kiro bridge and must keep Kiro accounts")
	}
	if accountFilterForCompactResponsesModelWithOriginal("claude-sonnet-4.6", "claude-sonnet-4.6", false)(account) {
		t.Fatal("the dedicated compact executor has no Kiro adapter")
	}
	if accountFilterForResponsesWebSocket("claude-sonnet-4.6")(account) {
		t.Fatal("Kiro is HTTP only and must stay out of the Responses WebSocket pool")
	}
}

func TestKiroResponsesToMessagesToolsAndHistory(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","instructions":"sys","stream":true,"reasoning":{"effort":"high"},"prompt_cache_key":"session_6f9619ff-8b86-d011-b42d-00cf4fc964ff",
	  "tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
	           {"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}},
	           {"type":"namespace","name":"mcp","description":"tools","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}],
	  "input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},
	           {"type":"custom_tool_call","call_id":"c1","name":"apply_patch","input":"*** Begin Patch"},
	           {"type":"custom_tool_call_output","call_id":"c1","output":"ok"},
	           {"type":"function_call","call_id":"c2","namespace":"mcp","name":"read","arguments":"{\"p\":1}"},
	           {"type":"function_call_output","call_id":"c2","output":[{"type":"input_text","text":"done"}]}]}`
	translation, err := kiroResponsesToMessages([]byte(body), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if !translation.kinds["apply_patch"].custom || translation.kinds["mcp__read"].namespace != "mcp" || translation.kinds["shell"].custom {
		t.Fatalf("tool kinds: %+v", translation.kinds)
	}
	encoded := string(translation.messages)
	for _, fragment := range []string{`"name":"mcp__read"`, `"input":{"input":"*** Begin Patch"}`, `"content":"done"`, `"effort":"high"`, `"user_id":"session_6f9619ff-8b86-d011-b42d-00cf4fc964ff"`, `Input format (lark grammar):\nstart: x`, `[mcp] tools`} {
		if !strings.Contains(encoded, fragment) {
			t.Errorf("translated request missing %s:\n%s", fragment, encoded)
		}
	}
	inbound, err := parseKiroInboundRequest(translation.messages)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertKiroRequest(inbound, kiroPurposeGenerate); err != nil {
		t.Fatalf("translated request must convert cleanly: %v", err)
	}
}

func TestKiroResponsesStreamRestoresCustomAndNamespaceTools(t *testing.T) {
	translation := &kiroResponsesTranslation{model: "m", config: kiroResponsesConfig{parallelToolCalls: true}, kinds: kiroToolKinds{
		"apply_patch": {custom: true, name: "apply_patch"},
		"mcp__read":   {name: "read", namespace: "mcp"},
	}}
	events := kiroEventsFromMessage("m", []any{
		map[string]any{"type": "text", "text": "hi"},
		map[string]any{"type": "tool_use", "id": "c1", "name": "apply_patch", "input": map[string]any{"input": "*** Begin Patch\n"}},
		map[string]any{"type": "tool_use", "id": "c2", "name": "mcp__read", "input": map[string]any{"p": 1}},
	}, "tool_use", map[string]any{"input_tokens": 5, "output_tokens": 3, "cache_read_input_tokens": 2})
	responses := newKiroResponsesStream(translation)
	var sse strings.Builder
	for _, event := range events {
		for _, chunk := range responses.handle(event) {
			sse.Write(chunk)
		}
	}
	stream := sse.String()
	for _, fragment := range []string{`"type":"custom_tool_call"`, `"input":"*** Begin Patch\n"`, `response.custom_tool_call_input.done`, `"namespace":"mcp"`, `"name":"read"`, `response.completed`, `"cached_tokens":2`, `"input_tokens":7`} {
		if !strings.Contains(stream, fragment) {
			t.Errorf("responses stream missing %s", fragment)
		}
	}
}

func TestKiroCompactionHelpers(t *testing.T) {
	if !kiroResponsesHasCompactionTrigger([]byte(`{"input":[{"role":"user","content":"x"},{"type":"compaction_trigger"}]}`)) {
		t.Fatal("trigger not detected")
	}
	big := strings.Repeat("a", 30_000)
	request := []byte(`{"input":[{"type":"function_call_output","call_id":"c","output":"` + big + `"}]}`)
	bounded, truncated := kiroBoundToolOutputsForRetry(request)
	if truncated != 1 || len(bounded) >= len(request) || !strings.Contains(string(bounded), "tool output truncated for compaction retry") {
		t.Fatalf("tool output not bounded: %d", truncated)
	}
	messages, err := kiroPrepareCompactionMessages([]byte(`{"model":"m","input":[{"role":"user","content":"hi"},{"type":"function_call","call_id":"c9","name":"shell","arguments":"{}"}]}`), http.Header{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(messages), kiroCancelledToolResult) || !strings.Contains(string(messages), `{\"c9\":\"shell\"}`) || strings.Contains(string(messages), `"tools"`) {
		t.Fatalf("fallback compaction request: %s", messages)
	}
	ok := kiroRenderCompaction(&kiroCompactionAttempt{text: " summary ", stopReason: "end_turn"}, map[string]any{}, "m", false)
	var response map[string]any
	_ = json.NewDecoder(ok.Body).Decode(&response)
	item := response["output"].([]any)[0].(map[string]any)
	if summary, decoded := kiroDecodeCompactionPayload(item["encrypted_content"].(string)); !decoded || summary != "summary" {
		t.Fatalf("compaction payload = %v", item)
	}
}

func TestKiroResponsesKeepsLargeIntegersInToolHistory(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","input":[{"role":"user","content":"hi"},
	  {"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"id\":9007199254740993}"},
	  {"type":"function_call_output","call_id":"c1","output":"ok"}],
	  "tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer"}}}}]}`
	translation, err := kiroResponsesToMessages([]byte(body), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := parseKiroInboundRequest(translation.messages)
	if err != nil {
		t.Fatal(err)
	}
	conversion, err := convertKiroRequest(inbound, kiroPurposeGenerate)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(conversion.Request)
	if !strings.Contains(string(encoded), "9007199254740993") {
		t.Fatalf("tool history lost integer precision: %s", encoded)
	}
}

func TestKiroResponsesCustomToolChoice(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","input":"patch it","tool_choice":{"type":"custom","name":"apply_patch"},
	  "tools":[{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}}]}`
	translation, err := kiroResponsesToMessages([]byte(body), http.Header{})
	if err != nil {
		t.Fatalf("custom tool_choice must be accepted: %v", err)
	}
	if !strings.Contains(string(translation.messages), `"tool_choice":{"name":"apply_patch","type":"tool"}`) || !translation.kinds["apply_patch"].custom {
		t.Fatalf("custom tool_choice not translated: %s", translation.messages)
	}
}

func TestKiroResponsesReasoningSummaryPartEvents(t *testing.T) {
	translation := &kiroResponsesTranslation{model: "m", kinds: kiroToolKinds{}}
	events := kiroEventsFromMessage("m", []any{
		map[string]any{"type": "thinking", "thinking": "plan", "signature": "sig"},
		map[string]any{"type": "text", "text": "done"},
	}, "end_turn", map[string]any{"input_tokens": 1, "output_tokens": 1})
	responses := newKiroResponsesStream(translation)
	var sse strings.Builder
	for _, event := range events {
		for _, chunk := range responses.handle(event) {
			sse.Write(chunk)
		}
	}
	stream := sse.String()
	added := strings.Index(stream, "response.reasoning_summary_part.added")
	delta := strings.Index(stream, "response.reasoning_summary_text.delta")
	partDone := strings.Index(stream, "response.reasoning_summary_part.done")
	itemDone := strings.Index(stream, `"type":"reasoning"`)
	if added < 0 || delta < 0 || partDone < 0 || added > delta || partDone < delta || itemDone < 0 {
		t.Fatalf("reasoning summary part events out of order:\n%s", stream)
	}
}

func TestKiroResponsesTextFormatUsesStructuredOutput(t *testing.T) {
	for _, format := range []string{
		`{"type":"json_schema","name":"n","schema":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}`,
		`{"type":"json_object"}`,
	} {
		body := `{"model":"claude-sonnet-4-6","input":"give n","text":{"format":` + format + `}}`
		translation, err := kiroResponsesToMessages([]byte(body), http.Header{})
		if err != nil {
			t.Fatal(err)
		}
		inbound, err := parseKiroInboundRequest(translation.messages)
		if err != nil {
			t.Fatal(err)
		}
		schema := kiroInjectStructuredInstruction(inbound)
		if schema == nil {
			t.Fatalf("text.format %s must enable structured output: %s", format, translation.messages)
		}
		upstream := &http.Response{Body: io.NopCloser(kiroTestFrames(t, [2]string{"assistantResponseEvent", `{"content":"Sure! {\"n\": 2} hope that helps"}`}))}
		response := kiroServeStructuredResponses(upstream, newKiroStreamContext("m", 1, false, nil), translation, schema)
		var decoded map[string]any
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		output, _ := decoded["output"].([]any)
		if len(output) != 1 {
			t.Fatalf("output = %v", decoded)
		}
		text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if text != `{"n":2}` {
			t.Fatalf("structured text = %q", text)
		}
	}
}

func TestKiroResponsesParallelToolCallsFalseKeepsFirstCall(t *testing.T) {
	tools := `"tools":[{"type":"function","name":"a","parameters":{"type":"object"}},{"type":"function","name":"b","parameters":{"type":"object"}}]`
	for _, tc := range []struct {
		parallel string
		want     int
	}{{"", 2}, {`"parallel_tool_calls":true,`, 2}, {`"parallel_tool_calls":false,`, 1}} {
		translation, err := kiroResponsesToMessages([]byte(`{"model":"m",`+tc.parallel+`"input":"go",`+tools+`}`), http.Header{})
		if err != nil {
			t.Fatal(err)
		}
		if hinted := strings.Contains(string(translation.messages), kiroSingleToolCallInstruction); hinted != (tc.want == 1) {
			t.Fatalf("%q: single-call instruction present=%v", tc.parallel, hinted)
		}
		events := kiroEventsFromMessage("m", []any{
			map[string]any{"type": "tool_use", "id": "call_a", "name": "a", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "call_b", "name": "b", "input": map[string]any{}},
		}, "tool_use", map[string]any{"input_tokens": 1, "output_tokens": 1})
		responses := newKiroResponsesStream(translation)
		for _, event := range events {
			responses.handle(event)
		}
		object := responses.responseObject("completed")
		output, _ := object["output"].([]any)
		if len(output) != tc.want || object["parallel_tool_calls"] != (tc.want == 2) {
			t.Fatalf("%q: output=%v parallel=%v", tc.parallel, output, object["parallel_tool_calls"])
		}
	}
}

func TestKiroChatControlsSurviveCodexTranslation(t *testing.T) {
	chat := []byte(`{"model":"m","parallel_tool_calls":false,"max_tokens":50,"max_completion_tokens":77,"tool_choice":{"type":"function","function":{"name":"f"}},"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`)
	codexBody, err := TranslateRequest(chat)
	if err != nil {
		t.Fatal(err)
	}
	translation, err := kiroResponsesToMessages(kiroRestoreChatControls(codexBody, chat), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	messages := gjson.ParseBytes(translation.messages)
	if translation.config.parallelToolCalls || messages.Get("tool_choice.name").String() != "f" || messages.Get("max_tokens").Int() != 77 {
		t.Fatalf("chat controls lost: parallel=%v %s", translation.config.parallelToolCalls, translation.messages)
	}
}

func TestKiroResponsesRemoteImageBecomesPlaceholder(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","input":[{"role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]}`
	translation, err := kiroResponsesToMessages([]byte(body), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	inbound, err := parseKiroInboundRequest(translation.messages)
	if err != nil {
		t.Fatal(err)
	}
	conversion, err := convertKiroRequest(inbound, kiroPurposeGenerate)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(conversion.Request)
	if !strings.Contains(string(encoded), "url image source is not supported by this gateway") {
		t.Fatalf("remote image must leave a placeholder for the model: %s", encoded)
	}
}
