package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestKiroMapModel(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-6":          "claude-sonnet-4.6",
		"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
		"claude-opus-4.6-thinking":   "claude-opus-4.6",
		"claude-3-5-sonnet-20241022": "claude-sonnet-3.5",
		"claude-haiku-5.5":           "claude-haiku-5.5",
		"gpt-5.6-sol-thinking":       "gpt-5.6-sol",
		"glm-5":                      "glm-5",
		"":                           "",
	}
	for in, want := range cases {
		if got := kiroMapModel(in); got != want {
			t.Errorf("kiroMapModel(%q)=%q want %q", in, got, want)
		}
	}
	if kiroContextWindow("claude-opus-4-6") != 1_000_000 || kiroContextWindow("grok-4.7") != 500_000 || kiroContextWindow("claude-sonnet-4.5") != 200_000 {
		t.Fatal("context windows drifted")
	}
}

func TestConvertKiroRequestHistoryToolsAndEffort(t *testing.T) {
	longName := strings.Repeat("mcp__server__", 6) + "tool"
	body := `{
	  "model":"claude-sonnet-4-6","max_tokens":1024,"stream":true,
	  "system":[{"type":"text","text":"be brief"}],
	  "thinking":{"type":"enabled","budget_tokens":30000},
	  "metadata":{"user_id":"user_x_account__session_6f9619ff-8b86-d011-b42d-00cf4fc964ff"},
	  "tools":[{"name":"` + longName + `","description":"","input_schema":{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"n":{"type":"integer","exclusiveMinimum":0}}}}],
	  "messages":[
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"sig"},{"type":"text","text":"calling"},{"type":"tool_use","id":"tu_1","name":"` + longName + `","input":{"n":1}},{"type":"tool_use","id":"tu_2","name":"old_tool","input":{}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"42"}]}]},
	    {"role":"user","content":"thanks"}
	  ]}`
	req, err := parseKiroInboundRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	conversion, err := convertKiroRequest(req, kiroPurposeGenerate)
	if err != nil {
		t.Fatal(err)
	}
	state := conversion.Request.ConversationState
	if state.ConversationID != "6f9619ff-8b86-d011-b42d-00cf4fc964ff" || conversion.ModelID != "claude-sonnet-4.6" {
		t.Fatalf("conversation id / model = %s / %s", state.ConversationID, conversion.ModelID)
	}
	if fields := conversion.Request.AdditionalModelRequestFields; fields == nil || fields.OutputConfig == nil || fields.OutputConfig.Effort != "high" {
		t.Fatalf("budget 24576 (capped) should map to high effort: %+v", fields)
	}
	if len(state.History) != 4 || !strings.Contains(state.History[0].User.Content, "<client_system_prompt>\nbe brief") {
		t.Fatalf("history shape: %+v", state.History)
	}
	assistant := state.History[3].Assistant
	if assistant == nil || assistant.ReasoningContent == nil || assistant.ReasoningContent.ReasoningText.Signature != "sig" || len(assistant.ToolUses) != 2 {
		t.Fatalf("assistant history: %+v", assistant)
	}
	short := assistant.ToolUses[0].Name
	if len(short) > kiroToolNameMaxLen || conversion.ToolNameMap[short] != longName {
		t.Fatalf("long tool name not shortened/restorable: %s", short)
	}
	current := state.CurrentMessage.UserInputMessage
	if !strings.Contains(current.Content, "thanks") {
		t.Fatalf("trailing user messages not merged: %q", current.Content)
	}
	results := current.UserInputMessageContext.ToolResults
	if len(results) != 2 || results[0].ToolUseID != "tu_1" || results[1].ToolUseID != "tu_2" || !results[1].IsError {
		t.Fatalf("tool results must pair tu_1 and synthesize interrupted tu_2: %+v", results)
	}
	names := map[string]bool{}
	for _, tool := range current.UserInputMessageContext.Tools {
		names[tool.ToolSpecification.Name] = true
		if tool.ToolSpecification.Name == short {
			encoded, _ := json.Marshal(tool.ToolSpecification.InputSchema.JSON)
			if strings.Contains(string(encoded), "$schema") || strings.Contains(string(encoded), "exclusiveMinimum") || tool.ToolSpecification.Description != longName {
				t.Fatalf("schema/description not normalized: %s %q", encoded, tool.ToolSpecification.Description)
			}
		}
	}
	if !names["old_tool"] {
		t.Fatal("history-only tool needs a placeholder definition")
	}
}

