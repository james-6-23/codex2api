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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestReviewModelCapabilitiesAsyncPublishesPartialUpdates(t *testing.T) {
	db := newTestModelRegistryDB(t)
	id, err := db.InsertAccount(context.Background(), "caps", "test-rt", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	account := &auth.Account{DBID: id, CredentialGeneration: row.CredentialGeneration}
	h := &Handler{db: db}
	for _, body := range []string{
		`{"models":[{"slug":"gpt-6-astra","use_responses_lite":true}]}`,
		`{"models":[{"slug":"gpt-5.6-sol","use_responses_lite":false}]}`,
	} {
		h.learnModelCapabilitiesAsync(account, []byte(body))
		model := gjson.Get(body, "models.0.slug").String()
		want := gjson.Get(body, "models.0.use_responses_lite").Bool()
		deadline := time.Now().Add(3 * time.Second)
		for {
			if got, known := account.ModelSupportsResponsesLite(model); known && got == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("async capability update was not published")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if got, known := account.ModelSupportsResponsesLite("gpt-6-astra"); !known || !got {
		t.Fatal("partial manifest erased a previously learned model")
	}
}

func TestReviewEncryptedMemoryUsesStableSessionAndAllAuthForms(t *testing.T) {
	previous := rejectedEncryptedContent
	rejectedEncryptedContent = &encryptedContentMemory{now: time.Now}
	t.Cleanup(func() { rejectedEncryptedContent = previous })
	body := []byte(`{"input":[{"type":"message","role":"user","content":"root"},{"type":"reasoning","encrypted_content":"cipher"}]}`)
	account := &auth.Account{DBID: 1, CredentialGeneration: 1}
	for _, name := range []string{"Authorization", "X-Api-Key", "Anthropic-Auth-Token", "Sec-WebSocket-Protocol"} {
		header := make(http.Header)
		header.Set(name, "owner-a")
		if name == "Authorization" {
			header.Set(name, "Bearer owner-a")
		}
		if name == "Sec-WebSocket-Protocol" {
			header.Set(name, "realtime, openai-insecure-api-key.owner-a")
			header.Set("Connection", "Upgrade")
			header.Set("Upgrade", "websocket")
		}
		ctx := context.WithValue(context.Background(), encryptedContentSessionKey{}, "stable-conversation")
		_, first := prepareEncryptedContentAttempt(ctx, account, body, "random-upstream-1", header)
		first.memory.mark(first.key, encryptedPayloadDigests(body))
		clean, next := prepareEncryptedContentAttempt(ctx, account, body, "random-upstream-2", header)
		if first.key != next.key || strings.Contains(string(clean), "cipher") {
			t.Fatalf("%s lost remembered rejection", name)
		}
		header.Set(name, strings.ReplaceAll(header.Get(name), "owner-a", "owner-b"))
		clean, _ = prepareEncryptedContentAttempt(ctx, account, body, "random-upstream-3", header)
		if string(clean) != string(body) {
			t.Fatalf("%s leaked into another static key", name)
		}
	}
}

func TestReviewSharedPayloadNormalizationAvoidsHistoricalReparse(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	item := json.RawMessage(`{"type":"message","content":"a < b & c"}`)
	setResponseCache("o", "r", []json.RawMessage{item})
	items := getResponseCacheForReplay("o", "r").Items
	baseline := testing.AllocsPerRun(20, func() { _, _ = cache.NormalizeResponseContextItems(items) })
	retained := testing.AllocsPerRun(20, func() {
		respCache.mu.Lock()
		respCache.normalizeResponseContextItemsLocked(items)
		respCache.mu.Unlock()
	})
	if retained >= baseline {
		t.Fatalf("retained payload reparsed: allocations %.0f vs %.0f", retained, baseline)
	}
}

func TestReviewBackendDrainRejectsNewAsyncWrites(t *testing.T) {
	resetResponseCacheForTest()
	backend := newRecordingResponseContextBackend(true)
	backend.blockWrites = make(chan struct{})
	t.Cleanup(func() { resetResponseCacheForTest(); backend.TokenCache.Close() })
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(backend.blockWrites) }) })
	writeResponseContextBackend(backend, "one", "one", []json.RawMessage{json.RawMessage(`{"content":"one"}`)})
	if DrainResponseCacheBackendWrites(time.Millisecond) {
		t.Fatal("drain completed while an admitted write was blocked")
	}
	finished := make(chan struct{})
	go func() {
		writeResponseContextBackend(backend, "two", "two", []json.RawMessage{json.RawMessage(`{"content":"two"}`)})
		close(finished)
	}()
	release.Do(func() { close(backend.blockWrites) })
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("write during drain deadlocked")
	}
	if !DrainResponseCacheBackendWrites(time.Second) {
		t.Fatal("drain did not complete")
	}
	if sets, _ := backend.counts(); sets != 2 {
		t.Fatalf("shutdown lost writes: %d", sets)
	}
}

