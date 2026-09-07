package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// Reconstructed protocol fixture: no captured prompts, IDs, or credentials.
func wsContextTestSSE(id string, items ...string) string {
	var stream strings.Builder
	for index, item := range items {
		fmt.Fprintf(&stream, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", index, item)
	}
	fmt.Fprintf(&stream, "data: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", id)
	return stream.String()
}

func TestResponsesWSContextSurvivesMultiTurnFallback(t *testing.T) {
	for _, fallback := range []string{"previous_not_found", "http", "incomplete", "expires_during_turn"} {
		t.Run(fallback, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			resetResponseCacheForTest()
			previousExec, previousResin := WebsocketExecuteFunc, resinCfg.Load()
			t.Cleanup(func() {
				WebsocketExecuteFunc = previousExec
				resinCfg.Store(previousResin)
				globalWSSizeRouter = websocketSizeRouter{}
				resetResponseCacheForTest()
			})
			globalWSSizeRouter = websocketSizeRouter{}
			bodies := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				bodies <- readUpstreamRequestBody(r)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, wsContextTestSSE("resp_after_fallback"))
			}))
			defer upstream.Close()
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
			var attempts atomic.Int32
			WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyOverride, apiKey string, cfg *DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
				n := attempts.Add(1)
				var sse string
				switch n {
				case 1:
					sse = wsContextTestSSE("resp_turn1", `{"type":"custom_tool_call","id":"ctc_one","call_id":"call_one","name":"exec","input":"print(\"hello\")"}`)
				case 2:
					if fallback == "expires_during_turn" {
						respCache.mu.Lock()
						if entry := respCache.store[responseCacheStoreKey("anon", "resp_turn1")]; entry != nil {
							entry.expiresAt = time.Now().Add(-time.Second)
						}
						respCache.mu.Unlock()
					}
					if gjson.GetBytes(body, "input.#").Int() != 1 || gjson.GetBytes(body, "previous_response_id").String() != "resp_turn1" {
						t.Error("healthy WS continuation must still send only the new tool result")
					}
					sse = wsContextTestSSE("resp_turn2", `{"type":"custom_tool_call","id":"ctc_two","call_id":"call_two","name":"exec","input":"print(\"world\")"}`)
				case 3:
					sse = wsContextTestSSE("resp_turn3", `{"type":"message","id":"msg_final","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"finished"}]}`)
					if fallback == "incomplete" {
						sse = strings.ReplaceAll(sse, "response.completed", "response.incomplete")
					}
				case 4:
					if fallback == "http" {
						return nil, errors.New("websocket: close 1009 (message too big)")
					}
					return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"previous_response_not_found","message":"Previous response with id not found"}}`))}, nil
				default:
					bodies <- append([]byte(nil), body...)
					sse = wsContextTestSSE("resp_after_fallback")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
			}
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
			store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "plus", AccountID: "test-account"})
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			router := gin.New()
			handler.RegisterRoutes(router)
			server := httptest.NewServer(router)
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			inputs := []string{
				`{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","format":{"type":"text"}}]}]},{"type":"message","role":"user","content":"original request"}]}`,
				`{"previous_response_id":"resp_turn1","input":[{"type":"custom_tool_call_output","call_id":"call_one","output":"hello"}]}`,
				`{"previous_response_id":"resp_turn2","input":[{"type":"custom_tool_call_output","call_id":"call_two","output":"world"}]}`,
				`{"previous_response_id":"resp_turn3","input":[{"type":"message","role":"user","content":"continue"}]}`,
			}
			for turnIndex, input := range inputs {
				var request map[string]any
				if err := json.Unmarshal([]byte(input), &request); err != nil {
					t.Fatal(err)
				}
				request["type"], request["model"], request["prompt_cache_key"] = "response.create", "gpt-5.4", "har-context-test"
				if err := conn.WriteJSON(request); err != nil {
					t.Fatal(err)
				}
				conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				terminal := readResponsesWSTerminalEvent(t, conn)
				wantTerminal := "response.completed"
				if fallback == "incomplete" && turnIndex == 2 {
					wantTerminal = "response.incomplete"
				}
				if gjson.GetBytes(terminal, "type").String() != wantTerminal {
					t.Fatalf("unexpected terminal: %s", terminal)
				}
			}
			select {
			case body := <-bodies:
				if gjson.GetBytes(body, "previous_response_id").Exists() {
					t.Fatal("fallback still references the unavailable upstream response")
				}
				items := gjson.GetBytes(body, "input").Array()
				if len(items) != 8 {
					t.Fatalf("recovered %d items, want tools + original message + two call/result pairs + final message + new message", len(items))
				}
				if items[0].Get("type").String() != "additional_tools" || items[1].Get("content").String() != "original request" || items[6].Get("phase").String() != "final_answer" {
					t.Fatal("lost tool declarations, original message, or final-answer phase")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no fallback request")
			}
		})
	}
}

