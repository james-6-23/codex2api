package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestSendAnthropicStreamErrorEscapesJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	writer := newStreamFlushWriter(c.Writer, nil)
	if err := writeAnthropicStreamErrorEvent(writer, "api_error", "bad \"quote\"\\slash\nand control \x01"); err != nil {
		t.Fatalf("writeAnthropicStreamErrorEvent: %v", err)
	}

	body := recorder.Body.String()
	if !strings.HasPrefix(body, "event: error\ndata: ") || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("unexpected SSE frame: %q", body)
	}
	data := strings.TrimSuffix(strings.TrimPrefix(body, "event: error\ndata: "), "\n\n")

	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("stream error data is not valid JSON: %v; data=%q", err, data)
	}
	if payload.Type != "error" || payload.Error.Type != "api_error" {
		t.Fatalf("unexpected payload metadata: %+v", payload)
	}
	wantMessage := "bad \"quote\"\\slash\nand control \x01"
	if payload.Error.Message != wantMessage {
		t.Fatalf("message = %q, want %q", payload.Error.Message, wantMessage)
	}
}

func TestTranslateAnthropicRejectsOrphanToolResult(t *testing.T) {
	raw := []byte(`{"model":"grok-4.5","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_missing","content":"result"}]}]}`)
	_, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"grok-4.5"})
	if err == nil || !strings.Contains(err.Error(), "orphan tool_result") {
		t.Fatalf("orphan tool_result error = %v", err)
	}
}

func TestTranslateAnthropicToResponsesForGrokPreservesControls(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"max_tokens":321,
		"messages":[{"role":"user","content":"hello"}],
		"temperature":0.4,
		"top_p":0.75,
		"stop_sequences":["END","DONE"],
		"tools":[{"name":"lookup","input_schema":{"type":"object","uniqueItems":true}}],
		"tool_choice":{"type":"tool","name":"lookup"},
		"output_config":{
			"effort":"high",
			"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"string"}},"uniqueItems":true}}
		}
	}`)
	got, originalModel, err := TranslateAnthropicToResponsesForGrok(raw, "", []string{"grok-4.5"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToResponsesForGrok: %v", err)
	}
	if originalModel != "grok-4.5" {
		t.Fatalf("original model = %q", originalModel)
	}
	checks := map[string]string{
		"model":             "grok-4.5",
		"max_output_tokens": "321",
		"temperature":       "0.4",
		"top_p":             "0.75",
		"stop.0":            "END",
		"stop.1":            "DONE",
		"tool_choice.type":  "function",
		"tool_choice.name":  "lookup",
		"text.format.type":  "json_schema",
		"text.format.name":  "structured_output",
		"reasoning.effort":  "high",
		"reasoning.summary": "detailed",
		"include.0":         "reasoning.encrypted_content",
		"include.1":         "no_inline_citations",
	}
	for path, want := range checks {
		if value := gjson.GetBytes(got, path); value.String() != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, value.String(), want, got)
		}
	}
	if !gjson.GetBytes(got, "text.format.schema.uniqueItems").Bool() {
		t.Fatalf("Messages output_config schema was not preserved; body=%s", got)
	}
}

func TestTranslateAnthropicToResponsesForGrokAcceptsNestedJSONSchemaFormat(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"messages":[{"role":"user","content":"hello"}],
		"output_config":{"format":{"type":"json_schema","json_schema":{"name":"answer","strict":false,"schema":{"type":"object"}}}}
	}`)
	got, _, err := TranslateAnthropicToResponsesForGrok(raw, "", []string{"grok-4.5"})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "text.format.name").String() != "answer" || gjson.GetBytes(got, "text.format.strict").Bool() {
		t.Fatalf("nested JSON schema metadata not preserved; body=%s", got)
	}
}

