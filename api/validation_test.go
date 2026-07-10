package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestValidateResponsesAPIRequestRejectsUnsupportedModel(t *testing.T) {
	result := ValidateResponsesAPIRequest(
		[]byte(`{"model":"gpt-unknown","input":"hello"}`),
		[]string{"gpt-5.5", "gpt-5.4"},
	)

	if result.Valid {
		t.Fatal("expected unsupported model to be invalid")
	}
	if len(result.Errors) != 1 {
		t.Fatalf("errors length = %d, want 1", len(result.Errors))
	}
	if got := result.Errors[0].Code; got != "unsupported_model" {
		t.Fatalf("error code = %q, want unsupported_model", got)
	}
}

func TestValidateResponsesAPIRequestAllowsObjectToolChoice(t *testing.T) {
	result := ValidateResponsesAPIRequest(
		[]byte(`{"model":"gpt-5.4","input":"draw a cat","tool_choice":{"type":"image_generation"}}`),
		[]string{"gpt-5.4"},
	)

	if !result.Valid {
		t.Fatalf("expected object tool_choice to be valid, got %#v", result.Errors)
	}
}

func TestValidateResponsesAPIRequestAllowsCodexToolInputTypes(t *testing.T) {
	result := ValidateResponsesAPIRequest(
		[]byte(`{
			"model":"gpt-5.4",
			"input":[
				{"type":"tool_search_output","call_id":"call_search","output":"ok"},
				{"type":"local_shell_call_output","call_id":"call_shell","output":"ok"},
				{"type":"custom_tool_call_output","call_id":"call_custom","output":"ok"},
				{"type":"mcp_tool_call_output","call_id":"call_mcp","output":"ok"},
				{"type":"image_generation_call","id":"ig_1","status":"completed"},
				{"type":"web_search_call","id":"ws_1","status":"completed"}
			]
		}`),
		[]string{"gpt-5.4"},
	)

	if !result.Valid {
		t.Fatalf("expected Codex tool input types to be valid, got %#v", result.Errors)
	}
}

func TestValidateResponsesAPIRequestAllowsCompactionInputType(t *testing.T) {
	result := ValidateResponsesAPIRequest(
		[]byte(`{
			"model":"gpt-5.4",
			"input":[
				{"type":"message","role":"user","content":"hello"},
				{"type":"compaction","summary":"previous context was compacted"}
			]
		}`),
		[]string{"gpt-5.4"},
	)

	if !result.Valid {
		t.Fatalf("expected compaction input type to be valid, got %#v", result.Errors)
	}
}

func TestValidateResponsesAPIRequestAllowsOfficialContentInputTypes(t *testing.T) {
	result := ValidateResponsesAPIRequest(
		[]byte(`{
			"model":"gpt-5.4",
			"input":[
				{"type":"input_text","text":"hello"},
				{"type":"input_image","image_url":"https://example.com/cat.png"},
				{"type":"input_file","file_id":"file_abc"},
				{"type":"computer_screenshot","image_url":"https://example.com/screen.png"},
				{"type":"summary_text","text":"summary"}
			]
		}`),
		[]string{"gpt-5.4"},
	)

	if !result.Valid {
		t.Fatalf("expected official Responses content input types to be valid, got %#v", result.Errors)
	}
}

func TestValidateResponsesAPIRequestMaxOutputTokensCap(t *testing.T) {
	tests := []struct {
		name  string
		body  []byte
		valid bool
	}{
		{
			name:  "gpt-5.5 allows 128k output tokens",
			body:  []byte(`{"model":"gpt-5.5","input":"hello","max_output_tokens":128000}`),
			valid: true,
		},
		{
			name:  "gpt-5.5 rejects above 128k output tokens",
			body:  []byte(`{"model":"gpt-5.5","input":"hello","max_output_tokens":128001}`),
			valid: false,
		},
		{
			name:  "other models also allow up to 128k (aligned cap, upstream decides actual ceiling)",
			body:  []byte(`{"model":"gpt-5.4","input":"hello","max_output_tokens":100000}`),
			valid: true,
		},
		{
			name:  "other models reject above 128k",
			body:  []byte(`{"model":"gpt-5.4","input":"hello","max_output_tokens":128001}`),
			valid: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := ValidateResponsesAPIRequest(test.body, []string{"gpt-5.5", "gpt-5.4"})
			if result.Valid != test.valid {
				t.Fatalf("Valid = %v, want %v; errors=%#v", result.Valid, test.valid, result.Errors)
			}
		})
	}
}

func TestValidateResponsesAPIRequestRejectsUnknownInputType(t *testing.T) {
	result := ValidateResponsesAPIRequest(
		[]byte(`{"model":"gpt-5.4","input":[{"type":"unknown_call","call_id":"call_1"}]}`),
		[]string{"gpt-5.4"},
	)

	if result.Valid {
		t.Fatal("expected unknown input type to be invalid")
	}
	if len(result.Errors) != 1 || result.Errors[0].Code != "invalid_input_type" {
		t.Fatalf("expected invalid_input_type, got %#v", result.Errors)
	}
}

func TestValidateResponsesAPIRequestAcceptsCompactV2InputTypes(t *testing.T) {
	// 新版 Codex CLI（compact v2）发送 compaction_trigger 触发服务端压缩，
	// 后续请求携带 context_compaction/compaction 加密上下文。
	result := ValidateResponsesAPIRequest(
		[]byte(`{"model":"gpt-5.4","input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction_trigger"},
			{"type":"context_compaction","encrypted_content":"opaque"},
			{"type":"compaction","encrypted_content":"opaque"}
		]}`),
		[]string{"gpt-5.4"},
	)

	if !result.Valid {
		t.Fatalf("expected compact v2 input types to be valid, got %#v", result.Errors)
	}
}

func TestValidateResponsesAPIRequestAcceptsAgentMessageInput(t *testing.T) {
	// multi-agent 会话续写时,历史里的代理间消息会随 input 回放(issue #341)。
	result := ValidateResponsesAPIRequest(
		[]byte(`{"model":"gpt-5.5","input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Message Type: MESSAGE"}]}
		]}`),
		[]string{"gpt-5.5"},
	)

	if !result.Valid {
		t.Fatalf("expected agent_message input to be valid, got %#v", result.Errors)
	}
}

func TestSendListIncludesOptionalHasMore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	SendList(c, "list", []string{"gpt-5.5"}, true)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Object  string   `json:"object"`
		Data    []string `json:"data"`
		HasMore *bool    `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Object != "list" {
		t.Fatalf("object = %q, want list", body.Object)
	}
	if len(body.Data) != 1 || body.Data[0] != "gpt-5.5" {
		t.Fatalf("data = %#v, want [gpt-5.5]", body.Data)
	}
	if body.HasMore == nil || !*body.HasMore {
		t.Fatalf("has_more = %#v, want true", body.HasMore)
	}
}

func TestHTTPStatusCodeForCommonAPIErrors(t *testing.T) {
	tests := []struct {
		name string
		code ErrorCode
		want int
	}{
		{name: "auth", code: ErrCodeInvalidAPIKey, want: http.StatusUnauthorized},
		{name: "rate limit", code: ErrCodeRateLimitReached, want: http.StatusTooManyRequests},
		{name: "unsupported model", code: ErrCodeUnsupportedModel, want: http.StatusBadRequest},
		{name: "upstream timeout", code: ErrCodeUpstreamTimeout, want: http.StatusInternalServerError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := HTTPStatusCode(test.code); got != test.want {
				t.Fatalf("HTTPStatusCode(%q) = %d, want %d", test.code, got, test.want)
			}
		})
	}
}
