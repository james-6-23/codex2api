package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

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

// TestAnthropicStreamTranslator_ToolInputBufferedAndCleaned 模拟 gpt-5.5 把
// "pages":"" 拆成多片 SSE 推送：translator 应缓冲到 tool_use 块关闭时再
// 整段清洗，并以单次 input_json_delta 发出，下游收到的 JSON 不含空 pages。
func TestAnthropicStreamTranslator_ToolInputBufferedAndCleaned(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		deltas    []string
		wantInput string
	}{
		{
			name:     "read drops empty pages",
			toolName: "Read",
			deltas: []string{
				`{"file_path":"/etc/hosts"`,
				`,"pages":""`,
				`}`,
			},
			wantInput: `{"file_path":"/etc/hosts"}`,
		},
		{
			name:     "write preserves empty content",
			toolName: "Write",
			deltas: []string{
				`{"file_path":"/tmp/empty.txt"`,
				`,"content":""`,
				`}`,
			},
			wantInput: `{"file_path":"/tmp/empty.txt","content":""}`,
		},
		{
			name:     "enter worktree drops empty name when path is set",
			toolName: "EnterWorktree",
			deltas: []string{
				`{"name":""`,
				`,"path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"`,
				`}`,
			},
			wantInput: `{"path":"F:\\Github\\codex2api\\.claude\\worktrees\\existing"}`,
		},
		{
			name:     "enter worktree drops empty path when name is set",
			toolName: "EnterWorktree",
			deltas: []string{
				`{"name":"feature-x"`,
				`,"path":""`,
				`}`,
			},
			wantInput: `{"name":"feature-x"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newAnthropicStreamTranslator("claude-sonnet-4-5")

			// response.created
			tr.translateEvent([]byte(`{"type":"response.created"}`))
			// output_item.added — 启动 tool_use 块
			tr.translateEvent([]byte(`{
				"type":"response.output_item.added",
				"output_index":0,
				"item":{"type":"function_call","call_id":"call_abc","name":` + mustJSONString(tc.toolName) + `}
			}`))

			var streamed []anthropicStreamEvent
			for _, d := range tc.deltas {
				evt := []byte(`{"type":"response.function_call_arguments.delta","delta":` +
					mustJSONString(d) + `}`)
				streamed = append(streamed, tr.translateEvent(evt)...)
			}

			// delta 阶段不应该泄漏任何 input_json_delta
			for _, evt := range streamed {
				if evt.Type == "content_block_delta" {
					t.Fatalf("expected no content_block_delta during streaming, got %+v", evt)
				}
			}

			// output_item.done 触发 closeCurrentBlock，整段清洗
			closing := tr.translateEvent([]byte(`{"type":"response.output_item.done"}`))

			var sawDelta bool
			var sawStop bool
			for _, evt := range closing {
				if evt.Type == "content_block_delta" {
					sawDelta = true
					if evt.Delta == nil || evt.Delta.Type != "input_json_delta" {
						t.Fatalf("expected input_json_delta, got %+v", evt.Delta)
					}
					if !jsonEqual(t, evt.Delta.PartialJSON, tc.wantInput) {
						t.Fatalf("cleaned tool input = %q, want equivalent to %q",
							evt.Delta.PartialJSON, tc.wantInput)
					}
				}
				if evt.Type == "content_block_stop" {
					sawStop = true
				}
			}
			if !sawDelta {
				t.Fatalf("expected one content_block_delta with cleaned input on close")
			}
			if !sawStop {
				t.Fatalf("expected content_block_stop on close")
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
	for _, evt := range streamed {
		if evt.Type == "content_block_delta" {
			t.Fatalf("expected no content_block_delta during streaming, got %+v", evt)
		}
	}

	closing := tr.translateEvent([]byte(`{"type":"response.output_item.done"}`))
	var sawDelta bool
	for _, evt := range closing {
		if evt.Type == "content_block_delta" {
			sawDelta = true
			if evt.Delta == nil || evt.Delta.Type != "input_json_delta" {
				t.Fatalf("expected input_json_delta, got %+v", evt.Delta)
			}
			if !jsonEqual(t, evt.Delta.PartialJSON, `{"query":"hello"}`) {
				t.Fatalf("custom tool input = %q", evt.Delta.PartialJSON)
			}
		}
	}
	if !sawDelta {
		t.Fatalf("expected custom tool input_json_delta on close")
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
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", resp.StopReason)
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