func TestTranslateAnthropicToResponsesForGrokSplitsSystemCacheBreakpoints(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.6",
		"messages":[{"role":"user","content":"分析项目"}],
		"system":[
			{"type":"text","text":"today git dirty","cache_control":null},
			{"type":"text","text":"You are Claude Code","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"CLAUDE.md rules","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"date 2026-08-18"}
		]
	}`)
	got, _, err := TranslateAnthropicToResponsesForGrok(raw, "", []string{"grok-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	input := gjson.GetBytes(got, "input")
	if input.Get("#").Int() != 5 {
		t.Fatalf("input len = %d, want 5; body=%s", input.Get("#").Int(), got)
	}
	want := []string{"You are Claude Code", "CLAUDE.md rules", "today git dirty", "date 2026-08-18"}
	for i, text := range want {
		item := input.Get(strconv.Itoa(i))
		if item.Get("role").String() != "developer" || item.Get("content.0.text").String() != text {
			t.Fatalf("input.%d = %s, want developer %q", i, item.Raw, text)
		}
		if strings.Contains(item.Raw, "cache_control") {
			t.Fatalf("Grok input must not forward cache_control: %s", item.Raw)
		}
	}
	if input.Get("4.role").String() != "user" || input.Get("4.content.0.text").String() != "分析项目" {
		t.Fatalf("last item should be the user turn; body=%s", got)
	}
}

func TestTranslateAnthropicToCodexStillJoinsSystemBlocks(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hi"}],
		"system":[
			{"type":"text","text":"static","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"dynamic"}
		]
	}`)
	got, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "input.#").Int() != 2 {
		t.Fatalf("Codex should keep one joined system message; body=%s", got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "static\n\ndynamic" {
		t.Fatalf("joined system = %q; body=%s", text, got)
	}
}

func TestTranslateAnthropicToResponsesForGrokDowngradesFollowUpReasoning(t *testing.T) {
	prev := currentGrokFollowUpEffortConfig()
	t.Cleanup(func() { SetGrokFollowUpEffortConfig(prev) })
	SetGrokFollowUpEffortConfig(auth.GrokFollowUpEffortConfig{Enabled: true, ToolEffort: "medium", SmallEffort: "low"})

	first := []byte(`{
		"model":"grok-4.6",
		"output_config":{"effort":"high"},
		"tools":[{"name":"Read","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"分析项目"}]
	}`)
	got, _, err := TranslateAnthropicToResponsesForGrok(first, "", []string{"grok-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "reasoning.effort").String() != "high" || gjson.GetBytes(got, "reasoning.summary").String() != "detailed" {
		t.Fatalf("first turn should stay high+detailed; body=%s", got)
	}

	follow := []byte(`{
		"model":"grok-4.6",
		"output_config":{"effort":"high"},
		"tools":[{"name":"Read","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":"分析项目"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"README.md"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		]
	}`)
	got, _, err = TranslateAnthropicToResponsesForGrok(follow, "", []string{"grok-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "reasoning.effort").String() != "medium" || gjson.GetBytes(got, "reasoning.summary").String() != "auto" {
		t.Fatalf("tool_result follow-up should be medium+auto; body=%s", got)
	}

	small := []byte(`{"model":"grok-4.6","system":"short","messages":[{"role":"user","content":"取个标题"}]}`)
	got, _, err = TranslateAnthropicToResponsesForGrok(small, "", []string{"grok-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "reasoning.effort").String() != "low" || gjson.GetBytes(got, "reasoning.summary").String() != "auto" {
		t.Fatalf("small no-tools request should be low+auto; body=%s", got)
	}

	lowClient := []byte(`{"model":"grok-4.6","output_config":{"effort":"low"},"tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"分析项目"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
	got, _, err = TranslateAnthropicToResponsesForGrok(lowClient, "", []string{"grok-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "reasoning.effort").String() != "low" {
		t.Fatalf("explicit lower effort must not be raised; body=%s", got)
	}

	SetGrokFollowUpEffortConfig(auth.GrokFollowUpEffortConfig{Enabled: false, ToolEffort: "medium", SmallEffort: "low"})
	got, _, err = TranslateAnthropicToResponsesForGrok(follow, "", []string{"grok-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "reasoning.effort").String() != "high" || gjson.GetBytes(got, "reasoning.summary").String() != "detailed" {
		t.Fatalf("disabled follow-up policy should keep high+detailed; body=%s", got)
	}
}

func TestTranslateAnthropicToResponsesForGrokRejectsTopK(t *testing.T) {
	raw := []byte(`{"model":"grok-4.5","max_tokens":1,"top_k":40,"messages":[{"role":"user","content":"hello"}]}`)
	_, _, err := TranslateAnthropicToResponsesForGrok(raw, "", []string{"grok-4.5"})
	if err == nil || !strings.Contains(err.Error(), "top_k cannot be represented by Responses") {
		t.Fatalf("top_k error = %v", err)
	}
}

func TestTranslateAnthropicToCodexRemainsCodexSafeWhenControlsPresent(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.4","max_tokens":11,"temperature":0.4,"top_p":0.75,"stop_sequences":["END"],"messages":[{"role":"user","content":"hello"}]}`)
	got, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"max_output_tokens", "temperature", "top_p", "stop"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("Codex converter unexpectedly retained %s; body=%s", field, got)
		}
	}
}

func TestAnthropicStreamErrorCarriesPolicyDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writer := newStreamFlushWriter(c.Writer, nil)
	details := gin.H{"codex2api_policy": gin.H{"decision_id": "dec_stream", "strike_eligible": true}}

	if err := writeAnthropicStreamErrorEvent(writer, "invalid_request_error", "blocked", details); err != nil {
		t.Fatalf("writeAnthropicStreamErrorEvent: %v", err)
	}
	data := strings.TrimSuffix(strings.TrimPrefix(recorder.Body.String(), "event: error\ndata: "), "\n\n")
	if got := gjson.Get(data, "error.details.codex2api_policy.decision_id").String(); got != "dec_stream" {
		t.Fatalf("policy details missing from Anthropic stream error: %s", data)
	}
}

func TestAnthropicStreamContentBlockStartPreservesEmptyFields(t *testing.T) {
	tests := []struct {
		name  string
		block anthropicContentBlock
		field string
	}{
		{"thinking block", anthropicContentBlock{Type: "thinking"}, "thinking"},
		{"text block", anthropicContentBlock{Type: "text"}, "text"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := 0
			data, err := json.Marshal(anthropicStreamEvent{
				Type:         "content_block_start",
				Index:        &idx,
				ContentBlock: &tc.block,
			})
			if err != nil {
				t.Fatalf("marshal content_block_start: %v", err)
			}

			field := gjson.GetBytes(data, "content_block."+tc.field)
			if !field.Exists() || field.String() != "" {
				t.Fatalf("content_block.%s = %q, exists=%v; body=%s", tc.field, field.String(), field.Exists(), data)
			}
		})
	}
}

func TestAnthropicStreamToolUseStartOmitsTextAndThinkingFields(t *testing.T) {
	idx := 0
	data, err := json.Marshal(anthropicStreamEvent{
		Type:  "content_block_start",
		Index: &idx,
		ContentBlock: &anthropicContentBlock{
			Type:  "tool_use",
			ID:    "toolu_abc",
			Name:  "Read",
			Input: json.RawMessage("{}"),
		},
	})
	if err != nil {
		t.Fatalf("marshal content_block_start: %v", err)
	}
	if gjson.GetBytes(data, "content_block.text").Exists() || gjson.GetBytes(data, "content_block.thinking").Exists() {
		t.Fatalf("tool_use content_block_start should not include text/thinking fields; body=%s", data)
	}
}

