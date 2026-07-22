package proxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/tidwall/gjson"
)

func resetResponseCacheForTest() {
	respCache.mu.Lock()
	respCache.store = make(map[string]*responseCacheEntry)
	respCache.runtimeCache = nil
	respCache.mu.Unlock()
}

func TestCacheCompletedResponseCachesCodexNativeToolCalls(t *testing.T) {
	resetResponseCacheForTest()

	expandedInput := []byte(`[{"type":"message","role":"user","content":"find a tool"}]`)
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_native","output":[{"type":"tool_search_call","id":"ts_123","call_id":"call_search","status":"completed"}]}}`)

	cacheCompletedResponse("key:1", expandedInput, completed)

	cached := getResponseCache("key:1", "resp_native")
	if len(cached) != 2 {
		t.Fatalf("cached items = %d, want 2", len(cached))
	}
	if got := gjson.GetBytes(cached[1], "call_id").String(); got != "call_search" {
		t.Fatalf("cached call_id = %q, want call_search", got)
	}
	if got := gjson.GetBytes(cached[1], "id"); got.Exists() {
		t.Fatalf("cached output item id should be stripped for store=false replay, got %s", got.Raw)
	}
}

func TestCacheCompletedResponseUsesOutputItemDoneFallback(t *testing.T) {
	resetResponseCacheForTest()

	expandedInput := []byte(`[{"type":"message","role":"user","content":"inspect workspace"}]`)
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_stream"}}`)
	outputItems := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","id":"fc_stream","call_id":"call_stream","name":"manage_todo_list","arguments":"{}"}`),
	}
	cacheCompletedResponseWithOutputItems("key:1", expandedInput, completed, outputItems)

	next := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp_stream","input":[{"type":"function_call_output","call_id":"call_stream","output":"ok"}]}`)
	got, _ := PrepareResponsesBodyForOwner(next, "key:1")
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 3 {
		t.Fatalf("expanded input count = %d, want 3; body=%s", len(items), got)
	}
	if typ := items[0].Get("type").String(); typ != "message" {
		t.Fatalf("input[0].type = %q, want message", typ)
	}
	if typ := items[1].Get("type").String(); typ != "function_call" {
		t.Fatalf("input[1].type = %q, want function_call", typ)
	}
	if id := items[1].Get("id"); id.Exists() {
		t.Fatalf("cached function_call id should be stripped, got %s", id.Raw)
	}
	if typ := items[2].Get("type").String(); typ != "function_call_output" {
		t.Fatalf("input[2].type = %q, want function_call_output", typ)
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id should be removed after expansion: %s", got)
	}
}