func TestConvertKiroRequestDropsPrefillAndOldThinking(t *testing.T) {
	body := `{"model":"claude-sonnet-4-5","messages":[
	  {"role":"user","content":"q1"},
	  {"role":"assistant","content":[{"type":"thinking","thinking":"t1","signature":"s1"},{"type":"text","text":"a1"}]},
	  {"role":"user","content":"q2"},
	  {"role":"assistant","content":"prefill"}]}`
	req, _ := parseKiroInboundRequest([]byte(body))
	conversion, err := convertKiroRequest(req, kiroPurposeGenerate)
	if err != nil {
		t.Fatal(err)
	}
	state := conversion.Request.ConversationState
	if state.CurrentMessage.UserInputMessage.Content != "q2" || len(state.History) != 2 {
		t.Fatalf("prefill not dropped: %+v", state)
	}
	if state.History[1].Assistant.ReasoningContent != nil {
		t.Fatal("sonnet 4.5 must not replay finished-turn thinking")
	}
}

func TestKiroStructuredOutput(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}, "tag": map[string]any{"enum": []any{"a", "b"}}}, "required": []any{"n"}, "additionalProperties": false}
	cases := map[string]string{
		"```json\n{\"n\": 3}\n```":                                 `{"n":3}`,
		"Sure! {\"n\": 1.5} no, final: {\"n\": 2, \"tag\": \"b\"}": `{"n":2,"tag":"b"}`,
	}
	for text, want := range cases {
		if got, ok := kiroExtractConformingJSON(text, schema); !ok || got != want {
			t.Errorf("extract(%q) = %q,%t want %q", text, got, ok, want)
		}
	}
	if _, ok := kiroExtractConformingJSON(`{"n":1,"extra":true}`, schema); ok {
		t.Fatal("additionalProperties:false must reject extra keys")
	}
	req, _ := parseKiroInboundRequest([]byte(`{"model":"claude-sonnet-4-6","system":"base","output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}},"messages":[{"role":"user","content":"x"}]}`))
	if kiroInjectStructuredInstruction(req) == nil || len(req.systemTexts()) != 2 || !strings.Contains(req.systemTexts()[1], kiroStructuredMarker) {
		t.Fatalf("instruction not appended: %v", req.systemTexts())
	}
	events := kiroEventsFromMessage("m", []any{map[string]any{"type": "thinking", "thinking": "t", "signature": "s"}, map[string]any{"type": "text", "text": "Here: {\"n\": 7}"}}, "end_turn", map[string]any{})
	message, _ := kiroAggregateEvents(kiroRewriteStructuredEvents(events, schema))
	content := message["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["text"] != `{"n":7}` {
		t.Fatalf("structured rewrite: %+v", content)
	}
}