func TestTranslateAnthropicToCodex_OutputConfigEffortTakesPrecedence(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"enabled","budget_tokens":512},
		"output_config":{"effort":"max"}
	}`)

	got, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4", "gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}

	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh; body=%s", effort, got)
	}
	if summary := gjson.GetBytes(got, "reasoning.summary").String(); summary != "auto" {
		t.Fatalf("reasoning.summary = %q, want auto; body=%s", summary, got)
	}
}

func TestTranslateAnthropicToCodex_OutputConfigMaxPreservedForGPT56(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"output_config":{"effort":"max"}
	}`)

	got, _, err := TranslateAnthropicToCodexWithModels(
		raw,
		`{"claude-sonnet-4-5":"gpt-5.6-sol"}`,
		[]string{"gpt-5.6-sol"},
	)
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}

	if model := gjson.GetBytes(got, "model").String(); model != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol; body=%s", model, got)
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", effort, got)
	}
}

func TestTranslateAnthropicToCodex_OutputConfigHighIsExplicit(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"output_config":{"effort":"high"}
	}`)

	got, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}

	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", effort, got)
	}
}

func TestTranslateAnthropicToCodex_DefaultsReasoningHighWithSummary(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}]
	}`)

	got, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}

	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", effort, got)
	}
	if summary := gjson.GetBytes(got, "reasoning.summary").String(); summary != "auto" {
		t.Fatalf("reasoning.summary = %q, want auto; body=%s", summary, got)
	}
	if tier := gjson.GetBytes(got, "service_tier"); tier.Exists() {
		t.Fatalf("service_tier should be omitted when speed is absent; body=%s", got)
	}
}

func TestTranslateAnthropicToCodex_ThinkingBudgetDoesNotControlEffort(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"enabled","budget_tokens":4096}
	}`)

	got, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}

	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", effort, got)
	}
}

func TestTranslateAnthropicToCodex_SpeedFastMapsToCodexPriority(t *testing.T) {
	cases := []struct {
		name     string
		field    string
		wantTier bool
	}{
		{"absent omits priority", "", false},
		{"speed fast maps to priority", `,"speed":"fast"`, true},
		{"speed standard omits priority", `,"speed":"standard"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{
				"model":"claude-sonnet-4-5",
				"messages":[{"role":"user","content":"hello"}]` + tc.field + `
			}`)

			got, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
			if err != nil {
				t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
			}

			tier := gjson.GetBytes(got, "service_tier")
			if tc.wantTier {
				if tier.String() != "priority" {
					t.Fatalf("service_tier = %q, want priority; body=%s", tier.String(), got)
				}
				if speed := gjson.GetBytes(got, "speed"); speed.Exists() {
					t.Fatalf("speed should not be forwarded to Codex body; body=%s", got)
				}
				return
			}
			if tier.Exists() {
				t.Fatalf("service_tier should be omitted; body=%s", got)
			}
			if speed := gjson.GetBytes(got, "speed"); speed.Exists() {
				t.Fatalf("speed should not be forwarded to Codex body; body=%s", got)
			}
		})
	}
}

func TestAnthropicUsageServiceTierResolution(t *testing.T) {
	cases := []struct {
		name   string
		speed  string
		actual string
		want   string
	}{
		{"no fast intent", "", "default", "default"},
		{"fast intent upstream default", "fast", "default", "default"},
		{"fast intent upstream priority", "fast", "priority", "fast"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field := ""
			if tc.speed != "" {
				field = `,"speed":"` + tc.speed + `"`
			}
			raw := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]` + field + `}`)
			codexBody, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.5"})
			if err != nil {
				t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
			}
			got := resolveServiceTier(tc.actual, extractServiceTier(codexBody))
			if got != tc.want {
				t.Fatalf("resolveServiceTier(%q, %q) = %q, want %q", tc.actual, extractServiceTier(codexBody), got, tc.want)
			}
		})
	}
}