func TestReviewWSFailOpenDoesNotPoisonLaterFailClosed(t *testing.T) {
	for _, transport := range []string{"websocket", "http"} {
		t.Run(transport, func(t *testing.T) {
			resetResponseCacheForTest()
			t.Setenv("CODEX_WS_CONTINUATION_FAIL_OPEN", "true")
			previousResin := resinCfg.Load()
			t.Cleanup(func() { resinCfg.Store(previousResin) })
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := readUpstreamRequestBody(r)
				if gjson.GetBytes(body, "previous_response_id").Exists() {
					t.Error("HTTP fallback retained unavailable ID")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, wsContextTestSSE("lossy-response"))
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
			var calls atomic.Int32
			conn, _ := newReviewWSClient(t, func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
				n := calls.Add(1)
				if transport == "http" && n == 1 {
					return nil, errors.New("websocket: close 1009 (message too big)")
				}
				if transport == "websocket" && n == 2 {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wsContextTestSSE("lossy-response")))}, nil
				}
				return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"previous_response_not_found"}}`))}, nil
			})
			for i, id := range []string{"missing-root", "lossy-response"} {
				if i == 1 {
					t.Setenv("CODEX_WS_CONTINUATION_FAIL_OPEN", "false")
				}
				if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","previous_response_id":%q,"input":[{"type":"message","role":"user","content":"increment"}]}`, id))); err != nil {
					t.Fatal(err)
				}
				conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				terminal := readResponsesWSTerminalEvent(t, conn)
				if i == 0 && gjson.GetBytes(terminal, "type").String() != "response.completed" {
					t.Fatalf("escape hatch failed: %s", terminal)
				}
				if i == 1 && gjson.GetBytes(terminal, "error.code").String() != "response_context_unavailable" {
					t.Fatalf("lossy history became trusted: %s", terminal)
				}
			}
			if getResponseCache("anon", "lossy-response") != nil {
				t.Fatal("lossy response was cached")
			}
		})
	}
}