func TestResponsesWSContextIsolationAndAdmission(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	input := `[{"type":"additional_tools","tools":[]},{"type":"message","role":"user","content":"root"}]`
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_context","output":[]}}`)
	outputs := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_secret","encrypted_content":"opaque-account-state"}`),
		json.RawMessage(`{"type":"message","id":"msg_answer","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"answer"}]}`),
	}
	cacheResponsesWSCompletedResponse("key:1", input, completed, outputs)
	next := []byte(`{"previous_response_id":"resp_context","input":[{"type":"message","role":"user","content":"next"}]}`)
	if _, err := degradeResponsesWSContinuationBody(next, "key:2"); err == nil {
		t.Fatal("another owner recovered the response")
	}
	recovered, err := degradeResponsesWSContinuationBody(next, "key:1")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(recovered, "input.#").Int() != 4 || strings.Contains(string(recovered), "opaque-account-state") || strings.Contains(string(recovered), "msg_answer") {
		t.Fatalf("unsafe or incomplete portable context: %s", recovered)
	}
	config := defaultResponseCacheConfig()
	config.maxItems = 2
	configureResponseCacheForTest(config)
	if _, err := degradeResponsesWSContinuationBody(next, "key:1"); err == nil {
		t.Fatal("oversized reconstruction was allowed")
	}
	cacheResponsesWSCompletedResponse("key:1", input, []byte(`{"response":{"id":"resp_oversize","output":[]}}`), outputs)
	if getResponseCache("key:1", "resp_oversize") != nil {
		t.Fatal("a truncated context was admitted")
	}
}

func TestResponsesWSContextRespectsOnDemandAndMissingAncestry(t *testing.T) {
	config := defaultResponseCacheConfig()
	config.writePolicy = database.ResponseCacheWritePolicyOnDemand
	resetResponseCacheStateForTest(config)
	t.Cleanup(resetResponseCacheForTest)
	root := `[{"type":"message","role":"user","content":"root"}]`
	cacheResponsesWSCompletedResponse("key:1", root, []byte(`{"response":{"id":"resp_first","output":[]}}`), nil)
	if GetResponseCacheStats().Entries != 0 {
		t.Fatal("on_demand was bypassed")
	}
	input, apiErr := responsesWSReplayInput([]byte(`{"previous_response_id":"resp_first","input":[{"type":"message","role":"user","content":"continue"}]}`), "key:1")
	if apiErr == nil || input != "" {
		t.Fatal("missing ancestry became a complete snapshot")
	}
	cacheResponsesWSCompletedResponse("key:1", input, []byte(`{"response":{"id":"resp_partial","output":[]}}`), nil)
	if GetResponseCacheStats().Entries != 0 {
		t.Fatal("a partial snapshot was cached")
	}
	cacheResponsesWSCompletedResponse("key:1", root, []byte(`{"response":{"id":"resp_new_root","output":[]}}`), nil)
	if GetResponseCacheStats().Entries != 1 {
		t.Fatal("a full new root was not cached after owner opted into continuation")
	}
}

