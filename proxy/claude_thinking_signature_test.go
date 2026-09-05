package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const validThinkingSig = "EqQBCgIYAhIM1gbcDa9GJwZA2bAbcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"

func TestDropUnsignedClaudeThinkingBlocks(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"a","signature":""},
			{"type":"thinking","thinking":"b","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b"},
			{"type":"thinking","thinking":"c","signature":"` + validThinkingSig + `"},
			{"type":"redacted_thinking","data":"opaque"},
			{"type":"text","text":"ok"},
			{"type":"tool_use","id":"toolu_1","name":"echo","input":{"text":"hi"}}
		]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hi"}]}
	]}`)
	out, dropped := dropUnsignedClaudeThinkingBlocks(body)
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2 (empty + truncated signature)", dropped)
	}
	blocks := gjson.GetBytes(out, "messages.1.content").Array()
	types := make([]string, 0, len(blocks))
	for _, b := range blocks {
		types = append(types, b.Get("type").String())
	}
	want := "thinking,redacted_thinking,text,tool_use"
	if got := strings.Join(types, ","); got != want {
		t.Fatalf("remaining blocks = %s, want %s", got, want)
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.thinking").String(); got != "c" {
		t.Fatalf("kept the wrong thinking block: %q", got)
	}
	if gjson.GetBytes(out, "messages.0.content").String() != "hi" || gjson.GetBytes(out, "messages.2.content.0.type").String() != "tool_result" {
		t.Fatal("non-assistant messages must be untouched")
	}
}

func TestDropUnsignedClaudeThinkingBlocks_NoChangeReturnsSameBody(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"c","signature":"` + validThinkingSig + `"},{"type":"text","text":"ok"}]}]}`)
	out, dropped := dropUnsignedClaudeThinkingBlocks(body)
	if dropped != 0 || !bytes.Equal(out, body) {
		t.Fatalf("valid signatures must be left byte-for-byte intact (dropped=%d)", dropped)
	}
}

func TestStripClaudeThinkingBlocks(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"thinking","thinking":"c","signature":"` + validThinkingSig + `"},{"type":"redacted_thinking","data":"x"},{"type":"text","text":"ok"}]},
		{"role":"user","content":"next"},
		{"role":"assistant","content":"plain string"}
	]}`)
	out, stripped := stripClaudeThinkingBlocks(body)
	if stripped != 2 {
		t.Fatalf("stripped = %d, want 2", stripped)
	}
	if got := gjson.GetBytes(out, "messages.0.content.#").Int(); got != 1 {
		t.Fatalf("assistant content blocks = %d, want 1 (text only)", got)
	}
	if gjson.GetBytes(out, "messages.2.content").String() != "plain string" {
		t.Fatal("string content must be untouched")
	}
}

func TestIsClaudeThinkingSignatureError(t *testing.T) {
	sigErr := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: Invalid ` + "`signature`" + ` in ` + "`thinking`" + ` block"}}`)
	if !isClaudeThinkingSignatureError(400, sigErr) {
		t.Fatal("signature error must be recognised")
	}
	if isClaudeThinkingSignatureError(400, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"A maximum of 4 blocks with cache_control may be provided"}}`)) {
		t.Fatal("other invalid_request errors must not match")
	}
	if isClaudeThinkingSignatureError(500, sigErr) {
		t.Fatal("non-400 must not match")
	}
}

func fakeHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestExecuteClaudeWithThinkingSignatureRetry_StripsAndRetriesOnce(t *testing.T) {
	sigErr := `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: Invalid ` + "`signature`" + ` in ` + "`thinking`" + ` block"}}`
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"c","signature":"` + validThinkingSig + `"},{"type":"tool_use","id":"t1","name":"echo","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"hi"}]}]}`)
	var sent [][]byte
	exec := func(_ context.Context, b []byte) (*http.Response, error) {
		sent = append(sent, b)
		if len(sent) == 1 {
			return fakeHTTPResponse(400, sigErr), nil
		}
		return fakeHTTPResponse(200, `{"type":"message","content":[{"type":"text","text":"ok"}]}`), nil
	}
	resp, err := executeClaudeWithThinkingSignatureRetry(context.Background(), body, exec)
	if err != nil || resp == nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v, want 200 after retry", resp, err)
	}
	if len(sent) != 2 {
		t.Fatalf("upstream called %d times, want 2", len(sent))
	}
	if gjson.GetBytes(sent[1], "messages.1.content.#").Int() != 1 || gjson.GetBytes(sent[1], "messages.1.content.0.type").String() != "tool_use" {
		t.Fatalf("retry body must have thinking stripped, got %s", sent[1])
	}
}

func TestExecuteClaudeWithThinkingSignatureRetry_NoRetryWithoutThinking(t *testing.T) {
	sigErr := `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: Invalid ` + "`signature`" + ` in ` + "`thinking`" + ` block"}}`
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`)
	calls := 0
	exec := func(_ context.Context, _ []byte) (*http.Response, error) {
		calls++
		return fakeHTTPResponse(400, sigErr), nil
	}
	resp, err := executeClaudeWithThinkingSignatureRetry(context.Background(), body, exec)
	if err != nil || resp.StatusCode != 400 || calls != 1 {
		t.Fatalf("resp=%v err=%v calls=%d; want original 400 passed through with one call", resp, err, calls)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != sigErr {
		t.Fatalf("error body must be preserved for the caller, got %s", got)
	}
}

func TestExecuteClaudeWithThinkingSignatureRetry_OtherErrorsPassThrough(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"c","signature":"` + validThinkingSig + `"}]}]}`)
	calls := 0
	exec := func(_ context.Context, _ []byte) (*http.Response, error) {
		calls++
		return fakeHTTPResponse(429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`), nil
	}
	resp, _ := executeClaudeWithThinkingSignatureRetry(context.Background(), body, exec)
	if resp.StatusCode != 429 || calls != 1 {
		t.Fatalf("status=%d calls=%d; non-signature errors must not trigger a retry", resp.StatusCode, calls)
	}
}

func TestInjectClaudeCodeSystemPrompt_OmitsCacheControlAtLimit(t *testing.T) {
	block := `{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}`
	body := []byte(`{"system":[` + block + `,` + block + `],"tools":[{"name":"a","input_schema":{},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[` + block + `]}]}`)
	out := injectClaudeCodeSystemPrompt(body)
	first := gjson.GetBytes(out, "system.1")
	if !strings.HasPrefix(first.Get("text").String(), claudeCodeSystemPreamble) {
		t.Fatalf("preamble must still be injected: %s", first.Raw)
	}
	if first.Get("cache_control").Exists() {
		t.Fatal("client already has 4 cache_control blocks; injected preamble must not add a 5th")
	}
}

func TestInjectClaudeCodeSystemPrompt_KeepsCacheControlBelowLimit(t *testing.T) {
	block := `{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}`
	body := []byte(`{"system":[` + block + `],"messages":[{"role":"user","content":"hi"}]}`)
	out := injectClaudeCodeSystemPrompt(body)
	if !gjson.GetBytes(out, "system.1.cache_control").Exists() {
		t.Fatal("with only 1 client cache_control block the preamble keeps its cache_control")
	}
}