func TestTranslateAnthropicToCodexCanonicalizesDynamicMappedModelAlias(t *testing.T) {
	raw := []byte(`{
		"model":"claude-haiku-4-5-20251001",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}]
	}`)

	body, originalModel, err := TranslateAnthropicToCodexWithModels(raw, `{"claude-haiku-4-5-20251001":"gpt5-4"}`, []string{"gpt-5.4", "gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}
	if originalModel != "claude-haiku-4-5-20251001" {
		t.Fatalf("originalModel = %q, want claude-haiku-4-5-20251001", originalModel)
	}

	var out struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal translated body: %v", err)
	}
	if out.Model != "gpt-5.4" {
		t.Fatalf("translated model = %q, want gpt-5.4", out.Model)
	}
}

func TestTranslateAnthropicToCodexDoesNotCanonicalizeDisabledModelAlias(t *testing.T) {
	raw := []byte(`{
		"model":"claude-haiku-4-5-20251001",
		"max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}]
	}`)

	body, _, err := TranslateAnthropicToCodexWithModels(raw, `{"claude-haiku-4-5-20251001":"gpt5-4"}`, []string{"gpt-5.4-mini"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}

	var out struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal translated body: %v", err)
	}
	if out.Model != "gpt5-4" {
		t.Fatalf("translated model = %q, want gpt5-4", out.Model)
	}
}

func TestSanitizeToolInputJSON(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		in       string
		want     string
	}{
		{
			name:     "read drops empty pages",
			toolName: "Read",
			in:       `{"file_path":"/etc/hosts","pages":""}`,
			want:     `{"file_path":"/etc/hosts"}`,
		},
		{
			name:     "read preserves null fields other than pages",
			toolName: "Read",
			in:       `{"file_path":"/etc/hosts","limit":null}`,
			want:     `{"file_path":"/etc/hosts","limit":null}`,
		},
		{
			name:     "read only drops empty pages",
			toolName: "Read",
			in:       `{"file_path":"/x","pages":"","limit":null,"offset":0}`,
			want:     `{"file_path":"/x","limit":null,"offset":0}`,
		},
		{
			name:     "write preserves empty content",
			toolName: "Write",
			in:       `{"file_path":"/tmp/empty.txt","content":""}`,
			want:     `{"file_path":"/tmp/empty.txt","content":""}`,
		},
		{
			name:     "edit preserves empty replacement",
			toolName: "Edit",
			in:       `{"file_path":"/tmp/a.txt","old_string":"abc","new_string":""}`,
			want:     `{"file_path":"/tmp/a.txt","old_string":"abc","new_string":""}`,
		},
		{
			name:     "custom tool preserves empty string",
			toolName: "Search",
			in:       `{"query":""}`,
			want:     `{"query":""}`,
		},
		{
			name:     "read preserves empty object",
			toolName: "Read",
			in:       `{"options":{}}`,
			want:     `{"options":{}}`,
		},
		{
			name:     "read preserves empty array",
			toolName: "Read",
			in:       `{"items":[]}`,
			want:     `{"items":[]}`,
		},
		{
			name:     "read preserves whitespace strings",
			toolName: "Read",
			in:       `{"sep":" "}`,
			want:     `{"sep":" "}`,
		},
		{
			name:     "read no-op when pages absent",
			toolName: "Read",
			in:       `{"file_path":"/etc/hosts"}`,
			want:     `{"file_path":"/etc/hosts"}`,
		},
		{
			name:     "enter worktree drops empty name when path is set",
			toolName: "EnterWorktree",
			in:       `{"name":"","path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"}`,
			want:     `{"path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"}`,
		},
		{
			name:     "enter worktree drops empty path when name is set",
			toolName: "EnterWorktree",
			in:       `{"name":"feature-x","path":""}`,
			want:     `{"name":"feature-x"}`,
		},
		{
			name:     "enter worktree preserves both non-empty mutually exclusive fields",
			toolName: "EnterWorktree",
			in:       `{"name":"feature-x","path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"}`,
			want:     `{"name":"feature-x","path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"}`,
		},
		{
			name:     "enter worktree preserves both empty fields",
			toolName: "EnterWorktree",
			in:       `{"name":"","path":""}`,
			want:     `{"name":"","path":""}`,
		},
		{
			name:     "invalid JSON returned as-is",
			toolName: "Read",
			in:       `{"file_path":`,
			want:     `{"file_path":`,
		},
		{
			name:     "empty input returned as-is",
			toolName: "Read",
			in:       ``,
			want:     ``,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeToolInputJSON(tc.toolName, tc.in)
			// Compare as JSON to ignore key ordering.
			if !jsonEqual(t, got, tc.want) {
				t.Fatalf("sanitizeToolInputJSON(%q, %q) = %q, want equivalent to %q",
					tc.toolName, tc.in, got, tc.want)
			}
		})
	}
}

func TestTranslateAnthropicToCodexBridgesToolResultImage(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{
			"role":"assistant",
			"content":[{"type":"tool_use","id":"toolu_img","name":"capture_screenshot","input":{}}]
		},{
			"role":"user",
			"content":[{
				"type":"tool_result",
				"tool_use_id":"toolu_img",
				"content":[
					{"type":"text","text":"screenshot captured"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAB"}}
				]
			}]
		}]
	}`)

	body, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}

	// function_call_output 保留文本、不含图片。
	out := gjson.GetBytes(body, `input.#(type=="function_call_output").output`).String()
	if out != "screenshot captured" {
		t.Fatalf("function_call_output.output = %q, want text only", out)
	}
	if strings.Contains(out, "data:image") {
		t.Fatalf("function_call_output leaked image data: %q", out)
	}

	// 紧随其后有一条 user 消息带 input_image。
	img := gjson.GetBytes(body, `input.#(type=="message")#|#(role=="user").content.#(type=="input_image").image_url`)
	if !strings.Contains(img.String(), "data:image/png;base64,AAAB") {
		t.Fatalf("synthesized user message missing input_image; body=%s", body)
	}
	attr := gjson.GetBytes(body, `input.#(type=="message")#|#(role=="user").content.#(type=="input_text").text`).String()
	if !strings.Contains(attr, "Tool output image for call") {
		t.Fatalf("attribution text = %q, want call attribution", attr)
	}
}

func TestTranslateAnthropicToCodexImageOnlyToolResultUsesMarker(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{
			"role":"assistant",
			"content":[{"type":"tool_use","id":"toolu_img","name":"capture_screenshot","input":{}}]
		},{
			"role":"user",
			"content":[{
				"type":"tool_result",
				"tool_use_id":"toolu_img",
				"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ZZZ"}}]
			}]
		}]
	}`)

	body, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}
	out := gjson.GetBytes(body, `input.#(type=="function_call_output").output`).String()
	if out != toolResultImageMovedMarker {
		t.Fatalf("image-only tool result output = %q, want marker", out)
	}
}

func TestTranslateAnthropicToCodexTextOnlyToolResultNoSyntheticMessage(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{
			"role":"assistant",
			"content":[{"type":"tool_use","id":"toolu_txt","name":"read_output","input":{}}]
		},{
			"role":"user",
			"content":[{
				"type":"tool_result",
				"tool_use_id":"toolu_txt",
				"content":[{"type":"text","text":"plain result"}]
			}]
		}]
	}`)

	body, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}
	// 无图片时不应产生任何 user 消息（仅 function_call_output）。
	if gjson.GetBytes(body, `input.#(type=="message")#|#(role=="user")`).Exists() {
		t.Fatalf("unexpected synthesized user message for text-only tool result; body=%s", body)
	}
}

