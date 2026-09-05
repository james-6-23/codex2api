package proxy

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const thinkingDisabledErr = `{"type":"error","error":{"type":"invalid_request_error","message":"\"thinking.type.disabled\" is not supported for this model. Thinking defaults to adaptive mode when not specified; use \"thinking.type.enabled\" with \"budget_tokens\" for extended thinking."}}`

func TestDropClaudeDisabledThinkingForAlwaysOnModels(t *testing.T) {
	cases := []struct {
		name, body string
		wantDrop   bool
	}{
		{"fable 5.1 disabled", `{"model":"claude-fable-5-1","thinking":{"type":"disabled"},"messages":[]}`, true},
		{"fable 5 disabled", `{"model":"claude-fable-5","thinking":{"type":"disabled"},"messages":[]}`, true},
		{"mythos disabled", `{"model":"claude-mythos-5-1","thinking":{"type":"disabled"},"messages":[]}`, true},
		{"fable adaptive kept", `{"model":"claude-fable-5-1","thinking":{"type":"adaptive"},"messages":[]}`, false},
		{"opus 5 disabled kept (allowed at effort<=high)", `{"model":"claude-opus-5","thinking":{"type":"disabled"},"messages":[]}`, false},
		{"no thinking field", `{"model":"claude-fable-5-1","messages":[]}`, false},
	}
	for _, tc := range cases {
		out, dropped := dropClaudeDisabledThinking([]byte(tc.body))
		if dropped != tc.wantDrop {
			t.Fatalf("%s: dropped=%v want %v", tc.name, dropped, tc.wantDrop)
		}
		if tc.wantDrop && gjson.GetBytes(out, "thinking").Exists() {
			t.Fatalf("%s: thinking must be removed, got %s", tc.name, out)
		}
		if !tc.wantDrop && string(out) != tc.body {
			t.Fatalf("%s: body must be untouched", tc.name)
		}
	}
}

func TestPrepareClaudeRequestBody_DropsDisabledThinkingForFable(t *testing.T) {
	out, err := prepareClaudeRequestBody([]byte(`{"model":"claude-fable-5-1","max_tokens":10,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`), auth.DefaultClaudeSecurityConfig(), claudeBodyIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("thinking.disabled must not reach Anthropic for Fable: %s", out)
	}
	if gjson.GetBytes(out, "max_tokens").Int() != 10 {
		t.Fatal("other fields must survive")
	}
}

func TestIsClaudeThinkingDisabledUnsupportedError(t *testing.T) {
	if !isClaudeThinkingDisabledUnsupportedError(400, []byte(thinkingDisabledErr)) {
		t.Fatal("must recognise the disabled-thinking rejection")
	}
	effortErr := `{"type":"error","error":{"type":"invalid_request_error","message":"output_config.effort 'max' is not supported when thinking is disabled on this model. Use effort 'high' or below, or enable thinking."}}`
	if !isClaudeThinkingDisabledUnsupportedError(400, []byte(effortErr)) {
		t.Fatal("effort-vs-disabled rejection must be treated the same way (drop thinking)")
	}
	if isClaudeThinkingDisabledUnsupportedError(400, []byte(`{"error":{"type":"invalid_request_error","message":"messages.1.content.0: Invalid signature in thinking block"}}`)) {
		t.Fatal("signature errors are a different rectifier")
	}
}

func TestExecuteClaudeWithThinkingSignatureRetry_RemovesDisabledThinkingOnRejection(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","output_config":{"effort":"max"},"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	var sent [][]byte
	exec := func(_ context.Context, b []byte) (*http.Response, error) {
		sent = append(sent, b)
		if len(sent) == 1 {
			return fakeHTTPResponse(400, thinkingDisabledErr), nil
		}
		return fakeHTTPResponse(200, `{"type":"message","content":[]}`), nil
	}
	resp, err := executeClaudeWithThinkingSignatureRetry(context.Background(), body, exec)
	if err != nil || resp.StatusCode != 200 || len(sent) != 2 {
		t.Fatalf("resp=%v err=%v sent=%d", resp, err, len(sent))
	}
	if gjson.GetBytes(sent[1], "thinking").Exists() || gjson.GetBytes(sent[1], "output_config.effort").String() != "max" {
		t.Fatalf("retry must drop thinking only: %s", sent[1])
	}
	// Without a thinking field there is nothing to fix: pass the 400 through untouched.
	calls := 0
	resp, _ = executeClaudeWithThinkingSignatureRetry(context.Background(), []byte(`{"model":"claude-opus-5","messages":[]}`), func(_ context.Context, _ []byte) (*http.Response, error) {
		calls++
		return fakeHTTPResponse(400, thinkingDisabledErr), nil
	})
	got, _ := io.ReadAll(resp.Body)
	if calls != 1 || resp.StatusCode != 400 || string(got) != thinkingDisabledErr {
		t.Fatalf("calls=%d status=%d body=%s", calls, resp.StatusCode, got)
	}
}
