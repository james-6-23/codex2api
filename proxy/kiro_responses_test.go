package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
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
	translation := &kiroResponsesTranslation{model: "m", kinds: kiroToolKinds{
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