func TestReviewSharedPayloadPartialEvictionAndReadmission(t *testing.T) {
	cfg := defaultResponseCacheConfig()
	cfg.maxEntries = 2
	resetResponseCacheStateForTest(cfg)
	t.Cleanup(resetResponseCacheForTest)
	x, y, z := json.RawMessage(`{"content":"x"}`), json.RawMessage(`{"content":"yy"}`), json.RawMessage(`{"content":"zzz"}`)
	setResponseCache("o", "a", []json.RawMessage{x, y})
	setResponseCache("o", "b", []json.RawMessage{y, z})
	borrowed := lookupResponseCacheResultWithOwnership("o", "a", true).Items
	lookupResponseCacheResultWithOwnership("o", "b", true)
	setResponseCache("o", "c", []json.RawMessage{z, z})
	if got := GetResponseCacheStats().SharedPayloadBytes; got != int64(len(y)+len(z)) {
		t.Fatalf("partial eviction retained %d bytes", got)
	}
	if string(borrowed[0]) != string(x) {
		t.Fatal("eviction invalidated a borrowed payload")
	}
	setResponseCache("o", "b", []json.RawMessage{y, z})
	if got := GetResponseCacheStats().SharedPayloadBytes; got != int64(len(y)+len(z)) {
		t.Fatalf("replacement changed unique bytes: %d", got)
	}
	setResponseCache("o", "d", []json.RawMessage{x})
	if got := GetResponseCacheStats().SharedPayloadBytes; got != int64(len(x)+len(y)+len(z)) {
		t.Fatalf("readmission reused a dead blob: %d", got)
	}
	cfg.maxEntries = 0
	configureResponseCacheForTest(cfg)
	if GetResponseCacheStats().SharedPayloadBytes != 0 || respCache.sharedItems != nil {
		t.Fatal("empty shared pool retained payloads or map buckets")
	}
}

func TestReviewWSPinnedAncestorSurvivesExpiry(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	setResponseCache("o", "prev", []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"root"}`)})
	body := []byte(`{"previous_response_id":"prev","input":[{"type":"message","role":"user","content":"next"}]}`)
	source := newResponsesWSReplaySource(body, "o")
	cleanupResponseCacheExpired(time.Now().Add(11 * time.Minute))
	before := GetResponseCacheStats()
	expanded, lost, err := degradeResponsesWSContinuationWithSource(body, "o", source)
	if err != nil || lost || gjson.GetBytes(expanded, "input.0.content").String() != "root" {
		t.Fatalf("lost a live-at-admission ancestor: %v", err)
	}
	if GetResponseCacheStats().Misses != before.Misses {
		t.Fatal("snapshot construction counted a cache miss")
	}
}

func TestReviewWSLossyFallbackCannotCreateSnapshot(t *testing.T) {
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	t.Setenv("CODEX_WS_CONTINUATION_FAIL_OPEN", "true")
	body := []byte(`{"previous_response_id":"missing","input":[{"type":"message","role":"user","content":"increment"}]}`)
	_, lost, err := degradeResponsesWSContinuationWithSource(body, "o", newResponsesWSReplaySource(body, "o"))
	if err != nil || !lost {
		t.Fatal("lossy fallback not distinguished from complete replay")
	}
}

func newReviewWSClient(t *testing.T, execute func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error)) (*websocket.Conn, *auth.Account) {
	t.Helper()
	previous := WebsocketExecuteFunc
	WebsocketExecuteFunc = execute
	t.Cleanup(func() {
		WebsocketExecuteFunc = previous
		globalWSSizeRouter = websocketSizeRouter{}
		resetResponseCacheForTest()
	})
	globalWSSizeRouter = websocketSizeRouter{}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "test", AccountID: "test", PlanType: "plus"}
	store.AddAccount(account)
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	r := gin.New()
	h.RegisterRoutes(r)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, account
}

func TestReviewWSHTTPFallbackMissingContextReleasesLease(t *testing.T) {
	resetResponseCacheForTest()
	conn, account := newReviewWSClient(t, func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		return nil, errors.New("websocket: close 1009 (message too big)")
	})
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"missing","input":[{"type":"message","role":"user","content":"next"}]}`)); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, body, err := conn.ReadMessage()
	if err != nil || gjson.GetBytes(body, "error.code").String() != "response_context_unavailable" {
		t.Fatalf("missing fail-closed error: %s %v", body, err)
	}
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) || account.GetActiveRequests() != 0 {
		t.Fatalf("fallback lease leaked: %d, %v", account.GetActiveRequests(), err)
	}
	if GetResponseCacheStats().KnownUnavailableErrors != 1 {
		t.Fatal("WS failure was not counted")
	}
}