func TestExpandPreviousResponseUsesCachedCodexNativeToolContext(t *testing.T) {
	resetResponseCacheForTest()

	cacheCompletedResponse("key:1",
		[]byte(`[{"type":"message","role":"user","content":"run mcp tool"}]`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_mcp","output":[{"type":"mcp_tool_call","call_id":"call_mcp","name":"read","arguments":"{}"}]}}`),
	)

	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_mcp","input":[{"type":"mcp_tool_call_output","call_id":"call_mcp","output":"ok"}]}`)
	got, prevID := expandPreviousResponse(body, "key:1")

	if prevID != "resp_mcp" {
		t.Fatalf("prevID = %q, want resp_mcp", prevID)
	}
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 3 {
		t.Fatalf("expanded input count = %d, want 3; body=%s", len(input), got)
	}
	if typ := input[1].Get("type").String(); typ != "mcp_tool_call" {
		t.Fatalf("cached tool call type = %q, want mcp_tool_call", typ)
	}
	if callID := input[2].Get("call_id").String(); callID != "call_mcp" {
		t.Fatalf("current output call_id = %q, want call_mcp", callID)
	}
}

// shell_call / apply_patch_call 是上游新版工具调用项，需与 function_call 一样
// 参与 call_id 续链缓存，否则 store=false 回放会丢上下文。
func TestExpandPreviousResponseCachesShellAndApplyPatchCalls(t *testing.T) {
	resetResponseCacheForTest()

	cacheCompletedResponse("key:1",
		[]byte(`[{"type":"message","role":"user","content":"run a command"}]`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_shell","output":[{"type":"shell_call","id":"sc_1","call_id":"call_shell","action":{"command":["echo","hi"]}},{"type":"apply_patch_call","id":"ap_1","call_id":"call_patch","action":{"patch":"x"}}]}}`),
	)

	body := []byte(`{"model":"gpt-5.5","previous_response_id":"resp_shell","input":[{"type":"shell_call_output","call_id":"call_shell","output":"hi"}]}`)
	got, prevID := expandPreviousResponse(body, "key:1")

	if prevID != "resp_shell" {
		t.Fatalf("prevID = %q, want resp_shell", prevID)
	}
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 4 {
		t.Fatalf("expanded input count = %d, want 4; body=%s", len(input), got)
	}
	if typ := input[1].Get("type").String(); typ != "shell_call" {
		t.Fatalf("cached item 1 type = %q, want shell_call", typ)
	}
	if input[1].Get("id").Exists() {
		t.Fatalf("cached shell_call id should be stripped: %s", input[1].Raw)
	}
	if typ := input[2].Get("type").String(); typ != "apply_patch_call" {
		t.Fatalf("cached item 2 type = %q, want apply_patch_call", typ)
	}
}

func TestExpandPreviousResponseUsesRuntimeCacheAfterLocalMiss(t *testing.T) {
	resetResponseCacheForTest()
	tc := cache.NewMemory(10)
	SetResponseContextCache(tc)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = tc.Close()
	})

	ctx := context.Background()
	items := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":"run mcp tool"}`),
		json.RawMessage(`{"type":"mcp_tool_call","call_id":"call_mcp","name":"read","arguments":"{}"}`),
	}
	if err := tc.SetResponseContext(ctx, responseCacheStoreKey("key:1", "resp_remote"), items, time.Minute); err != nil {
		t.Fatalf("SetResponseContext: %v", err)
	}

	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_remote","input":[{"type":"mcp_tool_call_output","call_id":"call_mcp","output":"ok"}]}`)
	got, prevID := expandPreviousResponse(body, "key:1")

	if prevID != "resp_remote" {
		t.Fatalf("prevID = %q, want resp_remote", prevID)
	}
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 3 {
		t.Fatalf("expanded input count = %d, want 3; body=%s", len(input), got)
	}
	if typ := input[1].Get("type").String(); typ != "mcp_tool_call" {
		t.Fatalf("cached tool call type = %q, want mcp_tool_call", typ)
	}
}

func TestExpandPreviousResponseSkipsInjectionWhenInputHasFunctionCall(t *testing.T) {
	resetResponseCacheForTest()

	cacheCompletedResponse("key:1",
		[]byte(`[{"type":"message","role":"user","content":"call tool"}]`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_dup","output":[{"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{}"}]}}`),
	)

	// 客户端续链时同时自带 function_call 和 function_call_output，再注入缓存里的 function_call 会让 call_abc 重复。
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_dup","input":[` +
		`{"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_abc","output":"sunny"}` +
		`]}`)
	got, prevID := expandPreviousResponse(body, "key:1")

	if prevID != "resp_dup" {
		t.Fatalf("prevID = %q, want resp_dup", prevID)
	}
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 2 {
		t.Fatalf("input count = %d, want 2 (no injection); body=%s", len(input), got)
	}
	if typ := input[0].Get("type").String(); typ != "function_call" {
		t.Fatalf("input[0].type = %q, want function_call", typ)
	}
	if callID := input[0].Get("call_id").String(); callID != "call_abc" {
		t.Fatalf("input[0].call_id = %q, want call_abc", callID)
	}
}