func TestResponsesWSContextDeduplicatesReplayedToolPair(t *testing.T) {
	root := json.RawMessage(`{"type":"message","role":"user","content":"root"}`)
	call := json.RawMessage(`{"type":"custom_tool_call","call_id":"call_one","name":"exec","input":"print(1)"}`)
	result := json.RawMessage(`{"type":"custom_tool_call_output","call_id":"call_one","output":"1"}`)
	items := mergeResponsesWSContext([]json.RawMessage{root, call}, []json.RawMessage{call, result})
	if len(items) != 3 {
		t.Fatalf("duplicate call was retained: %d items", len(items))
	}
	items = mergeResponsesWSContext(items, []json.RawMessage{call, result})
	if len(items) != 3 {
		t.Fatalf("duplicate pair was retained: %d items", len(items))
	}
	if repeated := mergeResponsesWSContext([]json.RawMessage{root}, []json.RawMessage{root}); len(repeated) != 2 {
		t.Fatal("an identical new user message was collapsed")
	}
}

func TestResponsesWSContextRecoversFromSharedBackend(t *testing.T) {
	resetResponseCacheForTest()
	backend := newRecordingResponseContextBackend(true)
	SetResponseContextCache(backend)
	t.Cleanup(func() { resetResponseCacheForTest(); backend.TokenCache.Close() })
	input := `[{"type":"additional_tools","tools":[]},{"type":"message","role":"user","content":"root"}]`
	cacheResponsesWSCompletedResponse("key:1", input, []byte(`{"response":{"id":"resp_shared","output":[]}}`), []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","phase":"final_answer","content":"answer"}`)})
	drainResponseCacheBackendWrites()
	backend.mu.Lock()
	backend.bounded = cache.ResponseContextReadResult{Status: cache.ResponseContextReadFound, Items: cloneResponseContextItems(backend.writes[responseCacheStoreKey("key:1", "resp_shared")])}
	backend.mu.Unlock()
	resetResponseCacheForTest()
	SetResponseContextCache(backend)
	next := []byte(`{"previous_response_id":"resp_shared","input":[{"type":"message","role":"user","content":"next"}]}`)
	body, apiErr := degradeResponsesWSContinuationBody(next, "key:1")
	if apiErr != nil || gjson.GetBytes(body, "input.#").Int() != 4 {
		t.Fatalf("shared context was not recovered: %v", apiErr)
	}
	resetResponseCacheForTest()
	SetResponseContextCache(backend)
	backend.mu.Lock()
	backend.boundedErr = errors.New("backend unavailable")
	backend.mu.Unlock()
	if _, apiErr := degradeResponsesWSContinuationBody(next, "key:1"); apiErr == nil || apiErr.Code != api.ErrCodeServiceUnavailable {
		t.Fatalf("backend failure was hidden: %v", apiErr)
	}
}

func TestResponsesWSContextMissingFailsClosedAndReleasesAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheForTest()
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec; resetResponseCacheForTest() })
	var attempts atomic.Int32
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"previous_response_not_found"}}`))}, nil
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "plus", AccountID: "test-account"}
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"custom_tool_call_output","call_id":"call_missing","output":"result"}]}`)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(payload, "error.code").String() != "response_context_unavailable" {
		t.Fatalf("unexpected error: %s", payload)
	}
	if attempts.Load() != 1 || account.GetActiveRequests() != 0 {
		t.Fatalf("retried without history or leaked account lease: attempts=%d active=%d", attempts.Load(), account.GetActiveRequests())
	}
}