func TestReviewWSStoreFalseSkipsAlwaysCache(t *testing.T) {
	resetResponseCacheForTest()
	var calls atomic.Int32
	conn, _ := newReviewWSClient(t, func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wsContextTestSSE(fmt.Sprintf("r%d", calls.Add(1)), `{"type":"message","role":"assistant","content":"ok"}`)))}, nil
	})
	for i := 0; i < 2; i++ {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","store":false,"input":[{"type":"message","role":"user","content":"root"}]}`)); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		readResponsesWSTerminalEvent(t, conn)
	}
	if getResponseCache("anon", "r1") != nil {
		t.Fatal("store:false was ignored by always policy")
	}
}

func TestReviewCustomToolEmptyDoneAndMessagesFailure(t *testing.T) {
	st := NewStreamTranslator("chat-test", "gpt-6-astra", 0)
	tr := newAnthropicStreamTranslator("gpt-6-astra")
	for _, event := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc","call_id":"c","name":"exec"}}`,
		`{"type":"response.custom_tool_call_input.delta","item_id":"ctc","delta":"print(1)"}`,
		`{"type":"response.custom_tool_call_input.done","item_id":"ctc","input":""}`,
	} {
		if _, done := st.Translate([]byte(event)); done || st.ToolArgumentsError() != nil {
			t.Fatal("empty final input rejected streamed custom input")
		}
		tr.translateEvent([]byte(event))
		if tr.toolInputError != nil {
			t.Fatal(tr.toolInputError)
		}
	}
	body, err := TranslateChatToResponsesForGrok([]byte(`{"model":"grok-4","messages":[{"role":"user","content":"run"}],"tools":[{"type":"custom","custom":{"name":"exec"}}],"tool_choice":{"type":"custom","custom":{"name":"exec"}}}`))
	if err != nil || gjson.GetBytes(body, "tool_choice.type").String() != "custom" {
		t.Fatalf("Grok rejected custom selection: %s %v", body, err)
	}
	h, _ := newAnthropicStreamFailureTestHandler(t, func(_ int32, w http.ResponseWriter) {
		writeCodexSSE(w, `{"type":"response.output_item.added","item":{"type":"custom_tool_call","id":"ctc","call_id":"c","name":"exec"}}`, `{"type":"response.custom_tool_call_input.delta","delta":"print(1)"}`, `{"type":"response.custom_tool_call_input.done","input":"different"}`, `{"type":"response.completed","response":{"output":[]}}`)
	})
	w := invokeAnthropicMessagesStream(t, h)
	if !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: message_stop") {
		t.Fatalf("invalid custom stream ended as success: %s", w.Body)
	}
}

func TestReviewCustomToolHistoryAndSizeLimits(t *testing.T) {
	h, calls := newAnthropicStreamFailureTestHandler(t, func(_ int32, _ http.ResponseWriter) { t.Error("invalid history reached upstream") })
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"claude-opus-4-6","max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"exec","input":{}}]},{"role":"user","content":"continue"}]}`, anthropicCustomToolID("call-test", "functions"))))
	h.Messages(c)
	if w.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("invalid custom history accepted: %d %s", w.Code, w.Body)
	}
	st := NewStreamTranslator("chat-test", "gpt-6-astra", 0)
	tr := newAnthropicStreamTranslator("gpt-6-astra")
	added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc","call_id":"c","name":"exec"}}`)
	st.Translate(added)
	tr.translateEvent(added)
	large := customToolTestEvent("response.custom_tool_call_input.delta", map[string]any{"item_id": "ctc", "delta": strings.Repeat("x", responseCacheMaxEntry+1)})
	if _, done := st.Translate(large); !done || st.ToolArgumentsError() == nil {
		t.Fatal("oversized Chat custom input was accepted")
	}
	tr.translateEvent(large)
	if tr.toolInputError == nil {
		t.Fatal("oversized Messages custom input was accepted")
	}
}
