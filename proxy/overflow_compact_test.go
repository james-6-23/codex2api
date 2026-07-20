package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsContextLengthExceededBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"upstream http error", `{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_length_exceeded","param":"input"}}`, true},
		{"code only", `{"error":{"code":"context_length_exceeded"}}`, true},
		{"message only", `{"error":{"message":"Your input exceeds the context window of this model."}}`, true},
		{"other 400", `{"error":{"code":"invalid_request_error","message":"No tool call found for function call output"}}`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		if got := isContextLengthExceededBody([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsContextLengthExceededFailedPayload(t *testing.T) {
	payload := `{"type":"response.failed","response":{"status":"failed","status_code":400,"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"}}}`
	if !isContextLengthExceededFailedPayload([]byte(payload)) {
		t.Fatal("real response.failed overflow payload should match")
	}
	other := `{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`
	if isContextLengthExceededFailedPayload([]byte(other)) {
		t.Fatal("non-overflow failure must not match")
	}
	if isContextLengthExceededFailedPayload(nil) {
		t.Fatal("empty payload must not match")
	}
}

func TestCompactOverflowResponsesBody_KeepsSystemAndTail(t *testing.T) {
	t.Setenv("CODEX_OVERFLOW_COMPACT_TAIL_KB", "1")

	big := strings.Repeat("x", 900)
	body := `{
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"system prompt"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"old fact: the project codename is Bluebird. ` + big + `"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"recent question"}]}
		]
	}`

	h := &Handler{}
	// body 无 model 字段：摘要调用被跳过，走省略标记退化路径，
	// 使该测试不依赖真实上游。
	got, ok := h.compactOverflowResponsesBody(context.Background(), []byte(body))
	if !ok {
		t.Fatal("expected compaction to succeed")
	}

	items := gjson.GetBytes(got, "input").Array()
	if len(items) >= 5 {
		t.Fatalf("expected old turns to be compacted away, got %d items: %s", len(items), got)
	}
	if role := items[0].Get("role").String(); role != "developer" {
		t.Fatalf("system prompt message should stay first, got %q", role)
	}
	if text := items[0].Get("content.0.text").String(); text != "system prompt" {
		t.Fatalf("system prompt content should be untouched, got %q", text)
	}
	if text := items[1].Get("content.0.text").String(); !strings.Contains(text, "omitted") {
		t.Fatalf("second item should be the compaction placeholder, got %q", text)
	}
	last := items[len(items)-1]
	if text := last.Get("content.0.text").String(); text != "recent question" {
		t.Fatalf("most recent turn should be preserved verbatim, got %q", text)
	}
}

func TestCompactOverflowResponsesBody_TooFewItems(t *testing.T) {
	body := `{"input":[{"type":"message","role":"user","content":"only one"}]}`
	h := &Handler{}
	if _, ok := h.compactOverflowResponsesBody(context.Background(), []byte(body)); ok {
		t.Fatal("single-item input must not be compacted")
	}
}

func TestCompactOverflowResponsesBody_RepairsCutToolPairs(t *testing.T) {
	t.Setenv("CODEX_OVERFLOW_COMPACT_TAIL_KB", "1")

	big := strings.Repeat("y", 900)
	body := `{
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + big + `"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + big + `"}]},
			{"type":"function_call","call_id":"call_cut1","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_cut1","output":"tool result kept in tail"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"recent"}]}
		]
	}`

	h := &Handler{}
	got, ok := h.compactOverflowResponsesBody(context.Background(), []byte(body))
	if !ok {
		t.Fatal("expected compaction to succeed")
	}
	// 不论切点落在哪，产出必须不含孤儿 *_call_output（上游会 400）。
	var orphan bool
	items := gjson.GetBytes(got, "input").Array()
	calls := map[string]bool{}
	for _, item := range items {
		if isCodexToolCallContextType(item.Get("type").String()) {
			calls[item.Get("call_id").String()] = true
		}
	}
	for _, item := range items {
		if isCodexToolCallOutputType(item.Get("type").String()) && !calls[item.Get("call_id").String()] {
			orphan = true
		}
	}
	if orphan {
		t.Fatalf("compacted input must not contain orphan tool outputs: %s", got)
	}
}

func TestFlattenOverflowItemsTranscript_CapsMiddle(t *testing.T) {
	items := []any{
		map[string]any{"type": "message", "role": "user", "content": "HEAD-" + strings.Repeat("a", 3000)},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": strings.Repeat("b", 3000)}}},
		map[string]any{"type": "message", "role": "user", "content": strings.Repeat("c", 3000) + "-TAIL"},
	}
	got := flattenOverflowItemsTranscript(items, 2000)
	if len(got) > 2100 {
		t.Fatalf("transcript should be capped near 2000 bytes, got %d", len(got))
	}
	if !strings.Contains(got, "HEAD-") || !strings.Contains(got, "-TAIL") {
		t.Fatalf("cap should keep both head and tail, got %q...", got[:80])
	}
	if !strings.Contains(got, "truncated") {
		t.Fatal("cap should insert a truncation marker")
	}
}

func TestExtractResponsesSSEOutputText(t *testing.T) {
	sse := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"par\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"reasoning\"},{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"part one. \"},{\"type\":\"output_text\",\"text\":\"part two.\"}]}]}}\n\n" +
		"data: [DONE]\n\n"
	if got := extractResponsesSSEOutputText([]byte(sse)); got != "part one. part two." {
		t.Fatalf("unexpected extracted text: %q", got)
	}
	if got := extractResponsesSSEOutputText([]byte("data: [DONE]\n")); got != "" {
		t.Fatalf("no completed event should yield empty, got %q", got)
	}
}