// Snapshot construction pins L1 without affecting replay statistics, then merges once.
func TestResponsesWSReplaySourceIsLazyAndMemoized(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	setResponseCache("key:1", "resp_prev", []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"root"}`)})
	before := GetResponseCacheStats()
	source := newResponsesWSReplaySource([]byte(`{"previous_response_id":"resp_prev","input":[{"type":"message","role":"user","content":"next"}]}`), "key:1")
	if after := GetResponseCacheStats(); after.Hits != before.Hits || after.Misses != before.Misses {
		t.Fatal("replay source looked up the cache eagerly")
	}
	first := source.Input()
	second := source.Input()
	if first == "" || first != second || gjson.Get(first, "#").Int() != 2 {
		t.Fatalf("unexpected replay input: %q / %q", first, second)
	}
	if after := GetResponseCacheStats(); after.Hits != before.Hits || after.Misses != before.Misses {
		t.Fatalf("snapshot construction polluted replay statistics: hits %d -> %d", before.Hits, after.Hits)
	}
	if got := newResponsesWSReplaySourceFromInput("[]").Input(); got != "[]" {
		t.Fatalf("precomputed input = %q", got)
	}
	var missing *responsesWSReplaySource
	if missing.Input() != "" {
		t.Fatal("nil source must yield no snapshot")
	}
	if got := newResponsesWSReplaySource([]byte(`{"previous_response_id":"resp_gone","input":[]}`), "key:1").Input(); got != "" {
		t.Fatalf("missing ancestry produced a snapshot: %q", got)
	}
}

// 逃生阀：默认 fail-closed；显式打开后剥 id 硬发，input 原样保留。
func TestResponsesWSContinuationFailOpenEscapeHatch(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	body := []byte(`{"previous_response_id":"resp_missing","input":[{"type":"message","role":"user","content":"next"}]}`)
	if _, apiErr := degradeResponsesWSContinuationBody(body, "key:1"); apiErr == nil {
		t.Fatal("fail-closed default was bypassed")
	}
	t.Setenv("CODEX_WS_CONTINUATION_FAIL_OPEN", "true")
	degraded, apiErr := degradeResponsesWSContinuationBody(body, "key:1")
	if apiErr != nil {
		t.Fatalf("fail-open escape hatch still failed: %v", apiErr)
	}
	if gjson.GetBytes(degraded, "previous_response_id").Exists() || gjson.GetBytes(degraded, "input.#").Int() != 1 || gjson.GetBytes(degraded, "input.0.content").String() != "next" {
		t.Fatalf("legacy degrade body mismatch: %s", degraded)
	}
}

// on_demand 下 WS 根轮凭 store 信号取得写入资格：store:false（Codex CLI 全量
// 上下文）不写，未声明或 true 的会话从根轮起入缓存，后续降级才有祖先可用。
func TestResponsesWSContextOnDemandBootstrapsFromStoreSignal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cacheConfig := defaultResponseCacheConfig()
	cacheConfig.writePolicy = database.ResponseCacheWritePolicyOnDemand
	resetResponseCacheStateForTest(cacheConfig)
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		resetResponseCacheForTest()
	})
	var turn atomic.Int32
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		id := fmt.Sprintf("resp_%d", turn.Add(1))
		sse := wsContextTestSSE(id, `{"type":"message","id":"msg_x","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "plus", AccountID: "test-account"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	send := func(payload string) {
		t.Helper()
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if terminal := readResponsesWSTerminalEvent(t, conn); gjson.GetBytes(terminal, "type").String() != "response.completed" {
			t.Fatalf("unexpected terminal: %s", terminal)
		}
	}
	// 缓存写入发生在终态帧写给客户端之后；帧循环是串行的，所以上一轮的写入
	// 决定在下一轮终态到达时必然已经落定，断言按这个顺序排。
	send(`{"type":"response.create","model":"gpt-5.4","store":false,"input":[{"type":"message","role":"user","content":"full context every turn"}]}`)
	send(`{"type":"response.create","model":"gpt-5.4","input":[{"type":"message","role":"user","content":"incremental root"}]}`)
	if getResponseCache("anon", "resp_1") != nil {
		t.Fatal("store:false root turn was cached under on_demand")
	}
	send(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_2","input":[{"type":"message","role":"user","content":"second turn"}]}`)
	if getResponseCache("anon", "resp_2") == nil {
		t.Fatal("continuation-capable root turn was not cached under on_demand")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cached := getResponseCache("anon", "resp_3"); len(cached) == 4 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("continuation snapshot has %d items, want root + answer + new message + answer", len(cached))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