func TestTranslateAnthropicToCodexPreservesToolInputByToolName(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{
			"role":"assistant",
			"content":[{
				"type":"tool_use",
				"id":"toolu_abc",
				"name":"Write",
				"input":{"file_path":"/tmp/empty.txt","content":""}
			}]
		}]
	}`)

	body, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}

	args := gjson.GetBytes(body, `input.#(type=="function_call").arguments`).String()
	want := `{"file_path":"/tmp/empty.txt","content":""}`
	if !jsonEqual(t, args, want) {
		t.Fatalf("function_call arguments = %q, want equivalent to %q; body=%s", args, want, body)
	}
}

func TestBuildAnthropicResponseFromCompletedPreservesToolInputByToolName(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		arguments string
		wantInput string
	}{
		{
			name:      "read drops empty pages",
			toolName:  "Read",
			arguments: `{"file_path":"/etc/hosts","pages":""}`,
			wantInput: `{"file_path":"/etc/hosts"}`,
		},
		{
			name:      "write preserves empty content",
			toolName:  "Write",
			arguments: `{"file_path":"/tmp/empty.txt","content":""}`,
			wantInput: `{"file_path":"/tmp/empty.txt","content":""}`,
		},
		{
			name:      "enter worktree drops empty name when path is set",
			toolName:  "EnterWorktree",
			arguments: `{"name":"","path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"}`,
			wantInput: `{"path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"}`,
		},
		{
			name:      "enter worktree preserves both non-empty mutually exclusive fields",
			toolName:  "EnterWorktree",
			arguments: `{"name":"feature-x","path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"}`,
			wantInput: `{"name":"feature-x","path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			completed := []byte(`{
				"type":"response.completed",
				"response":{
					"status":"completed",
					"output":[{
						"type":"function_call",
						"call_id":"call_abc",
						"name":` + mustJSONString(tc.toolName) + `,
						"arguments":` + mustJSONString(tc.arguments) + `
					}]
				}
			}`)

			resp := buildAnthropicResponseFromCompleted(completed, "claude-sonnet-4-5")
			if len(resp.Content) != 1 {
				t.Fatalf("len(content) = %d, want 1: %+v", len(resp.Content), resp.Content)
			}
			if got := resp.Content[0].Name; got != tc.toolName {
				t.Fatalf("tool name = %q, want %q", got, tc.toolName)
			}
			if !jsonEqual(t, string(resp.Content[0].Input), tc.wantInput) {
				t.Fatalf("tool input = %q, want equivalent to %q", string(resp.Content[0].Input), tc.wantInput)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	if a == b {
		return true
	}
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return a == b
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return a == b
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}

// TestAnthropicStreamTranslator_ToolInputStreamedIncrementally 对齐官方
// Anthropic：arguments 碎片立即变成 input_json_delta。空可选字段清洗只发生在
// 非流式聚合，不把整段 JSON 攒到关块再一次下发。
func TestAnthropicStreamTranslator_ToolInputStreamedIncrementally(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		deltas []string
	}{
		{
			name:   "read fragments stream as-is",
			tool:   "Read",
			deltas: []string{`{"file_path":"/etc/hosts"`, `,"pages":""`, `}`},
		},
		{
			name:   "write fragments stream as-is",
			tool:   "Write",
			deltas: []string{`{"file_path":"/tmp/empty.txt"`, `,"content":""`, `}`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newAnthropicStreamTranslator("claude-sonnet-4-5")
			tr.translateEvent([]byte(`{"type":"response.created"}`))
			tr.translateEvent([]byte(`{
				"type":"response.output_item.added",
				"output_index":0,
				"item":{"type":"function_call","call_id":"call_abc","name":` + mustJSONString(tc.tool) + `}
			}`))

			var got []string
			for _, d := range tc.deltas {
				evt := []byte(`{"type":"response.function_call_arguments.delta","delta":` +
					mustJSONString(d) + `}`)
				for _, streamed := range tr.translateEvent(evt) {
					if streamed.Type == "content_block_delta" && streamed.Delta != nil && streamed.Delta.Type == "input_json_delta" {
						got = append(got, streamed.Delta.PartialJSON)
					}
				}
			}
			if len(got) != len(tc.deltas) {
				t.Fatalf("streamed %d input_json_delta, want %d: %q", len(got), len(tc.deltas), got)
			}
			for i, d := range tc.deltas {
				if got[i] != d {
					t.Fatalf("delta[%d] = %q, want %q", i, got[i], d)
				}
			}

			closing := tr.translateEvent([]byte(`{"type":"response.output_item.done"}`))
			for _, evt := range closing {
				if evt.Type == "content_block_delta" {
					t.Fatalf("close must not emit another input_json_delta, got %+v", evt)
				}
			}
		})
	}
}

func TestAnthropicStreamTranslator_CustomToolCallInputDelta(t *testing.T) {
	tr := newAnthropicStreamTranslator("claude-sonnet-4-5")
	tr.translateEvent([]byte(`{"type":"response.created"}`))
	tr.translateEvent([]byte(`{
		"type":"response.output_item.added",
		"item":{"type":"custom_tool_call","id":"call_custom","name":"CustomTool"}
	}`))

	streamed := tr.translateEvent([]byte(`{
		"type":"response.custom_tool_call_input.delta",
		"delta":"{\"query\":\"hello\"}"
	}`))
	var sawDelta bool
	for _, evt := range streamed {
		if evt.Type == "content_block_delta" && evt.Delta != nil && evt.Delta.Type == "input_json_delta" {
			sawDelta = true
			if !jsonEqual(t, evt.Delta.PartialJSON, `{"query":"hello"}`) {
				t.Fatalf("custom tool input = %q", evt.Delta.PartialJSON)
			}
		}
	}
	if !sawDelta {
		t.Fatalf("expected custom tool input_json_delta while streaming")
	}
}

func TestAnthropicResponseAccumulatorUsesStreamDeltasWhenCompletedOutputIsEmpty(t *testing.T) {
	tr := newAnthropicStreamTranslator("claude-sonnet-4-5")
	acc := newAnthropicResponseAccumulator("claude-sonnet-4-5")

	events := [][]byte{
		[]byte(`{"type":"response.created"}`),
		[]byte(`{"type":"response.output_item.added","item":{"type":"reasoning"}}`),
		[]byte(`{"type":"response.output_item.done"}`),
		[]byte(`{"type":"response.output_item.added","item":{"type":"message"}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"O"}`),
		[]byte(`{"type":"response.output_text.delta","delta":"K"}`),
		[]byte(`{"type":"response.output_text.done"}`),
	}
	for _, event := range events {
		acc.apply(tr.translateEvent(event))
	}

	completed := []byte(`{
		"type":"response.completed",
		"response":{
			"status":"completed",
			"usage":{
				"input_tokens":10,
				"output_tokens":2,
				"input_tokens_details":{"cached_tokens":3}
			}
		}
	}`)
	acc.apply(tr.translateEvent(completed))

	resp := acc.build(completed)
	if len(resp.Content) != 1 {
		t.Fatalf("len(content) = %d, want 1: %+v", len(resp.Content), resp.Content)
	}
	if got := resp.Content[0].Text; got != "OK" {
		t.Fatalf("content text = %q, want OK", got)
	}
	if resp.Content[0].Type != "text" {
		t.Fatalf("content type = %q, want text", resp.Content[0].Type)
	}
	if anthropicStopReasonValue(resp.StopReason) != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", anthropicStopReasonValue(resp.StopReason))
	}
	// 上游 input_tokens=10 含 3 个缓存命中；Anthropic 语义下 input_tokens 不含
	// 缓存，应对外报 input=7（10-3）、cache_read=3，避免缓存 token 被重复计费。
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 2 || resp.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("usage = %+v, want input=7 output=2 cache_read=3", resp.Usage)
	}
}

// TestAnthropicStreamTranslatorUsageExcludesCachedTokens 验证流式 message_delta
// 的 usage 把缓存命中从 input_tokens 中扣除，避免缓存 token 被重复计费。
func TestAnthropicStreamTranslatorUsageExcludesCachedTokens(t *testing.T) {
	tr := newAnthropicStreamTranslator("claude-sonnet-4-5")
	tr.translateEvent([]byte(`{"type":"response.created"}`))

	completed := []byte(`{
		"type":"response.completed",
		"response":{
			"status":"completed",
			"usage":{
				"input_tokens":10,
				"output_tokens":2,
				"input_tokens_details":{"cached_tokens":3}
			}
		}
	}`)

	var usage *anthropicUsage
	for _, evt := range tr.translateEvent(completed) {
		if evt.Type == "message_delta" && evt.Usage != nil {
			usage = evt.Usage
		}
	}
	if usage == nil {
		t.Fatal("expected message_delta with usage")
	}
	// input_tokens=10 含 3 个缓存 → 对外 input=7、cache_read=3
	if usage.InputTokens != 7 || usage.OutputTokens != 2 || usage.CacheReadInputTokens != 3 {
		t.Fatalf("usage = %+v, want input=7 output=2 cache_read=3", *usage)
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestAnthropicThinkingSignatureRoundTrip_Output(t *testing.T) {
	tr := newAnthropicStreamTranslator("claude-sonnet-4-5")

	var all []anthropicStreamEvent
	for _, ev := range []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking hard"}`,
		`{"type":"response.reasoning_summary_text.done"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"ENCRYPTED_BLOB"}}`,
		`{"type":"response.output_text.delta","delta":"answer"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5}}}`,
	} {
		all = append(all, tr.translateEvent([]byte(ev))...)
	}

	var sawSignature, signatureBeforeStop bool
	for i, evt := range all {
		if evt.Type == "content_block_delta" && evt.Delta != nil && evt.Delta.Type == "signature_delta" {
			sawSignature = true
			if evt.Delta.Signature != "ENCRYPTED_BLOB" {
				t.Fatalf("signature = %q, want ENCRYPTED_BLOB", evt.Delta.Signature)
			}
			if i+1 < len(all) && all[i+1].Type != "content_block_stop" {
				t.Fatalf("signature_delta 后应紧跟 content_block_stop, got %s", all[i+1].Type)
			}
			signatureBeforeStop = i+1 < len(all) && all[i+1].Type == "content_block_stop"
		}
	}
	if !sawSignature {
		t.Fatal("stream should emit signature_delta carrying encrypted_content")
	}
	if !signatureBeforeStop {
		t.Fatal("signature_delta must precede the thinking content_block_stop")
	}
}

