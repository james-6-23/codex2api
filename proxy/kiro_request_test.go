package proxy

import (
	"encoding/json"
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
