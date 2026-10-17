package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func codexSearchTestResponse(events ...string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: " + strings.Join(events, "\n\ndata: ") + "\n\n"))}
}

func codexSearchTestCall(name, query string) map[string]any {
	arguments, _ := json.Marshal(map[string]any{"query": query})
	return map[string]any{"type": "function_call", "id": "fc_search", "call_id": "call_search", "name": name, "arguments": string(arguments)}
}

func codexSearchTestCompleted(items ...any) string {
	return string(customToolTestEvent("response.completed", map[string]any{"response": map[string]any{"status": "completed", "output": items, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "input_tokens_details": map[string]any{"cached_tokens": 2}, "output_tokens_details": map[string]any{"reasoning_tokens": 1}}}}))
}

func TestPrepareAnthropicCodexWebSearch(t *testing.T) {
	for _, body := range []string{`{"input":[],"tools":[]}`, `{"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}]}`} {
		out, bridge, err := prepareAnthropicCodexWebSearch([]byte(body), nil)
		if err != nil || bridge != nil || string(out) != body {
			t.Fatalf("ordinary requests must be unchanged: body=%s bridge=%+v err=%v", out, bridge, err)
		}
	}
	body := []byte(`{"model":"gpt-6-luna","input":[],"tools":[{"type":"web_search","filters":{"allowed_domains":["python.org"]}},{"type":"function","name":"Read","parameters":{"type":"object"}},{"type":"function","name":"codex2api_web_search","parameters":{"type":"object"}}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search"}]}}`)
	original := bytes.Clone(body)
	out, bridge, err := prepareAnthropicCodexWebSearch(body, []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if bridge.name == "codex2api_web_search" || bridge.remaining != 1 || len(bridge.domains) != 1 || bridge.domains[0] != "python.org" {
		t.Fatalf("config/collision handling failed: %+v", bridge)
	}
	if !bytes.Equal(original, body) || gjson.GetBytes(out, "tools.#").Int() != 3 || gjson.GetBytes(out, "tools.1.name").String() != "Read" || gjson.GetBytes(out, "tool_choice.tools.0.name").String() != bridge.name || gjson.GetBytes(out, "tool_choice.tools.0.type").String() != "function" {
		t.Fatalf("tool definitions, choice or ingress changed: %s", out)
	}
	for _, tool := range []string{`{"type":"web_search","user_location":{"city":"Paris"}}`, `{"type":"web_search","filters":{"blocked_domains":["example.com"]}}`} {
		if _, _, err := prepareAnthropicCodexWebSearch([]byte(`{"tools":[`+tool+`]}`), nil); err == nil {
			t.Fatal("unsupported search controls must not be silently dropped")
		}
	}
	for _, limit := range []string{`-1`, `1.5`, `"1"`, `null`} {
		if _, _, err := prepareAnthropicCodexWebSearch([]byte(`{"tools":[{"type":"web_search"}]}`), []byte(`{"tools":[{"name":"web_search","max_uses":`+limit+`}]}`)); err == nil {
			t.Fatalf("invalid max_uses %s was accepted", limit)
		}
	}
}

func TestAnthropicCodexWebSearchOfficialRoundTrip(t *testing.T) {
	body, bridge, err := prepareAnthropicCodexWebSearch([]byte(`{"model":"gpt-6-luna","tools":[{"type":"web_search","filters":{"allowed_domains":["python.org"]}}],"tool_choice":"required","input":[{"role":"user","content":"Find Python"}]}`), []byte(`{"tools":[{"name":"web_search","max_uses":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	call := codexSearchTestCall(bridge.name, "Python 官方网站")
	first := codexSearchTestResponse(`{"type":"response.created","response":{"id":"resp_first"}}`, string(mustMarshalSearchEvent("response.output_item.added", call)), `{"type":"response.function_call_arguments.delta","item_id":"fc_search","delta":"private arguments"}`, codexSearchTestCompleted(call))
	searches, rounds := 0, 0
	search := func(raw []byte) (*CodexAlphaSearchResponse, error) {
		searches++
		if gjson.GetBytes(raw, "model").String() != "gpt-6-luna" || gjson.GetBytes(raw, "commands.search_query.0.q").String() != "Python 官方网站" || gjson.GetBytes(raw, "commands.search_query.0.domains.0").String() != "python.org" || !strings.HasPrefix(gjson.GetBytes(raw, "id").String(), "srch_") {
			t.Errorf("bad official search request: %s", raw)
		}
		return &CodexAlphaSearchResponse{StatusCode: 200, Body: []byte(`{"encrypted_output":"opaque-search-state","output":"Official search found Python: https://www.python.org/","results":[{"type":"text_result","url":"https://www.python.org/","title":"Python","snippet":"Official Python website","ref_id":"turn0search0"}]}`)}, nil
	}
	open := func(next []byte) (*http.Response, error) {
		rounds++
		if gjson.GetBytes(next, "tool_choice").String() != "auto" || len(gjson.GetBytes(next, "tools").Array()) != 0 || gjson.GetBytes(next, "input.1.name").String() != bridge.name || gjson.GetBytes(next, "input.2.call_id").String() != "call_search" {
			t.Errorf("continuation lost tool pairing or limit: %s", next)
		}
		if output := gjson.GetBytes(next, "input.2.output").String(); !strings.Contains(output, "Official search found Python") || strings.Contains(output, "opaque-search-state") {
			t.Errorf("official results must reach the model as tool output: %s", output)
		}
		message := map[string]any{"type": "message", "id": "msg_answer", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Python: https://www.python.org/"}}}
		return codexSearchTestResponse(`{"type":"response.created","response":{"id":"resp_second"}}`, string(mustMarshalSearchEvent("response.output_item.added", message)), `{"type":"response.output_text.delta","delta":"Python: https://www.python.org/"}`, string(mustMarshalSearchEvent("response.output_item.done", message)), codexSearchTestCompleted(message)), nil
	}
	var stream strings.Builder
	if err := bridge.run(context.Background(), first, body, open, search, &stream); err != nil {
		t.Fatal(err)
	}
	if searches != 1 || rounds != 1 || strings.Contains(stream.String(), bridge.name) || strings.Contains(stream.String(), "private arguments") {
		t.Fatalf("search was skipped, duplicated or leaked: searches=%d rounds=%d stream=%s", searches, rounds, stream.String())
	}
	translator := newAnthropicStreamTranslator("claude-sonnet-5-5")
	accumulator := newAnthropicResponseAccumulator("claude-sonnet-5-5")
	var terminal []byte
	created, completed := 0, 0
	err = readSSEStreamWithContinuousRetryKeepalive(context.Background(), strings.NewReader(stream.String()), func(_ string, data []byte) bool {
		kind := gjson.GetBytes(data, "type").String()
		if kind == "response.created" {
			created++
		}
		if kind == "response.completed" {
			completed++
			terminal = bytes.Clone(data)
		}
		accumulator.apply(translator.translateEvent(data))
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	response := accumulator.build(terminal)
	if created != 1 || completed != 1 || len(response.Content) != 3 || response.Content[0].Type != "server_tool_use" || response.Content[1].Type != "web_search_tool_result" || response.Content[0].ID != response.Content[1].ToolUseID || response.Content[2].Text != "Python: https://www.python.org/" || response.StopReason == nil || *response.StopReason != "end_turn" {
		t.Fatalf("broken Claude search response: %+v", response)
	}
	if gjson.GetBytes(response.Content[1].Content, "0.url").String() != "https://www.python.org/" || gjson.GetBytes(terminal, "response.usage.input_tokens").Int() != 20 || gjson.GetBytes(terminal, "response.usage.output_tokens").Int() != 10 || gjson.GetBytes(terminal, "response.usage.input_tokens_details.cached_tokens").Int() != 4 {
		t.Fatalf("sources or actual multi-round usage were lost: %s", terminal)
	}
}

func TestAnthropicCodexWebSearchErrors(t *testing.T) {
	for name, test := range map[string]struct {
		arguments string
		remaining int
		response  *CodexAlphaSearchResponse
		err       error
		code      string
	}{
		"invalid input": {arguments: `{"query":42}`, remaining: 1, code: "invalid_tool_input"},
		"bad JSON":      {arguments: `{"query":`, remaining: 1, code: "invalid_tool_input"},
		"empty query":   {arguments: `{"query":"  "}`, remaining: 1, code: "invalid_tool_input"},
		"limit":         {arguments: `{"query":"Python"}`, code: "max_uses_exceeded"},
		"rate limit":    {arguments: `{"query":"Python"}`, remaining: 1, response: &CodexAlphaSearchResponse{StatusCode: 429}, code: "too_many_requests"},
		"upstream 400":  {arguments: `{"query":"Python"}`, remaining: 1, response: &CodexAlphaSearchResponse{StatusCode: 400}, code: "invalid_tool_input"},
		"upstream 500":  {arguments: `{"query":"Python"}`, remaining: 1, response: &CodexAlphaSearchResponse{StatusCode: 500}, code: "unavailable"},
		"network":       {arguments: `{"query":"Python"}`, remaining: 1, err: errors.New("network unavailable"), code: "unavailable"},
		"malformed":     {arguments: `{"query":"Python"}`, remaining: 1, response: &CodexAlphaSearchResponse{StatusCode: 200, Body: []byte(`{`)}, code: "unavailable"},
		"missing body":  {arguments: `{"query":"Python"}`, remaining: 1, response: &CodexAlphaSearchResponse{StatusCode: 200, Body: []byte(`{"encrypted_output":"opaque"}`)}, code: "unavailable"},
		"wrong output":  {arguments: `{"query":"Python"}`, remaining: 1, response: &CodexAlphaSearchResponse{StatusCode: 200, Body: []byte(`{"output":null,"results":[]}`)}, code: "unavailable"},
		"wrong results": {arguments: `{"query":"Python"}`, remaining: 1, response: &CodexAlphaSearchResponse{StatusCode: 200, Body: []byte(`{"output":"results","results":{}}`)}, code: "unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			bridge := &anthropicCodexWebSearch{name: "internal_search", model: "gpt-6-luna", remaining: test.remaining}
			call := codexSearchTestCall(bridge.name, "Python")
			call["arguments"] = test.arguments
			raw, _ := json.Marshal(call)
			item, result, err := bridge.executeSearch(context.Background(), gjson.ParseBytes(raw), func([]byte) (*CodexAlphaSearchResponse, error) { return test.response, test.err }, func([]byte) bool { return true })
			if err != nil || gjson.GetBytes(item, "status").String() != "failed" || gjson.GetBytes(item, "error.code").String() != test.code || gjson.Get(gjson.GetBytes(result, "output").String(), "error").String() != test.code {
				t.Fatalf("error was hidden or changed: item=%s result=%s err=%v", item, result, err)
			}
			blocks := anthropicServerToolBlocks(gjson.ParseBytes(item))
			if len(blocks) != 2 || blocks[0].ID != blocks[1].ToolUseID || gjson.GetBytes(blocks[1].Content, "error_code").String() != test.code {
				t.Fatalf("search error must remain paired for Claude: %+v", blocks)
			}
		})
	}
}

func TestAnthropicCodexWebSearchDoneArguments(t *testing.T) {
	for _, terminalOnly := range []bool{false, true} {
		bridge := &anthropicCodexWebSearch{name: "internal_search", model: "gpt-6-luna", remaining: 1}
		call := codexSearchTestCall(bridge.name, "Python")
		delete(call, "arguments")
		var completed string
		if terminalOnly {
			completed = codexSearchTestCompleted()
		} else {
			completed = codexSearchTestCompleted(call)
		}
		first := codexSearchTestResponse(string(mustMarshalSearchEvent("response.output_item.added", call)), string(customToolTestEvent("response.function_call_arguments.done", map[string]any{"item_id": "fc_search", "arguments": `{"query":"Python"}`})), completed)
		var output strings.Builder
		err := bridge.run(context.Background(), first, []byte(`{"input":[],"tools":[]}`), func(next []byte) (*http.Response, error) {
			if gjson.Get(gjson.GetBytes(next, "input.0.arguments").String(), "query").String() != "Python" {
				t.Fatalf("completed tool arguments were lost: %s", next)
			}
			return codexSearchTestResponse(codexSearchTestCompleted()), nil
		}, func(raw []byte) (*CodexAlphaSearchResponse, error) {
			if gjson.GetBytes(raw, "commands.search_query.0.q").String() != "Python" {
				t.Fatalf("search arguments were lost: %s", raw)
			}
			return &CodexAlphaSearchResponse{StatusCode: 200, Body: []byte(`{"output":"No results","results":[]}`)}, nil
		}, &output)
		if err != nil || strings.Contains(output.String(), `"status":"failed"`) {
			t.Fatalf("done-only arguments failed: err=%v stream=%s", err, output.String())
		}
	}
}

func TestAnthropicCodexWebSearchPreservesParallelClientTool(t *testing.T) {
	bridge := &anthropicCodexWebSearch{name: "internal_search", model: "gpt-6-luna", remaining: 1}
	searchCall := codexSearchTestCall(bridge.name, "Python")
	clientCall := map[string]any{"type": "function_call", "id": "fc_read", "call_id": "call_read", "name": "Read", "arguments": `{"file_path":"README.md"}`}
	first := codexSearchTestResponse(string(mustMarshalSearchEvent("response.output_item.added", clientCall)), string(mustMarshalSearchEvent("response.output_item.added", searchCall)), string(customToolTestEvent("response.function_call_arguments.delta", map[string]any{"item_id": "fc_read", "delta": `{"file_path":"README.md"}`})), string(mustMarshalSearchEvent("response.output_item.done", clientCall)), codexSearchTestCompleted(searchCall, clientCall))
	var output strings.Builder
	err := bridge.run(context.Background(), first, []byte(`{"input":[]}`), func([]byte) (*http.Response, error) {
		t.Fatal("client tools must be returned to Claude before another model round")
		return nil, nil
	}, func([]byte) (*CodexAlphaSearchResponse, error) {
		return &CodexAlphaSearchResponse{StatusCode: 200, Body: []byte(`{"output":"Python","results":[]}`)}, nil
	}, &output)
	if err != nil || !strings.Contains(output.String(), `"item_id":"fc_read"`) || !strings.Contains(output.String(), `"name":"Read"`) || strings.Contains(output.String(), bridge.name) {
		t.Fatalf("parallel client call was hidden or executed: err=%v stream=%s", err, output.String())
	}
}

func TestAnthropicCodexWebSearchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := &anthropicCodexWebSearch{name: "internal_search", model: "gpt-6-luna", remaining: 1}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- bridge.run(ctx, codexSearchTestResponse(codexSearchTestCompleted(codexSearchTestCall(bridge.name, "Python"))), []byte(`{"input":[]}`), func([]byte) (*http.Response, error) {
			t.Error("cancelled search resumed generation")
			return nil, ctx.Err()
		}, func([]byte) (*CodexAlphaSearchResponse, error) { close(started); <-ctx.Done(); return nil, ctx.Err() }, io.Discard)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("search did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation was swallowed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled search did not stop")
	}
}

func TestAnthropicCodexWebSearchMessagesHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "SSE"}[stream], func(t *testing.T) {
			previous := CurrentRuntimeSettings()
			runtime := previous
			runtime.CodexForceWebsocket = false
			ApplyRuntimeSettings(runtime)
			t.Cleanup(func() { ApplyRuntimeSettings(previous) })
			searchCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				searchCalls++
				if r.Header.Get("Authorization") != "Bearer at-1" || r.Header.Get("chatgpt-account-id") != "acct-1" {
					t.Error("official search did not use the selected Codex account")
				}
				_, _ = io.WriteString(w, `{"output":"Python official website https://www.python.org/","results":[{"url":"https://www.python.org/","title":"Python"}]}`)
			}))
			t.Cleanup(upstream.Close)
			previousURL := codexAlphaSearchURLForTest
			codexAlphaSearchURLForTest = upstream.URL
			t.Cleanup(func() { codexAlphaSearchURLForTest = previousURL })
			handler, calls := newAnthropicStreamFailureTestHandler(t, func(call int32, w http.ResponseWriter) {
				if call == 1 {
					writeCodexSSE(w, codexSearchTestCompleted(codexSearchTestCall("codex2api_web_search", "Python 官方网站")))
				} else {
					writeCodexSSE(w, codexSearchTestCompleted(map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Python: https://www.python.org/"}}}))
				}
			})
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			raw, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5-5", "stream": stream, "max_tokens": 100, "tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 1}}, "messages": []any{map[string]any{"role": "user", "content": "Search Python official website"}}})
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
			handler.Messages(ctx)
			if recorder.Code != http.StatusOK || calls.Load() != 2 || searchCalls != 1 || !strings.Contains(recorder.Body.String(), "web_search_tool_result") || !strings.Contains(recorder.Body.String(), "Python: https://www.python.org/") || strings.Contains(recorder.Body.String(), "codex2api_web_search") {
				t.Fatalf("Messages search failed: status=%d rounds=%d searches=%d body=%s", recorder.Code, calls.Load(), searchCalls, recorder.Body.String())
			}
		})
	}
}