func TestAnthropicThinkingSignatureRoundTrip_NoSignatureNoDelta(t *testing.T) {
	tr := newAnthropicStreamTranslator("claude-sonnet-4-5")
	var all []anthropicStreamEvent
	for _, ev := range []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"t"}`,
		`{"type":"response.reasoning_summary_text.done"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning"}}`,
	} {
		all = append(all, tr.translateEvent([]byte(ev))...)
	}
	for _, evt := range all {
		if evt.Delta != nil && evt.Delta.Type == "signature_delta" {
			t.Fatal("no encrypted_content → no signature_delta")
		}
	}
	// thinking 块最终被 output_item.done 关闭
	last := all[len(all)-1]
	if last.Type != "content_block_stop" {
		t.Fatalf("thinking block should close at output_item.done, last=%s", last.Type)
	}
}

func TestAnthropicThinkingSignatureRoundTrip_Input(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[
			{"role":"user","content":"question"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"prior reasoning","signature":"ENCRYPTED_BLOB"},
				{"type":"text","text":"prior answer"},
				{"type":"thinking","thinking":"unsigned reasoning"}
			]},
			{"role":"user","content":"follow-up"}
		]
	}`)

	got, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	items := gjson.GetBytes(got, "input").Array()
	var reasoningCount int
	for _, item := range items {
		if item.Get("type").String() != "reasoning" {
			continue
		}
		reasoningCount++
		if enc := item.Get("encrypted_content").String(); enc != "ENCRYPTED_BLOB" {
			t.Fatalf("encrypted_content = %q, want ENCRYPTED_BLOB", enc)
		}
		if txt := item.Get("summary.0.text").String(); txt != "prior reasoning" {
			t.Fatalf("summary text = %q", txt)
		}
	}
	if reasoningCount != 1 {
		t.Fatalf("signed thinking → 1 reasoning item, unsigned skipped; got %d", reasoningCount)
	}
	// reasoning item 必须在 assistant message 之前（保持块序）
	var seenReasoning bool
	for _, item := range items {
		switch item.Get("type").String() {
		case "reasoning":
			seenReasoning = true
		case "message":
			if item.Get("role").String() == "assistant" && !seenReasoning {
				t.Fatal("reasoning item should precede the assistant message it belongs to")
			}
		}
	}
}

func TestAnthropicAccumulator_SignatureInNonStreamResponse(t *testing.T) {
	tr := newAnthropicStreamTranslator("claude-sonnet-4-5")
	acc := newAnthropicResponseAccumulator("claude-sonnet-4-5")
	for _, ev := range []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thought"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"SIG"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	} {
		acc.apply(tr.translateEvent([]byte(ev)))
	}
	resp := acc.build(nil)
	if len(resp.Content) < 2 {
		t.Fatalf("want thinking+text blocks, got %d", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" || resp.Content[0].Signature != "SIG" {
		t.Fatalf("thinking block should carry signature, got %+v", resp.Content[0])
	}
}

func TestAnthropicMessageStartMatchesOfficialShape(t *testing.T) {
	tr := newAnthropicStreamTranslator("grok-4.6")
	events := tr.translateEvent([]byte(`{"type":"response.created"}`))
	if len(events) != 1 || events[0].Type != "message_start" || events[0].Message == nil {
		t.Fatalf("events = %+v", events)
	}
	data, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	if raw := gjson.GetBytes(data, "message.content").Raw; raw != "[]" {
		t.Fatalf("message.content = %s, want []", raw)
	}
	if raw := gjson.GetBytes(data, "message.stop_reason").Raw; raw != "null" {
		t.Fatalf("message.stop_reason = %s, want null", raw)
	}
	if raw := gjson.GetBytes(data, "message.stop_details").Raw; raw != "null" {
		t.Fatalf("message.stop_details = %s, want null", raw)
	}
}

func TestAnthropicStreamEmitsPingAfterFirstBlock(t *testing.T) {
	tr := newAnthropicStreamTranslator("grok-4.6")
	tr.translateEvent([]byte(`{"type":"response.created"}`))
	events := tr.translateEvent([]byte(`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"Read"}}`))
	var sawStart, sawPing bool
	for _, evt := range events {
		switch evt.Type {
		case "content_block_start":
			sawStart = true
		case "ping":
			if !sawStart {
				t.Fatal("ping must follow content_block_start")
			}
			sawPing = true
		}
	}
	if !sawStart || !sawPing {
		t.Fatalf("start=%v ping=%v events=%+v", sawStart, sawPing, events)
	}

	delta := tr.translateEvent([]byte(`{"type":"response.function_call_arguments.delta","delta":"{}"}`))
	if len(delta) == 0 || delta[0].Type != "content_block_delta" {
		t.Fatalf("expected streamed input_json_delta, got %+v", delta)
	}
}

func TestAnthropicAccumulatorSanitizesStreamedToolInput(t *testing.T) {
	tr := newAnthropicStreamTranslator("claude-sonnet-4-5")
	acc := newAnthropicResponseAccumulator("claude-sonnet-4-5")
	for _, ev := range []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_abc","name":"Read"}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"file_path\":\"/etc/hosts\""}`,
		`{"type":"response.function_call_arguments.delta","delta":",\"pages\":\"\""}`,
		`{"type":"response.function_call_arguments.delta","delta":"}"}`,
		`{"type":"response.output_item.done"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	} {
		acc.apply(tr.translateEvent([]byte(ev)))
	}
	resp := acc.build(nil)
	if len(resp.Content) != 1 {
		t.Fatalf("len(content) = %d, want 1: %+v", len(resp.Content), resp.Content)
	}
	if !jsonEqual(t, string(resp.Content[0].Input), `{"file_path":"/etc/hosts"}`) {
		t.Fatalf("sanitized tool input = %q", string(resp.Content[0].Input))
	}
}

func TestResolveMessagesRoutingBodySkipsFullTranslation(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.6",
		"messages":[{"role":"user","content":"hello from a large prompt"}],
		"output_config":{"effort":"high"},
		"speed":"fast"
	}`)
	handler := &Handler{}
	got := handler.resolveMessagesRoutingBody(raw, "grok-4.6", []string{"grok-4.6"})
	if gjson.GetBytes(got, "input").Exists() || gjson.GetBytes(got, "messages").Exists() {
		t.Fatalf("routing stub must not translate the message list: %s", got)
	}
	if gjson.GetBytes(got, "model").String() != "grok-4.6" {
		t.Fatalf("model = %s", got)
	}
	if gjson.GetBytes(got, "reasoning.effort").String() != "high" {
		t.Fatalf("effort = %s", got)
	}
	if !gjson.GetBytes(got, "service_tier").Exists() {
		t.Fatalf("speed=fast should set service_tier: %s", got)
	}
}