func TestExpandPreviousResponseLeavesBodyUntouchedOnCacheMiss(t *testing.T) {
	resetResponseCacheForTest()

	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[` +
		`{"type":"function_call_output","call_id":"call_missing","output":"x"}` +
		`]}`)
	got, prevID := expandPreviousResponse(body, "key:1")

	if prevID != "resp_missing" {
		t.Fatalf("prevID = %q, want resp_missing (returned for downstream cache linkage)", prevID)
	}
	if string(got) != string(body) {
		t.Fatalf("body mutated on cache miss; got=%s want=%s", got, body)
	}
}

func TestCacheCompletedResponseDoesNotCacheNonCallIDToolCalls(t *testing.T) {
	resetResponseCacheForTest()

	for _, test := range []struct {
		name string
		body []byte
	}{
		{
			name: "image_generation_call",
			body: []byte(`{"type":"response.completed","response":{"id":"resp_image","output":[{"type":"image_generation_call","id":"ig_1","status":"completed"}]}}`),
		},
		{
			name: "web_search_call",
			body: []byte(`{"type":"response.completed","response":{"id":"resp_web","output":[{"type":"web_search_call","call_id":"call_bad","status":"completed"}]}}`),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetResponseCacheForTest()

			cacheCompletedResponse("key:1", []byte(`[{"type":"message","role":"user","content":"hello"}]`), test.body)

			respID := gjson.GetBytes(test.body, "response.id").String()
			if cached := getResponseCache("key:1", respID); cached != nil {
				t.Fatalf("expected no cache for %s, got %d items", test.name, len(cached))
			}
		})
	}
}

func TestCacheCompletedResponseSkipsReasoningAndMessageOutputItems(t *testing.T) {
	resetResponseCacheForTest()

	cacheCompletedResponse("key:1",
		[]byte(`[`+
			`{"type":"reasoning","id":"rs_input","encrypted_content":"stale"},`+
			`{"type":"message","id":"msg_input","role":"user","content":"call a tool"}`+
			`]`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_reasoning","output":[`+
			`{"type":"reasoning","id":"rs_0609","encrypted_content":"opaque"},`+
			`{"type":"message","id":"msg_output","role":"assistant","content":[{"type":"output_text","text":"thinking"}]},`+
			`{"type":"function_call","id":"fc_123","call_id":"call_abc","name":"lookup","arguments":"{}"}`+
			`]}}`),
	)

	cached := getResponseCache("key:1", "resp_reasoning")
	if len(cached) != 2 {
		t.Fatalf("cached items = %d, want input message + function_call only", len(cached))
	}
	if typ := gjson.GetBytes(cached[0], "type").String(); typ != "message" {
		t.Fatalf("cached[0].type = %q, want message", typ)
	}
	if id := gjson.GetBytes(cached[0], "id"); id.Exists() {
		t.Fatalf("cached input id should be stripped, got %s", id.Raw)
	}
	if typ := gjson.GetBytes(cached[1], "type").String(); typ != "function_call" {
		t.Fatalf("cached[1].type = %q, want function_call", typ)
	}
	if id := gjson.GetBytes(cached[1], "id"); id.Exists() {
		t.Fatalf("cached function_call id should be stripped, got %s", id.Raw)
	}
}

// TestGetResponseCacheIsolatesOwners 验证 previous_response_id 缓存按 owner(下游
// API Key)隔离：用户B拿着用户A的 response_id 查询必须 miss，杜绝跨用户注入他人
// 对话历史（"用户2串到用户1的上下文"类泄露）。
func TestGetResponseCacheIsolatesOwners(t *testing.T) {
	resetResponseCacheForTest()

	cacheCompletedResponse("key:1",
		[]byte(`[{"type":"message","role":"user","content":"user A secret"}]`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_shared","output":[{"type":"function_call","call_id":"call_a","name":"lookup","arguments":"{}"}]}}`),
	)

	if cached := getResponseCache("key:1", "resp_shared"); len(cached) == 0 {
		t.Fatal("owner key:1 should hit its own cache")
	}
	if cached := getResponseCache("key:2", "resp_shared"); cached != nil {
		t.Fatalf("owner key:2 must NOT read key:1's cache, got %d items", len(cached))
	}
	if cached := getResponseCache("anon", "resp_shared"); cached != nil {
		t.Fatalf("anon owner must NOT read key:1's cache, got %d items", len(cached))
	}
}

// TestExpandPreviousResponseCrossOwnerMiss 验证跨 owner 的 previous_response_id
// 展开按缓存未命中处理，body 原样透传（不注入别人的历史）。
func TestExpandPreviousResponseCrossOwnerMiss(t *testing.T) {
	resetResponseCacheForTest()

	cacheCompletedResponse("key:1",
		[]byte(`[{"type":"message","role":"user","content":"user A secret"}]`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_cross","output":[{"type":"mcp_tool_call","call_id":"call_x","name":"read","arguments":"{}"}]}}`),
	)

	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_cross","input":[{"role":"user","content":"user B question"}]}`)
	got, prevID := expandPreviousResponse(body, "key:2")

	if prevID != "resp_cross" {
		t.Fatalf("prevID = %q, want resp_cross", prevID)
	}
	if string(got) != string(body) {
		t.Fatalf("cross-owner expansion must be a no-op; got=%s", got)
	}
}

func TestResponseCacheOwner(t *testing.T) {
	if got := responseCacheOwner(42); got != "key:42" {
		t.Fatalf("responseCacheOwner(42) = %q, want key:42", got)
	}
	if got := responseCacheOwner(0); got != "anon" {
		t.Fatalf("responseCacheOwner(0) = %q, want anon", got)
	}
	if got := responseCacheOwner(-1); got != "anon" {
		t.Fatalf("responseCacheOwner(-1) = %q, want anon", got)
	}
}
