package proxy

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

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
			name:     "drops Read empty pages field",
			toolName: "Read",
			in:       `{"file_path":"/etc/hosts","pages":""}`,
			want:     `{"file_path":"/etc/hosts"}`,
		},
		{
			name:     "preserves Read null field",
			toolName: "Read",
			in:       `{"file_path":"/etc/hosts","limit":null}`,
			want:     `{"file_path":"/etc/hosts","limit":null}`,
		},
		{
			name:     "only drops Read empty pages",
			toolName: "Read",
			in:       `{"file_path":"/x","pages":"","limit":null,"offset":0}`,
			want:     `{"file_path":"/x","limit":null,"offset":0}`,
		},
		{
			name:     "preserves Write empty content",
			toolName: "Write",
			in:       `{"file_path":"/tmp/a.txt","content":""}`,
			want:     `{"file_path":"/tmp/a.txt","content":""}`,
		},
		{
			name:     "preserves Edit empty new_string",
			toolName: "Edit",
			in:       `{"file_path":"/tmp/a.txt","old_string":"abc","new_string":""}`,
			want:     `{"file_path":"/tmp/a.txt","old_string":"abc","new_string":""}`,
		},
		{
			name:     "preserves non-Read empty string",
			toolName: "Search",
			in:       `{"query":""}`,
			want:     `{"query":""}`,
		},
		{
			name:     "preserves empty object",
			toolName: "Read",
			in:       `{"options":{}}`,
			want:     `{"options":{}}`,
		},
		{
			name:     "preserves empty array",
			toolName: "Read",
			in:       `{"items":[]}`,
			want:     `{"items":[]}`,
		},
		{
			name:     "preserves whitespace strings",
			toolName: "Read",
			in:       `{"sep":" "}`,
			want:     `{"sep":" "}`,
		},
		{
			name:     "no-op when nothing to drop",
			toolName: "Read",
			in:       `{"file_path":"/etc/hosts"}`,
			want:     `{"file_path":"/etc/hosts"}`,
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

func TestTranslateAnthropicToCodex_PreservesWriteEmptyContentInAssistantToolUse(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{
			"role":"assistant",
			"content":[{
				"type":"tool_use",
				"id":"toolu_write",
				"name":"Write",
				"input":{"file_path":"/tmp/a.txt","content":""}
			}]
		}]
	}`)

	body, _, err := TranslateAnthropicToCodexWithModels(raw, "", []string{"gpt-5.4"})
	if err != nil {
		t.Fatalf("TranslateAnthropicToCodexWithModels returned error: %v", err)
	}

	args := gjson.GetBytes(body, "input.0.arguments").String()
	want := `{"file_path":"/tmp/a.txt","content":""}`
	if !jsonEqual(t, args, want) {
		t.Fatalf("assistant Write arguments = %q, want equivalent to %q; body=%s", args, want, body)
	}
}

func TestBuildAnthropicResponseFromCompleted_ToolInputSanitizerIsNarrow(t *testing.T) {
	completed := []byte(`{
		"response":{
			"status":"completed",
			"output":[
				{
					"type":"function_call",
					"call_id":"call_read",
					"name":"Read",
					"arguments":` + mustJSONString(`{"file_path":"/tmp/demo.py","limit":2000,"offset":0,"pages":""}`) + `
				},
				{
					"type":"function_call",
					"call_id":"call_write",
					"name":"Write",
					"arguments":` + mustJSONString(`{"file_path":"/tmp/empty.txt","content":""}`) + `
				},
				{
					"type":"function_call",
					"call_id":"call_edit",
					"name":"Edit",
					"arguments":` + mustJSONString(`{"file_path":"/tmp/a.txt","old_string":"abc","new_string":""}`) + `
				}
			]
		}
	}`)

	resp := buildAnthropicResponseFromCompleted(completed, "claude-sonnet-4-5")
	if len(resp.Content) != 3 {
		t.Fatalf("len(content) = %d, want 3: %+v", len(resp.Content), resp.Content)
	}

	if !jsonEqual(t, string(resp.Content[0].Input), `{"file_path":"/tmp/demo.py","limit":2000,"offset":0}`) {
		t.Fatalf("Read input = %s", resp.Content[0].Input)
	}
	if !jsonEqual(t, string(resp.Content[1].Input), `{"file_path":"/tmp/empty.txt","content":""}`) {
		t.Fatalf("Write input = %s", resp.Content[1].Input)
	}
	if !jsonEqual(t, string(resp.Content[2].Input), `{"file_path":"/tmp/a.txt","old_string":"abc","new_string":""}`) {
		t.Fatalf("Edit input = %s", resp.Content[2].Input)
	}
}

func TestAnthropicStreamTranslator_PreservesWriteEmptyContent(t *testing.T) {
	tr := newAnthropicStreamTranslator("claude-sonnet-4-5")

	tr.translateEvent([]byte(`{"type":"response.created"}`))
	tr.translateEvent([]byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","call_id":"call_write","name":"Write"}
	}`))

	deltas := []string{
		`{"file_path":"/tmp/empty.txt"`,
		`,"content":""`,
		`}`,
	}
	var streamed []anthropicStreamEvent
	for _, d := range deltas {
		evt := []byte(`{"type":"response.function_call_arguments.delta","delta":` +
			mustJSONString(d) + `}`)
		streamed = append(streamed, tr.translateEvent(evt)...)
	}
	for _, evt := range streamed {
		if evt.Type == "content_block_delta" {
			t.Fatalf("expected no content_block_delta during streaming, got %+v", evt)
		}
	}

	closing := tr.translateEvent([]byte(`{"type":"response.output_item.done"}`))
	var sawDelta bool
	for _, evt := range closing {
		if evt.Type != "content_block_delta" {
			continue
		}
		sawDelta = true
		want := `{"file_path":"/tmp/empty.txt","content":""}`
		if evt.Delta == nil || evt.Delta.Type != "input_json_delta" || !jsonEqual(t, evt.Delta.PartialJSON, want) {
			t.Fatalf("Write stream input = %+v, want equivalent to %q", evt.Delta, want)
		}
	}
	if !sawDelta {
		t.Fatalf("expected one content_block_delta with Write input on close")
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
	tr := newAnthropicStreamTranslator("claude-sonnet-4-5")

	// response.created
	tr.translateEvent([]byte(`{"type":"response.created"}`))
	// output_item.added — 启动 tool_use 块
	tr.translateEvent([]byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","call_id":"call_abc","name":"Read"}
	}`))

	// 三片 function_call_arguments.delta，分别是开头/中段/结尾
	deltas := []string{
		`{"file_path":"/etc/hosts"`,
		`,"pages":""`,
		`}`,
	}
	var streamed []anthropicStreamEvent
	for _, d := range deltas {
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
			want := `{"file_path":"/etc/hosts"}`
			if !jsonEqual(t, evt.Delta.PartialJSON, want) {
				t.Fatalf("cleaned tool input = %q, want equivalent to %q",
					evt.Delta.PartialJSON, want)
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
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 2 || resp.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("usage = %+v, want input=10 output=2 cache_read=3", resp.Usage)
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