func TestNativeClaudeRoutingRespectsAvailabilityAndAPIKeyChannel(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	account := &auth.Account{
		DBID:         91,
		UpstreamType: auth.UpstreamClaude,
		AccessToken:  "claude-token",
		Status:       auth.StatusReady,
		Models:       []string{"claude-sonnet-4-5"},
	}
	store.AddAccount(account)
	h := &Handler{store: store}

	if !h.hasNativeClaudeAccountForModel("claude-sonnet-4-5") {
		t.Fatal("available Claude account should enable native routing")
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelCodex}})
	c.Set(contextAPIKeyID, int64(7))
	if h.hasNativeClaudeAccountForRequest(c, "claude-sonnet-4-5") {
		t.Fatal("a Codex-only API key must not force native Claude routing")
	}

	c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 8, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelClaude}})
	c.Set(contextAPIKeyID, int64(8))
	if !h.hasNativeClaudeAccountForRequest(c, "claude-sonnet-4-5") {
		t.Fatal("a Claude API key should allow native Claude routing")
	}

	store.MarkCooldown(account, time.Minute, "rate_limited")
	if h.hasNativeClaudeAccountForModel("claude-sonnet-4-5") {
		t.Fatal("a cooled-down Claude account must not force native routing")
	}
}

func TestClaudeAccountMappingDoesNotValidateOpenAIProtocolModels(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.AddAccount(&auth.Account{
		DBID:         92,
		UpstreamType: auth.UpstreamClaude,
		AccessToken:  "claude-token",
		Status:       auth.StatusReady,
		Models:       []string{"claude-sonnet-4-5"},
		ModelMapping: `{"claude-alias":"claude-sonnet-4-5"}`,
	})
	h := &Handler{store: store}
	if h.modelSupportedByAccountMapping("claude-alias") {
		t.Fatal("Claude native aliases must not validate Responses/Chat/Compact models")
	}
}