func TestKiroCLIBodyOnlyRewritesMessageOrigin(t *testing.T) {
	body := []byte(`{"conversationState":{"agentContinuationId":"x","currentMessage":{"userInputMessage":{"origin":"AI_EDITOR","content":"hi"}},
	  "history":[{"userInputMessage":{"origin":"AI_EDITOR","modelId":"m"}},
	             {"assistantResponseMessage":{"toolUses":[{"input":{"origin":"AI_EDITOR","id":9007199254740993}}]}}]}}`)
	out := string(kiroCLIBody(body))
	for _, want := range []string{`"currentMessage":{"userInputMessage":{"content":"hi","origin":"KIRO_CLI"}}`, `{"userInputMessage":{"origin":"KIRO_CLI"}}`, `"input":{"id":9007199254740993,"origin":"AI_EDITOR"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, "agentContinuationId") {
		t.Fatalf("agentContinuationId must be dropped: %s", out)
	}
}

func TestKiroImageDecodeBudgetRejectsHugeImages(t *testing.T) {
	if !kiroImageDecodeWithinBudget(1568, 1568) || !kiroImageDecodeWithinBudget(3840, 2160) {
		t.Fatal("ordinary images must be decodable")
	}
	if kiroImageDecodeWithinBudget(30000, 30000) || kiroImageDecodeWithinBudget(1<<31-1, 2) || kiroImageDecodeWithinBudget(0, 10) {
		t.Fatal("images above the 512 MiB decode budget must not be decoded")
	}
	if kiroImageDecodeWithinBudget(10000, 10000) || kiroImageDecodeWithinBudget(20000, 20000) || kiroImageDecodeWithinBudget(1, 1<<30) {
		t.Fatal("the decode budget must include intermediate buffers, regardless of color model")
	}
	var huge bytes.Buffer
	if err := png.Encode(&huge, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	// 改写 IHDR 宽高为 30000x30000：DecodeConfig 只读头部，解码会因预算被跳过而原样透传。
	raw := huge.Bytes()
	binary.BigEndian.PutUint32(raw[16:20], 30000)
	binary.BigEndian.PutUint32(raw[20:24], 30000)
	binary.BigEndian.PutUint32(raw[29:33], crc32.ChecksumIEEE(raw[12:29]))
	if config, _, err := image.DecodeConfig(bytes.NewReader(raw)); err != nil || config.Width != 30000 {
		t.Fatalf("fixture header must stay readable: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if format, data := kiroShrinkImage("png", encoded); data != encoded || format != "png" {
		t.Fatalf("oversized image must pass through untouched, got %s", format)
	}
}

func TestKiroStructuredOutputPrefersFullyConstrainedCandidate(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"age":  map[string]any{"type": "integer", "minimum": 0, "maximum": 150},
		"tags": map[string]any{"type": "array", "uniqueItems": true, "maxItems": 2},
	}}
	cases := map[string]string{
		`first {"age": 30} then {"age": 200}`:                        `{"age":30}`,
		`only {"age": 200}`:                                          `{"age":200}`,
		`{"age": 1, "tags": ["a","a"]} or {"age": 2, "tags": ["a"]}`: `{"age":2,"tags":["a"]}`,
	}
	for text, want := range cases {
		if got, ok := kiroExtractConformingJSON(text, schema); !ok || got != want {
			t.Errorf("extract(%q) = %q,%t want %q", text, got, ok, want)
		}
	}
	for raw, want := range map[string]bool{
		`[{"multipleOf":0.1},0.3]`:                    true,
		`[{"multipleOf":0.1},0.35]`:                   false,
		`[{"exclusiveMinimum":0},0]`:                  false,
		`[{"minimum":0,"exclusiveMinimum":true},0]`:   false,
		`[{"minLength":2,"maxLength":3},"你好"]`:        true,
		`[{"maxLength":1},"你好"]`:                      false,
		`[{"minProperties":1},{}]`:                    false,
		`[{"pattern":"^x$","maxItems":5},"anything"]`: true,
	} {
		var pair []any
		if err := kiroDecodeJSON([]byte(raw), &pair); err != nil {
			t.Fatal(err)
		}
		if got := kiroSchemaConstraints(pair[0].(map[string]any), pair[1]); got != want {
			t.Errorf("constraints %s = %v want %v", raw, got, want)
		}
	}
}

func TestKiroDecodeJSONRejectsTrailingData(t *testing.T) {
	var value any
	for _, raw := range []string{`{"id":1}}`, `[1]]`, `{"a":1} {"b":2}`, `{"a":1} x`} {
		if err := kiroDecodeJSON([]byte(raw), &value); err == nil {
			t.Errorf("%s must be rejected", raw)
		}
	}
	if err := kiroDecodeJSON([]byte("{\"id\":9007199254740993}\n  "), &value); err != nil {
		t.Fatalf("trailing whitespace is valid JSON: %v", err)
	}
}

func TestKiroStructuredOutputExactMultipleOfAndNestedEquality(t *testing.T) {
	multiple := map[string]any{"type": "array", "items": map[string]any{"type": "number", "multipleOf": 1}}
	if got, ok := kiroExtractConformingJSON("first [1] then [0.0000000001]", multiple); !ok || got != "[1]" {
		t.Fatalf("multipleOf must require an exact integer quotient, got %q", got)
	}
	unique := map[string]any{"type": "array", "uniqueItems": true}
	if got, ok := kiroExtractConformingJSON(`first [{"n":1},{"n":2}] then [{"n":1},{"n":1.0}]`, unique); !ok || got != `[{"n":1},{"n":2}]` {
		t.Fatalf("uniqueItems must compare nested numbers semantically, got %q", got)
	}
	if !kiroJSONEqual(map[string]any{"a": []any{json.Number("1.0")}}, map[string]any{"a": []any{json.Number("1")}}) || kiroJSONEqual(json.Number("1"), "1") {
		t.Fatal("JSON equality must normalize nested numbers and keep numbers distinct from strings")
	}
	ranged := map[string]any{"oneOf": []any{
		map[string]any{"type": "integer", "minimum": 0, "maximum": 9},
		map[string]any{"type": "integer", "minimum": 10, "maximum": 19},
	}}
	if got, ok := kiroExtractConformingJSON("answer: 2", ranged); ok {
		t.Fatalf("a bare number outside JSON containers is not extracted: %q", got)
	}
	if got, ok := kiroExtractConformingJSON("2", ranged); !ok || got != "2" {
		t.Fatalf("oneOf branches separated by ranges must accept 2, got %q %v", got, ok)
	}
	if !kiroInvokeInputConforms(map[string]any{"type": "object", "properties": map[string]any{"ids": map[string]any{"type": "array", "items": ranged}}}, map[string]any{"ids": []any{2, 15}}) {
		t.Fatal("the invoke gate must evaluate range keywords inside oneOf")
	}
}