func TestModelValidatorRejectsNativeClaudeIDsOnOpenAIProtocols(t *testing.T) {
	h := &Handler{}
	rule := h.modelValidator([]string{"gpt-5.4", "claude-sonnet-4-5"})
	if err := rule(gjson.Parse(`"claude-sonnet-4-5"`), "model"); err == nil {
		t.Fatal("native Claude IDs must not pass Responses/Chat model validation")
	}
	if err := rule(gjson.Parse(`"gpt-5.4"`), "model"); err != nil {
		t.Fatalf("Codex model unexpectedly rejected: %v", err)
	}
}

func TestSupportedModelIDsDoesNotExposeClaudeAccountAliases(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.AddAccount(&auth.Account{
		DBID:         93,
		UpstreamType: auth.UpstreamClaude,
		AccessToken:  "claude-token",
		Status:       auth.StatusReady,
		Models:       []string{"claude-sonnet-4-5"},
		ModelMapping: `{"client-alias":"claude-sonnet-4-5"}`,
	})
	h := &Handler{store: store}
	models := h.supportedModelIDs(nil)
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		seen[strings.ToLower(strings.TrimSpace(model))] = true
	}
	if !seen["claude-sonnet-4-5"] {
		t.Fatal("native Claude model should remain discoverable")
	}
	if seen["client-alias"] {
		t.Fatal("Claude account mapping aliases must not enter the shared OpenAI catalog")
	}
}

func TestNativeClaudeRoutingDoesNotReapplyCodexModelMapping(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.SetModelMapping(`{"claude-sonnet-4-5":"gpt-5.4"}`)
	store.AddAccount(&auth.Account{
		DBID:         94,
		UpstreamType: auth.UpstreamClaude,
		AccessToken:  "claude-token",
		Status:       auth.StatusReady,
		Models:       []string{"claude-sonnet-4-5"},
	})
	h := &Handler{store: store}
	body := h.resolveMessagesRoutingBodyForRequest(nil, []byte(`{"model":"claude-sonnet-4-5","messages":[]}`), "claude-sonnet-4-5", []string{"claude-sonnet-4-5", "gpt-5.4"})
	if got := gjson.GetBytes(body, "model").String(); got != "claude-sonnet-4-5" {
		t.Fatalf("native Claude routing model = %q, want native ID", got)
	}
}

func TestNativeClaudeRoutingResolvesClaudeAliasBeforePassthrough(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.SetModelMapping(`{"client-alias":"claude-sonnet-4-5"}`)
	store.AddAccount(&auth.Account{
		DBID: 95, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady,
		Models: []string{"claude-sonnet-4-5"},
	})
	h := &Handler{store: store}
	if !h.hasNativeClaudeAccountForRequest(nil, "client-alias") {
		t.Fatal("Claude alias should resolve to a native Claude account")
	}
	body := h.resolveMessagesRoutingBodyForRequest(nil, []byte(`{"model":"client-alias","messages":[]}`), "client-alias", []string{"client-alias", "claude-sonnet-4-5"})
	if got := gjson.GetBytes(body, "model").String(); got != "claude-sonnet-4-5" {
		t.Fatalf("Claude alias routing model = %q, want native target", got)
	}
}
